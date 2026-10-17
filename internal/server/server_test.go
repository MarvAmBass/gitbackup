package server

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/config"
)

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

var client = &http.Client{
	Timeout:       5 * time.Second,
	Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func get(t *testing.T, url string) (int, string, *http.Response) {
	t.Helper()
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ { // listener may still be starting
		if resp, err = client.Get(url); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp
}

func TestServerModes(t *testing.T) {
	dir := t.TempDir()
	addr := freePort(t)
	s := &Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS != nil {
				w.Write([]byte("hello tls"))
				return
			}
			w.Write([]byte("hello plain"))
		}),
		Listen: addr, StateDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan config.HTTPS)
	go s.Run(ctx, config.HTTPS{Mode: "off"}, updates)

	// off: plain only
	if code, body, _ := get(t, "http://"+addr+"/"); code != 200 || body != "hello plain" {
		t.Fatalf("off: %d %q", code, body)
	}

	// live switch: self-signed on the same port – both protocols answer
	updates <- config.HTTPS{Mode: "self-signed", Hostnames: []string{"gitbackup.lan", "192.0.2.10"}}
	time.Sleep(100 * time.Millisecond)
	if code, body, resp := get(t, "https://"+addr+"/"); code != 200 || body != "hello tls" || resp.TLS == nil {
		t.Fatalf("same port https: %d %q", code, body)
	}
	if code, body, _ := get(t, "http://"+addr+"/"); code != 200 || body != "hello plain" {
		t.Fatalf("same port http: %d %q", code, body)
	}
	st := s.State()
	if st.HTTPSAddr != addr || st.Fingerprint == "" || !strings.Contains(strings.Join(st.Names, ","), "192.0.2.10") {
		t.Fatalf("state: %+v", st)
	}

	// redirect plain HTTP on the same port
	updates <- config.HTTPS{Mode: "self-signed", Hostnames: []string{"gitbackup.lan"}, RedirectHTTP: true}
	time.Sleep(100 * time.Millisecond)
	if code, _, resp := get(t, "http://"+addr+"/repos?x=1"); code != http.StatusMovedPermanently || resp.Header.Get("Location") != "https://"+addr+"/repos?x=1" {
		t.Fatalf("redirect: %d %s", code, resp.Header.Get("Location"))
	}

	// separate port, provided certificate files (the self-signed ones)
	tlsAddr := freePort(t)
	updates <- config.HTTPS{Mode: "files", Port: tlsAddr, CertFile: filepath.Join(dir, "tls", "selfsigned.crt"),
		KeyFile: filepath.Join(dir, "tls", "selfsigned.key")}
	time.Sleep(100 * time.Millisecond)
	if code, body, _ := get(t, "https://"+tlsAddr+"/"); code != 200 || body != "hello tls" {
		t.Fatalf("separate port: %d %q", code, body)
	}
	if code, body, _ := get(t, "http://"+addr+"/"); code != 200 || body != "hello plain" {
		t.Fatalf("http next to separate https: %d %q", code, body)
	}

	// broken TLS setup: plain HTTP keeps working, the error is reported
	updates <- config.HTTPS{Mode: "files", CertFile: "/nope.crt", KeyFile: "/nope.key"}
	time.Sleep(100 * time.Millisecond)
	if code, body, _ := get(t, "http://"+addr+"/"); code != 200 || body != "hello plain" {
		t.Fatalf("fallback: %d %q", code, body)
	}
	if !strings.Contains(s.State().Error, "plain HTTP only") {
		t.Fatalf("error not reported: %+v", s.State())
	}
}

// Without a login, a separate HTTPS port reachable from other machines is refused.
func TestLoopbackOnlyRefusesOpenHTTPSPort(t *testing.T) {
	dir := t.TempDir()
	addr := freePort(t)
	s := &Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello plain")) }),
		Listen:  addr, StateDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), LoopbackOnly: true,
	}
	_, port, _ := net.SplitHostPort(freePort(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx, config.HTTPS{Mode: "self-signed", Port: ":" + port}, nil)
	if code, body, _ := get(t, "http://"+addr+"/"); code != 200 || body != "hello plain" {
		t.Fatalf("plain: %d %q", code, body)
	}
	if st := s.State(); st.HTTPSAddr != "" || !strings.Contains(st.Error, "not loopback-only") {
		t.Fatalf("open HTTPS port not refused: %+v", st)
	}
}

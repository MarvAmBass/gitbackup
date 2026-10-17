// Package server runs the web UI over HTTP and, optionally, HTTPS – on the
// same port (TLS is recognised per connection) or on a separate one – with
// self-signed, provided or Let's Encrypt certificates. TLS settings are
// applied live; a broken TLS setup falls back to plain HTTP so the UI stays
// reachable to fix it.
package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/MarvAmBass/gitbackup/internal/auth"
	"github.com/MarvAmBass/gitbackup/internal/config"
)

type Server struct {
	Handler  http.Handler
	Listen   string // HTTP address, e.g. :8080
	StateDir string // certificates live in StateDir/tls and StateDir/acme
	Log      *slog.Logger
	// LoopbackOnly refuses a separate HTTPS port that other machines could
	// reach (set while the UI runs without a login on a loopback address).
	LoopbackOnly bool

	mu    sync.Mutex
	state State
}

// State is shown on the settings page.
type State struct {
	Mode        string
	HTTPAddr    string
	HTTPSAddr   string
	Error       string
	Subject     string
	Names       []string
	NotAfter    time.Time
	Fingerprint string
}

func (s *Server) State() State { s.mu.Lock(); defer s.mu.Unlock(); return s.state }

func (s *Server) setState(st State) { s.mu.Lock(); s.state = st; s.mu.Unlock() }

// Run serves until ctx is done. A new value on updates replaces the TLS
// setup without restarting the process.
func (s *Server) Run(ctx context.Context, initial config.HTTPS, updates <-chan config.HTTPS) error {
	cur := initial
	for {
		stop, errc, err := s.start(cur)
		if err != nil {
			return err // HTTP listener itself failed (e.g. port in use)
		}
	wait:
		select {
		case <-ctx.Done():
			stop()
			return nil
		case err := <-errc:
			stop()
			return err
		case next := <-updates:
			if reflect.DeepEqual(next, cur) {
				goto wait // some other setting changed
			}
			s.Log.Info("applying new HTTPS settings", "mode", next.Mode, "port", next.Port)
			stop()
			cur = next
		}
	}
}

// start opens the listeners for cfg and returns a stop function.
func (s *Server) start(cfg config.HTTPS) (stop func(), errc chan error, err error) {
	st := State{Mode: cfg.Mode, HTTPAddr: s.Listen}
	var tlsConf *tls.Config
	var acmeHTTP func(http.Handler) http.Handler
	if cfg.Enabled() {
		tlsConf, acmeHTTP, err = s.tlsConfig(cfg, &st)
		if err != nil {
			st.Error = err.Error() + " – serving plain HTTP only"
			s.Log.Error("https setup failed, serving plain HTTP", "err", err)
			tlsConf = nil
		}
	}

	errc = make(chan error, 2)
	var servers []*http.Server
	serve := func(hs *http.Server, ln net.Listener) {
		servers = append(servers, hs)
		go func() {
			if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	newServer := func(h http.Handler) *http.Server {
		return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(s.Log.Handler(), slog.LevelDebug)}
	}

	ln, err := net.Listen("tcp", s.Listen)
	if err != nil {
		return nil, nil, err
	}
	stopAll := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, hs := range servers {
			hs.Shutdown(ctx)
		}
		ln.Close()
	}

	switch {
	case tlsConf == nil:
		serve(newServer(s.Handler), ln)
	case cfg.Port == "" || cfg.Port == s.Listen:
		// one port, both protocols: the first byte of a connection tells TLS apart
		st.HTTPSAddr = s.Listen
		h := s.plainHandler(cfg, "", acmeHTTP)
		serve(newServer(h), newSniffListener(ln, tlsConf))
	case s.LoopbackOnly && !auth.Loopback(cfg.Port):
		st.Error = "HTTPS port " + cfg.Port + " is not loopback-only and no login is set (GITBACKUP_ADMIN_PASSWORD) – serving plain HTTP only"
		s.Log.Error("https listener refused", "port", cfg.Port, "reason", "no login and not loopback")
		serve(newServer(s.Handler), ln)
	default:
		tln, err := net.Listen("tcp", cfg.Port)
		if err != nil {
			st.Error = "HTTPS port: " + err.Error() + " – serving plain HTTP only"
			s.Log.Error("https listener", "err", err)
			serve(newServer(s.Handler), ln)
			break
		}
		st.HTTPSAddr = cfg.Port
		serve(newServer(s.plainHandler(cfg, cfg.Port, acmeHTTP)), ln)
		serve(newServer(s.Handler), tls.NewListener(tln, tlsConf))
		prev := stopAll
		stopAll = func() { prev(); tln.Close() }
	}
	s.setState(st)
	s.Log.Info("web server listening", "http", st.HTTPAddr, "https", st.HTTPSAddr, "mode", st.Mode)
	return stopAll, errc, nil
}

// plainHandler serves unencrypted requests: ACME HTTP-01 challenges, an
// optional redirect to https://, otherwise the UI itself.
func (s *Server) plainHandler(cfg config.HTTPS, tlsPort string, acmeHTTP func(http.Handler) http.Handler) http.Handler {
	h := s.Handler
	if cfg.RedirectHTTP {
		next := h
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS != nil {
				next.ServeHTTP(w, r)
				return
			}
			host := r.Host
			if tlsPort != "" {
				if hn, _, err := net.SplitHostPort(r.Host); err == nil {
					host = hn
				}
				if !strings.HasSuffix(tlsPort, ":443") {
					host += tlsPort
				}
			}
			http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
		})
	}
	if acmeHTTP != nil {
		h = acmeHTTP(h)
	}
	return h
}

// tlsConfig builds the certificate source for cfg.
func (s *Server) tlsConfig(cfg config.HTTPS, st *State) (*tls.Config, func(http.Handler) http.Handler, error) {
	base := &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	switch cfg.Mode {
	case "self-signed":
		cert, info, err := selfSigned(filepath.Join(s.StateDir, "tls"), cfg.Hostnames)
		if err != nil {
			return nil, nil, err
		}
		info.fill(st)
		base.Certificates = []tls.Certificate{cert}
		return base, nil, nil
	case "files":
		fc := &fileCert{cert: cfg.CertFile, key: cfg.KeyFile}
		cert, err := fc.load()
		if err != nil {
			return nil, nil, err
		}
		certInfo(cert).fill(st)
		base.GetCertificate = fc.get
		return base, nil, nil
	case "letsencrypt":
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(filepath.Join(s.StateDir, "acme")),
			HostPolicy: autocert.HostWhitelist(cfg.Domains...),
			Email:      cfg.Email,
		}
		conf := m.TLSConfig() // includes the acme-tls/1 protocol for TLS-ALPN-01 challenges
		conf.MinVersion = tls.VersionTLS12
		conf.NextProtos = []string{"http/1.1", "acme-tls/1"}
		st.Subject, st.Names = "Let's Encrypt (issued on first request)", cfg.Domains
		return conf, m.HTTPHandler, nil
	}
	return nil, nil, errors.New("unknown https mode " + cfg.Mode)
}

// ---------------------------------------------------------------- one port, two protocols

// sniffListener hands out TLS connections for clients that start with a TLS
// handshake (first byte 0x16) and plain connections for everything else.
type sniffListener struct {
	net.Listener
	conf *tls.Config
	ch   chan net.Conn
	errc chan error
	done chan struct{}
	once sync.Once
}

func newSniffListener(inner net.Listener, conf *tls.Config) *sniffListener {
	l := &sniffListener{Listener: inner, conf: conf, ch: make(chan net.Conn), errc: make(chan error, 1), done: make(chan struct{})}
	go l.loop()
	return l
}

func (l *sniffListener) loop() {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			l.errc <- err
			return
		}
		go l.classify(c)
	}
}

func (l *sniffListener) classify(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	b, err := br.Peek(1)
	c.SetReadDeadline(time.Time{})
	if err != nil {
		c.Close()
		return
	}
	var out net.Conn = &peekedConn{Conn: c, r: br}
	if b[0] == 0x16 { // TLS handshake record
		out = tls.Server(out, l.conf)
	}
	select {
	case l.ch <- out:
	case <-l.done:
		c.Close()
	}
}

func (l *sniffListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case err := <-l.errc:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *sniffListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

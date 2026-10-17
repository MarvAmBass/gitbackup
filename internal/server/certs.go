package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

type certSummary struct {
	subject     string
	names       []string
	notAfter    time.Time
	fingerprint string
}

func (c certSummary) fill(st *State) {
	st.Subject, st.Names, st.NotAfter, st.Fingerprint = c.subject, c.names, c.notAfter, c.fingerprint
}

func certInfo(cert tls.Certificate) certSummary {
	if len(cert.Certificate) == 0 {
		return certSummary{}
	}
	x, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return certSummary{}
	}
	names := append([]string{}, x.DNSNames...)
	for _, ip := range x.IPAddresses {
		names = append(names, ip.String())
	}
	sum := sha256.Sum256(x.Raw)
	var fp []string
	for _, b := range sum {
		fp = append(fp, fmt.Sprintf("%02X", b))
	}
	return certSummary{subject: x.Subject.CommonName, names: names, notAfter: x.NotAfter, fingerprint: strings.Join(fp, ":")}
}

// selfSigned loads dir/selfsigned.{crt,key} or (re)creates them when missing,
// expiring within 30 days, or issued for other names.
func selfSigned(dir string, hostnames []string) (tls.Certificate, certSummary, error) {
	names := normNames(hostnames)
	crt, key := filepath.Join(dir, "selfsigned.crt"), filepath.Join(dir, "selfsigned.key")
	if cert, err := tls.LoadX509KeyPair(crt, key); err == nil {
		info := certInfo(cert)
		if time.Until(info.notAfter) > 30*24*time.Hour && slices.Equal(normNames(info.names), names) {
			return cert, info, nil
		}
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, certSummary{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "gitbackup", Organization: []string{"gitbackup (self-signed)"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(397 * 24 * time.Hour), // browsers accept at most ~398 days
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, certSummary{}, err
	}
	kb, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, certSummary{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, certSummary{}, err
	}
	if err := os.WriteFile(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, certSummary{}, err
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return tls.Certificate{}, certSummary{}, err
	}
	cert, err := tls.LoadX509KeyPair(crt, key)
	return cert, certInfo(cert), err
}

// normNames: configured names plus localhost, sorted and unique.
func normNames(in []string) []string {
	set := map[string]bool{"localhost": true, "127.0.0.1": true}
	if len(in) == 0 {
		if h, err := os.Hostname(); err == nil {
			set[h] = true
		}
	}
	for _, n := range in {
		if n = strings.TrimSpace(strings.ToLower(n)); n != "" {
			set[n] = true
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// fileCert serves a provided certificate and picks up renewals (e.g. by an
// external ACME client) by watching the files' modification time.
type fileCert struct {
	cert, key string

	mu      sync.Mutex
	current *tls.Certificate
	mtime   time.Time
	checked time.Time
}

func (f *fileCert) load() (tls.Certificate, error) {
	c, err := tls.LoadX509KeyPair(f.cert, f.key)
	if err != nil {
		return c, fmt.Errorf("load %s / %s: %w", f.cert, f.key, err)
	}
	f.mu.Lock()
	f.current = &c
	if fi, err := os.Stat(f.cert); err == nil {
		f.mtime = fi.ModTime()
	}
	f.checked = time.Now()
	f.mu.Unlock()
	return c, nil
}

func (f *fileCert) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	f.mu.Lock()
	stale := time.Since(f.checked) > 30*time.Second
	cur := f.current
	mtime := f.mtime
	if stale {
		f.checked = time.Now()
	}
	f.mu.Unlock()
	if stale {
		if fi, err := os.Stat(f.cert); err == nil && fi.ModTime().After(mtime) {
			if c, err := f.load(); err == nil {
				return &c, nil
			}
		}
	}
	return cur, nil
}

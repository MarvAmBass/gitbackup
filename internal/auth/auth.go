// Package auth protects the UI with a login page and signed session cookies.
// HTTP basic auth keeps working for scripts (curl -u).
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	CookieName  = "gitbackup_session"
	sessionLife = 30 * 24 * time.Hour
	maxFailures = 10               // per client address …
	lockout     = 15 * time.Minute // … then locked for this long
)

// Auth checks credentials for one admin user. An empty password disables it;
// StartupCheck decides whether that is allowed.
type Auth struct {
	User, Password string
	key            []byte // HMAC key for session cookies

	mu       sync.Mutex
	failures map[string]*failure
}

type failure struct {
	n     int
	until time.Time
}

// New loads (or creates) the session key in keyFile. Sessions survive
// restarts; deleting the file logs everybody out.
func New(user, password, keyFile string) (*Auth, error) {
	a := &Auth{User: user, Password: password, failures: map[string]*failure{}}
	if password == "" {
		return a, nil
	}
	b, err := os.ReadFile(keyFile)
	if err != nil || len(b) < 32 {
		b = make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(keyFile, b, 0o600); err != nil {
			return nil, err
		}
	}
	a.key = b
	return a, nil
}

func (a *Auth) Enabled() bool { return a.Password != "" }

func (a *Auth) valid(user, password string) bool {
	u := subtle.ConstantTimeCompare([]byte(user), []byte(a.User))
	p := subtle.ConstantTimeCompare([]byte(password), []byte(a.Password))
	return u&p == 1
}

// ---------------------------------------------------------------- startup

// ErrNoPassword stops the server from running without a login where other
// machines could reach it: the UI can add hooks, which run shell commands.
var ErrNoPassword = errors.New("GITBACKUP_ADMIN_PASSWORD is not set: refusing to serve the UI without a login on a non-loopback address " +
	"(set a password, listen on 127.0.0.1 only, or set GITBACKUP_NO_AUTH=1 if something in front of gitbackup does the authentication)")

// StartupCheck decides whether the server may start. With a password it
// always may. Without one it may only if noAuth is set explicitly
// (GITBACKUP_NO_AUTH=1) or listen is a loopback-only address; warning then
// says why the UI is open.
func StartupCheck(password string, noAuth bool, listen string) (warning string, err error) {
	switch {
	case password != "":
		return "", nil
	case noAuth:
		return "login disabled by GITBACKUP_NO_AUTH=1 – anyone who can reach " + listen + " controls gitbackup, including shell hooks", nil
	case Loopback(listen):
		return "no GITBACKUP_ADMIN_PASSWORD set – login disabled, the UI is only reachable from this machine (" + listen + ")", nil
	}
	return "", ErrNoPassword
}

// Loopback reports whether a listen address ("127.0.0.1:8080", "[::1]:8080",
// "localhost:8080") only accepts connections from this machine. An address
// without a host (":8080") listens on every interface.
func Loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------- sessions

func (a *Auth) sign(payload string) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// cookie value: base64(user|expiry) "." hmac
func (a *Auth) newSession(w http.ResponseWriter, r *http.Request) {
	exp := time.Now().Add(sessionLife)
	payload := base64.RawURLEncoding.EncodeToString([]byte(a.User + "|" + strconv.FormatInt(exp.Unix(), 10)))
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: payload + "." + a.sign(payload), Path: "/", Expires: exp,
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode})
}

// Session returns the logged-in user from the cookie.
func (a *Auth) Session(r *http.Request) (string, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return "", false
	}
	payload, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(a.sign(payload))) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", false
	}
	user, exp, _ := strings.Cut(string(raw), "|")
	ts, _ := strconv.ParseInt(exp, 10, 64)
	if user != a.User || time.Now().Unix() > ts {
		return "", false
	}
	return user, true
}

// ---------------------------------------------------------------- brute force

func client(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

var ErrLocked = errors.New("too many failed logins – try again later")

func (a *Auth) locked(addr string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.failures[addr]
	return f != nil && time.Now().Before(f.until)
}

func (a *Auth) fail(addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.failures[addr]
	if f == nil {
		f = &failure{}
		a.failures[addr] = f
	}
	f.n++
	if f.n >= maxFailures {
		f.n, f.until = 0, time.Now().Add(lockout)
	}
}

func (a *Auth) succeed(addr string) {
	a.mu.Lock()
	delete(a.failures, addr)
	a.mu.Unlock()
}

// Login checks a form login and starts a session.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request, user, password string) error {
	addr := client(r)
	if a.locked(addr) {
		return ErrLocked
	}
	if !a.valid(user, password) {
		a.fail(addr)
		time.Sleep(time.Second) // slow down guessing
		return errors.New("wrong user name or password")
	}
	a.succeed(addr)
	a.newSession(w, r)
	return nil
}

// Logout ends the session.
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode})
}

// ---------------------------------------------------------------- middleware

// Public paths need no login.
func public(p string) bool {
	return p == "/login" || p == "/healthz" || p == "/robots.txt" || strings.HasPrefix(p, "/static/")
}

// Middleware lets through: public paths, a valid session, or correct basic
// auth (scripts). Browsers are sent to the login page; scripts get a 401.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	if !a.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := a.Session(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		if u, p, ok := r.BasicAuth(); ok {
			addr := client(r)
			if !a.locked(addr) && a.valid(u, p) {
				a.succeed(addr)
				next.ServeHTTP(w, r)
				return
			}
			a.fail(addr)
		}
		if wantsHTML(r) {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="gitbackup"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// wantsHTML: browsers navigating (GET with Accept text/html) get the login
// page; API clients and scripts get a plain 401.
func wantsHTML(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") &&
		!strings.HasPrefix(r.URL.Path, "/api/")
}

// SafeNext keeps redirects after login on this site.
func SafeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, `\`) {
		return "/"
	}
	return next
}

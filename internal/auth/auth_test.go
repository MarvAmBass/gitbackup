package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginFlow(t *testing.T) {
	a, err := New("admin", "s3cret", filepath.Join(t.TempDir(), "session.key"))
	if err != nil {
		t.Fatal(err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret page")) })
	login := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := a.Login(w, r, r.FormValue("username"), r.FormValue("password")); err != nil {
				http.Error(w, err.Error(), 401)
				return
			}
		}
		w.Write([]byte("login page"))
	})
	mux := http.NewServeMux()
	mux.Handle("/login", login)
	mux.Handle("/", ok)
	h := a.Middleware(mux)
	do := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// a browser is sent to the login page, a script gets 401
	req := httptest.NewRequest("GET", "/repos/1", nil)
	req.Header.Set("Accept", "text/html")
	if rec := do(req); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?next=%2Frepos%2F1" {
		t.Fatalf("browser: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do(httptest.NewRequest("GET", "/api/repos.tsv", nil)); rec.Code != 401 || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("script: %d", rec.Code)
	}
	// curl -u keeps working
	req = httptest.NewRequest("GET", "/api/repos.tsv", nil)
	req.SetBasicAuth("admin", "s3cret")
	if rec := do(req); rec.Code != 200 {
		t.Fatalf("basic auth: %d", rec.Code)
	}
	if rec := do(httptest.NewRequest("GET", "/login", nil)); rec.Code != 200 {
		t.Fatal("login page must be public")
	}

	// form login sets a session cookie
	form := url.Values{"username": {"admin"}, "password": {"s3cret"}}
	req = httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := do(req)
	cookies := rec.Result().Cookies()
	if rec.Code != 200 || len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("login: %d %+v", rec.Code, cookies)
	}
	req = httptest.NewRequest("GET", "/repos/1", nil)
	req.AddCookie(cookies[0])
	if rec := do(req); rec.Code != 200 || rec.Body.String() != "secret page" {
		t.Fatalf("with session: %d", rec.Code)
	}
	// a tampered cookie is worthless
	bad := *cookies[0]
	bad.Value = strings.Replace(bad.Value, ".", ".00", 1)
	req = httptest.NewRequest("GET", "/api/x", nil)
	req.AddCookie(&bad)
	if rec := do(req); rec.Code != 401 {
		t.Fatalf("tampered cookie accepted: %d", rec.Code)
	}
	// sessions survive a restart (same key file)
	b, _ := New("admin", "s3cret", filepath.Join(t.TempDir(), "other.key")) // different key: invalid
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookies[0])
	if _, ok := b.Session(req); ok {
		t.Fatal("cookie must not validate under another key")
	}

	// lockout after repeated failures (without the 1s sleep: call fail directly)
	for i := 0; i < maxFailures; i++ {
		a.fail("192.0.2.1")
	}
	req = httptest.NewRequest("POST", "/login", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	if err := a.Login(httptest.NewRecorder(), req, "admin", "s3cret"); err != ErrLocked {
		t.Fatalf("expected lockout, got %v", err)
	}

	for in, want := range map[string]string{"/repos/1?x=2": "/repos/1?x=2", "//evil.com": "/", "https://evil.com": "/", "": "/", `/\evil`: "/"} {
		if got := SafeNext(in); got != want {
			t.Errorf("SafeNext(%q) = %q", in, got)
		}
	}
}

func TestStartupCheck(t *testing.T) {
	for _, c := range []struct {
		password string
		noAuth   bool
		listen   string
		ok, warn bool
	}{
		{"s3cret", false, ":8080", true, false},
		{"s3cret", false, "0.0.0.0:8080", true, false},
		{"", false, ":8080", false, false},
		{"", false, "0.0.0.0:8080", false, false},
		{"", false, "[::]:8080", false, false},
		{"", false, "192.0.2.10:8080", false, false},
		{"", false, "gitbackup.lan:8080", false, false},
		{"", true, ":8080", true, true},
		{"", false, "127.0.0.1:8080", true, true},
		{"", false, "127.0.0.2:8080", true, true},
		{"", false, "[::1]:8080", true, true},
		{"", false, "localhost:8080", true, true},
		{"", false, "LOCALHOST:8080", true, true},
	} {
		warn, err := StartupCheck(c.password, c.noAuth, c.listen)
		if (err == nil) != c.ok || (warn != "") != c.warn {
			t.Errorf("StartupCheck(%q, %v, %q) = %q, %v", c.password, c.noAuth, c.listen, warn, err)
		}
	}
}

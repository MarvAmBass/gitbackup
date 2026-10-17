package streagit

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandles(t *testing.T) {
	root := t.TempDir()
	for _, g := range []string{
		"github.com/a/repos/b/repo.git", "github.com/a/repos/b/repo.v2.git", "github.com/a/repos/b/wiki.git",
		"github.com/a/repos/b/forks/c/repo.git",
		"github.com/whatwg/repos/html/repo.git", // a repository called "html"
		"github.com/html/repos/x/repo.git",      // an owner called "html"
	} {
		os.MkdirAll(filepath.Join(root, g), 0o755)
		os.WriteFile(filepath.Join(root, g, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	}
	// a star is a symlink to the repo's folder
	os.MkdirAll(filepath.Join(root, "github.com/u/stars/a"), 0o755)
	os.Symlink("../../../a/repos/b", filepath.Join(root, "github.com/u/stars/a/b"))
	p := &Process{Root: root}
	for path, want := range map[string]bool{
		"github.com/a/repos/b/html/log.html":             true,
		"github.com/a/repos/b/html":                      true,
		"github.com/a/repos/b/html/commit/abc.html":      true,
		"github.com/a/repos/b/html.v2/files.html":        true,
		"github.com/a/repos/b/html.v3/files.html":        false, // no such version
		"github.com/a/repos/b/html.wiki/log.html":        true,
		"github.com/a/repos/b/html.wiki.v1/log.html":     false, // no such wiki version
		"github.com/a/repos/b/forks/c/html/log.html":     true,
		"github.com/u/stars/a/b/html/log.html":           true,
		"robots.txt":                                     true,
		"github.com/a/repos/b/index.html":                false, // gitbackup's landing page
		"github.com/a/repos/b/releases/v1/x.tar.gz":      false,
		"github.com/a/repos/b/releases/v1/html.tar.gz":   false, // a release file called html.*
		"github.com/a/repos/b/releases/v1/html":          false,
		"github.com/a/repos/b/repo.git/HEAD":             false, // dumb-http clone stays a file
		"github.com/a/repos/htmlx/index.html":            false,
		"github.com/a/repos/b/releases/v1/html-docs.zip": false,
		"github.com/whatwg/repos/html/index.html":        false, // the repo called html: landing page …
		"github.com/whatwg/repos/html/repo.git/HEAD":     false, // … and clone stay with the file server
		"github.com/whatwg/repos/html/html/log.html":     true,  // its pages are rendered
		"github.com/html/repos/x/index.html":             false,
		"github.com/html/index.html":                     false,
		"github.com/html/repos/x/html/refs.html":         true,
		"../gitbackup.yaml":                              false,
		"github.com/a/repos/b/html/../../../x":           false,
	} {
		if got := p.Handles(path); got != want {
			t.Errorf("Handles(%q) = %v, want %v", path, got, want)
		}
	}
	for seg, want := range map[string]string{"html": "repo.git", "html.v2": "repo.v2.git", "html.wiki": "wiki.git", "html.wiki.v1": "wiki.v1.git",
		"html.v0": "", "html.v01": "", "html.": "", "html..v1": "", "htmlx": "", "html.a.b.c": "", "html.tar.gz": "", "html.v1.wiki": ""} {
		if got := htmlGitDir(seg); got != want {
			t.Errorf("htmlGitDir(%q) = %q, want %q", seg, got, want)
		}
	}
}

func TestProxyKeepsPathAndReportsDowntime(t *testing.T) {
	var got string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.Write([]byte("rendered"))
	}))
	p := &Process{Listen: strings.TrimPrefix(fake.URL, "http://"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rec := httptest.NewRecorder()
	p.Proxy().ServeHTTP(rec, httptest.NewRequest("GET", "/browse/github.com/a/repos/b/html/log.html", nil))
	if rec.Body.String() != "rendered" || got != "/browse/github.com/a/repos/b/html/log.html" {
		t.Fatalf("proxy: %q path=%q", rec.Body.String(), got)
	}
	fake.Close()
	rec = httptest.NewRecorder()
	p.Proxy().ServeHTTP(rec, httptest.NewRequest("GET", "/browse/x/html/log.html", nil))
	if rec.Code != http.StatusBadGateway || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("down: %d", rec.Code)
	}
}

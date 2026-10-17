package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MarvAmBass/gitbackup/internal/api"
)

func client() *api.Client {
	return api.New("test", nopCache{}, func() api.Config { return api.Config{} })
}

func TestGitLab(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Gitlab-Meta", `{"version":"1"}`)
		switch r.URL.EscapedPath() {
		case "/api/v4/users":
			if r.URL.Query().Get("username") == "alice" {
				fmt.Fprint(w, `[{"id":5,"username":"alice"}]`)
			} else {
				fmt.Fprint(w, `[]`)
			}
		case "/api/v4/users/5/projects":
			fmt.Fprint(w, `[{"id":1,"path":"dots","path_with_namespace":"alice/dots","namespace":{"full_path":"alice"},
				"http_url_to_repo":"https://gl/alice/dots.git","last_activity_at":"2026-01-01T00:00:00Z"}]`)
		case "/api/v4/groups/org%2Fsub":
			fmt.Fprint(w, `{"id":9}`)
		case "/api/v4/groups/org%2Fsub/projects":
			fmt.Fprint(w, `[{"id":2,"path":"tool","path_with_namespace":"org/sub/deep/tool","namespace":{"full_path":"org/sub/deep"},
				"http_url_to_repo":"https://gl/org/sub/deep/tool.git","forked_from_project":{"id":1}}]`)
		case "/api/v4/projects":
			fmt.Fprint(w, `[]`)
		case "/api/v4/projects/org%2Fsub":
			http.NotFound(w, r) // a group, not a project
		case "/api/v4/projects/2/releases":
			fmt.Fprint(w, `[{"tag_name":"v1","name":"One","description":"notes","released_at":"2026-02-01T00:00:00Z",
				"assets":{"links":[{"id":3,"name":"bin","url":"https://gl/x","direct_asset_url":"https://gl/direct"}]}}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewGitLab("gl", srv.URL, client())
	ctx := context.Background()

	if typ, err := p.AccountType(ctx, "alice", false); err != nil || typ != "User" {
		t.Fatalf("user: %s %v", typ, err)
	}
	if typ, err := p.AccountType(ctx, "org/sub", false); err != nil || typ != "Group" {
		t.Fatalf("group: %s %v", typ, err)
	}
	if _, err := p.Repo(ctx, "org/sub", false); err != ErrNotFound {
		t.Fatalf("group as project should be not found: %v", err)
	}
	rs, err := p.Repos(ctx, "alice", false)
	if err != nil || len(rs) != 1 || rs[0].Owner != "alice" || rs[0].Name != "dots" || rs[0].ExtID != "1" {
		t.Fatalf("user repos: %+v %v", rs, err)
	}
	rs, err = p.Repos(ctx, "org/sub", false)
	if err != nil || len(rs) != 1 || rs[0].Owner != "org/sub/deep" || !rs[0].Fork {
		t.Fatalf("group repos: %+v %v", rs, err)
	}
	rels, err := p.Releases(ctx, Ref{Owner: "org/sub/deep", Name: "tool", ExtID: "2"}, false)
	if err != nil || len(rels) != 1 || rels[0].Assets[0].URL != "https://gl/direct" {
		t.Fatalf("releases: %+v %v", rels, err)
	}
	if !isGitLab(ctx, client(), srv.URL) {
		t.Fatal("detect gitlab")
	}
}

func TestGitHubGistsAndReleases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/bob/gists":
			fmt.Fprint(w, `[{"id":"abc123","description":"","git_pull_url":"https://gist.github.com/abc123.git",
				"updated_at":"2026-03-01T00:00:00Z","owner":{"login":"bob"},"files":{"notes.md":{"filename":"notes.md"}}}]`)
		case "/repos/bob/tool/releases":
			fmt.Fprint(w, `[{"id":7,"tag_name":"v2","name":"Two","draft":false,"published_at":"2026-03-02T00:00:00Z",
				"assets":[{"id":8,"name":"tool.zip","size":10,"updated_at":"2026-03-02T00:00:00Z","digest":"sha256:ff",
				"browser_download_url":"https://github.com/bob/tool/releases/download/v2/tool.zip"}]}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewGitHub("github.com", "https://github.com", client())
	p.apiURL = srv.URL
	ctx := context.Background()
	gs, err := p.Gists(ctx, "bob", false)
	if err != nil || len(gs) != 1 || gs[0].Name != "abc123" || gs[0].Description != "notes.md" || gs[0].ExtID != "gist:abc123" {
		t.Fatalf("gists: %+v %v", gs, err)
	}
	rels, err := p.Releases(ctx, Ref{Owner: "bob", Name: "tool"}, false)
	if err != nil || len(rels) != 1 || rels[0].Assets[0].Digest != "sha256:ff" || rels[0].Assets[0].Size != 10 {
		t.Fatalf("releases: %+v %v", rels, err)
	}
}

// Shaped like https://git.sr.ht/~user (2026).
const srhtPage = `<html><head><link rel="next" href="?page=2" /></head><body>
  <div class="event">
    <h4>
      <a href="/~alice/abuild">
        ~alice/abuild
      </a>

    </h4>

    <p>Alpine &amp; friends</p>
  </div>
  <div class="event">
    <h4>
      <a href="/~alice/scdoc">
        ~alice/scdoc
      </a>
    </h4>
  </div>
  <a href="?page=2">next</a>
</body></html>`

func TestSourcehutHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/~alice" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `<div class="event"><h4><a href="/~alice/last">~alice/last</a></h4></div>`)
			return
		}
		fmt.Fprint(w, srhtPage)
	}))
	defer srv.Close()
	p := NewSourcehut("sr", srv.URL, client())
	rs, err := p.Repos(context.Background(), "alice", false)
	if err != nil || len(rs) != 3 {
		t.Fatalf("repos: %+v %v", rs, err)
	}
	if rs[0].Owner != "~alice" || rs[0].Name != "abuild" || rs[0].Description != "Alpine & friends" ||
		!strings.HasSuffix(rs[0].CloneURL, "/~alice/abuild") || rs[2].Name != "last" {
		t.Fatalf("parsed: %+v", rs)
	}
	if r, _ := p.Repo(context.Background(), "alice/scdoc", false); r.Owner != "~alice" || r.CloneURL != srv.URL+"/~alice/scdoc" {
		t.Fatalf("repo: %+v", r)
	}
}

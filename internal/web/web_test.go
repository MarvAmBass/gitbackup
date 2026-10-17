package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/config"
	"github.com/MarvAmBass/gitbackup/internal/engine"
	"github.com/MarvAmBass/gitbackup/internal/pages"
	"github.com/MarvAmBass/gitbackup/internal/provider"
	"github.com/MarvAmBass/gitbackup/internal/releases"
	"github.com/MarvAmBass/gitbackup/internal/store"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, msg string) {
	os.WriteFile(filepath.Join(dir, "f"), []byte(msg), 0o644)
	git(t, dir, "add", "f")
	git(t, dir, "commit", "-q", "-m", msg)
}

// fakeForgejo serves the handful of Gitea/Forgejo API endpoints gitbackup uses.
type fakeForgejo struct {
	mu       sync.Mutex
	up       string // directory holding the upstream repos
	asset    string // content of the release asset
	assetVer int
}

func (f *fakeForgejo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	base := "http://" + r.Host
	file := func(name string) string { return "file://" + filepath.Join(f.up, name) }
	// no rate-limit headers: quota unknown, so background work is not paced here (see api tests)
	repo := func(id int, owner, name, clone string, fork bool, created, updated string) string {
		return fmt.Sprintf(`{"id":%d,"name":%q,"owner":{"login":%q},"description":"disk encryption","clone_url":%q,"fork":%v,
			"html_url":"%s/%s/%s","default_branch":"main","created_at":%q,"updated_at":%q}`, id, name, owner, clone, fork, base, owner, name, created, updated)
	}
	switch r.URL.Path {
	case "/api/v1/version":
		fmt.Fprint(w, `{"version":"12.0.0+gitea-1.22.0"}`)
	case "/api/v1/orgs/team":
		fmt.Fprint(w, `{"id":7,"username":"team"}`)
	case "/api/v1/users/alice":
		fmt.Fprint(w, `{"id":8,"login":"alice"}`)
	case "/api/v1/orgs/team/repos":
		fmt.Fprintf(w, `[%s]`, repo(42, "team", "cryptotool", file("cryptotool"), false, "2020-01-01T00:00:00Z", "2026-09-28T10:00:00Z"))
	case "/api/v1/users/alice/repos":
		fmt.Fprintf(w, `[%s]`, repo(50, "alice", "cryptotool", file("fork-alice"), true, "2026-01-01T00:00:00Z", "2026-09-01T00:00:00Z"))
	case "/api/v1/repos/team/cryptotool/forks":
		fmt.Fprintf(w, `[%s,%s,%s]`,
			repo(50, "alice", "cryptotool", file("fork-alice"), true, "2026-01-01T00:00:00Z", "2026-09-01T00:00:00Z"),
			repo(51, "bob", "cryptotool", file("fork-bob"), true, "2026-01-01T00:00:00Z", "2026-08-01T00:00:00Z"),
			repo(52, "carol", "cryptotool", file("does-not-exist"), true, "2026-02-01T00:00:00Z", "2026-02-01T00:00:00Z"))
	case "/api/v1/repos/team/cryptotool/releases":
		fmt.Fprintf(w, `[{"id":1,"tag_name":"v7.1a","name":"7.1a","body":"full version","published_at":"2026-09-01T10:00:00Z",
			"assets":[{"id":%d,"name":"cryptotool-7.1a.tar.gz","size":%d,"created_at":"2026-09-01T10:00:0%dZ",
			"browser_download_url":"%s/dl/cryptotool-7.1a.tar.gz"}]}]`, 100+f.assetVer, len(f.asset), f.assetVer, base)
	case "/dl/cryptotool-7.1a.tar.gz":
		fmt.Fprint(w, f.asset)
	default:
		if strings.HasSuffix(r.URL.Path, "/releases") || strings.HasSuffix(r.URL.Path, "/forks") {
			fmt.Fprint(w, `[]`)
			return
		}
		http.NotFound(w, r)
	}
}

// TestEndToEnd drives the whole system against a fake Forgejo API with local
// git repositories behind it.
func TestEndToEnd(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	data, state := filepath.Join(home, "data"), filepath.Join(home, "state")
	os.MkdirAll(data, 0o755)
	os.MkdirAll(state, 0o755)
	upDir := filepath.Join(tmp, "upstream")
	up := filepath.Join(upDir, "cryptotool")
	os.MkdirAll(up, 0o755)
	git(t, up, "init", "-q", "-b", "main")
	commit(t, up, "one")
	commit(t, up, "two: real feature")
	wiki := filepath.Join(upDir, "cryptotool.wiki.git")
	os.MkdirAll(wiki, 0o755)
	git(t, wiki, "init", "-q", "-b", "main")
	commit(t, wiki, "Home")
	git(t, upDir, "clone", "-q", up, "fork-alice")
	for i := 1; i <= 6; i++ {
		commit(t, filepath.Join(upDir, "fork-alice"), fmt.Sprintf("alice %d", i))
	}
	git(t, upDir, "clone", "-q", up, "fork-bob")
	commit(t, filepath.Join(upDir, "fork-bob"), "bob 1")

	forge := &fakeForgejo{up: upDir, asset: "original release bytes"}
	fsrv := httptest.NewServer(forge)
	defer fsrv.Close()

	cfg, err := config.Open(filepath.Join(home, "gitbackup.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	hookLog := filepath.Join(tmp, "hook.log")
	st, err := store.Open(filepath.Join(state, "gitbackup.db"), "wal")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	forges := provider.NewRegistry(st, func(host string) (provider.Instance, bool) {
		in, ok := cfg.Get().Instance(host)
		return provider.Instance{Host: in.Host, Platform: provider.Platform(in.Platform), BaseURL: in.BaseURL, Token: in.TokenValue(),
			ReservePct: in.Reserve}, ok
	})
	pb := &pages.Builder{DataDir: data}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rs := &releases.Syncer{Store: st, DataDir: data, HTTP: fsrv.Client()}
	eng := engine.New(st, cfg, forges, rs, pb, data, state, log)
	cfg.OnChange(func(c *config.Config) {
		if err := st.SyncConfig(c); err != nil {
			t.Errorf("sync config: %v", err)
		}
	})
	if err := st.SyncConfig(cfg.Get()); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Store: st, Engine: eng, Forges: forges, Cfg: cfg, DataDir: data, Version: "test", Log: log, BaseCtx: ctx}
	h, err := srv.Handler()
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path string, form url.Values) (int, string) {
		t.Helper()
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req := httptest.NewRequest(method, path, body)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if loc := rec.Header().Get("Location"); strings.Contains(loc, "err=") {
			t.Errorf("%s %s → %s", method, path, loc)
		}
		return rec.Code, rec.Body.String()
	}
	slot := filepath.Join(data, "127.0.0.1", "team", "repos", "cryptotool")
	yaml := func() string { b, _ := os.ReadFile(cfg.Path()); return string(b) }

	// ---- configure through the UI: everything ends up in gitbackup.yaml
	do("POST", "/hooks", url.Values{"name": {"log"}, "events": {"history"}, "run": {`echo "$GB_EVENT $GB_REPO" >> ` + hookLog}})
	if code, _ := do("POST", "/sources", url.Values{"target": {fsrv.URL + "/team"}, "plan": {"high"}, "repos": {"on"}}); code != http.StatusSeeOther {
		t.Fatalf("add source: %d", code)
	}
	for _, want := range []string{"target: team", "platform: gitea", "host: 127.0.0.1", "name: log", "events:\n      - history"} {
		if !strings.Contains(yaml(), want) {
			t.Fatalf("gitbackup.yaml lacks %q:\n%s", want, yaml())
		}
	}
	// a cross-origin POST is rejected
	req := httptest.NewRequest("POST", "/sources", strings.NewReader("target=x"))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("csrf: %d", rec.Code)
	}

	// ---- discovery + first check: repo, wiki, releases, fork scan
	srcs, _ := st.Sources()
	if len(srcs) != 1 || srcs[0].Platform != "gitea" || srcs[0].Kind != "account" {
		t.Fatalf("sources: %+v", srcs)
	}
	eng.Discover(ctx, srcs[0], false)
	due, _ := st.DueRepos(1<<62, 10, nil)
	if len(due) != 1 || due[0].Path != "127.0.0.1/team/repos/cryptotool" {
		t.Fatalf("due: %+v", due)
	}
	eng.Check(ctx, due[0])
	r, _ := st.Repo(due[0].ID)
	if r.State != "ok" || r.LastCommit == 0 || r.WikiState != "" || r.ReleasesChecked != 0 {
		t.Fatalf("the first check only clones (wiki, releases, forks come later): %+v", r)
	}
	st.TriggerCheck(r.ID)
	r, _ = st.Repo(r.ID)
	eng.Check(ctx, r)
	r, _ = st.Repo(r.ID)
	if r.State != "ok" || r.WikiState != "ok" || r.ReleasesChecked == 0 || r.ForksChecked == 0 {
		t.Fatalf("after first check: state=%s err=%s wiki=%s releases=%d forks=%d", r.State, r.LastError, r.WikiState, r.ReleasesChecked, r.ForksChecked)
	}
	for _, p := range []string{"repo.git/HEAD", "wiki.git/HEAD", "releases/v7.1a/release.json", "releases/v7.1a/cryptotool-7.1a.tar.gz"} {
		if _, err := os.Stat(filepath.Join(slot, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	forks, _ := st.ForksOf(r.ID)
	if len(forks) != 1 || forks[0].Owner != "alice" || forks[0].Ahead != 6 || forks[0].Path != "127.0.0.1/team/repos/cryptotool/forks/alice" {
		t.Fatalf("forks (alice +6 kept, bob +1 and never-pushed carol skipped): %+v", forks)
	}
	eng.Check(ctx, forks[0])
	if b, _ := os.ReadFile(filepath.Join(slot, "forks", "alice", "repo.git", "objects", "info", "alternates")); !strings.Contains(string(b), "repo.git/objects") {
		t.Fatalf("fork must borrow objects from its parent, alternates=%q", b)
	}

	// ---- a followed account owns that fork too: stored once, linked twice
	err = cfg.Update(func(c *config.Config) error {
		c.Sources = append(c.Sources, config.Source{Target: "alice", Platform: "gitea", Host: "127.0.0.1", Kind: "account", Plan: "default", Forks: true})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	alice, _ := st.SourceByKey("gitea", "127.0.0.1", "alice")
	eng.Discover(ctx, alice, false)
	link := filepath.Join(data, "127.0.0.1", "alice", "forks", "cryptotool")
	if dst, err := os.Readlink(link); err != nil || dst != "../../team/repos/cryptotool/forks/alice" {
		t.Fatalf("alias symlink: %q %v", dst, err)
	}

	// ---- upstream rewrites history AND swaps the release tarball
	git(t, up, "reset", "-q", "--hard", "HEAD~1")
	commit(t, up, "two: crippled")
	forge.mu.Lock()
	forge.asset, forge.assetVer = "decrypt only", 1
	forge.mu.Unlock()
	st.TriggerCheck(r.ID)
	st.ReleasesChecked(r.ID, 0)
	r, _ = st.Repo(r.ID)
	eng.Check(ctx, r)
	r, _ = st.Repo(r.ID)
	if r.Versions != 1 {
		t.Fatalf("rewrite not recorded: versions=%d", r.Versions)
	}
	clone := filepath.Join(tmp, "v1-clone")
	git(t, tmp, "clone", "-q", filepath.Join(slot, "repo.v1.git"), clone)
	if got := git(t, clone, "log", "-1", "--format=%s"); got != "two: real feature" {
		t.Fatalf("repo.v1.git should hold the state before the rewrite, HEAD=%q", got)
	}
	kept, _ := filepath.Glob(filepath.Join(slot, "releases", "v7.1a", ".versions", "*", "cryptotool-7.1a.tar.gz"))
	if len(kept) != 1 {
		t.Fatalf("old asset not preserved: %v", kept)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(hookLog)
		if strings.Contains(string(b), "rewritten 127.0.0.1/team/cryptotool") && strings.Contains(string(b), "asset-replaced") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("hook did not run, log=%q", b)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// ---- reconcile rebuilds version folders from the git refs
	os.RemoveAll(filepath.Join(slot, "repo.v1.git"))
	eng.Reconcile(ctx)
	if _, err := os.Stat(filepath.Join(slot, "repo.v1.git", "HEAD")); err != nil {
		t.Fatal("reconcile did not restore repo.v1.git")
	}

	// ---- no push according to a fresh listing: no git request at all
	r, _ = st.Repo(r.ID)
	st.LinkRepo(srcs[0].ID, r.ID, "own", time.Now().Unix()+100) // listing newer than our last check, pushed_at old
	commit(t, up, "pushed but not listed yet")
	st.Postpone(r.ID, 0)
	r, _ = st.Repo(r.ID)
	before := r.LastChange
	eng.Check(ctx, r)
	if r2, _ := st.Repo(r.ID); r2.LastChange != before {
		t.Fatal("unchanged repo (per listing) must not be fetched")
	}
	st.TriggerCheck(r.ID) // "Check now" always runs git
	r, _ = st.Repo(r.ID)
	eng.Check(ctx, r)
	if r2, _ := st.Repo(r.ID); r2.LastChange == before {
		t.Fatal("a forced check must fetch")
	}

	// ---- index and landing pages
	cctx, cancel := context.WithCancel(ctx)
	landing := filepath.Join(slot, "index.html")
	go func() {
		for cctx.Err() == nil {
			if b, err := os.ReadFile(landing); err == nil && strings.Contains(string(b), "repo.v1.git") {
				cancel()
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	go func() { time.Sleep(10 * time.Second); cancel() }()
	eng.Run(cctx)
	if b, _ := os.ReadFile(landing); !strings.Contains(string(b), "repo.v1.git") || !strings.Contains(string(b), "forks/alice/index.html") {
		t.Fatalf("landing page:\n%s", b)
	}

	for path, want := range map[string]string{
		"/":                     "History changes preserved",
		"/sources":              "Gitea / Forgejo",
		"/sources/1":            "cryptotool",
		"/plans":                "adaptive",
		"/repos":                "1 version",
		"/repos/1":              "two: real feature", // the dropped commit is shown
		"/events?kind=history":  "asset-replaced",
		"/events?kind=releases": "releases-imported",
		"/settings":             "127.0.0.1",
		"/browse/":              "cryptotool",
		"/browse/127.0.0.1/team/repos/cryptotool/releases/v7.1a/cryptotool-7.1a.tar.gz": "decrypt only",
	} {
		code, body := do("GET", path, nil)
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("GET %s: %d, missing %q", path, code, want)
		}
	}
	if _, body := do("GET", "/repos/1", nil); !strings.Contains(body, "repo.v1.git") || !strings.Contains(body, "alice") {
		t.Error("repo page lacks version clone path or forks")
	}

	// ---- more forms → gitbackup.yaml
	for path, form := range map[string]url.Values{
		"/repos/1/overrides": {"priority": {"high"}, "interval": {"6h"}},
		"/sources/1":         {"plan": {"default"}, "enabled": {"on"}, "repos": {"on"}},
		"/plans":             {"name": {"weekly"}, "priority": {"low"}, "discover_every": {"1w"}, "check_every": {"1w"}, "check_min": {"1d"}, "check_max": {"4w"}, "releases": {"off"}, "forks": {"off"}},
		"/settings":          {"git_workers": {"2"}, "html": {"on"}},
		"/instances":         {"host": {"github.com"}, "token": {"ghp_testtoken1234"}, "reserve": {"30"}, "gap": {"500ms"}},
		"/repos/1/releases":  {},
	} {
		if code, _ := do("POST", path, form); code != http.StatusSeeOther {
			t.Errorf("POST %s: %d", path, code)
		}
	}
	r, _ = st.Repo(1)
	if r.Priority != 2 || r.IntervalOverride.Int64 != 21600 || r.PlanName != "default" {
		t.Errorf("overrides/plan not applied: prio=%d iv=%v plan=%s", r.Priority, r.IntervalOverride, r.PlanName)
	}
	for _, want := range []string{"repo: 127.0.0.1/team/repos/cryptotool", "interval: 6h", "name: weekly", "git_workers: 2", "token: ghp_testtoken1234", "reserve: 30"} {
		if !strings.Contains(yaml(), want) {
			t.Errorf("gitbackup.yaml lacks %q", want)
		}
	}
	if _, body := do("GET", "/settings", nil); strings.Contains(body, "ghp_testtoken1234") {
		t.Error("token leaked into settings page")
	}
	if code, _ := do("GET", "/browse/../gitbackup.yaml", nil); code == 200 {
		t.Error("config must not be reachable via /browse/")
	}
}

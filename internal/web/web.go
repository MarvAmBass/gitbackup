// Package web is the management UI.
package web

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/auth"
	"github.com/MarvAmBass/gitbackup/internal/config"
	"github.com/MarvAmBass/gitbackup/internal/engine"
	"github.com/MarvAmBass/gitbackup/internal/gitops"
	"github.com/MarvAmBass/gitbackup/internal/provider"
	"github.com/MarvAmBass/gitbackup/internal/server"
	"github.com/MarvAmBass/gitbackup/internal/store"
	"github.com/MarvAmBass/gitbackup/internal/streagit"
	"github.com/MarvAmBass/gitbackup/internal/target"
)

//go:embed templates/*.html static/*
var assets embed.FS

type Server struct {
	Store    *store.Store
	Engine   *engine.Engine
	Forges   *provider.Registry
	Cfg      *config.File
	DataDir  string // data/: served read-only at /browse/
	Auth     *auth.Auth
	TLSState func() server.State // nil in tests
	Streagit *streagit.Process   // nil: no on-demand renderer
	Version  string
	Log      *slog.Logger
	BaseCtx  context.Context

	pages map[string]*template.Template
	login *template.Template
}

var funcs = template.FuncMap{
	"ago": func(ts int64) string {
		if ts == 0 {
			return "never"
		}
		return human(time.Since(time.Unix(ts, 0))) + " ago"
	},
	"until": func(ts int64) string {
		d := time.Until(time.Unix(ts, 0))
		if ts == 0 || d <= 0 {
			return "due"
		}
		return "in " + human(d)
	},
	"dur":  func(s int64) string { return human(time.Duration(s) * time.Second) },
	"date": func(ts int64) string { return time.Unix(ts, 0).Format("2006-01-02 15:04") },
	"time": func(t time.Time) string {
		if t.IsZero() {
			return "–"
		}
		return t.Format("2006-01-02 15:04")
	},
	"bytes": func(n int64) string {
		f := float64(n)
		for _, u := range []string{"B", "KB", "MB", "GB", "TB"} {
			if f < 1024 || u == "TB" {
				if u == "B" {
					return fmt.Sprintf("%d B", n)
				}
				return fmt.Sprintf("%.1f %s", f, u)
			}
			f /= 1024
		}
		return ""
	},
	"short": func(s string) string {
		if len(s) > 10 {
			return s[:10]
		}
		return s
	},
	"prio": func(p int) string { return map[int]string{0: "low", 1: "normal", 2: "high"}[p] },
	"trimref": func(s string) string {
		return strings.TrimPrefix(strings.TrimPrefix(s, "refs/heads/"), "refs/")
	},
	"htmlURL":    func(r store.Repo) string { return "/browse/" + target.HTMLDir(r.Path, 0) + "/log.html" },
	"browse":     func(r store.Repo) string { return "/browse/" + r.Path + "/" },
	"versionURL": func(r store.Repo, n int) string { return "/browse/" + target.HTMLDir(r.Path, n) + "/log.html" },
	"gitDir": func(dataDir string, r store.Repo, unit string, n int) string {
		return filepath.Join(dataDir, target.GitDir(r.Path, unit, n))
	},
	"pct": func(a, b int) int {
		if b == 0 {
			return 0
		}
		return a * 100 / b
	},
	"nullint": func(n sql.NullInt64) string {
		if !n.Valid {
			return ""
		}
		return strconv.FormatInt(n.Int64, 10)
	},
	"historyKind":   func(k string) bool { return config.HistoryKinds[k] },
	"platformLabel": func(p string) string { return provider.Platform(p).Label() },
	"goneLabel":     goneLabel,
	"masked": func(tok string) string {
		if len(tok) <= 8 {
			return strings.Repeat("•", len(tok))
		}
		return tok[:4] + "••••••••" + tok[len(tok)-4:]
	},
	"add": func(a, b int) int { return a + b },
	"planCtx": func(p config.Plan, usage map[string]int) map[string]any {
		return map[string]any{"Plan": p, "Used": usage[p.Name]}
	},
	"releaseDir": func(r store.Repo, tag string) string {
		return "/browse/" + target.ReleasesDir(r.Path) + "/" + target.Safe(tag)
	},
	"planReleases": func(ps []store.Plan, id sql.NullInt64) string {
		for _, p := range ps {
			if id.Valid && p.ID == id.Int64 {
				return p.Releases
			}
		}
		return "off"
	},
}

var historyList = []string{"rewritten", "tag-moved", "deleted", "gone", "back", "renamed",
	"asset-replaced", "asset-deleted", "release-deleted", "release-edited", "release-back"}

func human(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		h := int(d.Hours())
		if m := int(d.Minutes()) % 60; m != 0 && h < 10 {
			return fmt.Sprintf("%dh%02dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%.1fy", d.Hours()/24/365)
	}
}

// Handler builds the HTTP handler.
func (s *Server) Handler() (http.Handler, error) {
	s.pages = map[string]*template.Template{}
	names, _ := fs.Glob(assets, "templates/*.html")
	for _, n := range names {
		base := filepath.Base(n)
		if base == "layout.html" || base == "partials.html" || base == "login.html" {
			continue
		}
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials.html", n)
		if err != nil {
			return nil, err
		}
		s.pages[strings.TrimSuffix(base, ".html")] = t
	}

	login, err := template.ParseFS(assets, "templates/login.html")
	if err != nil {
		return nil, err
	}
	s.login = login
	if s.Auth == nil {
		s.Auth = &auth.Auth{}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.loginGet)
	mux.HandleFunc("POST /login", s.loginPost)
	mux.HandleFunc("POST /logout", s.logout)
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.Handle("GET /browse/", s.browse())
	mux.HandleFunc("GET /api/repos.tsv", s.reposTSV)
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("User-agent: *\nDisallow: /\n")) // a backup is nothing to index
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })

	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("GET /sources", s.sources)
	mux.HandleFunc("POST /sources", s.addSource)
	mux.HandleFunc("GET /sources/{id}", s.source)
	mux.HandleFunc("POST /sources/{id}", s.updateSource)
	mux.HandleFunc("POST /sources/{id}/discover", s.discoverSource)
	mux.HandleFunc("POST /sources/{id}/delete", s.deleteSource)
	mux.HandleFunc("GET /plans", s.plans)
	mux.HandleFunc("POST /plans", s.savePlan)
	mux.HandleFunc("POST /plans/delete", s.deletePlan)
	mux.HandleFunc("GET /repos", s.repos)
	mux.HandleFunc("GET /repos/{id}", s.repo)
	mux.HandleFunc("POST /repos/{id}/check", s.checkRepo)
	mux.HandleFunc("POST /repos/{id}/overrides", s.repoOverrides)
	mux.HandleFunc("POST /repos/{id}/releases", s.releasesNow)
	mux.HandleFunc("POST /instances", s.saveInstance)
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /settings", s.settings)
	mux.HandleFunc("POST /settings", s.saveSettings)
	mux.HandleFunc("POST /hooks", s.saveHook)
	mux.HandleFunc("POST /maintenance/retry-errors", s.retryErrors)
	mux.HandleFunc("POST /maintenance/reset-pace", s.resetPace)
	mux.HandleFunc("POST /settings/https", s.saveHTTPS)
	mux.HandleFunc("POST /hooks/delete", s.deleteHook)

	var h http.Handler = http.NewCrossOriginProtection().Handler(mux)
	h = s.Auth.Middleware(h)
	return h, nil
}

// ---------------------------------------------------------------- login

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request, user, errMsg string) {
	if !s.Auth.Enabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	s.login.Execute(w, map[string]any{"Next": auth.SafeNext(r.FormValue("next")), "User": user, "Error": errMsg, "Secure": r.TLS != nil})
}

func (s *Server) loginGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.Auth.Session(r); ok {
		http.Redirect(w, r, auth.SafeNext(r.FormValue("next")), http.StatusSeeOther)
		return
	}
	s.loginPage(w, r, "", "")
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	user := r.FormValue("username")
	if err := s.Auth.Login(w, r, user, r.FormValue("password")); err != nil {
		s.Log.Warn("login failed", "user", user, "from", r.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		s.loginPage(w, r, user, err.Error())
		return
	}
	http.Redirect(w, r, auth.SafeNext(r.FormValue("next")), http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.Auth.Logout(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type page struct {
	LoggedIn    bool
	ConfigError string
	Paused      bool
	Title       string
	Nav         string
	Flash       string
	Error       string
	Version     string
	Data        any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	t := s.pages[name]
	if t == nil {
		http.Error(w, "no template "+name, 500)
		return
	}
	p := page{LoggedIn: s.Auth.Enabled(), ConfigError: errString(s.Cfg.Error()), Paused: s.Cfg.Get().Settings.Paused, Title: title, Nav: name, Flash: r.URL.Query().Get("ok"), Error: r.URL.Query().Get("err"), Version: s.Version, Data: data}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout.html", p); err != nil {
		s.Log.Error("render", "page", name, "err", err)
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func back(w http.ResponseWriter, r *http.Request, to, okMsg string, err error) {
	q := url.Values{}
	if err != nil {
		q.Set("err", err.Error())
	} else if okMsg != "" {
		q.Set("ok", okMsg)
	}
	sep := "?"
	if strings.Contains(to, "?") {
		sep = "&"
	}
	if len(q) == 0 {
		sep = ""
	}
	http.Redirect(w, r, to+sep+q.Encode(), http.StatusSeeOther)
}

func idOf(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func durInput(s int64) string {
	for _, u := range []struct {
		n int64
		s string
	}{{7 * 86400, "w"}, {86400, "d"}, {3600, "h"}, {60, "m"}} {
		if s >= u.n && s%u.n == 0 {
			return strconv.FormatInt(s/u.n, 10) + u.s
		}
	}
	return strconv.FormatInt(s, 10)
}

func init() { funcs["durin"] = durInput }

// ---------------------------------------------------------------- dashboard

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	st, err := s.Store.Stats(now)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	hot, _, _ := s.Store.Repos(store.RepoFilter{Sort: "hot", Limit: 12})
	next, _, _ := s.Store.Repos(store.RepoFilter{Sort: "next", Limit: 8})
	history, _ := s.Store.Events(store.EventFilter{Kinds: historyList, Limit: 10})
	recent, _ := s.Store.Events(store.EventFilter{Limit: 15})
	s.render(w, r, "dashboard", "Dashboard", map[string]any{
		"Stats": st, "Hot": hot, "Next": next, "History": history, "Recent": recent,
		"APIs": s.Forges.States(), "Activity": s.Engine.Activity(),
	})
}

// ---------------------------------------------------------------- repos

func (s *Server) repos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pg, _ := strconv.Atoi(q.Get("page"))
	f := store.RepoFilter{Query: q.Get("q"), State: q.Get("state"), Kind: q.Get("kind"), Sort: q.Get("sort"), Limit: 100, Offset: pg * 100}
	repos, total, err := s.Store.Repos(f)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	qs := url.Values{"q": {f.Query}, "state": {f.State}, "kind": {f.Kind}, "sort": {f.Sort}}
	s.render(w, r, "repos", "Repositories", map[string]any{"Repos": repos, "Total": total, "F": f, "Page": pg,
		"HasNext": (pg+1)*100 < total, "QS": qs.Encode()})
}

type versionView struct {
	gitops.Version
	N    int             // repo.vN.git
	Only []gitops.Commit // commits reachable only from this version
}

func (s *Server) repo(w http.ResponseWriter, r *http.Request) {
	rp, err := s.Store.Repo(idOf(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	srcs, roles, _ := s.Store.Linked(rp.ID)
	events, _ := s.Store.Events(store.EventFilter{RepoID: rp.ID, Limit: 200})
	g := &gitops.Repo{Path: filepath.Join(s.DataDir, target.GitDir(rp.Path, target.RepoUnit, 0))}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var versions []versionView
	if vs, err := g.Versions(ctx); err == nil {
		for i := len(vs) - 1; i >= 0; i-- {
			v := versionView{Version: vs[i], N: i + 1}
			var tips []string
			for ref, sha := range vs[i].Refs {
				if strings.HasPrefix(ref, "heads/") {
					tips = append(tips, sha)
				}
			}
			if len(tips) > 0 {
				v.Only, _ = g.Log(ctx, 50, append(tips, "--not", "--branches", "--tags")...)
			}
			versions = append(versions, v)
		}
	}
	recent, _ := g.Log(ctx, 10, "HEAD")
	plans, _ := s.Store.Plans()
	type link struct {
		Source store.Source
		Role   string
	}
	var links []link
	for i := range srcs {
		links = append(links, link{srcs[i], roles[i]})
	}
	rels, _ := s.Store.Releases(rp.ID)
	feat := s.Forges.For(provider.Platform(rp.Platform), rp.Host).Supports()
	forks, _ := s.Store.ForksOf(rp.ID)
	var parent *store.Repo
	if rp.ParentID.Valid {
		if p, err := s.Store.Repo(rp.ParentID.Int64); err == nil {
			parent = &p
		}
	}
	s.render(w, r, "repo", rp.FullName(), map[string]any{"Repo": rp, "Links": links, "Events": events, "Versions": versions,
		"Recent": recent, "Plans": plans, "AbsPath": filepath.Join(s.DataDir, rp.Path), "DataDir": s.DataDir, "Releases": rels,
		"ReleaseSummary": s.Store.ReleaseSummary(rp.ID), "Features": feat, "Forks": forks, "Parent": parent,
		"Aliases": s.Store.RepoAliases(rp.ID)})
}

func (s *Server) checkRepo(w http.ResponseWriter, r *http.Request) {
	id := idOf(r)
	err := s.Store.TriggerCheck(id)
	s.Engine.Wake()
	back(w, r, fmt.Sprintf("/repos/%d", id), "Check queued", err)
}

func (s *Server) releasesNow(w http.ResponseWriter, r *http.Request) {
	id := idOf(r)
	err := s.Store.ReleasesChecked(id, 0)
	if err == nil {
		err = s.Store.TriggerCheck(id)
	}
	s.Engine.Wake()
	back(w, r, fmt.Sprintf("/repos/%d", id), "Release sync queued (runs with the next check; plan must have releases enabled)", err)
}

// ---------------------------------------------------------------- events

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	f := store.EventFilter{Limit: 500}
	kind := r.URL.Query().Get("kind")
	switch kind {
	case "history":
		f.Kinds = historyList
	case "releases":
		f.Kinds = []string{"releases-imported", "release-new", "release-edited", "release-deleted", "release-back", "asset-replaced", "asset-deleted"}
	case "errors":
		f.Kinds = []string{"error"}
	case "discovery":
		f.Kinds = []string{"discovered", "unstarred", "unlisted", "renamed"}
	}
	evs, err := s.Store.Events(f)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.render(w, r, "events", "Events", map[string]any{"Events": evs, "Kind": kind})
}

// reposTSV lists every repository as "folder<TAB>upstream URL<TAB>kind" –
// for scripts and quick greps.
func (s *Server) reposTSV(w http.ResponseWriter, r *http.Request) {
	repos, err := s.Store.AllRepos()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/tab-separated-values; charset=utf-8")
	for _, rp := range repos {
		fmt.Fprintf(w, "%s\t%s\t%s\n", rp.Path, rp.URL, rp.Kind)
	}
}

// goneLabel says where a repo disappeared from: "no longer on GitHub".
func goneLabel(r store.Repo) string {
	switch {
	case r.Platform == string(provider.Git):
		return "no longer at " + r.Host
	case store.DefaultHosts[r.Host]:
		return "no longer on " + provider.Platform(r.Platform).Label()
	}
	return "no longer on " + r.Host
}

// browse serves the data tree; HTML pages go to streagit when it renders on demand.
func (s *Server) browse() http.Handler {
	files := http.StripPrefix("/browse/", http.FileServer(http.Dir(s.DataDir)))
	if s.Streagit == nil {
		return files
	}
	pages := s.Streagit.Proxy()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		set := s.Cfg.Get().Settings
		if set.HTML && s.Streagit.Handles(strings.TrimPrefix(r.URL.Path, "/browse/")) {
			pages.ServeHTTP(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}

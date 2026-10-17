package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/config"
	"github.com/MarvAmBass/gitbackup/internal/provider"
	"github.com/MarvAmBass/gitbackup/internal/server"
	"github.com/MarvAmBass/gitbackup/internal/store"
	"github.com/MarvAmBass/gitbackup/internal/target"
)

// Everything in this file edits gitbackup.yaml (via config.File.Update);
// the database follows through the config's change callback.

// ---------------------------------------------------------------- sources

func (s *Server) sources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.Store.Sources()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	plans, _ := s.Store.Plans()
	s.render(w, r, "sources", "Sources", map[string]any{"Sources": srcs, "Plans": plans, "Platforms": provider.Platforms})
}

func (s *Server) addSource(w http.ResponseWriter, r *http.Request) {
	hint := provider.Platform(r.FormValue("platform"))
	t, err := target.Parse(r.FormValue("target"), hint)
	if err == nil && t.Platform == "" {
		// self-hosted server we don't know yet: ask it what it is
		t.Platform = provider.Detect(r.Context(), t.Host, t.BaseURL)
		err = t.Resolve(t.Platform, t.Path)
	}
	if err != nil {
		back(w, r, "/sources", "", err)
		return
	}
	src := config.Source{Target: t.Path, Platform: string(t.Platform), Host: t.Host, Kind: t.Kind, Plan: r.FormValue("plan"),
		Note: r.FormValue("note"), Repos: true}
	if t.Kind != target.Repo {
		src.Repos, src.Stars = r.FormValue("repos") != "", r.FormValue("stars") != ""
		src.Gists, src.Forks = r.FormValue("gists") != "", r.FormValue("forks") != ""
	}
	err = s.Cfg.Update(func(c *config.Config) error {
		for _, x := range c.Sources {
			if x.Key() == src.Key() {
				return fmt.Errorf("%s on %s is already watched", src.Target, src.Host)
			}
		}
		if t.Platform != provider.Git {
			c.EnsureInstance(t.Host, string(t.Platform), t.BaseURL)
		}
		c.Sources = append(c.Sources, src)
		return nil
	})
	if err != nil {
		back(w, r, "/sources", "", err)
		return
	}
	s.Engine.Wake()
	what := map[string]string{target.Account: "account", target.Repo: "repository", target.Auto: "group or project"}[t.Kind]
	msg := fmt.Sprintf("Added %s %s on %s (%s). Discovery starts shortly.", what, t.Path, t.Host, t.Platform.Label())
	if x, err := s.Store.SourceByKey(src.Platform, src.Host, src.Target); err == nil {
		back(w, r, "/sources/"+strconv.FormatInt(x.ID, 10), msg, nil)
		return
	}
	back(w, r, "/sources", msg, nil)
}

func (s *Server) source(w http.ResponseWriter, r *http.Request) {
	src, err := s.Store.Source(idOf(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	plans, _ := s.Store.Plans()
	repos, total, _ := s.Store.Repos(store.RepoFilter{Source: src.ID, Sort: r.URL.Query().Get("sort"), Limit: 500})
	feat := s.Forges.For(provider.Platform(src.Platform), src.Host).Supports()
	s.render(w, r, "source", src.Target, map[string]any{"Source": src, "Plans": plans, "Repos": repos, "Total": total, "Features": feat})
}

// editSource changes the gitbackup.yaml entry of the source with the given id.
func (s *Server) editSource(id int64, fn func(c *config.Config, i int) error) error {
	src, err := s.Store.Source(id)
	if err != nil {
		return err
	}
	return s.Cfg.Update(func(c *config.Config) error {
		for i := range c.Sources {
			x := c.Sources[i]
			if x.Platform == src.Platform && x.Host == src.Host && x.Target == src.Target {
				return fn(c, i)
			}
		}
		return errors.New("source not found in gitbackup.yaml")
	})
}

func (s *Server) updateSource(w http.ResponseWriter, r *http.Request) {
	id := idOf(r)
	err := s.editSource(id, func(c *config.Config, i int) error {
		x := &c.Sources[i]
		x.Plan = r.FormValue("plan")
		x.Disabled = r.FormValue("enabled") == ""
		x.Note = r.FormValue("note")
		if x.Kind != target.Repo {
			x.Repos, x.Stars = r.FormValue("repos") != "", r.FormValue("stars") != ""
			x.Gists, x.Forks = r.FormValue("gists") != "", r.FormValue("forks") != ""
		}
		return nil
	})
	back(w, r, fmt.Sprintf("/sources/%d", id), "Saved", err)
}

func (s *Server) discoverSource(w http.ResponseWriter, r *http.Request) {
	src, err := s.Store.Source(idOf(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !s.Engine.DiscoverNow(s.BaseCtx, src) {
		back(w, r, fmt.Sprintf("/sources/%d", src.ID), "", errors.New("a discovery is already running, try again in a moment"))
		return
	}
	back(w, r, fmt.Sprintf("/sources/%d", src.ID), "Discovery started (may use the API reserve)", nil)
}

func (s *Server) deleteSource(w http.ResponseWriter, r *http.Request) {
	err := s.editSource(idOf(r), func(c *config.Config, i int) error {
		c.Sources = append(c.Sources[:i], c.Sources[i+1:]...)
		return nil
	})
	s.Engine.SyncViews()
	s.Engine.MarkIndexDirty()
	back(w, r, "/sources", "Source removed. Its repositories stay in the backup.", err)
}

// ---------------------------------------------------------------- plans

func (s *Server) plans(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "plans", "Plans", map[string]any{"Plans": s.Cfg.Get().Plans, "Usage": s.planUsage(),
		"New": config.Plan{Priority: "normal", DiscoverEvery: config.Duration(12 * time.Hour),
			Check:    config.CheckCfg{Every: config.Duration(12 * time.Hour), Min: config.Duration(2 * time.Hour), Max: config.Duration(7 * 24 * time.Hour), Adaptive: true},
			Releases: "meta", ReleasesEvery: config.Duration(7 * 24 * time.Hour), ReleaseMaxMB: 200, Wiki: true, Forks: config.ForksCfg{MinAhead: 5, Max: 20}}})
}

func (s *Server) planUsage() map[string]int {
	u := map[string]int{}
	for _, x := range s.Cfg.Get().Sources {
		u[x.Plan]++
	}
	return u
}

func (s *Server) savePlan(w http.ResponseWriter, r *http.Request) {
	dur := func(k string) config.Duration {
		d, _ := config.ParseDuration(r.FormValue(k))
		return d
	}
	num := func(k string) int { n, _ := strconv.Atoi(strings.TrimSpace(r.FormValue(k))); return n }
	orig := r.FormValue("orig")
	p := config.Plan{
		Name: strings.TrimSpace(r.FormValue("name")), Priority: r.FormValue("priority"), DiscoverEvery: dur("discover_every"),
		Check:    config.CheckCfg{Every: dur("check_every"), Min: dur("check_min"), Max: dur("check_max"), Adaptive: r.FormValue("adaptive") != ""},
		Releases: r.FormValue("releases"), ReleasesEvery: dur("releases_every"), ReleaseMaxMB: num("release_max_mb"), Wiki: r.FormValue("wiki") != "",
		Forks: config.ForksCfg{Enabled: r.FormValue("forks") == "on", MinAhead: num("fork_min_ahead"), Max: num("fork_max")},
	}
	err := s.Cfg.Update(func(c *config.Config) error {
		if orig == "" {
			if _, ok := c.Plan(p.Name); ok {
				return fmt.Errorf("plan %q exists", p.Name)
			}
			c.Plans = append(c.Plans, p)
			return nil
		}
		for i := range c.Plans {
			if c.Plans[i].Name == orig {
				c.Plans[i] = p
				for j := range c.Sources { // follow a rename
					if c.Sources[j].Plan == orig {
						c.Sources[j].Plan = p.Name
					}
				}
				return nil
			}
		}
		return fmt.Errorf("plan %q not found", orig)
	})
	back(w, r, "/plans", "Plan saved", err)
}

func (s *Server) deletePlan(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	err := s.Cfg.Update(func(c *config.Config) error {
		if n := s.planUsage()[name]; n > 0 {
			return fmt.Errorf("plan %s is used by %d source(s)", name, n)
		}
		out := c.Plans[:0]
		for _, p := range c.Plans {
			if p.Name != name {
				out = append(out, p)
			}
		}
		c.Plans = out
		return nil
	})
	back(w, r, "/plans", "Plan deleted", err)
}

// ---------------------------------------------------------------- repo overrides

func (s *Server) repoOverrides(w http.ResponseWriter, r *http.Request) {
	id := idOf(r)
	rp, err := s.Store.Repo(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	o := config.Override{Repo: rp.Path, Priority: r.FormValue("priority"), Paused: r.FormValue("paused") != ""}
	if v := strings.TrimSpace(r.FormValue("interval")); v != "" {
		d, err := config.ParseDuration(v)
		if err != nil {
			back(w, r, fmt.Sprintf("/repos/%d", id), "", err)
			return
		}
		o.Interval = d
	}
	err = s.Cfg.Update(func(c *config.Config) error { c.SetOverride(o); return nil })
	back(w, r, fmt.Sprintf("/repos/%d", id), "Overrides saved", err)
}

// ---------------------------------------------------------------- settings

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	c := s.Cfg.Get()
	usage := map[string]int{}
	for _, x := range c.Sources {
		usage[x.Host]++
	}
	var tlsState server.State
	if s.TLSState != nil {
		tlsState = s.TLSState()
	}
	s.render(w, r, "settings", "Settings", map[string]any{
		"C": c, "Usage": usage, "TLS": tlsState, "Secure": r.TLS != nil, "Platforms": provider.Platforms[:4], "ConfigPath": s.Cfg.Path(),
		"DataDir":       s.DataDir,
		"StreagitAvail": s.Streagit != nil && s.Streagit.Available(), "StreagitUp": s.Streagit != nil && s.Streagit.Running(),
	})
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("git_workers")))
	if err != nil || n < 1 || n > 32 {
		back(w, r, "/settings", "", errors.New("parallel git checks must be between 1 and 32"))
		return
	}
	err = s.Cfg.Update(func(c *config.Config) error {
		c.Settings.GitWorkers = n
		c.Settings.HTML = r.FormValue("html") != ""
		c.Settings.Paused = r.FormValue("paused") != ""
		return nil
	})
	s.Engine.MarkIndexDirty()
	back(w, r, "/settings", "Settings saved", err)
}

// saveInstance adds or updates a forge instance (token, budget, platform).
func (s *Server) saveInstance(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(strings.TrimSpace(r.FormValue("host")))
	baseURL := strings.TrimSuffix(strings.TrimSpace(r.FormValue("base_url")), "/")
	plat := r.FormValue("platform")
	if host == "" { // new self-hosted instance
		u, err := url.Parse(baseURL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
			back(w, r, "/settings", "", fmt.Errorf("invalid base URL %q (e.g. https://gitlab.company.lan)", baseURL))
			return
		}
		host = strings.ToLower(u.Hostname())
		if plat == "" || plat == "auto" {
			plat = string(provider.Detect(r.Context(), host, baseURL))
		}
		if plat == string(provider.Git) {
			back(w, r, "/settings", "", fmt.Errorf("%s does not look like GitHub, GitLab, Gitea/Forgejo or sourcehut", baseURL))
			return
		}
	}
	err := s.Cfg.Update(func(c *config.Config) error {
		c.EnsureInstance(host, plat, baseURL)
		for i := range c.Instances {
			in := &c.Instances[i]
			if in.Host != host {
				continue
			}
			if plat != "" && plat != "auto" {
				in.Platform = plat
			}
			if baseURL != "" {
				in.BaseURL = baseURL
			}
			if v, err := strconv.Atoi(r.FormValue("reserve")); err == nil {
				in.Reserve = v
			}
			if v := r.FormValue("gap"); v != "" {
				d, err := config.ParseDuration(v)
				if err != nil {
					return err
				}
				in.Gap = d
			}
			switch tok := strings.TrimSpace(r.FormValue("token")); {
			case r.FormValue("clear_token") != "":
				in.Token = ""
			case tok != "":
				in.Token = tok // a value like ${MY_TOKEN} is kept as a reference
			}
		}
		return nil
	})
	back(w, r, "/settings", "Saved "+host, err)
}

// ---------------------------------------------------------------- hooks

func (s *Server) saveHook(w http.ResponseWriter, r *http.Request) {
	var on []string
	for _, k := range strings.FieldsFunc(r.FormValue("events"), func(c rune) bool { return c == ',' || c == ' ' }) {
		on = append(on, strings.TrimSpace(k))
	}
	h := config.Hook{Name: strings.TrimSpace(r.FormValue("name")), Events: on, Run: strings.TrimSpace(r.FormValue("run"))}
	if v := r.FormValue("timeout"); v != "" {
		d, err := config.ParseDuration(v)
		if err != nil {
			back(w, r, "/settings", "", err)
			return
		}
		h.Timeout = d
	}
	err := s.Cfg.Update(func(c *config.Config) error {
		if h.Name == "" {
			return errors.New("a hook needs a name")
		}
		for i := range c.Hooks {
			if c.Hooks[i].Name == h.Name {
				c.Hooks[i] = h
				return nil
			}
		}
		c.Hooks = append(c.Hooks, h)
		return nil
	})
	back(w, r, "/settings", "Hook saved", err)
}

func (s *Server) deleteHook(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	err := s.Cfg.Update(func(c *config.Config) error {
		out := c.Hooks[:0]
		for _, h := range c.Hooks {
			if h.Name != name {
				out = append(out, h)
			}
		}
		c.Hooks = out
		return nil
	})
	back(w, r, "/settings", "Hook removed", err)
}

func (s *Server) retryErrors(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.RetryErrors()
	s.Engine.Wake()
	back(w, r, "/settings", fmt.Sprintf("%d repositories in error state queued for a new check", n), err)
}

func splitList(v string) []string {
	var out []string
	for _, x := range strings.FieldsFunc(v, func(c rune) bool { return c == ',' || c == ' ' || c == '\n' }) {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// saveHTTPS updates settings.https; the web server applies it live.
func (s *Server) saveHTTPS(w http.ResponseWriter, r *http.Request) {
	h := config.HTTPS{
		Mode: r.FormValue("mode"), Port: strings.TrimSpace(r.FormValue("port")), RedirectHTTP: r.FormValue("redirect_http") != "",
		Hostnames: splitList(r.FormValue("hostnames")), CertFile: strings.TrimSpace(r.FormValue("cert_file")),
		KeyFile: strings.TrimSpace(r.FormValue("key_file")), Domains: splitList(r.FormValue("domains")),
		Email: strings.TrimSpace(r.FormValue("email")),
	}
	err := s.Cfg.Update(func(c *config.Config) error { c.Settings.HTTPS = h; return nil })
	msg := "HTTPS turned off"
	if h.Mode != "off" {
		msg = "HTTPS settings applied – see the status below"
	}
	back(w, r, "/settings", msg, err)
}

func (s *Server) resetPace(w http.ResponseWriter, r *http.Request) {
	s.Engine.ResetPace()
	s.Engine.Wake()
	back(w, r, "/settings", "Learned git pace reset – it is learned again from the next throttle", nil)
}

// Package config holds everything the user configures in one YAML file
// (gitbackup.yaml). The web UI edits it, hand edits are picked up while
// running, and comments survive saves from the UI.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config is the whole file.
type Config struct {
	Settings  Settings   `yaml:"settings"`
	Instances []Instance `yaml:"instances"`
	Plans     []Plan     `yaml:"plans"`
	Sources   []Source   `yaml:"sources"`
	Overrides []Override `yaml:"overrides,omitempty"`
	Hooks     []Hook     `yaml:"hooks,omitempty"`
}

type Settings struct {
	GitWorkers int    `yaml:"git_workers"`
	HTML       bool   `yaml:"html"`               // history/file/commit pages (rendered on demand by streagit)
	Stagit     *bool  `yaml:"stagit,omitempty"`   // legacy, unused: old name of html, read once and dropped on save
	Renderer   string `yaml:"renderer,omitempty"` // legacy, unused: streagit is the only renderer
	Paused     bool   `yaml:"paused,omitempty"`   // maintenance: discovery runs, git checks don't
	HTTPS      HTTPS  `yaml:"https"`
}

// HTTPS configures TLS for the web UI. Off by default.
type HTTPS struct {
	Mode         string   `yaml:"mode"`                    // off | self-signed | files | letsencrypt
	Port         string   `yaml:"port,omitempty"`          // "" = same port as HTTP (both protocols); or e.g. ":8443"
	RedirectHTTP bool     `yaml:"redirect_http,omitempty"` // send plain HTTP requests to https://
	Hostnames    []string `yaml:"hostnames,omitempty"`     // self-signed: names/IPs written into the certificate
	CertFile     string   `yaml:"cert_file,omitempty"`     // files: PEM certificate (chain) …
	KeyFile      string   `yaml:"key_file,omitempty"`      // … and key, paths as seen by gitbackup
	Domains      []string `yaml:"domains,omitempty"`       // letsencrypt: public names that reach this service
	Email        string   `yaml:"email,omitempty"`         // letsencrypt: account / expiry notices
}

// Instance is one forge installation.
type Instance struct {
	Host     string   `yaml:"host"`
	Platform string   `yaml:"platform"` // github | gitlab | gitea | sourcehut
	BaseURL  string   `yaml:"base_url"`
	Token    string   `yaml:"token,omitempty"`   // literal or ${ENV_VAR}
	Reserve  int      `yaml:"reserve"`           // percent of the API limit kept for manual actions
	Gap      Duration `yaml:"gap"`               // minimum pause between API requests
	GitGap   Duration `yaml:"git_gap,omitempty"` // pause between git network operations to this host (default 1s)
}

// TokenValue expands ${VAR} references.
func (i Instance) TokenValue() string { return Expand(i.Token) }

// TokenFromEnv reports whether the token is an ${ENV} reference.
func (i Instance) TokenFromEnv() bool { return envRef.MatchString(strings.TrimSpace(i.Token)) }

type Plan struct {
	Name          string     `yaml:"name"`
	Priority      string     `yaml:"priority"` // low | normal | high
	DiscoverEvery Duration   `yaml:"discover_every"`
	Check         CheckCfg   `yaml:"check"`
	Releases      string     `yaml:"releases"`       // off | meta | assets
	ReleasesEvery Duration   `yaml:"releases_every"` // sweep interval; tag changes trigger a sync right away
	ReleaseMaxMB  int        `yaml:"release_max_mb"`
	Wiki          bool       `yaml:"wiki"`
	Forks         ForksCfg   `yaml:"forks"`
	Stagit        *StagitCfg `yaml:"stagit,omitempty"` // legacy, unused (stagit pre-rendering): ignored, dropped on save
}

type CheckCfg struct {
	Every    Duration `yaml:"every"`
	Min      Duration `yaml:"min"`
	Max      Duration `yaml:"max"`
	Adaptive bool     `yaml:"adaptive"`
}

// ForksCfg controls scanning forks of watched repos.
type ForksCfg struct {
	Enabled  bool `yaml:"enabled"`
	MinAhead int  `yaml:"min_ahead"` // only keep forks with at least this many own commits
	Max      int  `yaml:"max"`       // per repo
}

// StagitCfg is legacy and unused: kept so configs from the stagit era still load.
type StagitCfg struct {
	OnChange bool     `yaml:"on_change"`
	Every    Duration `yaml:"every,omitempty"` // full rebuild interval, 0 = never
}

type Source struct {
	Target   string `yaml:"target"` // account, owner/name path, or clone URL (plain git)
	Platform string `yaml:"platform"`
	Host     string `yaml:"host"`
	Kind     string `yaml:"kind"` // account | repo | auto
	Plan     string `yaml:"plan"`
	Repos    bool   `yaml:"repos,omitempty"`
	Stars    bool   `yaml:"stars,omitempty"`
	Forks    bool   `yaml:"forks,omitempty"` // the account's own forks
	Gists    bool   `yaml:"gists,omitempty"` // gists (GitHub) / snippets (GitLab, token owner)
	Disabled bool   `yaml:"disabled,omitempty"`
	Note     string `yaml:"note,omitempty"`
}

// Key identifies a source.
func (s Source) Key() string { return s.Platform + "|" + s.Host + "|" + s.Target }

// Override pins per-repo behaviour. Repo is the repo's folder below data/,
// e.g. github.com/pi-hole/repos/pi-hole.
type Override struct {
	Repo     string   `yaml:"repo"`
	Priority string   `yaml:"priority,omitempty"`
	Interval Duration `yaml:"interval,omitempty"`
	Paused   bool     `yaml:"paused,omitempty"`
}

// Hook runs a shell command for matching events. Events lists event kinds,
// "history" (everything where upstream removed or changed something) or "*".
type Hook struct {
	Name    string   `yaml:"name"`
	Events  []string `yaml:"events"`
	Run     string   `yaml:"run"`
	Timeout Duration `yaml:"timeout,omitempty"`
}

// HistoryKinds are events where upstream removed or changed something and
// gitbackup kept the previous state.
var HistoryKinds = map[string]bool{"rewritten": true, "tag-moved": true, "deleted": true, "gone": true,
	"asset-replaced": true, "asset-deleted": true, "release-deleted": true, "release-edited": true}

// Matches reports whether the hook fires for an event kind.
func (h Hook) Matches(kind string) bool {
	for _, o := range h.Events {
		if o == "*" || o == kind || (o == "history" && HistoryKinds[kind]) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- defaults

const week = 7 * 24 * time.Hour

// Default is written on first start.
func Default() *Config {
	h := time.Hour
	return &Config{
		Settings: Settings{GitWorkers: 4, HTML: true},
		Instances: []Instance{
			{Host: "github.com", Platform: "github", BaseURL: "https://github.com", Token: "${GITHUB_TOKEN}", Reserve: 20, Gap: Duration(time.Second)},
		},
		Plans: []Plan{
			{Name: "default", Priority: "normal", DiscoverEvery: Duration(12 * h),
				Check:    CheckCfg{Every: Duration(12 * h), Min: Duration(2 * h), Max: Duration(week), Adaptive: true},
				Releases: "meta", ReleasesEvery: Duration(week), ReleaseMaxMB: 200, Wiki: true, Forks: ForksCfg{MinAhead: 5, Max: 20}},
			{Name: "high", Priority: "high", DiscoverEvery: Duration(3 * h),
				Check:    CheckCfg{Every: Duration(h), Min: Duration(15 * time.Minute), Max: Duration(12 * h), Adaptive: true},
				Releases: "assets", ReleasesEvery: Duration(24 * h), ReleaseMaxMB: 500, Wiki: true, Forks: ForksCfg{Enabled: true, MinAhead: 5, Max: 50}},
			{Name: "archive", Priority: "low", DiscoverEvery: Duration(week),
				Check:    CheckCfg{Every: Duration(week), Min: Duration(24 * h), Max: Duration(4 * week), Adaptive: true},
				Releases: "off", ReleasesEvery: Duration(4 * week), ReleaseMaxMB: 100, Wiki: false, Forks: ForksCfg{MinAhead: 10, Max: 10}},
		},
	}
}

// ---------------------------------------------------------------- lookups

func (c *Config) Plan(name string) (Plan, bool) {
	for _, p := range c.Plans {
		if p.Name == name {
			return p, true
		}
	}
	return Plan{}, false
}

func (c *Config) Instance(host string) (Instance, bool) {
	for _, in := range c.Instances {
		if in.Host == host {
			return in, true
		}
	}
	return Instance{}, false
}

func (c *Config) Override(repo string) (Override, bool) {
	for _, o := range c.Overrides {
		if o.Repo == repo {
			return o, true
		}
	}
	return Override{}, false
}

// SetOverride replaces (or, when empty, removes) the override of a repo.
func (c *Config) SetOverride(o Override) {
	out := c.Overrides[:0]
	for _, x := range c.Overrides {
		if x.Repo != o.Repo {
			out = append(out, x)
		}
	}
	if o.Priority != "" || o.Interval != 0 || o.Paused {
		out = append(out, o)
	}
	c.Overrides = out
}

// EnsureInstance adds a forge instance with defaults if it is missing.
func (c *Config) EnsureInstance(host, platform, baseURL string) {
	if _, ok := c.Instance(host); ok {
		return
	}
	gap := Duration(500 * time.Millisecond)
	tok := ""
	if env := DefaultTokenEnv[host]; env != "" {
		tok = "${" + env + "}"
	}
	if platform == "github" {
		gap = Duration(time.Second)
	}
	c.Instances = append(c.Instances, Instance{Host: host, Platform: platform, BaseURL: baseURL, Token: tok, Reserve: 20, Gap: gap})
}

// DefaultTokenEnv names the env variable new instances of public forges reference.
var DefaultTokenEnv = map[string]string{"github.com": "GITHUB_TOKEN", "gitlab.com": "GITLAB_TOKEN",
	"codeberg.org": "CODEBERG_TOKEN", "git.sr.ht": "SRHT_TOKEN"}

// ---------------------------------------------------------------- validation

var (
	platforms  = map[string]bool{"github": true, "gitlab": true, "gitea": true, "sourcehut": true, "git": true}
	priorities = map[string]bool{"low": true, "normal": true, "high": true}
	kinds      = map[string]bool{"account": true, "repo": true, "auto": true}
)

// Validate checks references and ranges; it also fills harmless defaults.
func (c *Config) Validate() error {
	if c.Settings.GitWorkers <= 0 {
		c.Settings.GitWorkers = 4
	}
	if c.Settings.GitWorkers > 32 {
		return fmt.Errorf("settings.git_workers must be 1–32")
	}
	// keys from before streagit replaced stagit: read once, dropped on the next save
	if c.Settings.Stagit != nil {
		c.Settings.HTML = *c.Settings.Stagit
		c.Settings.Stagit = nil
	}
	c.Settings.Renderer = ""
	if err := c.Settings.HTTPS.normalize(); err != nil {
		return fmt.Errorf("settings.https: %w", err)
	}
	seen := map[string]bool{}
	for i := range c.Instances {
		in := &c.Instances[i]
		if in.Host == "" || !platforms[in.Platform] || in.Platform == "git" {
			return fmt.Errorf("instances[%d]: host and a platform (github, gitlab, gitea, sourcehut) are required", i)
		}
		if seen[in.Host] {
			return fmt.Errorf("instance %s is listed twice", in.Host)
		}
		seen[in.Host] = true
		if in.BaseURL == "" {
			in.BaseURL = "https://" + in.Host
		}
		in.BaseURL = strings.TrimSuffix(in.BaseURL, "/")
		if in.Reserve < 0 || in.Reserve > 90 {
			return fmt.Errorf("instance %s: reserve must be 0–90 (%%)", in.Host)
		}
	}
	seen = map[string]bool{}
	for i := range c.Plans {
		p := &c.Plans[i]
		switch {
		case p.Name == "":
			return fmt.Errorf("plans[%d]: name is required", i)
		case seen[p.Name]:
			return fmt.Errorf("plan %q is listed twice", p.Name)
		}
		seen[p.Name] = true
		if p.Priority == "" {
			p.Priority = "normal"
		}
		if p.Releases == "" {
			p.Releases = "off"
		}
		p.Stagit = nil // obsolete since streagit renders on demand
		if p.ReleasesEvery == 0 {
			p.ReleasesEvery = Duration(week)
		}
		switch {
		case !priorities[p.Priority]:
			return fmt.Errorf("plan %s: priority must be low, normal or high", p.Name)
		case p.Releases != "off" && p.Releases != "meta" && p.Releases != "assets":
			return fmt.Errorf("plan %s: releases must be off, meta or assets", p.Name)
		case p.DiscoverEvery < Duration(5*time.Minute):
			return fmt.Errorf("plan %s: discover_every must be at least 5m", p.Name)
		case p.Check.Min <= 0 || p.Check.Min > p.Check.Max:
			return fmt.Errorf("plan %s: check.min must be > 0 and <= check.max", p.Name)
		case p.Check.Every < p.Check.Min || p.Check.Every > p.Check.Max:
			return fmt.Errorf("plan %s: check.every must be between check.min and check.max", p.Name)
		}
	}
	srcs := map[string]bool{}
	for i := range c.Sources {
		s := &c.Sources[i]
		if s.Kind == "" {
			s.Kind = "account"
		}
		switch {
		case s.Target == "" || s.Host == "" || !platforms[s.Platform] || !kinds[s.Kind]:
			return fmt.Errorf("sources[%d] (%s): target, host, platform and kind are required", i, s.Target)
		case srcs[s.Key()]:
			return fmt.Errorf("source %s on %s is listed twice", s.Target, s.Host)
		}
		srcs[s.Key()] = true
		if _, ok := c.Plan(s.Plan); !ok {
			return fmt.Errorf("source %s: unknown plan %q", s.Target, s.Plan)
		}
		if s.Platform != "git" {
			if _, ok := c.Instance(s.Host); !ok {
				return fmt.Errorf("source %s: no instance for host %s", s.Target, s.Host)
			}
		}
	}
	for _, o := range c.Overrides {
		if o.Priority != "" && !priorities[o.Priority] {
			return fmt.Errorf("override %s: priority must be low, normal or high", o.Repo)
		}
		if o.Interval != 0 && o.Interval < Duration(5*time.Minute) {
			return fmt.Errorf("override %s: interval must be at least 5m", o.Repo)
		}
	}
	for i, h := range c.Hooks {
		if h.Run == "" || len(h.Events) == 0 {
			return fmt.Errorf("hooks[%d] (%s): events and run are required", i, h.Name)
		}
	}
	return nil
}

// PriorityNum maps low/normal/high to 0/1/2.
func PriorityNum(p string) int { return map[string]int{"low": 0, "normal": 1, "high": 2}[p] }

// PriorityName maps 0/1/2 to low/normal/high.
func PriorityName(n int) string { return [...]string{"low", "normal", "high"}[max(0, min(n, 2))] }

// ---------------------------------------------------------------- env

var envRef = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// Expand resolves a value of the form ${VAR}; anything else is returned as is.
func Expand(v string) string {
	if m := envRef.FindStringSubmatch(strings.TrimSpace(v)); m != nil {
		return os.Getenv(m[1])
	}
	return v
}

// ---------------------------------------------------------------- durations

// Duration accepts 90s, 30m, 6h, 2d, 1w (or plain seconds) in YAML and
// writes the shortest exact form back.
type Duration time.Duration

func (d Duration) Seconds() int64 { return int64(time.Duration(d) / time.Second) }

func (d Duration) String() string {
	if d == 0 {
		return "0"
	}
	s := d.Seconds()
	if time.Duration(d)%time.Second != 0 {
		return time.Duration(d).String()
	}
	for _, u := range []struct {
		n int64
		s string
	}{{7 * 86400, "w"}, {86400, "d"}, {3600, "h"}, {60, "m"}} {
		if s%u.n == 0 {
			return strconv.FormatInt(s/u.n, 10) + u.s
		}
	}
	return strconv.FormatInt(s, 10) + "s"
}

// ParseDuration parses the formats Duration accepts.
func ParseDuration(v string) (Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "0" {
		return 0, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return Duration(time.Duration(n) * time.Second), nil
	}
	mult := map[byte]time.Duration{'d': 24 * time.Hour, 'w': week}
	if m, ok := mult[v[len(v)-1]]; ok {
		n, err := strconv.ParseFloat(v[:len(v)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("bad duration %q", v)
		}
		return Duration(time.Duration(n * float64(m))), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("bad duration %q (use e.g. 30m, 6h, 2d, 1w)", v)
	}
	return Duration(d), nil
}

func (h *HTTPS) normalize() error {
	if h.Mode == "" {
		h.Mode = "off"
	}
	if h.Port != "" && !strings.HasPrefix(h.Port, ":") && !strings.Contains(h.Port, ":") {
		h.Port = ":" + h.Port
	}
	switch h.Mode {
	case "off", "self-signed":
	case "files":
		if h.CertFile == "" || h.KeyFile == "" {
			return fmt.Errorf("mode files needs cert_file and key_file")
		}
	case "letsencrypt":
		if len(h.Domains) == 0 || h.Email == "" {
			return fmt.Errorf("mode letsencrypt needs domains (public names) and an email")
		}
	default:
		return fmt.Errorf("mode must be off, self-signed, files or letsencrypt")
	}
	return nil
}

// Enabled reports whether TLS is on.
func (h HTTPS) Enabled() bool { return h.Mode != "" && h.Mode != "off" }

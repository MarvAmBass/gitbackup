package provider

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/api"
)

// Instance is one forge installation (github.com, gitlab.company.lan, …).
type Instance struct {
	Host       string
	Platform   Platform
	BaseURL    string // scheme://host[:port][/prefix]
	Token      string
	ReservePct int
	MinGapMS   int
}

// KnownHosts maps public forges to their platform.
var KnownHosts = map[string]Platform{
	"github.com":       GitHub,
	"gitlab.com":       GitLab,
	"salsa.debian.org": GitLab,
	"gitlab.gnome.org": GitLab,
	"invent.kde.org":   GitLab,
	"framagit.org":     GitLab,
	"codeberg.org":     Gitea,
	"gitea.com":        Gitea,
	"next.forgejo.org": Gitea,
	"git.sr.ht":        Sourcehut,
}

// DefaultHost is where a bare "user" or "owner/repo" input goes per platform.
var DefaultHost = map[Platform]string{GitHub: "github.com", GitLab: "gitlab.com", Gitea: "codeberg.org", Sourcehut: "git.sr.ht"}

// Registry hands out one provider (and API client) per instance.
type Registry struct {
	cache  api.Cache
	lookup func(host string) (Instance, bool) // from gitbackup.yaml, tokens already expanded

	mu      sync.Mutex
	clients map[string]*api.Client
}

func NewRegistry(cache api.Cache, lookup func(host string) (Instance, bool)) *Registry {
	return &Registry{cache: cache, lookup: lookup, clients: map[string]*api.Client{}}
}

func (r *Registry) client(host string, platform Platform) *api.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.clients[host]; c != nil {
		return c
	}
	c := api.New(host, r.cache, func() api.Config {
		in, _ := r.lookup(host)
		cfg := api.Config{Token: in.Token, ReservePct: in.ReservePct, MinGap: time.Duration(in.MinGapMS) * time.Millisecond}
		if platform == Gitea {
			cfg.AuthScheme = "token"
		}
		return cfg
	})
	r.clients[host] = c
	return c
}

// For returns the provider for an instance.
func (r *Registry) For(platform Platform, host string) Provider {
	baseURL := "https://" + host
	if in, ok := r.lookup(host); ok && in.BaseURL != "" {
		baseURL = strings.TrimSuffix(in.BaseURL, "/")
	}
	switch platform {
	case GitHub:
		return NewGitHub(host, baseURL, r.client(host, platform))
	case GitLab:
		return NewGitLab(host, baseURL, r.client(host, platform))
	case Gitea:
		return NewGitea(host, baseURL, r.client(host, platform))
	case Sourcehut:
		return NewSourcehut(host, baseURL, r.client(host, platform))
	}
	return NewGit(host)
}

// ClientState is the quota view for the dashboard.
type ClientState struct {
	Host     string
	Rate     api.Rate
	Counters api.Counters
	Reserve  int
}

// States lists every instance client used so far.
func (r *Registry) States() []ClientState {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ClientState
	for h, c := range r.clients {
		rt := c.Rate()
		out = append(out, ClientState{Host: h, Rate: rt, Counters: c.Counters(), Reserve: rt.Limit * c.Config().ReservePct / 100})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// Detect figures out which forge software runs at baseURL. It returns Git
// when no known API answers.
func Detect(ctx context.Context, host, baseURL string) Platform {
	if p, ok := KnownHosts[host]; ok {
		return p
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := api.New(host, nopCache{}, func() api.Config { return api.Config{} })
	c.HTTP = &http.Client{Timeout: 10 * time.Second}
	switch {
	case isGitea(ctx, c, baseURL):
		return Gitea
	case isGitLab(ctx, c, baseURL):
		return GitLab
	case isSourcehut(ctx, c, baseURL):
		return Sourcehut
	case isGitHubEnterprise(ctx, c, baseURL):
		return GitHub
	}
	return Git
}

func isSourcehut(ctx context.Context, c *api.Client, baseURL string) bool {
	// sourcehut's GraphQL endpoint answers 401 with a telltale message
	resp, err := c.Do(ctx, api.Request{Method: "POST", URL: baseURL + "/query", Body: []byte(`{"query":"{version{major}}"}`), Manual: true})
	if err != nil {
		return strings.Contains(err.Error(), "Authorization header is required")
	}
	return strings.Contains(string(resp.Body), `"version"`)
}

func isGitHubEnterprise(ctx context.Context, c *api.Client, baseURL string) bool {
	resp, err := c.Do(ctx, api.Request{URL: baseURL + "/api/v3/meta", Manual: true})
	return err == nil && resp.Header.Get("X-GitHub-Request-Id") != ""
}

type nopCache struct{}

func (nopCache) CacheGet(string) (string, string, []byte, bool) { return "", "", nil, false }
func (nopCache) CachePut(string, string, string, []byte)        {}

package provider

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/api"
)

// GitHubProvider talks to api.github.com (or a GitHub Enterprise /api/v3).
type GitHubProvider struct {
	base
	apiURL string
}

func NewGitHub(host, baseURL string, c *api.Client) *GitHubProvider {
	apiURL := "https://api.github.com"
	if host != "github.com" {
		apiURL = baseURL + "/api/v3" // GitHub Enterprise Server
	}
	return &GitHubProvider{base: base{host: host, baseURL: baseURL, client: c}, apiURL: apiURL}
}

func (p *GitHubProvider) Platform() Platform { return GitHub }
func (p *GitHubProvider) Supports() Features {
	return Features{Stars: true, Gists: true, Releases: true, Accounts: true, Forks: true, Wiki: true}
}
func (p *GitHubProvider) CloneURL(path string) string { return p.baseURL + "/" + path + ".git" }

type ghRepo struct {
	ID            int64                  `json:"id"`
	Name          string                 `json:"name"`
	Owner         struct{ Login string } `json:"owner"`
	Description   string                 `json:"description"`
	Fork          bool                   `json:"fork"`
	Archived      bool                   `json:"archived"`
	PushedAt      time.Time              `json:"pushed_at"`
	CreatedAt     time.Time              `json:"created_at"`
	CloneURL      string                 `json:"clone_url"`
	HTMLURL       string                 `json:"html_url"`
	DefaultBranch string                 `json:"default_branch"`
}

func (r ghRepo) conv() Repo {
	return Repo{ExtID: strconv.FormatInt(r.ID, 10), Owner: r.Owner.Login, Name: r.Name, Description: r.Description,
		CloneURL: r.CloneURL, WebURL: r.HTMLURL, DefaultBranch: r.DefaultBranch, Fork: r.Fork, Archived: r.Archived, PushedAt: r.PushedAt, CreatedAt: r.CreatedAt}
}

func convAll(rs []ghRepo) []Repo {
	out := make([]Repo, len(rs))
	for i, r := range rs {
		out[i] = r.conv()
	}
	return out
}

// RefreshRate reads /rate_limit, which does not count against the quota.
func (p *GitHubProvider) RefreshRate(ctx context.Context) error {
	resp, err := p.client.Do(ctx, api.Request{URL: p.apiURL + "/rate_limit", Free: true})
	if err != nil {
		return err
	}
	var rl struct {
		Resources struct {
			Core struct {
				Limit, Remaining int
				Reset            int64
			}
		}
	}
	if json.Unmarshal(resp.Body, &rl) == nil && rl.Resources.Core.Limit > 0 {
		c := rl.Resources.Core
		p.client.SetRate(api.Rate{Limit: c.Limit, Remaining: c.Remaining, Reset: time.Unix(c.Reset, 0), Known: true,
			Authenticated: p.client.Config().Token != ""})
	}
	return nil
}

func (p *GitHubProvider) AccountType(ctx context.Context, name string, manual bool) (string, error) {
	a, err := getOne[struct{ Type string }](ctx, p.client, p.apiURL+"/users/"+url.PathEscape(name), manual)
	return a.Type, err
}

func (p *GitHubProvider) Repos(ctx context.Context, account string, manual bool) ([]Repo, error) {
	rs, err := getAll[ghRepo](ctx, p.client, p.apiURL+"/users/"+url.PathEscape(account)+"/repos?type=owner&sort=full_name&per_page=100", manual)
	return convAll(rs), err
}

func (p *GitHubProvider) Starred(ctx context.Context, account string, manual bool) ([]Repo, error) {
	rs, err := getAll[ghRepo](ctx, p.client, p.apiURL+"/users/"+url.PathEscape(account)+"/starred?per_page=100", manual)
	return convAll(rs), err
}

func (p *GitHubProvider) Repo(ctx context.Context, path string, manual bool) (Repo, error) {
	r, err := getOne[ghRepo](ctx, p.client, p.apiURL+"/repos/"+path, manual)
	return r.conv(), err
}

type ghGist struct {
	ID          string                 `json:"id"`
	Description string                 `json:"description"`
	GitPullURL  string                 `json:"git_pull_url"`
	HTMLURL     string                 `json:"html_url"`
	UpdatedAt   time.Time              `json:"updated_at"`
	Owner       struct{ Login string } `json:"owner"`
	Files       map[string]struct {
		Filename string `json:"filename"`
	} `json:"files"`
}

func (p *GitHubProvider) Gists(ctx context.Context, account string, manual bool) ([]Repo, error) {
	gs, err := getAll[ghGist](ctx, p.client, p.apiURL+"/users/"+url.PathEscape(account)+"/gists?per_page=100", manual)
	out := make([]Repo, 0, len(gs))
	for _, g := range gs {
		desc := g.Description
		if desc == "" {
			var names []string
			for n := range g.Files {
				names = append(names, n)
			}
			desc = strings.Join(names, ", ")
		}
		owner := g.Owner.Login
		if owner == "" {
			owner = account
		}
		out = append(out, Repo{ExtID: "gist:" + g.ID, Owner: owner, Name: g.ID, Description: desc, CloneURL: g.GitPullURL,
			WebURL: g.HTMLURL, PushedAt: g.UpdatedAt})
	}
	return out, err
}

type ghRelease struct {
	ID          int64     `json:"id"`
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		ID                 int64  `json:"id"`
		Name               string `json:"name"`
		Size               int64  `json:"size"`
		UpdatedAt          string `json:"updated_at"`
		Digest             string `json:"digest"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func (p *GitHubProvider) Releases(ctx context.Context, r Ref, manual bool) ([]Release, error) {
	rs, err := getAll[ghRelease](ctx, p.client, p.apiURL+"/repos/"+r.Owner+"/"+r.Name+"/releases?per_page=100", manual)
	out := make([]Release, 0, len(rs))
	for _, x := range rs {
		rel := Release{ExtID: strconv.FormatInt(x.ID, 10), Tag: x.TagName, Name: x.Name, Body: x.Body, Published: x.PublishedAt,
			Draft: x.Draft, Prerelease: x.Prerelease}
		for _, a := range x.Assets {
			rel.Assets = append(rel.Assets, Asset{ExtID: strconv.FormatInt(a.ID, 10), Name: a.Name, URL: a.BrowserDownloadURL,
				Size: a.Size, Updated: a.UpdatedAt, Digest: a.Digest})
		}
		out = append(out, rel)
	}
	return out, err
}

func (p *GitHubProvider) Forks(ctx context.Context, r Ref, maxPages int, manual bool) ([]Repo, error) {
	rs, err := getPages[ghRepo](ctx, p.client, p.apiURL+"/repos/"+r.Owner+"/"+r.Name+"/forks?sort=newest&per_page=100", maxPages, manual)
	return convAll(rs), err
}

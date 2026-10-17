package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/api"
)

// GiteaProvider talks to the Gitea API v1, which Forgejo (and Codeberg) share.
type GiteaProvider struct{ base }

func NewGitea(host, baseURL string, c *api.Client) *GiteaProvider {
	return &GiteaProvider{base{host: host, baseURL: baseURL, client: c}}
}

func (p *GiteaProvider) Platform() Platform { return Gitea }
func (p *GiteaProvider) Supports() Features {
	return Features{Stars: true, Releases: true, Accounts: true, Forks: true, Wiki: true}
}
func (p *GiteaProvider) CloneURL(path string) string { return p.baseURL + "/" + path + ".git" }
func (p *GiteaProvider) api(path string) string      { return p.baseURL + "/api/v1" + path }

type gtRepo struct {
	ID            int64                  `json:"id"`
	Name          string                 `json:"name"`
	Owner         struct{ Login string } `json:"owner"`
	Description   string                 `json:"description"`
	Fork          bool                   `json:"fork"`
	Mirror        bool                   `json:"mirror"`
	Archived      bool                   `json:"archived"`
	UpdatedAt     time.Time              `json:"updated_at"`
	CreatedAt     time.Time              `json:"created_at"`
	CloneURL      string                 `json:"clone_url"`
	HTMLURL       string                 `json:"html_url"`
	DefaultBranch string                 `json:"default_branch"`
}

func (r gtRepo) conv() Repo {
	return Repo{ExtID: strconv.FormatInt(r.ID, 10), Owner: r.Owner.Login, Name: r.Name, Description: r.Description,
		CloneURL: r.CloneURL, WebURL: r.HTMLURL, DefaultBranch: r.DefaultBranch, Fork: r.Fork, Archived: r.Archived, PushedAt: r.UpdatedAt, CreatedAt: r.CreatedAt}
}

func convGT(rs []gtRepo) []Repo {
	out := make([]Repo, len(rs))
	for i, r := range rs {
		out[i] = r.conv()
	}
	return out
}

func (p *GiteaProvider) isOrg(ctx context.Context, name string, manual bool) (bool, error) {
	_, err := getOne[json.RawMessage](ctx, p.client, p.api("/orgs/"+url.PathEscape(name)), manual)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (p *GiteaProvider) AccountType(ctx context.Context, name string, manual bool) (string, error) {
	org, err := p.isOrg(ctx, name, manual)
	if err != nil {
		return "", err
	}
	if org {
		return "Organization", nil
	}
	if _, err := getOne[json.RawMessage](ctx, p.client, p.api("/users/"+url.PathEscape(name)), manual); err != nil {
		return "", err
	}
	return "User", nil
}

func (p *GiteaProvider) Repos(ctx context.Context, account string, manual bool) ([]Repo, error) {
	org, err := p.isOrg(ctx, account, manual)
	if err != nil {
		return nil, err
	}
	kind := "/users/"
	if org {
		kind = "/orgs/"
	}
	rs, err := getAll[gtRepo](ctx, p.client, p.api(kind+url.PathEscape(account)+"/repos?limit=50"), manual)
	return convGT(rs), err
}

func (p *GiteaProvider) Starred(ctx context.Context, account string, manual bool) ([]Repo, error) {
	rs, err := getAll[gtRepo](ctx, p.client, p.api("/users/"+url.PathEscape(account)+"/starred?limit=50"), manual)
	return convGT(rs), err
}

func (p *GiteaProvider) Gists(ctx context.Context, account string, manual bool) ([]Repo, error) {
	return nil, ErrUnsupported
}

func (p *GiteaProvider) Repo(ctx context.Context, path string, manual bool) (Repo, error) {
	r, err := getOne[gtRepo](ctx, p.client, p.api("/repos/"+path), manual)
	return r.conv(), err
}

type gtRelease struct {
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
		CreatedAt          string `json:"created_at"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func (p *GiteaProvider) Releases(ctx context.Context, r Ref, manual bool) ([]Release, error) {
	rs, err := getAll[gtRelease](ctx, p.client, p.api("/repos/"+r.Owner+"/"+r.Name+"/releases?limit=50"), manual)
	out := make([]Release, 0, len(rs))
	for _, x := range rs {
		rel := Release{ExtID: strconv.FormatInt(x.ID, 10), Tag: x.TagName, Name: x.Name, Body: x.Body, Published: x.PublishedAt,
			Draft: x.Draft, Prerelease: x.Prerelease}
		for _, a := range x.Assets {
			rel.Assets = append(rel.Assets, Asset{ExtID: strconv.FormatInt(a.ID, 10), Name: a.Name, URL: a.BrowserDownloadURL,
				Size: a.Size, Updated: a.CreatedAt + "/" + strconv.FormatInt(a.Size, 10)})
		}
		out = append(out, rel)
	}
	return out, err
}

func isGitea(ctx context.Context, c *api.Client, baseURL string) bool {
	resp, err := c.Do(ctx, api.Request{URL: baseURL + "/api/v1/version", Manual: true})
	if err != nil {
		return false
	}
	var v struct{ Version string }
	return json.Unmarshal(resp.Body, &v) == nil && v.Version != ""
}

func (p *GiteaProvider) Forks(ctx context.Context, r Ref, maxPages int, manual bool) ([]Repo, error) {
	rs, err := getPages[gtRepo](ctx, p.client, p.api("/repos/"+r.Owner+"/"+r.Name+"/forks?limit=50"), maxPages, manual)
	return convGT(rs), err
}

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/api"
)

// GitLabProvider talks to the GitLab REST API v4 (gitlab.com or self-hosted).
// Accounts can be users or (nested) groups.
type GitLabProvider struct{ base }

func NewGitLab(host, baseURL string, c *api.Client) *GitLabProvider {
	return &GitLabProvider{base{host: host, baseURL: baseURL, client: c}}
}

func (p *GitLabProvider) Platform() Platform { return GitLab }
func (p *GitLabProvider) Supports() Features {
	return Features{Stars: true, Releases: true, Accounts: true, Forks: true, Wiki: true, Gists: true, GistsNeedToken: true}
}
func (p *GitLabProvider) CloneURL(path string) string { return p.baseURL + "/" + path + ".git" }
func (p *GitLabProvider) api(path string) string      { return p.baseURL + "/api/v4" + path }

type glProject struct {
	ID                int64     `json:"id"`
	Path              string    `json:"path"`
	PathWithNamespace string    `json:"path_with_namespace"`
	Description       string    `json:"description"`
	HTTPURLToRepo     string    `json:"http_url_to_repo"`
	WebURL            string    `json:"web_url"`
	DefaultBranch     string    `json:"default_branch"`
	Archived          bool      `json:"archived"`
	LastActivityAt    time.Time `json:"last_activity_at"`
	CreatedAt         time.Time `json:"created_at"`
	ForkedFrom        *struct {
		ID int64 `json:"id"`
	} `json:"forked_from_project"`
	Namespace struct {
		FullPath string `json:"full_path"`
	} `json:"namespace"`
}

func (r glProject) conv() Repo {
	owner, name := splitPath(r.PathWithNamespace)
	if r.Namespace.FullPath != "" {
		owner = r.Namespace.FullPath
	}
	if r.Path != "" {
		name = r.Path
	}
	return Repo{ExtID: strconv.FormatInt(r.ID, 10), Owner: owner, Name: name, Description: r.Description,
		CloneURL: r.HTTPURLToRepo, WebURL: r.WebURL, DefaultBranch: r.DefaultBranch, Fork: r.ForkedFrom != nil,
		Archived: r.Archived, PushedAt: r.LastActivityAt, CreatedAt: r.CreatedAt}
}

func convGL(ps []glProject) []Repo {
	out := make([]Repo, len(ps))
	for i, p := range ps {
		out[i] = p.conv()
	}
	return out
}

// user looks up a user id by username (0 if there is none).
func (p *GitLabProvider) user(ctx context.Context, name string, manual bool) (int64, error) {
	us, err := getOne[[]struct {
		ID int64 `json:"id"`
	}](ctx, p.client, p.api("/users?username="+url.QueryEscape(name)), manual)
	if err != nil || len(us) == 0 {
		return 0, err
	}
	return us[0].ID, nil
}

func (p *GitLabProvider) AccountType(ctx context.Context, name string, manual bool) (string, error) {
	if id, err := p.user(ctx, name, manual); err != nil || id != 0 {
		return "User", err
	}
	if _, err := getOne[struct {
		ID int64 `json:"id"`
	}](ctx, p.client, p.api("/groups/"+url.PathEscape(name)), manual); err != nil {
		return "", err
	}
	return "Group", nil
}

func (p *GitLabProvider) Repos(ctx context.Context, account string, manual bool) ([]Repo, error) {
	id, err := p.user(ctx, account, manual)
	if err != nil {
		return nil, err
	}
	var ps []glProject
	if id != 0 {
		ps, err = getAll[glProject](ctx, p.client, p.api("/users/"+strconv.FormatInt(id, 10)+"/projects?per_page=100&order_by=id&sort=asc"), manual)
	} else {
		ps, err = getAll[glProject](ctx, p.client, p.api("/groups/"+url.PathEscape(account)+"/projects?include_subgroups=true&per_page=100&order_by=id&sort=asc"), manual)
	}
	return convGL(ps), err
}

func (p *GitLabProvider) Starred(ctx context.Context, account string, manual bool) ([]Repo, error) {
	id, err := p.user(ctx, account, manual)
	if err != nil {
		return nil, err
	}
	if id == 0 {
		return nil, nil // groups cannot star
	}
	ps, err := getAll[glProject](ctx, p.client, p.api("/users/"+strconv.FormatInt(id, 10)+"/starred_projects?per_page=100&order_by=id&sort=asc"), manual)
	return convGL(ps), err
}

// Gists lists snippets. GitLab only lists the snippets of the token owner
// (there is no API for other users' snippets), so this needs a token that
// belongs to account. Only public snippets are kept: they can be cloned
// without credentials.
func (p *GitLabProvider) Gists(ctx context.Context, account string, manual bool) ([]Repo, error) {
	if p.client.Config().Token == "" {
		return nil, fmt.Errorf("GitLab snippets need a token for %s (Settings → %s)", account, p.host)
	}
	me, err := getOne[struct {
		Username string `json:"username"`
	}](ctx, p.client, p.api("/user"), manual)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(me.Username, account) {
		return nil, fmt.Errorf("GitLab only lists the token owner's snippets: the token belongs to %s, not %s", me.Username, account)
	}
	ss, err := getAll[struct {
		ID            int64     `json:"id"`
		Title         string    `json:"title"`
		Description   string    `json:"description"`
		Visibility    string    `json:"visibility"`
		HTTPURLToRepo string    `json:"http_url_to_repo"`
		WebURL        string    `json:"web_url"`
		UpdatedAt     time.Time `json:"updated_at"`
		CreatedAt     time.Time `json:"created_at"`
	}](ctx, p.client, p.api("/snippets?per_page=100"), manual)
	var out []Repo
	for _, x := range ss {
		if x.Visibility != "public" || x.HTTPURLToRepo == "" {
			continue
		}
		desc := x.Title
		if x.Description != "" {
			desc += " – " + x.Description
		}
		id := strconv.FormatInt(x.ID, 10)
		out = append(out, Repo{ExtID: "snippet:" + id, Owner: account, Name: id, Description: desc, CloneURL: x.HTTPURLToRepo,
			WebURL: x.WebURL, PushedAt: x.UpdatedAt, CreatedAt: x.CreatedAt})
	}
	return out, err
}

func (p *GitLabProvider) Forks(ctx context.Context, r Ref, maxPages int, manual bool) ([]Repo, error) {
	id := r.ExtID
	if id == "" {
		id = url.PathEscape(r.Owner + "/" + r.Name)
	}
	ps, err := getPages[glProject](ctx, p.client, p.api("/projects/"+id+"/forks?per_page=100&order_by=created_at&sort=desc"), maxPages, manual)
	return convGL(ps), err
}

func (p *GitLabProvider) Repo(ctx context.Context, path string, manual bool) (Repo, error) {
	r, err := getOne[glProject](ctx, p.client, p.api("/projects/"+url.PathEscape(path)), manual)
	return r.conv(), err
}

type glRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	ReleasedAt  time.Time `json:"released_at"`
	Upcoming    bool      `json:"upcoming_release"`
	Assets      struct {
		Links []struct {
			ID             int64  `json:"id"`
			Name           string `json:"name"`
			URL            string `json:"url"`
			DirectAssetURL string `json:"direct_asset_url"`
		} `json:"links"`
	} `json:"assets"`
}

func (p *GitLabProvider) Releases(ctx context.Context, r Ref, manual bool) ([]Release, error) {
	id := r.ExtID
	if id == "" {
		id = url.PathEscape(r.Owner + "/" + r.Name)
	}
	rs, err := getAll[glRelease](ctx, p.client, p.api("/projects/"+id+"/releases?per_page=100"), manual)
	out := make([]Release, 0, len(rs))
	for _, x := range rs {
		rel := Release{ExtID: x.TagName, Tag: x.TagName, Name: x.Name, Body: x.Description, Published: x.ReleasedAt, Prerelease: x.Upcoming}
		for _, l := range x.Assets.Links {
			u := l.DirectAssetURL
			if u == "" {
				u = l.URL
			}
			// link assets carry no size/date; the URL is the change marker
			rel.Assets = append(rel.Assets, Asset{ExtID: strconv.FormatInt(l.ID, 10), Name: l.Name, URL: u, Updated: l.URL})
		}
		out = append(out, rel)
	}
	return out, err
}

// IsGitLab probes a host (used by Detect).
func isGitLab(ctx context.Context, c *api.Client, baseURL string) bool {
	resp, err := c.Do(ctx, api.Request{URL: baseURL + "/api/v4/projects?per_page=1", Manual: true})
	if err != nil {
		var rl *api.RateLimitedError
		return errors.As(err, &rl)
	}
	return resp.Header.Get("X-Gitlab-Meta") != "" || len(resp.Body) > 0 && resp.Body[0] == '['
}

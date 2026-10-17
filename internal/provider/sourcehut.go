package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/api"
)

// SourcehutProvider lists repositories on git.sr.ht (or a self-hosted
// sourcehut). With a token it uses the GraphQL API; without one it reads the
// public profile pages, since sourcehut's API always requires authentication.
type SourcehutProvider struct{ base }

func NewSourcehut(host, baseURL string, c *api.Client) *SourcehutProvider {
	return &SourcehutProvider{base{host: host, baseURL: baseURL, client: c}}
}

func (p *SourcehutProvider) Platform() Platform { return Sourcehut }
func (p *SourcehutProvider) Supports() Features { return Features{Accounts: true} }
func (p *SourcehutProvider) CloneURL(path string) string {
	return p.baseURL + "/" + tilde(path)
}

// tilde makes sure the owner part starts with "~".
func tilde(path string) string {
	path = strings.Trim(path, "/")
	if !strings.HasPrefix(path, "~") {
		path = "~" + path
	}
	return path
}

func (p *SourcehutProvider) hasToken() bool { return p.client.Config().Token != "" }

func (p *SourcehutProvider) AccountType(ctx context.Context, name string, manual bool) (string, error) {
	_, err := p.client.Do(ctx, api.Request{URL: p.baseURL + "/" + tilde(name), Accept: "text/html", Manual: manual})
	return "User", err
}

var srhtRepoLink = regexp.MustCompile(`<h4>\s*<a href="/(~[^/"]+)/([^/"]+)"[^>]*>`)
var srhtDesc = regexp.MustCompile(`(?s)<h4>\s*<a href="/~[^/"]+/([^/"]+)"[^>]*>.*?</h4>\s*(?:<p>(.*?)</p>)?`)

func (p *SourcehutProvider) Repos(ctx context.Context, account string, manual bool) ([]Repo, error) {
	user := tilde(account)
	if p.hasToken() {
		return p.reposGraphQL(ctx, strings.TrimPrefix(user, "~"), manual)
	}
	var out []Repo
	seen := map[string]bool{}
	for page := 1; page <= 100; page++ {
		resp, err := p.client.Do(ctx, api.Request{URL: fmt.Sprintf("%s/%s?page=%d", p.baseURL, user, page), Accept: "text/html", Manual: manual})
		if err != nil {
			return out, err
		}
		body := string(resp.Body)
		descs := map[string]string{}
		for _, m := range srhtDesc.FindAllStringSubmatch(body, -1) {
			descs[m[1]] = strings.TrimSpace(html.UnescapeString(stripTags(m[2])))
		}
		found := 0
		for _, m := range srhtRepoLink.FindAllStringSubmatch(body, -1) {
			if seen[m[2]] {
				continue
			}
			seen[m[2]] = true
			found++
			out = append(out, Repo{Owner: m[1], Name: m[2], Description: descs[m[2]],
				CloneURL: p.baseURL + "/" + m[1] + "/" + m[2], WebURL: p.baseURL + "/" + m[1] + "/" + m[2]})
		}
		if found == 0 || !strings.Contains(body, fmt.Sprintf("?page=%d", page+1)) {
			break
		}
	}
	return out, nil
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

func stripTags(s string) string { return tagRe.ReplaceAllString(s, "") }

func (p *SourcehutProvider) reposGraphQL(ctx context.Context, user string, manual bool) ([]Repo, error) {
	const q = `query($u: String!, $c: Cursor) { user(username: $u) { canonicalName repositories(cursor: $c) {
		cursor results { id name description updated visibility } } } }`
	var out []Repo
	var cursor *string
	for {
		body, _ := json.Marshal(map[string]any{"query": q, "variables": map[string]any{"u": user, "c": cursor}})
		resp, err := p.client.Do(ctx, api.Request{Method: "POST", URL: p.baseURL + "/query", Body: body, Manual: manual})
		if err != nil {
			return out, err
		}
		var r struct {
			Data struct {
				User *struct {
					CanonicalName string
					Repositories  struct {
						Cursor  *string
						Results []struct {
							ID                            int64
							Name, Description, Visibility string
							Updated                       time.Time
						}
					}
				}
			}
			Errors []struct{ Message string }
		}
		if err := json.Unmarshal(resp.Body, &r); err != nil {
			return out, err
		}
		if len(r.Errors) > 0 {
			return out, fmt.Errorf("sourcehut: %s", r.Errors[0].Message)
		}
		if r.Data.User == nil {
			return out, ErrNotFound
		}
		for _, x := range r.Data.User.Repositories.Results {
			if x.Visibility == "PRIVATE" {
				continue
			}
			owner := r.Data.User.CanonicalName
			out = append(out, Repo{ExtID: strconv.FormatInt(x.ID, 10), Owner: owner, Name: x.Name, Description: x.Description,
				CloneURL: p.baseURL + "/" + owner + "/" + x.Name, WebURL: p.baseURL + "/" + owner + "/" + x.Name, PushedAt: x.Updated})
		}
		cursor = r.Data.User.Repositories.Cursor
		if cursor == nil {
			return out, nil
		}
	}
}

func (p *SourcehutProvider) Starred(context.Context, string, bool) ([]Repo, error) {
	return nil, ErrUnsupported
}
func (p *SourcehutProvider) Gists(context.Context, string, bool) ([]Repo, error) {
	return nil, ErrUnsupported
}
func (p *SourcehutProvider) Releases(context.Context, Ref, bool) ([]Release, error) {
	return nil, ErrUnsupported
}

// Repo needs no API call: the clone URL is derived from the path.
func (p *SourcehutProvider) Repo(ctx context.Context, path string, manual bool) (Repo, error) {
	owner, name := splitPath(tilde(path))
	return Repo{Owner: owner, Name: name, CloneURL: p.CloneURL(path), WebURL: p.CloneURL(path)}, nil
}

// GitProvider is a plain git server: no API, every source is a single repo.
type GitProvider struct{ host string }

func NewGit(host string) *GitProvider              { return &GitProvider{host: host} }
func (p *GitProvider) Platform() Platform          { return Git }
func (p *GitProvider) Host() string                { return p.host }
func (p *GitProvider) Client() *api.Client         { return nil }
func (p *GitProvider) Supports() Features          { return Features{} }
func (p *GitProvider) CloneURL(path string) string { return path }
func (p *GitProvider) AccountType(context.Context, string, bool) (string, error) {
	return "", ErrUnsupported
}
func (p *GitProvider) Repos(context.Context, string, bool) ([]Repo, error) {
	return nil, ErrUnsupported
}
func (p *GitProvider) Starred(context.Context, string, bool) ([]Repo, error) {
	return nil, ErrUnsupported
}
func (p *GitProvider) Gists(context.Context, string, bool) ([]Repo, error) {
	return nil, ErrUnsupported
}
func (p *GitProvider) Releases(context.Context, Ref, bool) ([]Release, error) {
	return nil, ErrUnsupported
}
func (p *GitProvider) Repo(context.Context, string, bool) (Repo, error) {
	return Repo{}, ErrUnsupported
}

func (p *SourcehutProvider) Forks(context.Context, Ref, int, bool) ([]Repo, error) {
	return nil, ErrUnsupported
}

func (p *GitProvider) Forks(context.Context, Ref, int, bool) ([]Repo, error) {
	return nil, ErrUnsupported
}

// Package provider abstracts the forges gitbackup can read from: GitHub,
// GitLab, Gitea/Forgejo, sourcehut, and plain git servers without any API.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/api"
)

// Platform identifies a forge type.
type Platform string

const (
	GitHub    Platform = "github"
	GitLab    Platform = "gitlab"
	Gitea     Platform = "gitea" // also Forgejo (Codeberg)
	Sourcehut Platform = "sourcehut"
	Git       Platform = "git" // plain git server, no API
)

// Platforms in UI order.
var Platforms = []Platform{GitHub, GitLab, Gitea, Sourcehut, Git}

func (p Platform) Label() string {
	return map[Platform]string{GitHub: "GitHub", GitLab: "GitLab", Gitea: "Gitea / Forgejo", Sourcehut: "sourcehut", Git: "plain git"}[p]
}

var (
	ErrUnsupported = errors.New("not supported by this platform")
	ErrNotFound    = api.ErrNotFound
	// ErrPartial: the forge stops paginating (GitHub: "only the first 1000
	// results"); the returned list is incomplete but usable.
	ErrPartial = errors.New("list incomplete: the forge returns only the first results")
)

// Repo is a repository (or gist/snippet) as reported by a forge.
type Repo struct {
	ExtID         string // platform id, survives renames
	Owner         string // user, org, group path (a/b), ~user
	Name          string
	Description   string
	CloneURL      string
	WebURL        string
	DefaultBranch string
	Fork          bool
	Archived      bool
	PushedAt      time.Time
	CreatedAt     time.Time
}

// Release is a published release with downloadable assets.
type Release struct {
	ExtID      string
	Tag        string
	Name       string
	Body       string
	Published  time.Time
	Draft      bool
	Prerelease bool
	Assets     []Asset
}

// Asset is one downloadable file of a release.
type Asset struct {
	ExtID   string
	Name    string
	URL     string
	Size    int64  // 0 if unknown
	Updated string // opaque change marker
	Digest  string // "sha256:<hex>" when the forge provides one
}

// Ref points at a stored repository.
type Ref struct {
	Owner, Name, ExtID string
}

// Provider is implemented per platform. manual=true may use the API reserve.
type Provider interface {
	Platform() Platform
	Host() string
	Client() *api.Client // nil for plain git
	// AccountType checks that an account exists ("User", "Organization", "Group").
	AccountType(ctx context.Context, name string, manual bool) (string, error)
	Repos(ctx context.Context, account string, manual bool) ([]Repo, error)
	Starred(ctx context.Context, account string, manual bool) ([]Repo, error)
	Gists(ctx context.Context, account string, manual bool) ([]Repo, error)
	Repo(ctx context.Context, path string, manual bool) (Repo, error)
	Releases(ctx context.Context, r Ref, manual bool) ([]Release, error)
	// Forks lists forks of a repo, newest first, at most maxPages API pages.
	Forks(ctx context.Context, r Ref, maxPages int, manual bool) ([]Repo, error)
	// CloneURL builds a clone URL without asking the API.
	CloneURL(path string) string
	// Supports tells the UI which features exist.
	Supports() Features
}

type Features struct {
	Stars, Gists, Releases, Accounts, Forks, Wiki bool
	GistsNeedToken                                bool // GitLab: only the token owner's snippets
}

// WikiURL derives the wiki repository of a clone URL (GitHub, GitLab and
// Gitea/Forgejo all use <repo>.wiki.git).
func WikiURL(cloneURL string) string {
	return strings.TrimSuffix(strings.TrimSuffix(cloneURL, "/"), ".git") + ".wiki.git"
}

// getAll follows Link pagination and decodes every page into a slice of T.
func getAll[T any](ctx context.Context, c *api.Client, u string, manual bool) ([]T, error) {
	return getPages[T](ctx, c, u, 0, manual)
}

// getPages is getAll limited to maxPages (0 = all).
func getPages[T any](ctx context.Context, c *api.Client, u string, maxPages int, manual bool) ([]T, error) {
	var out []T
	for n := 0; u != "" && (maxPages <= 0 || n < maxPages); n++ {
		resp, err := c.Do(ctx, api.Request{URL: u, Manual: manual, Cache: true})
		if err != nil {
			if n > 0 && strings.Contains(err.Error(), "422") {
				return out, ErrPartial
			}
			return out, err
		}
		var page []T
		if err := json.Unmarshal(resp.Body, &page); err != nil {
			return out, fmt.Errorf("decode %s: %w", u, err)
		}
		out = append(out, page...)
		if len(page) == 0 {
			break
		}
		u = resp.Next
	}
	return out, nil
}

func getOne[T any](ctx context.Context, c *api.Client, u string, manual bool) (T, error) {
	var v T
	resp, err := c.Do(ctx, api.Request{URL: u, Manual: manual, Cache: true})
	if err != nil {
		return v, err
	}
	return v, json.Unmarshal(resp.Body, &v)
}

// splitPath splits "a/b/c" into owner "a/b" and name "c".
func splitPath(p string) (owner, name string) {
	p = strings.Trim(p, "/")
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

// base is embedded by API-backed providers.
type base struct {
	host    string
	baseURL string // scheme://host[:port][/prefix], no trailing slash
	client  *api.Client
}

func (b *base) Host() string        { return b.host }
func (b *base) Client() *api.Client { return b.client }

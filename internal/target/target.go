// Package target turns user input ("octocat", "pi-hole/pi-hole",
// "https://gitlab.company.lan/group/sub", "git@host:a/b.git", "~sircmpwn")
// into a platform, an instance and a path, and maps repos to disk locations.
package target

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/MarvAmBass/gitbackup/internal/provider"
)

const (
	Account = "account" // user / org / group: list its repos
	Repo    = "repo"    // single repository
	Auto    = "auto"    // GitLab a/b: group or project, resolved via API
)

// Target is the parsed form of a source.
type Target struct {
	Platform provider.Platform // "" = unknown host, caller must Detect
	Host     string
	BaseURL  string // scheme://host[:port] for API calls
	Kind     string // Account | Repo | Auto
	Path     string // account name or owner/name (git: the clone URL)
	Original string // input as typed (clone URL for plain git)
}

var (
	segRe     = regexp.MustCompile(`^[A-Za-z0-9_.~+-]+$`)
	scpLikeRe = regexp.MustCompile(`^(?:[A-Za-z0-9_.-]+@)?([A-Za-z0-9.-]+):(.+)$`)
)

// Parse interprets free-form input. hint is the platform chosen in the UI
// ("" = auto).
func Parse(in string, hint provider.Platform) (Target, error) {
	s := strings.TrimSuffix(strings.TrimSpace(in), "/")
	if s == "" {
		return Target{}, fmt.Errorf("empty input")
	}
	if strings.HasPrefix(s, "file://") {
		p := path.Clean(strings.TrimPrefix(s, "file://"))
		if p == "/" || p == "." {
			return Target{}, fmt.Errorf("no repository path in %q", in)
		}
		return Target{Platform: provider.Git, Host: "local", Kind: Repo, Path: s, Original: s}, nil
	}

	// No host given: "user", "owner/repo", "~user/repo", "group/sub/proj".
	first := strings.SplitN(s, "/", 2)[0]
	if !strings.Contains(s, "://") && !scpLikeRe.MatchString(s) && !strings.Contains(first, ".") {
		p := hint
		if p == "" {
			p = provider.GitHub
			if strings.HasPrefix(s, "~") {
				p = provider.Sourcehut
			}
		}
		host := provider.DefaultHost[p]
		if host == "" {
			return Target{}, fmt.Errorf("%s needs a full URL", p.Label())
		}
		t := Target{Platform: p, Host: host, BaseURL: "https://" + host, Original: s}
		return t, t.Resolve(p, s)
	}

	host, rawPath, baseURL, err := split(s)
	if err != nil {
		return Target{}, err
	}
	t := Target{Host: host, BaseURL: baseURL, Original: s, Path: rawPath}
	p := hint
	if p == "" {
		p = provider.KnownHosts[host]
		if p == "" && host == "www.github.com" {
			p, t.Host = provider.GitHub, "github.com"
		}
	}
	if p == "" {
		return t, nil // unknown host: caller runs provider.Detect, then Resolve
	}
	return t, t.Resolve(p, rawPath)
}

// Resolve classifies path for platform p and fills Kind/Path.
func (t *Target) Resolve(p provider.Platform, rawPath string) error {
	t.Platform = p
	if p == provider.Git {
		t.Kind, t.Path = Repo, t.Original
		return nil
	}
	clean := strings.Trim(rawPath, "/")
	if i := strings.Index(clean, "/-/"); i >= 0 { // GitLab deep links
		clean = clean[:i]
	}
	clean = strings.TrimSuffix(clean, ".git")
	var segs []string
	for _, s := range strings.Split(clean, "/") {
		if s == "" {
			continue
		}
		if !segRe.MatchString(s) || s == "." || s == ".." {
			return fmt.Errorf("invalid path segment %q", s)
		}
		segs = append(segs, s)
	}
	if len(segs) == 0 {
		return fmt.Errorf("no user, organisation or repository in %q", t.Original)
	}
	switch p {
	case provider.GitHub, provider.Gitea:
		if len(segs) == 1 {
			t.Kind, t.Path = Account, segs[0]
		} else { // drop /tree/main, /src/branch/…, /releases …
			t.Kind, t.Path = Repo, segs[0]+"/"+strings.TrimSuffix(segs[1], ".git")
		}
	case provider.Sourcehut:
		if !strings.HasPrefix(segs[0], "~") {
			segs[0] = "~" + segs[0]
		}
		if len(segs) == 1 {
			t.Kind, t.Path = Account, segs[0]
		} else {
			t.Kind, t.Path = Repo, segs[0]+"/"+segs[1]
		}
	case provider.GitLab:
		t.Path = strings.Join(segs, "/")
		t.Kind = Account
		if len(segs) > 1 {
			t.Kind = Auto // group/subgroup or group/project – the API decides
		}
	default:
		return fmt.Errorf("unknown platform %q", p)
	}
	return nil
}

// split extracts host, path and an https base URL from URL-ish or scp-like input.
func split(s string) (host, p, baseURL string, err error) {
	if !strings.Contains(s, "://") {
		if m := scpLikeRe.FindStringSubmatch(s); m != nil && strings.Contains(m[1], ".") && !strings.HasPrefix(m[2], "//") {
			h := strings.ToLower(m[1])
			return h, m[2], "https://" + h, nil
		}
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", "", "", err
	}
	if u.Hostname() == "" {
		return "", "", "", fmt.Errorf("no host in %q", s)
	}
	host = strings.ToLower(u.Hostname())
	scheme := "https"
	if u.Scheme == "http" {
		scheme = "http"
	}
	hostport := host
	if u.Port() != "" && (u.Scheme == "http" || u.Scheme == "https") {
		hostport += ":" + u.Port()
	}
	return host, u.Path, scheme + "://" + hostport, nil
}

// PlainRepo splits a plain git URL into owner path and name for storage.
func PlainRepo(rawURL string) (host, owner, name string) {
	if strings.HasPrefix(rawURL, "file://") {
		p := strings.TrimSuffix(path.Clean(strings.TrimPrefix(rawURL, "file://")), ".git")
		o, n := path.Split(strings.Trim(p, "/"))
		return "local", strings.Trim(o, "/"), n
	}
	h, p, _, err := split(rawURL)
	if err != nil {
		return "unknown", "", "repo"
	}
	p = strings.TrimSuffix(strings.Trim(path.Clean("/"+p), "/"), ".git")
	o, n := path.Split(p)
	return h, strings.Trim(o, "/"), n
}

// Folder layout below data/:
//
//	<host>/<owner…>/<section>/<name>/      one folder per repository ("slot")
//	    repo.git  repo.v1.git …              current mirror, frozen versions
//	    wiki.git  wiki.v1.git …
//	    releases/<tag>/                       release.json, assets, .versions/
//	    html/  html.v1/ …                     streagit pages (rendered on demand, nothing on disk)
//	    forks/<fork owner>/                   forks of this repo (same layout)
//	<host>/<user>/stars/<owner>/<name>  →    symlink to the starred repo's slot
const (
	RepoUnit = "repo"
	WikiUnit = "wiki"
)

// Sections per repo kind.
var Sections = map[string]string{"repo": "repos", "fork": "forks", "gist": "gists", "snippet": "snippets"}

// Slot is the folder of one repository below data/.
func Slot(kind, host, owner, name string) string {
	sec := Sections[kind]
	if sec == "" {
		sec = "repos"
	}
	return path.Join(safe(host), safePath(owner), sec, safe(name))
}

// ForkSlot is where a fork of a watched repo lives: inside the parent's folder.
func ForkSlot(parentSlot, forkOwner string) string {
	return path.Join(parentSlot, "forks", safePath(forkOwner))
}

// StarLink is the symlink for a starred repo.
func StarLink(host, user, owner, name string) string {
	return path.Join(safe(host), safePath(user), "stars", safePath(owner), safe(name))
}

// GitDir is the bare repository of a unit (repo, wiki); version 0 = current.
func GitDir(slot, unit string, version int) string {
	if version == 0 {
		return path.Join(slot, unit+".git")
	}
	return path.Join(slot, fmt.Sprintf("%s.v%d.git", unit, version))
}

// HTMLDir is the URL folder of a version's streagit pages (0 = current).
func HTMLDir(slot string, version int) string {
	if version == 0 {
		return path.Join(slot, "html")
	}
	return path.Join(slot, fmt.Sprintf("html.v%d", version))
}

// ReleasesDir holds a repo's releases.
func ReleasesDir(slot string) string { return path.Join(slot, "releases") }

func safePath(p string) string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if seg = safe(seg); seg != "" && seg != "_" {
			out = append(out, seg)
		}
	}
	if len(out) == 0 {
		return "_"
	}
	return path.Join(out...)
}

// Safe makes one path segment filesystem-safe.
func Safe(s string) string { return safe(s) }

func safe(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == '~', r == '+':
			return r
		}
		return '_'
	}, s)
	s = strings.TrimLeft(s, ".")
	if s == "" {
		return "_"
	}
	return s
}

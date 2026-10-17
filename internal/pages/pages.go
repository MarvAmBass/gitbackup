// Package pages builds the browsable pages around the repositories:
// data/index.html, one page per owner, and a landing page in every repo
// folder (history, versions, wiki, releases, forks). History, file and commit
// pages themselves are rendered on demand by streagit.
package pages

import (
	"bytes"
	_ "embed"
	"html/template"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/store"
	"github.com/MarvAmBass/gitbackup/internal/target"
)

//go:embed style.css
var styleCSS []byte

//go:embed index.html.tmpl
var indexTmpl string

type Builder struct {
	DataDir   string // data/ – repo folders live below it
	AssetsDir string // logo.png / favicon.png (shipped with streagit)
}

func (b *Builder) abs(rel string) string { return filepath.Join(b.DataDir, filepath.FromSlash(rel)) }

// assets places style.css, logo.png and favicon.png next to the index pages.
func (b *Builder) assets(dir string) error {
	if err := writeIfChanged(filepath.Join(dir, "style.css"), styleCSS); err != nil {
		return err
	}
	for _, f := range []string{"logo.png", "favicon.png"} {
		dst := filepath.Join(dir, f)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := copyFile(filepath.Join(b.AssetsDir, f), dst); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- pages

type entry struct {
	store.Repo
	Link string // relative link to the repo's landing page
}

type group struct {
	Title string
	Link  string
	Repos []entry
}

type version struct {
	N           int
	Description string
	HTML        bool
}

type repoPage struct {
	store.Repo
	Versions     []version
	WikiVersions []int
	HasWiki      bool
	HasReleases  bool
	HasHTML      bool
	Forks        []entry
	Parent       *entry
}

type page struct {
	Title     string
	Root      string // relative path to data/
	Groups    []group
	Repo      *repoPage
	Generated time.Time
	HTML      bool // history/commit pages available (streagit)
	Total     int
}

var tmpl = template.Must(template.New("index").Funcs(template.FuncMap{
	"day": func(ts int64) string {
		if ts == 0 {
			return "–"
		}
		return time.Unix(ts, 0).Format("2006-01-02")
	},
	"gitdir": func(r store.Repo, unit string, n int) string { return path.Base(target.GitDir(r.Path, unit, n)) },
}).Parse(indexTmpl))

// ownerDir is the folder that groups a repo on the owner page: host/owner.
func ownerDir(slot string) string { return path.Dir(path.Dir(slot)) }

// Index writes data/index.html, <host>/<owner>/index.html and a landing page
// in every repo folder. With html on, every cloned repo and version links its
// streagit pages (html/, html.vN/) – nothing of them exists on disk.
func (b *Builder) Index(repos []store.Repo, stars map[string][]store.Repo, html bool) error {
	if err := b.assets(b.DataDir); err != nil {
		return err
	}
	now := time.Now()
	byID := map[int64]store.Repo{}
	forks := map[int64][]store.Repo{}
	byOwner := map[string][]store.Repo{}
	for _, r := range repos {
		byID[r.ID] = r
		if r.ParentID.Valid {
			forks[r.ParentID.Int64] = append(forks[r.ParentID.Int64], r)
			continue
		}
		byOwner[ownerDir(r.Path)] = append(byOwner[ownerDir(r.Path)], r)
	}
	link := func(from string, r store.Repo) entry {
		rel, _ := filepath.Rel(b.abs(from), b.abs(path.Join(r.Path, "index.html")))
		return entry{Repo: r, Link: filepath.ToSlash(rel)}
	}

	owners := make([]string, 0, len(byOwner))
	for o := range byOwner {
		owners = append(owners, o)
	}
	sort.Slice(owners, func(i, j int) bool { return strings.ToLower(owners[i]) < strings.ToLower(owners[j]) })

	root := page{Title: "gitbackup", Generated: now, HTML: html, Total: len(repos)}
	for _, o := range owners {
		g := group{Title: o, Link: o + "/index.html"}
		og := group{Title: o}
		for _, r := range byOwner[o] {
			g.Repos = append(g.Repos, link("", r))
			og.Repos = append(og.Repos, link(o, r))
		}
		root.Groups = append(root.Groups, g)
		ownerPage := page{Title: o, Root: relRoot(o), Groups: []group{og}, Generated: now, HTML: html, Total: len(og.Repos)}
		if err := b.write(o, ownerPage); err != nil {
			return err
		}
	}
	users := make([]string, 0, len(stars))
	for u := range stars {
		users = append(users, u)
	}
	sort.Strings(users)
	for _, u := range users {
		g := group{Title: "★ starred by " + u}
		for _, r := range stars[u] {
			g.Repos = append(g.Repos, link("", r))
		}
		root.Groups = append(root.Groups, g)
	}
	if err := b.write("", root); err != nil {
		return err
	}

	for _, r := range repos {
		if _, err := os.Stat(b.abs(r.Path)); err != nil {
			continue // not cloned yet
		}
		rp := &repoPage{Repo: r}
		for n := 1; n <= r.Versions; n++ {
			desc, _ := os.ReadFile(filepath.Join(b.abs(target.GitDir(r.Path, target.RepoUnit, n)), "description"))
			rp.Versions = append(rp.Versions, version{N: n, Description: strings.TrimSpace(string(desc)), HTML: html})
		}
		for n := 1; n <= r.WikiVersions; n++ {
			rp.WikiVersions = append(rp.WikiVersions, n)
		}
		_, err := os.Stat(b.abs(target.GitDir(r.Path, target.WikiUnit, 0)))
		rp.HasWiki = err == nil
		_, err = os.Stat(b.abs(target.ReleasesDir(r.Path)))
		rp.HasReleases = err == nil
		rp.HasHTML = html && r.LastCommit > 0
		for _, f := range forks[r.ID] {
			rp.Forks = append(rp.Forks, link(r.Path, f))
		}
		if p, ok := byID[r.ParentID.Int64]; ok && r.ParentID.Valid {
			e := link(r.Path, p)
			rp.Parent = &e
		}
		if err := b.write(r.Path, page{Title: r.FullName(), Root: relRoot(r.Path), Repo: rp, Generated: now, HTML: html}); err != nil {
			return err
		}
	}
	return nil
}

func relRoot(dir string) string {
	if dir == "" {
		return ""
	}
	return strings.Repeat("../", strings.Count(dir, "/")+1)
}

func (b *Builder) write(dir string, p page) error {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "page", p); err != nil {
		return err
	}
	abs := b.abs(dir)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	return writeIfChanged(filepath.Join(abs, "index.html"), buf.Bytes())
}

func writeIfChanged(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

package engine

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/gitops"
	"github.com/MarvAmBass/gitbackup/internal/target"
)

// SyncViews maintains the symlinks of the data tree:
//
//	<host>/<user>/stars/<owner>/<name> → the starred repo's folder
//	aliases (a repo reachable by a second route, e.g. a followed account's
//	fork that is also a fork of a watched repo) → the repo's folder
//
// Every repository is stored exactly once; everything else is a link.
func (e *Engine) SyncViews() error {
	stars, err := e.Store.StarViews()
	if err != nil {
		return err
	}
	want := map[string]string{} // link (below data/) → target folder (below data/)
	starRoots := map[string]bool{}
	for key, repos := range stars { // key: host/user
		host, user, _ := strings.Cut(key, "/")
		starRoots[filepath.Dir(target.StarLink(host, user, "x", "y"))] = true
		for _, r := range repos {
			want[target.StarLink(host, user, r.Owner, r.Name)] = r.Path
		}
	}
	aliases, err := e.Store.Aliases()
	if err != nil {
		return err
	}
	for link, to := range aliases {
		want[link] = to
	}

	// stars/ folders hold only links: drop the ones no longer wanted
	for root := range starRoots {
		e.pruneLinks(filepath.Dir(e.abs(root)), want)
	}
	for link, to := range want {
		e.ensureLink(link, to)
	}
	return nil
}

func (e *Engine) ensureLink(link, to string) {
	abs := e.abs(link)
	rel, err := filepath.Rel(filepath.Dir(abs), e.abs(to))
	if err != nil {
		return
	}
	if cur, err := os.Readlink(abs); err == nil {
		if cur == rel {
			return
		}
		os.Remove(abs)
	} else if _, err := os.Lstat(abs); err == nil {
		e.Log.Warn("view: a real folder is in the way, leaving it", "path", link)
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return
	}
	if err := os.Symlink(rel, abs); err != nil {
		e.Log.Warn("view", "link", link, "err", err)
	}
}

// pruneLinks removes symlinks below dir (a stars/ folder) that are not wanted
// any more, then empty folders. Real files are never touched.
func (e *Engine) pruneLinks(dir string, want map[string]string) {
	var dirs []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			rel, _ := filepath.Rel(e.DataDir, p)
			if _, ok := want[filepath.ToSlash(rel)]; !ok {
				os.Remove(p)
			}
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i]) // only succeeds when empty
	}
}

// Reconcile rebuilds what can be derived from the data on disk: frozen
// version repos for every preserved state, version counts, and the symlinks.
// The database is bookkeeping; git refs are the source of truth. It runs in
// the background one repo at a time and skips repos that are being checked.
func (e *Engine) Reconcile(ctx context.Context) {
	if err := e.SyncViews(); err != nil {
		e.Log.Warn("views", "err", err)
	}
	repos, err := e.Store.AllRepos()
	if err != nil {
		e.Log.Error("reconcile", "err", err)
		return
	}
	start := time.Now()
	fixed, packed := 0, 0
	for _, r := range repos {
		if ctx.Err() != nil {
			return
		}
		e.mu.Lock()
		if _, busy := e.inflight[r.ID]; busy {
			e.mu.Unlock()
			continue
		}
		e.inflight[r.ID] = "reconcile " + r.FullName()
		e.mu.Unlock()
		for _, unit := range []string{target.RepoUnit, target.WikiUnit} {
			dir := e.abs(target.GitDir(r.Path, unit, 0))
			if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
				continue
			}
			g := &gitops.Repo{Path: dir}
			if g.LooseRefs(100) >= 100 { // written before refs were packed after each sync
				if g.PackRefs(ctx) == nil {
					packed++
				}
			}
			n := e.materialize(ctx, r, g, unit)
			for v := 1; v <= n; v++ { // frozen versions written before packing existed
				if vg := (&gitops.Repo{Path: e.abs(target.GitDir(r.Path, unit, v))}); vg.LooseRefs(100) >= 100 {
					vg.PackRefs(ctx)
				}
			}
			switch {
			case unit == target.RepoUnit && n != r.Versions:
				e.Store.SetVersions(r.ID, n)
				fixed++
			case unit == target.WikiUnit && n != r.WikiVersions:
				state := r.WikiState
				if state == "" || state == "none" {
					state = "ok"
				}
				e.Store.WikiChecked(r.ID, state, n)
				fixed++
			}
		}
		e.mu.Lock()
		delete(e.inflight, r.ID)
		e.mu.Unlock()
	}
	e.Log.Info("reconciled", "repos", len(repos), "fixed", fixed, "packed", packed, "took", time.Since(start).Round(time.Second))
	e.MarkIndexDirty()
}

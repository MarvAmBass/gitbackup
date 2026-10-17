package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/api"
	"github.com/MarvAmBass/gitbackup/internal/gitops"
	"github.com/MarvAmBass/gitbackup/internal/provider"
	"github.com/MarvAmBass/gitbackup/internal/store"
	"github.com/MarvAmBass/gitbackup/internal/target"
)

// forkPages limits the API pages read per scan (newest forks first).
const forkPages = 3

// scanForks looks for forks of a watched repo that carry at least
// plan.ForkMinAhead commits of their own and stores them inside the parent's
// folder (forks/<owner>/). Cheap filters first, so most forks cost nothing:
//  1. never pushed to after forking (pure "backup forks") → skip, no git at all
//  2. ls-remote: every branch tip already in the parent → skip
//  3. fetch into a scratch repo that borrows the parent's objects, count
//     commits we don't have yet
func (e *Engine) scanForks(ctx context.Context, parent store.Repo, p provider.Provider, plan store.Plan, start time.Time) {
	e.setActivity(parent.ID, "forks of "+parent.FullName())
	forks, err := p.Forks(ctx, provider.Ref{Owner: parent.Owner, Name: parent.Name, ExtID: parent.ExtID.String}, forkPages, false)
	var rl *api.RateLimitedError
	switch {
	case errors.As(err, &rl):
		return // try again next check
	case err != nil:
		e.Log.Warn("forks", "repo", parent.FullName(), "err", err)
		e.Store.ForksChecked(parent.ID, start.Unix())
		return
	}
	defer e.Store.ForksChecked(parent.ID, start.Unix())

	existing, _ := e.Store.ForksOf(parent.ID)
	known := map[string]bool{}
	for _, f := range existing {
		known[f.ExtID.String] = true
	}
	var cands []provider.Repo
	for _, f := range forks {
		if f.PushedAt.After(f.CreatedAt.Add(time.Minute)) && !known[f.ExtID] {
			cands = append(cands, f)
		}
	}
	if len(cands) == 0 {
		return
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].PushedAt.After(cands[j].PushedAt) })

	parentGit := &gitops.Repo{Path: e.abs(target.GitDir(parent.Path, target.RepoUnit, 0))}
	have, err := parentGit.Refs(ctx, "refs/")
	if err != nil {
		return
	}
	var exclude []string
	for _, sha := range have {
		exclude = append(exclude, sha)
	}
	scratchDir := filepath.Join(e.StateDir, "tmp", fmt.Sprintf("forkscan-%d.git", parent.ID))
	os.RemoveAll(scratchDir)
	defer os.RemoveAll(scratchDir)
	scratch, err := gitops.Init(ctx, scratchDir)
	if err != nil {
		e.Log.Warn("forks scratch", "err", err)
		return
	}
	scratch.SetAlternates(parentGit.Path, "")

	stored, probed := len(existing), 0
	for i, f := range cands {
		if int64(stored) >= plan.ForkMax || int64(probed) >= plan.ForkMax*3 || ctx.Err() != nil {
			break
		}
		slot := target.ForkSlot(parent.Path, f.Owner)
		ext := sql.NullString{String: f.ExtID, Valid: f.ExtID != ""}
		// already in the backup under another route (e.g. a followed account's own fork)?
		if ext.Valid {
			if other, err := e.Store.RepoByExt(parent.Host, f.ExtID); err == nil {
				e.Store.AddAlias(other.ID, slot)
				stored++
				continue
			}
		}
		probed++
		if e.waitGit(ctx, parent.Host, PrioProbe) != nil {
			break // host cooling down
		}
		remote, err := gitops.LsRemote(ctx, f.CloneURL)
		if err != nil {
			continue
		}
		var tips []string
		for ref, sha := range remote.Refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				tips = append(tips, sha)
			}
		}
		if len(parentGit.Missing(ctx, tips)) == 0 {
			continue // nothing the parent doesn't have
		}
		got, err := scratch.FetchHeads(ctx, f.CloneURL, fmt.Sprintf("scan/%d", i))
		if err != nil {
			continue
		}
		ahead := scratch.CountNew(ctx, got, exclude)
		if int64(ahead) < plan.ForkMinAhead {
			continue
		}
		if _, err := e.Store.RepoByPath(slot); err == nil {
			slot = target.ForkSlot(parent.Path, f.Owner+"-"+f.Name) // owner forked twice under different names
		}
		id, created, _, _, err := e.Store.UpsertRepo(store.Repo{Kind: "fork", Platform: parent.Platform, Host: parent.Host,
			Owner: f.Owner, Name: f.Name, Path: slot, URL: f.CloneURL, ExtID: ext, ParentID: sql.NullInt64{Int64: parent.ID, Valid: true},
			Description: f.Description, DefaultBranch: f.DefaultBranch, PushedAt: f.PushedAt.Unix(), PlanID: parent.PlanID, Ahead: ahead})
		if err != nil {
			e.Log.Warn("save fork", "fork", f.Owner+"/"+f.Name, "err", err)
			continue
		}
		stored++
		if created {
			e.emit(store.Event{RepoID: sql.NullInt64{Int64: id, Valid: true}, Kind: "discovered",
				Detail: fmt.Sprintf("fork of %s with %d own commit(s)", parent.FullName(), ahead)})
		}
	}
	e.Log.Info("forks scanned", "repo", parent.FullName(), "listed", len(forks), "candidates", len(cands), "probed", probed, "stored", stored)
	if err := e.SyncViews(); err != nil {
		e.Log.Warn("views", "err", err)
	}
	e.Wake()
}

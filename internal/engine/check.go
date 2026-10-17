package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/api"
	"github.com/MarvAmBass/gitbackup/internal/gitops"
	"github.com/MarvAmBass/gitbackup/internal/provider"
	"github.com/MarvAmBass/gitbackup/internal/releases"
	"github.com/MarvAmBass/gitbackup/internal/store"
	"github.com/MarvAmBass/gitbackup/internal/target"
)

// Check ls-remotes a repo and, if anything moved, fetches with preservation.
// Then (per plan) the wiki, releases and forks are looked at.
func (e *Engine) Check(ctx context.Context, r store.Repo) {
	// a first clone of a big repo over a slow anonymous connection can take
	// long – and a killed fetch starts from scratch next time
	limit := 6 * time.Hour
	if r.LastCommit == 0 {
		limit = 24 * time.Hour
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	plan := e.effectivePlan(r)
	start := time.Now()
	now := start.Unix()
	rid := sql.NullInt64{Int64: r.ID, Valid: true}
	res := store.CheckResult{State: "ok", Versions: r.Versions}
	feat := e.Forges.For(provider.Platform(r.Platform), r.Host).Supports()

	finish := func() {
		// the initial clone is not "activity" for the adaptive schedule
		res.Interval = e.nextInterval(r, plan, res.Changed && r.LastCheck > 0, res.State)
		jitter := 0.9 + rand.Float64()*0.2
		res.NextCheck = now + int64(float64(res.Interval)*jitter)
		if err := e.Store.RepoChecked(r.ID, now, res); err != nil {
			e.Log.Error("save check", "repo", r.FullName(), "err", err)
		}
	}

	// Nothing pushed upstream since our last check, and the forge's listing
	// that says so is newer than that check: no git request needed. A safety
	// net still runs at the plan's maximum interval; "Check now" always runs.
	if !r.Forced && r.State == "ok" && r.LastCheck > 0 && r.PushedAt > 0 &&
		r.PushedAt < r.LastCheck-60 && r.Listed > r.LastCheck && now-r.LastCheck < plan.CheckMax {
		res.LastCommit, res.Versions = r.LastCommit, r.Versions
		finish()
		e.optional(ctx, r, plan, feat, false, start)
		return
	}

	gitDir := e.abs(target.GitDir(r.Path, target.RepoUnit, 0))
	prio := PrioUpdate
	if r.LastCommit == 0 {
		prio = PrioInitial
	}
	sr, repo, err := e.syncUnit(ctx, r, target.RepoUnit, r.URL, gitDir, start, prio)
	var cool *CoolingError
	if errors.Is(err, gitops.ErrDenied) {
		// 401/403: missing, private – or the host throttles anonymous git. Ask the web page.
		switch gone, known := e.confirmGone(ctx, r); {
		case gone:
			err = fmt.Errorf("%w (web page 404): %v", gitops.ErrGone, err)
		case known:
			cool = &CoolingError{Host: r.Host, Until: e.throttled(r.Host, r.FullName(), err)}
		}
	}
	if cool != nil || errors.As(err, &cool) {
		// not the repo's fault: try again after the cooldown, state unchanged
		e.Store.Postpone(r.ID, cool.Until.Unix()+rand.Int64N(600))
		return
	}
	tagsChanged := false
	if err == nil {
		for _, c := range sr.Changes {
			tagsChanged = tagsChanged || strings.HasPrefix(c.Ref, "refs/tags/")
		}
		res.Changed = len(sr.Changes) > 0
		res.Destructive = sr.Destructive()
		res.DefaultBranch = strings.TrimPrefix(sr.Head, "refs/heads/")
	}

	switch {
	case errors.Is(err, gitops.ErrGone):
		res.State, res.Error = "gone", err.Error()
		switch {
		case r.State == "new" && repo == nil:
			// never had a copy: nothing to keep, just unreachable
			res.State, res.Error = "error", "not reachable: "+err.Error()
		case r.State != "gone":
			// includes a copy that was placed here by hand (e.g. an old backup)
			e.emit(store.Event{RepoID: rid, TS: now, Kind: "gone", Detail: err.Error()})
			e.Log.Warn("repo gone upstream, keeping local copy", "repo", r.FullName())
		}
	case err != nil:
		res.State, res.Error = "error", err.Error()
		if r.State != "error" {
			e.emit(store.Event{RepoID: rid, TS: now, Kind: "error", Detail: err.Error()})
		}
		e.Log.Warn("check", "repo", r.FullName(), "err", err)
	default:
		if r.State == "gone" {
			e.emit(store.Event{RepoID: rid, TS: now, Kind: "back", Detail: "reachable again"})
		}
	}

	if repo != nil && (res.Changed || r.LastCheck == 0 || r.SizeBytes == 0) {
		res.SizeBytes = gitops.DiskUsage(repo.Path)
		res.LastCommit = repo.LastCommit(ctx).Unix()
		if vs, err := repo.Versions(ctx); err == nil {
			res.Versions = len(vs)
		}
		e.mu.Lock()
		e.indexDirty = true
		e.mu.Unlock()
	}
	finish()

	if res.State != "ok" {
		return
	}
	// the first clone is the urgent part; wiki, releases and forks follow on later checks
	if r.LastCommit > 0 {
		e.optional(ctx, r, plan, feat, tagsChanged, start)
	}
	if res.Changed {
		e.Log.Info("changed", "repo", r.FullName(), "destructive", res.Destructive, "took", time.Since(start).Round(time.Millisecond))
	}
}

// optional runs the lower-priority work of a check when it is due: the wiki
// (weekly – wiki edits don't show up in the repo), releases, fork scans.
func (e *Engine) optional(ctx context.Context, r store.Repo, plan store.Plan, feat provider.Features, tagsChanged bool, start time.Time) {
	now := start.Unix()
	if plan.Wiki && feat.Wiki && (r.Kind == "repo" || r.Kind == "fork") && now-r.WikiChecked >= plan.CheckMax {
		e.checkWiki(ctx, r, start)
	}
	// forks' releases are rarely their own and would cost one API call each
	if plan.Releases != "off" && feat.Releases && r.Kind == "repo" && !r.ParentID.Valid && e.Releases != nil {
		due := tagsChanged || r.ReleasesChecked == 0 || now-r.ReleasesChecked >= plan.ReleasesEvery
		p := e.Forges.For(provider.Platform(r.Platform), r.Host)
		if due && budgetOK(p.Client(), false) {
			e.syncReleases(ctx, r, p, plan, start)
		}
	}
	if plan.Forks == "on" && feat.Forks && r.Kind == "repo" && now-r.ForksChecked >= plan.DiscoverEvery {
		p := e.Forges.For(provider.Platform(r.Platform), r.Host)
		if budgetOK(p.Client(), false) {
			e.scanForks(ctx, r, p, plan, start)
		}
	}
}

// syncUnit brings one bare repository of a slot (repo or wiki) up to date and
// turns every preserved state into a frozen <unit>.vN.git. Nothing is created
// on disk when the remote does not exist.
func (e *Engine) syncUnit(ctx context.Context, r store.Repo, unit, url, gitDir string, start time.Time, prio int) (gitops.SyncResult, *gitops.Repo, error) {
	if err := e.waitGit(ctx, r.Host, prio); err != nil {
		return gitops.SyncResult{}, nil, err
	}
	remote, err := gitops.LsRemote(ctx, url)
	if err != nil {
		var g *gitops.Repo
		if _, serr := os.Stat(filepath.Join(gitDir, "HEAD")); serr == nil {
			g = &gitops.Repo{Path: gitDir}
		}
		return gitops.SyncResult{}, g, err
	}
	g, err := gitops.Init(ctx, gitDir)
	if err != nil {
		return gitops.SyncResult{}, nil, err
	}
	if unit == target.RepoUnit {
		if err := g.SetMeta(r.Description, r.Owner, url); err != nil {
			return gitops.SyncResult{}, g, err
		}
		if r.ParentID.Valid { // forks borrow objects from their parent: only own commits cost disk
			if _, err := os.Stat(filepath.Join(gitDir, "objects", "info", "alternates")); errors.Is(err, os.ErrNotExist) {
				if parent, err := e.Store.Repo(r.ParentID.Int64); err == nil {
					g.SetAlternates(e.abs(target.GitDir(parent.Path, target.RepoUnit, 0)), "")
				}
			}
		}
	} else {
		g.SetMeta(r.Description+" (wiki)", r.Owner, url)
	}
	sr, err := g.Sync(ctx, url, &remote, start)
	if err != nil {
		return sr, g, err
	}
	e.gitOK(r.Host)
	e.recordSync(r, sr, start.Unix(), unit)
	if sr.Destructive() {
		e.materialize(ctx, r, g, unit)
	}
	return sr, g, nil
}

// materialize makes sure every preserved state of a unit exists as a frozen,
// cloneable <unit>.vN.git next to it. Returns the number of versions.
func (e *Engine) materialize(ctx context.Context, r store.Repo, g *gitops.Repo, unit string) int {
	vs, err := g.Versions(ctx)
	if err != nil {
		return 0
	}
	for i, v := range vs {
		dir := e.abs(target.GitDir(r.Path, unit, i+1))
		desc := fmt.Sprintf("%s – %s as it was before upstream changed it (%s, version %d)", r.FullName(), unit, v.Stamp, i+1)
		if err := g.MaterializeVersion(ctx, dir, v, desc); err != nil {
			e.Log.Warn("materialize version", "repo", r.FullName(), "unit", unit, "version", i+1, "err", err)
		}
	}
	return len(vs)
}

// checkWiki mirrors <repo>.wiki.git. A missing wiki is normal ("none"); a wiki
// that disappears after we had it is history and gets an event.
func (e *Engine) checkWiki(ctx context.Context, r store.Repo, start time.Time) {
	url := provider.WikiURL(r.URL)
	gitDir := e.abs(target.GitDir(r.Path, target.WikiUnit, 0))
	e.setActivity(r.ID, "wiki "+r.FullName())
	_, g, err := e.syncUnit(ctx, r, target.WikiUnit, url, gitDir, start, PrioProbe)
	state, versions := "ok", r.WikiVersions
	var cool *CoolingError
	switch {
	case errors.As(err, &cool):
		return
	case errors.Is(err, gitops.ErrDenied) && r.WikiState == "ok":
		return // had a wiki a moment ago: rather throttling than deletion – look again next time
	case errors.Is(err, gitops.ErrDenied):
		err = gitops.ErrGone // forges answer 401 for "no wiki"
		fallthrough
	case errors.Is(err, gitops.ErrGone):
		state = "none"
		if r.WikiState == "ok" || r.WikiState == "gone" {
			state = "gone"
			if r.WikiState == "ok" {
				e.emit(store.Event{RepoID: sql.NullInt64{Int64: r.ID, Valid: true}, Kind: "gone", Ref: "wiki", Detail: "wiki removed upstream – kept"})
			}
		}
	case err != nil:
		e.Log.Warn("wiki", "repo", r.FullName(), "err", err)
		return
	}
	if g != nil {
		if vs, err := g.Versions(ctx); err == nil {
			versions = len(vs)
		}
	}
	e.Store.WikiChecked(r.ID, state, versions)
	if state != r.WikiState || versions != r.WikiVersions {
		e.mu.Lock()
		e.indexDirty = true
		e.mu.Unlock()
	}
}

// syncReleases mirrors release metadata (and assets, per plan). Errors never
// fail the git check; a rate limit just postpones to the next check.
func (e *Engine) syncReleases(ctx context.Context, r store.Repo, p provider.Provider, plan store.Plan, now time.Time) {
	rels, err := p.Releases(ctx, provider.Ref{Owner: r.Owner, Name: r.Name, ExtID: r.ExtID.String}, false)
	partial := errors.Is(err, provider.ErrPartial)
	if partial {
		err = nil // e.g. GitHub lists only the newest 1000 releases
	}
	var rl *api.RateLimitedError
	switch {
	case errors.As(err, &rl):
		return
	case errors.Is(err, api.ErrNotFound):
		rels, err = nil, nil // no releases endpoint for this repo (e.g. disabled)
	case err != nil:
		e.Log.Warn("releases", "repo", r.FullName(), "err", err)
		e.emit(store.Event{RepoID: sql.NullInt64{Int64: r.ID, Valid: true}, Kind: "error", Detail: "releases: " + err.Error()})
		e.Store.ReleasesChecked(r.ID, now.Unix())
		return
	}
	e.setActivity(r.ID, "releases "+r.FullName())
	evs, err := e.Releases.Sync(ctx, r, rels, releases.Options{Assets: plan.Releases == "assets", MaxBytes: plan.ReleaseMaxMB << 20, Partial: partial}, now)
	for _, ev := range evs {
		e.emit(ev)
	}
	if err != nil {
		e.Log.Warn("releases", "repo", r.FullName(), "err", err)
		return
	}
	e.Store.ReleasesChecked(r.ID, now.Unix())
	if len(evs) > 0 {
		e.Log.Info("releases", "repo", r.FullName(), "events", len(evs))
	}
}

func (e *Engine) effectivePlan(r store.Repo) store.Plan {
	if r.PlanID.Valid {
		if p, err := e.Store.Plan(r.PlanID.Int64); err == nil {
			return p
		}
	}
	ps, _ := e.Store.Plans()
	for _, p := range ps {
		if p.Name == "default" {
			return p
		}
	}
	if len(ps) > 0 {
		return ps[len(ps)-1]
	}
	return store.Plan{CheckEvery: 12 * 3600, CheckMin: 3600, CheckMax: 7 * 86400, DiscoverEvery: 12 * 3600, Adaptive: true}
}

// nextInterval implements the adaptive schedule: changes halve the interval,
// quiet checks stretch it by 1.5x, bounded by the plan.
func (e *Engine) nextInterval(r store.Repo, p store.Plan, changed bool, state string) int64 {
	if r.IntervalOverride.Valid && r.IntervalOverride.Int64 > 0 {
		return r.IntervalOverride.Int64
	}
	iv := r.Interval
	if iv <= 0 {
		iv = p.CheckEvery
	}
	switch {
	case state == "gone":
		return p.CheckMax
	case state == "error":
		iv = iv * 2
	case !p.Adaptive:
		iv = p.CheckEvery
	case changed:
		iv = iv / 2
	default:
		iv = iv * 3 / 2
	}
	if state == "error" && iv > p.CheckMax {
		return p.CheckMax
	}
	return max(p.CheckMin, min(iv, p.CheckMax))
}

// recordSync writes events: every destructive change individually, the rest
// as one summary event. Wiki refs are prefixed with "wiki:".
func (e *Engine) recordSync(r store.Repo, sr gitops.SyncResult, now int64, unit string) {
	if len(sr.Changes) == 0 {
		return
	}
	prefix := ""
	if unit != target.RepoUnit {
		prefix = unit + ":"
	}
	rid := sql.NullInt64{Int64: r.ID, Valid: true}
	var heads, tags, added, removed int
	var summary []string
	for _, c := range sr.Changes {
		if c.Kind == gitops.Removed { // merged branch deleted upstream: nothing lost, no version
			removed++
			continue
		}
		if c.Kind.Destructive() {
			e.emit(store.Event{RepoID: rid, TS: now, Kind: string(c.Kind), Ref: prefix + c.Ref, Old: c.Old, New: c.New,
				Preserved: c.Preserved, Version: sr.Version, Lost: c.Lost, Added: c.Added})
			e.Log.Warn("history change preserved", "repo", r.FullName(), "unit", unit, "ref", c.Ref, "kind", c.Kind, "lost", c.Lost, "version", sr.Version)
			continue
		}
		if strings.HasPrefix(c.Ref, "refs/tags/") {
			tags++
		} else {
			heads++
			added += c.Added
			if c.Kind == gitops.Updated && len(summary) < 5 {
				summary = append(summary, fmt.Sprintf("%s +%d", strings.TrimPrefix(c.Ref, "refs/heads/"), c.Added))
			}
		}
	}
	if heads+tags+removed == 0 {
		return
	}
	kind := "updated"
	first := r.LastCheck == 0 || r.State == "new"
	if unit == target.WikiUnit {
		first = r.WikiState == "" || r.WikiState == "none"
	}
	if first {
		kind = "created"
	}
	detail := strings.Join(summary, ", ")
	if tags > 0 {
		detail = strings.TrimPrefix(detail+fmt.Sprintf(", %d tag(s)", tags), ", ")
	}
	if kind == "created" {
		detail = fmt.Sprintf("%d branch(es), %d tag(s)", heads, tags)
	}
	if removed > 0 {
		detail = strings.TrimPrefix(detail+fmt.Sprintf(", %d merged branch(es) removed – nothing lost", removed), ", ")
	}
	if unit != target.RepoUnit {
		detail = unit + ": " + detail
	}
	e.emit(store.Event{RepoID: rid, TS: now, Kind: kind, Added: added, Detail: detail})
}

package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/api"
	"github.com/MarvAmBass/gitbackup/internal/config"
	"github.com/MarvAmBass/gitbackup/internal/provider"
	"github.com/MarvAmBass/gitbackup/internal/store"
	"github.com/MarvAmBass/gitbackup/internal/target"
)

// Discover refreshes the repo list of a source. manual=true may use the API reserve.
func (e *Engine) Discover(ctx context.Context, src store.Source, manual bool) {
	plan, _ := e.Store.Plan(src.PlanID)
	started := time.Now()
	seen := started.Unix()
	acctType, err := e.discover(ctx, src, seen, manual)
	next := started.Unix() + plan.DiscoverEvery
	msg := ""
	var rl *api.RateLimitedError
	switch {
	case errors.As(err, &rl):
		next = rl.Until.Unix() + 60 + rand.Int64N(300)
		msg = err.Error()
	case err != nil:
		next = started.Unix() + min(plan.DiscoverEvery, 3600)
		msg = err.Error()
	}
	if err != nil {
		e.Log.Warn("discover", "source", src.Target, "host", src.Host, "err", err)
	} else {
		e.Log.Info("discovered", "source", src.Target, "host", src.Host, "took", time.Since(started).Round(time.Millisecond))
	}
	if err := e.Store.SourceDiscovered(src.ID, started.Unix(), next, acctType, msg); err != nil {
		e.Log.Error("save source", "err", err)
	}
	if err := e.Store.RecomputePlans(); err != nil {
		e.Log.Error("recompute plans", "err", err)
	}
	if err := e.SyncViews(); err != nil {
		e.Log.Error("views", "err", err)
	}
	e.mu.Lock()
	e.indexDirty = true
	e.mu.Unlock()
}

// budgetOK gates optional API work (metadata, releases, forks): manual
// actions always pass, everything else is paced by Client.Background.
func budgetOK(c *api.Client, manual bool) bool {
	return c == nil || manual || c.Background()
}

// resolveKind stores how an ambiguous source (GitLab a/b) turned out, in the
// database and in gitbackup.yaml.
func (e *Engine) resolveKind(src store.Source, kind string) {
	e.Store.SetSourceKind(src.ID, kind)
	err := e.Cfg.Update(func(c *config.Config) error {
		for i := range c.Sources {
			if c.Sources[i].Platform == src.Platform && c.Sources[i].Host == src.Host && c.Sources[i].Target == src.Target {
				c.Sources[i].Kind = kind
				if kind == target.Account && !c.Sources[i].Repos {
					c.Sources[i].Repos = true
				}
			}
		}
		return nil
	})
	if err != nil {
		e.Log.Warn("save resolved source kind", "source", src.Target, "err", err)
	}
}

func (e *Engine) discover(ctx context.Context, src store.Source, seen int64, manual bool) (string, error) {
	plat := provider.Platform(src.Platform)
	p := e.Forges.For(plat, src.Host)

	if plat == provider.Git {
		host, owner, name := target.PlainRepo(src.Target)
		return "", e.upsertLinked(src, "single", seen, store.Repo{Kind: "repo", Platform: string(plat), Host: host, Owner: owner,
			Name: name, URL: src.Target, Path: target.Slot("repo", host, owner, name)})
	}

	kind := src.Kind
	if kind == target.Auto || kind == target.Repo {
		// Metadata is nice-to-have for single repos: only spend quota if the budget allows.
		var r provider.Repo
		var err error = api.ErrNotFound
		if kind == target.Auto || budgetOK(p.Client(), manual) {
			r, err = p.Repo(ctx, src.Target, manual)
		}
		switch {
		case err == nil:
			if kind == target.Auto {
				e.resolveKind(src, target.Repo)
			}
			return "", e.upsertLinked(src, "single", seen, e.fromProvider(p, r, "repo", src.Target))
		case kind == target.Auto && errors.Is(err, api.ErrNotFound):
			e.resolveKind(src, target.Account) // GitLab group/subgroup
			src.WithRepos = true
		case kind == target.Auto:
			return "", err
		default:
			var rl *api.RateLimitedError
			if err != nil && !errors.Is(err, api.ErrNotFound) && !errors.As(err, &rl) {
				return "", err
			}
			// not found / no budget: keep watching via git; Check() marks it gone if needed
			owner, name := splitLast(src.Target)
			return "", e.upsertLinked(src, "single", seen, store.Repo{Kind: "repo", Platform: string(plat), Host: src.Host,
				Owner: owner, Name: name, URL: p.CloneURL(src.Target), Path: target.Slot("repo", src.Host, owner, name)})
		}
	}

	// account: user, org or group
	acctType, err := p.AccountType(ctx, src.Target, manual)
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			return "", fmt.Errorf("account %q not found on %s", src.Target, src.Host)
		}
		return "", err
	}
	feat := p.Supports()
	if src.WithRepos || src.WithForks {
		repos, err := p.Repos(ctx, src.Target, manual)
		if err != nil {
			return acctType, err
		}
		for _, r := range repos {
			if r.Fork && !src.WithForks || !r.Fork && !src.WithRepos {
				continue
			}
			kind, role := "repo", "own"
			if r.Fork {
				kind, role = "fork", "fork"
			}
			if err := e.upsertLinked(src, role, seen, e.fromProvider(p, r, kind, "")); err != nil {
				return acctType, err
			}
		}
		for _, role := range []string{"own", "fork"} {
			if err := e.unlinkStale(src, role, seen); err != nil {
				return acctType, err
			}
		}
	}
	if src.WithStars && feat.Stars {
		stars, err := p.Starred(ctx, src.Target, manual)
		if err != nil {
			return acctType, err
		}
		for _, r := range stars {
			kind := "repo"
			if r.Fork {
				kind = "fork"
			}
			if err := e.upsertLinked(src, "star", seen, e.fromProvider(p, r, kind, "")); err != nil {
				return acctType, err
			}
		}
		if err := e.unlinkStale(src, "star", seen); err != nil {
			return acctType, err
		}
	}
	if src.WithGists && feat.Gists {
		gists, err := p.Gists(ctx, src.Target, manual)
		if err != nil {
			return acctType, err
		}
		kind := "gist"
		if plat == provider.GitLab {
			kind = "snippet"
		}
		for _, g := range gists {
			if err := e.upsertLinked(src, "gist", seen, e.fromProvider(p, g, kind, "")); err != nil {
				return acctType, err
			}
		}
		if err := e.unlinkStale(src, "gist", seen); err != nil {
			return acctType, err
		}
	}
	return acctType, nil
}

func splitLast(p string) (string, string) {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

// fromProvider maps a forge repo to a store row. pathAs keeps the folder the
// user typed for single repos (the API may report other casing).
func (e *Engine) fromProvider(p provider.Provider, r provider.Repo, kind, pathAs string) store.Repo {
	owner, name := r.Owner, r.Name
	if pathAs != "" {
		owner, name = splitLast(pathAs)
	}
	clone := r.CloneURL
	if clone == "" {
		clone = p.CloneURL(r.Owner + "/" + r.Name)
	}
	var ext sql.NullString
	if r.ExtID != "" {
		ext = sql.NullString{String: r.ExtID, Valid: true}
	}
	return store.Repo{
		Kind: kind, Platform: string(p.Platform()), Host: p.Host(), Owner: r.Owner, Name: r.Name,
		Path: target.Slot(kind, p.Host(), owner, name),
		URL:  clone, ExtID: ext, Description: r.Description, DefaultBranch: r.DefaultBranch, Archived: r.Archived,
		PushedAt: r.PushedAt.Unix(),
	}
}

// upsertLinked stores (or refreshes) a repo and links it to the source. If
// the repo already lives elsewhere (e.g. found as a fork of a watched repo
// first), this route becomes a symlink to it.
func (e *Engine) upsertLinked(src store.Source, role string, seen int64, r store.Repo) error {
	r.PlanID = sql.NullInt64{Int64: src.PlanID, Valid: true}
	id, created, renamedFrom, existingPath, err := e.Store.UpsertRepo(r)
	if err != nil {
		return fmt.Errorf("save %s: %w", r.FullName(), err)
	}
	if !created && existingPath != r.Path && role != "star" {
		if err := e.Store.AddAlias(id, r.Path); err != nil {
			e.Log.Warn("alias", "repo", r.FullName(), "err", err)
		}
	}
	rid := sql.NullInt64{Int64: id, Valid: true}
	sid := sql.NullInt64{Int64: src.ID, Valid: true}
	if created {
		e.emit(store.Event{RepoID: rid, SourceID: sid, Kind: "discovered", Detail: role + " of " + src.Target})
	}
	if renamedFrom != "" {
		e.emit(store.Event{RepoID: rid, SourceID: sid, Kind: "renamed", Old: renamedFrom, New: r.URL})
	}
	return e.Store.LinkRepo(src.ID, id, role, seen)
}

func (e *Engine) unlinkStale(src store.Source, role string, seen int64) error {
	gone, err := e.Store.UnlinkStale(src.ID, role, seen)
	for _, r := range gone {
		kind := map[string]string{"star": "unstarred"}[role]
		if kind == "" {
			kind = "unlisted"
		}
		// Not deleted: the repo stays in the backup. Check() finds out whether it is really gone.
		e.emit(store.Event{RepoID: sql.NullInt64{Int64: r.ID, Valid: true}, SourceID: sql.NullInt64{Int64: src.ID, Valid: true},
			Kind: kind, Detail: "no longer listed by " + src.Target})
		e.Store.TriggerCheck(r.ID)
	}
	return err
}

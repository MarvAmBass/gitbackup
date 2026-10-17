package store

import (
	"strings"

	"github.com/MarvAmBass/gitbackup/internal/config"
)

// SyncConfig mirrors plans, sources and repo overrides from gitbackup.yaml
// into the database (plans by name, sources by platform|host|target).
// Runtime state of existing rows is kept; removed sources stop being watched
// but their repositories stay.
func (s *Store) SyncConfig(c *config.Config) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	planNames := []any{}
	for _, p := range c.Plans {
		forks := "off"
		if p.Forks.Enabled {
			forks = "on"
		}
		if _, err := tx.Exec(`INSERT INTO plans(name,priority,discover_every,check_every,check_min,check_max,adaptive,releases,releases_every,release_max_mb,
				wiki,forks,fork_min_ahead,fork_max) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(name) DO UPDATE SET priority=excluded.priority, discover_every=excluded.discover_every,
				check_every=excluded.check_every, check_min=excluded.check_min, check_max=excluded.check_max, adaptive=excluded.adaptive,
				releases=excluded.releases, releases_every=excluded.releases_every, release_max_mb=excluded.release_max_mb, wiki=excluded.wiki, forks=excluded.forks,
				fork_min_ahead=excluded.fork_min_ahead, fork_max=excluded.fork_max`,
			p.Name, config.PriorityNum(p.Priority), p.DiscoverEvery.Seconds(), p.Check.Every.Seconds(), p.Check.Min.Seconds(),
			p.Check.Max.Seconds(), p.Check.Adaptive, p.Releases, p.ReleasesEvery.Seconds(), p.ReleaseMaxMB, p.Wiki, forks, p.Forks.MinAhead, p.Forks.Max); err != nil {
			return err
		}
		planNames = append(planNames, p.Name)
	}

	keys := []any{}
	for _, x := range c.Sources {
		if _, err := tx.Exec(`INSERT INTO sources(platform,host,kind,target,plan_id,with_repos,with_stars,with_gists,with_forks,enabled,note,created)
				VALUES(?,?,?,?,(SELECT id FROM plans WHERE name=?),?,?,?,?,?,?,?)
			ON CONFLICT(platform,host,target) DO UPDATE SET kind=excluded.kind, plan_id=excluded.plan_id, with_repos=excluded.with_repos,
				with_stars=excluded.with_stars, with_gists=excluded.with_gists, with_forks=excluded.with_forks, enabled=excluded.enabled,
				note=excluded.note`,
			x.Platform, x.Host, x.Kind, x.Target, x.Plan, x.Repos || x.Kind != "account", x.Stars, x.Gists, x.Forks, !x.Disabled, x.Note, now()); err != nil {
			return err
		}
		keys = append(keys, x.Key())
	}
	if _, err := tx.Exec(`DELETE FROM sources WHERE platform||'|'||host||'|'||target NOT IN (`+placeholders(len(keys))+`)`, keys...); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE repos SET plan_id=NULL WHERE plan_id IN (SELECT id FROM plans WHERE name NOT IN (`+placeholders(len(planNames))+`))`, planNames...); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM plans WHERE name NOT IN (`+placeholders(len(planNames))+`)`, planNames...); err != nil {
		return err
	}

	if _, err := tx.Exec(`UPDATE repos SET priority_override=NULL, interval_override=NULL, paused=0`); err != nil {
		return err
	}
	for _, o := range c.Overrides {
		var prio any
		if o.Priority != "" {
			prio = config.PriorityNum(o.Priority)
		}
		var iv any
		if o.Interval > 0 {
			iv = o.Interval.Seconds()
		}
		if _, err := tx.Exec(`UPDATE repos SET priority_override=?, interval_override=?, paused=? WHERE path=?`,
			prio, iv, o.Paused, strings.Trim(o.Repo, "/")); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.RecomputePlans()
}

func placeholders(n int) string {
	if n == 0 {
		return "SELECT NULL WHERE 0" // empty set: x NOT IN () is true (NOT IN (NULL) would be NULL)
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

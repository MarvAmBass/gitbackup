// Package store persists configuration, repo state and events in SQLite.
package store

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

// Open opens (and migrates) the database. journal is "wal" (default, local
// disks) or "delete" (safer on network filesystems such as NFS, where WAL's
// shared-memory locking does not work).
func Open(path, journal string) (*Store, error) {
	mode := "WAL"
	if strings.EqualFold(journal, "delete") {
		mode = "DELETE"
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode("+mode+")&_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: one writer; keeps things simple and BUSY-free
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	s := &Store{db: db}
	return s, s.addColumns()
}

func (s *Store) Close() error { return s.db.Close() }

// added lists columns introduced after a database may already exist; they are
// added in place (schema.sql has them for new databases).
var added = []struct{ table, column, ddl string }{
	{"plans", "releases_every", "INTEGER NOT NULL DEFAULT 604800"},
	{"repos", "gone_since", "INTEGER NOT NULL DEFAULT 0"},
	{"repos", "wiki_checked", "INTEGER NOT NULL DEFAULT 0"},
	{"repos", "forced", "INTEGER NOT NULL DEFAULT 0"},
}

func (s *Store) addColumns() error {
	for _, a := range added {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, a.table, a.column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := s.db.Exec(`ALTER TABLE ` + a.table + ` ADD COLUMN ` + a.column + ` ` + a.ddl); err != nil {
				return fmt.Errorf("add %s.%s: %w", a.table, a.column, err)
			}
		}
	}
	return nil
}

func now() int64 { return time.Now().Unix() }

// ---------------------------------------------------------------- settings

func (s *Store) Setting(key, def string) string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ---------------------------------------------------------------- plans

type Plan struct {
	ID            int64
	Name          string
	Priority      int
	DiscoverEvery int64
	CheckEvery    int64
	CheckMin      int64
	CheckMax      int64
	Adaptive      bool
	Releases      string
	ReleasesEvery int64
	ReleaseMaxMB  int64
	Forks         string
	ForkMinAhead  int64
	ForkMax       int64
	Wiki          bool
	Sources       int // usage count (list only)
}

const planCols = `id,name,priority,discover_every,check_every,check_min,check_max,adaptive,releases,releases_every,release_max_mb,forks,fork_min_ahead,fork_max,wiki`

func scanPlan(sc interface{ Scan(...any) error }, p *Plan, extra ...any) error {
	return sc.Scan(append([]any{&p.ID, &p.Name, &p.Priority, &p.DiscoverEvery, &p.CheckEvery, &p.CheckMin, &p.CheckMax,
		&p.Adaptive, &p.Releases, &p.ReleasesEvery, &p.ReleaseMaxMB, &p.Forks, &p.ForkMinAhead, &p.ForkMax, &p.Wiki}, extra...)...)
}

func (s *Store) Plans() ([]Plan, error) {
	rows, err := s.db.Query(`SELECT ` + planCols + `,(SELECT count(*) FROM sources WHERE plan_id=plans.id) FROM plans ORDER BY priority DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		var p Plan
		if err := scanPlan(rows, &p, &p.Sources); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Plan(id int64) (Plan, error) {
	var p Plan
	err := scanPlan(s.db.QueryRow(`SELECT `+planCols+` FROM plans WHERE id=?`, id), &p)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// RecomputePlans sets each repo's effective plan to the strongest plan of the
// sources that reference it (highest priority, then most frequent checks).
func (s *Store) RecomputePlans() error {
	_, err := s.db.Exec(`UPDATE repos SET plan_id = COALESCE((
		SELECT p.id FROM source_repos sr
		JOIN sources s ON s.id = sr.source_id
		JOIN plans p ON p.id = s.plan_id
		WHERE sr.repo_id = repos.id
		ORDER BY p.priority DESC, p.check_every ASC LIMIT 1), plan_id)`)
	return err
}

// ---------------------------------------------------------------- sources

type Source struct {
	ID           int64
	Platform     string
	Host         string
	Kind         string
	Target       string
	PlanID       int64
	WithRepos    bool
	WithStars    bool
	WithGists    bool
	WithForks    bool
	Enabled      bool
	Note         string
	AccountType  string
	LastDiscover int64
	NextDiscover int64
	LastError    string
	Created      int64
	// list-only
	PlanName string
	Repos    int
	Stars    int
}

const sourceCols = `s.id,s.platform,s.host,s.kind,s.target,s.plan_id,s.with_repos,s.with_stars,s.with_gists,s.with_forks,s.enabled,s.note,s.account_type,s.last_discover,s.next_discover,s.last_error,s.created`

func scanSource(sc interface{ Scan(...any) error }, x *Source, extra ...any) error {
	return sc.Scan(append([]any{&x.ID, &x.Platform, &x.Host, &x.Kind, &x.Target, &x.PlanID, &x.WithRepos, &x.WithStars, &x.WithGists, &x.WithForks,
		&x.Enabled, &x.Note, &x.AccountType, &x.LastDiscover, &x.NextDiscover, &x.LastError, &x.Created}, extra...)...)
}

func (s *Store) Sources() ([]Source, error) {
	rows, err := s.db.Query(`SELECT ` + sourceCols + `, p.name,
		(SELECT count(*) FROM source_repos WHERE source_id=s.id AND role!='star'),
		(SELECT count(*) FROM source_repos WHERE source_id=s.id AND role='star')
		FROM sources s JOIN plans p ON p.id=s.plan_id ORDER BY s.host, lower(s.target)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var x Source
		if err := scanSource(rows, &x, &x.PlanName, &x.Repos, &x.Stars); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) Source(id int64) (Source, error) {
	var x Source
	err := scanSource(s.db.QueryRow(`SELECT `+sourceCols+`, p.name FROM sources s JOIN plans p ON p.id=s.plan_id WHERE s.id=?`, id), &x, &x.PlanName)
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	return x, err
}

// SourceByKey finds a source by platform, host and target.
func (s *Store) SourceByKey(platform, host, target string) (Source, error) {
	var x Source
	err := scanSource(s.db.QueryRow(`SELECT `+sourceCols+`, p.name FROM sources s JOIN plans p ON p.id=s.plan_id
		WHERE s.platform=? AND s.host=? AND s.target=?`, platform, host, target), &x, &x.PlanName)
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	return x, err
}

// SetSourceKind records how an ambiguous source (GitLab a/b) was resolved.
func (s *Store) SetSourceKind(id int64, kind string) error {
	_, err := s.db.Exec(`UPDATE sources SET kind=? WHERE id=?`, kind, id)
	return err
}

func (s *Store) TriggerDiscover(id int64) error {
	_, err := s.db.Exec(`UPDATE sources SET next_discover=0 WHERE id=?`, id)
	return err
}

func (s *Store) DueSources(at int64) ([]Source, error) {
	rows, err := s.db.Query(`SELECT `+sourceCols+`, p.name FROM sources s JOIN plans p ON p.id=s.plan_id
		WHERE s.enabled=1 AND s.next_discover<=? ORDER BY p.priority DESC, s.next_discover`, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var x Source
		if err := scanSource(rows, &x, &x.PlanName); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) SourceDiscovered(id, at, next int64, accountType, errMsg string) error {
	_, err := s.db.Exec(`UPDATE sources SET last_discover=CASE WHEN ?='' THEN ? ELSE last_discover END, next_discover=?, last_error=?,
		account_type=CASE WHEN ?!='' THEN ? ELSE account_type END WHERE id=?`,
		errMsg, at, next, errMsg, accountType, accountType, id)
	return err
}

// ---------------------------------------------------------------- repos

type Repo struct {
	ID               int64
	Kind             string
	Platform         string
	Host             string
	Owner            string
	Name             string
	Path             string
	URL              string
	ExtID            sql.NullString
	ParentID         sql.NullInt64
	Description      string
	DefaultBranch    string
	State            string
	Archived         bool
	Paused           bool
	PlanID           sql.NullInt64
	PriorityOverride sql.NullInt64
	IntervalOverride sql.NullInt64
	Interval         int64
	PushedAt         int64
	LastCheck        int64
	NextCheck        int64
	LastChange       int64
	LastCommit       int64
	LastError        string
	Versions         int
	SizeBytes        int64
	GoneSince        int64 // no longer available upstream since
	WikiState        string
	WikiVersions     int
	WikiChecked      int64
	Forced           bool
	Ahead            int
	ForksChecked     int64
	ReleasesChecked  int64
	Created          int64
	// joined
	PlanName   string
	Priority   int   // effective
	Listed     int64 // last discovery that listed it (pushed_at is that fresh)
	Changes30d int
}

// DefaultHosts need no host prefix in names.
var DefaultHosts = map[string]bool{"github.com": true, "gitlab.com": true, "codeberg.org": true, "git.sr.ht": true}

// FullName is owner/name on the big public forges and host[/owner]/name elsewhere.
func (r Repo) FullName() string {
	switch {
	case DefaultHosts[r.Host]:
		return r.Owner + "/" + r.Name
	case r.Owner == "":
		return r.Host + "/" + r.Name
	}
	return r.Host + "/" + r.Owner + "/" + r.Name
}

const repoCols = `r.id,r.kind,r.platform,r.host,r.owner,r.name,r.path,r.url,r.ext_id,r.parent_id,r.description,r.default_branch,r.state,r.archived,r.paused,
	r.plan_id,r.priority_override,r.interval_override,r.interval,r.pushed_at,r.last_check,r.next_check,r.last_change,r.last_commit,
	r.last_error,r.versions,r.size_bytes,r.gone_since,r.wiki_state,r.wiki_versions,r.wiki_checked,r.forced,r.ahead,
	r.forks_checked,r.releases_checked,r.created,
	COALESCE(p.name,''), COALESCE(r.priority_override, p.priority, 1),
	COALESCE((SELECT max(seen) FROM source_repos sr WHERE sr.repo_id=r.id), 0)`

const repoFrom = ` FROM repos r LEFT JOIN plans p ON p.id=r.plan_id `

// repoTargets returns scan destinations for repoCols + changes30d.
func repoTargets(r *Repo) []any {
	return []any{&r.ID, &r.Kind, &r.Platform, &r.Host, &r.Owner, &r.Name, &r.Path, &r.URL, &r.ExtID, &r.ParentID, &r.Description, &r.DefaultBranch,
		&r.State, &r.Archived, &r.Paused, &r.PlanID, &r.PriorityOverride, &r.IntervalOverride, &r.Interval, &r.PushedAt,
		&r.LastCheck, &r.NextCheck, &r.LastChange, &r.LastCommit, &r.LastError, &r.Versions, &r.SizeBytes,
		&r.GoneSince, &r.WikiState, &r.WikiVersions, &r.WikiChecked, &r.Forced, &r.Ahead,
		&r.ForksChecked, &r.ReleasesChecked, &r.Created, &r.PlanName, &r.Priority, &r.Listed, &r.Changes30d}
}

func (s *Store) queryRepos(q string, args ...any) ([]Repo, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		var r Repo
		if err := rows.Scan(repoTargets(&r)...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const changes30d = `, (SELECT count(DISTINCT ts) FROM events e WHERE e.repo_id=r.id AND e.kind IN ('updated','rewritten','tag-moved','deleted') AND e.ts > strftime('%s','now')-2592000) AS changes30d`

func (s *Store) Repo(id int64) (Repo, error) {
	rs, err := s.queryRepos(`SELECT `+repoCols+changes30d+repoFrom+`WHERE r.id=?`, id)
	if err != nil {
		return Repo{}, err
	}
	if len(rs) == 0 {
		return Repo{}, ErrNotFound
	}
	return rs[0], nil
}

type RepoFilter struct {
	Query  string
	State  string
	Kind   string
	Source int64
	Sort   string // name | hot | next | changed | size | versions
	Limit  int
	Offset int
}

func (s *Store) Repos(f RepoFilter) ([]Repo, int, error) {
	where, args := []string{"1=1"}, []any{}
	if f.Query != "" {
		where = append(where, `(r.owner||'/'||r.name LIKE ? OR r.description LIKE ? OR r.url LIKE ?)`)
		q := "%" + f.Query + "%"
		args = append(args, q, q, q)
	}
	switch f.State {
	case "":
	case "rewritten":
		where = append(where, `r.versions>0`)
	case "paused":
		where = append(where, `r.paused=1`)
	default:
		where = append(where, `r.state=?`)
		args = append(args, f.State)
	}
	if f.Kind != "" {
		where = append(where, `r.kind=?`)
		args = append(args, f.Kind)
	}
	if f.Source != 0 {
		where = append(where, `r.id IN (SELECT repo_id FROM source_repos WHERE source_id=?)`)
		args = append(args, f.Source)
	}
	order := map[string]string{
		"name":     "lower(r.owner), lower(r.name)",
		"hot":      "COALESCE(r.priority_override, p.priority, 1) DESC, changes30d DESC, r.last_change DESC",
		"next":     "r.paused, r.next_check",
		"changed":  "r.last_change DESC",
		"size":     "r.size_bytes DESC",
		"versions": "r.versions DESC, r.last_change DESC",
	}[f.Sort]
	if order == "" {
		order = "lower(r.owner), lower(r.name)"
	}
	if f.Limit == 0 {
		f.Limit = 100
	}
	w := strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRow(`SELECT count(*)`+repoFrom+`WHERE `+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rs, err := s.queryRepos(`SELECT `+repoCols+changes30d+repoFrom+`WHERE `+w+` ORDER BY `+order+` LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...)
	return rs, total, err
}

// UpsertRepo inserts or refreshes a repo found by discovery. Identity is the
// platform id when known (survives renames and shows up under several
// sources), else the path. existingPath is the folder of an already stored
// repo; when it differs from r.Path the caller records an alias (symlink).
func (s *Store) UpsertRepo(r Repo) (id int64, created bool, renamedFrom, existingPath string, err error) {
	var oldURL string
	if r.ExtID.Valid {
		err = s.db.QueryRow(`SELECT id,url,path FROM repos WHERE host=? AND ext_id=?`, r.Host, r.ExtID).Scan(&id, &oldURL, &existingPath)
	}
	if id == 0 {
		err = s.db.QueryRow(`SELECT id,url,path FROM repos WHERE path=?`, r.Path).Scan(&id, &oldURL, &existingPath)
	}
	if errors.Is(err, sql.ErrNoRows) || id == 0 {
		res, err := s.db.Exec(`INSERT INTO repos(kind,platform,host,owner,name,path,url,ext_id,parent_id,description,default_branch,archived,pushed_at,plan_id,ahead,created)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.Kind, r.Platform, r.Host, r.Owner, r.Name, r.Path, r.URL, r.ExtID, r.ParentID,
			r.Description, r.DefaultBranch, r.Archived, r.PushedAt, r.PlanID, r.Ahead, now())
		if err != nil {
			return 0, false, "", "", err
		}
		id, err = res.LastInsertId()
		return id, true, "", r.Path, err
	}
	if err != nil {
		return 0, false, "", "", err
	}
	if oldURL != r.URL {
		renamedFrom = oldURL
	}
	// Path stays stable on rename; the URL follows upstream.
	_, err = s.db.Exec(`UPDATE repos SET url=?, owner=CASE WHEN ext_id IS NULL THEN owner ELSE ? END, name=CASE WHEN ext_id IS NULL THEN name ELSE ? END,
		ext_id=COALESCE(?,ext_id), parent_id=COALESCE(parent_id,?), description=CASE WHEN ?!='' THEN ? ELSE description END,
		default_branch=CASE WHEN ?!='' THEN ? ELSE default_branch END, archived=?,
		next_check=CASE WHEN ?>last_check AND last_check>0 THEN 0 ELSE next_check END, pushed_at=? WHERE id=?`,
		r.URL, r.Owner, r.Name, r.ExtID, r.ParentID, r.Description, r.Description, r.DefaultBranch, r.DefaultBranch, r.Archived,
		r.PushedAt, r.PushedAt, id)
	return id, false, renamedFrom, existingPath, err
}

// AddAlias records an extra place (symlink) where a repo appears.
func (s *Store) AddAlias(repoID int64, path string) error {
	_, err := s.db.Exec(`INSERT INTO repo_aliases(repo_id,path) VALUES(?,?) ON CONFLICT(path) DO NOTHING`, repoID, path)
	return err
}

// Aliases maps alias path → canonical repo path.
func (s *Store) Aliases() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT a.path, r.path FROM repo_aliases a JOIN repos r ON r.id=a.repo_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var a, p string
		if err := rows.Scan(&a, &p); err != nil {
			return nil, err
		}
		out[a] = p
	}
	return out, rows.Err()
}

// RepoAliases lists the alias paths of one repo.
func (s *Store) RepoAliases(repoID int64) []string {
	rows, err := s.db.Query(`SELECT path FROM repo_aliases WHERE repo_id=? ORDER BY path`, repoID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		out = append(out, p)
	}
	return out
}

// RepoByPath finds a repo by its folder.
func (s *Store) RepoByPath(path string) (Repo, error) {
	rs, err := s.queryRepos(`SELECT `+repoCols+changes30d+repoFrom+`WHERE r.path=?`, path)
	if err != nil || len(rs) == 0 {
		return Repo{}, ErrNotFound
	}
	return rs[0], nil
}

// RepoByExt finds a repo by its platform id.
func (s *Store) RepoByExt(host, extID string) (Repo, error) {
	rs, err := s.queryRepos(`SELECT `+repoCols+changes30d+repoFrom+`WHERE r.host=? AND r.ext_id=?`, host, extID)
	if err != nil || len(rs) == 0 {
		return Repo{}, ErrNotFound
	}
	return rs[0], nil
}

// ForksOf lists stored forks of a repo (found by fork scanning).
func (s *Store) ForksOf(parentID int64) ([]Repo, error) {
	return s.queryRepos(`SELECT `+repoCols+changes30d+repoFrom+`WHERE r.parent_id=? ORDER BY r.ahead DESC`, parentID)
}

func (s *Store) ForksChecked(id, at int64) error {
	_, err := s.db.Exec(`UPDATE repos SET forks_checked=? WHERE id=?`, at, id)
	return err
}

func (s *Store) WikiChecked(id int64, state string, versions int) error {
	_, err := s.db.Exec(`UPDATE repos SET wiki_state=?, wiki_versions=?, wiki_checked=strftime('%s','now') WHERE id=?`, state, versions, id)
	return err
}

func (s *Store) SetVersions(id int64, versions int) error {
	_, err := s.db.Exec(`UPDATE repos SET versions=? WHERE id=?`, versions, id)
	return err
}

func (s *Store) LinkRepo(sourceID, repoID int64, role string, seen int64) error {
	_, err := s.db.Exec(`INSERT INTO source_repos(source_id,repo_id,role,seen) VALUES(?,?,?,?)
		ON CONFLICT(source_id,repo_id,role) DO UPDATE SET seen=excluded.seen`, sourceID, repoID, role, seen)
	return err
}

// UnlinkStale removes links not seen in the latest discovery and returns the
// affected repos. The repos themselves stay.
func (s *Store) UnlinkStale(sourceID int64, role string, seen int64) ([]Repo, error) {
	rs, err := s.queryRepos(`SELECT `+repoCols+changes30d+repoFrom+`WHERE r.id IN (SELECT repo_id FROM source_repos WHERE source_id=? AND role=? AND seen<?)`,
		sourceID, role, seen)
	if err != nil {
		return nil, err
	}
	_, err = s.db.Exec(`DELETE FROM source_repos WHERE source_id=? AND role=? AND seen<?`, sourceID, role, seen)
	return rs, err
}

// Linked returns (source target, role) pairs referencing a repo.
func (s *Store) Linked(repoID int64) ([]Source, []string, error) {
	rows, err := s.db.Query(`SELECT `+sourceCols+`, p.name, sr.role FROM source_repos sr JOIN sources s ON s.id=sr.source_id
		JOIN plans p ON p.id=s.plan_id WHERE sr.repo_id=? ORDER BY s.target`, repoID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var srcs []Source
	var roles []string
	for rows.Next() {
		var x Source
		var role string
		if err := scanSource(rows, &x, &x.PlanName, &role); err != nil {
			return nil, nil, err
		}
		srcs, roles = append(srcs, x), append(roles, role)
	}
	return srcs, roles, rows.Err()
}

// StarViews maps "host/user" to the repos that user starred.
func (s *Store) StarViews() (map[string][]Repo, error) {
	rows, err := s.db.Query(`SELECT s.host||'/'||s.target, ` + repoCols + changes30d + repoFrom + `JOIN source_repos sr ON sr.repo_id=r.id AND sr.role='star'
		JOIN sources s ON s.id=sr.source_id ORDER BY s.target, lower(r.owner), lower(r.name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Repo{}
	for rows.Next() {
		var user string
		var r Repo
		if err := rows.Scan(append([]any{&user}, repoTargets(&r)...)...); err != nil {
			return nil, err
		}
		out[user] = append(out[user], r)
	}
	return out, rows.Err()
}

// DueRepos returns repos to check now, most important first.
func (s *Store) DueRepos(at int64, limit int, exclude []int64) ([]Repo, error) {
	ex := "0"
	for _, id := range exclude {
		ex += fmt.Sprintf(",%d", id)
	}
	// First clones before everything else, then priority, then the longest overdue.
	return s.queryRepos(`SELECT `+repoCols+changes30d+repoFrom+`WHERE r.paused=0 AND r.next_check<=? AND r.id NOT IN (`+ex+`)
		ORDER BY (r.last_commit=0 AND r.state IN ('new','error')) DESC, COALESCE(r.priority_override, p.priority, 1) DESC, r.next_check LIMIT ?`, at, limit)
}

type CheckResult struct {
	State         string
	Changed       bool
	Destructive   bool
	Interval      int64
	NextCheck     int64
	LastCommit    int64
	Versions      int
	SizeBytes     int64
	DefaultBranch string
	Error         string
}

func (s *Store) ReleasesChecked(id, at int64) error {
	_, err := s.db.Exec(`UPDATE repos SET releases_checked=? WHERE id=?`, at, id)
	return err
}

func (s *Store) RepoChecked(id int64, at int64, c CheckResult) error {
	_, err := s.db.Exec(`UPDATE repos SET forced=0, gone_since=CASE WHEN ?='gone' AND state!='gone' THEN ? WHEN ?='gone' THEN gone_since ELSE 0 END,
		state=?, interval=?, next_check=?, last_check=?,
		last_change=CASE WHEN ? THEN ? ELSE last_change END,
		last_commit=CASE WHEN ?>0 THEN ? ELSE last_commit END,
		versions=?, size_bytes=CASE WHEN ?>0 THEN ? ELSE size_bytes END,
		default_branch=CASE WHEN ?!='' THEN ? ELSE default_branch END,
		last_error=? WHERE id=?`,
		c.State, at, c.State, c.State, c.Interval, c.NextCheck, at, c.Changed, at, c.LastCommit, c.LastCommit, c.Versions,
		c.SizeBytes, c.SizeBytes, c.DefaultBranch, c.DefaultBranch, c.Error, id)
	return err
}

// Postpone moves a check without touching the repo's state.
func (s *Store) Postpone(id, until int64) error {
	_, err := s.db.Exec(`UPDATE repos SET next_check=? WHERE id=?`, until, id)
	return err
}

// RetryErrors schedules every repo in error state now, with a fresh interval.
func (s *Store) RetryErrors() (int64, error) {
	res, err := s.db.Exec(`UPDATE repos SET next_check=0, interval=0 WHERE state='error'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TriggerCheck runs a full git check as soon as possible.
func (s *Store) TriggerCheck(id int64) error {
	_, err := s.db.Exec(`UPDATE repos SET next_check=0, forced=1 WHERE id=?`, id)
	return err
}

// AllRepos returns every repo (for index generation).
func (s *Store) AllRepos() ([]Repo, error) {
	return s.queryRepos(`SELECT ` + repoCols + changes30d + repoFrom + `ORDER BY r.host, lower(r.owner), lower(r.name)`)
}

// ---------------------------------------------------------------- events

type Event struct {
	ID        int64
	RepoID    sql.NullInt64
	SourceID  sql.NullInt64
	TS        int64
	Kind      string
	Ref       string
	Old, New  string
	Preserved string
	Version   string
	Lost      int
	Added     int
	Detail    string
	// joined
	RepoName string
}

func (s *Store) AddEvent(e Event) error {
	if e.TS == 0 {
		e.TS = now()
	}
	_, err := s.db.Exec(`INSERT INTO events(repo_id,source_id,ts,kind,ref,old,new,preserved,version,lost,added,detail) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.RepoID, e.SourceID, e.TS, e.Kind, e.Ref, e.Old, e.New, e.Preserved, e.Version, e.Lost, e.Added, e.Detail)
	return err
}

type EventFilter struct {
	RepoID int64
	Kinds  []string
	Limit  int
}

func (s *Store) Events(f EventFilter) ([]Event, error) {
	where, args := []string{"1=1"}, []any{}
	if f.RepoID != 0 {
		where = append(where, "e.repo_id=?")
		args = append(args, f.RepoID)
	}
	if len(f.Kinds) > 0 {
		where = append(where, "e.kind IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.Kinds)), ",")+")")
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}
	if f.Limit == 0 {
		f.Limit = 100
	}
	rows, err := s.db.Query(`SELECT e.id,e.repo_id,e.source_id,e.ts,e.kind,e.ref,e.old,e.new,e.preserved,e.version,e.lost,e.added,e.detail,
		COALESCE(CASE WHEN r.host IN ('github.com','gitlab.com','codeberg.org','git.sr.ht') THEN r.owner||'/'||r.name WHEN r.owner='' THEN r.host||'/'||r.name
			ELSE r.host||'/'||r.owner||'/'||r.name END, s.target, '')
		FROM events e LEFT JOIN repos r ON r.id=e.repo_id LEFT JOIN sources s ON s.id=e.source_id
		WHERE `+strings.Join(where, " AND ")+` ORDER BY e.ts DESC, e.id DESC LIMIT ?`, append(args, f.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.RepoID, &e.SourceID, &e.TS, &e.Kind, &e.Ref, &e.Old, &e.New, &e.Preserved, &e.Version,
			&e.Lost, &e.Added, &e.Detail, &e.RepoName); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- stats

type Stats struct {
	Repos, OK, New, Gone, Errors, Paused, Rewritten, Sources int
	Bytes                                                    int64
	Due                                                      int
	Rewrites30d                                              int
}

func (s *Store) Stats(at int64) (Stats, error) {
	var st Stats
	err := s.db.QueryRow(`SELECT count(*),
		COALESCE(sum(state='ok'),0), COALESCE(sum(state='new'),0), COALESCE(sum(state='gone'),0), COALESCE(sum(state='error'),0),
		COALESCE(sum(paused),0), COALESCE(sum(versions>0),0), COALESCE(sum(size_bytes),0),
		COALESCE(sum(paused=0 AND next_check<=?),0) FROM repos`, at).
		Scan(&st.Repos, &st.OK, &st.New, &st.Gone, &st.Errors, &st.Paused, &st.Rewritten, &st.Bytes, &st.Due)
	if err != nil {
		return st, err
	}
	s.db.QueryRow(`SELECT count(*) FROM sources`).Scan(&st.Sources)
	s.db.QueryRow(`SELECT count(DISTINCT repo_id||version) FROM events WHERE version!='' AND ts>?`, at-30*86400).Scan(&st.Rewrites30d)
	return st, nil
}

// ---------------------------------------------------------------- api cache

func (s *Store) CacheGet(url string) (etag, next string, body []byte, ok bool) {
	err := s.db.QueryRow(`SELECT etag, next, body FROM api_cache WHERE url=?`, url).Scan(&etag, &next, &body)
	return etag, next, body, err == nil
}

func (s *Store) CachePut(url, etag, next string, body []byte) {
	s.db.Exec(`INSERT INTO api_cache(url,etag,next,body,ts) VALUES(?,?,?,?,?)
		ON CONFLICT(url) DO UPDATE SET etag=excluded.etag, next=excluded.next, body=excluded.body, ts=excluded.ts`,
		url, etag, next, body, now())
}

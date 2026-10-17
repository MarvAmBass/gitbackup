-- Runtime state only. Everything configurable lives in gitbackup.yaml;
-- plans and sources are mirrored here (by name / key) so state can refer to them.

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS plans (
  id             INTEGER PRIMARY KEY,
  name           TEXT NOT NULL UNIQUE,
  priority       INTEGER NOT NULL DEFAULT 1,      -- 0 low, 1 normal, 2 high
  discover_every INTEGER NOT NULL DEFAULT 43200,  -- seconds
  check_every    INTEGER NOT NULL DEFAULT 43200,
  check_min      INTEGER NOT NULL DEFAULT 7200,
  check_max      INTEGER NOT NULL DEFAULT 604800,
  adaptive       INTEGER NOT NULL DEFAULT 1,
  releases       TEXT NOT NULL DEFAULT 'off',     -- off | meta | assets
  release_max_mb INTEGER NOT NULL DEFAULT 200,
  releases_every INTEGER NOT NULL DEFAULT 604800,
  wiki           INTEGER NOT NULL DEFAULT 1,
  forks          TEXT NOT NULL DEFAULT 'off',     -- off | on
  fork_min_ahead INTEGER NOT NULL DEFAULT 5,
  fork_max       INTEGER NOT NULL DEFAULT 20,
  stagit         TEXT NOT NULL DEFAULT 'on-change', -- legacy, unused (stagit pre-rendering); kept for existing databases
  stagit_every   INTEGER NOT NULL DEFAULT 0       -- legacy, unused; kept for existing databases
);

CREATE TABLE IF NOT EXISTS sources (
  id            INTEGER PRIMARY KEY,
  platform      TEXT NOT NULL,
  host          TEXT NOT NULL,
  kind          TEXT NOT NULL,                    -- account | repo | auto
  target        TEXT NOT NULL,
  plan_id       INTEGER NOT NULL REFERENCES plans(id),
  with_repos    INTEGER NOT NULL DEFAULT 1,
  with_stars    INTEGER NOT NULL DEFAULT 0,
  with_gists    INTEGER NOT NULL DEFAULT 0,
  with_forks    INTEGER NOT NULL DEFAULT 0,
  enabled       INTEGER NOT NULL DEFAULT 1,
  note          TEXT NOT NULL DEFAULT '',
  account_type  TEXT NOT NULL DEFAULT '',
  last_discover INTEGER NOT NULL DEFAULT 0,
  next_discover INTEGER NOT NULL DEFAULT 0,
  last_error    TEXT NOT NULL DEFAULT '',
  created       INTEGER NOT NULL,
  UNIQUE (platform, host, target)
);

-- One row per backed-up repository. path is its folder below data/
-- (e.g. github.com/pi-hole/repos/pi-hole) and never changes.
CREATE TABLE IF NOT EXISTS repos (
  id             INTEGER PRIMARY KEY,
  kind           TEXT NOT NULL DEFAULT 'repo',    -- repo | fork | gist | snippet
  platform       TEXT NOT NULL DEFAULT 'git',
  host           TEXT NOT NULL,
  owner          TEXT NOT NULL,
  name           TEXT NOT NULL,
  path           TEXT NOT NULL UNIQUE,
  url            TEXT NOT NULL,
  ext_id         TEXT,                            -- platform id, survives renames
  parent_id      INTEGER REFERENCES repos(id),    -- forks of watched repos
  description    TEXT NOT NULL DEFAULT '',
  default_branch TEXT NOT NULL DEFAULT '',
  state          TEXT NOT NULL DEFAULT 'new',     -- new | ok | gone | error
  archived       INTEGER NOT NULL DEFAULT 0,
  paused         INTEGER NOT NULL DEFAULT 0,      -- from config overrides
  plan_id        INTEGER REFERENCES plans(id),    -- effective plan (strongest source)
  priority_override INTEGER,                      -- from config overrides
  interval_override INTEGER,                      -- from config overrides
  interval       INTEGER NOT NULL DEFAULT 0,      -- current adaptive interval
  pushed_at      INTEGER NOT NULL DEFAULT 0,
  last_check     INTEGER NOT NULL DEFAULT 0,
  next_check     INTEGER NOT NULL DEFAULT 0,
  last_change    INTEGER NOT NULL DEFAULT 0,
  last_commit    INTEGER NOT NULL DEFAULT 0,
  last_error     TEXT NOT NULL DEFAULT '',
  versions       INTEGER NOT NULL DEFAULT 0,
  size_bytes     INTEGER NOT NULL DEFAULT 0,
  gone_since     INTEGER NOT NULL DEFAULT 0,      -- no longer available upstream since
  wiki_state     TEXT NOT NULL DEFAULT '',        -- '' unknown | none | ok | gone
  wiki_versions  INTEGER NOT NULL DEFAULT 0,
  wiki_checked   INTEGER NOT NULL DEFAULT 0,
  forced         INTEGER NOT NULL DEFAULT 0,      -- manual "check now": always run git
  ahead          INTEGER NOT NULL DEFAULT 0,      -- forks: own commits vs parent at discovery
  stagit_dirty   INTEGER NOT NULL DEFAULT 0,
  stagit_rebuild INTEGER NOT NULL DEFAULT 0,
  stagit_built   INTEGER NOT NULL DEFAULT 0,
  releases_checked INTEGER NOT NULL DEFAULT 0,
  forks_checked  INTEGER NOT NULL DEFAULT 0,
  created        INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS repos_ext_id ON repos(host, ext_id) WHERE ext_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS repos_due ON repos(next_check);
CREATE INDEX IF NOT EXISTS repos_parent ON repos(parent_id);

-- Further places a repo appears (symlinks to its folder), e.g. a fork found
-- both as the account's own fork and as a fork of a watched repo.
CREATE TABLE IF NOT EXISTS repo_aliases (
  repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  path    TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS source_repos (
  source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  repo_id   INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  role      TEXT NOT NULL,                        -- own | star | single | fork | gist
  seen      INTEGER NOT NULL,
  PRIMARY KEY (source_id, repo_id, role)
);

CREATE TABLE IF NOT EXISTS events (
  id        INTEGER PRIMARY KEY,
  repo_id   INTEGER REFERENCES repos(id) ON DELETE CASCADE,
  source_id INTEGER REFERENCES sources(id) ON DELETE SET NULL,
  ts        INTEGER NOT NULL,
  kind      TEXT NOT NULL,
  ref       TEXT NOT NULL DEFAULT '',
  old       TEXT NOT NULL DEFAULT '',
  new       TEXT NOT NULL DEFAULT '',
  preserved TEXT NOT NULL DEFAULT '',
  version   TEXT NOT NULL DEFAULT '',
  lost      INTEGER NOT NULL DEFAULT 0,
  added     INTEGER NOT NULL DEFAULT 0,
  detail    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS events_repo ON events(repo_id, ts);
CREATE INDEX IF NOT EXISTS events_ts ON events(ts);

CREATE TABLE IF NOT EXISTS api_cache (
  url  TEXT PRIMARY KEY,
  etag TEXT NOT NULL,
  next TEXT NOT NULL DEFAULT '',
  body BLOB NOT NULL,
  ts   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS releases (
  id         INTEGER PRIMARY KEY,
  repo_id    INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  tag        TEXT NOT NULL,
  ext_id     TEXT NOT NULL DEFAULT '',
  name       TEXT NOT NULL DEFAULT '',
  published  INTEGER NOT NULL DEFAULT 0,
  prerelease INTEGER NOT NULL DEFAULT 0,
  meta_hash  TEXT NOT NULL DEFAULT '',
  state      TEXT NOT NULL DEFAULT 'ok',          -- ok | deleted
  first_seen INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL,
  UNIQUE (repo_id, tag)
);

CREATE TABLE IF NOT EXISTS release_assets (
  id         INTEGER PRIMARY KEY,
  release_id INTEGER NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  ext_id     TEXT NOT NULL DEFAULT '',
  url        TEXT NOT NULL,
  size       INTEGER NOT NULL DEFAULT 0,
  identity   TEXT NOT NULL,
  digest     TEXT NOT NULL DEFAULT '',
  sha256     TEXT NOT NULL DEFAULT '',
  path       TEXT NOT NULL DEFAULT '',            -- relative to data/
  state      TEXT NOT NULL,                       -- ok | pending | skipped | failed | replaced | deleted
  note       TEXT NOT NULL DEFAULT '',
  updated    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS release_assets_rel ON release_assets(release_id, name);

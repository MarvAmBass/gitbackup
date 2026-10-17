# gitbackup — design

A self-hosted, history-preserving backup system for git hosting platforms:
GitHub (+ Enterprise), GitLab, Gitea/Forgejo and sourcehut behind one provider
interface, and any plain git URL as a fallback.

## Goals

1. **Never lose history.** Upstream force-pushes, deleted branches, moved tags,
   deleted repos, replaced release assets: the previous state is kept and
   marked as a *version*, never overwritten. (When a project replaces its
   history or code – a force-push, a deleted branch, or a crippled
   "decrypt-only" release like TrueCrypt's in 2014 – a plain mirror follows
   along.)
2. **Watch more than single repos.** Users and orgs, including their repos,
   stars, gists and forks. Single repos and plain git URLs too.
3. **Per-target plans.** Schedules, releases on/off, fork thresholds and
   priority are configurable per plan. Adaptive re-check intervals.
4. **Be gentle with forge APIs.** Respect the advertised limits, back off and
   honor `Retry-After`. Works without a token (GitHub: 60 req/h), slowly but
   correctly; a token raises the limit. Budget and reserve are configurable.
5. **Web UI** to manage everything and to see what's hot, what changed and
   what got rewritten.
6. **Browsable HTML**: history/file/commit pages rendered on demand by
   streagit (github.com/MarvAmBass/streagit, Rust), plus generated index
   and landing pages.

## Core mechanism: change detection and rewrite preservation

All repos are **bare** repositories. The canonical refs are `refs/heads/*`
and `refs/tags/*`, which mirror upstream. The tool never runs `git pull`.

A check runs like this:

1. `git ls-remote --symref <url>`. This costs **no API quota**. If every
   upstream head and tag equals the local one, the check is done.
2. Otherwise, fetch into a staging namespace:
   `+refs/heads/*:refs/gitbackup/incoming/heads/*`, same for tags.
3. Compare each incoming ref with the local one:
   | case                                  | action |
   |---------------------------------------|--------|
   | new ref                               | create |
   | old tip is ancestor of new (ff)       | update |
   | not an ancestor (force-push / rewrite)| **preserve** old tip, then update |
   | tag points elsewhere                  | **preserve** old, then update |
   | ref gone upstream                     | **preserve**, then delete |
4. Preserved refs go to `refs/gitbackup/versions/<unix-ts>/<heads|tags>/<name>`.
   All ref changes of one check run as one atomic `git update-ref --stdin`
   transaction.
5. Each preservation is an **event** in the DB, with old sha, new sha,
   commits lost and commits added. A check with at least one preservation
   creates a new **version** of the repo, and the UI shows a timeline.
6. The repo config makes sure git never throws anything away:
   `gc.pruneExpire=never`, `gc.reflogExpire=never`,
   `gc.reflogExpireUnreachable=never`, `core.logAllRefUpdates=always`.

Repo-level events: `gone` (404, deleted or made private; local copy is kept
and frozen), `renamed` (tracked by GitHub id, not by name), `archived`, and
`back` (reappeared).

Release assets are content-addressed. If an asset with the same name changes
(size, digest or updated_at), the old file stays and the release gets a new
version.

## Filesystem layout

See README → "Folder layout". The principles:

- **One folder per repository** ("slot"): `<host>/<owner…>/<repos|forks|gists|snippets>/<name>/`
  holds `repo.git`, the frozen `repo.vN.git`, `wiki.git`, `releases/`,
  `html*/`, its own `forks/<owner>/`, and an `index.html` landing page.
- **Stored once.** Stars and second routes to the same repo (matched by
  platform id) are symlinks. The first place a repo is stored stays its
  folder forever; folders are never moved.
- **Versions are repositories.** `repo.vN.git` is a bare repo whose refs are
  the complete state before the N-th upstream rewrite. Objects are borrowed
  from `repo.git` via relative alternates. It is cloneable and rendered by
  streagit like any other repo (it never changes).
- **Config, state and data are separate.** `gitbackup.yaml` is the only thing
  a user edits. `state/` is rebuildable bookkeeping. `data/` is browsable
  (`/browse/`) without exposing tokens.

## Discovery and forks (API-frugal)

| source kind        | API calls per discovery                     |
|--------------------|---------------------------------------------|
| github user/org    | `ceil(repos/100)` (+ same for stars, gists) |
| github single repo | 1 (metadata; skipped for plain URL mode)    |
| plain git URL      | 0                                           |

- Every GET uses an **ETag / If-None-Match** cache. A `304` doesn't count
  against the primary rate limit (authenticated).
- The repo list includes `pushed_at`. When a repo hasn't been pushed since the
  last check, the scheduler skips even the `ls-remote`.
- **Fork threshold without per-fork API calls:**
  1. List forks (paginated). Drop forks with `pushed_at <= created_at`: they
     were never pushed to, so they're pure backup forks.
  2. Run `ls-remote` on each remaining fork. If every head sha already exists
     in the parent, it has nothing new; skip it.
  3. Otherwise fetch it into a scratch namespace (via alternates), count
     `git rev-list --count <fork-heads> --not --all`, and keep it only if
     the count is at least `min_ahead`.

## Rate limiting

- One serialized API client **per forge instance** (github.com, gitlab.com, each
  self-hosted server), which also avoids secondary rate limits. It honors
  `Retry-After`, and reads GitHub `X-RateLimit-*`, GitLab `RateLimit-*` and IETF
  draft `RateLimit: r=…;t=…` (Forgejo) headers.
- Per instance: token, `reserve` (default 20% of the limit, never spent by the
  scheduler, kept for manual actions) and a minimum gap between requests.
- When the budget is gone, API work is postponed to the reset time rather than
  blocking. Git transport work keeps going: it doesn't use API quota.
- Tokens are set in the UI or via env (`GITHUB_TOKEN`, `GITLAB_TOKEN`,
  `CODEBERG_TOKEN`, `SRHT_TOKEN`).
- Git transport has its own concurrency limit (default 4).

## Scheduling and priority

- **Plan** (reusable): `discover_every`, `check_every` (base), `check_min`,
  `check_max`, `adaptive`, `priority` (high / normal / low), `releases`
  (off / meta / assets + max asset size), `forks` (off / on + `min_ahead`,
  `max_forks`), `gists`.
- **Source**: kind + target + plan + include flags (repos / stars / gists / forks).
- **Repo**: many-to-many with sources. The effective plan is the "strongest" of
  its sources, and a per-repo override (pin priority / interval / pause) is allowed.
- **Adaptive interval**: a change halves the interval (down to `check_min`),
  no change multiplies it by 1.5 (up to `check_max`). Active repos end up
  checked often, dormant ones rarely.
- The dashboard's **Hot** list sorts by priority, changes per 30 days and
  next check. It answers "what's high prio and visited more often".

## Components

```
cmd/gitbackup          main: runs the server; `gitbackup version` prints the version
internal/config        gitbackup.yaml: load, validate, live reload, save with comments kept
internal/store         SQLite (modernc, pure Go), schema and migrations
internal/gitops        bare init, ls-remote, staged fetch, rewrite detection
internal/api           shared forge HTTP client: rate limits, ETag cache, pagination
internal/provider      GitHub, GitLab, Gitea/Forgejo, sourcehut, plain git
internal/target        user input → platform/instance/path; folder layout
internal/engine        scheduling, discovery, checks, forks, hooks, views, git pacing
internal/releases      release and asset mirroring with versioned files
internal/pages         index and landing pages
internal/streagit      supervises streagit, reverse-proxies html/ pages
internal/auth          session login (+ basic auth for scripts), lockout
internal/server        HTTP/HTTPS listeners, certificates (self-signed, files, ACME)
internal/web           management UI: net/http + html/template (embedded), no JS framework
```

Container: Alpine with `git`, `streagit` built from source (pinned release), a
single static Go binary, `/data` volume, port 8080. A login
(`GITBACKUP_ADMIN_PASSWORD`) is required unless gitbackup listens on loopback
only or `GITBACKUP_NO_AUTH=1` is set.

## Status

Implemented: all platforms above, the slot layout with versions as cloneable
repositories, wikis, releases with asset versioning, forks of watched repos,
gists/snippets, hooks, HTTPS, and on-demand HTML pages via streagit.

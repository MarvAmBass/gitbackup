# gitbackup

History-preserving backups of git hosting platforms, with a web UI.

**If upstream rewrites history, the previous state is kept, not overwritten.**
When a project replaces its history or code – a force-push, a deleted branch,
or a crippled "decrypt-only" release like TrueCrypt's in 2014 – a plain mirror
follows along. gitbackup keeps the previous state as a frozen version
(`repo.v1.git`, browsable and cloneable), marks the event, and shows which
commits upstream dropped. The same applies to wikis and release files.

See [DESIGN.md](DESIGN.md) for the architecture.

## Quick start

Requirements: Docker with Compose. The image build fetches
[streagit](https://github.com/MarvAmBass/streagit) from GitHub.

```sh
git clone https://github.com/MarvAmBass/gitbackup.git && cd gitbackup
echo 'GITBACKUP_ADMIN_PASSWORD=choose-a-long-password' > .env
mkdir -p gitbackup && sudo chown 1000 gitbackup   # the data folder; the container runs as uid 1000
docker compose up -d --build        # UI on http://localhost:8080, login "admin"
```

The compose file publishes the port on `127.0.0.1` only. See
[Security](#security) before you make it reachable from other machines.

## Platforms

| platform | accounts | stars | gists | wiki | releases | forks of watched repos |
|---|---|---|---|---|---|---|
| **GitHub** (+ Enterprise) | users, orgs | ✓ | gists | ✓ | ✓ (sha256-verified) | ✓ |
| **GitLab** (gitlab.com + self-hosted) | users, nested groups | ✓ | snippets¹ | ✓ | ✓ | ✓ |
| **Gitea / Forgejo** (Codeberg + self-hosted) | users, orgs | ✓ | – | ✓ | ✓ | ✓ |
| **sourcehut** | users² | – | – | – | – | – |
| **plain git** (https, ssh, `git://`, `file://`) | – | – | – | – | – | – |

¹ GitLab can only list the snippets of the token's owner. Public snippets are kept.
² Uses the API with a token; without one it reads the public profile pages.

Self-hosted servers are detected automatically. You can also pick the platform
when adding a source.

## Folder layout

Everything lives in one folder (`/data` in the container):

```
gitbackup.yaml                      all configuration (web UI + hand edits)
state/                              database: schedule, events, API cache (rebuildable)
data/                               everything to browse/download – served at /browse/
  index.html
  github.com/
    octocat/
      index.html
      repos/Hello-World/
        index.html                  landing page: history, versions, wiki, releases, forks
        repo.git                    current mirror (bare)
        repo.v1.git  repo.v2.git    frozen states from before each upstream rewrite
        wiki.git     wiki.v1.git
        releases/v1.0/              release.json, files, .versions/ (replaced files)
        html/  html.v1/             history pages per version (rendered on demand, nothing on disk)
        forks/<owner>/              forks with own commits – same layout, objects shared
      forks/<name>/                 the account's own forks
      gists/<id>/                   gists (history-preserved like repos)
      stars/<owner>/<name>  →       symlink to ../../../<owner>/repos/<name>
  gitlab.com/<group>/<sub>/repos/<project>/ …   (+ snippets/)
  git.sr.ht/~user/repos/<name>/ …
  <any git host>/…                  plain git servers
```

- Every repository is stored **once**. Stars, and a fork reachable from two
  places (a followed account's own fork that is also a fork of a watched
  repo), are symlinks.
- Versions and forks borrow their data from the repo they belong to
  (`objects/info/alternates`), so they cost almost no extra disk.
- `state/` is bookkeeping. Git refs (`refs/gitbackup/versions/<stamp>/…` in
  `repo.git`) are the source of truth, and gitbackup rebuilds version folders
  and links from them on every start.

## Configuration: `gitbackup.yaml`

The settings pages write to it, and you can edit it by hand: changes are
picked up within seconds. An invalid file is ignored and shown as an error in
the UI, and gitbackup keeps running with the last good version. Your comments
survive saves from the UI.

```yaml
settings:
  git_workers: 4
  html: true                  # history, file and commit pages (streagit)
instances:
  - host: github.com
    platform: github
    base_url: https://github.com
    token: ${GITHUB_TOKEN}      # literal or ${ENV_VAR}
    reserve: 20                 # % of the API limit only manual actions may use
    gap: 1s
plans:
  - name: default
    priority: normal            # low | normal | high
    discover_every: 12h
    check: {every: 12h, min: 2h, max: 1w, adaptive: true}
    releases: meta              # off | meta | assets
    release_max_mb: 200
    wiki: true
    forks: {enabled: false, min_ahead: 5, max: 20}
sources:
  - target: example-user
    platform: github
    host: github.com
    kind: account               # account | repo | auto (GitLab group/project)
    plan: default
    repos: true
    stars: true
    gists: true
overrides:
  - repo: github.com/pi-hole/repos/pi-hole
    priority: high
    interval: 6h
hooks:
  - name: notify
    events: [history]           # event kinds, "history" or "*"
    run: /data/hooks/notify.sh
```

### Hooks

A hook runs a shell command on matching events: send a message, trigger
another backup, anything. `history` matches every event where upstream
removed or rewrote something (`rewritten`, `tag-moved`, `deleted`, `gone`,
`asset-replaced`, `asset-deleted`, `release-deleted`, `release-edited`). The
command gets `GB_EVENT`, `GB_REPO`, `GB_URL`, `GB_PATH`, `GB_REF`, `GB_OLD`,
`GB_NEW`, `GB_VERSION`, `GB_PRESERVED`, `GB_LOST` and `GB_DETAIL`, plus the
event as JSON on stdin.

## What a check does

1. `git ls-remote` (no API quota). If nothing moved, the check is done.
2. Otherwise it fetches into a staging area and compares every ref. On a
   force-push, a moved tag, or a deleted branch whose commits exist nowhere
   else upstream, the **complete** previous state is kept as a new
   `repo.vN.git`. A branch deleted after being merged loses nothing, so it
   only gets an event (`removed`), not a version.
3. The wiki gets the same treatment, if the plan says so.
4. Releases are synced right away when tags change, otherwise on a slow
   sweep (`releases_every`, default 1 week). Each sync is 1 API call,
   ETag-cached. A replaced file is kept in `.versions/`.
5. Forks are scanned once per discovery interval, if the plan has forks on.
   It reads 3 API pages, newest first, and only probes forks pushed to after
   forking. A fork is kept only if it has ≥ `min_ahead` commits the original
   doesn't have.
6. **Polite to the API.** gitbackup reads the rate-limit headers of every
   response, backs off when the budget runs low, honors `Retry-After`, and
   sends an identifying User-Agent. Optional API work (releases, fork scans,
   repo metadata) is paced: it stops 10% above the reserve so discovery
   always has budget, and spreads the rest evenly until the limit resets. It
   works without a token, slowly but correctly: GitHub allows 60 anonymous
   requests per hour. A token is recommended for more than a few hundred
   repos. For sourcehut a token is recommended too, since without one
   gitbackup reads the public HTML pages.
7. **Git traffic is paced per host as well.** Forges throttle anonymous git
   too: GitHub then answers HTTP 401, exactly like for a deleted repo, and
   doesn't publish the limit. gitbackup learns the pace. Each throttle
   quadruples the gap between git requests to that host and pauses the host
   (15 min, doubling up to 2 h); its repos are postponed, not marked as
   errors. Long success streaks shorten the gap again, never below
   `git_gap` (default 1s). The dashboard shows the learned pace.
8. A repo is only tagged **"no longer on GitHub · since …"** when git is
   refused *and* its web page returns 404. It stays in the backup with full
   history.
9. The interval adapts. A change halves it and a quiet check stretches it by
   1.5×, within the plan's min/max. Overrides pin priority, interval or pause
   per repo.

## HTML pages: streagit

History, files and commit pages are rendered **on demand** by
[streagit](https://github.com/MarvAmBass/streagit), a stagit-compatible
renderer in Rust. gitbackup starts it as a child process on
`127.0.0.1:8081` and forwards `…/html/…` and `…/html.vN/…` below `/browse/`
to it, behind the same login. Nothing is pre-generated, so a history rewrite
or a new version costs nothing until somebody looks at it. Pages are cached
by git object id and revalidated with ETags.

The image builds streagit from GitHub at a pinned release (`STREAGIT_REF`
build argument in the Dockerfile). Pre-rendering every page with stagit, as
earlier versions did, produced terabytes of HTML on large mirrors.

## Run

```sh
docker compose up -d --build        # UI on http://localhost:8080
```

| env | default | |
|---|---|---|
| `GITBACKUP_HOME` | `/data` | the folder above |
| `GITBACKUP_ADMIN_PASSWORD` | – | **required**: the login password (see below) |
| `GITBACKUP_ADMIN_USER` | `admin` | the login user name |
| `GITBACKUP_NO_AUTH` | – | `1` runs without a login, e.g. behind an authenticating reverse proxy |
| `GITBACKUP_LISTEN` | `:8080` | |
| `GITBACKUP_DB_JOURNAL` | `wal` | `delete` if `state/` must live on NFS |
| `GITBACKUP_BROWSE_URL` | – | absolute URL of `/browse/` for atom feeds |
| `GITBACKUP_STREAGIT` | `streagit` | path of the streagit binary |
| `GITBACKUP_STREAGIT_LISTEN` | `127.0.0.1:8081` | where streagit listens (internal) |
| `GITBACKUP_ASSETS` | `/usr/local/share/gitbackup/assets` | logo/favicon/style for the index pages |
| any | – | referenced from `gitbackup.yaml` as `${NAME}` (tokens) |

Never open `state/gitbackup.db` with another process while gitbackup runs.

### Login and HTTPS

gitbackup refuses to start without `GITBACKUP_ADMIN_PASSWORD`, unless it
listens on a loopback address only (`GITBACKUP_LISTEN=127.0.0.1:8080`,
`[::1]:8080` or `localhost:8080`) or `GITBACKUP_NO_AUTH=1` is set. In both
cases it logs a warning. Note that inside a container a loopback address is
the container's own, so the published port can't reach it.

The UI shows a login page that works with password managers. Sessions last 30 days, and cookies are HttpOnly, SameSite and
Secure over HTTPS. After 10 wrong attempts a client is locked out for 15
minutes. Scripts can keep using basic auth (`curl -u admin:… …/api/repos.tsv`).

HTTPS is **off** by default. Turn it on under Settings, or in `gitbackup.yaml`:

```yaml
settings:
  https:
    mode: self-signed        # off | self-signed | files | letsencrypt
    port: ""                 # "" = same port as HTTP; or e.g. ":8443" (publish it in docker-compose)
    redirect_http: true
    hostnames: [gitbackup.lan, 192.0.2.10]        # self-signed: names in the certificate
    # cert_file: /data/tls/fullchain.pem          # files: reloaded when they change
    # key_file: /data/tls/privkey.pem
    # domains: [git.example.org]                  # letsencrypt: public names …
    # email: you@example.org                      # … reachable on 443 (or 80) from the internet
```

- **Same port:** each connection is recognised as TLS or plain HTTP, so
  `http://gitbackup.example.org:8080` and `https://gitbackup.example.org:8080`
  both work. Use `redirect_http` to force HTTPS.
- **Certificates:** self-signed certificates are created in `state/tls/`
  and renewed automatically. Let's Encrypt certificates are kept in
  `state/acme/`.
- **Live and safe:** changes apply without a restart, and if a certificate
  can't be loaded, plain HTTP keeps working and Settings shows the error.

## Security

- **Login required.** The UI and everything below it need the admin login
  (see [Login and HTTPS](#login-and-https)); running without one is only
  possible on loopback or with an explicit `GITBACKUP_NO_AUTH=1`.
- **Hooks run shell commands** as the container user. Anyone with access to
  the UI can add a hook, so anyone with UI access can run commands in the
  container and read everything in `/data`.
- **Tokens** are stored in `gitbackup.yaml` under `/data` (or referenced as
  `${ENV_VAR}`). `/browse/` only serves `data/`, never the config or
  `state/`.
- **`/browse/`** serves all backed-up data, including private repositories
  your tokens can read, behind the same login.
- **Beyond localhost**, turn on HTTPS (Settings) or put gitbackup behind a
  reverse proxy that terminates TLS. Otherwise passwords and session cookies
  travel in plain text.
- **`safe.directory '*'`** is set in the image's system git config, because
  the `/data` volume may be owned by another uid than the container user. It
  only applies to git inside the container.

## Backup & upgrade

Back up the whole `/data` folder. `state/` contains the SQLite database
(`state/gitbackup.db`, plus `-wal`/`-shm` files): stop the container first,
or copy it with `sqlite3 state/gitbackup.db ".backup copy.db"`. The database
can be rebuilt from the git refs, but schedules, events and the API cache
would start over.

To upgrade, pull the new version and run `docker compose up -d --build`.
Database migrations run on start.

## Development

```sh
docker run --rm -v "$PWD:/src" -v gomod-cache:/go/pkg/mod -w /src golang:1.27-alpine \
  sh -c 'apk add --no-cache git && go vet ./... && go test ./...'
```

After changing dependencies, regenerate `THIRD_PARTY_NOTICES`:

```sh
docker run --rm -v "$PWD:/src" -v gomod-cache:/go/pkg/mod -w /src golang:1.27-alpine sh contrib/gen-notices.sh
```

`internal/web/web_test.go` runs a full cycle against a fake Forgejo API. It
covers configuration through the UI into `gitbackup.yaml`, discovery, clone,
wiki, the fork scan (kept vs. skipped), the alias symlink, a force-push with
`repo.v1.git`, a swapped release file, hooks, and reconcile.

## License

MIT, see [LICENSE](LICENSE). Parts of `internal/pages/style.css` are derived
from [stagit](https://codemadness.org/stagit.html) (MIT/X), see
[LICENSE-stagit](LICENSE-stagit). The licenses of the Go modules linked into
the binary are in [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES). The Docker
image ships all of these, plus streagit's license files, in
`/usr/share/licenses/`. The runtime image is based on Alpine Linux and
includes GPL-licensed tools such as git and busybox, which gitbackup runs as
separate programs.

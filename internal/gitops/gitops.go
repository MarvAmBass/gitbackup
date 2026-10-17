// Package gitops manages bare mirror repositories without ever losing history.
//
// Upstream refs live in refs/heads/* and refs/tags/*. When an upstream ref is
// rewritten (force-push), moved (tag) or deleted, the previous tip is kept under
// refs/gitbackup/versions/<stamp>/... before the canonical ref follows upstream.
package gitops

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	incomingPrefix = "refs/gitbackup/incoming/"
	VersionsPrefix = "refs/gitbackup/versions/"
)

// ErrGone means the server said the repository does not exist.
var ErrGone = errors.New("remote repository not found")

// ErrDenied means the server refused anonymous access (HTTP 401/403). Forges
// answer that for missing or private repos *and* when they throttle
// anonymous git traffic – the caller has to find out which (see engine).
var ErrDenied = errors.New("access denied by remote")

// Git binary, overridable for tests.
var Git = "git"

func run(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	// safe.directory: the data volume is often owned by another uid (NFS,
	// bind mounts); gitbackup only ever runs git on directories it manages.
	full := append([]string{"-c", "credential.helper=", "-c", "core.askPass=", "-c", "safe.directory=*"}, args...)
	cmd := exec.CommandContext(ctx, Git, full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=true",
		"SSH_ASKPASS=true",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new",
		"LC_ALL=C",
	)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		switch {
		case isDenied(msg):
			return nil, fmt.Errorf("%w: %s", ErrDenied, firstLine(msg))
		case isGone(msg):
			return nil, fmt.Errorf("%w: %s", ErrGone, firstLine(msg))
		}
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, msg)
	}
	return stdout.Bytes(), nil
}

func isDenied(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, m := range []string{
		"could not read username", // HTTP 401 without credentials
		"http 401", "error: 401", "returned error: 401",
		"http 403", "returned error: 403",
		"access denied",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func isGone(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, m := range []string{
		"repository not found",
		"access to this repository has been disabled", // e.g. DMCA takedown by GitHub staff
		"does not appear to be a git repository",
		"not found",
		"http 404", "error: 404", "returned error: 404",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Repo is a bare repository on disk.
type Repo struct{ Path string }

// Init creates the bare repo (if missing) and enforces the never-prune config.
func Init(ctx context.Context, path string) (*Repo, error) {
	if _, err := os.Stat(filepath.Join(path, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, err
		}
		if _, err := run(ctx, path, nil, "init", "--bare", "--quiet", "."); err != nil {
			return nil, err
		}
	}
	r := &Repo{Path: path}
	cleanStaleLocks(path)
	for _, kv := range [][2]string{
		{"gc.auto", "0"},
		{"gc.pruneExpire", "never"},
		{"gc.reflogExpire", "never"},
		{"gc.reflogExpireUnreachable", "never"},
		{"core.logAllRefUpdates", "always"},
		{"fetch.writeCommitGraph", "true"},
	} {
		if _, err := run(ctx, path, nil, "config", kv[0], kv[1]); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// SetMeta writes the files streagit reads (description, owner, url).
func (r *Repo) SetMeta(description, owner, url string) error {
	for name, v := range map[string]string{"description": description, "owner": owner, "url": url} {
		if err := os.WriteFile(filepath.Join(r.Path, name), []byte(strings.TrimSpace(v)+"\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// RemoteState is what ls-remote reports.
type RemoteState struct {
	Head string            // symbolic target of HEAD, e.g. refs/heads/main
	Refs map[string]string // refs/heads/* and refs/tags/* → sha
}

// LsRemote lists heads and tags of url. It uses git transport only (no API quota).
func LsRemote(ctx context.Context, url string) (RemoteState, error) {
	out, err := run(ctx, "", nil, "ls-remote", "--symref", url, "HEAD", "refs/heads/*", "refs/tags/*")
	if err != nil {
		return RemoteState{}, err
	}
	st := RemoteState{Refs: map[string]string{}}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "ref: ") {
			f := strings.Fields(strings.TrimPrefix(line, "ref: "))
			if len(f) == 2 && f[1] == "HEAD" {
				st.Head = f[0]
			}
			continue
		}
		sha, ref, ok := strings.Cut(line, "\t")
		if !ok || strings.HasSuffix(ref, "^{}") {
			continue
		}
		if strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/") {
			st.Refs[ref] = sha
		}
	}
	return st, sc.Err()
}

// Refs returns refs under the given prefixes (default: heads and tags).
func (r *Repo) Refs(ctx context.Context, prefixes ...string) (map[string]string, error) {
	if len(prefixes) == 0 {
		prefixes = []string{"refs/heads/", "refs/tags/"}
	}
	args := append([]string{"for-each-ref", "--format=%(objectname) %(refname)"}, prefixes...)
	out, err := run(ctx, r.Path, nil, args...)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if sha, ref, ok := strings.Cut(line, " "); ok {
			m[ref] = sha
		}
	}
	return m, nil
}

// InSync reports whether the local canonical refs equal the remote ones.
func InSync(local, remote map[string]string) bool {
	if len(local) != len(remote) {
		return false
	}
	for k, v := range remote {
		if local[k] != v {
			return false
		}
	}
	return true
}

// ChangeKind classifies a ref change.
type ChangeKind string

const (
	Created   ChangeKind = "created"
	Updated   ChangeKind = "updated"   // fast-forward
	Rewritten ChangeKind = "rewritten" // non-fast-forward branch update
	TagMoved  ChangeKind = "tag-moved"
	Deleted   ChangeKind = "deleted" // branch/tag gone and commits exist nowhere else upstream
	Removed   ChangeKind = "removed" // branch gone, but every commit is still upstream (e.g. merged PR branch)
)

// Destructive reports whether the change would have lost data without preservation.
func (k ChangeKind) Destructive() bool { return k == Rewritten || k == TagMoved || k == Deleted }

// RefChange is one ref updated by Sync.
type RefChange struct {
	Ref       string
	Kind      ChangeKind
	Old, New  string
	Preserved string // version ref holding Old, if Kind.Destructive()
	Lost      int    // commits reachable from Old but not from New (rewrite/delete)
	Added     int    // commits reachable from New but not from Old
}

// SyncResult summarises a Sync call.
type SyncResult struct {
	Fetched bool
	Changes []RefChange
	Version string // version stamp when anything was preserved
	Head    string
}

// Destructive reports whether any change in the result was preserved.
func (s SyncResult) Destructive() bool { return s.Version != "" }

// Sync brings the repo up to date with url. remote may be passed in when the
// caller already ran LsRemote; otherwise it is fetched.
func (r *Repo) Sync(ctx context.Context, url string, remote *RemoteState, now time.Time) (SyncResult, error) {
	var res SyncResult
	if remote == nil {
		st, err := LsRemote(ctx, url)
		if err != nil {
			return res, err
		}
		remote = &st
	}
	res.Head = remote.Head
	local, err := r.Refs(ctx)
	if err != nil {
		return res, err
	}
	if InSync(local, remote.Refs) {
		return res, r.setHead(ctx, remote.Head)
	}

	if err := r.clearIncoming(ctx); err != nil {
		return res, err
	}
	if _, err := run(ctx, r.Path, nil, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-auto-gc", url,
		"+refs/heads/*:"+incomingPrefix+"heads/*",
		"+refs/tags/*:"+incomingPrefix+"tags/*"); err != nil {
		return res, err
	}
	res.Fetched = true
	incoming, err := r.Refs(ctx, incomingPrefix)
	if err != nil {
		return res, err
	}
	upstream := map[string]string{}
	for ref, sha := range incoming {
		upstream["refs/"+strings.TrimPrefix(ref, incomingPrefix)] = sha
	}

	stamp := r.versionStamp(ctx, now)
	var tx, creates strings.Builder
	names := make([]string, 0, len(upstream)+len(local))
	seen := map[string]bool{}
	for ref := range upstream {
		names, seen[ref] = append(names, ref), true
	}
	for ref := range local {
		if !seen[ref] {
			names = append(names, ref)
		}
	}
	sort.Strings(names)
	upstreamTips := make([]string, 0, len(upstream))
	for _, sha := range upstream {
		upstreamTips = append(upstreamTips, sha)
	}
	// Branches deleted upstream lose nothing when every commit is still
	// reachable from an upstream ref (a merged and deleted feature branch).
	var deletedHeads []string
	for ref, sha := range local {
		if _, has := upstream[ref]; !has && strings.HasPrefix(ref, "refs/heads/") {
			deletedHeads = append(deletedHeads, sha)
		}
	}
	lost := r.lostPerTip(ctx, deletedHeads, upstreamTips)

	for _, ref := range names {
		old, had := local[ref]
		nw, has := upstream[ref]
		c := RefChange{Ref: ref, Old: old, New: nw}
		switch {
		case had && has && old == nw:
			continue
		case !had:
			c.Kind = Created
			fmt.Fprintf(&creates, "create %s %s\n", ref, nw)
		case !has:
			c.Kind = Deleted
			if n, counted := lost[old]; counted && n == 0 {
				c.Kind = Removed // still upstream: no version
			} else if counted {
				c.Lost = max(n, 0) // -1: git could not tell – preserved anyway
			}
			fmt.Fprintf(&tx, "delete %s %s\n", ref, old)
		case strings.HasPrefix(ref, "refs/tags/"):
			c.Kind = TagMoved
			fmt.Fprintf(&tx, "update %s %s %s\n", ref, nw, old)
		default:
			ff, err := r.isAncestor(ctx, old, nw)
			if err != nil {
				return res, err
			}
			if ff {
				c.Kind = Updated
			} else {
				c.Kind = Rewritten
			}
			fmt.Fprintf(&tx, "update %s %s %s\n", ref, nw, old)
		}
		if c.Kind.Destructive() {
			c.Preserved = VersionsPrefix + stamp + "/" + strings.TrimPrefix(ref, "refs/")
			fmt.Fprintf(&tx, "create %s %s\n", c.Preserved, old)
			res.Version = stamp
		}
		if c.Kind != Deleted && c.Kind != Removed {
			c.Lost, c.Added = r.counts(ctx, ref, c)
		}
		res.Changes = append(res.Changes, c)
	}

	// A version is the complete previous state: also keep the refs that did
	// not change, so repo.vN.git is the whole repository as it was.
	if res.Version != "" {
		kept := map[string]bool{}
		for _, c := range res.Changes {
			kept[c.Ref] = c.Preserved != ""
		}
		for ref, sha := range local {
			if !kept[ref] {
				fmt.Fprintf(&tx, "create %s%s/%s %s\n", VersionsPrefix, stamp, strings.TrimPrefix(ref, "refs/"), sha)
			}
		}
	}
	// Two atomic transactions: first preserve/update/delete, then create.
	// Upstream may delete "ci" and create "ci/x" at once; git cannot do both
	// in one transaction (directory/file conflict).
	for _, t := range []string{tx.String(), creates.String()} {
		if t == "" {
			continue
		}
		if _, err := run(ctx, r.Path, []byte(t), "update-ref", "--stdin"); err != nil {
			return res, err
		}
	}
	if err := r.clearIncoming(ctx); err != nil {
		return res, err
	}
	// update-ref writes one file per ref; repos with thousands of tags (times
	// every preserved version) would turn into tens of thousands of tiny files
	if err := r.PackRefs(ctx); err != nil {
		return res, err
	}
	return res, r.setHead(ctx, remote.Head)
}

func (r *Repo) counts(ctx context.Context, ref string, c RefChange) (lost, added int) {
	if !strings.HasPrefix(ref, "refs/heads/") {
		return 0, 0
	}
	switch c.Kind {
	case Created:
		return 0, r.revCount(ctx, c.New, "--not", "--branches")
	default:
		return r.revCount(ctx, c.Old, "^"+c.New), r.revCount(ctx, c.New, "^"+c.Old)
	}
}

func (r *Repo) revCount(ctx context.Context, args ...string) int {
	out, err := run(ctx, r.Path, nil, append([]string{"rev-list", "--count"}, args...)...)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func (r *Repo) isAncestor(ctx context.Context, a, b string) (bool, error) {
	cmd := exec.CommandContext(ctx, Git, "-c", "safe.directory=*", "merge-base", "--is-ancestor", a, b)
	cmd.Dir = r.Path
	// Exit 1 means "not an ancestor"; any other failure (e.g. old tip is not a
	// commit) is treated the same way so the old tip gets preserved.
	return cmd.Run() == nil, nil
}

func (r *Repo) clearIncoming(ctx context.Context) error {
	refs, err := r.Refs(ctx, incomingPrefix)
	if err != nil || len(refs) == 0 {
		return err
	}
	var tx strings.Builder
	for ref := range refs {
		fmt.Fprintf(&tx, "delete %s\n", ref)
	}
	_, err = run(ctx, r.Path, []byte(tx.String()), "update-ref", "--stdin")
	return err
}

func (r *Repo) setHead(ctx context.Context, head string) error {
	if head == "" {
		return nil
	}
	if _, err := run(ctx, r.Path, nil, "show-ref", "--verify", "--quiet", head); err != nil {
		return nil // target ref missing (empty repo) – leave HEAD alone
	}
	_, err := run(ctx, r.Path, nil, "symbolic-ref", "HEAD", head)
	return err
}

// versionStamp returns a sortable, unique stamp like 20260928T101500Z.
func (r *Repo) versionStamp(ctx context.Context, now time.Time) string {
	base := now.UTC().Format("20060102T150405Z")
	stamp := base
	for i := 1; ; i++ {
		refs, _ := r.Refs(ctx, VersionsPrefix+stamp+"/")
		if len(refs) == 0 {
			return stamp
		}
		stamp = fmt.Sprintf("%s-%d", base, i)
	}
}

// Version is a preserved snapshot.
type Version struct {
	Stamp string
	Refs  map[string]string // heads/x, tags/y → sha
}

// Versions lists preserved snapshots, oldest first.
func (r *Repo) Versions(ctx context.Context) ([]Version, error) {
	refs, err := r.Refs(ctx, VersionsPrefix)
	if err != nil {
		return nil, err
	}
	by := map[string]*Version{}
	for ref, sha := range refs {
		rest := strings.TrimPrefix(ref, VersionsPrefix)
		stamp, name, _ := strings.Cut(rest, "/")
		v := by[stamp]
		if v == nil {
			v = &Version{Stamp: stamp, Refs: map[string]string{}}
			by[stamp] = v
		}
		v.Refs[name] = sha
	}
	out := make([]Version, 0, len(by))
	for _, v := range by {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stamp < out[j].Stamp })
	return out, nil
}

// LastCommit returns the committer time of HEAD (zero if empty).
func (r *Repo) LastCommit(ctx context.Context) time.Time {
	out, err := run(ctx, r.Path, nil, "log", "-1", "--format=%ct", "HEAD")
	if err != nil {
		return time.Time{}
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return time.Unix(n, 0)
}

// Log returns up to n commits for the given rev arguments
// (e.g. "<sha>", "--not", "--branches", "--tags").
func (r *Repo) Log(ctx context.Context, n int, revs ...string) ([]Commit, error) {
	args := append([]string{"log", "--format=%H%x1f%s%x1f%an%x1f%ct", "-n", strconv.Itoa(n)}, revs...)
	out, err := run(ctx, r.Path, nil, append(args, "--")...)
	if err != nil {
		return nil, err
	}
	var cs []Commit
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\x1f")
		if len(f) != 4 {
			continue
		}
		ts, _ := strconv.ParseInt(f[3], 10, 64)
		cs = append(cs, Commit{SHA: f[0], Subject: f[1], Author: f[2], Time: time.Unix(ts, 0)})
	}
	return cs, nil
}

// Commit is a log entry.
type Commit struct {
	SHA, Subject, Author string
	Time                 time.Time
}

// DiskUsage returns the size of the repo in bytes.
func DiskUsage(path string) int64 {
	var n int64
	_ = filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// ---------------------------------------------------------------- versions as repos

// MaterializeVersion creates dir as a frozen bare repository holding version
// v of r. It borrows all objects from r (objects/info/alternates), so it costs
// almost no disk; r never prunes, so the objects stay available. An existing
// dir is left untouched: versions never change.
func (r *Repo) MaterializeVersion(ctx context.Context, dir string, v Version, description string) error {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return nil
	}
	tmp := dir + ".tmp"
	os.RemoveAll(tmp)
	if _, err := run(ctx, "", nil, "init", "--bare", "--quiet", tmp); err != nil {
		return err
	}
	vr := &Repo{Path: tmp}
	if err := vr.SetAlternates(r.Path, dir); err != nil {
		return err
	}
	var tx strings.Builder
	head := ""
	for name, sha := range v.Refs {
		fmt.Fprintf(&tx, "create refs/%s %s\n", name, sha)
		if strings.HasPrefix(name, "heads/") && (head == "" || name == "heads/main" || name == "heads/master" && head != "heads/main") {
			head = name
		}
	}
	if tx.Len() > 0 {
		if _, err := run(ctx, tmp, []byte(tx.String()), "update-ref", "--stdin"); err != nil {
			return err
		}
	}
	if head != "" {
		run(ctx, tmp, nil, "symbolic-ref", "HEAD", "refs/"+head)
	}
	(&Repo{Path: tmp}).PackRefs(ctx)
	for k, val := range map[string]string{"gc.auto": "0", "gc.pruneExpire": "never", "receive.denyNonFastForwards": "true"} {
		run(ctx, tmp, nil, "config", k, val)
	}
	os.WriteFile(filepath.Join(tmp, "description"), []byte(description+"\n"), 0o644)
	return os.Rename(tmp, dir)
}

// SetAlternates lets r borrow objects from other. The path is stored
// relative (to where r will finally live, finalPath), so the whole data tree
// can be moved or mounted elsewhere.
func (r *Repo) SetAlternates(other, finalPath string) error {
	if finalPath == "" {
		finalPath = r.Path
	}
	rel, err := filepath.Rel(filepath.Join(finalPath, "objects"), filepath.Join(other, "objects"))
	if err != nil {
		return err
	}
	f := filepath.Join(r.Path, "objects", "info", "alternates")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		return err
	}
	return os.WriteFile(f, []byte(rel+"\n"), 0o644)
}

// ---------------------------------------------------------------- fork scanning

// Missing returns the shas r does not have.
func (r *Repo) Missing(ctx context.Context, shas []string) []string {
	if len(shas) == 0 {
		return nil
	}
	out, err := run(ctx, r.Path, []byte(strings.Join(shas, "\n")+"\n"), "cat-file", "--batch-check")
	if err != nil {
		return shas
	}
	var missing []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasSuffix(line, " missing") {
			missing = append(missing, strings.TrimSuffix(line, " missing"))
		}
	}
	return missing
}

// FetchHeads fetches url's branches into refs/<prefix>/* (no tags) and
// returns the fetched tips.
func (r *Repo) FetchHeads(ctx context.Context, url, prefix string) ([]string, error) {
	if _, err := run(ctx, r.Path, nil, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-auto-gc", url,
		"+refs/heads/*:refs/"+prefix+"/*"); err != nil {
		return nil, err
	}
	refs, err := r.Refs(ctx, "refs/"+prefix+"/")
	if err != nil {
		return nil, err
	}
	var tips []string
	for _, sha := range refs {
		tips = append(tips, sha)
	}
	return tips, nil
}

// CountNew counts commits reachable from tips but not from exclude (0 when
// git fails: a fork that cannot be counted is not worth keeping).
func (r *Repo) CountNew(ctx context.Context, tips, exclude []string) int {
	n, _ := r.countNew(ctx, tips, exclude)
	return n
}

func (r *Repo) countNew(ctx context.Context, tips, exclude []string) (int, error) {
	var in strings.Builder
	for _, t := range tips {
		in.WriteString(t + "\n")
	}
	for _, x := range exclude {
		in.WriteString("^" + x + "\n") // rev-list peels annotated tags itself
	}
	out, err := run(ctx, r.Path, []byte(in.String()), "rev-list", "--count", "--stdin")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// lostPerTip counts, per tip, the commits that none of the upstream refs
// reach any more. One walk covers all tips first – deleted branches are
// usually merged ones, and a repo may have thousands of tags to exclude –
// and only when something really is gone are the tips counted one by one.
// A count git cannot deliver is -1: the caller must treat the tip as lost.
func (r *Repo) lostPerTip(ctx context.Context, tips, upstream []string) map[string]int {
	out := make(map[string]int, len(tips))
	if len(tips) == 0 {
		return out
	}
	if n, err := r.countNew(ctx, tips, upstream); err == nil && n == 0 {
		for _, t := range tips {
			out[t] = 0
		}
		return out
	}
	for _, t := range tips {
		if _, done := out[t]; done {
			continue
		}
		n, err := r.countNew(ctx, []string{t}, upstream)
		if err != nil {
			n = -1
		}
		out[t] = n
	}
	return out
}

// cleanStaleLocks removes *.lock files left behind by a git process that was
// killed (e.g. a container restart during a fetch). gitbackup never runs two
// operations on the same repository, so a lock older than a few minutes is stale.
func cleanStaleLocks(path string) {
	cutoff := time.Now().Add(-10 * time.Minute)
	filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// objects/ holds the data – only its small info/ folder (commit-graph) has locks
		if d.IsDir() && filepath.Base(filepath.Dir(p)) == "objects" && d.Name() != "info" {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".lock") {
			if fi, err := d.Info(); err == nil && fi.ModTime().Before(cutoff) {
				os.Remove(p)
			}
		}
		return nil
	})
}

// PackRefs moves all refs into the single packed-refs file.
func (r *Repo) PackRefs(ctx context.Context) error {
	_, err := run(ctx, r.Path, nil, "pack-refs", "--all", "--prune")
	return err
}

// LooseRefs counts ref files (cheap: stops counting at limit).
func (r *Repo) LooseRefs(limit int) int {
	n := 0
	filepath.WalkDir(filepath.Join(r.Path, "refs"), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
			if n >= limit {
				return filepath.SkipAll
			}
		}
		return nil
	})
	return n
}

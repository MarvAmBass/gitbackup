package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte(msg), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, dir, "add", "f")
	sh(t, dir, "commit", "-q", "-m", msg)
	return sh(t, dir, "rev-parse", "HEAD")
}

func change(res SyncResult, ref string) *RefChange {
	for i := range res.Changes {
		if res.Changes[i].Ref == ref {
			return &res.Changes[i]
		}
	}
	return nil
}

func TestSyncPreservesRewrites(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	up := filepath.Join(tmp, "upstream")
	os.MkdirAll(up, 0o755)
	sh(t, up, "init", "-q", "-b", "main")
	c1 := commit(t, up, "one")
	c2 := commit(t, up, "two: full crypto")
	c3 := commit(t, up, "three: more crypto")
	sh(t, up, "tag", "v1")
	sh(t, up, "branch", "feature")

	r, err := Init(ctx, filepath.Join(tmp, "mirror.git"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

	// 1. initial sync
	res, err := r.Sync(ctx, up, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Fetched || res.Destructive() || len(res.Changes) != 3 {
		t.Fatalf("initial: %+v", res)
	}
	if res.Head != "refs/heads/main" {
		t.Fatalf("head %q", res.Head)
	}

	// 2. no-op sync does not fetch
	res, err = r.Sync(ctx, up, nil, now)
	if err != nil || res.Fetched || len(res.Changes) != 0 {
		t.Fatalf("noop: %+v %v", res, err)
	}

	// 3. fast-forward
	c4 := commit(t, up, "four")
	res, _ = r.Sync(ctx, up, nil, now)
	if c := change(res, "refs/heads/main"); c == nil || c.Kind != Updated || c.Added != 1 || res.Destructive() {
		t.Fatalf("ff: %+v", res)
	}

	// 4. a rewrite: history replaced by a crippled "decrypt only" commit,
	//    tag moved, feature branch deleted.
	sh(t, up, "reset", "-q", "--hard", c1)
	c5 := commit(t, up, "decrypt only")
	sh(t, up, "tag", "-f", "v1")
	sh(t, up, "branch", "-D", "feature")
	later := now.Add(time.Hour)
	res, err = r.Sync(ctx, up, nil, later)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Destructive() || res.Version != "20260928T110000Z" {
		t.Fatalf("rewrite not detected: %+v", res)
	}
	m := change(res, "refs/heads/main")
	if m.Kind != Rewritten || m.Old != c4 || m.New != c5 || m.Lost != 3 || m.Added != 1 {
		t.Fatalf("main: %+v", m)
	}
	if tg := change(res, "refs/tags/v1"); tg.Kind != TagMoved || tg.Old != c3 {
		t.Fatalf("tag: %+v", tg)
	}
	if f := change(res, "refs/heads/feature"); f.Kind != Deleted || f.Old != c3 {
		t.Fatalf("feature: %+v", f)
	}

	// canonical refs mirror upstream …
	refs, _ := r.Refs(ctx)
	if refs["refs/heads/main"] != c5 || refs["refs/heads/feature"] != "" || refs["refs/tags/v1"] != c5 {
		t.Fatalf("canonical refs: %v", refs)
	}
	// … and the old state is still fully reachable.
	vs, _ := r.Versions(ctx)
	if len(vs) != 1 || vs[0].Refs["heads/main"] != c4 || vs[0].Refs["tags/v1"] != c3 || vs[0].Refs["heads/feature"] != c3 {
		t.Fatalf("versions: %+v", vs)
	}
	lost, _ := r.Log(ctx, 10, vs[0].Refs["heads/main"], "--not", "--branches", "--tags")
	if len(lost) != 3 || lost[2].SHA != c2 {
		t.Fatalf("lost commits: %+v", lost)
	}
	if inc, _ := r.Refs(ctx, incomingPrefix); len(inc) != 0 {
		t.Fatalf("incoming not cleaned: %v", inc)
	}

	// 5. a second rewrite in the same second gets its own version
	sh(t, up, "commit", "-q", "--amend", "-m", "decrypt only (amended)")
	res, _ = r.Sync(ctx, up, nil, later)
	if res.Version != "20260928T110000Z-1" {
		t.Fatalf("second version stamp %q", res.Version)
	}

	// 6. gone upstream
	os.RemoveAll(up)
	if _, err := r.Sync(ctx, up, nil, later); !errors.Is(err, ErrGone) {
		t.Fatalf("expected ErrGone, got %v", err)
	}
}

func TestVersionRepoAndForkHelpers(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	up := filepath.Join(tmp, "up")
	os.MkdirAll(up, 0o755)
	sh(t, up, "init", "-q", "-b", "main")
	commit(t, up, "one")
	commit(t, up, "two")
	sh(t, up, "branch", "stable") // untouched by the rewrite below
	r, _ := Init(ctx, filepath.Join(tmp, "slot", "repo.git"))
	r.Sync(ctx, up, nil, time.Now())
	sh(t, up, "reset", "-q", "--hard", "HEAD~1")
	commit(t, up, "two (rewritten)")
	res, _ := r.Sync(ctx, up, nil, time.Now())
	vs, _ := r.Versions(ctx)
	if !res.Destructive() || len(vs) != 1 || vs[0].Refs["heads/stable"] == "" {
		t.Fatalf("version must be a full snapshot incl. unchanged refs: %+v", vs)
	}

	vdir := filepath.Join(tmp, "slot", "repo.v1.git")
	if err := r.MaterializeVersion(ctx, vdir, vs[0], "state before rewrite"); err != nil {
		t.Fatal(err)
	}
	// the version repo is self-contained for git (via alternates) and cloneable
	clone := filepath.Join(tmp, "clone")
	sh(t, tmp, "clone", "-q", vdir, clone)
	if got := sh(t, clone, "log", "-1", "--format=%s"); got != "two" {
		t.Fatalf("version clone HEAD %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(vdir, "objects", "info", "alternates")); strings.TrimSpace(string(b)) != "../../repo.git/objects" {
		t.Fatalf("alternates must be relative: %q", b)
	}

	// fork helpers: a fork with 2 own commits
	fork := filepath.Join(tmp, "fork")
	sh(t, tmp, "clone", "-q", up, fork)
	commit(t, fork, "fork 1")
	commit(t, fork, "fork 2")
	st, _ := LsRemote(ctx, fork)
	var tips []string
	for ref, sha := range st.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			tips = append(tips, sha)
		}
	}
	if m := r.Missing(ctx, tips); len(m) != 1 {
		t.Fatalf("missing: %v", m)
	}
	scratch, _ := Init(ctx, filepath.Join(tmp, "scratch.git"))
	scratch.SetAlternates(r.Path, "")
	got, err := scratch.FetchHeads(ctx, fork, "scan")
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := r.Refs(ctx)
	var excl []string
	for _, sha := range parent {
		excl = append(excl, sha)
	}
	if n := scratch.CountNew(ctx, got, excl); n != 2 {
		t.Fatalf("ahead = %d, want 2", n)
	}
}

func TestDirectoryFileConflictAndStaleLocks(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	up := filepath.Join(tmp, "up")
	os.MkdirAll(up, 0o755)
	sh(t, up, "init", "-q", "-b", "main")
	commit(t, up, "one")
	sh(t, up, "branch", "ci")
	r, _ := Init(ctx, filepath.Join(tmp, "m.git"))
	if _, err := r.Sync(ctx, up, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	// upstream replaces branch "ci" with "ci/build" in one push
	sh(t, up, "branch", "-D", "ci")
	sh(t, up, "branch", "ci/build")
	res, err := r.Sync(ctx, up, nil, time.Now())
	if err != nil {
		t.Fatalf("ci → ci/build: %v", err)
	}
	refs, _ := r.Refs(ctx)
	// "ci" pointed at a commit main still has: removed without a version
	if refs["refs/heads/ci/build"] == "" || refs["refs/heads/ci"] != "" || res.Destructive() || change(res, "refs/heads/ci").Kind != Removed {
		t.Fatalf("refs %v", refs)
	}
	// a lock left by a killed git process is cleaned up
	lock := filepath.Join(r.Path, "refs", "heads", "main.lock")
	os.WriteFile(lock, nil, 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(lock, old, old)
	Init(ctx, r.Path)
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatal("stale lock not removed")
	}
	cg := filepath.Join(r.Path, "objects", "info", "commit-graphs")
	os.MkdirAll(cg, 0o755)
	cgLock := filepath.Join(cg, "commit-graph-chain.lock")
	os.WriteFile(cgLock, nil, 0o644)
	os.Chtimes(cgLock, old, old)
	Init(ctx, r.Path)
	if _, err := os.Stat(cgLock); !os.IsNotExist(err) {
		t.Fatal("stale commit-graph lock not removed")
	}
}

func TestRefsArePacked(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	up := filepath.Join(tmp, "up")
	os.MkdirAll(up, 0o755)
	sh(t, up, "init", "-q", "-b", "main")
	commit(t, up, "one")
	for i := 0; i < 150; i++ {
		sh(t, up, "tag", fmt.Sprintf("v%d", i))
	}
	r, _ := Init(ctx, filepath.Join(tmp, "m.git"))
	if _, err := r.Sync(ctx, up, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := r.LooseRefs(1000); n > 5 {
		t.Fatalf("%d loose ref files after sync, refs should be packed", n)
	}
	if refs, _ := r.Refs(ctx); len(refs) != 151 {
		t.Fatalf("refs %d", len(refs))
	}
}

func TestDisabledRepoIsGone(t *testing.T) {
	if !isGone("remote: Access to this repository has been disabled by GitHub staff.") || isDenied("remote: Access to this repository has been disabled by GitHub staff.") {
		t.Fatal("a repository disabled by GitHub staff is gone, not throttled")
	}
}

func TestMergedBranchDeletionIsNoVersion(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	up := filepath.Join(tmp, "up")
	os.MkdirAll(up, 0o755)
	sh(t, up, "init", "-q", "-b", "main")
	commit(t, up, "one")
	sh(t, up, "checkout", "-q", "-b", "feature")
	commit(t, up, "feature work")
	sh(t, up, "checkout", "-q", "main")
	sh(t, up, "merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	sh(t, up, "checkout", "-q", "-b", "experiment")
	commit(t, up, "never merged")
	sh(t, up, "checkout", "-q", "main")
	r, _ := Init(ctx, filepath.Join(tmp, "m.git"))
	r.Sync(ctx, up, nil, time.Now())

	// the merged PR branch is deleted: nothing is lost, no version
	sh(t, up, "branch", "-D", "feature")
	res, err := r.Sync(ctx, up, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if c := change(res, "refs/heads/feature"); c == nil || c.Kind != Removed || res.Destructive() {
		t.Fatalf("merged branch deletion: %+v", res)
	}
	if vs, _ := r.Versions(ctx); len(vs) != 0 {
		t.Fatalf("no version expected, got %d", len(vs))
	}
	// an unmerged branch disappears: its commit would be lost – keep it
	sh(t, up, "branch", "-D", "experiment")
	res, _ = r.Sync(ctx, up, nil, time.Now())
	if c := change(res, "refs/heads/experiment"); c == nil || c.Kind != Deleted || c.Lost != 1 || !res.Destructive() {
		t.Fatalf("unmerged branch deletion: %+v", res)
	}
	vs, _ := r.Versions(ctx)
	if len(vs) != 1 || vs[0].Refs["heads/experiment"] == "" {
		t.Fatalf("version must hold the unmerged branch: %+v", vs)
	}
}

func TestDeletedBranchesKeepWhatOnlyTheyReach(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	up := filepath.Join(tmp, "up")
	os.MkdirAll(up, 0o755)
	sh(t, up, "init", "-q", "-b", "main")
	commit(t, up, "one")
	for _, b := range []string{"m1", "m2", "m3"} { // merged feature branches
		sh(t, up, "checkout", "-q", "-b", b)
		commit(t, up, b+" work")
		sh(t, up, "checkout", "-q", "main")
		sh(t, up, "merge", "-q", "--no-ff", "-m", "merge "+b, b)
	}
	sh(t, up, "checkout", "-q", "-b", "tagged") // only an annotated tag keeps its commit
	commit(t, up, "tagged work")
	sh(t, up, "tag", "-a", "-m", "keep", "keep-tag")
	sh(t, up, "checkout", "-q", "-b", "tag-goes") // its tag is deleted in the same sync
	commit(t, up, "tag-goes work")
	sh(t, up, "tag", "gone-tag")
	sh(t, up, "checkout", "-q", "-b", "tag-moves") // its tag moves away in the same sync
	commit(t, up, "tag-moves work")
	sh(t, up, "tag", "-a", "-m", "mv", "moving-tag")
	sh(t, up, "checkout", "-q", "main")
	r, _ := Init(ctx, filepath.Join(tmp, "m.git"))
	if _, err := r.Sync(ctx, up, nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	sh(t, up, "branch", "-D", "m1", "m2", "m3", "tagged", "tag-goes", "tag-moves")
	sh(t, up, "tag", "-d", "gone-tag")
	sh(t, up, "tag", "-f", "-a", "-m", "mv", "moving-tag", "main")
	res, err := r.Sync(ctx, up, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string]ChangeKind{
		"refs/heads/m1": Removed, "refs/heads/m2": Removed, "refs/heads/m3": Removed, "refs/heads/tagged": Removed,
		"refs/heads/tag-goes": Deleted, "refs/heads/tag-moves": Deleted, "refs/tags/gone-tag": Deleted, "refs/tags/moving-tag": TagMoved,
	} {
		c := change(res, ref)
		if c == nil || c.Kind != want {
			t.Errorf("%s: %+v, want %s", ref, c, want)
		} else if want == Deleted && strings.HasPrefix(ref, "refs/heads/") && c.Lost < 1 {
			t.Errorf("%s: lost %d, want at least 1", ref, c.Lost)
		}
	}
	vs, _ := r.Versions(ctx)
	if len(vs) != 1 {
		t.Fatalf("one version expected, got %d", len(vs))
	}
	// the version is the complete previous state, removed branches included
	for _, name := range []string{"heads/tag-goes", "heads/tag-moves", "tags/gone-tag", "tags/moving-tag", "heads/m1", "heads/tagged", "heads/main"} {
		if vs[0].Refs[name] == "" {
			t.Errorf("version lacks %s: %v", name, vs[0].Refs)
		}
	}
}

// When git cannot count (a broken object, a killed process), a deleted branch
// must be preserved rather than passed off as "still upstream".
func TestUncountableDeletionIsPreserved(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	up := filepath.Join(tmp, "up")
	os.MkdirAll(up, 0o755)
	sh(t, up, "init", "-q", "-b", "main")
	commit(t, up, "one")
	sh(t, up, "checkout", "-q", "-b", "feature")
	commit(t, up, "feature work")
	sh(t, up, "checkout", "-q", "main")
	sh(t, up, "merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	r, _ := Init(ctx, filepath.Join(tmp, "m.git"))
	if _, err := r.Sync(ctx, up, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := r.lostPerTip(ctx, []string{"0000000000000000000000000000000000000001"}, nil); got["0000000000000000000000000000000000000001"] != -1 {
		t.Fatalf("unknown object must count as unknown, got %v", got)
	}

	// a git whose rev-list fails
	real, _ := exec.LookPath("git")
	wrapper := filepath.Join(tmp, "git")
	os.WriteFile(wrapper, []byte("#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = rev-list ] && exit 128; done\nexec "+real+" \"$@\"\n"), 0o755)
	defer func(g string) { Git = g }(Git)
	Git = wrapper

	sh(t, up, "branch", "-D", "feature")
	res, err := r.Sync(ctx, up, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if c := change(res, "refs/heads/feature"); c == nil || c.Kind != Deleted || !res.Destructive() {
		t.Fatalf("uncountable deletion must be preserved: %+v", res)
	}
	if vs, _ := r.Versions(ctx); len(vs) != 1 || vs[0].Refs["heads/feature"] == "" {
		t.Fatalf("version must hold the branch: %+v", vs)
	}
}

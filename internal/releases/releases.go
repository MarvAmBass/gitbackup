// Package releases mirrors forge releases and their assets without ever
// overwriting what was downloaded before.
//
// Layout: <repo folder>/releases/<tag>/{release.json, <assets>}
// Replaced assets and edited release notes move to <tag>/.versions/<stamp>/.
package releases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/api"
	"github.com/MarvAmBass/gitbackup/internal/provider"
	"github.com/MarvAmBass/gitbackup/internal/store"
	"github.com/MarvAmBass/gitbackup/internal/target"
)

type Syncer struct {
	Store   *store.Store
	DataDir string
	HTTP    *http.Client
}

// Options come from the repo's plan.
type Options struct {
	Assets   bool  // download files, not only metadata
	MaxBytes int64 // skip larger assets (0 = no limit)
	Partial  bool  // upstream list is incomplete: don't treat missing releases as deleted
}

// Sync records upstream releases for repo and returns the events to log.
func (s *Syncer) Sync(ctx context.Context, repo store.Repo, upstream []provider.Release, opt Options, now time.Time) ([]store.Event, error) {
	existing, err := s.Store.Releases(repo.ID)
	if err != nil {
		return nil, err
	}
	byTag := map[string]*store.Release{}
	for i := range existing {
		byTag[existing[i].Tag] = &existing[i]
	}
	stamp := now.UTC().Format("20060102T150405Z")
	root := filepath.Join(s.DataDir, target.ReleasesDir(repo.Path))
	rid := repo.ID
	var events []store.Event
	ev := func(kind, ref, detail string, extra ...string) {
		e := store.Event{TS: now.Unix(), Kind: kind, Ref: ref, Detail: detail}
		e.RepoID.Int64, e.RepoID.Valid = rid, true
		if len(extra) > 0 {
			e.Preserved = extra[0]
		}
		events = append(events, e)
	}

	seen := map[string]bool{}
	for _, up := range upstream {
		if up.Draft || up.Tag == "" {
			continue
		}
		seen[up.Tag] = true
		dir := filepath.Join(root, target.Safe(up.Tag))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return events, err
		}
		hash := metaHash(up)
		old := byTag[up.Tag]
		rec := store.Release{RepoID: rid, Tag: up.Tag, ExtID: up.ExtID, Name: up.Name, Published: up.Published.Unix(),
			Prerelease: up.Prerelease, MetaHash: hash, State: "ok", LastSeen: now.Unix()}
		switch {
		case old == nil:
			ev("release-new", up.Tag, fmt.Sprintf("%s · %d asset(s)", orTag(up), len(up.Assets)))
		case old.State == "deleted":
			ev("release-back", up.Tag, "release reappeared upstream")
		case old.MetaHash != hash:
			kept := preserve(filepath.Join(dir, "release.json"), filepath.Join(dir, ".versions", stamp))
			ev("release-edited", up.Tag, "name or notes changed upstream", rel(s.DataDir, kept))
		}
		relID, err := s.Store.SaveRelease(rec)
		if err != nil {
			return events, err
		}
		if err := writeJSON(filepath.Join(dir, "release.json"), up); err != nil {
			return events, err
		}
		if old != nil && old.State == "deleted" {
			s.Store.SetReleaseState(relID, "ok")
		}
		var cur []store.Asset
		if old != nil {
			cur = old.Assets
		}
		evs, err := s.syncAssets(ctx, relID, dir, stamp, up, cur, opt)
		for _, e := range evs {
			ev(e[0], up.Tag, e[1], e[2])
		}
		if err != nil {
			return events, err
		}
	}
	if len(existing) == 0 {
		events = collapseImport(events, rid, now)
	}
	for _, old := range existing {
		if !opt.Partial && !seen[old.Tag] && old.State != "deleted" {
			s.Store.SetReleaseState(old.ID, "deleted")
			ev("release-deleted", old.Tag, "removed upstream – files kept")
		}
	}
	return events, nil
}

// syncAssets returns [kind, detail, preserved] triples.
func (s *Syncer) syncAssets(ctx context.Context, relID int64, dir, stamp string, up provider.Release, stored []store.Asset, opt Options) ([][3]string, error) {
	current := map[string]store.Asset{} // newest live row per name
	for _, a := range stored {
		if a.State == "replaced" || a.State == "deleted" {
			continue
		}
		if c, ok := current[a.Name]; !ok || a.ID > c.ID {
			current[a.Name] = a
		}
	}
	var out [][3]string
	upNames := map[string]bool{}
	for _, ua := range up.Assets {
		upNames[ua.Name] = true
		identity := strings.Join([]string{ua.ExtID, ua.Updated, strconv.FormatInt(ua.Size, 10), ua.Digest}, "|")
		cur, has := current[ua.Name]
		if has && cur.Identity == identity {
			retry := cur.State == "failed" || cur.State == "pending" || (cur.State == "skipped" && opt.Assets && fits(cur.Size, opt))
			if !retry {
				continue
			}
		}
		if has && cur.Identity != identity {
			// same name, different file upstream: keep ours, then fetch the new one
			kept := ""
			if cur.Path != "" {
				kept = preserve(filepath.Join(s.DataDir, cur.Path), filepath.Join(dir, ".versions", stamp))
				cur.Path = rel(s.DataDir, kept)
			}
			cur.State = "replaced"
			s.Store.SaveAsset(cur)
			out = append(out, [3]string{"asset-replaced", ua.Name + " changed upstream", cur.Path})
			has = false
		}
		a := cur
		if !has {
			a = store.Asset{ReleaseID: relID, Name: ua.Name, ExtID: ua.ExtID, URL: ua.URL, Size: ua.Size, Identity: identity, Digest: ua.Digest, State: "pending"}
		}
		switch {
		case !opt.Assets:
			a.State, a.Note = "skipped", "plan stores metadata only"
		case !fits(ua.Size, opt):
			a.State, a.Note = "skipped", fmt.Sprintf("larger than %d MB", opt.MaxBytes>>20)
		default:
			dst := filepath.Join(dir, target.Safe(ua.Name))
			sum, n, err := s.download(ctx, ua, dst, opt.MaxBytes)
			if err != nil {
				if ctx.Err() != nil {
					return out, ctx.Err()
				}
				a.State, a.Note = "failed", err.Error()
			} else {
				a.State, a.Note, a.SHA256, a.Size, a.Path = "ok", "", sum, n, rel(s.DataDir, dst)
			}
		}
		if _, err := s.Store.SaveAsset(a); err != nil {
			return out, err
		}
	}
	for name, cur := range current {
		if !upNames[name] {
			cur.State = "deleted"
			cur.Note = "removed upstream"
			s.Store.SaveAsset(cur)
			out = append(out, [3]string{"asset-deleted", name + " removed upstream – file kept", cur.Path})
		}
	}
	return out, nil
}

// collapseImport turns the release-new flood of a first sync into one event.
func collapseImport(events []store.Event, repoID int64, now time.Time) []store.Event {
	var out []store.Event
	n := 0
	for _, e := range events {
		if e.Kind == "release-new" {
			n++
			continue
		}
		out = append(out, e)
	}
	if n > 0 {
		e := store.Event{TS: now.Unix(), Kind: "releases-imported", Detail: fmt.Sprintf("%d release(s) found on first sync", n)}
		e.RepoID.Int64, e.RepoID.Valid = repoID, true
		out = append([]store.Event{e}, out...)
	}
	return out
}

func fits(size int64, opt Options) bool { return opt.MaxBytes <= 0 || size <= opt.MaxBytes }

// download streams url to dst via a temp file, verifying size and digest.
func (s *Syncer) download(ctx context.Context, a provider.Asset, dst string, max int64) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", api.UserAgent)
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("download: %s", resp.Status)
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	var r io.Reader = resp.Body
	if max > 0 {
		r = io.LimitReader(resp.Body, max+1)
	}
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	sum := hex.EncodeToString(h.Sum(nil))
	switch {
	case err != nil:
	case max > 0 && n > max:
		err = fmt.Errorf("larger than %d MB", max>>20)
	case a.Size > 0 && n != a.Size:
		err = fmt.Errorf("size mismatch: got %d, expected %d", n, a.Size)
	case strings.HasPrefix(a.Digest, "sha256:") && strings.TrimPrefix(a.Digest, "sha256:") != sum:
		err = errors.New("sha256 digest mismatch")
	}
	if err != nil {
		os.Remove(tmp)
		return "", 0, err
	}
	return sum, n, os.Rename(tmp, dst)
}

// preserve moves file into dir (keeping its name) and returns the new path.
func preserve(file, dir string) string {
	if _, err := os.Stat(file); err != nil {
		return ""
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	dst := filepath.Join(dir, filepath.Base(file))
	if err := os.Rename(file, dst); err != nil {
		return ""
	}
	return dst
}

func rel(base, p string) string {
	if p == "" {
		return ""
	}
	r, err := filepath.Rel(base, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

func metaHash(r provider.Release) string {
	h := sha256.Sum256([]byte(strings.Join([]string{r.Name, r.Body, strconv.FormatBool(r.Prerelease)}, "\x00")))
	return hex.EncodeToString(h[:8])
}

func orTag(r provider.Release) string {
	if r.Name != "" {
		return r.Name
	}
	return r.Tag
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil && string(old) == string(b) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/config"
	"github.com/MarvAmBass/gitbackup/internal/store"
)

// emit records an event and runs the matching hooks from gitbackup.yaml.
func (e *Engine) emit(ev store.Event) {
	if ev.TS == 0 {
		ev.TS = time.Now().Unix()
	}
	if err := e.Store.AddEvent(ev); err != nil {
		e.Log.Error("event", "kind", ev.Kind, "err", err)
	}
	var hooks []config.Hook
	for _, h := range e.Cfg.Get().Hooks {
		if h.Matches(ev.Kind) {
			hooks = append(hooks, h)
		}
	}
	if len(hooks) == 0 {
		return
	}
	var r store.Repo
	if ev.RepoID.Valid {
		r, _ = e.Store.Repo(ev.RepoID.Int64)
	}
	for _, h := range hooks {
		go e.runHook(h, ev, r)
	}
}

// HookPayload is what a hook receives as JSON on stdin.
type HookPayload struct {
	Event     string `json:"event"`
	Time      string `json:"time"`
	Repo      string `json:"repo,omitempty"` // owner/name (host-prefixed off the big forges)
	URL       string `json:"url,omitempty"`  // upstream
	Path      string `json:"path,omitempty"` // absolute folder of the repo
	Ref       string `json:"ref,omitempty"`
	Old       string `json:"old,omitempty"`
	New       string `json:"new,omitempty"`
	Version   string `json:"version,omitempty"`   // preserved state stamp
	Preserved string `json:"preserved,omitempty"` // ref or file that holds the old state
	Lost      int    `json:"lost,omitempty"`      // commits dropped upstream
	Detail    string `json:"detail,omitempty"`
}

func (e *Engine) runHook(h config.Hook, ev store.Event, r store.Repo) {
	e.hookSem <- struct{}{}
	defer func() { <-e.hookSem }()
	timeout := time.Duration(h.Timeout)
	if timeout <= 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	p := HookPayload{Event: ev.Kind, Time: time.Unix(ev.TS, 0).UTC().Format(time.RFC3339), Ref: ev.Ref, Old: ev.Old, New: ev.New,
		Version: ev.Version, Preserved: ev.Preserved, Lost: ev.Lost, Detail: ev.Detail}
	if r.ID != 0 {
		p.Repo, p.URL, p.Path = r.FullName(), r.URL, e.abs(r.Path)
	}
	body, _ := json.Marshal(p)
	cmd := exec.CommandContext(ctx, "sh", "-c", h.Run)
	cmd.Dir = filepath.Dir(e.DataDir)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = append(os.Environ(),
		"GB_EVENT="+p.Event, "GB_TIME="+p.Time, "GB_REPO="+p.Repo, "GB_URL="+p.URL, "GB_PATH="+p.Path, "GB_REF="+p.Ref,
		"GB_OLD="+p.Old, "GB_NEW="+p.New, "GB_VERSION="+p.Version, "GB_PRESERVED="+p.Preserved,
		"GB_LOST="+strconv.Itoa(p.Lost), "GB_DETAIL="+p.Detail, "GB_HOOK="+h.Name)
	out, err := cmd.CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	if err != nil {
		e.Log.Warn("hook failed", "hook", h.Name, "event", ev.Kind, "repo", p.Repo, "err", err, "output", msg)
		return
	}
	e.Log.Info("hook ran", "hook", h.Name, "event", ev.Kind, "repo", p.Repo, "output", msg)
}

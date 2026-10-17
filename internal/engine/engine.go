// Package engine schedules discovery (API) and checks (git transport),
// records what changed and keeps the data/ folder tree in shape.
package engine

import (
	"context"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/config"
	"github.com/MarvAmBass/gitbackup/internal/pages"
	"github.com/MarvAmBass/gitbackup/internal/provider"
	"github.com/MarvAmBass/gitbackup/internal/releases"
	"github.com/MarvAmBass/gitbackup/internal/store"
)

type Engine struct {
	Store    *store.Store
	Cfg      *config.File
	Forges   *provider.Registry
	Releases *releases.Syncer
	Pages    *pages.Builder
	DataDir  string // data/: everything browsable
	StateDir string // state/: database and scratch space
	Log      *slog.Logger

	mu          sync.Mutex
	inflight    map[int64]string // repo id → activity
	discovering map[int64]string
	indexBusy   string
	wake        chan struct{}
	indexDirty  bool
	hookSem     chan struct{}
	gates       map[string]*hostGate // per-host git pacing and cooldowns
}

func New(st *store.Store, cfg *config.File, forges *provider.Registry, rs *releases.Syncer, pb *pages.Builder,
	dataDir, stateDir string, log *slog.Logger) *Engine {
	return &Engine{Store: st, Cfg: cfg, Forges: forges, Releases: rs, Pages: pb, DataDir: dataDir, StateDir: stateDir, Log: log,
		inflight: map[int64]string{}, discovering: map[int64]string{}, wake: make(chan struct{}, 1), indexDirty: true,
		hookSem: make(chan struct{}, 4)}
}

// abs turns a path below data/ into an absolute path.
func (e *Engine) abs(rel string) string { return filepath.Join(e.DataDir, filepath.FromSlash(rel)) }

// Wake makes the scheduler look for due work immediately.
func (e *Engine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Activity lists running jobs for the dashboard.
func (e *Engine) Activity() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, v := range e.discovering {
		out = append(out, v)
	}
	for _, v := range e.inflight {
		out = append(out, v)
	}
	if e.indexBusy != "" {
		out = append(out, e.indexBusy)
	}
	sort.Strings(out)
	e.mu.Unlock()
	out = append(out, e.coolingHosts()...)
	e.mu.Lock()
	return out
}

// setActivity relabels a running job (the scheduler owns the entry; a label
// must never register a repo as busy by itself).
func (e *Engine) setActivity(id int64, what string) {
	e.mu.Lock()
	if _, ok := e.inflight[id]; ok {
		e.inflight[id] = what
	}
	e.mu.Unlock()
}

// Run blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	if gh, ok := e.Forges.For(provider.GitHub, "github.com").(*provider.GitHubProvider); ok {
		if err := gh.RefreshRate(ctx); err != nil {
			e.Log.Warn("github rate_limit", "err", err)
		}
	}
	go e.Reconcile(ctx) // in the background: never holds up scheduling
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		e.tick(ctx, &wg)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.wake:
		}
	}
}

func (e *Engine) tick(ctx context.Context, wg *sync.WaitGroup) {
	now := time.Now().Unix()

	// discovery: one source at a time (API clients are serialized anyway)
	e.mu.Lock()
	busy := len(e.discovering) > 0
	e.mu.Unlock()
	if !busy {
		if srcs, err := e.Store.DueSources(now); err != nil {
			e.Log.Error("due sources", "err", err)
		} else if len(srcs) > 0 {
			src := srcs[0]
			e.mu.Lock()
			e.discovering[src.ID] = "discover " + src.Target
			e.mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				e.Discover(ctx, src, false)
				e.mu.Lock()
				delete(e.discovering, src.ID)
				e.mu.Unlock()
				e.Wake()
			}()
		}
	}

	// checks: fill free git worker slots, most important first
	e.mu.Lock()
	free := e.Cfg.Get().Settings.GitWorkers - len(e.inflight)
	if e.Cfg.Get().Settings.Paused {
		free = 0 // maintenance: discovery only
	}
	var exclude []int64
	for id := range e.inflight {
		exclude = append(exclude, id)
	}
	e.mu.Unlock()
	if free > 0 {
		repos, err := e.Store.DueRepos(now, free, exclude)
		if err != nil {
			e.Log.Error("due repos", "err", err)
		}
		for _, r := range repos {
			e.mu.Lock()
			e.inflight[r.ID] = "check " + r.FullName()
			e.mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				e.Check(ctx, r)
				e.mu.Lock()
				delete(e.inflight, r.ID)
				e.mu.Unlock()
				e.Wake()
			}()
		}
	}

	// index/landing pages: single background worker
	e.mu.Lock()
	ibusy := e.indexBusy != ""
	if !ibusy {
		e.indexBusy = "html index"
	}
	e.mu.Unlock()
	if !ibusy {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.runIndex()
			e.mu.Lock()
			e.indexBusy = ""
			e.mu.Unlock()
		}()
	}
}

// DiscoverNow runs a manual discovery in the background. Manual runs may use
// the API reserve. Returns false if a discovery is already running.
func (e *Engine) DiscoverNow(ctx context.Context, src store.Source) bool {
	e.mu.Lock()
	if len(e.discovering) > 0 {
		e.mu.Unlock()
		return false
	}
	e.discovering[src.ID] = "discover " + src.Target + " (manual)"
	e.mu.Unlock()
	go func() {
		e.Discover(ctx, src, true)
		e.mu.Lock()
		delete(e.discovering, src.ID)
		e.mu.Unlock()
		e.Wake()
	}()
	return true
}

// MarkIndexDirty forces the HTML index to be regenerated on the next tick.
func (e *Engine) MarkIndexDirty() {
	e.mu.Lock()
	e.indexDirty = true
	e.mu.Unlock()
	e.Wake()
}

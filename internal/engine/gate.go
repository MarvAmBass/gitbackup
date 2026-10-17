package engine

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/api"
	"github.com/MarvAmBass/gitbackup/internal/provider"
	"github.com/MarvAmBass/gitbackup/internal/store"
)

// Git transport has no rate-limit headers, but forges throttle anonymous git
// traffic (GitHub answers HTTP 401, exactly like for a missing repo) and do
// not publish the limit. So git network operations are paced per host and the
// pace is learned: every throttle doubles the gap between requests (plus a
// cooldown during which the host's repos are postponed – not marked as
// errors); success streaks shorten it again, never below the configured
// git_gap. The learned gap survives restarts.
//
// Waiting for a slot is fair and prioritised: initial clones before updates
// before wiki and fork probes, first come first served within a priority.

const (
	defaultGitGap = time.Second
	maxGitGap     = 2 * time.Minute
	cooldownStart = 15 * time.Minute
	cooldownMax   = 2 * time.Hour
	speedUpAfter  = 20 // successful operations before the gap shrinks by 15%
)

// Priorities for a git slot.
const (
	PrioProbe   = 0 // wiki, fork probes
	PrioUpdate  = 1 // routine check of a repo we already have
	PrioInitial = 2 // first clone – most important
)

type hostGate struct {
	mu      sync.Mutex
	last    time.Time
	until   time.Time
	backoff time.Duration
	learned time.Duration // 0 = configured gap
	streak  int
	busy    bool
	queue   [3][]chan struct{} // waiting operations per priority, FIFO
}

// next hands the slot to the oldest waiter of the highest priority.
func (g *hostGate) next() {
	if g.busy {
		return
	}
	for p := len(g.queue) - 1; p >= 0; p-- {
		if len(g.queue[p]) > 0 {
			ch := g.queue[p][0]
			g.queue[p] = g.queue[p][1:]
			g.busy = true
			close(ch)
			return
		}
	}
}

func (g *hostGate) remove(ch chan struct{}) bool {
	for p := range g.queue {
		for i, c := range g.queue[p] {
			if c == ch {
				g.queue[p] = append(g.queue[p][:i], g.queue[p][i+1:]...)
				return true
			}
		}
	}
	return false
}

// Waiting reports queued operations per priority (dashboard).
func (g *hostGate) waiting() (n [3]int) {
	for p := range g.queue {
		n[p] = len(g.queue[p])
	}
	return
}

// CoolingError means the host is in a throttling cooldown.
type CoolingError struct {
	Host  string
	Until time.Time
}

func (c *CoolingError) Error() string {
	return fmt.Sprintf("%s throttles git requests – paused until %s", c.Host, c.Until.Format("15:04"))
}

func (e *Engine) gate(host string) *hostGate {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gates == nil {
		e.gates = map[string]*hostGate{}
	}
	g := e.gates[host]
	if g == nil {
		g = &hostGate{}
		if e.Store != nil {
			g.learned, _ = time.ParseDuration(e.Store.Setting("git_gap:"+host, "0s"))
			g.learned = min(g.learned, maxGitGap)
		}
		e.gates[host] = g
	}
	return g
}

func (e *Engine) saveGap(host string, gap time.Duration) {
	if e.Store != nil {
		e.Store.SetSetting("git_gap:"+host, gap.Round(time.Millisecond).String())
	}
}

// baseGap is the configured minimum gap of a host.
func (e *Engine) baseGap(host string) time.Duration {
	if host == "local" {
		return 0
	}
	if in, ok := e.Cfg.Get().Instance(host); ok && in.GitGap > 0 {
		return time.Duration(in.GitGap)
	}
	return defaultGitGap
}

// gitGap is the gap currently used: the learned one, at least the base.
func (e *Engine) gitGap(host string) time.Duration {
	base := e.baseGap(host)
	g := e.gate(host)
	g.mu.Lock()
	defer g.mu.Unlock()
	return max(base, g.learned)
}

// waitGit blocks until a git network operation to host may start.
func (e *Engine) waitGit(ctx context.Context, host string, prio int) error {
	gap := e.gitGap(host)
	g := e.gate(host)
	ch := make(chan struct{})
	g.mu.Lock()
	g.queue[prio] = append(g.queue[prio], ch)
	g.next()
	g.mu.Unlock()
	select {
	case <-ch: // our turn
	case <-ctx.Done():
		g.mu.Lock()
		if !g.remove(ch) { // the slot was handed to us meanwhile: pass it on
			g.busy = false
			g.next()
		}
		g.mu.Unlock()
		return ctx.Err()
	}
	release := func() {
		g.mu.Lock()
		g.busy = false
		g.next()
		g.mu.Unlock()
	}
	g.mu.Lock()
	if time.Now().Before(g.until) {
		until := g.until
		g.mu.Unlock()
		release()
		return &CoolingError{Host: host, Until: until}
	}
	wait := g.last.Add(gap).Sub(time.Now())
	g.mu.Unlock()
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			release()
			return ctx.Err()
		}
	}
	g.mu.Lock()
	g.last = time.Now()
	g.mu.Unlock()
	release()
	return nil
}

// throttled starts (or doubles) the cooldown of a host and slows its pace.
// repo and cause are logged so false alarms can be told apart.
func (e *Engine) throttled(host, repo string, cause error) time.Time {
	base := e.baseGap(host)
	g := e.gate(host)
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Now().Before(g.until) {
		return g.until // another check already noticed
	}
	g.backoff = min(max(g.backoff*2, cooldownStart), cooldownMax)
	g.until = time.Now().Add(g.backoff)
	g.learned = min(max(max(g.learned, base)*2, time.Second), maxGitGap)
	g.streak = 0
	e.saveGap(host, g.learned)
	e.Log.Warn("host throttles git requests, pausing it and slowing down", "host", host,
		"until", g.until.Format("15:04"), "new_gap", g.learned, "repo", repo, "cause", fmt.Sprint(cause))
	return g.until
}

// gitOK records a successful operation; a long streak speeds the host up again.
func (e *Engine) gitOK(host string) {
	base := e.baseGap(host)
	g := e.gate(host)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.backoff = 0
	if g.learned <= base {
		return
	}
	if g.streak++; g.streak >= speedUpAfter {
		g.streak = 0
		g.learned = max(base, g.learned*85/100)
		e.saveGap(host, g.learned)
	}
}

// ResetPace forgets the learned git pace of every host (back to git_gap).
func (e *Engine) ResetPace() {
	e.mu.Lock()
	gates := e.gates
	e.mu.Unlock()
	for host, g := range gates {
		g.mu.Lock()
		g.learned, g.streak, g.backoff, g.until = 0, 0, 0, time.Time{}
		g.mu.Unlock()
		e.saveGap(host, 0)
	}
	e.Log.Info("learned git pace reset")
}

// coolingHosts lists hosts in cooldown, for the dashboard.
func (e *Engine) coolingHosts() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for h, g := range e.gates {
		g.mu.Lock()
		if time.Now().Before(g.until) {
			out = append(out, fmt.Sprintf("%s: git throttled, paused until %s", h, g.until.Format("15:04")))
		}
		if g.learned > defaultGitGap {
			out = append(out, fmt.Sprintf("%s: git pace learned – one request every %s", h, g.learned.Round(time.Second)))
		}
		if w := g.waiting(); w[PrioInitial]+w[PrioUpdate]+w[PrioProbe] > 0 {
			out = append(out, fmt.Sprintf("%s: waiting for a git slot – %d first clone(s), %d update(s), %d wiki/fork probe(s)",
				h, w[PrioInitial], w[PrioUpdate], w[PrioProbe]))
		}
		g.mu.Unlock()
	}
	sort.Strings(out)
	return out
}

// confirmGone asks the repo's web page whether it still exists – a 401 from
// git does not tell. No API quota is used. known=false: could not tell.
func (e *Engine) confirmGone(ctx context.Context, r store.Repo) (gone, known bool) {
	if r.Platform == string(provider.Git) || !strings.HasPrefix(r.URL, "http") {
		return false, false
	}
	web := strings.TrimSuffix(strings.TrimSuffix(r.URL, "/"), ".git")
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, web, nil)
	if err != nil {
		return false, false
	}
	req.Header.Set("User-Agent", api.UserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, false
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return true, true
	case resp.StatusCode < 400:
		return false, true // the page exists: we were throttled
	}
	return false, false
}

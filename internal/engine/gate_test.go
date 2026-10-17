package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/config"
	"github.com/MarvAmBass/gitbackup/internal/store"
)

func TestGitGateAndGoneConfirmation(t *testing.T) {
	cfg, err := config.Open(filepath.Join(t.TempDir(), "gitbackup.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Update(func(c *config.Config) error {
		c.Instances[0].GitGap = config.Duration(50 * time.Millisecond)
		return nil
	})
	e := &Engine{Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), inflight: map[int64]string{}, discovering: map[int64]string{}}
	ctx := context.Background()

	// pacing: three operations to github.com take at least two gaps
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := e.waitGit(ctx, "github.com", PrioUpdate); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("not paced: %v", d)
	}
	if err := e.waitGit(ctx, "local", PrioUpdate); err != nil { // file:// is never paced
		t.Fatal(err)
	}

	// cooldown: starts at 15m, doubles, others hosts unaffected
	until := e.throttled("github.com", "a/b", errors.New("401"))
	var cool *CoolingError
	if err := e.waitGit(ctx, "github.com", PrioUpdate); !errors.As(err, &cool) || !cool.Until.Equal(until) {
		t.Fatalf("expected cooldown, got %v", err)
	}
	if d := time.Until(until); d < 14*time.Minute || d > 16*time.Minute {
		t.Fatalf("first cooldown %v", d)
	}
	if err := e.waitGit(ctx, "gitlab.com", PrioUpdate); err != nil {
		t.Fatalf("other hosts must not cool down: %v", err)
	}
	if g := e.gitGap("github.com"); g != time.Second {
		t.Fatalf("first throttle: gap should become 1s, got %v", g)
	}
	e.gate("github.com").until = time.Time{} // cooldown over, still throttled
	if d := time.Until(e.throttled("github.com", "a/b", errors.New("401"))); d < 29*time.Minute {
		t.Fatalf("second cooldown should double, got %v", d)
	}
	if g := e.gitGap("github.com"); g != 2*time.Second {
		t.Fatalf("second throttle: gap should double to 2s, got %v", g)
	}
	for i := 0; i < speedUpAfter; i++ {
		e.gitOK("github.com")
	}
	if g := e.gitGap("github.com"); g != 1700*time.Millisecond {
		t.Fatalf("a long success streak should shorten the gap by 10%%, got %v", g)
	}
	if len(e.coolingHosts()) != 2 {
		t.Fatalf("dashboard should show cooldown and learned pace: %v", e.coolingHosts())
	}

	// a 401 from git: the web page decides between deleted and throttled
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/a/deleted" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("repo page"))
	}))
	defer web.Close()
	if gone, known := e.confirmGone(ctx, store.Repo{Platform: "github", URL: web.URL + "/a/deleted.git"}); !gone || !known {
		t.Fatal("404 page: repo is gone")
	}
	if gone, known := e.confirmGone(ctx, store.Repo{Platform: "github", URL: web.URL + "/a/exists.git"}); gone || !known {
		t.Fatal("existing page: we were throttled, not gone")
	}
	if _, known := e.confirmGone(ctx, store.Repo{Platform: "git", URL: "git://host/x"}); known {
		t.Fatal("plain git: cannot tell")
	}
}

func TestGatePriorityIsFairAndOrdered(t *testing.T) {
	cfg, _ := config.Open(filepath.Join(t.TempDir(), "gitbackup.yaml"))
	cfg.Update(func(c *config.Config) error {
		c.Instances[0].GitGap = config.Duration(30 * time.Millisecond)
		return nil
	})
	e := &Engine{Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	e.waitGit(ctx, "github.com", PrioUpdate) // occupy: the next slot is 30ms away

	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	start := func(name string, prio int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.waitGit(ctx, "github.com", prio)
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
		}()
		time.Sleep(5 * time.Millisecond) // arrival order
	}
	start("update-1", PrioUpdate)
	start("wiki", PrioProbe)
	start("update-2", PrioUpdate)
	start("clone", PrioInitial)
	wg.Wait()
	// update-1 already had the slot when the others arrived; then strictly by priority, FIFO within
	if got := strings.Join(order, ","); got != "update-1,clone,update-2,wiki" {
		t.Fatalf("order %s", got)
	}
}

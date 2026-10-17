// Package streagit runs streagit – the on-demand, stagit-compatible page
// renderer (github.com/MarvAmBass/streagit) – as a child process and
// reverse-proxies the HTML paths of the data tree to it. Pages are rendered
// when somebody looks at them instead of pre-generating every commit page.
package streagit

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// ansi strips terminal colour codes from the child's log lines.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Process supervises the streagit child process.
type Process struct {
	Binary string
	Listen string // e.g. 127.0.0.1:8081
	Root   string // gitbackup's data/ folder
	Prefix string // URL prefix gitbackup serves it under, e.g. /browse
	Cache  string // persistent diffstat cache folder
	Public string // optional absolute URL of the prefix (atom feeds)
	Extra  []string
	Log    *slog.Logger

	running atomic.Bool
}

// Handles reports whether a path below the browse prefix belongs to
// streagit: robots.txt, and anything below an html segment (html/, html.v2/,
// html.wiki.v1/ …) that stands next to the git directory it names – the same
// rule streagit resolves with, rightmost segment first. A repository, owner
// or release file that merely is called "html" stays with the file server.
func (p *Process) Handles(rel string) bool {
	if rel == "robots.txt" {
		return true
	}
	segs := strings.Split(rel, "/")
	for _, s := range segs {
		if s == ".." {
			return false
		}
	}
	for i := len(segs) - 1; i >= 0; i-- {
		gitDir := htmlGitDir(segs[i])
		if gitDir == "" {
			continue
		}
		slot := filepath.Join(append([]string{p.Root}, segs[:i]...)...)
		if _, err := os.Stat(filepath.Join(slot, gitDir, "HEAD")); err == nil {
			return true
		}
	}
	return false
}

// htmlGitDir maps an html segment to the git directory streagit renders:
// html → repo.git, html.v2 → repo.v2.git, html.wiki → wiki.git,
// html.wiki.v1 → wiki.v1.git. "" for anything else.
func htmlGitDir(seg string) string {
	rest, ok := strings.CutPrefix(seg, "html")
	if !ok {
		return ""
	}
	if rest == "" {
		return "repo.git"
	}
	if rest[0] != '.' {
		return ""
	}
	parts := strings.Split(rest[1:], ".")
	switch {
	case len(parts) == 1 && isVersion(parts[0]):
		return "repo." + parts[0] + ".git"
	case len(parts) == 1 && isUnit(parts[0]):
		return parts[0] + ".git"
	case len(parts) == 2 && isUnit(parts[0]) && isVersion(parts[1]):
		return parts[0] + "." + parts[1] + ".git"
	}
	return ""
}

var (
	version  = regexp.MustCompile(`^v[1-9][0-9]*$`)
	vDigits  = regexp.MustCompile(`^v[0-9]+$`)
	unitName = regexp.MustCompile(`^[A-Za-z0-9_~+-]+$`)
)

func isVersion(s string) bool { return version.MatchString(s) }
func isUnit(s string) bool    { return unitName.MatchString(s) && !vDigits.MatchString(s) }

// Available reports whether the binary can be found.
func (p *Process) Available() bool {
	_, err := exec.LookPath(p.Binary)
	return err == nil
}

// Running reports whether the child is up.
func (p *Process) Running() bool { return p.running.Load() }

func (p *Process) args() []string {
	// low CPU priority, two render threads, quiet logs, cache pages up to 4 MiB
	a := []string{"--listen", p.Listen, "--root", p.Root, "--prefix", p.Prefix,
		"--nice", "10", "--render-threads", "2", "--log-level", "warn", "--cache-max-entry", "4MiB"}
	if p.Cache != "" {
		a = append(a, "--cache-dir", p.Cache)
	}
	if p.Public != "" {
		a = append(a, "--public-url", p.Public)
	}
	return append(a, p.Extra...)
}

// Run keeps streagit running until ctx is done, restarting it with a growing
// delay when it exits.
func (p *Process) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		bin, args := p.Binary, p.args()
		// lowest best-effort I/O priority: backups go first, but somebody waiting
		// for a page is never starved (the idle class -c3 was, on busy disks)
		if io, err := exec.LookPath("ionice"); err == nil {
			bin, args = io, append([]string{"-c2", "-n7", p.Binary}, args...)
		}
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Cancel = func() error { return cmd.Process.Signal(sigterm) }
		cmd.WaitDelay = 10 * time.Second
		cmd.Env = append(cmd.Environ(), "NO_COLOR=1") // plain log lines in gitbackup's log
		out, _ := cmd.StdoutPipe()
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			p.Log.Error("streagit: cannot start", "binary", p.Binary, "err", err)
		} else {
			p.running.Store(true)
			p.Log.Info("streagit started", "listen", p.Listen, "pid", cmd.Process.Pid)
			go p.pipe(out)
			err := cmd.Wait()
			p.running.Store(false)
			if ctx.Err() != nil {
				return
			}
			p.Log.Warn("streagit exited, restarting", "err", err, "after", backoff)
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second // ran fine for a while: restart quickly
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// pipe forwards the child's log lines. It keeps reading whatever happens: a
// child whose stdout nobody drains would block on its next write.
func (p *Process) pipe(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		p.Log.Info("streagit", "out", strings.TrimSpace(ansi.ReplaceAllString(sc.Text(), "")))
	}
	if sc.Err() != nil {
		io.Copy(io.Discard, r)
	}
}

// Proxy forwards requests (path unchanged, prefix included) to streagit.
func (p *Process) Proxy() http.Handler {
	target := &url.URL{Scheme: "http", Host: p.Listen}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1 // pages are streamed: pass chunks on immediately
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "The page renderer (streagit) is not reachable right now – try again in a few seconds.\n")
		p.Log.Warn("streagit proxy", "path", r.URL.Path, "err", err)
	}
	return rp
}

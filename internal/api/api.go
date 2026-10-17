// Package api is a small, rate-limit-aware HTTP client shared by all forge
// providers.
//
// Requests to one instance are serialized, spaced out, conditional (ETag)
// where possible, and refuse to dip into a configurable reserve of the quota
// unless the call is manual (user-triggered in the UI). It understands the
// GitHub (X-RateLimit-*), GitLab (RateLimit-*) and IETF draft (RateLimit:
// r=..;t=.., used by Forgejo/Codeberg) header families.
package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UserAgent identifies gitbackup in every HTTP request it makes to a forge.
const UserAgent = "gitbackup (+https://github.com/MarvAmBass/gitbackup)"

var ErrNotFound = errors.New("not found")

// RateLimitedError is returned when a call would exceed the budget.
type RateLimitedError struct{ Until time.Time }

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("API budget exhausted until %s", e.Until.Format("15:04:05"))
}

// Cache is the conditional-request cache (implemented by store.Store).
type Cache interface {
	CacheGet(url string) (etag, next string, body []byte, ok bool)
	CachePut(url, etag, next string, body []byte)
}

// Config is read on every request so UI changes apply immediately.
type Config struct {
	Token      string
	AuthScheme string        // "Bearer" (default) or "token" (Gitea)
	ReservePct int           // share of the limit the scheduler must not touch
	MinGap     time.Duration // minimum pause between requests
}

// Rate is the last known quota state.
type Rate struct {
	Limit, Remaining int
	Reset            time.Time
	Known            bool
	Authenticated    bool
}

// Counters for the dashboard.
type Counters struct {
	Requests, NotModified, Errors int
	LastRequest                   time.Time
}

type Client struct {
	Name   string // instance host, for display
	HTTP   *http.Client
	cache  Cache
	config func() Config

	mu       sync.Mutex
	rate     Rate
	counters Counters
	last     time.Time
	lastTok  string
	lastBg   time.Time
}

// DiscoveryPct is the share of the limit (on top of the reserve) that
// background work leaves free, so discovery always finds budget.
const DiscoveryPct = 10

// Background reports whether optional work (release syncs, fork scans, repo
// metadata) may make a request now. It stops DiscoveryPct above the reserve
// and spreads what is left evenly until the reset, so an anonymous 60/h
// budget is used as a steady trickle instead of one burst. With a token the
// spacing is about a second.
func (c *Client) Background() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !c.rate.Known || now.After(c.rate.Reset) {
		return true // unknown or fresh window: just try
	}
	floor := reserve(c.rate.Limit, c.config().ReservePct) + reserve(c.rate.Limit, DiscoveryPct)
	spare := c.rate.Remaining - floor
	if spare <= 0 {
		return false
	}
	if gap := c.rate.Reset.Sub(now) / time.Duration(spare); now.Sub(c.lastBg) < gap {
		return false
	}
	c.lastBg = now
	return true
}

func New(name string, cache Cache, config func() Config) *Client {
	return &Client{Name: name, HTTP: &http.Client{Timeout: 60 * time.Second}, cache: cache, config: config}
}

func (c *Client) Rate() Rate         { c.mu.Lock(); defer c.mu.Unlock(); return c.rate }
func (c *Client) Counters() Counters { c.mu.Lock(); defer c.mu.Unlock(); return c.counters }
func (c *Client) Config() Config     { return c.config() }

func reserve(limit, pct int) int { return limit * pct / 100 }

// Budget reports how many requests the scheduler may still spend before
// reset. known is false when no quota information is available (or the window
// has already reset), in which case a request may simply be tried.
func (c *Client) Budget() (n int, known bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.rate.Known || time.Now().After(c.rate.Reset) {
		return 0, false
	}
	return c.rate.Remaining - reserve(c.rate.Limit, c.config().ReservePct), true
}

// SetRate lets providers feed quota info from a dedicated endpoint.
func (c *Client) SetRate(r Rate) { c.mu.Lock(); c.rate = r; c.mu.Unlock() }

// Request describes one call.
type Request struct {
	Method  string // default GET
	URL     string
	Body    []byte // JSON body for POST
	Accept  string // default application/json
	Manual  bool   // may use the reserve
	Cache   bool   // conditional GET with ETag cache
	Headers map[string]string
	Free    bool // endpoint does not count against the quota (e.g. /rate_limit)
}

// Response is the decoded result.
type Response struct {
	Body   []byte
	Next   string // pagination link (Link: rel="next")
	Header http.Header
	Cached bool
}

// Do performs one request.
func (c *Client) Do(ctx context.Context, rq Request) (*Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cfg := c.config()
	if cfg.Token != c.lastTok { // token changed → quota state is stale
		c.rate, c.lastTok = Rate{}, cfg.Token
	}
	now := time.Now()
	if c.rate.Known && now.Before(c.rate.Reset) && !rq.Free {
		floor := reserve(c.rate.Limit, cfg.ReservePct)
		if rq.Manual {
			floor = 0
		}
		if c.rate.Remaining <= floor {
			return nil, &RateLimitedError{Until: c.rate.Reset}
		}
	}
	if wait := cfg.MinGap - now.Sub(c.last); wait > 0 {
		c.mu.Unlock()
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			c.mu.Lock()
			return nil, ctx.Err()
		}
		c.mu.Lock()
	}

	method := rq.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if rq.Body != nil {
		body = bytes.NewReader(rq.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rq.URL, body)
	if err != nil {
		return nil, err
	}
	accept := rq.Accept
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", UserAgent)
	if rq.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range rq.Headers {
		req.Header.Set(k, v)
	}
	if cfg.Token != "" {
		scheme := cfg.AuthScheme
		if scheme == "" {
			scheme = "Bearer"
		}
		req.Header.Set("Authorization", scheme+" "+cfg.Token)
	}
	var cachedBody []byte
	var cachedNext string
	useCache := rq.Cache && method == http.MethodGet
	if useCache {
		if etag, nx, b, ok := c.cache.CacheGet(rq.URL); ok {
			req.Header.Set("If-None-Match", etag)
			cachedBody, cachedNext = b, nx
		}
	}
	resp, err := c.HTTP.Do(req)
	c.last = time.Now()
	c.counters.LastRequest = c.last
	c.counters.Requests++
	if err != nil {
		c.counters.Errors++
		return nil, err
	}
	defer resp.Body.Close()
	if r, ok := parseRate(resp.Header, time.Now()); ok {
		r.Authenticated = cfg.Token != ""
		c.rate = r
	}
	out := &Response{Header: resp.Header, Next: nextLink(resp.Header.Get("Link"))}

	switch {
	case resp.StatusCode == http.StatusNotModified && cachedBody != nil:
		c.counters.NotModified++
		out.Body, out.Cached = cachedBody, true
		if out.Next == "" {
			out.Next = cachedNext
		}
		return out, nil
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		c.counters.Errors++
		until := c.rate.Reset
		if ra, _ := strconv.Atoi(resp.Header.Get("Retry-After")); ra > 0 {
			until = time.Now().Add(time.Duration(ra) * time.Second)
		}
		exhausted := resp.StatusCode == http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "" ||
			(c.rate.Known && c.rate.Remaining == 0)
		if exhausted {
			if until.IsZero() || until.Before(time.Now()) {
				until = time.Now().Add(time.Minute)
			}
			return nil, &RateLimitedError{Until: until}
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: %s: %s", c.Name, resp.Status, strings.TrimSpace(string(b)))
	case resp.StatusCode >= 300:
		c.counters.Errors++
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: %s: %s", c.Name, resp.Status, strings.TrimSpace(string(b)))
	}
	out.Body, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if etag := resp.Header.Get("ETag"); useCache && etag != "" {
		c.cache.CachePut(rq.URL, etag, out.Next, out.Body)
	}
	return out, nil
}

var (
	linkNext  = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)
	ietfRemR  = regexp.MustCompile(`(?:^|[;,\s])r=(\d+)`)
	ietfResT  = regexp.MustCompile(`(?:^|[;,\s])t=(\d+)`)
	ietfLimit = regexp.MustCompile(`(?:^|[;,\s])q=(\d+)`)
)

func nextLink(h string) string {
	if m := linkNext.FindStringSubmatch(h); m != nil {
		return m[1]
	}
	return ""
}

// parseRate understands GitHub, GitLab and the IETF RateLimit draft headers.
func parseRate(h http.Header, now time.Time) (Rate, bool) {
	// GitHub: only track the "core" resource
	if res := h.Get("X-RateLimit-Resource"); res != "" && res != "core" {
		return Rate{}, false
	}
	for _, p := range []string{"X-RateLimit-", "RateLimit-"} {
		lim, e1 := strconv.Atoi(h.Get(p + "Limit"))
		rem, e2 := strconv.Atoi(h.Get(p + "Remaining"))
		reset, e3 := strconv.ParseInt(h.Get(p+"Reset"), 10, 64)
		if e1 == nil && e2 == nil && e3 == nil {
			t := time.Unix(reset, 0)
			if reset < 1e9 { // some servers send "seconds until reset"
				t = now.Add(time.Duration(reset) * time.Second)
			}
			return Rate{Limit: lim, Remaining: rem, Reset: t, Known: true}, true
		}
	}
	// IETF draft: RateLimit: "baseline";r=1997;t=600  RateLimit-Policy: "baseline";q=2000;w=600
	if v := h.Get("RateLimit"); v != "" {
		rm, tm := ietfRemR.FindStringSubmatch(v), ietfResT.FindStringSubmatch(v)
		qm := ietfLimit.FindStringSubmatch(h.Get("RateLimit-Policy"))
		if rm != nil && tm != nil {
			rem, _ := strconv.Atoi(rm[1])
			secs, _ := strconv.Atoi(tm[1])
			lim := rem
			if qm != nil {
				lim, _ = strconv.Atoi(qm[1])
			}
			return Rate{Limit: lim, Remaining: rem, Reset: now.Add(time.Duration(secs) * time.Second), Known: true}, true
		}
	}
	return Rate{}, false
}

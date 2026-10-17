package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

type memCache struct {
	sync.Mutex
	m map[string][3]string
}

func (c *memCache) CacheGet(u string) (string, string, []byte, bool) {
	c.Lock()
	defer c.Unlock()
	v, ok := c.m[u]
	return v[0], v[1], []byte(v[2]), ok
}
func (c *memCache) CachePut(u, etag, next string, body []byte) {
	c.Lock()
	defer c.Unlock()
	c.m[u] = [3]string{etag, next, string(body)}
}

func TestClient(t *testing.T) {
	remaining := 60
	var conditional int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reset := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Reset", reset)
		if r.Header.Get("If-None-Match") == `"v1"` {
			conditional++
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining)) // 304 is free
			w.WriteHeader(http.StatusNotModified)
			return
		}
		remaining--
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		switch r.URL.Path {
		case "/list":
			w.Header().Set("ETag", `"v1"`)
			if r.URL.Query().Get("page") == "" {
				w.Header().Set("Link", fmt.Sprintf(`<http://%s/list?page=2>; rel="next"`, r.Host))
			}
			fmt.Fprint(w, `[1]`)
		case "/missing":
			http.NotFound(w, r)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	cfg := Config{ReservePct: 20}
	c := New("test", &memCache{m: map[string][3]string{}}, func() Config { return cfg })
	ctx := context.Background()
	get := func(path string, manual bool) (*Response, error) {
		return c.Do(ctx, Request{URL: srv.URL + path, Manual: manual, Cache: true})
	}

	r1, err := get("/list", false)
	if err != nil || r1.Next == "" {
		t.Fatalf("first page: %v %+v", err, r1)
	}
	// conditional request: 304 served from cache, pagination link survives
	r2, err := get("/list", false)
	if err != nil || !r2.Cached || r2.Next != r1.Next || conditional != 1 {
		t.Fatalf("etag: %v %+v conditional=%d", err, r2, conditional)
	}
	if _, err := get("/missing", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404: %v", err)
	}

	remaining = 13 // → 12 after the next call = the reserve (20% of 60)
	if _, err := get("/x", false); err != nil {
		t.Fatal(err)
	}
	var rl *RateLimitedError
	if _, err := get("/y", false); !errors.As(err, &rl) {
		t.Fatalf("scheduler call should hit reserve, got %v", err)
	}
	if b, known := c.Budget(); b != 0 || !known {
		t.Fatalf("budget %d %v", b, known)
	}
	if _, err := get("/y", true); err != nil {
		t.Fatalf("manual call may use reserve: %v", err)
	}
	cfg.Token = "t" // token change drops the stale quota
	if _, err := get("/z", false); err != nil {
		t.Fatalf("after token change: %v", err)
	}
}

func TestParseRate(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h := http.Header{}
	h.Set("RateLimit-Limit", "500")
	h.Set("RateLimit-Remaining", "499")
	h.Set("RateLimit-Reset", "1800000060")
	if r, ok := parseRate(h, now); !ok || r.Limit != 500 || r.Remaining != 499 || !r.Reset.Equal(now.Add(time.Minute)) {
		t.Fatalf("gitlab: %+v", r)
	}
	h = http.Header{}
	h.Set("RateLimit", `"baseline";r=1997;t=600`)
	h.Set("RateLimit-Policy", `"baseline";q=2000;w=600`)
	if r, ok := parseRate(h, now); !ok || r.Limit != 2000 || r.Remaining != 1997 || !r.Reset.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("ietf: %+v", r)
	}
	h = http.Header{}
	h.Set("X-RateLimit-Resource", "search")
	h.Set("X-RateLimit-Limit", "10")
	if _, ok := parseRate(h, now); ok {
		t.Fatal("non-core github resource must be ignored")
	}
}

func TestBackgroundPacing(t *testing.T) {
	c := New("t", &memCache{m: map[string][3]string{}}, func() Config { return Config{ReservePct: 20} })
	if !c.Background() {
		t.Fatal("unknown quota: background work may try")
	}
	// 60/h, 30 left, reset in 30 min: floor = 12 + 6 → 12 spare → one every 2.5 min
	c.SetRate(Rate{Limit: 60, Remaining: 30, Reset: time.Now().Add(30 * time.Minute), Known: true})
	if !c.Background() {
		t.Fatal("first background call should pass")
	}
	if c.Background() {
		t.Fatal("second call right away must wait for its slot")
	}
	c.SetRate(Rate{Limit: 60, Remaining: 18, Reset: time.Now().Add(time.Minute), Known: true})
	c.lastBg = time.Time{}
	if c.Background() {
		t.Fatal("the last 10% above the reserve belong to discovery")
	}
	c.SetRate(Rate{Limit: 5000, Remaining: 4000, Reset: time.Now().Add(time.Hour), Known: true})
	c.lastBg = time.Now().Add(-2 * time.Second)
	if !c.Background() {
		t.Fatal("with a token the spacing is about a second")
	}
}

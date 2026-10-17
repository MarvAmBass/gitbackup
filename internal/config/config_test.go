package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitbackup.yaml")
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "# gitbackup configuration") || !strings.Contains(string(b), "check:\n      every: 12h") {
		t.Fatalf("default file:\n%s", b)
	}

	// a hand edit with comments
	edited := strings.Replace(string(b), "  - name: high\n", "  # my favourite plan\n  - name: high # fast lane\n", 1)
	edited = strings.Replace(edited, "settings:\n", "settings: # global\n", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls int
	f.OnChange(func(*Config) { calls++ })
	ctx, cancel := context.WithCancel(context.Background())
	go f.Watch(ctx, 10*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for calls == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls == 0 {
		t.Fatal("hand edit not picked up")
	}

	// UI save keeps comments
	err = f.Update(func(c *Config) error {
		c.EnsureInstance("gitlab.com", "gitlab", "https://gitlab.com")
		c.Sources = append(c.Sources, Source{Target: "example-user", Platform: "github", Host: "github.com", Kind: "account", Plan: "default", Repos: true})
		c.SetOverride(Override{Repo: "github.com/pi-hole/repos/pi-hole", Priority: "high", Interval: Duration(6 * time.Hour)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	for _, want := range []string{"# my favourite plan", "# fast lane", "# global", "# gitbackup configuration",
		"token: ${GITLAB_TOKEN}", "interval: 6h", "target: example-user"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("saved file lacks %q:\n%s", want, b)
		}
	}

	// invalid hand edit: ignored, error reported, last good config kept
	os.WriteFile(path, []byte("plans:\n  - name: x\n    priority: urgent\n"), 0o600)
	deadline = time.Now().Add(2 * time.Second)
	for f.Error() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if f.Error() == nil || !strings.Contains(f.Error().Error(), "priority") {
		t.Fatalf("expected load error, got %v", f.Error())
	}
	if len(f.Get().Sources) != 1 {
		t.Fatal("last good config was not kept")
	}

	// validation through Update
	if err := f.Update(func(c *Config) error { c.Sources[0].Plan = "nope"; return nil }); err == nil {
		t.Fatal("unknown plan accepted")
	}
}

func TestDurationAndHooks(t *testing.T) {
	for in, want := range map[string]string{"90": "90s", "500ms": "500ms", "30m": "30m", "36h": "36h", "2d": "2d", "1w": "1w", "14d": "2w", "0": "0"} {
		d, err := ParseDuration(in)
		if err != nil || d.String() != want {
			t.Errorf("%s → %s (%v), want %s", in, d, err, want)
		}
	}
	h := Hook{Events: []string{"history"}}
	if !h.Matches("rewritten") || !h.Matches("asset-replaced") || h.Matches("updated") {
		t.Error("history group")
	}
	t.Setenv("X_TOKEN", "secret")
	if (Instance{Token: "${X_TOKEN}"}).TokenValue() != "secret" || !(Instance{Token: "${X_TOKEN}"}).TokenFromEnv() {
		t.Error("env expansion")
	}
}

func TestOldStagitKeysMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitbackup.yaml")
	old := `settings:
  git_workers: 4
  stagit: true
  renderer: streagit
plans:
  - name: default
    priority: normal
    discover_every: 12h
    check: {every: 12h, min: 2h, max: 1w, adaptive: true}
    releases: meta
    stagit:
      on_change: true
      every: 1w
`
	os.WriteFile(path, []byte(old), 0o600)
	f, err := Open(path)
	if err != nil {
		t.Fatalf("a config from the stagit era must still load: %v", err)
	}
	if !f.Get().Settings.HTML {
		t.Fatal("stagit: true must become html: true")
	}
	if err := f.Update(func(c *Config) error { return nil }); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "stagit") || strings.Contains(string(b), "renderer") || !strings.Contains(string(b), "html: true") {
		t.Fatalf("obsolete keys should be gone after a save:\n%s", b)
	}
}

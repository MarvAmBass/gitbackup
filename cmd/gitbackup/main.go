// gitbackup – history-preserving backups of git hosting platforms.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/MarvAmBass/gitbackup/internal/auth"
	"github.com/MarvAmBass/gitbackup/internal/config"
	"github.com/MarvAmBass/gitbackup/internal/engine"
	"github.com/MarvAmBass/gitbackup/internal/pages"
	"github.com/MarvAmBass/gitbackup/internal/provider"
	"github.com/MarvAmBass/gitbackup/internal/releases"
	"github.com/MarvAmBass/gitbackup/internal/server"
	"github.com/MarvAmBass/gitbackup/internal/store"
	"github.com/MarvAmBass/gitbackup/internal/streagit"
	"github.com/MarvAmBass/gitbackup/internal/web"
)

var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// Layout of GITBACKUP_HOME (/data in the container):
//
//	gitbackup.yaml   all configuration (UI + hand edits)
//	state/           database (schedule, events, API cache) and scratch space
//	data/            everything browsable/downloadable, served at /browse/
func run(log *slog.Logger) error {
	home := env("GITBACKUP_HOME", "/data")
	dataDir := filepath.Join(home, "data")
	stateDir := filepath.Join(home, "state")
	for _, d := range []string{dataDir, stateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	cfg, err := config.Open(env("GITBACKUP_CONFIG", filepath.Join(home, "gitbackup.yaml")))
	if err != nil {
		return err
	}
	st, err := store.Open(env("GITBACKUP_DB", filepath.Join(stateDir, "gitbackup.db")), env("GITBACKUP_DB_JOURNAL", "wal"))
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.SyncConfig(cfg.Get()); err != nil {
		return fmt.Errorf("apply %s: %w", cfg.Path(), err)
	}

	forges := provider.NewRegistry(st, func(host string) (provider.Instance, bool) {
		in, ok := cfg.Get().Instance(host)
		return provider.Instance{Host: in.Host, Platform: provider.Platform(in.Platform), BaseURL: in.BaseURL,
			Token: in.TokenValue(), ReservePct: in.Reserve, MinGapMS: int(time.Duration(in.Gap) / time.Millisecond)}, ok
	})
	rs := &releases.Syncer{Store: st, DataDir: dataDir, HTTP: &http.Client{Timeout: 6 * time.Hour}}
	pb := &pages.Builder{DataDir: dataDir, AssetsDir: env("GITBACKUP_ASSETS", "/usr/local/share/gitbackup/assets")}
	eng := engine.New(st, cfg, forges, rs, pb, dataDir, stateDir, log)
	httpsUpdates := make(chan config.HTTPS, 1)
	cfg.OnChange(func(c *config.Config) {
		if err := st.SyncConfig(c); err != nil {
			log.Error("apply config", "err", err)
			return
		}
		log.Info("configuration applied", "file", cfg.Path())
		eng.MarkIndexDirty()
		select { // latest HTTPS settings win
		case <-httpsUpdates:
		default:
		}
		httpsUpdates <- c.Settings.HTTPS
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go cfg.Watch(ctx, 5*time.Second)

	// No password means no login. That is only allowed on loopback or when
	// asked for explicitly: anyone with UI access can add shell hooks.
	listen := env("GITBACKUP_LISTEN", ":8080")
	password := os.Getenv("GITBACKUP_ADMIN_PASSWORD")
	noAuth := os.Getenv("GITBACKUP_NO_AUTH") == "1"
	warning, err := auth.StartupCheck(password, noAuth, listen)
	if err != nil {
		return err
	}
	if warning != "" {
		log.Warn(warning)
	}
	login, err := auth.New(env("GITBACKUP_ADMIN_USER", "admin"), password, filepath.Join(stateDir, "session.key"))
	if err != nil {
		return err
	}
	// without a login only because of loopback: a separate HTTPS port must not open it up
	ws := &server.Server{Listen: listen, StateDir: stateDir, Log: log, LoopbackOnly: password == "" && !noAuth}
	// streagit renders HTML pages on demand (instead of pre-rendering with stagit)
	renderer := &streagit.Process{
		Binary: env("GITBACKUP_STREAGIT", "streagit"), Listen: env("GITBACKUP_STREAGIT_LISTEN", "127.0.0.1:8081"),
		Root: dataDir, Prefix: "/browse", Cache: filepath.Join(stateDir, "streagit"),
		Public: os.Getenv("GITBACKUP_BROWSE_URL"), Log: log,
	}
	if renderer.Available() {
		go renderer.Run(ctx)
	} else {
		log.Warn("streagit binary not found – history pages are unavailable", "binary", renderer.Binary)
	}
	srv := &web.Server{
		Store: st, Engine: eng, Forges: forges, Cfg: cfg, DataDir: dataDir, Auth: login,
		Version: version, Log: log, BaseCtx: ctx, TLSState: ws.State, Streagit: renderer,
	}
	h, err := srv.Handler()
	if err != nil {
		return err
	}
	ws.Handler = h

	done := make(chan struct{})
	go func() {
		eng.Run(ctx)
		close(done)
	}()

	log.Info("gitbackup started", "version", version, "listen", listen, "home", home, "config", cfg.Path(),
		"login", login.Enabled(), "https", cfg.Get().Settings.HTTPS.Mode, "streagit", renderer.Available())
	if err := ws.Run(ctx, cfg.Get().Settings.HTTPS, httpsUpdates); err != nil {
		stop()
		<-done
		return err
	}
	log.Info("shutting down, waiting for running git jobs")
	<-done
	return nil
}

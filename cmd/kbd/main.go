// Command kbd is the KB Core daemon: it loads config, opens and migrates the
// database, runs the enrichment job loop, and serves the HTTP API until a
// shutdown signal.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"kb/internal/api"
	"kb/internal/config"
	"kb/internal/events"
	"kb/internal/jobs"
	"kb/internal/store"
)

// Version is the build version reported by /healthz (override with -ldflags).
var Version = "0.0.0-dev"

func main() {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		log.Fatalf("kbd: config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Fatalf("kbd: listen on %s: %v", cfg.ListenAddr, err)
	}

	if err := serve(ctx, ln, cfg); err != nil {
		log.Fatalf("kbd: %v", err)
	}
}

// serve assembles the daemon over an already-bound listener (injected so it is
// testable) and runs until ctx is cancelled, then shuts the HTTP server down
// gracefully.
func serve(ctx context.Context, ln net.Listener, cfg *config.Config) error {
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := store.Migrate(db); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	st := store.New(db)
	bus := events.NewBus()

	runner := jobs.NewRunner(st)
	// Placeholder until Phase 2 wires the real enrichment pipeline; for now an
	// ingest job simply completes so the walking skeleton runs end to end.
	runner.Handle("ingest", func(context.Context, store.Job) error { return nil })
	go func() { _ = runner.Run(ctx, time.Second) }()

	srv := api.NewServer(api.Deps{
		Store:          st,
		Bus:            bus,
		Token:          cfg.APIToken,
		DefaultPrivacy: cfg.DefaultPrivacy,
		Build:          api.BuildInfo{Version: Version},
	})
	httpSrv := &http.Server{Handler: srv.Handler()}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("kbd: serving on %s", ln.Addr())
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

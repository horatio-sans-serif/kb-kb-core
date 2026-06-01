// Command kbd is the KB Core daemon: it loads config, opens and migrates the
// database, runs the enrichment job loop, and serves the HTTP API until a
// shutdown signal.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"kb/internal/api"
	"kb/internal/config"
	"kb/internal/enrich"
	"kb/internal/events"
	"kb/internal/fetch"
	"kb/internal/jobs"
	"kb/internal/llm"
	"kb/internal/pipeline"
	"kb/internal/store"
)

// Version is the build version reported by /healthz (override with -ldflags).
var Version = "0.0.0-dev"

// jobRetention is how long terminal (done/failed) jobs are kept before purge.
const jobRetention = 7 * 24 * time.Hour

func main() {
	syscall.Umask(0o077) // daemon-created files (DB, WAL, SHM) are owner-only

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel(os.Getenv("KB_LOG_LEVEL")),
	}))
	slog.SetDefault(logger)

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		logger.Error("listen", "addr", cfg.ListenAddr, "err", err)
		os.Exit(1)
	}

	if err := serve(ctx, ln, cfg, logger); err != nil {
		logger.Error("kbd exited with error", "err", err)
		os.Exit(1)
	}
}

func logLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// serve assembles the daemon over an already-bound listener (injected so it is
// testable) and runs until ctx is cancelled, then shuts the HTTP server down
// gracefully.
func serve(ctx context.Context, ln net.Listener, cfg *config.Config, logger *slog.Logger) error {
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := store.Migrate(db); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	rdb, err := store.OpenReadOnly(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open read pool: %w", err)
	}
	defer rdb.Close()

	st := store.NewWithReadPool(db, rdb)
	bus := events.NewBus()

	runner := jobs.NewRunner(st, jobs.WithLogger(logger))

	// The enrichment pipeline: each ingested artifact is expanded into its stages.
	// fetch retrieves the body; summarize runs a local Ollama model over it to
	// produce summary/tags/critical-analysis enrichment rows (design §8.5).
	if !cfg.OllamaIsLocal {
		logger.Warn("Ollama endpoint is not loopback; sensitive/restricted artifacts will skip summarization",
			"url", cfg.OllamaURL)
	}
	llmClient := llm.NewOllama(cfg.OllamaURL)
	enrichment := pipeline.New()
	enrichment.Register(enrich.NewFetch(fetch.New()))
	enrichment.Register(enrich.NewSummarize(llmClient, cfg.SummarizeModel, cfg.OllamaIsLocal))
	enrichment.Register(enrich.NewEmbed(llmClient, cfg.EmbedModel, cfg.OllamaIsLocal))
	enrichment.RegisterHandlers(st, runner, bus)

	// Startup maintenance: reclaim crash-orphaned jobs (fatal on failure — do not
	// serve with a dead loop), then purge old terminal jobs (best-effort).
	if _, err := runner.ReclaimStuck(ctx); err != nil {
		return fmt.Errorf("reclaim stuck jobs: %w", err)
	}
	cutoff := store.FormatTime(time.Now().Add(-jobRetention))
	if n, err := st.Jobs.Purge(ctx, cutoff); err != nil {
		logger.Warn("purge old jobs", "err", err)
	} else if n > 0 {
		logger.Info("purged old jobs", "count", n)
	}

	// Wake the runner on each event (a fresh job was enqueued) so it drains
	// promptly instead of polling the single writer; a coarse fallback tick
	// covers backoff-rescheduled jobs.
	wake := make(chan struct{}, 1)
	sub, unsub := bus.Subscribe()
	defer unsub()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-sub:
				if !ok {
					return
				}
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}
	}()

	runErr := make(chan error, 1)
	go func() { runErr <- runner.Run(ctx, wake, 30*time.Second) }()

	srv := api.NewServer(api.Deps{
		Store:          st,
		Bus:            bus,
		Token:          cfg.APIToken,
		DefaultPrivacy: cfg.DefaultPrivacy,
		Build:          api.BuildInfo{Version: Version},
		Logger:         logger,
		Enqueue:        enrichment,
	})
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,   // slowloris guard; safe for SSE
		ReadTimeout:       15 * time.Second,  // full request incl. /ingest body
		IdleTimeout:       120 * time.Second, // reclaim idle keep-alives
		// WriteTimeout deliberately unset: GET /events streams indefinitely and a
		// server-wide write deadline would sever it. Bound writes per-handler if
		// the non-streaming routes ever need it.
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	logger.Info("serving", "addr", ln.Addr().String())
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	// Wait for the runner to observe ctx cancellation and exit cleanly.
	if err := <-runErr; err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("runner exited", "err", err)
	}
	return nil
}

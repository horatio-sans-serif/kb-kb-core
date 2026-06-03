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
	"kb/internal/decay"
	"kb/internal/enrich"
	"kb/internal/events"
	"kb/internal/fetch"
	"kb/internal/ingest"
	"kb/internal/jobs"
	"kb/internal/llm"
	"kb/internal/pipeline"
	"kb/internal/rag"
	"kb/internal/relevance"
	"kb/internal/relsignals"
	"kb/internal/render"
	"kb/internal/search"
	"kb/internal/sources"
	"kb/internal/sources/rss"
	"kb/internal/ssrf"
	"kb/internal/store"
	"kb/internal/surface"
)

// Version is the build version reported by /healthz (override with -ldflags).
var Version = "0.0.0-dev"

// jobRetention is how long terminal (done/failed) jobs are kept before purge.
const jobRetention = 7 * 24 * time.Hour

// decayInterval is how often the relevance decay sweep re-scores all artifacts.
const decayInterval = 6 * time.Hour

// sourcePollInterval is how often the scheduler checks for PULL sources due to
// refresh (each source's own schedule decides whether it actually fetches).
const sourcePollInterval = time.Minute

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
		logger.Warn("Ollama endpoint is not loopback; sensitive/restricted artifacts will skip summarization/embedding",
			"url", cfg.OllamaURL)
	}
	llmClient := llm.NewOllama(cfg.OllamaURL)

	// Build the relevance profile by embedding the configured interest phrases.
	// Best-effort: if the embed model is unreachable at startup, fall back to a
	// neutral (empty) profile so the daemon still serves and enriches.
	embedFn := func(ctx context.Context, text string) ([]float32, error) {
		return llmClient.Embed(ctx, llm.EmbedRequest{Model: cfg.EmbedModel, Input: text})
	}
	profile, err := relevance.BuildProfile(ctx, embedFn, cfg.Interests)
	if err != nil {
		logger.Warn("building relevance profile failed; using a neutral profile", "err", err)
		profile = relevance.NewProfile(nil)
	}

	enrichment := pipeline.New()
	enrichment.Register(enrich.NewFetch(fetch.New()))
	enrichment.Register(enrich.NewSummarize(llmClient, cfg.SummarizeModel, cfg.OllamaIsLocal))
	enrichment.Register(enrich.NewEmbed(llmClient, cfg.EmbedModel, cfg.OllamaIsLocal))
	enrichment.Register(enrich.NewRelevance(relsignals.New(st), profile))
	enrichment.SetRescoreStage(enrich.RelevanceStage) // re-scores re-run the relevance stage
	enrichment.RegisterHandlers(st, runner, bus)

	// Startup maintenance: reclaim crash-orphaned jobs (fatal on failure — do not
	// serve with a dead loop), reconcile artifacts whose finalize was skipped by a
	// crash (after reclaim, so a re-pending job keeps its artifact out of the
	// sweep), then purge old terminal jobs (best-effort).
	if _, err := runner.ReclaimStuck(ctx); err != nil {
		return fmt.Errorf("reclaim stuck jobs: %w", err)
	}
	if err := enrichment.Reconcile(ctx, st, bus); err != nil {
		logger.Warn("reconcile enriching artifacts", "err", err)
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

	// Relevance decay: periodically re-score every artifact in place so time-based
	// recency decay takes effect (the score otherwise only recomputes on
	// ingest/engage/re-encounter). Best-effort and patch-only (no enrichment rows).
	sweeper := decay.NewSweeper(st, profile)
	runSweep := func() {
		switch n, err := sweeper.Sweep(ctx); {
		case err != nil && ctx.Err() == nil:
			logger.Warn("relevance decay sweep", "err", err)
		case n > 0:
			logger.Info("relevance decay sweep", "rescored", n)
		}
	}
	go func() {
		runSweep() // once at startup: fix scores that went stale across downtime
		t := time.NewTicker(decayInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runSweep()
			}
		}
	}()

	// Re-score = re-run the relevance stage standalone (no deps; the embedding
	// already exists). Triggered by a fresh encounter or an engagement (§9).
	// Coalesce: skip if a re-score for this artifact is already queued.
	rescore := func(ctx context.Context, tx *store.Store, artifactID string) error {
		has, err := tx.Jobs.HasUnfinishedType(ctx, artifactID, pipeline.RescoreJobType)
		if err != nil || has {
			return err
		}
		_, err = tx.Jobs.Enqueue(ctx, store.EnqueueSpec{Type: pipeline.RescoreJobType, ArtifactID: &artifactID})
		return err
	}

	// PULL sources: register connectors and poll due sources on a ticker, ingesting
	// new items through the same shared path as push ingest.
	registry := sources.NewRegistry()
	registry.Register("rss", rss.New(ssrf.HTTPClient(30*time.Second)))
	ingester := ingest.New(st, enrichment, rescore, bus)
	scheduler := sources.NewScheduler(st, registry, ingester, logger)
	go scheduler.Run(ctx, sourcePollInterval)

	// Retrieval: hybrid search, and RAG question-answering grounded on it. Both
	// take the daemon's locality so the privacy gate extends to query embedding
	// (search) and answer synthesis (rag).
	searcher := search.NewSearcher(st, llmClient, cfg.EmbedModel, cfg.OllamaIsLocal)

	// Output/render layer (§8.14): one Renderer per format over the same assembled
	// Markdown. The Markdown renderer is dependency-free; the pandoc-backed formats
	// need the pandoc binary (warn if absent — the route then answers 503), and
	// patat decks are presented with the patat binary the user runs separately.
	// Create the render output directory up front (owner-only) so a misconfigured
	// path fails at boot, not as a 500 on the first render. config.Load always sets
	// RenderDir; the empty guard is for a directly-constructed Config (tests), where
	// rendering is simply unused.
	if cfg.RenderDir != "" {
		if err := os.MkdirAll(cfg.RenderDir, 0o700); err != nil {
			return fmt.Errorf("create render dir %q: %w", cfg.RenderDir, err)
		}
	}
	renderReg := render.NewRegistry()
	renderReg.Register(render.NewMarkdown(cfg.RenderDir))
	renderReg.Register(render.NewPatat(cfg.RenderDir))
	for _, format := range []string{"html", "pdf", "docx", "epub"} {
		pr, err := render.NewPandoc(format, cfg.RenderDir)
		if err != nil {
			return fmt.Errorf("render: %w", err)
		}
		renderReg.Register(pr)
	}
	if !render.PandocAvailable() {
		logger.Warn("pandoc not found on PATH; html/pdf/docx/epub renders will return 503 until it is installed")
	}
	renderSvc := render.NewService(st, renderReg)

	srv := api.NewServer(api.Deps{
		Store:          st,
		Bus:            bus,
		Token:          cfg.APIToken,
		DefaultPrivacy: cfg.DefaultPrivacy,
		Build:          api.BuildInfo{Version: Version},
		Logger:         logger,
		Enqueue:        enrichment,
		Search:         searcher,
		Ask:            rag.NewEngine(searcher, st, llmClient, cfg.SummarizeModel, cfg.OllamaIsLocal),
		Rescore:        rescore,
		Connectors:     registry,
		Render:         renderSvc,
		Surface:        surface.New(st),
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

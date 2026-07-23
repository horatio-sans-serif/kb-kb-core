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
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"database/sql"

	"kb/internal/api"
	"kb/internal/config"
	"kb/internal/crsync"
	"kb/internal/decay"
	"kb/internal/enrich"
	"kb/internal/events"
	"kb/internal/fetch"
	"kb/internal/ingest"
	"kb/internal/jobs"
	"kb/internal/kb"
	"kb/internal/lifecycle"
	"kb/internal/llm"
	"kb/internal/pipeline"
	"kb/internal/rag"
	"kb/internal/reconcile"
	"kb/internal/relevance"
	"kb/internal/relsignals"
	"kb/internal/render"
	"kb/internal/review"
	"kb/internal/search"
	"kb/internal/sources"
	"kb/internal/sources/github"
	"kb/internal/sources/hn"
	"kb/internal/sources/mastodon"
	"kb/internal/sources/rss"
	"kb/internal/ssrf"
	"kb/internal/store"
	"kb/internal/surface"
	"kb/internal/topics"
	"kb/internal/vecsearch"
)

// Version is the build version reported by /healthz (override with -ldflags).
var Version = "0.0.0-dev"

// jobRetention is how long terminal (done/failed) jobs are kept before purge.
const jobRetention = 7 * 24 * time.Hour

// decayInterval is how often the relevance decay sweep re-scores all artifacts.
const decayInterval = 6 * time.Hour

// clusterInterval is how often the topic-clustering sweep groups newly-embedded
// artifacts into topics.
const clusterInterval = 5 * time.Minute

// lifecycleInterval is how often the lifecycle sweep transitions artifacts
// active -> archive -> purge.
const lifecycleInterval = 6 * time.Hour

// reconcileInterval is how often the reconcile sweep merges cross-replica
// duplicate artifacts.
const reconcileInterval = 10 * time.Minute

// syncPullInterval is how often the puller pulls changesets from configured peers
// (when multi-device sync is enabled).
const syncPullInterval = 30 * time.Second

// vecReconcileInterval is how often the vec0 ANN index is reconciled from the
// embeddings (a count guard makes a no-change reconcile cheap).
const vecReconcileInterval = 2 * time.Minute

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

// openWriteDB opens the single-writer pool. When a cr-sqlite extension is configured
// and loads (the daemon was built with the `crsqlite` tag), the pool uses the
// replication driver and the second return is true; otherwise it falls back to the
// plain driver and runs single-node. A configured-but-unavailable extension is a
// warning, not a failure, so the daemon still serves.
func openWriteDB(cfg *config.Config, logger *slog.Logger) (*sql.DB, bool, error) {
	driver, err := crsync.Register(cfg.CRSqlitePath)
	if err != nil {
		if cfg.CRSqlitePath != "" {
			logger.Warn("cr-sqlite configured but unavailable; running single-node",
				"path", cfg.CRSqlitePath, "err", err)
		}
		db, oerr := store.Open(cfg.DBPath)
		return db, false, oerr
	}
	db, oerr := store.OpenWithDriver(cfg.DBPath, driver)
	if oerr != nil {
		return nil, false, oerr
	}
	return db, true, nil
}

// warnInsecurePeers logs a warning for any sync peer reached over plaintext http://
// whose host is not loopback or a Tailscale address (100.64.0.0/10, fd7a:115c:a1e0::/48).
// The bearer token crosses such a link in the clear; the intended deployment is
// https or http-over-Tailscale (WireGuard-encrypted). A hostname (not an IP) cannot
// be verified, so it is warned too.
func warnInsecurePeers(peers []string, logger *slog.Logger) {
	for _, peer := range peers {
		u, err := url.Parse(peer)
		if err != nil {
			logger.Warn("sync peer URL is unparseable", "peer", peer, "err", err)
			continue
		}
		if u.Scheme == "https" {
			continue
		}
		ip := net.ParseIP(u.Hostname())
		if ip != nil && (ip.IsLoopback() || isTailscale(ip)) {
			continue
		}
		logger.Warn("sync peer uses plaintext http and is not loopback/Tailscale; the API token crosses the network in the clear — use https or http-over-Tailscale",
			"peer", peer)
	}
}

// isTailscale reports whether ip is in Tailscale's CGNAT range (100.64.0.0/10) or
// its ULA range (fd7a:115c:a1e0::/48).
func isTailscale(ip net.IP) bool {
	for _, cidr := range []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"} {
		_, n, _ := net.ParseCIDR(cidr)
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// serve assembles the daemon over an already-bound listener (injected so it is
// testable) and runs until ctx is cancelled, then shuts the HTTP server down
// gracefully.
func serve(ctx context.Context, ln net.Listener, cfg *config.Config, logger *slog.Logger) error {
	// Register the vec0 ANN extension (no-op in a default build) BEFORE any pool
	// opens, so every connection — read, write, and the cr-sqlite driver — carries it.
	vecsearch.Enable()

	db, syncEnabled, err := openWriteDB(cfg, logger)
	if err != nil {
		return err
	}
	defer db.Close()
	if syncEnabled {
		// crsql_finalize must run before the pool closes; deferred after Close so it
		// runs first (LIFO).
		defer func() {
			if ferr := crsync.Finalize(context.Background(), db); ferr != nil {
				logger.Warn("cr-sqlite finalize", "err", ferr)
			}
		}()
	}
	if _, err := store.Migrate(db); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if syncEnabled {
		// Convert the synced tables to CRRs (idempotent; safe every startup) so writes
		// produce a changeset peers can merge.
		if err := crsync.AsCRR(ctx, db, crsync.SyncedTables()); err != nil {
			return fmt.Errorf("cr-sqlite convert tables: %w", err)
		}
		logger.Info("multi-device sync enabled", "peers", len(cfg.SyncPeers))
	}

	rdb, err := store.OpenReadOnly(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open read pool: %w", err)
	}
	defer rdb.Close()

	st := store.NewWithReadPool(db, rdb)

	// vec0 ANN index (when built with the `vec0` tag): a local, rebuilt KNN index over
	// the embedding enrichments that makes semantic search sublinear. Absent it,
	// search uses the exact brute-force scan. The index is reconciled from the
	// embeddings at startup and on a ticker (a count guard skips no-change rebuilds).
	var vecIndex vecsearch.Index
	if idx, err := vecsearch.New(db, rdb, cfg.EmbedDim); err != nil {
		if !errors.Is(err, vecsearch.ErrUnavailable) {
			return fmt.Errorf("vec0 index: %w", err)
		}
	} else {
		vecIndex = idx
		logger.Info("vec0 ANN index enabled", "embed_dim", cfg.EmbedDim)
		runVecReconcile := func() {
			switch n, err := vecIndex.Reconcile(ctx, st.Enrichments); {
			case err != nil && ctx.Err() == nil:
				logger.Warn("vec0 reconcile", "err", err)
			case n > 0:
				logger.Info("vec0 reconcile", "indexed", n)
			}
		}
		go func() {
			runVecReconcile()
			t := time.NewTicker(vecReconcileInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					runVecReconcile()
				}
			}
		}()
	}
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
	enrichment.Register(enrich.NewEntities(llmClient, cfg.SummarizeModel, cfg.OllamaIsLocal))
	enrichment.Register(enrich.NewClaims(llmClient, llmClient, cfg.SummarizeModel, cfg.EmbedModel, cfg.OllamaIsLocal))
	enrichment.Register(enrich.NewConflicts(st.Claims, llmClient, cfg.SummarizeModel, cfg.OllamaIsLocal))
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

	// Topic clustering: periodically group ready, embedded artifacts into topics by
	// embedding similarity (incremental leader clustering). Runs more often than the
	// decay sweep — a freshly-enriched artifact should be clustered promptly.
	// LLM-derived topic labels (§8.7): when a topic is founded, replace its
	// title-derived label with a concise model-generated one. Best-effort and
	// privacy-gated — skipped entirely when the endpoint is not on-device.
	clusterer := topics.New(st).WithLabeler(
		topics.NewLabeler(st, llmClient, cfg.SummarizeModel, cfg.OllamaIsLocal))
	runCluster := func() {
		switch n, err := clusterer.Sweep(ctx); {
		case err != nil && ctx.Err() == nil:
			logger.Warn("topic clustering sweep", "err", err)
		case n > 0:
			logger.Info("topic clustering sweep", "clustered", n)
		}
	}
	go func() {
		runCluster()
		t := time.NewTicker(clusterInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runCluster()
			}
		}
	}()

	// Re-score = re-run the relevance stage standalone (no deps; the embedding
	// already exists). Triggered by a fresh encounter, an engagement (§9), or a
	// reconcile merge (the survivor inherits the loser's encounters). Coalesce: skip
	// if a re-score for this artifact is already queued.
	rescore := func(ctx context.Context, tx *store.Store, artifactID string) error {
		has, err := tx.Jobs.HasUnfinishedType(ctx, artifactID, pipeline.RescoreJobType)
		if err != nil || has {
			return err
		}
		_, err = tx.Jobs.Enqueue(ctx, store.EnqueueSpec{Type: pipeline.RescoreJobType, ArtifactID: &artifactID})
		return err
	}

	// Reconcile: merge cross-replica duplicate artifacts (the same_as dedup after
	// sync, §8.2/§8.16). Single-node it finds nothing; once peers sync, it collapses
	// duplicates created offline into one survivor, re-scoring each survivor whose
	// inputs the merge changed.
	reconciler := reconcile.New(st, rescore)
	runReconcile := func() {
		switch n, err := reconciler.Sweep(ctx); {
		case err != nil && ctx.Err() == nil:
			logger.Warn("artifact reconcile sweep", "err", err)
		case n > 0:
			logger.Info("artifact reconcile sweep", "merged", n)
		}
	}
	go func() {
		runReconcile()
		t := time.NewTicker(reconcileInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runReconcile()
			}
		}
	}()

	// Lifecycle GC: periodically transition artifacts active -> archive -> purge by
	// relevance and age (§8.12). Runs alongside the decay sweep, which keeps the
	// scores it acts on fresh. Compaction physically reclaims purged artifacts after
	// the retention window (KB_PURGE_RETENTION_DAYS); the hard deletes replicate
	// cleanly under cr-sqlite (§8.16).
	lifePolicy := lifecycle.DefaultPolicy()
	lifePolicy.CompactAfter = time.Duration(cfg.PurgeRetention) * 24 * time.Hour
	lifePolicy.AuditRetention = time.Duration(cfg.AuditRetention) * 24 * time.Hour
	lifeSweeper := lifecycle.NewWithPolicy(st, lifePolicy, time.Now)
	runLifecycle := func() {
		switch stats, err := lifeSweeper.Sweep(ctx); {
		case err != nil && ctx.Err() == nil:
			logger.Warn("lifecycle sweep", "err", err)
		case stats.Archived > 0 || stats.Purged > 0:
			logger.Info("lifecycle sweep", "archived", stats.Archived, "purged", stats.Purged)
		}
		switch n, err := lifeSweeper.EnforceBudgets(ctx); {
		case err != nil && ctx.Err() == nil:
			logger.Warn("storage budget enforcement", "err", err)
		case n > 0:
			logger.Info("storage budget enforcement", "archived", n)
		}
		switch n, err := lifeSweeper.Compact(ctx); {
		case err != nil && ctx.Err() == nil:
			logger.Warn("purge compaction", "err", err)
		case n > 0:
			logger.Info("purge compaction", "reclaimed", n)
		}
	}
	go func() {
		runLifecycle()
		t := time.NewTicker(lifecycleInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runLifecycle()
			}
		}
	}()

	// PULL sources: register connectors and poll due sources on a ticker, ingesting
	// new items through the same shared path as push ingest.
	registry := sources.NewRegistry()
	registry.Register("rss", rss.New(ssrf.HTTPClient(30*time.Second)))
	registry.Register("hackernews", hn.New(ssrf.HTTPClient(30*time.Second)))
	registry.Register("github", github.New(ssrf.HTTPClient(30*time.Second)))
	registry.Register("mastodon", mastodon.New(ssrf.HTTPClient(30*time.Second)))
	ingester := ingest.New(st, enrichment, rescore, bus)
	scheduler := sources.NewScheduler(st, registry, ingester, bus, logger)
	go scheduler.Run(ctx, sourcePollInterval)

	// Retrieval: hybrid search, and RAG question-answering grounded on it. Both
	// take the daemon's locality so the privacy gate extends to query embedding
	// (search) and answer synthesis (rag).
	searcher := search.NewSearcher(st, llmClient, cfg.EmbedModel, cfg.OllamaIsLocal)
	if vecIndex != nil {
		searcher.SetIndex(vecIndex)
	}

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
		ReadToken:      cfg.ReadToken,
		ReadCeiling:    kb.PrivacyTier(cfg.ReadTier),
		DefaultPrivacy: cfg.DefaultPrivacy,
		Build:          api.BuildInfo{Version: Version},
		Logger:         logger,
		Enqueue:        enrichment,
		Search:         searcher,
		Ask:            rag.NewEngine(searcher, st, llmClient, llmClient, cfg.SummarizeModel, cfg.EmbedModel, cfg.OllamaIsLocal),
		Rescore:        rescore,
		Connectors:     registry,
		Render:         renderSvc,
		Surface:        surface.New(st),
		Review:         review.New(st),
	})
	// When sync is enabled, expose the changeset-exchange endpoints (so peers can
	// pull from us) and start the puller (to pull from configured peers). The /sync
	// routes carry the same bearer token as the rest of the API.
	handler := srv.Handler()
	if syncEnabled {
		cs := crsync.DBChangeStore{DB: db}
		mux := http.NewServeMux()
		mux.Handle("/sync/", http.StripPrefix("/sync", crsync.Handler(cs, cfg.APIToken, logger)))
		mux.Handle("/", handler)
		handler = mux
		if len(cfg.SyncPeers) > 0 {
			warnInsecurePeers(cfg.SyncPeers, logger)
			puller := crsync.NewPuller(cs, st.Sync, cfg.SyncPeers, cfg.APIToken, nil, logger)
			go puller.Run(ctx, syncPullInterval)
		}
	}

	httpSrv := &http.Server{
		Handler:           handler,
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

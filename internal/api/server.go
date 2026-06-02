package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"kb/internal/canon"
	"kb/internal/events"
	"kb/internal/kb"
	"kb/internal/rag"
	"kb/internal/search"
	"kb/internal/store"
)

// BuildInfo is reported by the health endpoint.
type BuildInfo struct {
	Version string `json:"version"`
}

// Enqueuer expands an ingested artifact into its enrichment-pipeline jobs within
// the caller's transaction (satisfied by *pipeline.Pipeline). When nil, ingest
// records the artifact and its encounter but starts no pipeline.
type Enqueuer interface {
	EnqueueTx(ctx context.Context, tx *store.Store, artifactID string) error
}

// Searcher ranks artifacts for a query (satisfied by *search.Searcher). When nil,
// the /search route reports 503. Semantic is meaning-based (embeds the query);
// Keyword is full-text (FTS5); Hybrid blends both (the default).
type Searcher interface {
	Semantic(ctx context.Context, query string, k int) ([]search.Result, error)
	Keyword(ctx context.Context, query string, k int) ([]search.Result, error)
	Hybrid(ctx context.Context, query string, k int) ([]search.Result, error)
}

// Asker answers a question over retrieved artifacts (satisfied by *rag.Engine).
// When nil, the /ask route reports 503.
type Asker interface {
	Answer(ctx context.Context, query string, k int) (rag.Answer, error)
}

// Deps are the server's collaborators and settings.
type Deps struct {
	Store          *store.Store
	Bus            *events.Bus
	Token          string
	DefaultPrivacy string
	Build          BuildInfo
	Logger         *slog.Logger
	Enqueue        Enqueuer
	Search         Searcher
	Ask            Asker
}

// Server wires the HTTP handlers.
type Server struct {
	deps Deps
}

func NewServer(d Deps) *Server {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Server{deps: d}
}

// Handler builds the routed, auth-wrapped HTTP handler. /healthz is public;
// /events and /ingest require a bearer token.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	// /events uses bearer-header auth, which suits local CLI tools. A browser
	// EventSource cannot set headers, so the Stage 10 web UI will need a
	// query-param/cookie token path scoped to this route (same constant-time
	// compare). Header auth is correct for Phase 0.
	mux.Handle("GET /events", s.requireAuth(SSEHandler(s.deps.Bus)))
	mux.Handle("POST /ingest", s.requireAuth(http.HandlerFunc(s.handleIngest)))
	mux.Handle("GET /search", s.requireAuth(http.HandlerFunc(s.handleSearch)))
	mux.Handle("GET /ask", s.requireAuth(http.HandlerFunc(s.handleAsk)))
	return mux
}

// requireAuth enforces a constant-time bearer-token check. The token is never
// logged or echoed; an empty configured token never authenticates.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	const prefix = "Bearer "
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		token := h[len(prefix):]
		if s.deps.Token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.deps.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.deps.Build.Version})
}

type ingestRequest struct {
	SourceID string `json:"source_id"`
	URL      string `json:"url,omitempty"`
	Body     string `json:"body,omitempty"`
	Title    string `json:"title,omitempty"`
	Privacy  string `json:"privacy,omitempty"` // optional per-artifact tier override
}

type ingestResponse struct {
	ArtifactID string `json:"artifact_id"`
}

const (
	maxIngestBytes      = 8 << 20 // 8 MiB body cap
	maxTitleBytes       = 4096
	maxSourceIDBytes    = 64
	maxSearchQueryBytes = 1024 // bound MATCH/embed work on the authenticated search route
)

func validPrivacy(t string) bool {
	switch t {
	case "public", "personal", "sensitive", "restricted":
		return true
	}
	return false
}

// validText rejects invalid UTF-8 and control characters (a stored-injection
// guard for the future renderers). Tab is always allowed; newlines/CR only when
// allowNewlines (the Markdown body, not the title).
func validText(s string, allowNewlines bool) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r == '\t', allowNewlines && (r == '\n' || r == '\r'):
			continue
		case r < 0x20 || r == 0x7f:
			return false
		}
	}
	return true
}

// handleIngest is the walking skeleton: it writes the artifact, its encounter,
// and an enrichment job atomically, then publishes artifact.ingested (only after
// the transaction commits, so a rolled-back ingest emits no event). The artifact
// privacy tier comes from an explicit request override, else the source's
// default, else the daemon default (design §8.17).
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxIngestBytes)
	var req ingestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	switch {
	case req.SourceID == "" || len(req.SourceID) > maxSourceIDBytes:
		http.Error(w, "source_id is required", http.StatusBadRequest)
		return
	case req.URL == "" && req.Body == "":
		http.Error(w, "url or body is required", http.StatusBadRequest)
		return
	case len(req.Title) > maxTitleBytes:
		http.Error(w, "title too long", http.StatusBadRequest)
		return
	case req.Privacy != "" && !validPrivacy(req.Privacy):
		http.Error(w, "invalid privacy tier", http.StatusBadRequest)
		return
	case !validText(req.Title, false) || !validText(req.Body, true):
		http.Error(w, "title/body contains invalid UTF-8 or control characters", http.StatusBadRequest)
		return
	}

	var canonicalURL string
	if req.URL != "" {
		c, err := canon.URL(req.URL)
		if err != nil {
			http.Error(w, "invalid url", http.StatusBadRequest)
			return
		}
		canonicalURL = c
	}

	ctx := r.Context()
	src, err := s.deps.Store.Sources.Get(ctx, req.SourceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown source_id", http.StatusBadRequest)
			return
		}
		s.serverError(w, "lookup source", err)
		return
	}

	// Privacy tier: explicit request override > source default > daemon default.
	tier := kb.PrivacyTier(s.deps.DefaultPrivacy)
	if src.DefaultPrivacy != "" {
		tier = src.DefaultPrivacy
	}
	if req.Privacy != "" {
		tier = kb.PrivacyTier(req.Privacy)
	}

	var artifactID string
	err = s.deps.Store.Tx(ctx, func(tx *store.Store) error {
		a := &store.Artifact{Title: req.Title, PrivacyTier: tier}
		var art *store.Artifact
		var err error
		if req.URL != "" {
			a.Type = "webpage"
			a.CanonicalURL = &canonicalURL // dedup by canonical URL
			art, err = tx.Artifacts.UpsertByCanonical(ctx, a)
		} else {
			a.Type = "note"
			a.BodyMarkdown = req.Body
			h := canon.Content(req.Body)
			a.ContentHash = &h // dedup by content fingerprint
			art, err = tx.Artifacts.UpsertByContentHash(ctx, a)
		}
		if err != nil {
			return err
		}
		artifactID = art.ID

		var rawURL *string
		if req.URL != "" {
			rawURL = &req.URL // preserve the as-seen URL on the encounter
		}
		if err := tx.Encounters.Record(ctx, &store.Encounter{
			ArtifactID: art.ID, SourceID: req.SourceID, RawURL: rawURL,
		}); err != nil {
			return err
		}
		// Only a fresh INSERT starts a pipeline. Re-ingesting a known artifact
		// records another encounter (a relevance signal, §8.9) but must not
		// re-run enrichment on unchanged content. The upsert leaves
		// created_at==updated_at only on insert; a re-sighting bumps updated_at.
		inserted := art.CreatedAt == art.UpdatedAt
		if inserted && s.deps.Enqueue != nil {
			return s.deps.Enqueue.EnqueueTx(ctx, tx, art.ID)
		}
		return nil
	})
	if err != nil {
		s.serverError(w, "ingest", err)
		return
	}

	s.deps.Bus.Publish(events.New(events.ArtifactIngested,
		map[string]string{"artifact_id": artifactID, "source_id": req.SourceID}))

	s.writeJSON(w, http.StatusOK, ingestResponse{ArtifactID: artifactID})
}

type searchResultJSON struct {
	ArtifactID string  `json:"artifact_id"`
	Title      string  `json:"title,omitempty"`
	URL        string  `json:"url,omitempty"`
	Score      float64 `json:"score"`
}

type searchResponse struct {
	Query   string             `json:"query"`
	Results []searchResultJSON `json:"results"`
}

// handleSearch ranks artifacts by semantic similarity to ?q and attaches each
// hit's title/url for display. ?k bounds the result count (the searcher caps it).
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if s.deps.Search == nil {
		http.Error(w, "search not configured", http.StatusServiceUnavailable)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return
	}
	if len(q) > maxSearchQueryBytes {
		http.Error(w, "query too long", http.StatusBadRequest)
		return
	}
	k := 0
	if ks := r.URL.Query().Get("k"); ks != "" {
		n, err := strconv.Atoi(ks)
		if err != nil || n < 0 {
			http.Error(w, "k must be a non-negative integer", http.StatusBadRequest)
			return
		}
		k = n
	}

	ctx := r.Context()
	var hits []search.Result
	var err error
	switch mode := r.URL.Query().Get("mode"); mode {
	case "", "hybrid":
		hits, err = s.deps.Search.Hybrid(ctx, q, k)
	case "semantic":
		hits, err = s.deps.Search.Semantic(ctx, q, k)
	case "keyword":
		hits, err = s.deps.Search.Keyword(ctx, q, k)
	default:
		http.Error(w, "mode must be 'hybrid', 'semantic', or 'keyword'", http.StatusBadRequest)
		return
	}
	if err != nil {
		if errors.Is(err, search.ErrRemoteDisallowed) {
			http.Error(w, "semantic search requires an on-device embed endpoint", http.StatusServiceUnavailable)
			return
		}
		s.serverError(w, "search", err)
		return
	}

	out := searchResponse{Query: q, Results: make([]searchResultJSON, 0, len(hits))}
	for _, h := range hits {
		a, err := s.deps.Store.Artifacts.Get(ctx, h.ArtifactID)
		if err != nil {
			continue // artifact hard-deleted / not found between ranking and load — drop
		}
		res := searchResultJSON{ArtifactID: h.ArtifactID, Title: a.Title, Score: h.Score}
		if a.CanonicalURL != nil {
			res.URL = *a.CanonicalURL
		}
		out.Results = append(out.Results, res)
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleAsk answers a question (RAG) over the user's artifacts with citations.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if s.deps.Ask == nil {
		http.Error(w, "question answering not configured", http.StatusServiceUnavailable)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return
	}
	if len(q) > maxSearchQueryBytes {
		http.Error(w, "query too long", http.StatusBadRequest)
		return
	}
	k := 0
	if ks := r.URL.Query().Get("k"); ks != "" {
		n, err := strconv.Atoi(ks)
		if err != nil || n < 0 {
			http.Error(w, "k must be a non-negative integer", http.StatusBadRequest)
			return
		}
		k = n
	}

	ans, err := s.deps.Ask.Answer(r.Context(), q, k)
	if err != nil {
		if errors.Is(err, rag.ErrLocalRequired) {
			http.Error(w, "question answering requires an on-device LLM endpoint", http.StatusServiceUnavailable)
			return
		}
		s.serverError(w, "ask", err)
		return
	}
	s.writeJSON(w, http.StatusOK, ans)
}

func (s *Server) serverError(w http.ResponseWriter, what string, err error) {
	s.deps.Logger.Error("api request failed", "op", what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	// Encode to a buffer first so a marshal failure becomes a clean 500 instead of
	// a 200 with a half-written, invalid body (the status line and prefix would
	// otherwise already be committed).
	buf, err := json.Marshal(v)
	if err != nil {
		s.serverError(w, "encode response", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		s.deps.Logger.Error("api: write response", "err", err)
	}
}

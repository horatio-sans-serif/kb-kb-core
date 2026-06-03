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

	"kb/internal/canon"
	"kb/internal/events"
	"kb/internal/ingest"
	"kb/internal/kb"
	"kb/internal/rag"
	"kb/internal/search"
	"kb/internal/store"
)

// BuildInfo is reported by the health endpoint.
type BuildInfo struct {
	Version string `json:"version"`
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

// ConnectorRegistry reports which source connectors are available (satisfied by
// *sources.Registry); /sources uses it to reject an unknown connector at create
// time. When nil, source creation is not offered (503).
type ConnectorRegistry interface {
	Has(name string) bool
	Names() []string
}

// Deps are the server's collaborators and settings.
type Deps struct {
	Store          *store.Store
	Bus            *events.Bus
	Token          string
	DefaultPrivacy string
	Build          BuildInfo
	Logger         *slog.Logger
	Enqueue        ingest.Enqueuer
	Search         Searcher
	Ask            Asker
	Rescore        ingest.RescoreFunc
	Connectors     ConnectorRegistry
}

// Server wires the HTTP handlers.
type Server struct {
	deps     Deps
	ingester *ingest.Ingester
}

func NewServer(d Deps) *Server {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Server{deps: d, ingester: ingest.New(d.Store, d.Enqueue, d.Rescore, d.Bus)}
}

// Handler builds the routed, auth-wrapped HTTP handler. /healthz is public; all
// other routes (/events, /ingest, /search, /ask) require a bearer token.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	// /events uses bearer-header auth, which suits local CLI tools. A browser
	// EventSource cannot set headers, so the Stage 10 web UI will need a
	// query-param/cookie token path scoped to this route (same constant-time
	// compare). Header auth is correct for Phase 0.
	mux.Handle("GET /events", s.requireAuth(SSEHandler(s.deps.Bus)))
	mux.Handle("POST /ingest", s.requireAuth(http.HandlerFunc(s.handleIngest)))
	mux.Handle("POST /engage", s.requireAuth(http.HandlerFunc(s.handleEngage)))
	mux.Handle("GET /search", s.requireAuth(http.HandlerFunc(s.handleSearch)))
	mux.Handle("GET /ask", s.requireAuth(http.HandlerFunc(s.handleAsk)))
	mux.Handle("POST /sources", s.requireAuth(http.HandlerFunc(s.handleCreateSource)))
	mux.Handle("GET /sources", s.requireAuth(http.HandlerFunc(s.handleListSources)))
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
	maxIngestBytes   = 8 << 20 // 8 MiB body cap
	maxTitleBytes    = 4096
	maxSourceIDBytes = 64
	maxQueryBytes    = 1024 // bound MATCH/embed/synthesis work on the /search and /ask routes
)

func validPrivacy(t string) bool {
	switch t {
	case "public", "personal", "sensitive", "restricted":
		return true
	}
	return false
}

// handleIngest validates and canonicalizes a push, resolves the privacy tier,
// and delegates to the shared ingest path. The tier comes from an explicit
// request override, else the source's default, else the daemon default (§8.17).
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
	case !ingest.ValidText(req.Title, false) || !ingest.ValidText(req.Body, true):
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

	item := ingest.Item{SourceID: req.SourceID, Title: req.Title, Tier: tier}
	if req.URL != "" {
		item.Type, item.CanonicalURL, item.RawURL = "webpage", canonicalURL, req.URL
	} else {
		item.Type, item.Body, item.ContentHash = "note", req.Body, canon.Content(req.Body)
	}

	artifactID, err := s.ingester.Ingest(ctx, item)
	if err != nil {
		s.serverError(w, "ingest", err)
		return
	}
	s.writeJSON(w, http.StatusOK, ingestResponse{ArtifactID: artifactID})
}

type engageRequest struct {
	ArtifactID string `json:"artifact_id"`
}

// handleEngage records that the user engaged with an artifact (the §8.9
// engagement signal) and re-scores it so the boost takes effect. Idempotent in
// effect — engaging again just re-stamps last_engaged_at and re-scores.
func (s *Server) handleEngage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req engageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.ArtifactID == "" || len(req.ArtifactID) > maxSourceIDBytes || !ingest.ValidText(req.ArtifactID, false) {
		http.Error(w, "valid artifact_id is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	err := s.deps.Store.Tx(ctx, func(tx *store.Store) error {
		ok, err := tx.Artifacts.RecordEngagement(ctx, req.ArtifactID)
		if err != nil {
			return err
		}
		if !ok {
			return errNoSuchArtifact // unknown/tombstoned — surfaced as 404 below
		}
		if s.deps.Rescore != nil {
			return s.deps.Rescore(ctx, tx, req.ArtifactID)
		}
		return nil
	})
	if errors.Is(err, errNoSuchArtifact) {
		http.Error(w, "unknown artifact_id", http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, "engage", err)
		return
	}

	s.deps.Bus.Publish(events.New(events.EngagementRecorded, map[string]string{"artifact_id": req.ArtifactID}))
	w.WriteHeader(http.StatusNoContent)
}

// errNoSuchArtifact rolls back the engage transaction for an unknown/tombstoned
// artifact so the handler can return 404 (a sentinel, not a store error).
var errNoSuchArtifact = errors.New("api: no such artifact")

type createSourceRequest struct {
	Kind           string          `json:"kind"` // PULL | PUSH | AMBIENT
	Connector      string          `json:"connector"`
	Config         json.RawMessage `json:"config,omitempty"`          // connector config object, e.g. {"url":"..."}
	Schedule       json.RawMessage `json:"schedule,omitempty"`        // e.g. {"every_seconds":1800}
	DefaultPrivacy string          `json:"default_privacy,omitempty"` // default personal
}

type sourceJSON struct {
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	Connector      string          `json:"connector"`
	Enabled        bool            `json:"enabled"`
	DefaultPrivacy string          `json:"default_privacy"`
	LastFetchedAt  string          `json:"last_fetched_at,omitempty"`
	Health         json.RawMessage `json:"health,omitempty"`
}

func validKind(k string) bool {
	switch k {
	case "PULL", "PUSH", "AMBIENT":
		return true
	}
	return false
}

// handleCreateSource registers a source (e.g. an RSS feed). The connector must be
// one the daemon has registered, so an unusable source can't be created.
func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	if s.deps.Connectors == nil {
		http.Error(w, "source management not configured", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req createSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	switch {
	case !validKind(req.Kind):
		http.Error(w, "kind must be PULL, PUSH, or AMBIENT", http.StatusBadRequest)
		return
	case req.Connector == "" || !s.deps.Connectors.Has(req.Connector):
		http.Error(w, "unknown connector (available: "+strings.Join(s.deps.Connectors.Names(), ", ")+")", http.StatusBadRequest)
		return
	case req.DefaultPrivacy != "" && !validPrivacy(req.DefaultPrivacy):
		http.Error(w, "invalid default_privacy", http.StatusBadRequest)
		return
	case len(req.Config) > 0 && !json.Valid(req.Config):
		http.Error(w, "config must be a JSON object", http.StatusBadRequest)
		return
	case len(req.Schedule) > 0 && !json.Valid(req.Schedule):
		http.Error(w, "schedule must be a JSON object", http.StatusBadRequest)
		return
	}

	src := &store.Source{Kind: kb.SourceKind(req.Kind), Connector: req.Connector}
	if len(req.Config) > 0 {
		src.Config = string(req.Config)
	}
	if len(req.Schedule) > 0 {
		sc := string(req.Schedule)
		src.Schedule = &sc
	}
	if req.DefaultPrivacy != "" {
		src.DefaultPrivacy = kb.PrivacyTier(req.DefaultPrivacy)
	}
	if err := s.deps.Store.Sources.Create(r.Context(), src); err != nil {
		s.serverError(w, "create source", err)
		return
	}
	// Re-read so the response reflects the persisted row (schema defaults like
	// enabled/source_weight that Create does not write back into the struct).
	created, err := s.deps.Store.Sources.Get(r.Context(), src.ID)
	if err != nil {
		s.serverError(w, "load created source", err)
		return
	}
	s.writeJSON(w, http.StatusCreated, toSourceJSON(created))
}

// handleListSources lists configured sources with their last poll time and health.
func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.deps.Store.Sources.List(r.Context())
	if err != nil {
		s.serverError(w, "list sources", err)
		return
	}
	out := make([]sourceJSON, 0, len(srcs))
	for _, src := range srcs {
		out = append(out, toSourceJSON(src))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"sources": out})
}

func toSourceJSON(src *store.Source) sourceJSON {
	j := sourceJSON{
		ID:             src.ID,
		Kind:           string(src.Kind),
		Connector:      src.Connector,
		Enabled:        src.Enabled,
		DefaultPrivacy: string(src.DefaultPrivacy),
	}
	if src.LastFetchedAt != nil {
		j.LastFetchedAt = *src.LastFetchedAt
	}
	if src.Health != nil {
		j.Health = json.RawMessage(*src.Health)
	}
	return j
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
// parseQueryAndK reads and validates the shared ?q and ?k params for the search
// and ask routes, writing the 400 itself and returning ok=false on bad input.
func parseQueryAndK(w http.ResponseWriter, r *http.Request) (q string, k int, ok bool) {
	q = strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return "", 0, false
	}
	if len(q) > maxQueryBytes {
		http.Error(w, "query too long", http.StatusBadRequest)
		return "", 0, false
	}
	if ks := r.URL.Query().Get("k"); ks != "" {
		n, err := strconv.Atoi(ks)
		if err != nil || n < 0 {
			http.Error(w, "k must be a non-negative integer", http.StatusBadRequest)
			return "", 0, false
		}
		k = n
	}
	return q, k, true
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if s.deps.Search == nil {
		http.Error(w, "search not configured", http.StatusServiceUnavailable)
		return
	}
	q, k, ok := parseQueryAndK(w, r)
	if !ok {
		return
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
	q, k, ok := parseQueryAndK(w, r)
	if !ok {
		return
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

package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kb/internal/canon"
	"kb/internal/events"
	"kb/internal/ingest"
	"kb/internal/kb"
	"kb/internal/rag"
	"kb/internal/render"
	"kb/internal/review"
	"kb/internal/search"
	"kb/internal/sourcehealth"
	"kb/internal/store"
	"kb/internal/surface"
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
	Kind(name string) (kb.SourceKind, bool)
}

// RenderService produces an output file from a set of artifacts (satisfied by
// *render.Service). When nil, the /render route reports 503.
type RenderService interface {
	Render(ctx context.Context, req render.Request) (*kb.RenderRecord, error)
	Formats() []string
}

// Surfacer ranks artifacts for surfacing (satisfied by *surface.Surfacer). When
// nil, the /surface route reports 503.
type Surfacer interface {
	Surface(ctx context.Context, opts surface.Options) ([]*kb.Artifact, error)
}

// ReviewService records spaced-repetition reviews and lists what's due (satisfied
// by *review.Service). When nil, the /review routes report 503.
type ReviewService interface {
	Record(ctx context.Context, artifactID string, grade int) (*kb.ReviewState, error)
	Due(ctx context.Context, limit int) ([]review.DueItem, error)
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
	Render         RenderService
	Surface        Surfacer
	Review         ReviewService
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
	mux.Handle("POST /render", s.requireAuth(http.HandlerFunc(s.handleRender)))
	mux.Handle("GET /renders", s.requireAuth(http.HandlerFunc(s.handleListRenders)))
	mux.Handle("GET /surface", s.requireAuth(http.HandlerFunc(s.handleSurface)))
	mux.Handle("POST /digest", s.requireAuth(http.HandlerFunc(s.handleDigest)))
	mux.Handle("POST /review", s.requireAuth(http.HandlerFunc(s.handleReview)))
	mux.Handle("GET /review/due", s.requireAuth(http.HandlerFunc(s.handleReviewDue)))
	mux.Handle("GET /graph/{id}", s.requireAuth(http.HandlerFunc(s.handleGraph)))
	mux.Handle("GET /conflicts", s.requireAuth(http.HandlerFunc(s.handleListConflicts)))
	mux.Handle("POST /conflicts/{id}", s.requireAuth(http.HandlerFunc(s.handleResolveConflict)))
	mux.Handle("GET /topics", s.requireAuth(http.HandlerFunc(s.handleListTopics)))
	mux.Handle("GET /topics/{id}", s.requireAuth(http.HandlerFunc(s.handleGetTopic)))
	mux.Handle("GET /lifecycle", s.requireAuth(http.HandlerFunc(s.handleLifecycle)))
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
	ID                  string          `json:"id"`
	Kind                string          `json:"kind"`
	Connector           string          `json:"connector"`
	Enabled             bool            `json:"enabled"`
	DefaultPrivacy      string          `json:"default_privacy"`
	Status              string          `json:"status"` // unknown|healthy|degraded|failing
	ConsecutiveFailures int             `json:"consecutive_failures"`
	LastFetchedAt       string          `json:"last_fetched_at,omitempty"`
	NextFetchAt         string          `json:"next_fetch_at,omitempty"`
	Health              json.RawMessage `json:"health,omitempty"`
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
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	connKind, known := s.deps.Connectors.Kind(req.Connector)
	switch {
	case !validKind(req.Kind):
		http.Error(w, "kind must be PULL, PUSH, or AMBIENT", http.StatusBadRequest)
		return
	case req.Connector == "" || !known:
		http.Error(w, "unknown connector (available: "+strings.Join(s.deps.Connectors.Names(), ", ")+")", http.StatusBadRequest)
		return
	case req.Kind != string(connKind):
		http.Error(w, "kind "+req.Kind+" does not match connector kind "+string(connKind), http.StatusBadRequest)
		return
	case req.DefaultPrivacy != "" && !validPrivacy(req.DefaultPrivacy):
		http.Error(w, "invalid default_privacy", http.StatusBadRequest)
		return
	case len(req.Config) > 0 && !isJSONObject(req.Config):
		http.Error(w, "config must be a JSON object", http.StatusBadRequest)
		return
	case len(req.Schedule) > 0 && !isJSONObject(req.Schedule):
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
	// Create populates the schema defaults (enabled, source_weight, …) into src,
	// so it reflects the persisted row without a re-read.
	s.writeJSON(w, http.StatusCreated, toSourceJSON(src))
}

// isJSONObject reports whether b is a JSON object (not a scalar/array/null).
func isJSONObject(b []byte) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(b, &m) == nil
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
	var health kb.Health
	polled := src.Health != nil
	if polled {
		j.Health = json.RawMessage(*src.Health)
		_ = json.Unmarshal([]byte(*src.Health), &health)
		j.ConsecutiveFailures = health.ConsecutiveFailures
	}
	j.Status = sourcehealth.DeriveStatus(health, polled)
	if next, ok := sourcehealth.NextFetchAt(src.LastFetchedAt, src.Schedule); ok {
		j.NextFetchAt = next
	}
	return j
}

type renderRequest struct {
	Format         string   `json:"format"`
	ArtifactIDs    []string `json:"artifact_ids"`
	Title          string   `json:"title,omitempty"`
	SourceQuery    string   `json:"source_query,omitempty"`
	PrivacyCeiling string   `json:"privacy_ceiling,omitempty"` // export filter ceiling; empty = include all
}

type renderJSON struct {
	ID          string   `json:"id"`
	Format      string   `json:"format"`
	ArtifactIDs []string `json:"artifact_ids"`
	OutputPath  string   `json:"output_path"`
	PrivacyMax  string   `json:"privacy_max"`
	SourceQuery string   `json:"source_query,omitempty"`
	CreatedAt   string   `json:"created_at"`
}

// maxRenderArtifacts bounds the artifact set a single render may name.
const maxRenderArtifacts = 1000

// defaultExportCeiling is the privacy ceiling applied when a render request omits
// one. An export writes tier-bearing content to a file, so the default fails safe:
// it includes public/personal but excludes the deliberately-elevated sensitive and
// restricted tiers, which must be requested explicitly via privacy_ceiling.
const defaultExportCeiling = "personal"

// handleRender renders a set of artifacts to a file in the requested format. The
// render is synchronous (a pandoc-backed format runs the converter under the
// request context, bounded by the renderer's own timeout). On success it returns
// the persisted render record (201) whose output_path names the file on disk.
func (s *Server) handleRender(w http.ResponseWriter, r *http.Request) {
	if s.deps.Render == nil {
		http.Error(w, "rendering not configured", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req renderRequest
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
	case req.Format == "":
		http.Error(w, "format is required", http.StatusBadRequest)
		return
	case len(req.ArtifactIDs) == 0:
		http.Error(w, "artifact_ids is required", http.StatusBadRequest)
		return
	case len(req.ArtifactIDs) > maxRenderArtifacts:
		http.Error(w, "too many artifact_ids", http.StatusBadRequest)
		return
	case len(req.Title) > maxTitleBytes:
		http.Error(w, "title too long", http.StatusBadRequest)
		return
	case len(req.SourceQuery) > maxQueryBytes:
		http.Error(w, "source_query too long", http.StatusBadRequest)
		return
	case req.PrivacyCeiling != "" && !validPrivacy(req.PrivacyCeiling):
		http.Error(w, "invalid privacy_ceiling", http.StatusBadRequest)
		return
	}
	for _, id := range req.ArtifactIDs {
		if id == "" || len(id) > maxSourceIDBytes || !ingest.ValidText(id, false) {
			http.Error(w, "invalid artifact_id", http.StatusBadRequest)
			return
		}
	}

	// Apply the fail-safe export ceiling when the caller names none, so an omitted
	// field cannot silently export sensitive/restricted content.
	ceiling := req.PrivacyCeiling
	if ceiling == "" {
		ceiling = defaultExportCeiling
	}

	rec, err := s.deps.Render.Render(r.Context(), render.Request{
		Format:         req.Format,
		ArtifactIDs:    req.ArtifactIDs,
		Title:          req.Title,
		SourceQuery:    req.SourceQuery,
		PrivacyCeiling: kb.PrivacyTier(ceiling),
	})
	if s.writeRenderError(w, err) {
		return
	}
	s.writeJSON(w, http.StatusCreated, toRenderJSON(rec))
}

// writeRenderError maps a render.Service error to the right HTTP response and
// reports whether it wrote one (err != nil). Shared by /render and /digest.
func (s *Server) writeRenderError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, render.ErrUnknownFormat):
		http.Error(w, "unknown format (have: "+strings.Join(s.deps.Render.Formats(), ", ")+")", http.StatusBadRequest)
	case errors.Is(err, render.ErrInvalidCeiling):
		http.Error(w, "invalid privacy_ceiling", http.StatusBadRequest)
	case errors.Is(err, render.ErrNoEligibleArtifacts):
		http.Error(w, "no eligible artifacts (all missing or above the privacy ceiling)", http.StatusUnprocessableEntity)
	case errors.Is(err, render.ErrRendererUnavailable):
		http.Error(w, "the converter for this format is not installed", http.StatusServiceUnavailable)
	case errors.Is(err, render.ErrBusy):
		w.Header().Set("Retry-After", "2")
		http.Error(w, "too many concurrent renders, retry shortly", http.StatusServiceUnavailable)
	default:
		s.serverError(w, "render", err)
	}
	return true
}

// handleListRenders lists the render history (newest first).
func (s *Server) handleListRenders(w http.ResponseWriter, r *http.Request) {
	recs, err := s.deps.Store.Renders.List(r.Context())
	if err != nil {
		s.serverError(w, "list renders", err)
		return
	}
	out := make([]renderJSON, 0, len(recs))
	for _, rec := range recs {
		out = append(out, toRenderJSON(rec))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"renders": out})
}

func toRenderJSON(rec *kb.RenderRecord) renderJSON {
	ids := rec.ArtifactIDs
	if ids == nil {
		ids = []string{} // emit [] not null
	}
	return renderJSON{
		ID:          rec.ID,
		Format:      rec.Format,
		ArtifactIDs: ids,
		OutputPath:  rec.OutputPath,
		PrivacyMax:  string(rec.PrivacyMax),
		SourceQuery: rec.SourceQuery,
		CreatedAt:   rec.CreatedAt,
	}
}

type surfaceItemJSON struct {
	ArtifactID  string  `json:"artifact_id"`
	Title       string  `json:"title,omitempty"`
	URL         string  `json:"url,omitempty"`
	Score       float64 `json:"score"`
	PrivacyTier string  `json:"privacy_tier"`
	FirstSeenAt string  `json:"first_seen_at"`
}

// maxSurfaceLimit caps how many items one /surface call may return.
const maxSurfaceLimit = 200

// handleSurface returns the ready, scored artifacts ranked by relevance — "what
// should I see now" (§8.11). Like the render export, the privacy ceiling defaults
// conservatively so an omitted filter cannot surface sensitive/restricted content.
func (s *Server) handleSurface(w http.ResponseWriter, r *http.Request) {
	if s.deps.Surface == nil {
		http.Error(w, "surfacing not configured", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()

	limit := 20
	if ls := q.Get("limit"); ls != "" {
		n, err := strconv.Atoi(ls)
		if err != nil || n < 1 || n > maxSurfaceLimit {
			http.Error(w, "limit must be between 1 and 200", http.StatusBadRequest)
			return
		}
		limit = n
	}

	var minScore float64
	if ms := q.Get("min_score"); ms != "" {
		f, err := strconv.ParseFloat(ms, 64)
		if err != nil || f < 0 {
			http.Error(w, "min_score must be a non-negative number", http.StatusBadRequest)
			return
		}
		minScore = f
	}

	var since string
	if ss := q.Get("since"); ss != "" {
		norm, ok := normalizeTimestamp(ss)
		if !ok {
			http.Error(w, "since must be a valid timestamp", http.StatusBadRequest)
			return
		}
		since = norm
	}

	ceiling := q.Get("privacy_ceiling")
	if ceiling == "" {
		ceiling = defaultExportCeiling
	} else if !validPrivacy(ceiling) {
		http.Error(w, "invalid privacy_ceiling", http.StatusBadRequest)
		return
	}

	arts, err := s.deps.Surface.Surface(r.Context(), surface.Options{
		Limit:          limit,
		Since:          since,
		MinScore:       minScore,
		PrivacyCeiling: kb.PrivacyTier(ceiling),
	})
	if err != nil {
		s.serverError(w, "surface", err)
		return
	}

	out := make([]surfaceItemJSON, 0, len(arts))
	for _, a := range arts {
		item := surfaceItemJSON{
			ArtifactID:  a.ID,
			Title:       a.Title,
			PrivacyTier: string(a.PrivacyTier),
			FirstSeenAt: a.FirstSeenAt,
		}
		if a.RelevanceScore != nil {
			item.Score = *a.RelevanceScore
		}
		if a.CanonicalURL != nil {
			item.URL = *a.CanonicalURL
		}
		out = append(out, item)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"surface": out})
}

type digestRequest struct {
	Format         string  `json:"format"`
	Title          string  `json:"title,omitempty"`
	Limit          int     `json:"limit,omitempty"`
	Since          string  `json:"since,omitempty"`
	MinScore       float64 `json:"min_score,omitempty"`
	PrivacyCeiling string  `json:"privacy_ceiling,omitempty"`
}

// handleDigest surfaces the top-ranked artifacts and renders them into one
// document — the cyclic loop's delivery step (§8.11), built on the §8.14 "digests
// are render inputs" contract. The same privacy ceiling gates both the surfacing
// and the render (defense in depth), and defaults conservatively to personal.
func (s *Server) handleDigest(w http.ResponseWriter, r *http.Request) {
	if s.deps.Surface == nil || s.deps.Render == nil {
		http.Error(w, "digests not configured", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req digestRequest
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
	case req.Format == "":
		http.Error(w, "format is required", http.StatusBadRequest)
		return
	case req.Limit < 0 || req.Limit > maxSurfaceLimit:
		http.Error(w, "limit out of range", http.StatusBadRequest)
		return
	case req.MinScore < 0:
		http.Error(w, "min_score must be non-negative", http.StatusBadRequest)
		return
	case len(req.Title) > maxTitleBytes:
		http.Error(w, "title too long", http.StatusBadRequest)
		return
	}

	since := req.Since
	if since != "" {
		norm, ok := normalizeTimestamp(since)
		if !ok {
			http.Error(w, "since must be a valid timestamp", http.StatusBadRequest)
			return
		}
		since = norm
	}

	ceiling := req.PrivacyCeiling
	if ceiling == "" {
		ceiling = defaultExportCeiling
	} else if !validPrivacy(ceiling) {
		http.Error(w, "invalid privacy_ceiling", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	arts, err := s.deps.Surface.Surface(ctx, surface.Options{
		Limit:          req.Limit,
		Since:          since,
		MinScore:       req.MinScore,
		PrivacyCeiling: kb.PrivacyTier(ceiling),
	})
	if err != nil {
		s.serverError(w, "digest surface", err)
		return
	}
	if len(arts) == 0 {
		http.Error(w, "nothing to digest (no eligible artifacts)", http.StatusUnprocessableEntity)
		return
	}
	ids := make([]string, len(arts))
	for i, a := range arts {
		ids[i] = a.ID // preserve the relevance rank order
	}

	title := req.Title
	if title == "" {
		title = "Digest"
	}
	rec, err := s.deps.Render.Render(ctx, render.Request{
		Format:         req.Format,
		ArtifactIDs:    ids,
		Title:          title,
		SourceQuery:    "digest",
		PrivacyCeiling: kb.PrivacyTier(ceiling),
	})
	if s.writeRenderError(w, err) {
		return
	}
	s.writeJSON(w, http.StatusCreated, toRenderJSON(rec))
}

type lifecycleSourceJSON struct {
	ID          string `json:"id"`
	Connector   string `json:"connector"`
	Budget      int    `json:"budget"`
	ActiveCount int    `json:"active_count"`
}

type lifecycleJSON struct {
	Tiers struct {
		Active  int `json:"active"`
		Archive int `json:"archive"`
		Purged  int `json:"purged"`
	} `json:"tiers"`
	Sources []lifecycleSourceJSON `json:"sources"`
}

// handleLifecycle reports the artifact population by lifecycle tier and the
// active footprint of each budgeted source against its budget (§8.12).
func (s *Server) handleLifecycle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	counts, err := s.deps.Store.Artifacts.LifecycleCounts(ctx)
	if err != nil {
		s.serverError(w, "lifecycle counts", err)
		return
	}
	srcs, err := s.deps.Store.Sources.ListWithBudget(ctx)
	if err != nil {
		s.serverError(w, "lifecycle sources", err)
		return
	}
	var out lifecycleJSON
	out.Tiers.Active, out.Tiers.Archive, out.Tiers.Purged = counts.Active, counts.Archive, counts.Purged
	out.Sources = make([]lifecycleSourceJSON, 0, len(srcs))
	for _, src := range srcs {
		n, err := s.deps.Store.Encounters.ActiveArtifactCountForSource(ctx, src.ID)
		if err != nil {
			s.serverError(w, "lifecycle source count", err)
			return
		}
		budget := 0
		if src.StorageBudget != nil {
			budget = *src.StorageBudget
		}
		out.Sources = append(out.Sources, lifecycleSourceJSON{
			ID: src.ID, Connector: src.Connector, Budget: budget, ActiveCount: n,
		})
	}
	s.writeJSON(w, http.StatusOK, out)
}

type topicListItemJSON struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	MemberCount int    `json:"member_count"`
}

type topicMemberJSON struct {
	ArtifactID string  `json:"artifact_id"`
	Title      string  `json:"title,omitempty"`
	URL        string  `json:"url,omitempty"`
	Score      float64 `json:"score"`
}

type topicJSON struct {
	ID      string            `json:"id"`
	Label   string            `json:"label"`
	Members []topicMemberJSON `json:"members"`
}

// handleListTopics lists the emergent topics by size (§8.7), largest first.
func (s *Server) handleListTopics(w http.ResponseWriter, r *http.Request) {
	topics, err := s.deps.Store.Topics.ListWithCounts(r.Context(), maxSurfaceLimit)
	if err != nil {
		s.serverError(w, "list topics", err)
		return
	}
	out := make([]topicListItemJSON, 0, len(topics))
	for _, t := range topics {
		out = append(out, topicListItemJSON{ID: t.Topic.ID, Label: t.Topic.Label, MemberCount: t.MemberCount})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"topics": out})
}

// handleGetTopic returns a topic and its member artifacts (highest score first).
func (s *Server) handleGetTopic(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || len(id) > maxSourceIDBytes || !ingest.ValidText(id, false) {
		http.Error(w, "invalid topic id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	topic, err := s.deps.Store.Topics.Get(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown topic", http.StatusNotFound)
			return
		}
		s.serverError(w, "get topic", err)
		return
	}
	members, err := s.deps.Store.Topics.Members(ctx, id, maxSurfaceLimit)
	if err != nil {
		s.serverError(w, "topic members", err)
		return
	}
	out := topicJSON{ID: topic.ID, Label: topic.Label, Members: make([]topicMemberJSON, 0, len(members))}
	for _, m := range members {
		out.Members = append(out.Members, topicMemberJSON{
			ArtifactID: m.ArtifactID, Title: m.Title, URL: m.URL, Score: m.Score,
		})
	}
	s.writeJSON(w, http.StatusOK, out)
}

type claimJSON struct {
	ID         string `json:"id"`
	ArtifactID string `json:"artifact_id"`
	Statement  string `json:"statement"`
}

type conflictJSON struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Rationale string    `json:"rationale,omitempty"`
	CreatedAt string    `json:"created_at"`
	ClaimA    claimJSON `json:"claim_a"`
	ClaimB    claimJSON `json:"claim_b"`
}

func validConflictStatus(s string) bool {
	switch s {
	case "open", "resolved", "dismissed":
		return true
	}
	return false
}

// handleListConflicts lists detected contradictions for human resolution (§8.13),
// each with both claim statements. ?status defaults to open. Claim statements are
// returned regardless of their artifacts' privacy tiers: this is a local,
// token-authenticated, single-user API and the tiers gate cloud egress, not local
// reads (the requireAuth boundary is the relevant control).
func (s *Server) handleListConflicts(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "open"
	} else if !validConflictStatus(status) {
		http.Error(w, "status must be open, resolved, or dismissed", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	conflicts, err := s.deps.Store.Conflicts.ListByStatus(ctx, status, maxSurfaceLimit)
	if err != nil {
		s.serverError(w, "list conflicts", err)
		return
	}
	out := make([]conflictJSON, 0, len(conflicts))
	for _, cf := range conflicts {
		out = append(out, conflictJSON{
			ID:        cf.ID,
			Status:    cf.Status,
			Rationale: cf.Rationale,
			CreatedAt: cf.CreatedAt,
			ClaimA:    s.claimJSON(ctx, cf.ClaimA),
			ClaimB:    s.claimJSON(ctx, cf.ClaimB),
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"conflicts": out})
}

// claimJSON loads a claim's display fields; a missing claim (shouldn't happen via
// the FK) degrades to just the id rather than failing the whole list.
func (s *Server) claimJSON(ctx context.Context, id string) claimJSON {
	c, err := s.deps.Store.Claims.Get(ctx, id)
	if err != nil {
		return claimJSON{ID: id}
	}
	return claimJSON{ID: c.ID, ArtifactID: c.ArtifactID, Statement: c.Statement}
}

type resolveConflictRequest struct {
	Status    string `json:"status"` // resolved | dismissed
	Rationale string `json:"rationale,omitempty"`
}

// handleResolveConflict moves a conflict to resolved or dismissed with an optional
// rationale (§8.13: surfaced for human resolution).
func (s *Server) handleResolveConflict(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || len(id) > maxSourceIDBytes || !ingest.ValidText(id, false) {
		http.Error(w, "invalid conflict id", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req resolveConflictRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Status != "resolved" && req.Status != "dismissed" {
		http.Error(w, "status must be 'resolved' or 'dismissed'", http.StatusBadRequest)
		return
	}
	if len(req.Rationale) > maxTitleBytes {
		http.Error(w, "rationale too long", http.StatusBadRequest)
		return
	}

	err := s.deps.Store.Conflicts.SetStatus(r.Context(), id, req.Status, req.Rationale)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "unknown conflict", http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, "resolve conflict", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type entityJSON struct {
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
}

type graphNodeJSON struct {
	ArtifactID string `json:"artifact_id"`
	Title      string `json:"title,omitempty"`
	URL        string `json:"url,omitempty"`
}

type graphJSON struct {
	ArtifactID string          `json:"artifact_id"`
	Title      string          `json:"title,omitempty"`
	Entities   []entityJSON    `json:"entities"`
	Connected  []graphNodeJSON `json:"connected"`
}

// maxGraphHops bounds a graph traversal so one request cannot walk the whole
// graph (the store also caps the node count).
const maxGraphHops = 5

// handleGraph returns the knowledge-graph view of an artifact (§8.7): the
// entities it mentions and the artifacts connected to it within ?hops edges
// (undirected, default 1).
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || len(id) > maxSourceIDBytes || !ingest.ValidText(id, false) {
		http.Error(w, "invalid artifact id", http.StatusBadRequest)
		return
	}
	hops := 1
	if hs := r.URL.Query().Get("hops"); hs != "" {
		n, err := strconv.Atoi(hs)
		if err != nil || n < 1 || n > maxGraphHops {
			http.Error(w, fmt.Sprintf("hops must be between 1 and %d", maxGraphHops), http.StatusBadRequest)
			return
		}
		hops = n
	}

	ctx := r.Context()
	start, err := s.deps.Store.Artifacts.GetLive(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown artifact", http.StatusNotFound)
			return
		}
		s.serverError(w, "graph artifact", err)
		return
	}

	ents, err := s.deps.Store.Graph.EntitiesForArtifact(ctx, id)
	if err != nil {
		s.serverError(w, "graph entities", err)
		return
	}
	connectedIDs, err := s.deps.Store.Graph.ConnectedWithin(ctx, id, hops)
	if err != nil {
		s.serverError(w, "graph connected", err)
		return
	}

	out := graphJSON{
		ArtifactID: id,
		Title:      start.Title,
		Entities:   make([]entityJSON, 0, len(ents)),
		Connected:  make([]graphNodeJSON, 0, len(connectedIDs)),
	}
	for _, e := range ents {
		out.Entities = append(out.Entities, entityJSON{Kind: e.Kind, Name: e.Name, Aliases: e.Aliases})
	}
	for _, cid := range connectedIDs {
		a, err := s.deps.Store.Artifacts.GetLive(ctx, cid)
		if err != nil {
			continue // tombstoned between traversal and load — drop
		}
		node := graphNodeJSON{ArtifactID: cid, Title: a.Title}
		if a.CanonicalURL != nil {
			node.URL = *a.CanonicalURL
		}
		out.Connected = append(out.Connected, node)
	}
	s.writeJSON(w, http.StatusOK, out)
}

type reviewRequest struct {
	ArtifactID string `json:"artifact_id"`
	Grade      *int   `json:"grade"` // 0..5; pointer so a missing grade is distinguishable from 0
}

type reviewJSON struct {
	ArtifactID   string  `json:"artifact_id"`
	NextReviewAt string  `json:"next_review_at"`
	IntervalDays float64 `json:"interval_days"`
	Ease         float64 `json:"ease"`
	Reviews      int     `json:"reviews"`
}

type dueItemJSON struct {
	ArtifactID   string  `json:"artifact_id"`
	Title        string  `json:"title,omitempty"`
	URL          string  `json:"url,omitempty"`
	NextReviewAt string  `json:"next_review_at"`
	IntervalDays float64 `json:"interval_days"`
	Reviews      int     `json:"reviews"`
}

// handleReview records a spaced-repetition recall grade (0..5) for an artifact and
// returns the updated schedule (§8.11). A grade <3 lapses the item; 3+ lengthens
// the interval.
func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	if s.deps.Review == nil {
		http.Error(w, "review not configured", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req reviewRequest
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
	case req.ArtifactID == "" || len(req.ArtifactID) > maxSourceIDBytes || !ingest.ValidText(req.ArtifactID, false):
		http.Error(w, "valid artifact_id is required", http.StatusBadRequest)
		return
	case req.Grade == nil || *req.Grade < 0 || *req.Grade > 5:
		http.Error(w, "grade must be an integer 0..5", http.StatusBadRequest)
		return
	}

	rs, err := s.deps.Review.Record(r.Context(), req.ArtifactID, *req.Grade)
	if errors.Is(err, review.ErrArtifactNotFound) {
		http.Error(w, "unknown artifact_id", http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, "review", err)
		return
	}
	s.writeJSON(w, http.StatusOK, reviewJSON{
		ArtifactID:   rs.ArtifactID,
		NextReviewAt: rs.NextReviewAt,
		IntervalDays: rs.IntervalDays,
		Ease:         rs.Ease,
		Reviews:      rs.Reviews,
	})
}

// handleReviewDue lists the artifacts due for review now, soonest first.
func (s *Server) handleReviewDue(w http.ResponseWriter, r *http.Request) {
	if s.deps.Review == nil {
		http.Error(w, "review not configured", http.StatusServiceUnavailable)
		return
	}
	limit := 20
	if ls := r.URL.Query().Get("limit"); ls != "" {
		n, err := strconv.Atoi(ls)
		if err != nil || n < 1 || n > maxSurfaceLimit {
			http.Error(w, "limit must be between 1 and 200", http.StatusBadRequest)
			return
		}
		limit = n
	}

	items, err := s.deps.Review.Due(r.Context(), limit)
	if err != nil {
		s.serverError(w, "review due", err)
		return
	}
	out := make([]dueItemJSON, 0, len(items))
	for _, it := range items {
		out = append(out, dueItemJSON{
			ArtifactID:   it.ArtifactID,
			Title:        it.Title,
			URL:          it.URL,
			NextReviewAt: it.NextReviewAt,
			IntervalDays: it.IntervalDays,
			Reviews:      it.Reviews,
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"due": out})
}

// normalizeTimestamp parses s as a store-format or RFC3339 timestamp and returns
// it re-encoded in store.TimeFormat UTC. This is required before any SQL
// comparison: the stored timestamps are fixed-width UTC and only byte-compare
// (lexicographic == chronological) within that one encoding, so an RFC3339 input
// with a different offset, fractional width, or 'Z'-vs-'+00:00' must be canonical-
// ized first or the comparison is silently wrong. ok is false if s does not parse.
func normalizeTimestamp(s string) (string, bool) {
	if len(s) > 64 {
		return "", false
	}
	for _, layout := range []string{store.TimeFormat, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return store.FormatTime(t), true
		}
	}
	return "", false
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

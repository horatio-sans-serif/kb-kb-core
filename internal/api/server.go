package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"kb/internal/canon"
	"kb/internal/events"
	"kb/internal/kb"
	"kb/internal/store"
)

// BuildInfo is reported by the health endpoint.
type BuildInfo struct {
	Version string `json:"version"`
}

// Enqueuer expands an ingested artifact into its enrichment-pipeline jobs within
// the caller's transaction (satisfied by *pipeline.Pipeline). When nil, ingest
// enqueues a single placeholder "ingest" job instead.
type Enqueuer interface {
	EnqueueTx(ctx context.Context, tx *store.Store, artifactID string) error
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
		// TODO(Phase 2.x): re-ingesting a known URL re-runs the pipeline even
		// though the artifact is unchanged. Make this idempotent once
		// UpsertByCanonical reports inserted-vs-updated.
		if s.deps.Enqueue != nil {
			return s.deps.Enqueue.EnqueueTx(ctx, tx, art.ID)
		}
		_, err = tx.Jobs.Enqueue(ctx, store.EnqueueSpec{Type: "ingest", ArtifactID: &art.ID})
		return err
	})
	if err != nil {
		s.serverError(w, "ingest", err)
		return
	}

	s.deps.Bus.Publish(events.New(events.ArtifactIngested,
		map[string]string{"artifact_id": artifactID, "source_id": req.SourceID}))

	s.writeJSON(w, http.StatusOK, ingestResponse{ArtifactID: artifactID})
}

func (s *Server) serverError(w http.ResponseWriter, what string, err error) {
	s.deps.Logger.Error("api request failed", "op", what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.deps.Logger.Error("api: encode response", "err", err)
	}
}

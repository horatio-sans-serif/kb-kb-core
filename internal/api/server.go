package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"kb/internal/canon"
	"kb/internal/events"
	"kb/internal/store"
)

// BuildInfo is reported by the health endpoint.
type BuildInfo struct {
	Version string `json:"version"`
}

// Deps are the server's collaborators and settings.
type Deps struct {
	Store          *store.Store
	Bus            *events.Bus
	Token          string
	DefaultPrivacy string
	Build          BuildInfo
}

// Server wires the HTTP handlers.
type Server struct {
	deps Deps
}

func NewServer(d Deps) *Server { return &Server{deps: d} }

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
// logged or echoed.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	const prefix = "Bearer "
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		token := strings.TrimPrefix(h, prefix)
		// An empty configured token must never authenticate (config forbids it;
		// this is defense-in-depth, since "" == "" would otherwise pass).
		if s.deps.Token == "" ||
			!strings.HasPrefix(h, prefix) ||
			subtle.ConstantTimeCompare([]byte(token), []byte(s.deps.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.deps.Build.Version})
}

type ingestRequest struct {
	SourceID string `json:"source_id"`
	URL      string `json:"url,omitempty"`
	Body     string `json:"body,omitempty"`
	Title    string `json:"title,omitempty"`
}

type ingestResponse struct {
	ArtifactID string `json:"artifact_id"`
}

// handleIngest is the walking skeleton: it writes the artifact, its encounter,
// and an enrichment job atomically, then publishes artifact.ingested. The
// publish happens only after the transaction commits, so a rolled-back ingest
// emits no event.
// maxIngestBytes caps the ingest request body; raise it when real capture
// payloads are sized (Phase 1+).
const maxIngestBytes = 8 << 20 // 8 MiB

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
	if req.SourceID == "" {
		http.Error(w, "source_id is required", http.StatusBadRequest)
		return
	}
	if req.URL == "" && req.Body == "" {
		http.Error(w, "url or body is required", http.StatusBadRequest)
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
	if _, err := s.deps.Store.Sources.Get(ctx, req.SourceID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown source_id", http.StatusBadRequest)
			return
		}
		s.serverError(w, "lookup source", err)
		return
	}

	var artifactID string
	err := s.deps.Store.Tx(ctx, func(tx *store.Store) error {
		a := &store.Artifact{Title: req.Title, PrivacyTier: s.deps.DefaultPrivacy}
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
		// TODO(Phase 2): re-ingesting a known URL re-enqueues enrichment even
		// though the artifact is unchanged. Make this idempotent (skip when
		// enrichments already exist) once UpsertByCanonical reports
		// inserted-vs-updated, so the cloud pipeline doesn't re-run for free.
		_, err = tx.Jobs.Enqueue(ctx, store.EnqueueSpec{Type: "ingest", ArtifactID: &art.ID})
		return err
	})
	if err != nil {
		s.serverError(w, "ingest", err)
		return
	}

	s.deps.Bus.Publish(events.New(events.ArtifactIngested,
		map[string]string{"artifact_id": artifactID, "source_id": req.SourceID}))

	writeJSON(w, http.StatusOK, ingestResponse{ArtifactID: artifactID})
}

func (s *Server) serverError(w http.ResponseWriter, what string, err error) {
	log.Printf("api: %s: %v", what, err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("api: encode response: %v", err)
	}
}

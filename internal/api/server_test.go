package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kb/internal/events"
	"kb/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store.New(db)
}

func testServer(t *testing.T) (*store.Store, *events.Bus, http.Handler) {
	t.Helper()
	st := newStore(t)
	bus := events.NewBus()
	srv := NewServer(Deps{Store: st, Bus: bus, Token: "tok", DefaultPrivacy: "personal", Build: BuildInfo{Version: "test"}})
	return st, bus, srv.Handler()
}

func TestHealthz_NoAuth(t *testing.T) {
	_, _, h := testServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body = %q, want it to report status ok", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "test") {
		t.Errorf("body = %q, want it to include the build version", rec.Body.String())
	}
}

func TestAuth_RejectsMissingToken(t *testing.T) {
	_, _, h := testServer(t)

	// /ingest without a token → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/ingest without token: status = %d, want 401", rec.Code)
	}

	// wrong token → 401
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/ingest with wrong token: status = %d, want 401", rec.Code)
	}

	// /healthz needs no token → 200
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz without token: status = %d, want 200", rec.Code)
	}
}

func TestIngest_EndToEnd(t *testing.T) {
	st, bus, h := testServer(t)
	ctx := context.Background()

	src := &store.Source{Kind: "PUSH", Connector: "extension"}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}

	sub, cancel := bus.Subscribe()
	defer cancel()

	body := `{"source_id":"` + src.ID + `","url":"https://example.com/x","title":"X"}`
	req := httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp ingestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ArtifactID == "" {
		t.Fatal("response has no artifact_id")
	}

	// Artifact written.
	arts, err := st.Artifacts.List(ctx)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(arts) != 1 || arts[0].ID != resp.ArtifactID {
		t.Errorf("artifacts = %d, want exactly the ingested one (%s)", len(arts), resp.ArtifactID)
	}

	// Encounter recorded.
	n, err := st.Encounters.CountForArtifact(ctx, resp.ArtifactID)
	if err != nil {
		t.Fatalf("count encounters: %v", err)
	}
	if n != 1 {
		t.Errorf("encounter count = %d, want 1", n)
	}

	// Enrichment job enqueued for the artifact.
	job, err := st.Jobs.Claim(ctx, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if job == nil || job.Type != "ingest" || job.ArtifactID == nil || *job.ArtifactID != resp.ArtifactID {
		t.Errorf("expected an 'ingest' job for %s, got %+v", resp.ArtifactID, job)
	}

	// artifact.ingested published (after the transaction committed).
	select {
	case e := <-sub:
		if e.Type != events.ArtifactIngested {
			t.Errorf("event type = %q, want %q", e.Type, events.ArtifactIngested)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no artifact.ingested event published")
	}
}

func TestIngest_UnknownSource(t *testing.T) {
	_, _, h := testServer(t)

	body := `{"source_id":"01NOPE","url":"https://example.com/x"}`
	req := httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown source", rec.Code)
	}
}

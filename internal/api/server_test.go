package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kb/internal/enrich"
	"kb/internal/events"
	"kb/internal/fetch"
	"kb/internal/jobs"
	"kb/internal/pipeline"
	"kb/internal/store"
	"kb/internal/storetest"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newStore(t *testing.T) *store.Store { return storetest.New(t) }

func testServer(t *testing.T) (*store.Store, *events.Bus, http.Handler) {
	t.Helper()
	st := newStore(t)
	bus := events.NewBus()
	srv := NewServer(Deps{Store: st, Bus: bus, Token: "tok", DefaultPrivacy: "personal", Build: BuildInfo{Version: "test"}, Logger: discardLogger()})
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

	// With no Enqueuer wired, ingest records the artifact and its encounter but
	// starts no pipeline — there is nothing claimable.
	job, err := st.Jobs.Claim(ctx, store.FormatTime(time.Now()))
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if job != nil {
		t.Errorf("expected no job with a nil Enqueuer, got %+v", job)
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
	rec := doIngest(h, `{"source_id":"01NOPE","url":"https://example.com/x"}`, "tok")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown source", rec.Code)
	}
}

func TestIngest_BodyType(t *testing.T) {
	st, _, h := testServer(t)
	ctx := context.Background()
	src := &store.Source{Kind: "PUSH", Connector: "x"}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}

	rec := doIngest(h, `{"source_id":"`+src.ID+`","body":"# note"}`, "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	arts, err := st.Artifacts.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(arts) != 1 || arts[0].Type != "note" || arts[0].BodyMarkdown != "# note" {
		t.Errorf("got %+v, want one note artifact with the body persisted", arts)
	}
}

func TestIngest_MalformedJSON(t *testing.T) {
	_, _, h := testServer(t)
	if rec := doIngest(h, `not json`, "tok"); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for malformed JSON", rec.Code)
	}
}

func TestIngest_MissingFields(t *testing.T) {
	_, _, h := testServer(t)
	if rec := doIngest(h, `{"url":"https://e.com"}`, "tok"); rec.Code != http.StatusBadRequest {
		t.Errorf("missing source_id: status = %d, want 400", rec.Code)
	}
	if rec := doIngest(h, `{"source_id":"x"}`, "tok"); rec.Code != http.StatusBadRequest {
		t.Errorf("missing url and body: status = %d, want 400", rec.Code)
	}
}

func TestIngest_ReingestDedups(t *testing.T) {
	st, bus, h := testServer(t)
	ctx := context.Background()
	src := &store.Source{Kind: "PUSH", Connector: "x"}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	sub, cancel := bus.Subscribe()
	defer cancel()

	body := `{"source_id":"` + src.ID + `","url":"https://example.com/x"}`
	var r1, r2 ingestResponse
	if err := json.Unmarshal(doIngest(h, body, "tok").Body.Bytes(), &r1); err != nil {
		t.Fatalf("decode r1: %v", err)
	}
	if err := json.Unmarshal(doIngest(h, body, "tok").Body.Bytes(), &r2); err != nil {
		t.Fatalf("decode r2: %v", err)
	}

	if r1.ArtifactID == "" || r1.ArtifactID != r2.ArtifactID {
		t.Errorf("re-ingest created a duplicate: %q vs %q", r1.ArtifactID, r2.ArtifactID)
	}
	arts, _ := st.Artifacts.List(ctx)
	if len(arts) != 1 {
		t.Errorf("artifacts = %d, want 1 (deduped)", len(arts))
	}
	n, _ := st.Encounters.CountForArtifact(ctx, r1.ArtifactID)
	if n != 2 {
		t.Errorf("encounters = %d, want 2 (one per sighting)", n)
	}
	for i := range 2 {
		select {
		case <-sub:
		case <-time.After(2 * time.Second):
			t.Fatalf("missing artifact.ingested event %d", i)
		}
	}
}

func TestIngest_BodyTooLarge(t *testing.T) {
	_, _, h := testServer(t)
	big := strings.Repeat("a", 9<<20) // 9 MiB > the 8 MiB limit
	rec := doIngest(h, `{"source_id":"x","body":"`+big+`"}`, "tok")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 for an oversized body", rec.Code)
	}
}

func TestAuth_EmptyConfiguredTokenRejects(t *testing.T) {
	srv := NewServer(Deps{Store: newStore(t), Bus: events.NewBus(), Token: "", DefaultPrivacy: "personal"})
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer ") // empty token after the scheme
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (an empty configured token must never authenticate)", rec.Code)
	}
}

func TestIngest_CanonicalDedup(t *testing.T) {
	st, _, h := testServer(t)
	ctx := context.Background()
	src := &store.Source{Kind: "PUSH", Connector: "x"}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// Two URLs differing only by tracking params canonicalize to the same URL.
	doIngest(h, `{"source_id":"`+src.ID+`","url":"https://example.com/p?utm_source=a"}`, "tok")
	doIngest(h, `{"source_id":"`+src.ID+`","url":"https://example.com/p?utm_source=b&fbclid=z"}`, "tok")

	arts, err := st.Artifacts.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("artifacts = %d, want 1 (deduped by canonical URL)", len(arts))
	}
	if arts[0].CanonicalURL == nil || *arts[0].CanonicalURL != "https://example.com/p" {
		t.Errorf("canonical_url = %v, want https://example.com/p", arts[0].CanonicalURL)
	}
	if n, _ := st.Encounters.CountForArtifact(ctx, arts[0].ID); n != 2 {
		t.Errorf("encounters = %d, want 2", n)
	}
}

func TestIngest_ContentDedup(t *testing.T) {
	st, _, h := testServer(t)
	ctx := context.Background()
	src := &store.Source{Kind: "PUSH", Connector: "x"}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// Same content, different whitespace -> same fingerprint -> one artifact.
	doIngest(h, `{"source_id":"`+src.ID+`","body":"hello world"}`, "tok")
	doIngest(h, `{"source_id":"`+src.ID+`","body":"hello   world"}`, "tok")

	arts, err := st.Artifacts.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(arts) != 1 {
		t.Errorf("artifacts = %d, want 1 (deduped by content hash)", len(arts))
	}
}

func TestIngest_InvalidURL(t *testing.T) {
	st, _, h := testServer(t)
	src := &store.Source{Kind: "PUSH", Connector: "x"}
	if err := st.Sources.Create(context.Background(), src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if rec := doIngest(h, `{"source_id":"`+src.ID+`","url":"not a url"}`, "tok"); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an invalid url", rec.Code)
	}
}

func TestIngest_PrivacyFromSource(t *testing.T) {
	st, _, h := testServer(t)
	ctx := context.Background()
	src := &store.Source{Kind: "PUSH", Connector: "imessage", DefaultPrivacy: "sensitive"}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	doIngest(h, `{"source_id":"`+src.ID+`","body":"secret"}`, "tok")
	arts, _ := st.Artifacts.List(ctx)
	if len(arts) != 1 || arts[0].PrivacyTier != "sensitive" {
		t.Errorf("privacy = %v, want sensitive (from the source default)", arts)
	}
}

func TestIngest_PrivacyOverrideAndValidation(t *testing.T) {
	st, _, h := testServer(t)
	ctx := context.Background()
	src := &store.Source{Kind: "PUSH", Connector: "x"} // default personal
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	doIngest(h, `{"source_id":"`+src.ID+`","body":"x","privacy":"restricted"}`, "tok")
	arts, _ := st.Artifacts.List(ctx)
	if len(arts) != 1 || arts[0].PrivacyTier != "restricted" {
		t.Errorf("privacy = %v, want restricted (request override)", arts)
	}
	if rec := doIngest(h, `{"source_id":"`+src.ID+`","body":"y","privacy":"bogus"}`, "tok"); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid privacy: status = %d, want 400", rec.Code)
	}
}

func TestIngest_BothURLAndBody(t *testing.T) {
	st, _, h := testServer(t)
	ctx := context.Background()
	src := &store.Source{Kind: "PUSH", Connector: "x"}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	doIngest(h, `{"source_id":"`+src.ID+`","url":"https://e.com/x","body":"ignored"}`, "tok")
	arts, _ := st.Artifacts.List(ctx)
	if len(arts) != 1 || arts[0].Type != "webpage" || arts[0].BodyMarkdown != "" {
		t.Errorf("both url+body: got %+v, want a webpage with no body (url wins)", arts)
	}
}

func TestIngest_RejectsControlChars(t *testing.T) {
	st, _, h := testServer(t)
	src := &store.Source{Kind: "PUSH", Connector: "x"}
	if err := st.Sources.Create(context.Background(), src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	body := `{"source_id":"` + src.ID + `","body":"ok","title":"bad\u0007title"}`
	if rec := doIngest(h, body, "tok"); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a control char in the title", rec.Code)
	}
}

func TestIngest_StoreFailureReturns500(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	srv := NewServer(Deps{Store: store.New(db), Bus: events.NewBus(), Token: "tok", DefaultPrivacy: "personal", Logger: discardLogger()})
	h := srv.Handler()
	db.Close() // subsequent store ops fail

	rec := doIngest(h, `{"source_id":"01ABC","url":"https://e.com/x"}`, "tok")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when the store fails", rec.Code)
	}
}

func TestSSEHandler_NonFlusherReturns500(t *testing.T) {
	rec := httptest.NewRecorder()
	w := struct{ http.ResponseWriter }{rec} // embeds the interface, hiding Flush
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	SSEHandler(events.NewBus())(w, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for a non-flushing writer", rec.Code)
	}
}

func TestIngest_PipelineFetchesAndMarksReady(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<html><head><title>P</title></head><body><p>Fetched body.</p></body></html>`)
	}))
	defer page.Close()

	st := storetest.New(t)
	bus := events.NewBus()
	runner := jobs.NewRunner(st, jobs.WithLogger(discardLogger()))
	p := pipeline.New()
	p.Register(enrich.NewFetch(fetch.NewWebFetcher(page.Client()))) // injected client reaches loopback
	p.RegisterHandlers(st, runner, bus)
	srv := NewServer(Deps{Store: st, Bus: bus, Token: "tok", DefaultPrivacy: "personal", Logger: discardLogger(), Enqueue: p})
	h := srv.Handler()

	ctx := context.Background()
	src := &store.Source{Kind: "PUSH", Connector: "x"}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}

	body := `{"source_id":"` + src.ID + `","url":"` + page.URL + `/article"}`
	if rec := doIngest(h, body, "tok"); rec.Code != http.StatusOK {
		t.Fatalf("ingest status = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := runner.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	arts, err := st.Artifacts.List(ctx)
	if err != nil || len(arts) != 1 {
		t.Fatalf("artifacts = %v (err %v), want one", arts, err)
	}
	a := arts[0]
	if !strings.Contains(a.BodyMarkdown, "Fetched body.") {
		t.Errorf("artifact body was not fetched: %q", a.BodyMarkdown)
	}
	if a.Status != "ready" {
		t.Errorf("artifact status = %q, want ready after the pipeline completes", a.Status)
	}
	if ens, _ := st.Enrichments.ListForArtifact(ctx, a.ID); len(ens) != 1 || ens[0].Kind != "fetch" {
		t.Errorf("enrichments = %v, want one 'fetch' row", ens)
	}
}

// countingEnqueuer records how many times the pipeline expansion is invoked.
type countingEnqueuer struct{ n int }

func (c *countingEnqueuer) EnqueueTx(_ context.Context, _ *store.Store, _ string) error {
	c.n++
	return nil
}

func TestIngest_ReingestDoesNotRerunPipeline(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	enq := &countingEnqueuer{}
	srv := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", DefaultPrivacy: "personal", Logger: discardLogger(), Enqueue: enq})
	h := srv.Handler()

	src := &store.Source{Kind: "PUSH", Connector: "x"}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}

	body := `{"source_id":"` + src.ID + `","url":"https://example.com/same"}`
	for i := 0; i < 2; i++ {
		if rec := doIngest(h, body, "tok"); rec.Code != http.StatusOK {
			t.Fatalf("ingest %d: status %d: %s", i, rec.Code, rec.Body.String())
		}
	}

	// One artifact, two encounters, but the pipeline expanded only once (insert).
	arts, _ := st.Artifacts.List(ctx)
	if len(arts) != 1 {
		t.Fatalf("artifacts = %d, want 1 (same URL dedups)", len(arts))
	}
	n, _ := st.Encounters.CountForArtifact(ctx, arts[0].ID)
	if n != 2 {
		t.Errorf("encounters = %d, want 2 (each ingest is a sighting)", n)
	}
	if enq.n != 1 {
		t.Errorf("pipeline enqueued %d times, want 1 (re-ingest of unchanged content must not re-run)", enq.n)
	}
}

func doIngest(h http.Handler, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

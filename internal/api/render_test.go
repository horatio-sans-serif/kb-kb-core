package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"kb/internal/events"
	"kb/internal/kb"
	"kb/internal/render"
	"kb/internal/store"
)

// renderTestServer wires a real render.Service with the dependency-free Markdown
// renderer writing into a temp dir.
func renderTestServer(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st := newStore(t)
	reg := render.NewRegistry()
	reg.Register(render.NewMarkdown(t.TempDir()))
	svc := render.NewService(st, reg)
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger(), Render: svc}).Handler()
	return st, h
}

func seedArtifact(t *testing.T, st *store.Store, hash, title, body string, tier kb.PrivacyTier) string {
	t.Helper()
	a, err := st.Artifacts.UpsertByContentHash(context.Background(),
		&kb.Artifact{Type: "note", Title: title, BodyMarkdown: body, ContentHash: &hash, PrivacyTier: tier})
	if err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	return a.ID
}

func TestRender_CreateAndList(t *testing.T) {
	st, h := renderTestServer(t)
	id1 := seedArtifact(t, st, "h1", "First", "alpha", kb.Public)
	id2 := seedArtifact(t, st, "h2", "Second", "beta", kb.Personal)

	body := `{"format":"markdown","artifact_ids":["` + id1 + `","` + id2 + `"],"title":"My Export"}`
	rec := doPost(h, "/render", body, "tok")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ID          string   `json:"id"`
		Format      string   `json:"format"`
		ArtifactIDs []string `json:"artifact_ids"`
		OutputPath  string   `json:"output_path"`
		PrivacyMax  string   `json:"privacy_max"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID == "" || got.Format != "markdown" || len(got.ArtifactIDs) != 2 || got.PrivacyMax != "personal" {
		t.Errorf("render record = %+v", got)
	}
	if b, err := os.ReadFile(got.OutputPath); err != nil || len(b) == 0 {
		t.Errorf("output file not written: %v", err)
	}

	lrec := doGet(h, "/renders", "tok")
	if lrec.Code != http.StatusOK {
		t.Fatalf("list status = %d", lrec.Code)
	}
	var list struct {
		Renders []struct {
			ID string `json:"id"`
		} `json:"renders"`
	}
	if err := json.Unmarshal(lrec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Renders) != 1 || list.Renders[0].ID != got.ID {
		t.Errorf("list = %+v, want the created render", list.Renders)
	}
}

func TestRender_UnknownFormatIs400(t *testing.T) {
	st, h := renderTestServer(t)
	id := seedArtifact(t, st, "h", "T", "b", kb.Public)
	rec := doPost(h, "/render", `{"format":"rtf","artifact_ids":["`+id+`"]}`, "tok")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown format: %s", rec.Code, rec.Body.String())
	}
}

func TestRender_InvalidCeilingIs400(t *testing.T) {
	st, h := renderTestServer(t)
	id := seedArtifact(t, st, "h", "T", "b", kb.Public)
	rec := doPost(h, "/render", `{"format":"markdown","artifact_ids":["`+id+`"],"privacy_ceiling":"bogus"}`, "tok")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an invalid ceiling", rec.Code)
	}
}

func TestRender_EmptyArtifactsIs400(t *testing.T) {
	_, h := renderTestServer(t)
	rec := doPost(h, "/render", `{"format":"markdown","artifact_ids":[]}`, "tok")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for no artifacts", rec.Code)
	}
}

func TestRender_DefaultCeilingExcludesSensitive(t *testing.T) {
	// With no privacy_ceiling, the export must fail safe: public/personal in,
	// sensitive/restricted out.
	st, h := renderTestServer(t)
	pub := seedArtifact(t, st, "hp", "Public", "open", kb.Public)
	sens := seedArtifact(t, st, "hs", "Secret", "classified", kb.Sensitive)

	rec := doPost(h, "/render", `{"format":"markdown","artifact_ids":["`+pub+`","`+sens+`"]}`, "tok")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ArtifactIDs []string `json:"artifact_ids"`
		PrivacyMax  string   `json:"privacy_max"`
		OutputPath  string   `json:"output_path"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.ArtifactIDs) != 1 || got.ArtifactIDs[0] != pub || got.PrivacyMax != "public" {
		t.Errorf("default ceiling should keep only the public artifact, got %+v", got)
	}
	if b, _ := os.ReadFile(got.OutputPath); strings.Contains(string(b), "classified") {
		t.Error("sensitive content leaked into a default-ceiling export")
	}
}

func TestRender_NoEligibleArtifactsIs422(t *testing.T) {
	st, h := renderTestServer(t)
	sens := seedArtifact(t, st, "hs", "Secret", "x", kb.Sensitive)
	// Only a sensitive artifact, default (personal) ceiling => nothing eligible.
	rec := doPost(h, "/render", `{"format":"markdown","artifact_ids":["`+sens+`"]}`, "tok")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 when nothing is eligible: %s", rec.Code, rec.Body.String())
	}
}

func TestRender_BusyIs503(t *testing.T) {
	st := newStore(t)
	id := seedArtifact(t, st, "h", "T", "b", kb.Public)
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger(),
		Render: fakeRenderSvc{err: render.ErrBusy}}).Handler()
	rec := doPost(h, "/render", `{"format":"markdown","artifact_ids":["`+id+`"]}`, "tok")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("busy render = %d (Retry-After %q), want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestRender_RoutesRequireAuth(t *testing.T) {
	_, h := renderTestServer(t)
	if rec := doPost(h, "/render", `{"format":"markdown","artifact_ids":["x"]}`, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /render without token = %d, want 401", rec.Code)
	}
	if rec := doGet(h, "/renders", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /renders without token = %d, want 401", rec.Code)
	}
}

func TestRender_ListWorksWithoutRenderService(t *testing.T) {
	// Render history is independent of a configured renderer (it reads the store).
	st := newStore(t)
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger()}).Handler()
	rec := doGet(h, "/renders", "tok")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /renders with no Render service = %d, want 200", rec.Code)
	}
}

func TestRender_OversizeBodyIs413(t *testing.T) {
	_, h := renderTestServer(t)
	big := `{"format":"markdown","title":"` + strings.Repeat("a", 70<<10) + `","artifact_ids":["x"]}`
	if rec := doPost(h, "/render", big, "tok"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize body = %d, want 413", rec.Code)
	}
}

func TestRender_NotConfiguredIs503(t *testing.T) {
	st := newStore(t)
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger()}).Handler() // no Render
	rec := doPost(h, "/render", `{"format":"markdown","artifact_ids":["x"]}`, "tok")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when rendering is not configured", rec.Code)
	}
}

// fakeRenderSvc lets tests drive the error-mapping branches.
type fakeRenderSvc struct {
	err     error
	formats []string
}

func (f fakeRenderSvc) Render(context.Context, render.Request) (*kb.RenderRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &kb.RenderRecord{ID: "x", Format: "markdown"}, nil
}
func (f fakeRenderSvc) Formats() []string { return f.formats }

func TestRender_ToolUnavailableIs503(t *testing.T) {
	st := newStore(t)
	id := seedArtifact(t, st, "h", "T", "b", kb.Public)
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger(),
		Render: fakeRenderSvc{err: render.ErrRendererUnavailable}}).Handler()
	rec := doPost(h, "/render", `{"format":"pdf","artifact_ids":["`+id+`"]}`, "tok")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when the converter tool is missing: %s", rec.Code, rec.Body.String())
	}
}

package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"kb/internal/events"
	"kb/internal/kb"
	"kb/internal/render"
	"kb/internal/store"
	"kb/internal/surface"
)

func digestTestServer(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st := newStore(t)
	reg := render.NewRegistry()
	reg.Register(render.NewMarkdown(t.TempDir()))
	h := NewServer(Deps{
		Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger(),
		Render: render.NewService(st, reg), Surface: surface.New(st),
	}).Handler()
	return st, h
}

func TestDigest_RendersSurfacedArtifactsInRankOrder(t *testing.T) {
	st, h := digestTestServer(t)
	hi := seedReady(t, st, "ha", "High", kb.Public, 0.9)
	lo := seedReady(t, st, "hl", "Low", kb.Public, 0.3)

	rec := doPost(h, "/digest", `{"format":"markdown","title":"Daily"}`, "tok")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ArtifactIDs []string `json:"artifact_ids"`
		Format      string   `json:"format"`
		OutputPath  string   `json:"output_path"`
		SourceQuery string   `json:"source_query"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Surfaced by relevance: High before Low.
	if len(got.ArtifactIDs) != 2 || got.ArtifactIDs[0] != hi || got.ArtifactIDs[1] != lo {
		t.Errorf("digest order = %v, want [High, Low]", got.ArtifactIDs)
	}
	if got.SourceQuery != "digest" {
		t.Errorf("source_query = %q, want digest", got.SourceQuery)
	}
	if b, err := os.ReadFile(got.OutputPath); err != nil || !strings.Contains(string(b), "# Daily") {
		t.Errorf("digest file missing or wrong: %v", err)
	}
}

func TestDigest_DefaultCeilingExcludesSensitive(t *testing.T) {
	st, h := digestTestServer(t)
	pub := seedReady(t, st, "hp", "Pub", kb.Public, 0.9)
	seedReady(t, st, "hs", "Secret", kb.Sensitive, 0.95)

	rec := doPost(h, "/digest", `{"format":"markdown"}`, "tok")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ArtifactIDs []string `json:"artifact_ids"`
		PrivacyMax  string   `json:"privacy_max"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.ArtifactIDs) != 1 || got.ArtifactIDs[0] != pub || got.PrivacyMax != "public" {
		t.Errorf("digest should exclude the sensitive artifact by default: %+v", got)
	}
}

func TestDigest_EmptyIs422(t *testing.T) {
	_, h := digestTestServer(t) // no artifacts seeded
	rec := doPost(h, "/digest", `{"format":"markdown"}`, "tok")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 when nothing is eligible to digest", rec.Code)
	}
}

func TestDigest_UnknownFormatIs400(t *testing.T) {
	st, h := digestTestServer(t)
	seedReady(t, st, "ha", "High", kb.Public, 0.9)
	rec := doPost(h, "/digest", `{"format":"rtf"}`, "tok")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown format", rec.Code)
	}
}

func TestDigest_NotConfiguredIs503(t *testing.T) {
	st := newStore(t)
	// Render configured but Surface missing → 503.
	reg := render.NewRegistry()
	reg.Register(render.NewMarkdown(t.TempDir()))
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger(), Render: render.NewService(st, reg)}).Handler()
	if rec := doPost(h, "/digest", `{"format":"markdown"}`, "tok"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when surfacing is not configured", rec.Code)
	}
}

func TestDigest_RequiresAuth(t *testing.T) {
	_, h := digestTestServer(t)
	if rec := doPost(h, "/digest", `{"format":"markdown"}`, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without a token", rec.Code)
	}
}

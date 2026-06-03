package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"kb/internal/events"
	"kb/internal/kb"
	"kb/internal/store"
	"kb/internal/surface"
)

func surfaceTestServer(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st := newStore(t)
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger(), Surface: surface.New(st)}).Handler()
	return st, h
}

func seedReady(t *testing.T, st *store.Store, hash, title string, tier kb.PrivacyTier, score float64) string {
	t.Helper()
	ctx := context.Background()
	a, err := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: "note", Title: title, BodyMarkdown: "b", ContentHash: &hash, PrivacyTier: tier})
	if err != nil {
		t.Fatalf("seed %s: %v", title, err)
	}
	rdy := kb.StatusReady
	if err := st.Artifacts.Patch(ctx, a.ID, kb.ArtifactPatch{Status: &rdy, RelevanceScore: &score}); err != nil {
		t.Fatalf("patch %s: %v", title, err)
	}
	return a.ID
}

func TestSurface_RanksAndDefaultsConservative(t *testing.T) {
	st, h := surfaceTestServer(t)
	hi := seedReady(t, st, "ha", "High", kb.Public, 0.9)
	seedReady(t, st, "hs", "Secret", kb.Sensitive, 0.95) // excluded by the default ceiling
	lo := seedReady(t, st, "hl", "Low", kb.Public, 0.3)

	rec := doGet(h, "/surface", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Surface []struct {
			ArtifactID string  `json:"artifact_id"`
			Title      string  `json:"title"`
			Score      float64 `json:"score"`
		} `json:"surface"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Surface) != 2 || out.Surface[0].ArtifactID != hi || out.Surface[1].ArtifactID != lo {
		t.Errorf("surface = %+v, want [High, Low] (sensitive excluded by default ceiling)", out.Surface)
	}
	if out.Surface[0].Score != 0.9 {
		t.Errorf("score not returned: %+v", out.Surface[0])
	}
}

func TestSurface_LimitParam(t *testing.T) {
	st, h := surfaceTestServer(t)
	seedReady(t, st, "ha", "High", kb.Public, 0.9)
	seedReady(t, st, "hl", "Low", kb.Public, 0.3)
	rec := doGet(h, "/surface?limit=1", "tok")
	var out struct {
		Surface []json.RawMessage `json:"surface"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Surface) != 1 {
		t.Errorf("limit=1 returned %d items", len(out.Surface))
	}
}

func TestSurface_NotConfiguredIs503(t *testing.T) {
	st := newStore(t)
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger()}).Handler()
	if rec := doGet(h, "/surface", "tok"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when surfacing is not configured", rec.Code)
	}
}

func TestSurface_InvalidCeilingIs400(t *testing.T) {
	_, h := surfaceTestServer(t)
	if rec := doGet(h, "/surface?privacy_ceiling=bogus", "tok"); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an invalid ceiling", rec.Code)
	}
}

func TestSurface_InvalidSinceIs400(t *testing.T) {
	_, h := surfaceTestServer(t)
	if rec := doGet(h, "/surface?since=not-a-time", "tok"); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an invalid since", rec.Code)
	}
}

func TestSurface_RequiresAuth(t *testing.T) {
	_, h := surfaceTestServer(t)
	if rec := doGet(h, "/surface", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without a token", rec.Code)
	}
}

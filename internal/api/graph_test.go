package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"kb/internal/events"
	"kb/internal/kb"
	"kb/internal/store"
)

func graphTestServer(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st := newStore(t)
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger()}).Handler()
	return st, h
}

func TestGraph_ConnectedAndEntities(t *testing.T) {
	ctx := context.Background()
	st, h := graphTestServer(t)
	a := seedReady(t, st, "ha", "Start", kb.Public, 0.5)
	b := seedReady(t, st, "hb", "One hop", kb.Public, 0.5)
	c := seedReady(t, st, "hc", "Two hops", kb.Public, 0.5)

	// a mentions Go; chain a—b—c.
	goID, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "technology", Name: "Go"})
	_ = st.Graph.LinkArtifactEntity(ctx, a, goID)
	_ = st.Graph.AddLink(ctx, a, b, "shares_entity", 1.0)
	_ = st.Graph.AddLink(ctx, b, c, "references", 1.0)

	rec := doGet(h, "/graph/"+a+"?hops=2", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ArtifactID string `json:"artifact_id"`
		Entities   []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"entities"`
		Connected []struct {
			ArtifactID string `json:"artifact_id"`
			Title      string `json:"title"`
		} `json:"connected"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ArtifactID != a {
		t.Errorf("artifact_id = %q, want %q", out.ArtifactID, a)
	}
	if len(out.Entities) != 1 || out.Entities[0].Name != "Go" {
		t.Errorf("entities = %+v, want [Go]", out.Entities)
	}
	if len(out.Connected) != 2 {
		t.Errorf("connected within 2 hops = %d, want 2 (b, c)", len(out.Connected))
	}
}

func TestGraph_DefaultHops(t *testing.T) {
	ctx := context.Background()
	st, h := graphTestServer(t)
	a := seedReady(t, st, "ha", "Start", kb.Public, 0.5)
	b := seedReady(t, st, "hb", "Near", kb.Public, 0.5)
	c := seedReady(t, st, "hc", "Far", kb.Public, 0.5)
	_ = st.Graph.AddLink(ctx, a, b, "references", 1.0)
	_ = st.Graph.AddLink(ctx, b, c, "references", 1.0)

	rec := doGet(h, "/graph/"+a, "tok") // default 1 hop
	var out struct {
		Connected []json.RawMessage `json:"connected"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Connected) != 1 {
		t.Errorf("default hops should be 1 → only b, got %d connected", len(out.Connected))
	}
}

func TestGraph_UnknownArtifactIs404(t *testing.T) {
	_, h := graphTestServer(t)
	if rec := doGet(h, "/graph/01HMISSING", "tok"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unknown artifact", rec.Code)
	}
}

func TestGraph_InvalidHopsIs400(t *testing.T) {
	st, h := graphTestServer(t)
	a := seedReady(t, st, "ha", "S", kb.Public, 0.5)
	for _, bad := range []string{"0", "-1", "9", "abc"} {
		if rec := doGet(h, "/graph/"+a+"?hops="+bad, "tok"); rec.Code != http.StatusBadRequest {
			t.Errorf("hops=%s = %d, want 400", bad, rec.Code)
		}
	}
}

func TestGraph_RequiresAuth(t *testing.T) {
	st, h := graphTestServer(t)
	a := seedReady(t, st, "ha", "S", kb.Public, 0.5)
	if rec := doGet(h, "/graph/"+a, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without a token", rec.Code)
	}
}

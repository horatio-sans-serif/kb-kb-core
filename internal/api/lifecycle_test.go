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

func TestLifecycle_Status(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger()}).Handler()

	// Two active artifacts from a budgeted source.
	budget := 5
	src := &store.Source{Kind: "PULL", Connector: "rss", StorageBudget: &budget}
	_ = st.Sources.Create(ctx, src)
	for _, hash := range []string{"h1", "h2"} {
		id := seedReady(t, st, hash, "t", kb.Public, 0.5)
		_ = st.Encounters.Record(ctx, &store.Encounter{ArtifactID: id, SourceID: src.ID, SeenAt: store.Now()})
	}

	rec := doGet(h, "/lifecycle", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Tiers struct {
			Active  int `json:"active"`
			Archive int `json:"archive"`
			Purged  int `json:"purged"`
		} `json:"tiers"`
		Sources []struct {
			ID          string `json:"id"`
			Connector   string `json:"connector"`
			Budget      int    `json:"budget"`
			ActiveCount int    `json:"active_count"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Tiers.Active != 2 {
		t.Errorf("active tier count = %d, want 2", out.Tiers.Active)
	}
	if len(out.Sources) != 1 || out.Sources[0].Budget != 5 || out.Sources[0].ActiveCount != 2 {
		t.Errorf("budgeted sources = %+v, want one with budget 5, 2 active", out.Sources)
	}
}

func TestLifecycle_RequiresAuth(t *testing.T) {
	st := newStore(t)
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger()}).Handler()
	if rec := doGet(h, "/lifecycle", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without a token", rec.Code)
	}
}

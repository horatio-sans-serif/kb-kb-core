package api

import (
	"context"
	"encoding/json"
	"testing"

	"kb/internal/events"
	"kb/internal/store"
)

func TestSources_DerivedHealthStatus(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	reg := fakeRegistry{have: map[string]bool{"rss": true}}
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger(), Connectors: reg}).Handler()

	sched := "{\"every_seconds\":60}"
	src := &store.Source{Kind: "PULL", Connector: "rss", Schedule: &sched}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Never polled → unknown status, no next fetch.
	var list struct {
		Sources []struct {
			Status              string `json:"status"`
			ConsecutiveFailures int    `json:"consecutive_failures"`
			NextFetchAt         string `json:"next_fetch_at"`
		} `json:"sources"`
	}
	_ = json.Unmarshal(doGet(h, "/sources", "tok").Body.Bytes(), &list)
	if len(list.Sources) != 1 || list.Sources[0].Status != "unknown" {
		t.Fatalf("fresh source status = %+v, want unknown", list.Sources)
	}

	// Record a failing health snapshot with a fetch time.
	failing := `{"ok":false,"detail":"down","consecutive_failures":3}`
	last := store.Now()
	if err := st.Sources.UpdateFetchState(ctx, src.ID, nil, &last, &failing); err != nil {
		t.Fatalf("update: %v", err)
	}

	_ = json.Unmarshal(doGet(h, "/sources", "tok").Body.Bytes(), &list)
	s := list.Sources[0]
	if s.Status != "failing" {
		t.Errorf("status = %q, want failing (3 consecutive failures)", s.Status)
	}
	if s.ConsecutiveFailures != 3 {
		t.Errorf("consecutive_failures = %d, want 3", s.ConsecutiveFailures)
	}
	if s.NextFetchAt == "" {
		t.Errorf("next_fetch_at should be set once a source has been polled")
	}
}

func TestSources_DegradedStatus(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	reg := fakeRegistry{have: map[string]bool{"rss": true}}
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger(), Connectors: reg}).Handler()
	src := &store.Source{Kind: "PULL", Connector: "rss"}
	_ = st.Sources.Create(ctx, src)
	degraded := `{"ok":false,"consecutive_failures":1}`
	last := store.Now()
	_ = st.Sources.UpdateFetchState(ctx, src.ID, nil, &last, &degraded)

	var list struct {
		Sources []struct {
			Status string `json:"status"`
		} `json:"sources"`
	}
	_ = json.Unmarshal(doGet(h, "/sources", "tok").Body.Bytes(), &list)
	if list.Sources[0].Status != "degraded" {
		t.Errorf("status = %q, want degraded (1 failure, below threshold)", list.Sources[0].Status)
	}
}

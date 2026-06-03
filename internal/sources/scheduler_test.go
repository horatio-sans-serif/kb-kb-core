package sources

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"kb/internal/events"
	"kb/internal/ingest"
	"kb/internal/kb"
	"kb/internal/store"
	"kb/internal/storetest"
)

type stubConnector struct {
	items   []kb.RawItem
	cursor  kb.Cursor
	err     error
	fetched int
}

func (s *stubConnector) Name() string        { return "stub" }
func (s *stubConnector) Kind() kb.SourceKind { return kb.Pull }
func (s *stubConnector) Fetch(context.Context, *kb.Source, kb.Cursor) ([]kb.RawItem, kb.Cursor, error) {
	s.fetched++
	return s.items, s.cursor, s.err
}
func (s *stubConnector) HealthCheck(context.Context, *kb.Source) (kb.Health, error) {
	return kb.Health{OK: true}, nil
}

func newScheduler(t *testing.T, conn kb.SourceConnector) (*store.Store, *Scheduler) {
	t.Helper()
	st := storetest.New(t)
	reg := NewRegistry()
	reg.Register("stub", conn)
	return st, NewScheduler(st, reg, ingest.New(st, nil, nil, events.NewBus()), nil)
}

func TestScheduler_PollsAndIngests(t *testing.T) {
	ctx := context.Background()
	conn := &stubConnector{
		items:  []kb.RawItem{{URL: "https://e.com/a", Title: "A"}, {URL: "https://e.com/b", Title: "B"}},
		cursor: "cur-1",
	}
	st, sch := newScheduler(t, conn)
	src := &store.Source{Kind: "PULL", Connector: "stub", DefaultPrivacy: "personal"}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}

	n, err := sch.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 2 {
		t.Errorf("ingested %d, want 2", n)
	}
	if arts, _ := st.Artifacts.List(ctx); len(arts) != 2 {
		t.Errorf("artifacts = %d, want 2", len(arts))
	}

	got, _ := st.Sources.Get(ctx, src.ID)
	if got.Cursor == nil || *got.Cursor != "cur-1" {
		t.Errorf("cursor = %v, want cur-1", got.Cursor)
	}
	if got.LastFetchedAt == nil {
		t.Error("last_fetched_at not recorded")
	}
	if got.Health == nil || !strings.Contains(*got.Health, `"ok":true`) {
		t.Errorf("health = %v, want ok:true", got.Health)
	}
}

func TestScheduler_SkipsNotDue(t *testing.T) {
	ctx := context.Background()
	conn := &stubConnector{items: []kb.RawItem{{URL: "https://e.com/a", Title: "A"}}, cursor: "c"}
	st, sch := newScheduler(t, conn)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	sch.now = func() time.Time { return now }

	sched := `{"every_seconds":3600}`
	src := &store.Source{Kind: "PULL", Connector: "stub", DefaultPrivacy: "personal", Schedule: &sched}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := sch.Tick(ctx); err != nil { // first tick: never fetched → due
		t.Fatalf("tick 1: %v", err)
	}
	if _, err := sch.Tick(ctx); err != nil { // same instant: last_fetched=now, ttl 1h → not due
		t.Fatalf("tick 2: %v", err)
	}
	if conn.fetched != 1 {
		t.Errorf("fetched %d times, want 1 (second tick is within the TTL)", conn.fetched)
	}
}

func TestScheduler_RecordsHealthOnFetchError(t *testing.T) {
	ctx := context.Background()
	conn := &stubConnector{err: errors.New("feed unreachable")}
	st, sch := newScheduler(t, conn)
	src := &store.Source{Kind: "PULL", Connector: "stub", DefaultPrivacy: "personal"}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create: %v", err)
	}

	n, err := sch.Tick(ctx)
	if err == nil {
		t.Error("expected the joined fetch error")
	}
	if n != 0 {
		t.Errorf("ingested %d, want 0 on a fetch error", n)
	}
	got, _ := st.Sources.Get(ctx, src.ID)
	if got.Cursor != nil {
		t.Errorf("cursor = %v, want nil kept (no successful fetch)", got.Cursor)
	}
	if got.Health == nil || !strings.Contains(*got.Health, `"ok":false`) {
		t.Errorf("health = %v, want ok:false", got.Health)
	}
	if got.LastFetchedAt == nil {
		t.Error("last_fetched_at should record the failed attempt")
	}
}

func TestScheduler_UnregisteredConnectorSkipped(t *testing.T) {
	ctx := context.Background()
	st, sch := newScheduler(t, &stubConnector{})
	src := &store.Source{Kind: "PULL", Connector: "nonexistent", DefaultPrivacy: "personal"}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create: %v", err)
	}
	if n, err := sch.Tick(ctx); err != nil || n != 0 {
		t.Errorf("tick = %d, %v; want 0, nil (unknown connector is skipped, not fatal)", n, err)
	}
}

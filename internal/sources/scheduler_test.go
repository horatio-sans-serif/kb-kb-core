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
	bus := events.NewBus()
	return st, NewScheduler(st, reg, ingest.New(st, nil, nil, bus), bus, nil)
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

func TestScheduler_TracksConsecutiveFailuresAndAlerts(t *testing.T) {
	ctx := context.Background()
	conn := &stubConnector{err: errors.New("feed unreachable")}
	st := storetest.New(t)
	reg := NewRegistry()
	reg.Register("stub", conn)
	bus := events.NewBus()
	sub, cancel := bus.Subscribe()
	defer cancel()
	sch := NewScheduler(st, reg, ingest.New(st, nil, nil, bus), bus, nil)

	src := &store.Source{Kind: "PULL", Connector: "stub", DefaultPrivacy: "personal"}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create: %v", err)
	}

	now := time.Now()
	// Poll directly, re-reading the source so each poll sees the prior health.
	for i := 1; i <= failureAlertThreshold; i++ {
		cur, _ := st.Sources.Get(ctx, src.ID)
		if _, err := sch.poll(ctx, conn, cur, now); err == nil {
			t.Fatalf("poll %d: expected the fetch error", i)
		}
		got, _ := st.Sources.Get(ctx, src.ID)
		if !strings.Contains(*got.Health, `"consecutive_failures":`+itoa(i)) {
			t.Errorf("after failure %d, health = %s, want consecutive_failures %d", i, *got.Health, i)
		}
	}

	// source.failed published exactly once, at the threshold.
	select {
	case e := <-sub:
		if e.Type != events.SourceFailed {
			t.Errorf("event = %q, want source.failed", e.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no source.failed event after reaching the failure threshold")
	}

	// A success resets the consecutive-failure count.
	conn.err = nil
	cur, _ := st.Sources.Get(ctx, src.ID)
	if _, err := sch.poll(ctx, conn, cur, now); err != nil {
		t.Fatalf("recovery poll: %v", err)
	}
	got, _ := st.Sources.Get(ctx, src.ID)
	if !strings.Contains(*got.Health, `"ok":true`) || strings.Contains(*got.Health, `"consecutive_failures"`) {
		t.Errorf("after recovery, health = %s, want ok:true and no consecutive_failures", *got.Health)
	}
}

func itoa(n int) string { return string(rune('0' + n)) }

func TestScheduler_BadURLFallsBackToNote(t *testing.T) {
	ctx := context.Background()
	// An item with an uncanonicalizable URL but a real body must become a note,
	// not be dropped.
	conn := &stubConnector{items: []kb.RawItem{{URL: "not a url", Title: "T", Body: "real content"}}, cursor: "c"}
	st, sch := newScheduler(t, conn)
	src := &store.Source{Kind: "PULL", Connector: "stub", DefaultPrivacy: "personal"}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create: %v", err)
	}
	if n, err := sch.Tick(ctx); err != nil || n != 1 {
		t.Fatalf("tick = %d, %v; want 1, nil", n, err)
	}
	arts, _ := st.Artifacts.List(ctx)
	if len(arts) != 1 || arts[0].Type != "note" || arts[0].BodyMarkdown != "real content" {
		t.Errorf("artifact = %+v, want a note with the body (URL fell back)", arts)
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

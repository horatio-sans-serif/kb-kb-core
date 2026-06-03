package review

import (
	"context"
	"errors"
	"testing"
	"time"

	"kb/internal/kb"
	"kb/internal/store"
	"kb/internal/storetest"
)

func mkArtifact(t *testing.T, st *store.Store, hash string) string {
	t.Helper()
	a, err := st.Artifacts.UpsertByContentHash(context.Background(),
		&kb.Artifact{Type: "note", Title: "Topic", BodyMarkdown: "b", ContentHash: &hash, PrivacyTier: kb.Public})
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	return a.ID
}

func TestService_RecordSchedulesNextReview(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	id := mkArtifact(t, st, "h1")

	clock := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	svc := NewWithClock(st, func() time.Time { return clock })

	// First successful review: due in 1 day.
	rs, err := svc.Record(ctx, id, 5)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rs.Reviews != 1 || rs.NextReviewAt != store.FormatTime(clock.AddDate(0, 0, 1)) {
		t.Errorf("first review = %+v, want reviews 1, next +1 day", rs)
	}

	// Second successful review: due in 6 days.
	rs2, err := svc.Record(ctx, id, 5)
	if err != nil {
		t.Fatalf("record 2: %v", err)
	}
	if rs2.Reviews != 2 || rs2.NextReviewAt != store.FormatTime(clock.AddDate(0, 0, 6)) {
		t.Errorf("second review = %+v, want reviews 2, next +6 days", rs2)
	}
}

func TestService_RecordUnknownArtifact(t *testing.T) {
	st := storetest.New(t)
	svc := New(st)
	_, err := svc.Record(context.Background(), "01HMISSING", 5)
	if !errors.Is(err, ErrArtifactNotFound) {
		t.Errorf("Record(missing) = %v, want ErrArtifactNotFound", err)
	}
}

func TestService_DueListsArtifactsPastDue(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	id := mkArtifact(t, st, "h1")

	clock := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	svc := NewWithClock(st, func() time.Time { return clock })

	if _, err := svc.Record(ctx, id, 5); err != nil { // next review = 2025-06-02
		t.Fatalf("record: %v", err)
	}
	// Not yet due.
	if due, _ := svc.Due(ctx, 10); len(due) != 0 {
		t.Errorf("nothing should be due before the interval elapses, got %d", len(due))
	}
	// Advance past the interval.
	clock = clock.AddDate(0, 0, 2)
	due, err := svc.Due(ctx, 10)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 1 || due[0].ArtifactID != id || due[0].Title != "Topic" {
		t.Errorf("due = %+v, want the artifact with its title", due)
	}
}

func TestService_DueSkipsTombstoned(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	id := mkArtifact(t, st, "h1")
	clock := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	svc := NewWithClock(st, func() time.Time { return clock })

	if _, err := svc.Record(ctx, id, 5); err != nil {
		t.Fatalf("record: %v", err)
	}
	clock = clock.AddDate(0, 0, 5)
	if err := st.Artifacts.SoftDelete(ctx, id); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	due, err := svc.Due(ctx, 10)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("a tombstoned artifact must be dropped from the queue, got %d", len(due))
	}
}

func TestService_LongTermPerfectRecallStaysFuture(t *testing.T) {
	// Many perfect reviews must keep next_review_at in the FUTURE (the C1 overflow
	// would wrap it into the past). Exercised through the store round-trip.
	ctx := context.Background()
	st := storetest.New(t)
	id := mkArtifact(t, st, "h1")
	clock := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	svc := NewWithClock(st, func() time.Time { return clock })

	for i := range 40 {
		rs, err := svc.Record(ctx, id, 5)
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if rs.NextReviewAt <= store.FormatTime(clock) {
			t.Fatalf("review %d: next_review_at %q is not after now %q (overflow)", i, rs.NextReviewAt, store.FormatTime(clock))
		}
	}
}

func TestService_LapseThenRecover(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	id := mkArtifact(t, st, "h1")
	clock := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	svc := NewWithClock(st, func() time.Time { return clock })

	for range 4 {
		if _, err := svc.Record(ctx, id, 5); err != nil { // build up an interval
			t.Fatalf("record: %v", err)
		}
	}
	lapsed, err := svc.Record(ctx, id, 1) // lapse
	if err != nil {
		t.Fatalf("lapse: %v", err)
	}
	if lapsed.Reviews != 0 || lapsed.IntervalDays != 1 {
		t.Errorf("lapse via service = %+v, want reps 0 interval 1", lapsed)
	}
	recovered, err := svc.Record(ctx, id, 5) // recover
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered.Reviews != 1 || recovered.IntervalDays != 1 {
		t.Errorf("recover via service = %+v, want reps 1 interval 1", recovered)
	}
}

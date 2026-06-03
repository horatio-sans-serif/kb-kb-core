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

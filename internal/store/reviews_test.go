package store

import (
	"context"
	"errors"
	"testing"

	"kb/internal/kb"
)

func mkArtifact(t *testing.T, st *Store, hash string) string {
	t.Helper()
	a, err := st.Artifacts.UpsertByContentHash(context.Background(),
		&Artifact{Type: "note", Title: "t", BodyMarkdown: "b", ContentHash: &hash, PrivacyTier: kb.Public})
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	return a.ID
}

func TestReviewRepo_UpsertGet(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	id := mkArtifact(t, st, "h1")

	rs := &ReviewState{ArtifactID: id, NextReviewAt: "2025-06-10T00:00:00.000000000Z", IntervalDays: 6, Ease: 2.6, Reviews: 2}
	if err := st.Reviews.Upsert(ctx, rs); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := st.Reviews.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.IntervalDays != 6 || got.Ease != 2.6 || got.Reviews != 2 || got.NextReviewAt != rs.NextReviewAt {
		t.Errorf("round-trip = %+v", got)
	}

	// Upsert again updates in place (PK is artifact_id).
	rs.IntervalDays, rs.Reviews = 15, 3
	if err := st.Reviews.Upsert(ctx, rs); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got2, _ := st.Reviews.Get(ctx, id)
	if got2.IntervalDays != 15 || got2.Reviews != 3 {
		t.Errorf("update-in-place failed: %+v", got2)
	}
}

func TestReviewRepo_GetMissing(t *testing.T) {
	st := newTestStore(t)
	_, err := st.Reviews.Get(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
}

func TestReviewRepo_DueOrdersAndFilters(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	past := mkArtifact(t, st, "hp")
	soon := mkArtifact(t, st, "hs")
	future := mkArtifact(t, st, "hf")

	_ = st.Reviews.Upsert(ctx, &ReviewState{ArtifactID: future, NextReviewAt: "2030-01-01T00:00:00.000000000Z", Ease: 2.5})
	_ = st.Reviews.Upsert(ctx, &ReviewState{ArtifactID: past, NextReviewAt: "2020-01-01T00:00:00.000000000Z", Ease: 2.5})
	_ = st.Reviews.Upsert(ctx, &ReviewState{ArtifactID: soon, NextReviewAt: "2024-01-01T00:00:00.000000000Z", Ease: 2.5})

	due, err := st.Reviews.Due(ctx, "2025-01-01T00:00:00.000000000Z", 10)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	// past and soon are due (<= now), ordered by next_review_at asc; future excluded.
	if len(due) != 2 || due[0].ArtifactID != past || due[1].ArtifactID != soon {
		t.Errorf("due = %v, want [past, soon]", reviewIDs(due))
	}

	if one, _ := st.Reviews.Due(ctx, "2025-01-01T00:00:00.000000000Z", 1); len(one) != 1 || one[0].ArtifactID != past {
		t.Errorf("limit not applied: %v", reviewIDs(one))
	}
}

func reviewIDs(rs []*ReviewState) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ArtifactID
	}
	return out
}

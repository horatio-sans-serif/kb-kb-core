package store

import (
	"context"
	"testing"

	"kb/internal/kb"
)

func scored(t *testing.T, st *Store, hash, title string, tier kb.PrivacyTier, score float64, status string, firstSeen string) string {
	t.Helper()
	ctx := context.Background()
	a := &Artifact{Type: "note", Title: title, BodyMarkdown: "b", ContentHash: &hash, PrivacyTier: tier}
	if firstSeen != "" {
		a.FirstSeenAt = firstSeen
	}
	got, err := st.Artifacts.UpsertByContentHash(ctx, a)
	if err != nil {
		t.Fatalf("upsert %s: %v", title, err)
	}
	st2 := kb.ArtifactStatus(status)
	if err := st.Artifacts.Patch(ctx, got.ID, kb.ArtifactPatch{Status: &st2, RelevanceScore: &score}); err != nil {
		t.Fatalf("patch %s: %v", title, err)
	}
	return got.ID
}

func TestSurface_RanksByRelevanceAndFilters(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	a := scored(t, st, "ha", "A high", kb.Public, 0.9, "ready", "")
	bID := scored(t, st, "hb", "B low", kb.Public, 0.5, "ready", "")
	c := scored(t, st, "hc", "C mid sensitive", kb.Sensitive, 0.7, "ready", "")
	_ = scored(t, st, "hd", "D not ready", kb.Public, 0.95, "ingested", "") // excluded by status

	// Ranked by score desc, ready only: A(0.9), C(0.7), B(0.5).
	got, err := st.Artifacts.Surface(ctx, SurfaceQuery{Status: "ready", Limit: 10})
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if len(got) != 3 || got[0].ID != a || got[1].ID != c || got[2].ID != bID {
		t.Errorf("ranking wrong: %v", ids(got))
	}

	// Privacy tier whitelist excludes the sensitive one.
	pub, err := st.Artifacts.Surface(ctx, SurfaceQuery{Status: "ready", Tiers: []string{"public", "personal"}, Limit: 10})
	if err != nil {
		t.Fatalf("surface tiers: %v", err)
	}
	if len(pub) != 2 || pub[0].ID != a || pub[1].ID != bID {
		t.Errorf("tier filter wrong: %v", ids(pub))
	}

	// Limit caps the result.
	top, _ := st.Artifacts.Surface(ctx, SurfaceQuery{Status: "ready", Limit: 1})
	if len(top) != 1 || top[0].ID != a {
		t.Errorf("limit wrong: %v", ids(top))
	}

	// MinScore floors the result.
	hi, _ := st.Artifacts.Surface(ctx, SurfaceQuery{Status: "ready", MinScore: 0.8, Limit: 10})
	if len(hi) != 1 || hi[0].ID != a {
		t.Errorf("min-score wrong: %v", ids(hi))
	}
}

func TestSurface_SinceFiltersNewArrivals(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	scored(t, st, "old", "Old", kb.Public, 0.9, "ready", "2020-01-01T00:00:00.000000000Z")
	newID := scored(t, st, "new", "New", kb.Public, 0.5, "ready", "2030-01-01T00:00:00.000000000Z")

	got, err := st.Artifacts.Surface(ctx, SurfaceQuery{Status: "ready", Since: "2025-01-01T00:00:00.000000000Z", Limit: 10})
	if err != nil {
		t.Fatalf("surface since: %v", err)
	}
	if len(got) != 1 || got[0].ID != newID {
		t.Errorf("since filter should keep only the new arrival: %v", ids(got))
	}
}

func ids(arts []*Artifact) []string {
	out := make([]string, len(arts))
	for i, a := range arts {
		out[i] = a.ID
	}
	return out
}

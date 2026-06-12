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
	got, err := st.Artifacts.Surface(ctx, SurfaceQuery{PrivacyCeiling: kb.Restricted, Status: "ready", Limit: 10})
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if len(got) != 3 || got[0].ID != a || got[1].ID != c || got[2].ID != bID {
		t.Errorf("ranking wrong: %v", ids(got))
	}

	// Privacy tier whitelist excludes the sensitive one.
	pub, err := st.Artifacts.Surface(ctx, SurfaceQuery{Status: "ready", PrivacyCeiling: kb.Personal, Limit: 10})
	if err != nil {
		t.Fatalf("surface tiers: %v", err)
	}
	if len(pub) != 2 || pub[0].ID != a || pub[1].ID != bID {
		t.Errorf("tier filter wrong: %v", ids(pub))
	}

	// Limit caps the result.
	top, _ := st.Artifacts.Surface(ctx, SurfaceQuery{PrivacyCeiling: kb.Restricted, Status: "ready", Limit: 1})
	if len(top) != 1 || top[0].ID != a {
		t.Errorf("limit wrong: %v", ids(top))
	}

	// MinScore floors the result.
	hi, _ := st.Artifacts.Surface(ctx, SurfaceQuery{PrivacyCeiling: kb.Restricted, Status: "ready", MinScore: 0.8, Limit: 10})
	if len(hi) != 1 || hi[0].ID != a {
		t.Errorf("min-score wrong: %v", ids(hi))
	}
}

func TestSurface_SinceFiltersNewArrivals(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	scored(t, st, "old", "Old", kb.Public, 0.9, "ready", "2020-01-01T00:00:00.000000000Z")
	newID := scored(t, st, "new", "New", kb.Public, 0.5, "ready", "2030-01-01T00:00:00.000000000Z")

	got, err := st.Artifacts.Surface(ctx, SurfaceQuery{PrivacyCeiling: kb.Restricted, Status: "ready", Since: "2025-01-01T00:00:00.000000000Z", Limit: 10})
	if err != nil {
		t.Fatalf("surface since: %v", err)
	}
	if len(got) != 1 || got[0].ID != newID {
		t.Errorf("since filter should keep only the new arrival: %v", ids(got))
	}
}

func TestSurface_ZeroScoreVsNull(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	zero := scored(t, st, "hz", "Zero", kb.Public, 0.0, "ready", "")
	// An unscored (NULL relevance_score) ready artifact: upsert, mark ready, no score.
	un, err := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", Title: "Unscored", ContentHash: ptrStr("hu"), PrivacyTier: kb.Public})
	if err != nil {
		t.Fatalf("upsert unscored: %v", err)
	}
	rdy := kb.StatusReady
	if err := st.Artifacts.Patch(ctx, un.ID, kb.ArtifactPatch{Status: &rdy}); err != nil {
		t.Fatalf("patch unscored: %v", err)
	}

	// Default min_score 0 includes the explicitly-0-scored artifact but never the
	// unscored (NULL) one.
	got, _ := st.Artifacts.Surface(ctx, SurfaceQuery{PrivacyCeiling: kb.Restricted, Status: "ready", Limit: 10})
	if len(got) != 1 || got[0].ID != zero {
		t.Errorf("min_score 0 should surface the 0-scored artifact only, got %v", ids(got))
	}
	// A positive floor excludes the 0-scored one.
	hi, _ := st.Artifacts.Surface(ctx, SurfaceQuery{PrivacyCeiling: kb.Restricted, Status: "ready", MinScore: 0.0001, Limit: 10})
	if len(hi) != 0 {
		t.Errorf("min_score 0.0001 should exclude a 0-scored artifact, got %v", ids(hi))
	}
}

func TestSurface_DeterministicTiebreak(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	score := 0.5
	rdy := kb.StatusReady

	// Equal score, different first_seen_at: newer first.
	older := &Artifact{ID: "01OLDER0000000000000000000", Type: "note", Title: "Older", ContentHash: ptrStr("ho"),
		PrivacyTier: kb.Public, FirstSeenAt: "2020-01-01T00:00:00.000000000Z"}
	newer := &Artifact{ID: "01NEWER0000000000000000000", Type: "note", Title: "Newer", ContentHash: ptrStr("hn"),
		PrivacyTier: kb.Public, FirstSeenAt: "2030-01-01T00:00:00.000000000Z"}
	for _, a := range []*Artifact{older, newer} {
		if _, err := st.Artifacts.UpsertByContentHash(ctx, a); err != nil {
			t.Fatalf("upsert %s: %v", a.Title, err)
		}
		if err := st.Artifacts.Patch(ctx, a.ID, kb.ArtifactPatch{Status: &rdy, RelevanceScore: &score}); err != nil {
			t.Fatalf("patch %s: %v", a.Title, err)
		}
	}
	got, _ := st.Artifacts.Surface(ctx, SurfaceQuery{PrivacyCeiling: kb.Restricted, Status: "ready", Limit: 10})
	if len(got) != 2 || got[0].ID != newer.ID || got[1].ID != older.ID {
		t.Errorf("equal score should order newer first_seen first: %v", ids(got))
	}

	// Equal score AND equal first_seen_at: ordered by id ascending.
	st2 := newTestStore(t)
	a1 := &Artifact{ID: "01AAA00000000000000000000A", Type: "note", Title: "A", ContentHash: ptrStr("h1"),
		PrivacyTier: kb.Public, FirstSeenAt: "2025-01-01T00:00:00.000000000Z"}
	a2 := &Artifact{ID: "01BBB00000000000000000000B", Type: "note", Title: "B", ContentHash: ptrStr("h2"),
		PrivacyTier: kb.Public, FirstSeenAt: "2025-01-01T00:00:00.000000000Z"}
	for _, a := range []*Artifact{a2, a1} { // insert out of id order
		if _, err := st2.Artifacts.UpsertByContentHash(ctx, a); err != nil {
			t.Fatalf("upsert %s: %v", a.Title, err)
		}
		if err := st2.Artifacts.Patch(ctx, a.ID, kb.ArtifactPatch{Status: &rdy, RelevanceScore: &score}); err != nil {
			t.Fatalf("patch %s: %v", a.Title, err)
		}
	}
	got2, _ := st2.Artifacts.Surface(ctx, SurfaceQuery{PrivacyCeiling: kb.Restricted, Status: "ready", Limit: 10})
	if len(got2) != 2 || got2[0].ID != a1.ID || got2[1].ID != a2.ID {
		t.Errorf("equal score+first_seen should order by id asc: %v", ids(got2))
	}
}

func ids(arts []*Artifact) []string {
	out := make([]string, len(arts))
	for i, a := range arts {
		out[i] = a.ID
	}
	return out
}

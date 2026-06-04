package lifecycle

import (
	"context"
	"testing"
	"time"

	"kb/internal/kb"
	"kb/internal/store"
	"kb/internal/storetest"
)

func TestClassify(t *testing.T) {
	p := DefaultPolicy()
	day := 24 * time.Hour
	cases := []struct {
		name  string
		score float64
		age   time.Duration
		want  kb.LifecycleTier
	}{
		{"fresh relevant", 0.9, 1 * day, kb.Active},
		{"old but relevant", 0.9, 400 * day, kb.Active},
		{"recent low score", 0.1, 1 * day, kb.Active}, // not old enough to archive
		{"old low score", 0.1, 60 * day, kb.Archive},
		{"ancient very low score", 0.01, 300 * day, kb.Purged},
		{"old very low but not ancient", 0.01, 60 * day, kb.Archive}, // below purge score but not old enough
	}
	for _, tc := range cases {
		if got := classify(tc.score, tc.age, p); got != tc.want {
			t.Errorf("%s: classify(%.2f, %v) = %q, want %q", tc.name, tc.score, tc.age, got, tc.want)
		}
	}
}

func seedScored(t *testing.T, st *store.Store, hash string, score float64, firstSeen string) string {
	t.Helper()
	ctx := context.Background()
	a := &store.Artifact{Type: "note", Title: "t", BodyMarkdown: "b", ContentHash: &hash, PrivacyTier: kb.Public, FirstSeenAt: firstSeen}
	got, err := st.Artifacts.UpsertByContentHash(ctx, a)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rdy := kb.StatusReady
	if err := st.Artifacts.Patch(ctx, got.ID, kb.ArtifactPatch{Status: &rdy, RelevanceScore: &score}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	return got.ID
}

func TestSweeper_TransitionsTiers(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	now := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	ts := func(daysAgo int) string { return store.FormatTime(now.AddDate(0, 0, -daysAgo)) }

	active := seedScored(t, st, "h1", 0.9, ts(400)) // relevant → stays active
	archive := seedScored(t, st, "h2", 0.1, ts(60)) // low + old → archive
	purge := seedScored(t, st, "h3", 0.01, ts(300)) // very low + ancient → purge (tombstone)
	young := seedScored(t, st, "h4", 0.01, ts(1))   // very low but young → active

	sw := NewWithPolicy(st, DefaultPolicy(), func() time.Time { return now })
	stats, err := sw.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if stats.Archived != 1 || stats.Purged != 1 {
		t.Errorf("stats = %+v, want 1 archived, 1 purged", stats)
	}

	tier := func(id string) kb.LifecycleTier {
		a, err := st.Artifacts.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		return a.LifecycleTier
	}
	if tier(active) != kb.Active {
		t.Errorf("relevant artifact = %q, want active", tier(active))
	}
	if tier(archive) != kb.Archive {
		t.Errorf("old low-score artifact = %q, want archive", tier(archive))
	}
	if tier(young) != kb.Active {
		t.Errorf("young artifact = %q, want active", tier(young))
	}
	// Purged artifact is tombstoned and tier=purged.
	p, _ := st.Artifacts.Get(ctx, purge)
	if p.LifecycleTier != kb.Purged || p.DeletedAt == nil {
		t.Errorf("purged artifact = %+v, want lifecycle purged + tombstoned", p)
	}
	if _, err := st.Artifacts.GetLive(ctx, purge); err == nil {
		t.Error("purged artifact should no longer be live")
	}
}

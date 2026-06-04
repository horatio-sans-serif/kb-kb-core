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

func TestSweeper_EnforceBudgets(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	budget := 2
	src := &store.Source{Kind: "PULL", Connector: "rss", StorageBudget: &budget}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("source: %v", err)
	}
	mk := func(hash string, score float64) string {
		id := seedScored(t, st, hash, score, store.Now())
		_ = st.Encounters.Record(ctx, &store.Encounter{ArtifactID: id, SourceID: src.ID, SeenAt: store.Now()})
		return id
	}
	lo := mk("h1", 0.1)
	mid := mk("h2", 0.2)
	hi1 := mk("h3", 0.8)
	hi2 := mk("h4", 0.9)

	sw := New(st)
	n, err := sw.EnforceBudgets(ctx)
	if err != nil {
		t.Fatalf("enforce: %v", err)
	}
	if n != 2 {
		t.Errorf("archived %d, want 2 (the over-budget excess)", n)
	}

	tier := func(id string) kb.LifecycleTier {
		a, _ := st.Artifacts.Get(ctx, id)
		return a.LifecycleTier
	}
	if tier(lo) != kb.Archive || tier(mid) != kb.Archive {
		t.Errorf("lowest-relevance excess should be archived: lo=%q mid=%q", tier(lo), tier(mid))
	}
	if tier(hi1) != kb.Active || tier(hi2) != kb.Active {
		t.Errorf("the most relevant should stay active: hi1=%q hi2=%q", tier(hi1), tier(hi2))
	}
	// Re-running is a no-op now that the source is within budget.
	if n2, _ := sw.EnforceBudgets(ctx); n2 != 0 {
		t.Errorf("second enforce archived %d, want 0 (now within budget)", n2)
	}
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

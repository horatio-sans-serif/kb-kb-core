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

func TestSweeper_DemoteOnly_NoReactivationFlap(t *testing.T) {
	// A healthy-score artifact archived for budget reasons must NOT be reactivated
	// by a relevance sweep (that would flap against the next budget pass).
	ctx := context.Background()
	st := storetest.New(t)
	now := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	id := seedScored(t, st, "h1", 0.9, store.FormatTime(now)) // high score, fresh
	arch := kb.Archive
	_ = st.Artifacts.Patch(ctx, id, kb.ArtifactPatch{LifecycleTier: &arch}) // budget-archived

	sw := NewWithPolicy(st, DefaultPolicy(), func() time.Time { return now })
	if _, err := sw.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	a, _ := st.Artifacts.Get(ctx, id)
	if a.LifecycleTier != kb.Archive {
		t.Errorf("sweep promoted an archived artifact to %q; it must stay archived (demote-only)", a.LifecycleTier)
	}
}

func TestSweeper_EngagementReactivates(t *testing.T) {
	// Engagement is the explicit promotion path: it returns an archived artifact to
	// active.
	ctx := context.Background()
	st := storetest.New(t)
	id := seedScored(t, st, "h1", 0.01, store.Now())
	arch := kb.Archive
	_ = st.Artifacts.Patch(ctx, id, kb.ArtifactPatch{LifecycleTier: &arch})

	if ok, err := st.Artifacts.RecordEngagement(ctx, id); err != nil || !ok {
		t.Fatalf("engage: ok=%v err=%v", ok, err)
	}
	a, _ := st.Artifacts.Get(ctx, id)
	if a.LifecycleTier != kb.Active {
		t.Errorf("engagement should reactivate, got %q", a.LifecycleTier)
	}
}

func TestSweeper_BudgetSharedArtifact(t *testing.T) {
	// An artifact encountered from two sources, one over budget, is archived for the
	// over-budget source (active-footprint cap, not ownership).
	ctx := context.Background()
	st := storetest.New(t)
	tight := 1
	a := &store.Source{Kind: "PULL", Connector: "rss", StorageBudget: &tight}
	b := &store.Source{Kind: "PUSH", Connector: "x"}
	_ = st.Sources.Create(ctx, a)
	_ = st.Sources.Create(ctx, b)

	lo := seedScored(t, st, "h1", 0.1, store.Now())
	hi := seedScored(t, st, "h2", 0.9, store.Now())
	for _, id := range []string{lo, hi} {
		_ = st.Encounters.Record(ctx, &store.Encounter{ArtifactID: id, SourceID: a.ID, SeenAt: store.Now()})
	}
	// The shared low-relevance one is also from b.
	_ = st.Encounters.Record(ctx, &store.Encounter{ArtifactID: lo, SourceID: b.ID, SeenAt: store.Now()})

	if n, err := New(st).EnforceBudgets(ctx); err != nil || n != 1 {
		t.Fatalf("enforce = (%d, %v), want (1, nil)", n, err)
	}
	got, _ := st.Artifacts.Get(ctx, lo)
	if got.LifecycleTier != kb.Archive {
		t.Errorf("the shared low-relevance artifact should be archived for the over-budget source, got %q", got.LifecycleTier)
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

func TestSweeper_PrunesAuditLog(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// Three audit entries: two older than the retention window, one within it.
	old1 := store.FormatTime(now.AddDate(0, 0, -400))
	old2 := store.FormatTime(now.AddDate(0, 0, -380))
	fresh := store.FormatTime(now.AddDate(0, 0, -10))
	if err := st.Audit.Record(ctx,
		store.AuditEntry{Actor: "owner", Action: "search", CreatedAt: old1},
		store.AuditEntry{Actor: "owner", Action: "ask", CreatedAt: old2},
		store.AuditEntry{Actor: "owner", Action: "graph", CreatedAt: fresh},
	); err != nil {
		t.Fatalf("record: %v", err)
	}

	policy := DefaultPolicy()
	policy.AuditRetention = 365 * 24 * time.Hour
	sw := NewWithPolicy(st, policy, func() time.Time { return now })

	stats, err := sw.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if stats.AuditPruned != 2 {
		t.Errorf("AuditPruned = %d, want 2 (the two entries older than retention)", stats.AuditPruned)
	}
	rows, err := st.Audit.Recent(ctx, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(rows) != 1 || rows[0].Action != "graph" {
		t.Errorf("after prune = %+v, want only the fresh 'graph' entry kept", rows)
	}
}

func TestSweeper_AuditRetentionZeroNeverPrunes(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	old := store.FormatTime(now.AddDate(0, 0, -1000)) // ancient, but never pruned with zero retention
	if err := st.Audit.Record(ctx,
		store.AuditEntry{Actor: "owner", Action: "search", CreatedAt: old},
	); err != nil {
		t.Fatalf("record: %v", err)
	}

	policy := DefaultPolicy()
	policy.AuditRetention = 0 // disabled
	sw := NewWithPolicy(st, policy, func() time.Time { return now })

	stats, err := sw.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if stats.AuditPruned != 0 {
		t.Errorf("AuditPruned = %d, want 0 (zero retention disables pruning)", stats.AuditPruned)
	}
	if rows, _ := st.Audit.Recent(ctx, 10); len(rows) != 1 {
		t.Errorf("audit rows after disabled prune = %d, want 1 (kept)", len(rows))
	}
}

func TestSweeper_Compact(t *testing.T) {
	ctx := context.Background()
	st, db := storetest.NewWithDB(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	ins := func(id, tier string, deletedAt *string) {
		ts := store.FormatTime(now)
		var del any
		if deletedAt != nil {
			del = *deletedAt
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artifacts(id,type,canonical_url,title,privacy_tier,lifecycle_tier,status,first_seen_at,last_seen_at,deleted_at,created_at,updated_at)
			 VALUES (?, 'webpage', ?, ?, 'public', ?, 'ready', ?, ?, ?, ?, ?)`,
			id, "u"+id, id, tier, ts, ts, del, ts, ts); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	old := store.FormatTime(now.AddDate(0, 0, -200))   // purged long ago -> compact
	recent := store.FormatTime(now.AddDate(0, 0, -10)) // purged recently -> keep
	ins("01PURGEDOLD0000000000000A", "purged", &old)
	ins("01PURGEDRECENT000000000B", "purged", &recent)
	ins("01LIVE000000000000000000C", "active", nil)

	policy := DefaultPolicy()
	policy.CompactAfter = 90 * 24 * time.Hour
	sw := NewWithPolicy(st, policy, func() time.Time { return now })

	n, err := sw.Compact(ctx)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if n != 1 {
		t.Errorf("compacted %d, want 1 (only the long-purged artifact)", n)
	}
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("artifacts remaining = %d, want 2 (recent purge + live kept)", total)
	}

	// A zero retention disables compaction.
	policy.CompactAfter = 0
	if n2, _ := NewWithPolicy(st, policy, func() time.Time { return now }).Compact(ctx); n2 != 0 {
		t.Errorf("compaction with zero retention reclaimed %d, want 0 (disabled)", n2)
	}
}

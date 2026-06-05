package store

import (
	"context"
	"testing"
)

func purge(t *testing.T, st *Store, id, deletedAt string) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(),
		`UPDATE artifacts SET deleted_at = ?, lifecycle_tier = 'purged', updated_at = ? WHERE id = ?`,
		deletedAt, deletedAt, id); err != nil {
		t.Fatalf("purge %s: %v", id, err)
	}
}

func TestArtifactRepo_HardDelete(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	art := rawArtifact(t, st, "01AAAAAAAAAAAAAAAAAAAAAAAA", "https://e.com/x")
	src := &Source{Kind: "PULL", Connector: "rss"}
	_ = st.Sources.Create(ctx, src)
	_ = st.Encounters.Record(ctx, &Encounter{ArtifactID: art, SourceID: src.ID, SeenAt: now()})
	_ = st.Enrichments.Create(ctx, &Enrichment{ArtifactID: art, Kind: "summary", Payload: "{}"})
	_ = st.Claims.Create(ctx, &Claim{ArtifactID: art, Statement: "s"})
	purge(t, st, art, now())

	if err := st.Artifacts.HardDelete(ctx, art); err != nil {
		t.Fatalf("hard delete: %v", err)
	}

	count := func(table string) int {
		var n int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE artifact_id = ?`, art).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var arts int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE id = ?`, art).Scan(&arts); err != nil {
		t.Fatal(err)
	}
	if arts != 0 {
		t.Errorf("artifact row not hard-deleted (%d)", arts)
	}
	for _, tbl := range []string{"encounters", "enrichments", "claims"} {
		if c := count(tbl); c != 0 {
			t.Errorf("%s rows not deleted (%d)", tbl, c)
		}
	}
}

func TestArtifactRepo_HardDelete_RefusesLive(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	art := rawArtifact(t, st, "01AAAAAAAAAAAAAAAAAAAAAAAA", "https://e.com/x") // live, not tombstoned
	src := &Source{Kind: "PULL", Connector: "rss"}
	_ = st.Sources.Create(ctx, src)
	_ = st.Encounters.Record(ctx, &Encounter{ArtifactID: art, SourceID: src.ID, SeenAt: now()})

	if err := st.Artifacts.HardDelete(ctx, art); err == nil {
		t.Fatal("HardDelete must refuse a live (non-tombstoned) artifact")
	}
	// Nothing was deleted — the guard runs before any child delete.
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM encounters WHERE artifact_id = ?`, art).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("children should be intact after a refused hard-delete, got %d", n)
	}
}

func TestArtifactRepo_PurgedBefore(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	old := rawArtifact(t, st, "01OLD0000000000000000000AA", "https://e.com/old")
	recent := rawArtifact(t, st, "01RECENT00000000000000000B", "https://e.com/recent")
	tombNotPurged := rawArtifact(t, st, "01TOMB0000000000000000000C", "https://e.com/tomb")
	_ = rawArtifact(t, st, "01LIVE0000000000000000000D", "https://e.com/live") // live

	purge(t, st, old, "2025-01-01T00:00:00.000000000Z")
	purge(t, st, recent, "2026-05-01T00:00:00.000000000Z")
	// Tombstoned but lifecycle_tier still 'active' (a user delete, not a GC purge).
	if _, err := st.db.ExecContext(ctx, `UPDATE artifacts SET deleted_at = '2025-01-01T00:00:00.000000000Z' WHERE id = ?`, tombNotPurged); err != nil {
		t.Fatal(err)
	}

	ids, err := st.Artifacts.PurgedBefore(ctx, "2026-01-01T00:00:00.000000000Z")
	if err != nil {
		t.Fatalf("purged before: %v", err)
	}
	if len(ids) != 1 || ids[0] != old {
		t.Errorf("PurgedBefore = %v, want only the old purged artifact %q (not recent, not tomb-only, not live)", ids, old)
	}
}

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

// TestArtifactRepo_HardDelete seeds a row in every table that references the artifact
// and asserts all are removed — guarding the child-table completeness HardDelete
// depends on (no foreign keys exist to cascade).
func TestArtifactRepo_HardDelete(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	exec := func(q string, args ...any) {
		if _, err := st.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	art := rawArtifact(t, st, "01AAAAAAAAAAAAAAAAAAAAAAAA", "https://e.com/x")
	other := rawArtifact(t, st, "01OTHERAAAAAAAAAAAAAAAAAAA", "https://e.com/o")
	ts := now()

	exec(`INSERT INTO sources(id,kind,connector) VALUES ('s1','PULL','rss')`)
	exec(`INSERT INTO encounters(id,artifact_id,source_id,seen_at) VALUES ('e1',?, 's1', ?)`, art, ts)
	exec(`INSERT INTO enrichments(id,artifact_id,kind,payload,model,prompt_version) VALUES ('en1',?, 'summary','{}','m','v')`, art)
	exec(`INSERT INTO entities(id,kind,name) VALUES ('ent1','technology','Go')`)
	exec(`INSERT INTO artifact_entities(artifact_id,entity_id) VALUES (?, 'ent1')`, art)
	exec(`INSERT INTO topics(id,label) VALUES ('top1','T')`)
	exec(`INSERT INTO artifact_topics(artifact_id,topic_id,score) VALUES (?, 'top1', 1)`, art)
	exec(`INSERT INTO review_schedule(artifact_id,next_review_at) VALUES (?, ?)`, art, ts)
	exec(`INSERT INTO engagements(id,artifact_id,kind) VALUES ('eng1',?, 'thumbs_up')`, art)
	exec(`INSERT INTO jobs(id,type,artifact_id,status) VALUES ('j1','fetch',?, 'pending')`, art)
	exec(`INSERT INTO audit_log(id,actor,action,artifact_id) VALUES ('au1','local','read',?)`, art)
	exec(`INSERT INTO links(id,src_id,dst_id,relation) VALUES ('l1',?, ?, 'references')`, art, other)
	exec(`INSERT INTO links(id,src_id,dst_id,relation) VALUES ('l2',?, ?, 'references')`, other, art)
	// A claim on the artifact and a claim on the other artifact, in conflict — the
	// conflict must be dropped (it references the deleted claim) and the partner claim kept.
	exec(`INSERT INTO claims(id,artifact_id,statement) VALUES ('c1',?, 'mine')`, art)
	exec(`INSERT INTO claims(id,artifact_id,statement) VALUES ('c2',?, 'theirs')`, other)
	exec(`INSERT INTO conflicts(id,claim_a,claim_b,status) VALUES ('cf1','c1','c2','open')`)

	purge(t, st, art, ts)
	if err := st.Artifacts.HardDelete(ctx, art); err != nil {
		t.Fatalf("hard delete: %v", err)
	}

	scalar := func(q string, args ...any) int {
		var n int
		if err := st.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Every table referencing the artifact is empty of it.
	for _, c := range []struct {
		table, col string
	}{
		{"artifacts", "id"}, {"encounters", "artifact_id"}, {"enrichments", "artifact_id"},
		{"artifact_entities", "artifact_id"}, {"artifact_topics", "artifact_id"},
		{"review_schedule", "artifact_id"}, {"engagements", "artifact_id"},
		{"jobs", "artifact_id"}, {"audit_log", "artifact_id"}, {"claims", "artifact_id"},
	} {
		if n := scalar(`SELECT COUNT(*) FROM `+c.table+` WHERE `+c.col+` = ?`, art); n != 0 {
			t.Errorf("%s still has %d row(s) for the deleted artifact", c.table, n)
		}
	}
	if n := scalar(`SELECT COUNT(*) FROM links WHERE src_id = ? OR dst_id = ?`, art, art); n != 0 {
		t.Errorf("links still reference the deleted artifact (%d)", n)
	}
	if n := scalar(`SELECT COUNT(*) FROM conflicts WHERE id = 'cf1'`); n != 0 {
		t.Error("conflict referencing the deleted claim was not dropped")
	}
	// The partner artifact and its claim are untouched.
	if n := scalar(`SELECT COUNT(*) FROM claims WHERE id = 'c2'`); n != 1 {
		t.Error("partner claim should survive")
	}
	if _, err := st.Artifacts.GetLive(ctx, other); err != nil {
		t.Errorf("partner artifact should survive: %v", err)
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
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM encounters WHERE artifact_id = ?`, art).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("children should be intact after a refused hard-delete, got %d", n)
	}
}

// TestArtifactRepo_HardDelete_RefusesNonPurgedTombstone: a tombstone that is not a GC
// purge (e.g. a Merge loser, tombstoned but lifecycle_tier unchanged) must NOT be
// reclaimable by HardDelete.
func TestArtifactRepo_HardDelete_RefusesNonPurgedTombstone(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	art := rawArtifact(t, st, "01AAAAAAAAAAAAAAAAAAAAAAAA", "https://e.com/x")
	// Tombstone WITHOUT setting lifecycle_tier='purged'.
	if _, err := st.db.ExecContext(ctx, `UPDATE artifacts SET deleted_at = ? WHERE id = ?`, now(), art); err != nil {
		t.Fatal(err)
	}
	if err := st.Artifacts.HardDelete(ctx, art); err == nil {
		t.Error("HardDelete must refuse a tombstone that is not lifecycle_tier='purged'")
	}
	var n int
	st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE id = ?`, art).Scan(&n)
	if n != 1 {
		t.Errorf("non-purged tombstone should not be physically removed, got %d", n)
	}
}

func TestArtifactRepo_PurgedBefore(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	old := rawArtifact(t, st, "01OLD0000000000000000000AA", "https://e.com/old")
	recent := rawArtifact(t, st, "01RECENT00000000000000000B", "https://e.com/recent")
	atCutoff := rawArtifact(t, st, "01ATCUTOFF000000000000000E", "https://e.com/atcutoff")
	tombNotPurged := rawArtifact(t, st, "01TOMB0000000000000000000C", "https://e.com/tomb")
	_ = rawArtifact(t, st, "01LIVE0000000000000000000D", "https://e.com/live") // live

	const cutoff = "2026-01-01T00:00:00.000000000Z"
	purge(t, st, old, "2025-01-01T00:00:00.000000000Z")
	purge(t, st, recent, "2026-05-01T00:00:00.000000000Z")
	purge(t, st, atCutoff, cutoff) // exactly == cutoff: excluded by strict `<`
	if _, err := st.db.ExecContext(ctx, `UPDATE artifacts SET deleted_at = '2025-01-01T00:00:00.000000000Z' WHERE id = ?`, tombNotPurged); err != nil {
		t.Fatal(err)
	}

	ids, err := st.Artifacts.PurgedBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("purged before: %v", err)
	}
	if len(ids) != 1 || ids[0] != old {
		t.Errorf("PurgedBefore = %v, want only the old purged artifact %q (recent/at-cutoff/tomb-only/live excluded)", ids, old)
	}
}

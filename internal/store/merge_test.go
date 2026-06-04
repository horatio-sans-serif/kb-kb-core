package store

import (
	"context"
	"testing"

	"kb/internal/kb"
)

// rawArtifact inserts an artifact with an explicit id + canonical_url, bypassing
// dedup — to simulate the cross-replica duplicates the merge reconciles.
func rawArtifact(t *testing.T, st *Store, id, url string) string {
	t.Helper()
	_, err := st.db.ExecContext(context.Background(),
		`INSERT INTO artifacts(id, type, canonical_url, title, privacy_tier, lifecycle_tier, status,
		   first_seen_at, last_seen_at, created_at, updated_at)
		 VALUES (?, 'webpage', ?, ?, 'public', 'active', 'ready', ?, ?, ?, ?)`,
		id, url, id, now(), now(), now(), now())
	if err != nil {
		t.Fatalf("raw artifact %s: %v", id, err)
	}
	return id
}

func TestArtifactRepo_Merge(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	url := "https://e.com/dup"
	survivor := rawArtifact(t, st, "01AAAAAAAAAAAAAAAAAAAAAAAA", url)
	loser := rawArtifact(t, st, "01BBBBBBBBBBBBBBBBBBBBBBBB", url)

	src := &Source{Kind: "PULL", Connector: "rss"}
	_ = st.Sources.Create(ctx, src)
	// Loser's children: an encounter, an enrichment, a shared entity (also on
	// survivor, to exercise the dedup-on-merge), a claim.
	_ = st.Encounters.Record(ctx, &Encounter{ArtifactID: loser, SourceID: src.ID, SeenAt: now()})
	_ = st.Enrichments.Create(ctx, &Enrichment{ArtifactID: loser, Kind: "summary", Payload: "{}"})
	ent, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "technology", Name: "Go"})
	_ = st.Graph.LinkArtifactEntity(ctx, survivor, ent) // survivor already has it
	_ = st.Graph.LinkArtifactEntity(ctx, loser, ent)    // loser too → merge must not PK-collide
	_ = st.Claims.Create(ctx, &Claim{ArtifactID: loser, Statement: "loser claim"})

	if err := st.Artifacts.Merge(ctx, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}

	// Loser is tombstoned; survivor is live.
	if _, err := st.Artifacts.GetLive(ctx, loser); err == nil {
		t.Error("loser should be tombstoned after merge")
	}
	if _, err := st.Artifacts.GetLive(ctx, survivor); err != nil {
		t.Errorf("survivor should remain live: %v", err)
	}
	// The encounter and enrichment re-pointed to the survivor.
	if n, _ := st.Encounters.CountForArtifact(ctx, survivor); n != 1 {
		t.Errorf("survivor should inherit the loser's encounter, got %d", n)
	}
	if rows, _ := st.Enrichments.ListForArtifact(ctx, survivor); len(rows) != 1 {
		t.Errorf("survivor should inherit the loser's enrichment, got %d", len(rows))
	}
	// The shared entity link de-duplicated (no PK collision), loser's gone.
	if ents, _ := st.Graph.EntitiesForArtifact(ctx, survivor); len(ents) != 1 {
		t.Errorf("survivor entity links should dedup to 1, got %d", len(ents))
	}
	if ents, _ := st.Graph.EntitiesForArtifact(ctx, loser); len(ents) != 0 {
		t.Errorf("loser should have no remaining entity links, got %d", len(ents))
	}
}

func TestArtifactRepo_DuplicateGroups(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	// Two artifacts share a URL; one is unique; a content-hash pair too.
	rawArtifact(t, st, "01A0000000000000000000000A", "https://e.com/x")
	rawArtifact(t, st, "01B0000000000000000000000B", "https://e.com/x")
	rawArtifact(t, st, "01C0000000000000000000000C", "https://e.com/unique")
	ha, hb := "hashdup", "hashdup"
	_, _ = st.db.ExecContext(ctx, `INSERT INTO artifacts(id,type,content_hash,privacy_tier,lifecycle_tier,status,first_seen_at,last_seen_at,created_at,updated_at) VALUES ('01D0000000000000000000000D','note',?,'public','active','ready',?,?,?,?)`, ha, now(), now(), now(), now())
	_, _ = st.db.ExecContext(ctx, `INSERT INTO artifacts(id,type,content_hash,privacy_tier,lifecycle_tier,status,first_seen_at,last_seen_at,created_at,updated_at) VALUES ('01E0000000000000000000000E','note',?,'public','active','ready',?,?,?,?)`, hb, now(), now(), now(), now())

	groups, err := st.Artifacts.DuplicateGroups(ctx)
	if err != nil {
		t.Fatalf("groups: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2 (the url pair + the hash pair)", len(groups))
	}
	for _, g := range groups {
		if len(g) != 2 {
			t.Errorf("each dup group should have 2 members, got %v", g)
		}
		if g[0] >= g[1] {
			t.Errorf("group members must be id-ascending (deterministic survivor first): %v", g)
		}
	}
}

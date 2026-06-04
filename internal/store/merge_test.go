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

func TestArtifactRepo_Merge_FoldsEngagementSignal(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// Survivor sighted later and never engaged; loser sighted earlier and engaged.
	// The merge must keep the earliest sighting and carry the engagement forward:
	// last_engaged_at and the active tier live on the artifact row the merge is
	// about to tombstone, so without folding they would be lost.
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO artifacts(id,type,canonical_url,title,privacy_tier,lifecycle_tier,status,first_seen_at,last_seen_at,created_at,updated_at)
		 VALUES ('01AAAAAAAAAAAAAAAAAAAAAAAA','webpage','https://e.com/d','s','public','archive','ready',
		         '2025-06-01T00:00:00.000000000Z','2025-06-01T00:00:00.000000000Z','2025-06-01T00:00:00.000000000Z','2025-06-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO artifacts(id,type,canonical_url,title,privacy_tier,lifecycle_tier,status,first_seen_at,last_seen_at,last_engaged_at,created_at,updated_at)
		 VALUES ('01BBBBBBBBBBBBBBBBBBBBBBBB','webpage','https://e.com/d','l','public','active','ready',
		         '2025-01-01T00:00:00.000000000Z','2025-03-01T00:00:00.000000000Z','2025-03-01T00:00:00.000000000Z','2025-01-01T00:00:00.000000000Z','2025-01-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	survivor, loser := "01AAAAAAAAAAAAAAAAAAAAAAAA", "01BBBBBBBBBBBBBBBBBBBBBBBB"

	if err := st.Artifacts.Merge(ctx, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, err := st.Artifacts.Get(ctx, survivor)
	if err != nil {
		t.Fatalf("get survivor: %v", err)
	}
	if got.LastEngagedAt == nil || *got.LastEngagedAt != "2025-03-01T00:00:00.000000000Z" {
		t.Errorf("survivor last_engaged_at = %v, want the loser's 2025-03-01 (engagement signal lost otherwise)", got.LastEngagedAt)
	}
	if got.LifecycleTier != "active" {
		t.Errorf("survivor lifecycle = %q, want active (engagement reverses archive)", got.LifecycleTier)
	}
	if got.FirstSeenAt != "2025-01-01T00:00:00.000000000Z" {
		t.Errorf("survivor first_seen_at = %q, want the earliest (loser's)", got.FirstSeenAt)
	}
}

func TestArtifactRepo_Merge_DropsSelfLoopLinks(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	url := "https://e.com/loop"
	survivor := rawArtifact(t, st, "01AAAAAAAAAAAAAAAAAAAAAAAA", url)
	loser := rawArtifact(t, st, "01BBBBBBBBBBBBBBBBBBBBBBBB", url)

	// Duplicates share entities/topics, so the graph builder linked them both ways.
	// Re-pointing src then dst would collapse both edges to one S->S self-loop.
	if err := st.Graph.AddLink(ctx, survivor, loser, "shares_entity", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Graph.AddLink(ctx, loser, survivor, "shares_entity", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Artifacts.Merge(ctx, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}
	var selfLoops int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM links WHERE src_id = dst_id`).Scan(&selfLoops); err != nil {
		t.Fatal(err)
	}
	if selfLoops != 0 {
		t.Errorf("merge left %d self-loop link(s); a node is not its own neighbor", selfLoops)
	}
}

func TestArtifactRepo_Merge_RepointsEngagementsAndJobs(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	url := "https://e.com/ref"
	survivor := rawArtifact(t, st, "01AAAAAAAAAAAAAAAAAAAAAAAA", url)
	loser := rawArtifact(t, st, "01BBBBBBBBBBBBBBBBBBBBBBBB", url)

	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO engagements(id, artifact_id, kind, created_at) VALUES ('01E0000000000000000000000E', ?, 'thumbs_up', ?)`,
		loser, now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Jobs.Enqueue(ctx, EnqueueSpec{Type: "fetch", ArtifactID: &loser}); err != nil {
		t.Fatal(err)
	}
	if err := st.Artifacts.Merge(ctx, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}

	count := func(table, id string) int {
		var n int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE artifact_id = ?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if c := count("engagements", loser); c != 0 {
		t.Errorf("loser still has %d engagement(s); merge must re-point them (lost feedback)", c)
	}
	if c := count("jobs", loser); c != 0 {
		t.Errorf("loser still has %d job(s); merge must re-point them (orphaned work)", c)
	}
	if c := count("engagements", survivor); c != 1 {
		t.Errorf("survivor should inherit the engagement, got %d", c)
	}
	if c := count("jobs", survivor); c != 1 {
		t.Errorf("survivor should inherit the job, got %d", c)
	}
}

func TestArtifactRepo_Merge_LatestEnrichmentPerKind(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	url := "https://e.com/enr"
	survivor := rawArtifact(t, st, "01AAAAAAAAAAAAAAAAAAAAAAAA", url)
	loser := rawArtifact(t, st, "01BBBBBBBBBBBBBBBBBBBBBBBB", url)
	_ = st.Enrichments.Create(ctx, &Enrichment{ArtifactID: survivor, Kind: "summary", Payload: `"S"`})
	_ = st.Enrichments.Create(ctx, &Enrichment{ArtifactID: loser, Kind: "summary", Payload: `"L"`})

	if err := st.Artifacts.Merge(ctx, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}
	// Both summaries re-point to the survivor — enrichments keep history (a synced
	// table can carry no uniqueness), so the merge does not delete the redundant
	// row. Every production read is latest-per-kind, so exactly one wins.
	all, _ := st.Enrichments.ListForArtifact(ctx, survivor)
	if len(all) != 2 {
		t.Errorf("survivor should accumulate both summaries (history preserved), got %d", len(all))
	}
	if _, err := st.Enrichments.LatestByKind(ctx, survivor, "summary"); err != nil {
		t.Fatalf("latest-by-kind should return one summary after merge: %v", err)
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

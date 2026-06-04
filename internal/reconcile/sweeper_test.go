package reconcile

import (
	"context"
	"sync"
	"testing"

	"kb/internal/store"
	"kb/internal/storetest"
)

func TestSweeper_MergesDuplicates(t *testing.T) {
	ctx := context.Background()
	st, db := storetest.NewWithDB(t)

	// Three replicas independently created the same URL while offline.
	for _, id := range []string{"01C0000000000000000000000C", "01A0000000000000000000000A", "01B0000000000000000000000B"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artifacts(id,type,canonical_url,title,privacy_tier,lifecycle_tier,status,first_seen_at,last_seen_at,created_at,updated_at)
			 VALUES (?, 'webpage', 'https://e.com/same', ?, 'public','active','ready', '2025-01-01T00:00:00.000000000Z','2025-01-01T00:00:00.000000000Z','2025-01-01T00:00:00.000000000Z','2025-01-01T00:00:00.000000000Z')`,
			id, id); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	n, err := New(st, nil).Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Errorf("merged %d, want 2 (3 dups → 1 survivor)", n)
	}

	live, _ := st.Artifacts.List(ctx)
	if len(live) != 1 {
		t.Fatalf("after reconcile, live artifacts = %d, want 1", len(live))
	}
	// The survivor is the lowest id (deterministic across replicas).
	if live[0].ID != "01A0000000000000000000000A" {
		t.Errorf("survivor = %q, want the lowest id", live[0].ID)
	}

	// A second sweep is a no-op (idempotent — no duplicates remain).
	if n2, _ := New(st, nil).Sweep(ctx); n2 != 0 {
		t.Errorf("second sweep merged %d, want 0", n2)
	}
}

func TestSweeper_ReconcilesClaimsConflictsLinks(t *testing.T) {
	ctx := context.Background()
	st, db := storetest.NewWithDB(t)
	exec := func(q string, args ...any) {
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	ts := store.Now()
	artRow := func(id, url string) {
		exec(`INSERT INTO artifacts(id,type,canonical_url,title,privacy_tier,lifecycle_tier,status,first_seen_at,last_seen_at,created_at,updated_at)
		      VALUES (?, 'webpage', ?, ?, 'public','active','ready', ?, ?, ?, ?)`, id, url, id, ts, ts, ts, ts)
	}
	artRow("01A0000000000000000000000A", "https://e.com/a")
	artRow("01B0000000000000000000000B", "https://e.com/b")

	// Two replicas each created the same link (same src,dst,relation).
	exec(`INSERT INTO links(id,src_id,dst_id,relation,weight,created_at) VALUES ('01L0000000000000000000000A','01A0000000000000000000000A','01B0000000000000000000000B','references',1,?)`, ts)
	exec(`INSERT INTO links(id,src_id,dst_id,relation,weight,created_at) VALUES ('01L0000000000000000000000B','01A0000000000000000000000A','01B0000000000000000000000B','references',1,?)`, ts)

	// Duplicate claims (same artifact+statement) and two distinct claims for a
	// duplicate conflict pair.
	exec(`INSERT INTO claims(id,artifact_id,statement,created_at) VALUES ('01C0000000000000000000000A','01A0000000000000000000000A','dup',?)`, ts)
	exec(`INSERT INTO claims(id,artifact_id,statement,created_at) VALUES ('01C0000000000000000000000B','01A0000000000000000000000A','dup',?)`, ts)
	exec(`INSERT INTO claims(id,artifact_id,statement,created_at) VALUES ('01C0000000000000000000000X','01A0000000000000000000000A','x',?)`, ts)
	exec(`INSERT INTO claims(id,artifact_id,statement,created_at) VALUES ('01C0000000000000000000000Y','01A0000000000000000000000A','y',?)`, ts)
	exec(`INSERT INTO conflicts(id,claim_a,claim_b,status,created_at) VALUES ('01F0000000000000000000000A','01C0000000000000000000000X','01C0000000000000000000000Y','open',?)`, ts)
	exec(`INSERT INTO conflicts(id,claim_a,claim_b,status,created_at) VALUES ('01F0000000000000000000000B','01C0000000000000000000000X','01C0000000000000000000000Y','open',?)`, ts)

	total, err := New(st, nil).Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if total != 3 {
		t.Errorf("sweep total = %d, want 3 (1 claim + 1 conflict + 1 link)", total)
	}
	count := func(table string) int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if c := count("links"); c != 1 {
		t.Errorf("links after sweep = %d, want 1", c)
	}
	if c := count("conflicts"); c != 1 {
		t.Errorf("conflicts after sweep = %d, want 1", c)
	}
	if c := count("claims"); c != 3 {
		t.Errorf("claims after sweep = %d, want 3 (dup pair collapsed)", c)
	}
}

func TestSweeper_RescoresSurvivor(t *testing.T) {
	ctx := context.Background()
	st, db := storetest.NewWithDB(t)

	// Two replicas created the same URL offline; after the merge the survivor
	// inherits the loser's encounters, so its relevance inputs changed and it must
	// be re-scored. The loser is tombstoned and must not be re-scored.
	for _, id := range []string{"01A0000000000000000000000A", "01B0000000000000000000000B"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artifacts(id,type,canonical_url,title,privacy_tier,lifecycle_tier,status,first_seen_at,last_seen_at,created_at,updated_at)
			 VALUES (?, 'webpage','https://e.com/r', ?, 'public','active','ready',
			         '2025-01-01T00:00:00.000000000Z','2025-01-01T00:00:00.000000000Z','2025-01-01T00:00:00.000000000Z','2025-01-01T00:00:00.000000000Z')`,
			id, id); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	var mu sync.Mutex
	var rescored []string
	rescore := func(ctx context.Context, tx *store.Store, artifactID string) error {
		mu.Lock()
		defer mu.Unlock()
		rescored = append(rescored, artifactID)
		return nil
	}

	if _, err := New(st, rescore).Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(rescored) != 1 || rescored[0] != "01A0000000000000000000000A" {
		t.Errorf("expected only the survivor (lowest id) re-scored, got %v", rescored)
	}
}

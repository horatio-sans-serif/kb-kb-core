package reconcile

import (
	"context"
	"testing"

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

	n, err := New(st).Sweep(ctx)
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
	if n2, _ := New(st).Sweep(ctx); n2 != 0 {
		t.Errorf("second sweep merged %d, want 0", n2)
	}
}

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

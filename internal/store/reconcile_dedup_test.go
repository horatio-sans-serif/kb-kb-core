package store

import (
	"context"
	"testing"
)

// These exercise the cross-replica dedup that replaces the unique indexes
// cr-sqlite forbids (§8.16): within a replica the write paths find-then-insert
// (no duplicate created), and the reconcile primitives collapse the duplicates two
// offline replicas can each create. Raw inserts simulate those cross-replica rows
// (the public API now prevents same-replica duplicates).

func TestGraphRepo_AddLink_Dedups(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a1 := rawArtifact(t, st, "01A0000000000000000000000A", "https://e.com/1")
	a2 := rawArtifact(t, st, "01B0000000000000000000000B", "https://e.com/2")

	for range 2 {
		if err := st.Graph.AddLink(ctx, a1, a2, "references", 1); err != nil {
			t.Fatalf("add link: %v", err)
		}
	}
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM links`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("AddLink should dedup without a unique index, got %d links", n)
	}
}

func TestGraphRepo_DedupLinks(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a1 := rawArtifact(t, st, "01A0000000000000000000000A", "https://e.com/1")
	a2 := rawArtifact(t, st, "01B0000000000000000000000B", "https://e.com/2")

	for _, id := range []string{"01L0000000000000000000000B", "01L0000000000000000000000A"} {
		if _, err := st.db.ExecContext(ctx,
			`INSERT INTO links(id, src_id, dst_id, relation, weight, created_at) VALUES (?, ?, ?, 'references', 1, ?)`,
			id, a1, a2, now()); err != nil {
			t.Fatalf("seed link %s: %v", id, err)
		}
	}
	n, err := st.Graph.DedupLinks(ctx)
	if err != nil {
		t.Fatalf("dedup links: %v", err)
	}
	if n != 1 {
		t.Errorf("deduped %d links, want 1", n)
	}
	var survivor string
	if err := st.db.QueryRowContext(ctx, `SELECT id FROM links`).Scan(&survivor); err != nil {
		t.Fatal(err)
	}
	if survivor != "01L0000000000000000000000A" {
		t.Errorf("link survivor = %q, want the lowest id", survivor)
	}
}

func TestConflictRepo_Create_Dedups(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	art := rawArtifact(t, st, "01A0000000000000000000000A", "https://e.com/c")
	ca := rawClaim(t, st, "01CA00000000000000000000AA", art, "claim A")
	cb := rawClaim(t, st, "01CB00000000000000000000BB", art, "claim B")

	// The same contradiction discovered from either side dedups to one row.
	for _, pair := range [][2]string{{ca, cb}, {cb, ca}} {
		if err := st.Conflicts.Create(ctx, &Conflict{ClaimA: pair[0], ClaimB: pair[1]}); err != nil {
			t.Fatalf("create conflict: %v", err)
		}
	}
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conflicts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Conflict.Create should dedup the canonical pair, got %d", n)
	}
}

func TestConflictRepo_DedupDuplicates(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	art := rawArtifact(t, st, "01A0000000000000000000000A", "https://e.com/c")
	ca := rawClaim(t, st, "01CA00000000000000000000AA", art, "claim A")
	cb := rawClaim(t, st, "01CB00000000000000000000BB", art, "claim B")

	for _, id := range []string{"01F0000000000000000000000B", "01F0000000000000000000000A"} {
		if _, err := st.db.ExecContext(ctx,
			`INSERT INTO conflicts(id, claim_a, claim_b, status, created_at) VALUES (?, ?, ?, 'open', ?)`,
			id, ca, cb, now()); err != nil {
			t.Fatalf("seed conflict %s: %v", id, err)
		}
	}
	n, err := st.Conflicts.DedupDuplicates(ctx)
	if err != nil {
		t.Fatalf("dedup conflicts: %v", err)
	}
	if n != 1 {
		t.Errorf("deduped %d conflicts, want 1", n)
	}
	var survivor string
	if err := st.db.QueryRowContext(ctx, `SELECT id FROM conflicts`).Scan(&survivor); err != nil {
		t.Fatal(err)
	}
	if survivor != "01F0000000000000000000000A" {
		t.Errorf("conflict survivor = %q, want the lowest id", survivor)
	}
}

func TestClaimRepo_Create_Dedups(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	art := rawArtifact(t, st, "01A0000000000000000000000A", "https://e.com/c")

	for range 2 {
		if err := st.Claims.Create(ctx, &Claim{ArtifactID: art, Statement: "the same statement"}); err != nil {
			t.Fatalf("create claim: %v", err)
		}
	}
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM claims`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Claim.Create should dedup the same statement, got %d", n)
	}
}

func TestClaimRepo_ReconcileDuplicates(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	art := rawArtifact(t, st, "01A0000000000000000000000A", "https://e.com/c")

	// Two replicas each extracted "X" from the same artifact: duplicate claims.
	survivor := rawClaim(t, st, "01CC0000000000000000000001", art, "X")
	loser := rawClaim(t, st, "01CC0000000000000000000002", art, "X")
	other := rawClaim(t, st, "01DD000000000000000000000Z", art, "Y")

	// A conflict references the loser; reconcile must re-point it to the survivor
	// (canonical claim_a < claim_b, so claim_a is the loser here).
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO conflicts(id, claim_a, claim_b, status, created_at) VALUES ('01FF000000000000000000000Z', ?, ?, 'open', ?)`,
		loser, other, now()); err != nil {
		t.Fatalf("seed conflict: %v", err)
	}

	n, err := st.Claims.ReconcileDuplicates(ctx)
	if err != nil {
		t.Fatalf("reconcile claims: %v", err)
	}
	if n != 1 {
		t.Errorf("merged %d claims, want 1", n)
	}
	// Loser gone, survivor remains, statement "X" appears once.
	var xCount int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM claims WHERE statement = 'X'`).Scan(&xCount); err != nil {
		t.Fatal(err)
	}
	if xCount != 1 {
		t.Errorf("statement X should survive once, got %d", xCount)
	}
	if _, err := st.Claims.Get(ctx, loser); err == nil {
		t.Error("loser claim should be deleted")
	}
	// The conflict now references the survivor, not the tombstoned loser.
	var claimA string
	if err := st.db.QueryRowContext(ctx, `SELECT claim_a FROM conflicts WHERE id = '01FF000000000000000000000Z'`).Scan(&claimA); err != nil {
		t.Fatalf("conflict should still exist, re-pointed: %v", err)
	}
	if claimA != survivor {
		t.Errorf("conflict.claim_a = %q, want the survivor %q", claimA, survivor)
	}
}

// rawClaim inserts a claim with an explicit id + statement, bypassing dedup — to
// simulate the cross-replica duplicates the reconcile sweep merges.
func rawClaim(t *testing.T, st *Store, id, artifactID, statement string) string {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(),
		`INSERT INTO claims(id, artifact_id, statement, created_at) VALUES (?, ?, ?, ?)`,
		id, artifactID, statement, now()); err != nil {
		t.Fatalf("raw claim %s: %v", id, err)
	}
	return id
}

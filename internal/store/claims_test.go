package store

import (
	"context"
	"errors"
	"testing"
)

func TestClaimRepo_CreateAndListByArtifact(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a := mkArtifact(t, st, "h1")

	c1 := &Claim{ArtifactID: a, Statement: "The sky is blue.", Embedding: []float32{0.1, 0.2, 0.3}}
	if err := st.Claims.Create(ctx, c1); err != nil {
		t.Fatalf("create: %v", err)
	}
	if c1.ID == "" || c1.CreatedAt == "" {
		t.Fatal("Create should assign ID and CreatedAt")
	}
	c2 := &Claim{ArtifactID: a, Statement: "Water is wet."}
	if err := st.Claims.Create(ctx, c2); err != nil {
		t.Fatalf("create 2: %v", err)
	}

	got, err := st.Claims.ListByArtifact(ctx, a)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("claims = %d, want 2", len(got))
	}
	// Embedding round-trips.
	var withVec *Claim
	for _, c := range got {
		if c.ID == c1.ID {
			withVec = c
		}
	}
	if withVec == nil || len(withVec.Embedding) != 3 || withVec.Embedding[1] != 0.2 {
		t.Errorf("embedding did not round-trip: %+v", withVec)
	}
}

func TestClaimRepo_ListExcludingArtifact(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a, b := mkArtifact(t, st, "h1"), mkArtifact(t, st, "h2")
	_ = st.Claims.Create(ctx, &Claim{ArtifactID: a, Statement: "A1", Embedding: []float32{1}})
	_ = st.Claims.Create(ctx, &Claim{ArtifactID: b, Statement: "B1", Embedding: []float32{2}})
	_ = st.Claims.Create(ctx, &Claim{ArtifactID: b, Statement: "B2"}) // no embedding → skipped

	// Comparison set for artifact a: only b's claims that have an embedding.
	others, err := st.Claims.ListExcludingArtifact(ctx, a, false)
	if err != nil {
		t.Fatalf("list excluding: %v", err)
	}
	if len(others) != 1 || others[0].Statement != "B1" {
		t.Errorf("excluding-a set = %+v, want [B1] (embedded, not from a)", others)
	}
}

func TestClaimRepo_CloudSafeExcludesSensitive(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a := mkArtifact(t, st, "h1")
	// A sensitive artifact with an embedded claim.
	hs := "hs"
	sens, _ := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", Title: "s", BodyMarkdown: "b", ContentHash: &hs, PrivacyTier: "sensitive"})
	_ = st.Claims.Create(ctx, &Claim{ArtifactID: sens.ID, Statement: "secret claim", Embedding: []float32{1}})

	// cloudSafeOnly excludes the sensitive artifact's claim (never sent to a remote judge).
	safe, _ := st.Claims.ListExcludingArtifact(ctx, a, true)
	if len(safe) != 0 {
		t.Errorf("cloud-safe set must exclude sensitive claims, got %d", len(safe))
	}
	// Without the flag it is included.
	all, _ := st.Claims.ListExcludingArtifact(ctx, a, false)
	if len(all) != 1 {
		t.Errorf("local set should include all embedded claims, got %d", len(all))
	}
}

func TestClaimRepo_ExcludesTombstoned(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a, b := mkArtifact(t, st, "h1"), mkArtifact(t, st, "h2")
	_ = st.Claims.Create(ctx, &Claim{ArtifactID: b, Statement: "B1", Embedding: []float32{2}})
	if err := st.Artifacts.SoftDelete(ctx, b); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	others, _ := st.Claims.ListExcludingArtifact(ctx, a, false)
	if len(others) != 0 {
		t.Errorf("claims of a tombstoned artifact must be excluded, got %d", len(others))
	}
}

func TestConflictRepo_CreateDedupListResolve(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a := mkArtifact(t, st, "h1")
	ca := &Claim{ArtifactID: a, Statement: "X is true"}
	cb := &Claim{ArtifactID: a, Statement: "X is false"}
	_ = st.Claims.Create(ctx, ca)
	_ = st.Claims.Create(ctx, cb)

	// Create the same conflict twice, in opposite orders → one row (canonical dedup).
	if err := st.Conflicts.Create(ctx, &Conflict{ClaimA: ca.ID, ClaimB: cb.ID, Rationale: "contradiction"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Conflicts.Create(ctx, &Conflict{ClaimA: cb.ID, ClaimB: ca.ID}); err != nil {
		t.Fatalf("create reversed: %v", err)
	}

	open, err := st.Conflicts.ListByStatus(ctx, "open", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("open conflicts = %d, want 1 (deduped)", len(open))
	}
	id := open[0].ID

	// Resolve it.
	if err := st.Conflicts.SetStatus(ctx, id, "resolved", "A is correct"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if again, _ := st.Conflicts.ListByStatus(ctx, "open", 10); len(again) != 0 {
		t.Errorf("resolved conflict should not be open, got %d", len(again))
	}
	resolved, _ := st.Conflicts.ListByStatus(ctx, "resolved", 10)
	if len(resolved) != 1 || resolved[0].Rationale != "A is correct" {
		t.Errorf("resolved = %+v", resolved)
	}
}

func TestConflictRepo_SetStatusMissing(t *testing.T) {
	st := newTestStore(t)
	err := st.Conflicts.SetStatus(context.Background(), "nope", "resolved", "")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("SetStatus(missing) = %v, want ErrNotFound", err)
	}
}

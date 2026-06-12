package store

import (
	"context"
	"errors"
	"testing"

	"kb/internal/kb"
)

func art(t *testing.T, st *Store, hash string) string {
	t.Helper()
	a, err := st.Artifacts.UpsertByContentHash(context.Background(),
		&Artifact{Type: "note", Title: hash, BodyMarkdown: "b", ContentHash: &hash, PrivacyTier: kb.Public})
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	return a.ID
}

func TestGraph_EntityDedupAndLink(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a1 := art(t, st, "h1")
	a2 := art(t, st, "h2")

	// Upserting the same (kind, name) twice returns one canonical id.
	id1, err := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "technology", Name: "Go", Aliases: []string{"Golang"}})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	id2, err := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "technology", Name: "Go"})
	if err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	if id1 != id2 {
		t.Errorf("same (kind,name) should dedup to one entity: %q vs %q", id1, id2)
	}

	// Link both artifacts to the entity; idempotent.
	for _, a := range []string{a1, a2, a1} {
		if err := st.Graph.LinkArtifactEntity(ctx, a, id1); err != nil {
			t.Fatalf("link %s: %v", a, err)
		}
	}
	arts, err := st.Graph.ArtifactsForEntity(ctx, id1)
	if err != nil {
		t.Fatalf("artifacts-for-entity: %v", err)
	}
	if len(arts) != 2 {
		t.Errorf("entity should link to 2 distinct artifacts, got %v", arts)
	}

	ents, err := st.Graph.EntitiesForArtifact(ctx, a1)
	if err != nil {
		t.Fatalf("entities-for-artifact: %v", err)
	}
	if len(ents) != 1 || ents[0].Name != "Go" || len(ents[0].Aliases) != 1 || ents[0].Aliases[0] != "Golang" {
		t.Errorf("entities-for-artifact = %+v, want [Go(Golang)]", ents)
	}
}

func TestGraph_AddLinkDedup(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a1, a2 := art(t, st, "h1"), art(t, st, "h2")

	for range 3 { // same edge added repeatedly
		if err := st.Graph.AddLink(ctx, a1, a2, "shares_entity", 1.0); err != nil {
			t.Fatalf("add link: %v", err)
		}
	}
	nbrs, err := st.Graph.NeighborIDs(ctx, []string{a1}, kb.Restricted)
	if err != nil {
		t.Fatalf("neighbors: %v", err)
	}
	if len(nbrs) != 1 || nbrs[0] != a2 {
		t.Errorf("a1 should have exactly one neighbor a2 (dedup), got %v", nbrs)
	}
}

func TestGraph_ConnectedWithinHops(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a := art(t, st, "ha")
	b := art(t, st, "hb")
	c := art(t, st, "hc")
	d := art(t, st, "hd") // unconnected

	// a — b — c (chain). Edges are directional but traversal is undirected.
	if err := st.Graph.AddLink(ctx, a, b, "references", 1.0); err != nil {
		t.Fatalf("link a-b: %v", err)
	}
	if err := st.Graph.AddLink(ctx, c, b, "references", 1.0); err != nil { // note: c->b
		t.Fatalf("link c-b: %v", err)
	}

	// 1 hop from a: just b.
	one, err := st.Graph.ConnectedWithin(ctx, a, 1, kb.Restricted)
	if err != nil {
		t.Fatalf("connected 1: %v", err)
	}
	if len(one) != 1 || one[0] != b {
		t.Errorf("1 hop from a = %v, want [b]", one)
	}

	// 2 hops from a: b and c (via the reverse edge c->b).
	two, err := st.Graph.ConnectedWithin(ctx, a, 2, kb.Restricted)
	if err != nil {
		t.Fatalf("connected 2: %v", err)
	}
	if len(two) != 2 || !contains(two, b) || !contains(two, c) {
		t.Errorf("2 hops from a = %v, want [b, c]", two)
	}
	if contains(two, d) || contains(two, a) {
		t.Errorf("traversal must exclude the start and the unconnected node: %v", two)
	}
}

func TestGraph_AliasPreservedOnReUpsert(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a := art(t, st, "h1")

	id, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "person", Name: "Ada", Aliases: []string{"Ada Lovelace"}})
	// Re-upsert with NO aliases must not wipe the stored ones (COALESCE).
	if _, err := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "person", Name: "Ada"}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	_ = st.Graph.LinkArtifactEntity(ctx, a, id)
	ents, _ := st.Graph.EntitiesForArtifact(ctx, a)
	if len(ents) != 1 || len(ents[0].Aliases) != 1 || ents[0].Aliases[0] != "Ada Lovelace" {
		t.Errorf("aliases not preserved across a no-alias re-upsert: %+v", ents)
	}
}

func TestGraph_TombstonedExcluded(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a, b := art(t, st, "ha"), art(t, st, "hb")
	id, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "concept", Name: "Graphs"})
	_ = st.Graph.LinkArtifactEntity(ctx, a, id)
	_ = st.Graph.LinkArtifactEntity(ctx, b, id)
	_ = st.Graph.AddLink(ctx, a, b, "shares_entity", 1.0)

	if err := st.Artifacts.SoftDelete(ctx, b); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	// b is tombstoned: it drops from both the entity's artifacts and a's neighbors.
	arts, _ := st.Graph.ArtifactsForEntity(ctx, id)
	if len(arts) != 1 || arts[0] != a {
		t.Errorf("ArtifactsForEntity should exclude the tombstoned b: %v", arts)
	}
	nbrs, _ := st.Graph.NeighborIDs(ctx, []string{a}, kb.Restricted)
	if len(nbrs) != 0 {
		t.Errorf("NeighborIDs should exclude the tombstoned b: %v", nbrs)
	}
}

func TestGraph_ConnectedZeroHopsAndCycle(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a, b := art(t, st, "ha"), art(t, st, "hb")
	// A cycle a<->b must not loop forever.
	_ = st.Graph.AddLink(ctx, a, b, "references", 1.0)
	_ = st.Graph.AddLink(ctx, b, a, "references", 1.0)

	if z, _ := st.Graph.ConnectedWithin(ctx, a, 0, kb.Restricted); len(z) != 0 {
		t.Errorf("0 hops should be empty, got %v", z)
	}
	got, err := st.Graph.ConnectedWithin(ctx, a, 5, kb.Restricted)
	if err != nil {
		t.Fatalf("connected: %v", err)
	}
	if len(got) != 1 || got[0] != b {
		t.Errorf("cycle traversal = %v, want [b] (start excluded, no infinite loop)", got)
	}
}

func TestGraph_RejectsInvalidVocabulary(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a, b := art(t, st, "ha"), art(t, st, "hb")
	if _, err := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "animal", Name: "Cat"}); !errors.Is(err, ErrInvalidEntityKind) {
		t.Errorf("invalid kind = %v, want ErrInvalidEntityKind", err)
	}
	if err := st.Graph.AddLink(ctx, a, b, "befriends", 1.0); !errors.Is(err, ErrInvalidRelation) {
		t.Errorf("invalid relation = %v, want ErrInvalidRelation", err)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

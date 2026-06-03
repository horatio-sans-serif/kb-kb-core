package store

import (
	"context"
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
	nbrs, err := st.Graph.NeighborIDs(ctx, []string{a1})
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
	one, err := st.Graph.ConnectedWithin(ctx, a, 1)
	if err != nil {
		t.Fatalf("connected 1: %v", err)
	}
	if len(one) != 1 || one[0] != b {
		t.Errorf("1 hop from a = %v, want [b]", one)
	}

	// 2 hops from a: b and c (via the reverse edge c->b).
	two, err := st.Graph.ConnectedWithin(ctx, a, 2)
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

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

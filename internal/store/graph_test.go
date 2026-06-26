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

func TestGraph_MergeEntities(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a, b, c := art(t, st, "ha"), art(t, st, "hb"), art(t, st, "hc")

	// Survivor "Go" mentioned by a, b; loser "Golang" mentioned by b, c (b shared).
	survivor, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "technology", Name: "Go", Aliases: []string{"Go lang"}})
	loser, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "technology", Name: "Golang", Aliases: []string{"Go lang", "golang.org"}})
	_ = st.Graph.LinkArtifactEntity(ctx, a, survivor)
	_ = st.Graph.LinkArtifactEntity(ctx, b, survivor)
	_ = st.Graph.LinkArtifactEntity(ctx, b, loser)
	_ = st.Graph.LinkArtifactEntity(ctx, c, loser)

	if err := st.Graph.MergeEntities(ctx, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}

	// Loser is gone.
	if _, err := st.Graph.GetEntity(ctx, loser); !errors.Is(err, ErrNotFound) {
		t.Errorf("loser should be deleted, GetEntity err = %v", err)
	}

	// artifact_entities re-pointed and deduped: survivor now mentioned by a, b, c.
	arts, err := st.Graph.ArtifactsForEntity(ctx, survivor)
	if err != nil {
		t.Fatalf("artifacts-for-entity: %v", err)
	}
	if len(arts) != 3 || !contains(arts, a) || !contains(arts, b) || !contains(arts, c) {
		t.Errorf("survivor should mention a,b,c (deduped), got %v", arts)
	}

	// Aliases are the union (deduped): "Go lang", "golang.org".
	e, err := st.Graph.GetEntity(ctx, survivor)
	if err != nil {
		t.Fatalf("get survivor: %v", err)
	}
	if len(e.Aliases) != 2 || !contains(e.Aliases, "Go lang") || !contains(e.Aliases, "golang.org") {
		t.Errorf("survivor aliases = %v, want union {Go lang, golang.org}", e.Aliases)
	}
}

func TestGraph_MergeEntitiesIdempotentAndSelfRefused(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a := art(t, st, "ha")
	survivor, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "person", Name: "Ada"})
	loser, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "person", Name: "Lovelace"})
	_ = st.Graph.LinkArtifactEntity(ctx, a, loser)

	// Self-merge is a no-op (refused), survivor untouched.
	if err := st.Graph.MergeEntities(ctx, survivor, survivor); err != nil {
		t.Fatalf("self-merge should be a no-op, got %v", err)
	}
	if _, err := st.Graph.GetEntity(ctx, survivor); err != nil {
		t.Errorf("survivor should survive a self-merge: %v", err)
	}

	if err := st.Graph.MergeEntities(ctx, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}
	// Re-running with a now-gone loser is idempotent (no error).
	if err := st.Graph.MergeEntities(ctx, survivor, loser); err != nil {
		t.Fatalf("re-merge of an already-merged loser should be a no-op, got %v", err)
	}
	arts, _ := st.Graph.ArtifactsForEntity(ctx, survivor)
	if len(arts) != 1 || arts[0] != a {
		t.Errorf("survivor should mention a after merge, got %v", arts)
	}
}

func TestGraph_SetAliases(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a := art(t, st, "ha")
	id, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "org", Name: "Acme", Aliases: []string{"old"}})
	_ = st.Graph.LinkArtifactEntity(ctx, a, id)

	if err := st.Graph.SetAliases(ctx, id, []string{"Acme Inc", "ACME"}); err != nil {
		t.Fatalf("set aliases: %v", err)
	}
	e, _ := st.Graph.GetEntity(ctx, id)
	if len(e.Aliases) != 2 || !contains(e.Aliases, "Acme Inc") || !contains(e.Aliases, "ACME") {
		t.Errorf("aliases = %v, want {Acme Inc, ACME} (replaced)", e.Aliases)
	}

	// Empty set clears the aliases.
	if err := st.Graph.SetAliases(ctx, id, nil); err != nil {
		t.Fatalf("clear aliases: %v", err)
	}
	e, _ = st.Graph.GetEntity(ctx, id)
	if len(e.Aliases) != 0 {
		t.Errorf("aliases after clear = %v, want empty", e.Aliases)
	}
}

func TestGraph_SetAliasesUnknownIs404(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.Graph.SetAliases(ctx, "01HMISSING", []string{"x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("set aliases on missing entity = %v, want ErrNotFound", err)
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

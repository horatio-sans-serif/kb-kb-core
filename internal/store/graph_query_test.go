package store

import (
	"context"
	"errors"
	"testing"

	"kb/internal/kb"
)

func TestGraph_ListEntitiesByMentionCount(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a, b, c := art(t, st, "ha"), art(t, st, "hb"), art(t, st, "hc")

	popular, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "technology", Name: "Go"})
	niche, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "concept", Name: "Esoterica"})
	orphan, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "person", Name: "Nobody"}) // no live mentions
	for _, id := range []string{a, b, c} {
		_ = st.Graph.LinkArtifactEntity(ctx, id, popular)
	}
	_ = st.Graph.LinkArtifactEntity(ctx, a, niche)
	_ = st.Graph.LinkArtifactEntity(ctx, b, orphan)
	_ = st.Artifacts.SoftDelete(ctx, b) // orphan's only mention is now tombstoned

	got, err := st.Graph.ListEntities(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Go (3, but b tombstoned → 2) then Esoterica (1); orphan excluded (0 live).
	if len(got) != 2 || got[0].Entity.Name != "Go" || got[0].ArtifactCount != 2 {
		t.Fatalf("entities = %+v, want [Go(2), Esoterica(1)]", got)
	}
	if got[1].Entity.Name != "Esoterica" || got[1].ArtifactCount != 1 {
		t.Errorf("second entity = %+v, want Esoterica(1)", got[1])
	}
}

func TestGraph_GetEntity(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	id, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "org", Name: "Anthropic", Aliases: []string{"PBC"}})
	got, err := st.Graph.GetEntity(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Anthropic" || len(got.Aliases) != 1 {
		t.Errorf("entity = %+v", got)
	}
	if _, err := st.Graph.GetEntity(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
}

func TestGraph_Backlinks(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	target, a, b, gone := art(t, st, "ht"), art(t, st, "ha"), art(t, st, "hb"), art(t, st, "hg")

	// a and gone reference target; b does not. target references b (outgoing, not a backlink).
	_ = st.Graph.AddLink(ctx, a, target, "references", 1.0)
	_ = st.Graph.AddLink(ctx, gone, target, "references", 1.0)
	_ = st.Graph.AddLink(ctx, target, b, "references", 1.0)
	_ = st.Artifacts.SoftDelete(ctx, gone)

	back, err := st.Graph.Backlinks(ctx, target, 10)
	if err != nil {
		t.Fatalf("backlinks: %v", err)
	}
	// Only a (live, incoming). gone is tombstoned; b is outgoing.
	if len(back) != 1 || back[0].ID != a || back[0].Title != "ha" || back[0].Relation != "references" {
		t.Errorf("backlinks = %+v, want [a/references with title]", back)
	}
}

func TestGraph_EntityArtifactsLimit(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	id, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "technology", Name: "Go"})
	for i := 0; i < 5; i++ {
		a := art(t, st, "h"+string(rune('a'+i)))
		_ = st.Graph.LinkArtifactEntity(ctx, a, id)
	}
	refs, err := st.Graph.EntityArtifacts(ctx, id, 3)
	if err != nil {
		t.Fatalf("entity artifacts: %v", err)
	}
	if len(refs) != 3 {
		t.Errorf("limit 3 should cap the result, got %d", len(refs))
	}
	if refs[0].Title == "" {
		t.Errorf("display fields should be joined in, got %+v", refs[0])
	}
}

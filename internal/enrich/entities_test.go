package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"kb/internal/kb"
	"kb/internal/store"
	"kb/internal/storetest"
)

const entitiesJSON = `{"entities":[
  {"name":"Go","kind":"technology","aliases":["Golang"]},
  {"name":"Rob Pike","kind":"person"},
  {"name":"  ","kind":"concept"},
  {"name":"Cat","kind":"animal"}
]}`

func TestEntities_Contract(t *testing.T) {
	e := NewEntities(&fakeLLM{}, "m", true)
	if e.Name() != "entities" {
		t.Errorf("Name = %q", e.Name())
	}
	if len(e.DependsOn()) != 1 || e.DependsOn()[0] != "fetch" {
		t.Errorf("DependsOn = %v, want [fetch]", e.DependsOn())
	}
	// Privacy gate: on-device allows all tiers; a remote endpoint gates sensitive.
	if !e.PrivacyOK(kb.Sensitive) {
		t.Error("local entities should allow sensitive")
	}
	remote := NewEntities(&fakeLLM{}, "m", false)
	if remote.PrivacyOK(kb.Sensitive) || !remote.PrivacyOK(kb.Public) {
		t.Error("remote entities must gate sensitive but allow public")
	}
}

func TestEntities_ExtractsLinksAndSharesEdge(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)

	h1, h2 := "h1", "h2"
	a, err := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: "note", Title: "T", BodyMarkdown: "Go by Rob Pike", ContentHash: &h1, PrivacyTier: kb.Public})
	if err != nil {
		t.Fatalf("seed a: %v", err)
	}
	b, err := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: "note", Title: "Other", BodyMarkdown: "x", ContentHash: &h2, PrivacyTier: kb.Public})
	if err != nil {
		t.Fatalf("seed b: %v", err)
	}
	// b already mentions Go.
	goID, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "technology", Name: "Go"})
	_ = st.Graph.LinkArtifactEntity(ctx, b.ID, goID)

	e := NewEntities(&fakeLLM{resp: entitiesJSON}, "test-model", true)
	res, err := e.Enrich(ctx, a)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	// One enrichment row records the extraction.
	if len(res.Enrichments) != 1 || res.Enrichments[0].Kind != "entities" || res.Enrichments[0].ArtifactID != a.ID {
		t.Fatalf("enrichment row = %+v, want one 'entities' row", res.Enrichments)
	}
	// The graph is written by the ApplyGraph hook (run by the pipeline in its tx).
	if res.ApplyGraph == nil {
		t.Fatal("expected an ApplyGraph hook")
	}
	if err := res.ApplyGraph(ctx, st.Graph); err != nil {
		t.Fatalf("apply graph: %v", err)
	}

	// Only the two valid entities are linked (blank name and invalid 'animal' kind dropped).
	ents, _ := st.Graph.EntitiesForArtifact(ctx, a.ID)
	if len(ents) != 2 {
		t.Errorf("artifact should link to 2 valid entities (Go, Rob Pike), got %d: %+v", len(ents), ents)
	}

	// A shares_entity edge connects a and b (both mention Go).
	nbrs, _ := st.Graph.NeighborIDs(ctx, []string{a.ID}, kb.Restricted)
	if len(nbrs) != 1 || nbrs[0] != b.ID {
		t.Errorf("expected a shares_entity edge to b, neighbors = %v", nbrs)
	}

	// The payload lists the extracted entities.
	var payload struct {
		Entities []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"entities"`
	}
	_ = json.Unmarshal([]byte(res.Enrichments[0].Payload), &payload)
	if len(payload.Entities) != 2 {
		t.Errorf("payload should list 2 entities, got %+v", payload.Entities)
	}
}

func TestEntities_EmptyBodyNoOp(t *testing.T) {
	ctx := context.Background()
	e := NewEntities(&fakeLLM{resp: entitiesJSON}, "m", true)
	res, err := e.Enrich(ctx, &kb.Artifact{ID: "01X", BodyMarkdown: "   "})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(res.Enrichments) != 0 || res.ApplyGraph != nil {
		t.Errorf("empty body should produce no enrichment and no graph hook, got %+v", res)
	}
}

func TestEntities_ApplyGraphAtomicRollback(t *testing.T) {
	// The graph write runs in the pipeline's tx. If that tx rolls back, NO graph
	// rows must persist — the property that makes a failed/retried attempt safe.
	ctx := context.Background()
	st := storetest.New(t)
	h := "h1"
	a, _ := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: "note", Title: "T", BodyMarkdown: "Go by Rob Pike", ContentHash: &h, PrivacyTier: kb.Public})

	res, err := NewEntities(&fakeLLM{resp: entitiesJSON}, "m", true).Enrich(ctx, a)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	boom := errors.New("boom")
	err = st.Tx(ctx, func(tx *store.Store) error {
		if err := res.ApplyGraph(ctx, tx.Graph); err != nil {
			return err
		}
		return boom // force rollback after the graph writes
	})
	if !errors.Is(err, boom) {
		t.Fatalf("tx = %v, want boom", err)
	}
	ents, _ := st.Graph.EntitiesForArtifact(ctx, a.ID)
	if len(ents) != 0 {
		t.Errorf("rolled-back graph writes must not persist, got %d entity links", len(ents))
	}
}

func TestEntities_HubFanoutCapped(t *testing.T) {
	// A hub entity (mentioned by more than the fan-out cap) must not create new
	// pairwise shares_entity edges, but the membership link is still recorded.
	ctx := context.Background()
	st := storetest.New(t)
	hub, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "technology", Name: "AI"})
	for i := 0; i < maxSharesEntityFanout+5; i++ {
		h := "seed" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		seed, _ := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: "note", Title: "s", BodyMarkdown: "b", ContentHash: &h, PrivacyTier: kb.Public})
		_ = st.Graph.LinkArtifactEntity(ctx, seed.ID, hub)
	}

	hn := "newcomer"
	newcomer, _ := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: "note", Title: "new", BodyMarkdown: "About AI", ContentHash: &hn, PrivacyTier: kb.Public})
	res, _ := NewEntities(&fakeLLM{resp: `{"entities":[{"name":"AI","kind":"technology"}]}`}, "m", true).Enrich(ctx, newcomer)
	if err := res.ApplyGraph(ctx, st.Graph); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Membership recorded, but no pairwise edges (the hub is over the cap).
	if arts, _ := st.Graph.ArtifactsForEntity(ctx, hub); len(arts) != maxSharesEntityFanout+6 {
		t.Errorf("newcomer should still be linked to the hub entity, got %d members", len(arts))
	}
	if nbrs, _ := st.Graph.NeighborIDs(ctx, []string{newcomer.ID}, kb.Restricted); len(nbrs) != 0 {
		t.Errorf("a hub entity must not create pairwise edges, got %d neighbors", len(nbrs))
	}
}

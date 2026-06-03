package enrich

import (
	"context"
	"encoding/json"
	"testing"

	"kb/internal/kb"
	"kb/internal/storetest"
)

const entitiesJSON = `{"entities":[
  {"name":"Go","kind":"technology","aliases":["Golang"]},
  {"name":"Rob Pike","kind":"person"},
  {"name":"  ","kind":"concept"},
  {"name":"Cat","kind":"animal"}
]}`

func TestEntities_Contract(t *testing.T) {
	e := NewEntities(&fakeLLM{}, "m", true, nil)
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
	remote := NewEntities(&fakeLLM{}, "m", false, nil)
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

	e := NewEntities(&fakeLLM{resp: entitiesJSON}, "test-model", true, st.Graph)
	res, err := e.Enrich(ctx, a)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	// One enrichment row records the extraction.
	if len(res.Enrichments) != 1 || res.Enrichments[0].Kind != "entities" || res.Enrichments[0].ArtifactID != a.ID {
		t.Fatalf("enrichment row = %+v, want one 'entities' row", res.Enrichments)
	}

	// Only the two valid entities are linked (blank name and invalid 'animal' kind dropped).
	ents, _ := st.Graph.EntitiesForArtifact(ctx, a.ID)
	if len(ents) != 2 {
		t.Errorf("artifact should link to 2 valid entities (Go, Rob Pike), got %d: %+v", len(ents), ents)
	}

	// A shares_entity edge connects a and b (both mention Go).
	nbrs, _ := st.Graph.NeighborIDs(ctx, []string{a.ID})
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
	st := storetest.New(t)
	e := NewEntities(&fakeLLM{resp: entitiesJSON}, "m", true, st.Graph)
	res, err := e.Enrich(ctx, &kb.Artifact{ID: "01X", BodyMarkdown: "   "})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(res.Enrichments) != 0 {
		t.Errorf("empty body should produce no enrichment, got %d", len(res.Enrichments))
	}
}

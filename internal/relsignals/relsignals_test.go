package relsignals

import (
	"context"
	"encoding/json"
	"testing"

	"kb/internal/enrich"
	"kb/internal/kb"
	"kb/internal/storetest"
)

// compile-time check: Provider satisfies the enricher's SignalProvider seam.
var _ enrich.SignalProvider = (*Provider)(nil)

func TestProvider_Signals(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()

	src := &kb.Source{Kind: "PUSH", Connector: "x"}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	src.SourceWeight = 2.5
	if err := st.Sources.Update(ctx, src); err != nil {
		t.Fatalf("update source: %v", err)
	}
	a, err := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: kb.Note, BodyMarkdown: "b", ContentHash: strptr("h")})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if err := st.Encounters.Record(ctx, &kb.Encounter{ArtifactID: a.ID, SourceID: src.ID}); err != nil {
		t.Fatalf("record encounter: %v", err)
	}

	p := New(st)

	// Before any embedding: empty vector, real count/weight.
	sig, err := p.Signals(ctx, a.ID)
	if err != nil {
		t.Fatalf("signals (no embedding): %v", err)
	}
	if len(sig.Embedding) != 0 {
		t.Errorf("embedding = %v, want empty before any embed row", sig.Embedding)
	}
	if sig.Encounters != 1 || sig.SourceWeight != 2.5 {
		t.Errorf("signals = %+v, want encounters=1 weight=2.5", sig)
	}

	// After an embedding row: the vector is returned.
	payload, _ := json.Marshal(kb.EmbeddingPayload{Model: "m", Dim: 3, Vector: []float32{0.1, 0.2, 0.3}})
	if err := st.Enrichments.Create(ctx, &kb.Enrichment{ArtifactID: a.ID, Kind: "embedding", Payload: string(payload), Model: "m", PromptVersion: "v1"}); err != nil {
		t.Fatalf("create embedding: %v", err)
	}
	sig, err = p.Signals(ctx, a.ID)
	if err != nil {
		t.Fatalf("signals (with embedding): %v", err)
	}
	if len(sig.Embedding) != 3 || sig.Embedding[1] != 0.2 {
		t.Errorf("embedding = %v, want [0.1 0.2 0.3]", sig.Embedding)
	}
}

func strptr(s string) *string { return &s }

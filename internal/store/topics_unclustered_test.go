package store

import (
	"context"
	"encoding/json"
	"testing"

	"kb/internal/kb"
)

func embedArtifact(t *testing.T, st *Store, hash, title string, status string, vec []float32) string {
	t.Helper()
	ctx := context.Background()
	a, err := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", Title: title, BodyMarkdown: "b", ContentHash: &hash, PrivacyTier: kb.Public})
	if err != nil {
		t.Fatalf("artifact %s: %v", title, err)
	}
	stt := kb.ArtifactStatus(status)
	_ = st.Artifacts.Patch(ctx, a.ID, kb.ArtifactPatch{Status: &stt})
	payload, _ := json.Marshal(kb.EmbeddingPayload{Model: "m", Dim: len(vec), Vector: vec})
	if err := st.Enrichments.Create(ctx, &Enrichment{ArtifactID: a.ID, Kind: "embedding", Payload: string(payload)}); err != nil {
		t.Fatalf("embedding %s: %v", title, err)
	}
	return a.ID
}

func TestTopicRepo_UnclusteredEmbeddings(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	a := embedArtifact(t, st, "ha", "A", "ready", []float32{1, 0})
	b := embedArtifact(t, st, "hb", "B", "ready", []float32{0, 1})
	embedArtifact(t, st, "hd", "Draft", "enriching", []float32{1, 1}) // not ready → excluded

	// Assign b to a topic → it is no longer unclustered.
	tp := &Topic{Label: "t", Centroid: []float32{0, 1}}
	_ = st.Topics.Create(ctx, tp)
	_ = st.Topics.Assign(ctx, b, tp.ID, 1.0)

	cands, err := st.Topics.UnclusteredEmbeddings(ctx, 10)
	if err != nil {
		t.Fatalf("unclustered: %v", err)
	}
	if len(cands) != 1 || cands[0].ArtifactID != a || cands[0].Title != "A" || len(cands[0].Embedding) != 2 {
		t.Fatalf("unclustered = %+v, want only A (ready, embedded, unassigned)", cands)
	}
	if cands[0].Embedding[0] != 1 {
		t.Errorf("embedding not parsed: %+v", cands[0].Embedding)
	}
}

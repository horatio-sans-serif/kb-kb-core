package topics

import (
	"context"
	"encoding/json"
	"testing"

	"kb/internal/kb"
	"kb/internal/store"
	"kb/internal/storetest"
)

func seed(t *testing.T, st *store.Store, hash, title string, vec []float32) string {
	t.Helper()
	ctx := context.Background()
	a, err := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: "note", Title: title, BodyMarkdown: "b", ContentHash: &hash, PrivacyTier: kb.Public})
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	ready := kb.StatusReady
	_ = st.Artifacts.Patch(ctx, a.ID, kb.ArtifactPatch{Status: &ready})
	payload, _ := json.Marshal(kb.EmbeddingPayload{Model: "m", Dim: len(vec), Vector: vec})
	_ = st.Enrichments.Create(ctx, &kb.Enrichment{ArtifactID: a.ID, Kind: "embedding", Payload: string(payload)})
	return a.ID
}

func TestClusterer_GroupsSimilarSeparatesDifferent(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	a := seed(t, st, "ha", "Alpha", []float32{1, 0})
	b := seed(t, st, "hb", "Beta", []float32{0.99, 0.01}) // ~A
	c := seed(t, st, "hc", "Gamma", []float32{0, 1})      // orthogonal

	n, err := New(st).Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 3 {
		t.Errorf("sweep clustered %d, want 3", n)
	}

	ta, _ := st.Topics.TopicsForArtifact(ctx, a)
	tb, _ := st.Topics.TopicsForArtifact(ctx, b)
	tc, _ := st.Topics.TopicsForArtifact(ctx, c)
	if len(ta) != 1 || len(tb) != 1 || len(tc) != 1 {
		t.Fatalf("each artifact should be in exactly one topic: a=%d b=%d c=%d", len(ta), len(tb), len(tc))
	}
	if ta[0].ID != tb[0].ID {
		t.Errorf("similar artifacts A and B should share a topic")
	}
	if tc[0].ID == ta[0].ID {
		t.Errorf("orthogonal artifact C should be in its own topic")
	}
	// Two topics total.
	all, _ := st.Topics.ListWithCentroids(ctx)
	if len(all) != 2 {
		t.Errorf("topics = %d, want 2", len(all))
	}
}

func TestClusterer_SecondSweepIsNoOp(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	seed(t, st, "ha", "Alpha", []float32{1, 0})
	cl := New(st)
	if n, _ := cl.Sweep(ctx); n != 1 {
		t.Fatalf("first sweep clustered %d, want 1", n)
	}
	if n, err := cl.Sweep(ctx); err != nil || n != 0 {
		t.Errorf("second sweep = (%d, %v), want (0, nil) — already clustered", n, err)
	}
}

func TestClusterer_CentroidIsRunningMean(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	seed(t, st, "ha", "A", []float32{2, 0})
	seed(t, st, "hb", "B", []float32{0, 2}) // cosine 0 to A → but we force one topic via threshold 0
	cl := NewWithThreshold(st, 0.0, 100)    // threshold 0 → everything joins the first topic
	if _, err := cl.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	all, _ := st.Topics.ListWithCentroids(ctx)
	if len(all) != 1 {
		t.Fatalf("threshold 0 should yield one topic, got %d", len(all))
	}
	// Centroid = mean of [2,0] and [0,2] = [1,1].
	cen := all[0].Centroid
	if len(cen) != 2 || cen[0] != 1 || cen[1] != 1 {
		t.Errorf("centroid = %v, want the running mean [1,1]", cen)
	}
}

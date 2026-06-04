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

func TestClusterer_AddsSharesTopicEdges(t *testing.T) {
	// Artifacts clustered into the same topic gain a shares_topic graph edge.
	ctx := context.Background()
	st := storetest.New(t)
	a := seed(t, st, "ha", "Alpha", []float32{1, 0})
	b := seed(t, st, "hb", "Beta", []float32{0.99, 0.01}) // joins A's topic
	c := seed(t, st, "hc", "Gamma", []float32{0, 1})      // separate topic

	if _, err := New(st).Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// A and B (co-topic) are graph neighbors; C is not.
	nbrs, _ := st.Graph.NeighborIDs(ctx, []string{a})
	if len(nbrs) != 1 || nbrs[0] != b {
		t.Errorf("co-topic artifacts should share a graph edge: A neighbors = %v, want [B]", nbrs)
	}
	if cn, _ := st.Graph.NeighborIDs(ctx, []string{c}); len(cn) != 0 {
		t.Errorf("a separate-topic artifact should have no co-topic edge, got %v", cn)
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
	// Three members with an asymmetric mean, so an off-by-one in the member count
	// (reading N after the assign) would give a different, detectable result.
	seed(t, st, "ha", "A", []float32{6, 0})
	seed(t, st, "hb", "B", []float32{3, 0})
	seed(t, st, "hc", "C", []float32{0, 3})
	cl := NewWithThreshold(st, 0.0, 100) // threshold 0 → all join the first topic
	if _, err := cl.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	all, _ := st.Topics.ListWithCentroids(ctx)
	if len(all) != 1 {
		t.Fatalf("threshold 0 should yield one topic, got %d", len(all))
	}
	// Mean of [6,0],[3,0],[0,3] = [3,1].
	cen := all[0].Centroid
	if len(cen) != 2 || cen[0] != 3 || cen[1] != 1 {
		t.Errorf("centroid = %v, want the running mean [3,1]", cen)
	}
}

func TestClusterer_AssignIsIdempotent(t *testing.T) {
	// Re-running the clusterer must not re-fold an already-clustered artifact's
	// vector into its topic centroid (the centroid update is not naturally
	// idempotent; the in-tx membership re-check makes it so).
	ctx := context.Background()
	st := storetest.New(t)
	a := seed(t, st, "ha", "A", []float32{2, 0})
	cl := New(st)
	if _, err := cl.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	before, _ := st.Topics.ListWithCentroids(ctx)

	// Re-cluster the same already-assigned artifact directly.
	if err := cl.assign(ctx, store.ClusterCandidate{ArtifactID: a, Embedding: []float32{2, 0}}); err != nil {
		t.Fatalf("re-assign: %v", err)
	}
	after, _ := st.Topics.ListWithCentroids(ctx)
	if len(after) != 1 || after[0].Centroid[0] != before[0].Centroid[0] {
		t.Errorf("centroid changed on re-assign: before %v after %v", before[0].Centroid, after[0].Centroid)
	}
}

package search

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"kb/internal/kb"
	"kb/internal/llm"
	"kb/internal/storetest"
)

// queryEmbedder returns a fixed query vector regardless of input.
type queryEmbedder struct {
	vec []float32
	err error
}

func (q queryEmbedder) Embed(_ context.Context, _ llm.EmbedRequest) ([]float32, error) {
	return q.vec, q.err
}

func TestSemantic_RanksByCosine(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()

	// Seed three artifacts at known cosines to the query {1,0}.
	seed := func(hash string, vec []float32) string {
		a, err := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: kb.Note, BodyMarkdown: hash, ContentHash: &hash})
		if err != nil {
			t.Fatalf("create artifact: %v", err)
		}
		payload, _ := json.Marshal(kb.EmbeddingPayload{Model: "m", Dim: len(vec), Vector: vec})
		if err := st.Enrichments.Create(ctx, &kb.Enrichment{ArtifactID: a.ID, Kind: "embedding", Payload: string(payload), Model: "m", PromptVersion: "v1"}); err != nil {
			t.Fatalf("create embedding: %v", err)
		}
		return a.ID
	}
	aligned := seed("aligned", []float32{1, 0})       // cos 1
	orthogonal := seed("orthogonal", []float32{0, 1}) // cos 0
	opposite := seed("opposite", []float32{-1, 0})    // cos -1
	_ = opposite

	s := NewSearcher(st, queryEmbedder{vec: []float32{1, 0}}, "m")

	results, err := s.Semantic(ctx, "find aligned things", 2)
	if err != nil {
		t.Fatalf("semantic: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (top-k)", len(results))
	}
	if results[0].ArtifactID != aligned {
		t.Errorf("rank 0 = %s, want the aligned artifact %s", results[0].ArtifactID, aligned)
	}
	if results[1].ArtifactID != orthogonal {
		t.Errorf("rank 1 = %s, want the orthogonal artifact %s", results[1].ArtifactID, orthogonal)
	}
	if results[0].Score <= results[1].Score {
		t.Errorf("scores not descending: %v then %v", results[0].Score, results[1].Score)
	}
}

func TestSemantic_SkipsDimMismatch(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()

	good := "good"
	a, _ := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: kb.Note, BodyMarkdown: good, ContentHash: &good})
	gp, _ := json.Marshal(kb.EmbeddingPayload{Model: "m", Dim: 2, Vector: []float32{1, 0}})
	_ = st.Enrichments.Create(ctx, &kb.Enrichment{ArtifactID: a.ID, Kind: "embedding", Payload: string(gp), Model: "m", PromptVersion: "v1"})

	stale := "stale"
	b, _ := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: kb.Note, BodyMarkdown: stale, ContentHash: &stale})
	bp, _ := json.Marshal(kb.EmbeddingPayload{Model: "old", Dim: 3, Vector: []float32{1, 0, 0}})
	_ = st.Enrichments.Create(ctx, &kb.Enrichment{ArtifactID: b.ID, Kind: "embedding", Payload: string(bp), Model: "old", PromptVersion: "v1"})

	s := NewSearcher(st, queryEmbedder{vec: []float32{1, 0}}, "m")
	results, err := s.Semantic(ctx, "q", 10)
	if err != nil {
		t.Fatalf("semantic: %v", err)
	}
	if len(results) != 1 || results[0].ArtifactID != a.ID {
		t.Errorf("results = %+v, want only the dimension-matching artifact %s", results, a.ID)
	}
}

func TestSemantic_EmptyQueryIsError(t *testing.T) {
	st := storetest.New(t)
	s := NewSearcher(st, queryEmbedder{vec: []float32{1, 0}}, "m")
	if _, err := s.Semantic(context.Background(), "   ", 5); err == nil {
		t.Error("an empty query should be an error")
	}
}

func TestSemantic_EmbedErrorPropagates(t *testing.T) {
	st := storetest.New(t)
	s := NewSearcher(st, queryEmbedder{err: errors.New("ollama down")}, "m")
	if _, err := s.Semantic(context.Background(), "q", 5); err == nil {
		t.Error("an embed error must propagate")
	}
}

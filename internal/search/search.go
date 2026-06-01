// Package search is semantic retrieval over the knowledge base (design §8.8): it
// embeds a query and ranks artifacts by cosine similarity to their stored
// embeddings. Stage 1 ranks in-process (brute force) — correct and fine at
// personal scale; the vec0 ANN index is a localized swap behind this same type.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"kb/internal/kb"
	"kb/internal/llm"
	"kb/internal/relevance"
	"kb/internal/store"
)

const (
	defaultK = 20
	maxK     = 200
)

// Result is one ranked hit: an artifact and its similarity to the query.
type Result struct {
	ArtifactID string  `json:"artifact_id"`
	Score      float64 `json:"score"`
}

// Searcher ranks artifacts by semantic similarity to a query.
type Searcher struct {
	st       *store.Store
	embedder llm.Embedder
	model    string
}

// NewSearcher builds a Searcher over the store and an embed client. model is the
// embed model used for the query (must match the model artifacts were embedded
// with for the vectors to share a space).
func NewSearcher(st *store.Store, embedder llm.Embedder, model string) *Searcher {
	return &Searcher{st: st, embedder: embedder, model: model}
}

// Semantic embeds the query and returns the top-k artifacts by cosine similarity
// to their latest embedding. k <= 0 uses a default; k is capped. Vectors whose
// dimension differs from the query (a stale embed model) are skipped rather than
// scored 0. Results are ordered by descending score, ties broken by artifact id
// for determinism.
func (s *Searcher) Semantic(ctx context.Context, query string, k int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("search: empty query")
	}
	switch {
	case k <= 0:
		k = defaultK
	case k > maxK:
		k = maxK
	}

	qvec, err := s.embedder.Embed(ctx, llm.EmbedRequest{Model: s.model, Input: query})
	if err != nil {
		return nil, fmt.Errorf("search: embed query: %w", err)
	}
	if len(qvec) == 0 {
		return nil, fmt.Errorf("search: empty query embedding")
	}

	rows, err := s.st.Enrichments.LatestEmbeddings(ctx)
	if err != nil {
		return nil, err
	}

	results := make([]Result, 0, len(rows))
	for _, r := range rows {
		var ep kb.EmbeddingPayload
		if json.Unmarshal([]byte(r.Payload), &ep) != nil {
			continue // skip a corrupt payload rather than fail the whole query
		}
		if len(ep.Vector) != len(qvec) {
			continue // dimension mismatch (stale embed model) — not comparable
		}
		results = append(results, Result{ArtifactID: r.ArtifactID, Score: relevance.Cosine(qvec, ep.Vector)})
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].ArtifactID < results[j].ArtifactID
	})
	if len(results) > k {
		results = results[:k]
	}
	return results, nil
}

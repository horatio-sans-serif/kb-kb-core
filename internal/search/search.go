// Package search is semantic retrieval over the knowledge base (design §8.8): it
// embeds a query and ranks artifacts by cosine similarity to their stored
// embeddings. Stage 1 ranks in-process (brute force) — correct and fine at
// personal scale; the vec0 ANN index is a localized swap behind this same type.
package search

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

// ErrRemoteDisallowed is returned when the embed endpoint is not on-device: a
// search query would otherwise be sent in plaintext to a remote model, leaking
// the user's intent even though sensitive content is gated off embedding. The API
// maps it to 503.
var ErrRemoteDisallowed = errors.New("search: semantic search requires an on-device embed endpoint")

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
	local    bool
}

// NewSearcher builds a Searcher over the store and an embed client. model is the
// embed model used for the query (must match the model artifacts were embedded
// with). local must reflect whether the embed endpoint is on-device; when false,
// Semantic refuses so query text never leaves the machine.
func NewSearcher(st *store.Store, embedder llm.Embedder, model string, local bool) *Searcher {
	return &Searcher{st: st, embedder: embedder, model: model, local: local}
}

// Semantic embeds the query and returns up to k artifacts by descending cosine
// similarity to their latest embedding. k <= 0 uses a default; k is capped.
// Vectors whose dimension differs from the query (a stale embed model), corrupt
// payloads, and non-finite or non-positive scores are skipped — a "find by
// meaning" result set should not include unrelated or anti-correlated artifacts.
// Memory is bounded to O(k): rows are streamed into a top-k heap, never all held.
func (s *Searcher) Semantic(ctx context.Context, query string, k int) ([]Result, error) {
	if !s.local {
		return nil, ErrRemoteDisallowed
	}
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

	h := &resultHeap{}
	err = s.st.Enrichments.ForEachLatestEmbedding(ctx, func(artifactID, payload string) error {
		var ep kb.EmbeddingPayload
		if json.Unmarshal([]byte(payload), &ep) != nil {
			return nil // skip a corrupt payload rather than fail the whole query
		}
		if len(ep.Vector) != len(qvec) {
			return nil // dimension mismatch (stale embed model) — not comparable
		}
		score := relevance.Cosine(qvec, ep.Vector)
		if math.IsNaN(score) || math.IsInf(score, 0) || score <= 0 {
			return nil // non-finite (a pre-validation row) or non-positive — not a match
		}
		heap.Push(h, Result{ArtifactID: artifactID, Score: score})
		if h.Len() > k {
			heap.Pop(h) // drop the current lowest, keeping the top k
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Drain the min-heap into descending order (greatest popped last).
	out := make([]Result, h.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(h).(Result)
	}
	return out, nil
}

// resultHeap is a bounded min-heap of results: the smallest (by score, then
// largest artifact id) sits at the root so it is the first dropped when over
// capacity. The id tiebreak makes the kept top-k deterministic on equal scores.
type resultHeap []Result

func (h resultHeap) Len() int { return len(h) }
func (h resultHeap) Less(i, j int) bool {
	if h[i].Score != h[j].Score {
		return h[i].Score < h[j].Score
	}
	return h[i].ArtifactID > h[j].ArtifactID
}
func (h resultHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *resultHeap) Push(x any)   { *h = append(*h, x.(Result)) }
func (h *resultHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

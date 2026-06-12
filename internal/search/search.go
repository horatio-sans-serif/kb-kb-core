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
	"regexp"
	"sort"
	"strings"

	"kb/internal/kb"
	"kb/internal/llm"
	"kb/internal/relevance"
	"kb/internal/store"
	"kb/internal/vecsearch"
)

const (
	defaultK = 20
	maxK     = 200
	// fuseDepth is how many candidates Hybrid pulls from each ranker before
	// fusing, so an item ranked modestly by one but well by the other still
	// surfaces. Capped at maxK.
	fuseDepth = 50
	// rrfK is the Reciprocal Rank Fusion constant (the standard 60): it damps the
	// weight of top ranks so a single ranker cannot dominate the blend.
	rrfK = 60
)

func clampK(k int) int {
	switch {
	case k <= 0:
		return defaultK
	case k > maxK:
		return maxK
	default:
		return k
	}
}

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
	index    vecsearch.Index // optional vec0 ANN index; nil → brute-force scan
}

// NewSearcher builds a Searcher over the store and an embed client. model is the
// embed model used for the query (must match the model artifacts were embedded
// with). local must reflect whether the embed endpoint is on-device; when false,
// Semantic refuses so query text never leaves the machine.
func NewSearcher(st *store.Store, embedder llm.Embedder, model string, local bool) *Searcher {
	return &Searcher{st: st, embedder: embedder, model: model, local: local}
}

// SetIndex installs the vec0 ANN index so Semantic uses sublinear nearest-neighbor
// retrieval instead of the O(n) brute-force scan. nil (the default) keeps brute force.
func (s *Searcher) SetIndex(index vecsearch.Index) { s.index = index }

// Semantic embeds the query and returns up to k artifacts by descending cosine
// similarity to their latest embedding. k <= 0 uses a default; k is capped.
// Vectors whose dimension differs from the query (a stale embed model), corrupt
// payloads, and non-finite or non-positive scores are skipped — a "find by
// meaning" result set should not include unrelated or anti-correlated artifacts.
// Memory is bounded to O(k): rows are streamed into a top-k heap, never all held.
func (s *Searcher) Semantic(ctx context.Context, query string, k int, ceiling kb.PrivacyTier) ([]Result, error) {
	if !s.local {
		return nil, ErrRemoteDisallowed
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("search: empty query")
	}
	k = clampK(k)

	qvec, err := s.embedder.Embed(ctx, llm.EmbedRequest{Model: s.model, Input: query})
	if err != nil {
		return nil, fmt.Errorf("search: embed query: %w", err)
	}
	if len(qvec) == 0 {
		return nil, fmt.Errorf("search: empty query embedding")
	}

	// Sublinear path: when the vec0 ANN index is present and returns a FULL k hits, use
	// it. Fall back to the exact brute-force scan on an index error (e.g. a
	// query-dimension mismatch) OR a short result (fewer than k hits).
	//
	// Short results have two causes, and the fallback is correct for both. (1) The index
	// may not have been reconciled yet (the startup window before the first rebuild), so
	// an empty/partial index must not shadow matches the brute-force scan would find.
	// (2) The index applies the read-ceiling and live filters AFTER the k-NN over-fetch
	// (LIMIT k+liveBuffer), so when many of the nearest neighbors are above the caller's
	// ceiling or tombstoned, the index can return fewer than k in-ceiling results even
	// though k exist — the brute-force scan filters BEFORE scoring (ForEachLatestEmbedding
	// Within streams only in-ceiling rows), so it is exact and complete. A genuinely small
	// corpus makes both paths return the same <k set, so the fallback only costs a wasted
	// scan in that case.
	if s.index != nil {
		if hits, err := s.index.Search(ctx, qvec, k, ceiling); err == nil && len(hits) >= k {
			out := make([]Result, len(hits))
			for i, h := range hits {
				out[i] = Result{ArtifactID: h.ArtifactID, Score: h.Score}
			}
			return out, nil
		}
	}

	h := &resultHeap{}
	err = s.st.Enrichments.ForEachLatestEmbeddingWithin(ctx, ceiling, func(artifactID, payload string) error {
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

// ftsTerm matches a run of letters/digits (Unicode-aware); everything else is a
// separator. Used to turn arbitrary user input into safe FTS5 query terms.
var ftsTerm = regexp.MustCompile(`[\p{L}\p{N}]+`)

// Keyword runs a full-text (FTS5) search over artifact title/body. It needs no
// embedder and no locality check — the query never leaves SQLite — so it works
// even when semantic search is disallowed. User input is reduced to quoted word
// terms (implicit AND), which both defends against FTS5 syntax errors/injection
// and makes the match a plain bag-of-words. A query with no word terms returns no
// results (not an error); an empty query is an error.
func (s *Searcher) Keyword(ctx context.Context, query string, k int, ceiling kb.PrivacyTier) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("search: empty query")
	}
	k = clampK(k)

	match := ftsMatch(query)
	if match == "" {
		return []Result{}, nil // no searchable terms
	}
	hits, err := s.st.Artifacts.SearchKeyword(ctx, match, k, ceiling)
	if err != nil {
		return nil, err
	}
	out := make([]Result, len(hits))
	for i, h := range hits {
		out[i] = Result{ArtifactID: h.ArtifactID, Score: h.Score}
	}
	return out, nil
}

// Hybrid blends keyword and semantic results with Reciprocal Rank Fusion (design
// §8.8). Keyword runs always (local, reliable); semantic is best-effort — if it
// is unavailable (a non-local embed endpoint, or the embed model is down) Hybrid
// degrades to keyword-only rather than failing, since the keyword half still
// serves the user. RRF is rank-based, so it fuses the two incomparable score
// scales (cosine vs bm25) without normalization.
func (s *Searcher) Hybrid(ctx context.Context, query string, k int, ceiling kb.PrivacyTier) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("search: empty query")
	}
	k = clampK(k)
	depth := k
	if depth < fuseDepth {
		depth = fuseDepth // fuseDepth and k are both <= maxK, so depth needs no re-clamp
	}

	keyword, err := s.Keyword(ctx, query, depth, ceiling)
	if err != nil {
		return nil, err
	}
	semantic, err := s.Semantic(ctx, query, depth, ceiling)
	if err != nil {
		// Degrade to keyword-only only when semantic is legitimately unavailable
		// (non-local endpoint, model down). A cancelled/expired context means the
		// request itself is dead — propagate it rather than fabricating a 200 with
		// a silently-degraded ranking.
		if ctx.Err() != nil {
			return nil, err
		}
		semantic = nil
	}
	return rrf(k, semantic, keyword), nil
}

// rrf fuses ranked lists by Reciprocal Rank Fusion: each artifact accumulates
// 1/(rrfK + rank) across the lists it appears in (rank is 0-based). Returns the
// top-k by fused score, ties broken by artifact id for determinism.
func rrf(k int, lists ...[]Result) []Result {
	scores := make(map[string]float64)
	for _, list := range lists {
		for rank, r := range list {
			scores[r.ArtifactID] += 1.0 / float64(rrfK+rank)
		}
	}
	fused := make([]Result, 0, len(scores))
	for id, sc := range scores {
		fused = append(fused, Result{ArtifactID: id, Score: sc})
	}
	sort.Slice(fused, func(i, j int) bool {
		if fused[i].Score != fused[j].Score {
			return fused[i].Score > fused[j].Score
		}
		return fused[i].ArtifactID < fused[j].ArtifactID
	})
	if len(fused) > k {
		fused = fused[:k]
	}
	return fused
}

// ftsMatch builds a safe FTS5 MATCH expression from raw user input: each word
// term is double-quoted (so FTS5 treats it literally, neutralizing operators like
// OR/NEAR/"/( ) and column filters), joined by spaces for an implicit AND.
func ftsMatch(raw string) string {
	terms := ftsTerm.FindAllString(raw, -1)
	if len(terms) == 0 {
		return ""
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + t + `"`
	}
	return strings.Join(quoted, " ")
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

package search

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"kb/internal/kb"
	"kb/internal/llm"
	"kb/internal/store"
	"kb/internal/storetest"
	"kb/internal/vecsearch"
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
	aligned := seed("aligned", []float32{1, 0})           // cos 1
	partial := seed("partial", []float32{0.7071, 0.7071}) // cos ~0.707
	opposite := seed("opposite", []float32{-1, 0})        // cos -1 (skipped: non-positive)
	_ = opposite

	s := NewSearcher(st, queryEmbedder{vec: []float32{1, 0}}, "m", true)

	results, err := s.Semantic(ctx, "find aligned things", 10)
	if err != nil {
		t.Fatalf("semantic: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (the opposite-meaning artifact is floored out)", len(results))
	}
	if results[0].ArtifactID != aligned || results[1].ArtifactID != partial {
		t.Errorf("ranking = %+v, want [aligned, partial]", results)
	}
	if results[0].Score <= results[1].Score {
		t.Errorf("scores not descending: %v then %v", results[0].Score, results[1].Score)
	}
}

func TestSemantic_RemoteEndpointRefuses(t *testing.T) {
	st := storetest.New(t)
	s := NewSearcher(st, queryEmbedder{vec: []float32{1, 0}}, "m", false) // not local
	if _, err := s.Semantic(context.Background(), "secret query", 5); !errors.Is(err, ErrRemoteDisallowed) {
		t.Errorf("err = %v, want ErrRemoteDisallowed (query must not leave the box)", err)
	}
}

func TestSemantic_SkipsNonFiniteScore(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	// A pre-validation row whose vector contains a NaN: cosine is NaN and must be
	// skipped, not ranked (a NaN would corrupt the sort and the JSON response).
	hash := "nan"
	a, _ := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: kb.Note, BodyMarkdown: hash, ContentHash: &hash})
	payload, _ := json.Marshal(kb.EmbeddingPayload{Model: "m", Dim: 2, Vector: []float32{float32(math.NaN()), 0}})
	_ = st.Enrichments.Create(ctx, &kb.Enrichment{ArtifactID: a.ID, Kind: "embedding", Payload: string(payload), Model: "m", PromptVersion: "v1"})

	s := NewSearcher(st, queryEmbedder{vec: []float32{1, 0}}, "m", true)
	results, err := s.Semantic(ctx, "q", 10)
	if err != nil {
		t.Fatalf("semantic: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %+v, want 0 (the NaN-vector artifact is skipped)", results)
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

	s := NewSearcher(st, queryEmbedder{vec: []float32{1, 0}}, "m", true)
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
	s := NewSearcher(st, queryEmbedder{vec: []float32{1, 0}}, "m", true)
	if _, err := s.Semantic(context.Background(), "   ", 5); err == nil {
		t.Error("an empty query should be an error")
	}
}

func TestSemantic_EmbedErrorPropagates(t *testing.T) {
	st := storetest.New(t)
	s := NewSearcher(st, queryEmbedder{err: errors.New("ollama down")}, "m", true)
	if _, err := s.Semantic(context.Background(), "q", 5); err == nil {
		t.Error("an embed error must propagate")
	}
}

func seedNote(t *testing.T, st *store.Store, hash, title, body string) string {
	t.Helper()
	a, err := st.Artifacts.UpsertByContentHash(context.Background(), &kb.Artifact{Type: kb.Note, Title: title, BodyMarkdown: body, ContentHash: &hash})
	if err != nil {
		t.Fatalf("seed note: %v", err)
	}
	return a.ID
}

func TestKeyword_FindsAndRanks(t *testing.T) {
	st := storetest.New(t)
	rust := seedNote(t, st, "h1", "Rust async", "tokio runtime and tasks")
	seedNote(t, st, "h2", "Go channels", "goroutines and selects")

	// Keyword search needs no embedder and no locality (it never leaves SQLite).
	s := NewSearcher(st, queryEmbedder{err: errors.New("must not be called")}, "m", false)
	results, err := s.Keyword(context.Background(), "rust async", 10)
	if err != nil {
		t.Fatalf("keyword: %v", err)
	}
	if len(results) != 1 || results[0].ArtifactID != rust {
		t.Errorf("results = %+v, want only the rust artifact %s", results, rust)
	}
}

func TestKeyword_SanitizesSpecialChars(t *testing.T) {
	st := storetest.New(t)
	seedNote(t, st, "h1", "Rust async", "tokio")
	s := NewSearcher(st, queryEmbedder{}, "m", true)
	// FTS5 syntax metacharacters in raw input must not produce a query error.
	if _, err := s.Keyword(context.Background(), `rust" OR (NEAR foo`, 10); err != nil {
		t.Errorf("special characters must be sanitized, not error: %v", err)
	}
}

func TestKeyword_NoSearchableTerms(t *testing.T) {
	st := storetest.New(t)
	s := NewSearcher(st, queryEmbedder{}, "m", true)
	results, err := s.Keyword(context.Background(), "!!! ??? ...", 10)
	if err != nil {
		t.Fatalf("keyword: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("a query with no word tokens should return no results, got %+v", results)
	}
}

func TestKeyword_EmptyQueryIsError(t *testing.T) {
	st := storetest.New(t)
	s := NewSearcher(st, queryEmbedder{}, "m", true)
	if _, err := s.Keyword(context.Background(), "  ", 10); err == nil {
		t.Error("an empty query should be an error")
	}
}

func seedNoteWithEmbedding(t *testing.T, st *store.Store, hash, title, body string, vec []float32) string {
	t.Helper()
	id := seedNote(t, st, hash, title, body)
	payload, _ := json.Marshal(kb.EmbeddingPayload{Model: "m", Dim: len(vec), Vector: vec})
	if err := st.Enrichments.Create(context.Background(), &kb.Enrichment{ArtifactID: id, Kind: "embedding", Payload: string(payload), Model: "m", PromptVersion: "v1"}); err != nil {
		t.Fatalf("seed embedding: %v", err)
	}
	return id
}

func TestHybrid_FusesSemanticAndKeyword(t *testing.T) {
	st := storetest.New(t)
	// A: semantic match (embedding aligns with query {1,0}), body lacks the term.
	// B: keyword match (body has "beta"), embedding orthogonal → floored from semantic.
	a := seedNoteWithEmbedding(t, st, "h1", "Alpha", "alpha content", []float32{1, 0})
	b := seedNoteWithEmbedding(t, st, "h2", "Beta", "beta keyword text", []float32{0, 1})

	s := NewSearcher(st, queryEmbedder{vec: []float32{1, 0}}, "m", true)
	results, err := s.Hybrid(context.Background(), "beta", 10)
	if err != nil {
		t.Fatalf("hybrid: %v", err)
	}
	got := map[string]bool{}
	for _, r := range results {
		got[r.ArtifactID] = true
	}
	if !got[a] || !got[b] {
		t.Errorf("hybrid results = %+v, want both the semantic hit %s and the keyword hit %s", results, a, b)
	}
}

func TestHybrid_DegradesToKeywordWhenSemanticUnavailable(t *testing.T) {
	st := storetest.New(t)
	b := seedNoteWithEmbedding(t, st, "h2", "Beta", "beta keyword text", []float32{0, 1})

	// local=false → Semantic refuses; Hybrid must still return keyword results.
	s := NewSearcher(st, queryEmbedder{vec: []float32{1, 0}}, "m", false)
	results, err := s.Hybrid(context.Background(), "beta", 10)
	if err != nil {
		t.Fatalf("hybrid: %v", err)
	}
	if len(results) != 1 || results[0].ArtifactID != b {
		t.Errorf("results = %+v, want the keyword hit %s (semantic degraded)", results, b)
	}
}

func TestHybrid_DegradesWhenEmbedModelDown(t *testing.T) {
	st := storetest.New(t)
	b := seedNote(t, st, "h2", "Beta", "beta keyword text")

	// Local endpoint but the embed model errors (not a context error): degrade to
	// keyword rather than failing the whole search.
	s := NewSearcher(st, queryEmbedder{err: errors.New("model not loaded")}, "m", true)
	results, err := s.Hybrid(context.Background(), "beta", 10)
	if err != nil {
		t.Fatalf("hybrid should degrade on a model error, got %v", err)
	}
	if len(results) != 1 || results[0].ArtifactID != b {
		t.Errorf("results = %+v, want the keyword hit %s", results, b)
	}
}

func TestHybrid_CancelledContextPropagates(t *testing.T) {
	st := storetest.New(t)
	seedNote(t, st, "h2", "Beta", "beta keyword text")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // dead request

	s := NewSearcher(st, queryEmbedder{vec: []float32{1, 0}}, "m", true)
	if _, err := s.Hybrid(ctx, "beta", 10); err == nil {
		t.Error("a cancelled context must propagate as an error, not a degraded 200")
	}
}

type fakeIndex struct {
	hits  []vecsearch.Hit
	err   error
	calls int
}

func (f *fakeIndex) Search(_ context.Context, _ []float32, _ int) ([]vecsearch.Hit, error) {
	f.calls++
	return f.hits, f.err
}
func (f *fakeIndex) Reconcile(_ context.Context, _ vecsearch.EmbeddingSource) (int, error) {
	return 0, nil
}

func TestSemantic_UsesIndexWhenSet(t *testing.T) {
	st := storetest.New(t)
	s := NewSearcher(st, queryEmbedder{vec: []float32{1, 0, 0}}, "m", true)
	idx := &fakeIndex{hits: []vecsearch.Hit{{ArtifactID: "01A", Score: 0.95}, {ArtifactID: "01B", Score: 0.5}}}
	s.SetIndex(idx)

	res, err := s.Semantic(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("semantic: %v", err)
	}
	if idx.calls != 1 {
		t.Errorf("index Search called %d times, want 1 (the ANN path)", idx.calls)
	}
	if len(res) != 2 || res[0].ArtifactID != "01A" || res[0].Score != 0.95 {
		t.Errorf("results = %+v, want the index hits passed through", res)
	}
}

func TestSemantic_FallsBackOnIndexError(t *testing.T) {
	st := storetest.New(t)
	a := seedNoteWithEmbedding(t, st, "h1", "T", "body", []float32{1, 0, 0})
	s := NewSearcher(st, queryEmbedder{vec: []float32{1, 0, 0}}, "m", true)
	s.SetIndex(&fakeIndex{err: errors.New("index down")})

	res, err := s.Semantic(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("an index error must fall back to brute force, not fail: %v", err)
	}
	if len(res) != 1 || res[0].ArtifactID != a {
		t.Errorf("results = %+v, want the brute-force scan to find %s", res, a)
	}
}

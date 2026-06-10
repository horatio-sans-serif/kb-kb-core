package rag

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"kb/internal/kb"
	"kb/internal/llm"
	"kb/internal/search"
	"kb/internal/store"
	"kb/internal/storetest"
)

type fakeEmbedder struct{ vec []float32 }

func (f fakeEmbedder) Embed(_ context.Context, _ llm.EmbedRequest) ([]float32, error) {
	return f.vec, nil
}

func seedEmbedding(t *testing.T, st *store.Store, artifactID string, vec []float32) {
	t.Helper()
	payload, err := json.Marshal(kb.EmbeddingPayload{Model: "embed", Dim: len(vec), Vector: vec})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Enrichments.Create(context.Background(), &kb.Enrichment{
		ArtifactID: artifactID, Kind: "embedding", Payload: string(payload), Model: "embed", PromptVersion: "v1",
	}); err != nil {
		t.Fatalf("seed embedding: %v", err)
	}
}

type fakeRetriever struct {
	hits []search.Result
	err  error
}

func (f fakeRetriever) Hybrid(_ context.Context, _ string, _ int) ([]search.Result, error) {
	return f.hits, f.err
}

type fakeLLM struct {
	resp     string
	err      error
	gotSys   string
	gotPromt string
	calls    int
}

func (f *fakeLLM) Generate(_ context.Context, req llm.GenerateRequest) (string, error) {
	f.calls++
	f.gotSys, f.gotPromt = req.System, req.Prompt
	return f.resp, f.err
}

func seed(t *testing.T, st *store.Store, hash, title, body string) string {
	t.Helper()
	a, err := st.Artifacts.UpsertByContentHash(context.Background(), &kb.Artifact{Type: kb.Note, Title: title, BodyMarkdown: body, ContentHash: &hash})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return a.ID
}

func TestAnswer_SynthesizesWithSources(t *testing.T) {
	st := storetest.New(t)
	a1 := seed(t, st, "h1", "Rust async", "Tokio is an async runtime for Rust.")
	a2 := seed(t, st, "h2", "Go scheduling", "Goroutines are scheduled by the Go runtime.")

	llmc := &fakeLLM{resp: "Tokio powers async Rust [1]."}
	e := NewEngine(fakeRetriever{hits: []search.Result{{ArtifactID: a1, Score: 0.9}, {ArtifactID: a2, Score: 0.4}}},
		st, llmc, nil, "model", "embed", true)

	ans, err := e.Answer(context.Background(), "what is tokio", 5)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if ans.Answer != "Tokio powers async Rust [1]." {
		t.Errorf("answer = %q, want the synthesized text", ans.Answer)
	}
	if len(ans.Sources) != 2 || ans.Sources[0].ArtifactID != a1 || ans.Sources[0].Title != "Rust async" {
		t.Errorf("sources = %+v, want both loaded with metadata, a1 first", ans.Sources)
	}
	// The prompt must contain the numbered, fenced source bodies for grounding.
	if !strings.Contains(llmc.gotPromt, "SOURCE 1") || !strings.Contains(llmc.gotPromt, "Tokio is an async runtime") {
		t.Errorf("prompt missing numbered source content: %q", llmc.gotPromt)
	}
}

func TestAnswer_EmptyModelAnswerIsError(t *testing.T) {
	st := storetest.New(t)
	a1 := seed(t, st, "h1", "T", "some body text")
	e := NewEngine(fakeRetriever{hits: []search.Result{{ArtifactID: a1}}}, st, &fakeLLM{resp: "   "}, nil, "model", "embed", true)
	if _, err := e.Answer(context.Background(), "q", 5); err == nil {
		t.Error("an empty/whitespace model answer must be an error, not a 200 with sources and no text")
	}
}

func TestAnswer_SkipsTombstonedSource(t *testing.T) {
	st := storetest.New(t)
	a1 := seed(t, st, "h1", "Live", "live body")
	gone := seed(t, st, "h2", "Gone", "deleted body")
	if err := st.Artifacts.SoftDelete(context.Background(), gone); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	llmc := &fakeLLM{resp: "answer [1]"}
	// Both ranked (a stale ranking), but the tombstoned one must not reach the prompt.
	e := NewEngine(fakeRetriever{hits: []search.Result{{ArtifactID: a1}, {ArtifactID: gone}}}, st, llmc, nil, "model", "embed", true)
	ans, err := e.Answer(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if len(ans.Sources) != 1 || ans.Sources[0].ArtifactID != a1 {
		t.Errorf("sources = %+v, want only the live artifact %s", ans.Sources, a1)
	}
	if strings.Contains(llmc.gotPromt, "deleted body") {
		t.Errorf("tombstoned body leaked into the prompt: %q", llmc.gotPromt)
	}
}

func TestAnswer_RemoteEndpointRefused(t *testing.T) {
	st := storetest.New(t)
	e := NewEngine(fakeRetriever{}, st, &fakeLLM{}, nil, "model", "embed", false) // not local
	if _, err := e.Answer(context.Background(), "q", 5); !errors.Is(err, ErrLocalRequired) {
		t.Errorf("err = %v, want ErrLocalRequired (bodies + query must stay on-device)", err)
	}
}

func TestAnswer_EmptyQueryIsError(t *testing.T) {
	st := storetest.New(t)
	e := NewEngine(fakeRetriever{}, st, &fakeLLM{}, nil, "model", "embed", true)
	if _, err := e.Answer(context.Background(), "   ", 5); err == nil {
		t.Error("an empty query should be an error")
	}
}

func TestAnswer_NoHitsSkipsLLM(t *testing.T) {
	st := storetest.New(t)
	llmc := &fakeLLM{resp: "should not be called"}
	e := NewEngine(fakeRetriever{hits: nil}, st, llmc, nil, "model", "embed", true)
	ans, err := e.Answer(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if len(ans.Sources) != 0 || llmc.calls != 0 {
		t.Errorf("no hits should skip synthesis, sources=%d calls=%d", len(ans.Sources), llmc.calls)
	}
	if ans.Answer == "" {
		t.Error("expected a 'no sources' answer, not empty")
	}
}

func TestAnswer_LLMErrorPropagates(t *testing.T) {
	st := storetest.New(t)
	a1 := seed(t, st, "h1", "T", "body")
	e := NewEngine(fakeRetriever{hits: []search.Result{{ArtifactID: a1}}}, st, &fakeLLM{err: errors.New("model down")}, nil, "model", "embed", true)
	if _, err := e.Answer(context.Background(), "q", 5); err == nil {
		t.Error("an LLM error must propagate")
	}
}

func TestAnswer_RetrieverErrorPropagates(t *testing.T) {
	st := storetest.New(t)
	e := NewEngine(fakeRetriever{err: errors.New("search down")}, st, &fakeLLM{}, nil, "model", "embed", true)
	if _, err := e.Answer(context.Background(), "q", 5); err == nil {
		t.Error("a retriever error must propagate")
	}
}

func TestAnswer_MMRRerankPrefersDiversity(t *testing.T) {
	st := storetest.New(t)
	// A and B are near-duplicates (same vector); C is relevant on a different axis.
	a := seed(t, st, "ha", "A", "alpha body")
	b := seed(t, st, "hb", "B", "bravo body")
	c := seed(t, st, "hc", "C", "charlie body")
	seedEmbedding(t, st, a, []float32{1, 0, 0})
	seedEmbedding(t, st, b, []float32{1, 0, 0}) // duplicate of A
	seedEmbedding(t, st, c, []float32{0, 1, 0}) // diverse, equally relevant to the query

	llmc := &fakeLLM{resp: "answer [1] [2]"}
	// Retrieval order A, B, C (B ranked above C). Query embedding covers both axes.
	e := NewEngine(
		fakeRetriever{hits: []search.Result{{ArtifactID: a, Score: 0.9}, {ArtifactID: b, Score: 0.8}, {ArtifactID: c, Score: 0.7}}},
		st, llmc, fakeEmbedder{vec: []float32{1, 1, 0}}, "model", "embed", true)

	ans, err := e.Answer(context.Background(), "q", 2)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if len(ans.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(ans.Sources))
	}
	// MMR must drop the near-duplicate B in favor of the diverse C, even though B
	// outranked C in retrieval.
	got := []string{ans.Sources[0].ArtifactID, ans.Sources[1].ArtifactID}
	if got[0] != a || got[1] != c {
		t.Errorf("reranked sources = %v, want [A C] (diversity beats the near-duplicate B)", got)
	}
}

func TestAnswer_NilEmbedderKeepsRetrievalOrder(t *testing.T) {
	st := storetest.New(t)
	a := seed(t, st, "ha", "A", "alpha")
	b := seed(t, st, "hb", "B", "bravo")
	c := seed(t, st, "hc", "C", "charlie")
	llmc := &fakeLLM{resp: "answer [1] [2]"}
	// nil embedder → no rerank → top-k of the retrieval order.
	e := NewEngine(
		fakeRetriever{hits: []search.Result{{ArtifactID: a}, {ArtifactID: b}, {ArtifactID: c}}},
		st, llmc, nil, "model", "embed", true)
	ans, err := e.Answer(context.Background(), "q", 2)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if len(ans.Sources) != 2 || ans.Sources[0].ArtifactID != a || ans.Sources[1].ArtifactID != b {
		t.Errorf("sources = %+v, want the retrieval-order top 2 [A B]", ans.Sources)
	}
}

func TestAnswer_CitationValidation(t *testing.T) {
	st := storetest.New(t)
	a := seed(t, st, "ha", "A", "alpha")
	b := seed(t, st, "hb", "B", "bravo")
	// The answer cites source 1 (valid), source 3 (out of range → hallucinated), and 1 again.
	llmc := &fakeLLM{resp: "Per [1], yes; also [3]; see [1]."}
	e := NewEngine(
		fakeRetriever{hits: []search.Result{{ArtifactID: a}, {ArtifactID: b}}},
		st, llmc, nil, "model", "embed", true)

	ans, err := e.Answer(context.Background(), "q", 2)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if !ans.Sources[0].Cited {
		t.Error("source 1 was cited ([1]) but Cited=false")
	}
	if ans.Sources[1].Cited {
		t.Error("source 2 was never cited but Cited=true")
	}
	if len(ans.InvalidCitations) != 1 || ans.InvalidCitations[0] != 3 {
		t.Errorf("InvalidCitations = %v, want [3] (out-of-range reference)", ans.InvalidCitations)
	}
}

func TestParseCitations(t *testing.T) {
	cited, invalid := parseCitations("uses [1] and [2], plus a bogus [9] and [2] again", 3)
	if !cited[1] || !cited[2] || cited[3] {
		t.Errorf("cited = %v, want {1,2} in-range used", cited)
	}
	if len(invalid) != 1 || invalid[0] != 9 {
		t.Errorf("invalid = %v, want [9]", invalid)
	}
}

func TestParseCitations_CombinedAndRangeFormats(t *testing.T) {
	// Models emit [1,2] (comma) and [1-3] (range), not only [1][2].
	cited, invalid := parseCitations("see [1, 2] and [3-5]; bogus [8]", 5)
	for _, n := range []int{1, 2, 3, 4, 5} {
		if !cited[n] {
			t.Errorf("source %d should be cited (from a combined/range reference)", n)
		}
	}
	if len(invalid) != 1 || invalid[0] != 8 {
		t.Errorf("invalid = %v, want [8]", invalid)
	}

	// A bracket with letters is not a citation; an inverted/absurd range is treated
	// as a single reference, not expanded.
	cited, _ = parseCitations("relation [derived-from] and [2024-2025]", 3)
	if len(cited) != 0 {
		t.Errorf("cited = %v, want none (letters are not citations; the year range is out of range)", cited)
	}
}

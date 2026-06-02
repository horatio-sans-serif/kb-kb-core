package rag

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kb/internal/kb"
	"kb/internal/llm"
	"kb/internal/search"
	"kb/internal/store"
	"kb/internal/storetest"
)

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
		st, llmc, "model", true)

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
	// The prompt must contain the numbered source bodies for grounding.
	if !strings.Contains(llmc.gotPromt, "[1]") || !strings.Contains(llmc.gotPromt, "Tokio is an async runtime") {
		t.Errorf("prompt missing numbered source content: %q", llmc.gotPromt)
	}
}

func TestAnswer_RemoteEndpointRefused(t *testing.T) {
	st := storetest.New(t)
	e := NewEngine(fakeRetriever{}, st, &fakeLLM{}, "model", false) // not local
	if _, err := e.Answer(context.Background(), "q", 5); !errors.Is(err, ErrLocalRequired) {
		t.Errorf("err = %v, want ErrLocalRequired (bodies + query must stay on-device)", err)
	}
}

func TestAnswer_EmptyQueryIsError(t *testing.T) {
	st := storetest.New(t)
	e := NewEngine(fakeRetriever{}, st, &fakeLLM{}, "model", true)
	if _, err := e.Answer(context.Background(), "   ", 5); err == nil {
		t.Error("an empty query should be an error")
	}
}

func TestAnswer_NoHitsSkipsLLM(t *testing.T) {
	st := storetest.New(t)
	llmc := &fakeLLM{resp: "should not be called"}
	e := NewEngine(fakeRetriever{hits: nil}, st, llmc, "model", true)
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
	e := NewEngine(fakeRetriever{hits: []search.Result{{ArtifactID: a1}}}, st, &fakeLLM{err: errors.New("model down")}, "model", true)
	if _, err := e.Answer(context.Background(), "q", 5); err == nil {
		t.Error("an LLM error must propagate")
	}
}

func TestAnswer_RetrieverErrorPropagates(t *testing.T) {
	st := storetest.New(t)
	e := NewEngine(fakeRetriever{err: errors.New("search down")}, st, &fakeLLM{}, "model", true)
	if _, err := e.Answer(context.Background(), "q", 5); err == nil {
		t.Error("a retriever error must propagate")
	}
}

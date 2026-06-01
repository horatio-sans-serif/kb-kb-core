package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"kb/internal/kb"
	"kb/internal/llm"
)

type fakeLLM struct {
	resp   string
	err    error
	gotReq llm.GenerateRequest
	calls  int
}

func (f *fakeLLM) Generate(_ context.Context, req llm.GenerateRequest) (string, error) {
	f.calls++
	f.gotReq = req
	return f.resp, f.err
}

const goodSummaryJSON = `{
  "summary": "A concise summary.",
  "tags": ["Go", "go", " SQLite ", "", "databases"],
  "critical_analysis": "Assumes single-writer; ignores concurrency."
}`

func TestSummarize_EmitsThreeRows(t *testing.T) {
	f := &fakeLLM{resp: goodSummaryJSON}
	s := NewSummarize(f, "test-model")

	res, err := s.Enrich(context.Background(), &kb.Artifact{ID: "01ART", Title: "T", BodyMarkdown: "Some body text."})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(res.Enrichments) != 3 {
		t.Fatalf("rows = %d, want 3 (summary, tags, critical_analysis)", len(res.Enrichments))
	}

	byKind := map[string]kb.Enrichment{}
	for _, e := range res.Enrichments {
		byKind[e.Kind] = e
		if e.ArtifactID != "01ART" || e.Model != "test-model" || e.PromptVersion == "" {
			t.Errorf("row %q has wrong provenance: %+v", e.Kind, e)
		}
	}
	for _, want := range []string{"summary", "tags", "critical_analysis"} {
		if _, ok := byKind[want]; !ok {
			t.Errorf("missing %q row", want)
		}
	}

	// Tags normalized: lowercased, trimmed, de-duplicated, empties dropped.
	var tagPayload struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(byKind["tags"].Payload), &tagPayload); err != nil {
		t.Fatalf("tags payload not JSON: %v", err)
	}
	got := strings.Join(tagPayload.Tags, ",")
	if got != "go,sqlite,databases" {
		t.Errorf("normalized tags = %q, want %q", got, "go,sqlite,databases")
	}

	// Structured output requested.
	if f.gotReq.Format == nil {
		t.Error("expected a structured-output Format to be sent to the model")
	}
}

func TestSummarize_EmptyBodyIsNoop(t *testing.T) {
	f := &fakeLLM{resp: goodSummaryJSON}
	s := NewSummarize(f, "m")
	res, err := s.Enrich(context.Background(), &kb.Artifact{ID: "01ART", BodyMarkdown: "   "})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(res.Enrichments) != 0 || f.calls != 0 {
		t.Errorf("empty body should be a no-op with no model call, got rows=%d calls=%d", len(res.Enrichments), f.calls)
	}
}

func TestSummarize_ModelErrorPropagates(t *testing.T) {
	f := &fakeLLM{err: errors.New("ollama down")}
	s := NewSummarize(f, "m")
	if _, err := s.Enrich(context.Background(), &kb.Artifact{ID: "01ART", BodyMarkdown: "text"}); err == nil {
		t.Error("a model error must propagate so the stage retries/fails")
	}
}

func TestSummarize_BadJSONIsError(t *testing.T) {
	f := &fakeLLM{resp: "not json at all"}
	s := NewSummarize(f, "m")
	if _, err := s.Enrich(context.Background(), &kb.Artifact{ID: "01ART", BodyMarkdown: "text"}); err == nil {
		t.Error("unparseable model output must be an error")
	}
}

func TestSummarize_EmptySummaryIsError(t *testing.T) {
	f := &fakeLLM{resp: `{"summary":"  ","tags":["x"],"critical_analysis":"y"}`}
	s := NewSummarize(f, "m")
	if _, err := s.Enrich(context.Background(), &kb.Artifact{ID: "01ART", BodyMarkdown: "text"}); err == nil {
		t.Error("an empty summary must be an error (the model failed its task)")
	}
}

func TestSummarize_Contract(t *testing.T) {
	s := NewSummarize(&fakeLLM{}, "m")
	if s.Name() != "summarize" {
		t.Errorf("name = %q, want summarize", s.Name())
	}
	if len(s.DependsOn()) != 1 || s.DependsOn()[0] != "fetch" {
		t.Errorf("DependsOn = %v, want [fetch]", s.DependsOn())
	}
	for _, tier := range []kb.PrivacyTier{kb.Public, kb.Personal, kb.Sensitive, kb.Restricted} {
		if !s.PrivacyOK(tier) {
			t.Errorf("PrivacyOK(%s) = false, want true (local Ollama processes every tier)", tier)
		}
	}
}

package topics

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kb/internal/kb"
	"kb/internal/llm"
	"kb/internal/store"
	"kb/internal/storetest"
)

// fakeLLM returns a canned response (or error) for Generate.
type fakeLLM struct {
	resp   string
	err    error
	called bool
}

func (f *fakeLLM) Generate(_ context.Context, _ llm.GenerateRequest) (string, error) {
	f.called = true
	if f.err != nil {
		return "", f.err
	}
	return f.resp, nil
}

// foundTopic founds a topic from one seeded artifact via a sweep and returns its id.
func foundTopic(t *testing.T, st *store.Store, hash, title string, tier kb.PrivacyTier) string {
	t.Helper()
	ctx := context.Background()
	a, err := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{
		Type: "note", Title: title, BodyMarkdown: "b", ContentHash: &hash, PrivacyTier: tier,
	})
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	ready := kb.StatusReady
	_ = st.Artifacts.Patch(ctx, a.ID, kb.ArtifactPatch{Status: &ready})
	payload := `{"model":"m","dim":2,"vector":[1,0]}`
	_ = st.Enrichments.Create(ctx, &kb.Enrichment{ArtifactID: a.ID, Kind: "embedding", Payload: payload})
	if _, err := New(st).Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	tps, _ := st.Topics.TopicsForArtifact(ctx, a.ID)
	if len(tps) != 1 {
		t.Fatalf("want 1 topic for founded artifact, got %d", len(tps))
	}
	return tps[0].ID
}

func topicLabel(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	tp, err := st.Topics.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get topic: %v", err)
	}
	return tp.Label
}

func TestLabeler_UsesLLMLabel(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	id := foundTopic(t, st, "h1", "A long original founding artifact title", kb.Public)
	want := "Distributed Systems Consensus"
	lab := NewLabeler(st, &fakeLLM{resp: want}, "m", true)
	if err := lab.Label(ctx, id, "fallback"); err != nil {
		t.Fatalf("label: %v", err)
	}
	if got := topicLabel(t, st, id); got != want {
		t.Errorf("label = %q, want %q", got, want)
	}
}

func TestLabeler_SanitizesAndClamps(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	id := foundTopic(t, st, "h1", "orig", kb.Public)
	// Quoted, multi-line, over-length model output.
	long := strings.Repeat("x", maxLabelLen+50)
	raw := "  \"First line label\nSECOND LINE " + long + "\"  "
	lab := NewLabeler(st, &fakeLLM{resp: raw}, "m", true)
	if err := lab.Label(ctx, id, "fallback"); err != nil {
		t.Fatalf("label: %v", err)
	}
	got := topicLabel(t, st, id)
	if len(got) > maxLabelLen {
		t.Errorf("label %d bytes, want <= %d", len(got), maxLabelLen)
	}
	if strings.ContainsAny(got, "\n\"") {
		t.Errorf("label %q should have no quotes/newlines", got)
	}
	if !strings.HasPrefix(got, "First line label") {
		t.Errorf("label %q should be the first sanitized line", got)
	}
}

func TestLabeler_NoLLMKeepsFallback(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	id := foundTopic(t, st, "h1", "orig", kb.Public)
	before := topicLabel(t, st, id)
	lab := NewLabeler(st, nil, "m", true) // no LLM configured
	if err := lab.Label(ctx, id, before); err != nil {
		t.Fatalf("label: %v", err)
	}
	if got := topicLabel(t, st, id); got != before {
		t.Errorf("label = %q, want unchanged %q (no LLM)", got, before)
	}
}

func TestLabeler_LLMErrorKeepsFallback(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	id := foundTopic(t, st, "h1", "orig", kb.Public)
	before := topicLabel(t, st, id)
	lab := NewLabeler(st, &fakeLLM{err: errors.New("boom")}, "m", true)
	if err := lab.Label(ctx, id, before); err != nil {
		t.Fatalf("label: %v", err) // best-effort: error swallowed, fallback kept
	}
	if got := topicLabel(t, st, id); got != before {
		t.Errorf("label = %q, want unchanged %q (LLM error)", got, before)
	}
}

func TestLabeler_NonLocalSkips(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	id := foundTopic(t, st, "h1", "orig", kb.Public)
	before := topicLabel(t, st, id)
	f := &fakeLLM{resp: "Should Not Be Used"}
	lab := NewLabeler(st, f, "m", false) // non-local model: must not send member text
	if err := lab.Label(ctx, id, before); err != nil {
		t.Fatalf("label: %v", err)
	}
	if f.called {
		t.Error("non-local model must not be called with member text")
	}
	if got := topicLabel(t, st, id); got != before {
		t.Errorf("label = %q, want unchanged %q (non-local skip)", got, before)
	}
}

func TestLabeler_EmptyOutputKeepsFallback(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	id := foundTopic(t, st, "h1", "orig", kb.Public)
	before := topicLabel(t, st, id)
	lab := NewLabeler(st, &fakeLLM{resp: "  \"\" \n "}, "m", true)
	if err := lab.Label(ctx, id, before); err != nil {
		t.Fatalf("label: %v", err)
	}
	if got := topicLabel(t, st, id); got != before {
		t.Errorf("label = %q, want unchanged %q (empty sanitized output)", got, before)
	}
}

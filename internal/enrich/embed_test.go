package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"kb/internal/kb"
	"kb/internal/llm"
)

type fakeEmbedder struct {
	vec    []float32
	err    error
	calls  int
	gotReq llm.EmbedRequest
}

func (f *fakeEmbedder) Embed(_ context.Context, req llm.EmbedRequest) ([]float32, error) {
	f.calls++
	f.gotReq = req
	return f.vec, f.err
}

func TestEmbed_WritesEmbeddingRow(t *testing.T) {
	f := &fakeEmbedder{vec: []float32{0.1, -0.2, 0.3}}
	e := NewEmbed(f, "embed-model", true)

	res, err := e.Enrich(context.Background(), &kb.Artifact{ID: "01ART", BodyMarkdown: "Some text to embed."})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(res.Enrichments) != 1 || res.Enrichments[0].Kind != "embedding" {
		t.Fatalf("enrichments = %+v, want one 'embedding' row", res.Enrichments)
	}
	row := res.Enrichments[0]
	if row.ArtifactID != "01ART" || row.Model != "embed-model" || row.PromptVersion == "" {
		t.Errorf("row provenance wrong: %+v", row)
	}
	if res.Patch != nil {
		t.Error("embed must not patch the artifact")
	}

	var payload struct {
		Model  string    `json:"model"`
		Dim    int       `json:"dim"`
		Vector []float32 `json:"vector"`
	}
	if err := json.Unmarshal([]byte(row.Payload), &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if payload.Model != "embed-model" || payload.Dim != 3 || len(payload.Vector) != 3 {
		t.Errorf("payload = %+v, want model/dim=3/3-vector", payload)
	}
	if payload.Vector[0] != 0.1 || payload.Vector[1] != -0.2 || payload.Vector[2] != 0.3 {
		t.Errorf("vector = %v, want [0.1 -0.2 0.3]", payload.Vector)
	}
}

func TestEmbed_EmptyBodyIsNoop(t *testing.T) {
	f := &fakeEmbedder{vec: []float32{0.1}}
	e := NewEmbed(f, "m", true)
	res, err := e.Enrich(context.Background(), &kb.Artifact{ID: "01ART", BodyMarkdown: "  "})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(res.Enrichments) != 0 || f.calls != 0 {
		t.Errorf("empty body should be a no-op with no embed call, rows=%d calls=%d", len(res.Enrichments), f.calls)
	}
}

func TestEmbed_ErrorPropagates(t *testing.T) {
	f := &fakeEmbedder{err: errors.New("embed model missing")}
	e := NewEmbed(f, "m", true)
	if _, err := e.Enrich(context.Background(), &kb.Artifact{ID: "01ART", BodyMarkdown: "text"}); err == nil {
		t.Error("an embedder error must propagate so the stage retries/fails")
	}
}

func TestEmbed_EmptyVectorIsError(t *testing.T) {
	f := &fakeEmbedder{vec: []float32{}}
	e := NewEmbed(f, "m", true)
	if _, err := e.Enrich(context.Background(), &kb.Artifact{ID: "01ART", BodyMarkdown: "text"}); err == nil {
		t.Error("an empty vector must be an error")
	}
}

func TestEmbed_NonFiniteVectorIsError(t *testing.T) {
	for _, bad := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		f := &fakeEmbedder{vec: []float32{0.1, bad, 0.3}}
		e := NewEmbed(f, "m", true)
		_, err := e.Enrich(context.Background(), &kb.Artifact{ID: "01ART", BodyMarkdown: "text"})
		if err == nil {
			t.Errorf("a non-finite component (%v) must be a stage error, not a silently corrupt row", bad)
		}
	}
}

func TestEmbed_Contract(t *testing.T) {
	local := NewEmbed(&fakeEmbedder{}, "m", true)
	if local.Name() != "embed" {
		t.Errorf("name = %q, want embed", local.Name())
	}
	if len(local.DependsOn()) != 1 || local.DependsOn()[0] != "fetch" {
		t.Errorf("DependsOn = %v, want [fetch]", local.DependsOn())
	}
	for _, tier := range []kb.PrivacyTier{kb.Public, kb.Personal, kb.Sensitive, kb.Restricted} {
		if !local.PrivacyOK(tier) {
			t.Errorf("local PrivacyOK(%s) = false, want true", tier)
		}
	}
	remote := NewEmbed(&fakeEmbedder{}, "m", false)
	if remote.PrivacyOK(kb.Sensitive) || remote.PrivacyOK(kb.Restricted) {
		t.Error("remote embed endpoint must refuse sensitive/restricted (privacy floor)")
	}
	if !remote.PrivacyOK(kb.Public) || !remote.PrivacyOK(kb.Personal) {
		t.Error("remote embed endpoint should still process public/personal")
	}
}

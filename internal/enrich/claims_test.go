package enrich

import (
	"context"
	"strings"
	"testing"

	"kb/internal/kb"
)

func TestClaims_Contract(t *testing.T) {
	c := NewClaims(&fakeLLM{}, &fakeEmbedder{}, "gen", "emb", true)
	if c.Name() != "claims" {
		t.Errorf("Name = %q", c.Name())
	}
	if len(c.DependsOn()) != 1 || c.DependsOn()[0] != "fetch" {
		t.Errorf("DependsOn = %v, want [fetch]", c.DependsOn())
	}
	if !c.PrivacyOK(kb.Sensitive) {
		t.Error("local claims should allow sensitive")
	}
	if NewClaims(&fakeLLM{}, &fakeEmbedder{}, "g", "e", false).PrivacyOK(kb.Sensitive) {
		t.Error("remote claims must gate sensitive")
	}
}

func TestClaims_ExtractsAndEmbeds(t *testing.T) {
	long := strings.Repeat("x", 600)
	resp := `{"claims":["The sky is blue.","","` + long + `","Water is wet.","The sky is blue."]}`
	gen := &fakeLLM{resp: resp}
	emb := &fakeEmbedder{vec: []float32{0.1, 0.2}}
	c := NewClaims(gen, emb, "gen", "emb", true)

	res, err := c.Enrich(context.Background(), &kb.Artifact{ID: "01ART", Title: "T", BodyMarkdown: "Some text."})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	// Two valid, de-duplicated claims (blank, over-long, and the duplicate dropped).
	if len(res.Claims) != 2 {
		t.Fatalf("claims = %d, want 2 (blank/over-long/dupe dropped): %+v", len(res.Claims), res.Claims)
	}
	for _, cl := range res.Claims {
		if cl.ArtifactID != "01ART" || cl.Statement == "" || len(cl.Embedding) != 2 {
			t.Errorf("claim malformed: %+v", cl)
		}
	}
	// One enrichment row records the extraction.
	if len(res.Enrichments) != 1 || res.Enrichments[0].Kind != "claims" {
		t.Errorf("want one 'claims' enrichment row, got %+v", res.Enrichments)
	}
	// Each kept claim was embedded.
	if emb.calls != 2 {
		t.Errorf("embed calls = %d, want 2", emb.calls)
	}
}

func TestClaims_EmptyBodyNoOp(t *testing.T) {
	c := NewClaims(&fakeLLM{resp: `{"claims":["x"]}`}, &fakeEmbedder{vec: []float32{1}}, "g", "e", true)
	res, err := c.Enrich(context.Background(), &kb.Artifact{ID: "01X", BodyMarkdown: "  "})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(res.Claims) != 0 || len(res.Enrichments) != 0 {
		t.Errorf("empty body should produce nothing, got %+v", res)
	}
}

package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kb/internal/kb"
	"kb/internal/llm"
	"kb/internal/textutil"
)

const (
	claimsPromptVersion = "v1"
	// maxClaimsInputBytes caps the body sent to the model (~6k tokens).
	maxClaimsInputBytes = 24000
	maxClaims           = 16  // also bounds the per-artifact embed calls
	maxClaimLen         = 400 // a claim is one sentence; reject paragraphs
)

var claimsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "claims": {"type": "array", "items": {"type": "string"}}
  },
  "required": ["claims"]
}`)

type claimsOutput struct {
	Claims []string `json:"claims"`
}

// Claims is the claim-extraction stage (design §8.13): one local-model call
// extracts the artifact's discrete factual assertions, each of which is embedded
// (a separate embed call) so a later detector can vector-prefilter the
// contradiction-comparison set. The claims are returned as data and inserted in
// the pipeline's effect transaction (exactly-once). local governs the privacy
// gate exactly as the summarize stage; both the generate and embed calls hit the
// same endpoint, so one flag covers both.
type Claims struct {
	gen        llm.Client
	embedder   llm.Embedder
	genModel   string
	embedModel string
	local      bool
}

// NewClaims wraps a generator and an embedder as the "claims" stage.
func NewClaims(gen llm.Client, embedder llm.Embedder, genModel, embedModel string, local bool) *Claims {
	return &Claims{gen: gen, embedder: embedder, genModel: genModel, embedModel: embedModel, local: local}
}

func (*Claims) Name() string        { return "claims" }
func (*Claims) DependsOn() []string { return []string{"fetch"} }

func (c *Claims) PrivacyOK(tier kb.PrivacyTier) bool {
	if c.local {
		return true
	}
	return tier.AllowsCloud()
}

func (c *Claims) Enrich(ctx context.Context, a *kb.Artifact) (kb.EnrichResult, error) {
	body := strings.TrimSpace(a.BodyMarkdown)
	if body == "" {
		return kb.EnrichResult{}, nil
	}
	statements, err := c.extract(ctx, a.Title, textutil.TruncateBytes(body, maxClaimsInputBytes))
	if err != nil {
		return kb.EnrichResult{}, err
	}

	seen := make(map[string]bool, len(statements))
	claims := make([]kb.Claim, 0, len(statements))
	kept := make([]string, 0, len(statements))
	for _, s := range statements {
		s = strings.TrimSpace(s)
		if s == "" || len(s) > maxClaimLen || seen[s] {
			continue
		}
		seen[s] = true
		vec, err := c.embedder.Embed(ctx, llm.EmbedRequest{Model: c.embedModel, Input: s})
		if err != nil {
			return kb.EnrichResult{}, fmt.Errorf("claims: embed %q: %w", s, err)
		}
		claims = append(claims, kb.Claim{ArtifactID: a.ID, Statement: s, Embedding: vec})
		kept = append(kept, s)
		if len(claims) >= maxClaims {
			break
		}
	}
	if len(claims) == 0 {
		return kb.EnrichResult{}, nil
	}

	return kb.EnrichResult{
		Claims: claims,
		Enrichments: []kb.Enrichment{{
			ArtifactID:    a.ID,
			Kind:          "claims",
			Payload:       jsonString(claimsOutput{Claims: kept}),
			Model:         c.genModel,
			PromptVersion: claimsPromptVersion,
		}},
	}, nil
}

func (c *Claims) extract(ctx context.Context, title, body string) ([]string, error) {
	system := "You extract discrete, checkable factual claims from documents. Return STRICT JSON. " +
		"Each claim is one self-contained declarative sentence; do not include opinions, questions, " +
		"or the document's structure. Do not invent claims not supported by the text."
	prompt := fmt.Sprintf("Title: %s\n\nDocument:\n%s\n\nReturn JSON with a 'claims' array of "+
		"short factual statements.", title, body)

	raw, err := c.gen.Generate(ctx, llm.GenerateRequest{
		Model:  c.genModel,
		System: system,
		Prompt: prompt,
		Format: claimsSchema,
	})
	if err != nil {
		return nil, err
	}
	var out claimsOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("claims: parse model JSON: %w", err)
	}
	return out.Claims, nil
}

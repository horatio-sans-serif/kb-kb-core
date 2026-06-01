package enrich

import (
	"context"
	"fmt"
	"strings"

	"kb/internal/kb"
	"kb/internal/llm"
)

const (
	embedPromptVersion = "v1"
	// maxEmbedInputBytes caps the text sent to the embed model. Embedding models
	// have a smaller context than generators; the model truncates anyway, so this
	// bounds the request size. Trimmed on a rune boundary (see truncateBytes).
	maxEmbedInputBytes = 16000
)

// Embed is the artifact-level embedding stage (design §6.5): it embeds the
// artifact body with the local embed model and writes one `embedding` enrichment
// row. It does not patch the artifact.
//
// local mirrors Summarize: with an on-device endpoint every tier is embedded;
// with a remote endpoint (operator opt-in) sensitive/restricted artifacts are
// gated off (§8.17), so the privacy guarantee holds by construction.
type Embed struct {
	embedder llm.Embedder
	model    string
	local    bool
}

// NewEmbed wraps an embedder as the "embed" stage. local must reflect whether the
// embedder's endpoint is on-device (see config.Config.OllamaIsLocal).
func NewEmbed(embedder llm.Embedder, model string, local bool) *Embed {
	return &Embed{embedder: embedder, model: model, local: local}
}

func (*Embed) Name() string        { return "embed" }
func (*Embed) DependsOn() []string { return []string{"fetch"} }

func (e *Embed) PrivacyOK(tier kb.PrivacyTier) bool {
	if e.local {
		return true
	}
	return tier.AllowsCloud()
}

func (e *Embed) Enrich(ctx context.Context, a *kb.Artifact) (kb.EnrichResult, error) {
	body := strings.TrimSpace(a.BodyMarkdown)
	if body == "" {
		return kb.EnrichResult{}, nil // nothing to embed yet
	}
	body = truncateBytes(body, maxEmbedInputBytes)

	vec, err := e.embedder.Embed(ctx, llm.EmbedRequest{Model: e.model, Input: body})
	if err != nil {
		return kb.EnrichResult{}, err
	}
	if len(vec) == 0 {
		return kb.EnrichResult{}, fmt.Errorf("embed: model returned an empty vector")
	}

	payload := jsonString(kb.EmbeddingPayload{Model: e.model, Dim: len(vec), Vector: vec})
	return kb.EnrichResult{Enrichments: []kb.Enrichment{{
		ArtifactID:    a.ID,
		Kind:          "embedding",
		Payload:       payload,
		Model:         e.model,
		PromptVersion: embedPromptVersion,
	}}}, nil
}

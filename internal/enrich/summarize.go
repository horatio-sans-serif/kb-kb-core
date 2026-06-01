package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kb/internal/kb"
	"kb/internal/llm"
)

const (
	summarizePromptVersion = "v1"
	// maxSummarizeInputChars caps the body sent to the local model (~6k tokens),
	// so a large artifact does not blow the context window or stall generation.
	maxSummarizeInputChars = 24000
	maxTags                = 12
)

// summarizeSchema is the Ollama structured-output schema: the model must return
// exactly these fields, which makes parsing reliable instead of best-effort.
var summarizeSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string"},
    "tags": {"type": "array", "items": {"type": "string"}},
    "critical_analysis": {"type": "string"}
  },
  "required": ["summary", "tags", "critical_analysis"]
}`)

// summaryOutput is the schema-validated shape returned by the model.
type summaryOutput struct {
	Summary          string   `json:"summary"`
	Tags             []string `json:"tags"`
	CriticalAnalysis string   `json:"critical_analysis"`
}

// Summarize is the structured-LLM enrichment stage (design §8.5, the SUMMARIZE
// stage): one local-model call over the artifact body yields a summary, topical
// tags, and the mandatory critical analysis (weaknesses, hidden assumptions,
// missing context — §6.4, which exists to prevent uncritical hoarding). The stage
// writes three enrichment rows from that single call.
//
// PrivacyOK is always true: Ollama runs on-device, so even sensitive/restricted
// artifacts may be summarized locally. The privacy gate exists to keep a *cloud*
// enricher off those tiers (§8.17); a cloud summarizer would be a separate stage.
type Summarize struct {
	client llm.Client
	model  string
}

// NewSummarize wraps a local LLM client as the "summarize" stage.
func NewSummarize(client llm.Client, model string) *Summarize {
	return &Summarize{client: client, model: model}
}

func (*Summarize) Name() string                  { return "summarize" }
func (*Summarize) DependsOn() []string           { return []string{"fetch"} }
func (*Summarize) PrivacyOK(kb.PrivacyTier) bool { return true }

func (s *Summarize) Enrich(ctx context.Context, a *kb.Artifact) (kb.EnrichResult, error) {
	body := strings.TrimSpace(a.BodyMarkdown)
	if body == "" {
		return kb.EnrichResult{}, nil // nothing to summarize yet (e.g. fetch produced no body)
	}
	if len(body) > maxSummarizeInputChars {
		body = body[:maxSummarizeInputChars]
	}

	out, err := s.generate(ctx, a.Title, body)
	if err != nil {
		return kb.EnrichResult{}, err
	}

	return kb.EnrichResult{Enrichments: []kb.Enrichment{
		s.row(a.ID, "summary", jsonString(map[string]string{"text": out.Summary})),
		s.row(a.ID, "tags", jsonString(map[string][]string{"tags": normalizeTags(out.Tags)})),
		s.row(a.ID, "critical_analysis", jsonString(map[string]string{"text": out.CriticalAnalysis})),
	}}, nil
}

func (s *Summarize) generate(ctx context.Context, title, body string) (summaryOutput, error) {
	system := "You are a precise analyst. Read the document and return STRICT JSON matching " +
		"the requested schema. Be concise and factual; do not invent content."
	prompt := fmt.Sprintf("Title: %s\n\nDocument:\n%s\n\n"+
		"Return JSON with: summary (3-5 sentences), tags (3-8 short lowercase topic tags), "+
		"critical_analysis (weaknesses, hidden assumptions, and missing context).", title, body)

	raw, err := s.client.Generate(ctx, llm.GenerateRequest{
		Model:  s.model,
		System: system,
		Prompt: prompt,
		Format: summarizeSchema,
	})
	if err != nil {
		return summaryOutput{}, err
	}

	var out summaryOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return summaryOutput{}, fmt.Errorf("summarize: parse model JSON: %w", err)
	}
	if strings.TrimSpace(out.Summary) == "" {
		return summaryOutput{}, fmt.Errorf("summarize: model returned an empty summary")
	}
	return out, nil
}

func (s *Summarize) row(artifactID, kind, payload string) kb.Enrichment {
	return kb.Enrichment{
		ArtifactID:    artifactID,
		Kind:          kind,
		Payload:       payload,
		Model:         s.model,
		PromptVersion: summarizePromptVersion,
	}
}

// normalizeTags lowercases, trims, de-duplicates, drops empties, and caps the
// list so a chatty model cannot flood the tag set.
func normalizeTags(tags []string) []string {
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) >= maxTags {
			break
		}
	}
	return out
}

// jsonString marshals v to a JSON string; the simple maps used here cannot fail
// to marshal, so an error falls back to an empty object.
func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

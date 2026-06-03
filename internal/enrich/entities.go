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
	entitiesPromptVersion = "v1"
	// maxEntitiesInputBytes caps the body sent to the model (~6k tokens).
	maxEntitiesInputBytes = 24000
	maxEntities           = 32  // a chatty model can't flood the graph
	maxEntityNameLen      = 120 // an entity name is short; reject sentences
	maxAliases            = 8
)

// GraphWriter is the slice of the graph store the entities stage needs. The
// store's *GraphRepo satisfies it; the narrow interface keeps the enricher
// testable and decoupled.
type GraphWriter interface {
	UpsertEntity(ctx context.Context, e *kb.Entity) (string, error)
	LinkArtifactEntity(ctx context.Context, artifactID, entityID string) error
	ArtifactsForEntity(ctx context.Context, entityID string) ([]string, error)
	AddLink(ctx context.Context, src, dst, relation string, weight float64) error
}

var entitiesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "entities": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "name": {"type": "string"},
          "kind": {"type": "string", "enum": ["person","org","technology","concept","place"]},
          "aliases": {"type": "array", "items": {"type": "string"}}
        },
        "required": ["name", "kind"]
      }
    }
  },
  "required": ["entities"]
}`)

type extractedEntity struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Aliases []string `json:"aliases,omitempty"`
}

type entitiesOutput struct {
	Entities []extractedEntity `json:"entities"`
}

// Entities is the named-entity-extraction stage (design §8.7): one local-model
// call over the artifact body extracts people, orgs, technologies, concepts, and
// places, which are deduped into the entity graph and linked to the artifact.
// Artifacts sharing an entity gain a shares_entity edge — the graph builds itself
// during enrichment.
//
// The graph writes are idempotent (entity upsert by (kind,name); INSERT OR IGNORE
// links/edges), so this stage is safe to re-run: the pipeline records completion
// with an "entities" enrichment row, and a crash that re-runs Enrich re-derives
// the same graph. local governs the privacy gate exactly as the summarize stage.
type Entities struct {
	client llm.Client
	model  string
	local  bool
	graph  GraphWriter
}

// NewEntities wraps an LLM client and a graph writer as the "entities" stage.
func NewEntities(client llm.Client, model string, local bool, graph GraphWriter) *Entities {
	return &Entities{client: client, model: model, local: local, graph: graph}
}

func (*Entities) Name() string        { return "entities" }
func (*Entities) DependsOn() []string { return []string{"fetch"} }

func (e *Entities) PrivacyOK(tier kb.PrivacyTier) bool {
	if e.local {
		return true
	}
	return tier.AllowsCloud()
}

func (e *Entities) Enrich(ctx context.Context, a *kb.Artifact) (kb.EnrichResult, error) {
	body := strings.TrimSpace(a.BodyMarkdown)
	if body == "" {
		return kb.EnrichResult{}, nil
	}
	out, err := e.extract(ctx, a.Title, textutil.TruncateBytes(body, maxEntitiesInputBytes))
	if err != nil {
		return kb.EnrichResult{}, err
	}

	kept := make([]extractedEntity, 0, len(out.Entities))
	for _, ex := range out.Entities {
		ex.Name = strings.TrimSpace(ex.Name)
		if ex.Name == "" || len(ex.Name) > maxEntityNameLen || !kb.ValidEntityKind(ex.Kind) {
			continue // drop blank names, over-long names, out-of-vocabulary kinds
		}
		if err := e.materialize(ctx, a.ID, ex); err != nil {
			return kb.EnrichResult{}, err
		}
		kept = append(kept, extractedEntity{Name: ex.Name, Kind: ex.Kind})
		if len(kept) >= maxEntities {
			break
		}
	}

	return kb.EnrichResult{Enrichments: []kb.Enrichment{{
		ArtifactID:    a.ID,
		Kind:          "entities",
		Payload:       jsonString(entitiesOutput{Entities: kept}),
		Model:         e.model,
		PromptVersion: entitiesPromptVersion,
	}}}, nil
}

// materialize upserts one entity, links it to the artifact, and connects the
// artifact to every other live artifact that already mentions the entity via a
// canonical (deduped) shares_entity edge.
func (e *Entities) materialize(ctx context.Context, artifactID string, ex extractedEntity) error {
	entityID, err := e.graph.UpsertEntity(ctx, &kb.Entity{Kind: ex.Kind, Name: ex.Name, Aliases: normalizeAliases(ex.Aliases)})
	if err != nil {
		return err
	}
	// Co-mentioning artifacts BEFORE we add this one, so we don't self-edge.
	others, err := e.graph.ArtifactsForEntity(ctx, entityID)
	if err != nil {
		return err
	}
	if err := e.graph.LinkArtifactEntity(ctx, artifactID, entityID); err != nil {
		return err
	}
	for _, other := range others {
		if other == artifactID {
			continue
		}
		src, dst := canonicalPair(artifactID, other)
		if err := e.graph.AddLink(ctx, src, dst, "shares_entity", 1.0); err != nil {
			return err
		}
	}
	return nil
}

func (e *Entities) extract(ctx context.Context, title, body string) (entitiesOutput, error) {
	system := "You extract named entities from documents. Return STRICT JSON matching the schema. " +
		"Only include entities actually present; do not invent. Use the canonical name and the most " +
		"specific applicable kind."
	prompt := fmt.Sprintf("Title: %s\n\nDocument:\n%s\n\nReturn JSON with an 'entities' array of "+
		"{name, kind (person|org|technology|concept|place), aliases?}.", title, body)

	raw, err := e.client.Generate(ctx, llm.GenerateRequest{
		Model:  e.model,
		System: system,
		Prompt: prompt,
		Format: entitiesSchema,
	})
	if err != nil {
		return entitiesOutput{}, err
	}
	var out entitiesOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return entitiesOutput{}, fmt.Errorf("entities: parse model JSON: %w", err)
	}
	return out, nil
}

// canonicalPair orders two ids so the same unordered pair always yields the same
// directed edge, letting AddLink's (src,dst,relation) dedup work no matter which
// artifact's enrichment discovers the shared entity.
func canonicalPair(a, b string) (string, string) {
	if a <= b {
		return a, b
	}
	return b, a
}

// normalizeAliases trims, de-duplicates, drops empties/over-long, and caps aliases.
func normalizeAliases(aliases []string) []string {
	seen := make(map[string]bool, len(aliases))
	out := make([]string, 0, len(aliases))
	for _, al := range aliases {
		al = strings.TrimSpace(al)
		if al == "" || len(al) > maxEntityNameLen || seen[al] {
			continue
		}
		seen[al] = true
		out = append(out, al)
		if len(out) >= maxAliases {
			break
		}
	}
	return out
}

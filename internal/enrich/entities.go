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
	maxEntityNameLen      = 120 // an entity name is short (bytes); reject sentences
	maxAliases            = 8
	// maxSharesEntityFanout caps how many co-mentioning artifacts an entity may
	// connect to. Past it the entity is a hub/stop-word ("AI", "Go") that carries
	// no discriminating signal, so creating O(N) pairwise edges per new mention
	// would only add O(N^2) noise: beyond the cap, the pairwise edges are skipped
	// (the artifact_entities membership is still recorded).
	maxSharesEntityFanout = 50
)

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
// The LLM call (slow, non-deterministic) happens in Enrich, but the graph WRITES
// are returned as an ApplyGraph hook the pipeline runs inside the effect
// transaction — so they are atomic with the stage's completion (a rolled-back or
// abandoned attempt writes no graph rows, and only the committing attempt's
// entities land). local governs the privacy gate exactly as the summarize stage.
type Entities struct {
	client llm.Client
	model  string
	local  bool
}

// NewEntities wraps an LLM client as the "entities" stage. The graph store is not
// injected here — the pipeline passes the transaction-scoped graph repo to the
// ApplyGraph hook this stage returns.
func NewEntities(client llm.Client, model string, local bool) *Entities {
	return &Entities{client: client, model: model, local: local}
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

	// Sanitize and de-duplicate within this response before any graph write.
	seen := make(map[string]bool, len(out.Entities))
	kept := make([]extractedEntity, 0, len(out.Entities))
	for _, ex := range out.Entities {
		ex.Name = strings.TrimSpace(ex.Name)
		key := ex.Kind + "\x00" + ex.Name
		if ex.Name == "" || len(ex.Name) > maxEntityNameLen || !kb.ValidEntityKind(ex.Kind) || seen[key] {
			continue // drop blank/over-long names, out-of-vocabulary kinds, and dupes
		}
		seen[key] = true
		kept = append(kept, extractedEntity{Name: ex.Name, Kind: ex.Kind, Aliases: normalizeAliases(ex.Aliases)})
		if len(kept) >= maxEntities {
			break
		}
	}

	artifactID := a.ID
	return kb.EnrichResult{
		Enrichments: []kb.Enrichment{{
			ArtifactID:    artifactID,
			Kind:          "entities",
			Payload:       jsonString(entitiesOutput{Entities: payloadEntities(kept)}),
			Model:         e.model,
			PromptVersion: entitiesPromptVersion,
		}},
		ApplyGraph: func(ctx context.Context, g kb.GraphTx) error {
			for _, ex := range kept {
				if err := materialize(ctx, g, artifactID, ex); err != nil {
					return err
				}
			}
			return nil
		},
	}, nil
}

// materialize upserts one entity, links it to the artifact, and connects the
// artifact to every other live artifact that already mentions the entity via a
// canonical (deduped) shares_entity edge — unless the entity is a hub (mentioned
// by more than maxSharesEntityFanout artifacts), in which case the pairwise edges
// are skipped as noise but the membership link is still recorded.
func materialize(ctx context.Context, g kb.GraphTx, artifactID string, ex extractedEntity) error {
	entityID, err := g.UpsertEntity(ctx, &kb.Entity{Kind: ex.Kind, Name: ex.Name, Aliases: ex.Aliases})
	if err != nil {
		return err
	}
	// Co-mentioning artifacts BEFORE we add this one, so we don't self-edge.
	others, err := g.ArtifactsForEntity(ctx, entityID)
	if err != nil {
		return err
	}
	if err := g.LinkArtifactEntity(ctx, artifactID, entityID); err != nil {
		return err
	}
	if len(others) > maxSharesEntityFanout {
		return nil // hub entity: skip pairwise edges (Finding 2 — no signal, O(N^2) noise)
	}
	for _, other := range others {
		if other == artifactID {
			continue
		}
		src, dst := canonicalPair(artifactID, other)
		if err := g.AddLink(ctx, src, dst, "shares_entity", 1.0); err != nil {
			return err
		}
	}
	return nil
}

// payloadEntities strips aliases from the recorded payload (the kinds/names are
// the audit signal; aliases live on the entity rows).
func payloadEntities(kept []extractedEntity) []extractedEntity {
	out := make([]extractedEntity, len(kept))
	for i, ex := range kept {
		out[i] = extractedEntity{Name: ex.Name, Kind: ex.Kind}
	}
	return out
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

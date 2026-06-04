package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"kb/internal/kb"
	"kb/internal/llm"
	"kb/internal/relevance"
)

const (
	// conflictSimThreshold is the cosine floor for a pair to be worth judging:
	// contradictions are about the same topic (high similarity) but assert opposite
	// things, so a dissimilar pair is never a contradiction and is pre-filtered out.
	conflictSimThreshold = 0.6
	// maxCandidatesPerClaim bounds the LLM judgments per claim (the closest few
	// candidates), so a large corpus can't make one artifact's detection unbounded.
	maxCandidatesPerClaim = 5
)

var conflictsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "contradicts": {"type": "boolean"},
    "rationale": {"type": "string"}
  },
  "required": ["contradicts"]
}`)

type contradictionVerdict struct {
	Contradicts bool   `json:"contradicts"`
	Rationale   string `json:"rationale"`
}

// ClaimReader is the read access the conflicts stage needs (satisfied by
// *store.ClaimRepo).
type ClaimReader interface {
	ListByArtifact(ctx context.Context, artifactID string) ([]*kb.Claim, error)
	ListExcludingArtifact(ctx context.Context, artifactID string, cloudSafeOnly bool) ([]*kb.Claim, error)
}

// Conflicts is the contradiction-detection stage (design §8.13). For each of the
// artifact's claims it vector-prefilters the most similar claims from other
// artifacts, asks the local model whether the pair actually contradicts, and emits
// a conflict for each that does. Conflicts are returned as data and inserted in
// the pipeline's effect transaction; the pipeline announces conflict.detected.
//
// DependsOn "claims" so this artifact's claims are already committed when the
// stage runs. When the judge endpoint is remote, the candidate set is restricted
// to cloud-safe (public/personal) claims so sensitive statements never leave the
// machine (§8.17). local also gates the stage on the artifact's own tier.
type Conflicts struct {
	reader ClaimReader
	judge  llm.Client
	model  string
	local  bool
}

// NewConflicts wraps a claim reader and an LLM judge as the "conflicts" stage.
func NewConflicts(reader ClaimReader, judge llm.Client, model string, local bool) *Conflicts {
	return &Conflicts{reader: reader, judge: judge, model: model, local: local}
}

func (*Conflicts) Name() string        { return "conflicts" }
func (*Conflicts) DependsOn() []string { return []string{"claims"} }

func (c *Conflicts) PrivacyOK(tier kb.PrivacyTier) bool {
	if c.local {
		return true
	}
	return tier.AllowsCloud()
}

func (c *Conflicts) Enrich(ctx context.Context, a *kb.Artifact) (kb.EnrichResult, error) {
	mine, err := c.reader.ListByArtifact(ctx, a.ID)
	if err != nil {
		return kb.EnrichResult{}, err
	}
	mine = withEmbedding(mine)
	if len(mine) == 0 {
		return kb.EnrichResult{}, nil
	}
	others, err := c.reader.ListExcludingArtifact(ctx, a.ID, !c.local)
	if err != nil {
		return kb.EnrichResult{}, err
	}
	if len(others) == 0 {
		return kb.EnrichResult{}, nil
	}

	var conflicts []kb.Conflict
	for _, m := range mine {
		for _, cand := range topCandidates(m, others) {
			verdict, err := c.judgeContradiction(ctx, m.Statement, cand.claim.Statement)
			if err != nil {
				return kb.EnrichResult{}, err
			}
			if verdict.Contradicts {
				conflicts = append(conflicts, kb.Conflict{
					ClaimA:    m.ID,
					ClaimB:    cand.claim.ID,
					Rationale: verdict.Rationale,
				})
			}
		}
	}
	return kb.EnrichResult{Conflicts: conflicts}, nil
}

type scoredClaim struct {
	claim *kb.Claim
	sim   float64
}

// topCandidates returns the most similar other-claims to m above the threshold,
// capped at maxCandidatesPerClaim.
func topCandidates(m *kb.Claim, others []*kb.Claim) []scoredClaim {
	cands := make([]scoredClaim, 0, len(others))
	for _, o := range others {
		if len(o.Embedding) != len(m.Embedding) {
			continue // dimension mismatch (e.g. a different embed model): not comparable
		}
		sim := relevance.Cosine(m.Embedding, o.Embedding)
		if sim >= conflictSimThreshold {
			cands = append(cands, scoredClaim{claim: o, sim: sim})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].sim != cands[j].sim {
			return cands[i].sim > cands[j].sim
		}
		return cands[i].claim.ID < cands[j].claim.ID // stable tiebreak
	})
	if len(cands) > maxCandidatesPerClaim {
		cands = cands[:maxCandidatesPerClaim]
	}
	return cands
}

func (c *Conflicts) judgeContradiction(ctx context.Context, a, b string) (contradictionVerdict, error) {
	system := "You judge whether two statements directly contradict each other — they cannot both be " +
		"true. Return STRICT JSON. Differing topics or merely different emphasis are NOT contradictions; " +
		"only a genuine factual conflict is."
	prompt := fmt.Sprintf("Statement A: %s\n\nStatement B: %s\n\nReturn JSON {contradicts: bool, "+
		"rationale: a short explanation}.", a, b)

	raw, err := c.judge.Generate(ctx, llm.GenerateRequest{
		Model:  c.model,
		System: system,
		Prompt: prompt,
		Format: conflictsSchema,
	})
	if err != nil {
		return contradictionVerdict{}, err
	}
	var v contradictionVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return contradictionVerdict{}, fmt.Errorf("conflicts: parse model JSON: %w", err)
	}
	return v, nil
}

// withEmbedding keeps only claims that carry an embedding (the comparison key).
func withEmbedding(claims []*kb.Claim) []*kb.Claim {
	out := claims[:0]
	for _, c := range claims {
		if len(c.Embedding) > 0 {
			out = append(out, c)
		}
	}
	return out
}

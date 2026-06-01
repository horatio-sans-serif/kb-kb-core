// Package relevance is the pure scoring model behind the relevance enricher
// (design §8.9): it combines a profile match (semantic similarity to the user's
// interests) with recency, encounter, engagement, and source-weight factors into
// a single ranking score. It holds no I/O — the enricher feeds it inputs gathered
// from the store via a SignalProvider, so the math is deterministic and unit-
// testable.
package relevance

import (
	"context"
	"fmt"
	"math"
	"strings"
)

// recencyHalfLifeHours is the age at which the recency factor halves (~30 days).
const recencyHalfLifeHours = 30 * 24

// Cosine is the cosine similarity of two equal-length vectors. Mismatched lengths
// or a zero-magnitude vector yield 0 (an undefined direction is treated as "no
// signal" rather than a panic or NaN).
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// WeightedVector is one profile interest vector and its relative weight.
type WeightedVector struct {
	Vector []float32
	Weight float64
}

// Profile is the (static) set of weighted interest vectors an artifact is scored
// against (design §8.9). An empty profile is neutral.
type Profile struct {
	interests []WeightedVector
}

// NewProfile builds a profile from weighted interest vectors.
func NewProfile(interests []WeightedVector) *Profile {
	return &Profile{interests: interests}
}

// BuildProfile embeds each interest phrase (equal weight) into a profile. Blank
// phrases are skipped; an embed failure aborts so the daemon can decide whether to
// proceed with an empty (neutral) profile. The embed func is injected so this
// stays I/O-free and testable.
func BuildProfile(ctx context.Context, embed func(ctx context.Context, text string) ([]float32, error), interests []string) (*Profile, error) {
	var ivs []WeightedVector
	for _, phrase := range interests {
		phrase = strings.TrimSpace(phrase)
		if phrase == "" {
			continue
		}
		vec, err := embed(ctx, phrase)
		if err != nil {
			return nil, fmt.Errorf("relevance: embed interest %q: %w", phrase, err)
		}
		ivs = append(ivs, WeightedVector{Vector: vec, Weight: 1})
	}
	return NewProfile(ivs), nil
}

// Size reports the number of interest vectors (0 = neutral profile).
func (p *Profile) Size() int { return len(p.interests) }

// Dim is the dimension of the profile's interest vectors (0 for an empty
// profile). A caller can compare it to an artifact embedding's dimension to
// catch a model mismatch, which Cosine would otherwise silently score as 0.
func (p *Profile) Dim() int {
	if len(p.interests) == 0 {
		return 0
	}
	return len(p.interests[0].Vector)
}

// Match scores vec against the profile: the maximum weight-scaled cosine over all
// interests, clamped to [0,1]. With the default equal weights this is simply the
// best cosine match; a non-unit weight scales an interest's contribution (a weight
// above 1 is capped by the clamp). An empty profile returns 1 (neutral — no
// interest bias), so relevance still ranks by the other factors when no interests
// are set.
func (p *Profile) Match(vec []float32) float64 {
	if len(p.interests) == 0 {
		return 1
	}
	best := 0.0
	for _, iv := range p.interests {
		if s := Cosine(vec, iv.Vector) * iv.Weight; s > best {
			best = s
		}
	}
	return clamp01(best)
}

// RecencyFactor decays from 1 (just seen) toward 0 with a ~30-day half-life. A
// negative age (a future timestamp, e.g. clock skew) clamps to 1.
func RecencyFactor(ageHours float64) float64 {
	if ageHours <= 0 {
		return 1
	}
	return math.Pow(0.5, ageHours/recencyHalfLifeHours)
}

// EncounterFactor rewards artifacts seen from multiple sources (the multi-source
// relevance signal, §8.9): 1 at a single sighting, growing logarithmically so a
// few extra sightings help and a flood does not dominate. Never below 1.
//
// Note: with scoring currently computed once at first ingest (see Score), the
// live encounter count is 1, so this factor is dormant until a re-scoring trigger
// (deferred to the §9 living-profile stage) runs relevance again on a re-encounter.
func EncounterFactor(count int) float64 {
	if count <= 1 {
		return 1
	}
	return 1 + math.Log2(float64(count))
}

// engagementBoost is applied to artifacts the user has engaged with.
const engagementBoost = 1.5

// EngagementFactor boosts engaged artifacts and leaves the rest neutral.
func EngagementFactor(engaged bool) float64 {
	if engaged {
		return engagementBoost
	}
	return 1
}

// Signals is the per-artifact data the scorer needs beyond the artifact row: its
// embedding vector, how many times it has been encountered, and the weight of the
// most authoritative source that yielded it. Gathered by a SignalProvider.
type Signals struct {
	Embedding    []float32
	Encounters   int
	SourceWeight float64
}

// Inputs are the resolved scalar inputs to Score.
type Inputs struct {
	Match        float64
	AgeHours     float64
	Encounters   int
	Engaged      bool
	SourceWeight float64
}

// Breakdown is the score with its factors, stored on the relevance enrichment row
// for explainability.
type Breakdown struct {
	ProfileMatch float64 `json:"profile_match"`
	Recency      float64 `json:"recency"`
	Encounter    float64 `json:"encounter"`
	Engagement   float64 `json:"engagement"`
	SourceWeight float64 `json:"source_weight"`
	Score        float64 `json:"score"`
}

// Score combines the factors multiplicatively (design §8.9). profile_match and
// recency are in [0,1]; the encounter, engagement, and source-weight factors are
// multipliers >= 1 (or the source's own weight), so the result is a relative
// ranking score, not a probability.
//
// Caveat: it is currently computed once, at first ingest, when the encounter
// count is 1 and only the first source is known — so EncounterFactor and
// SourceWeight do not yet differentiate artifacts. Re-scoring on re-encounter and
// engagement is deferred to the §9 living-profile stage.
//
// SourceWeight is floored at 0: source_weight is an externally-mutable column (a
// future yield feedback loop adjusts it), and a negative value would invert the
// relevance_score DESC ranking.
func Score(in Inputs) Breakdown {
	recency := RecencyFactor(in.AgeHours)
	encounter := EncounterFactor(in.Encounters)
	engagement := EngagementFactor(in.Engaged)
	sourceWeight := math.Max(0, in.SourceWeight)
	return Breakdown{
		ProfileMatch: in.Match,
		Recency:      recency,
		Encounter:    encounter,
		Engagement:   engagement,
		SourceWeight: sourceWeight,
		Score:        in.Match * recency * encounter * engagement * sourceWeight,
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

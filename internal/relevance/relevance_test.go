package relevance

import (
	"context"
	"errors"
	"math"
	"testing"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestCosine(t *testing.T) {
	if c := Cosine([]float32{1, 0}, []float32{1, 0}); !almost(c, 1) {
		t.Errorf("identical vectors cosine = %v, want 1", c)
	}
	if c := Cosine([]float32{1, 0}, []float32{0, 1}); !almost(c, 0) {
		t.Errorf("orthogonal cosine = %v, want 0", c)
	}
	if c := Cosine([]float32{1, 0}, []float32{-1, 0}); !almost(c, -1) {
		t.Errorf("opposite cosine = %v, want -1", c)
	}
	// Mismatched dimensions and zero vectors yield 0 (no panic, no NaN).
	if c := Cosine([]float32{1, 2, 3}, []float32{1, 2}); c != 0 {
		t.Errorf("mismatched-dim cosine = %v, want 0", c)
	}
	if c := Cosine([]float32{0, 0}, []float32{1, 1}); c != 0 {
		t.Errorf("zero-vector cosine = %v, want 0", c)
	}
}

func TestProfile_Match(t *testing.T) {
	// Two interests; Match is the max cosine, clamped to [0,1].
	p := NewProfile([]WeightedVector{
		{Vector: []float32{1, 0}, Weight: 1},
		{Vector: []float32{0, 1}, Weight: 1},
	})
	if m := p.Match([]float32{1, 0}); !almost(m, 1) {
		t.Errorf("match aligned with an interest = %v, want 1", m)
	}
	// Pointing opposite both interests → negative cosine clamps to 0.
	if m := p.Match([]float32{-1, -1}); m != 0 {
		t.Errorf("match opposite all interests = %v, want 0 (clamped)", m)
	}
	// An empty profile is neutral (no interest bias).
	if m := NewProfile(nil).Match([]float32{1, 0}); !almost(m, 1) {
		t.Errorf("empty-profile match = %v, want 1 (neutral)", m)
	}
}

func TestProfile_MatchWith_LivingProfile(t *testing.T) {
	// Static interest {0,1}; the artifact {1,0} is orthogonal → static match 0.
	p := NewProfile([]WeightedVector{{Vector: []float32{0, 1}, Weight: 1}})
	if m := p.MatchWith([]float32{1, 0}, nil); m != 0 {
		t.Errorf("static-only match = %v, want 0", m)
	}
	// An engaged vector aligned with the artifact lifts the match (discounted).
	if m := p.MatchWith([]float32{1, 0}, [][]float32{{1, 0}}); !almost(m, engagedInterestWeight) {
		t.Errorf("living match = %v, want %v (engaged, weighted)", m, engagedInterestWeight)
	}
	// A perfect static interest match still dominates an engaged one.
	if m := p.MatchWith([]float32{0, 1}, [][]float32{{0, 1}}); !almost(m, 1) {
		t.Errorf("static perfect match = %v, want 1 (beats the discounted engaged)", m)
	}
	// A dimension-mismatched engaged vector contributes nothing (no panic).
	if m := p.MatchWith([]float32{1, 0}, [][]float32{{1, 0, 0}}); m != 0 {
		t.Errorf("mismatched-dim engaged = %v, want 0", m)
	}
	// Empty everything is still neutral.
	if m := NewProfile(nil).MatchWith([]float32{1, 0}, nil); !almost(m, 1) {
		t.Errorf("empty match = %v, want 1 (neutral)", m)
	}
}

func TestProfile_Match_RespectsWeight(t *testing.T) {
	// A down-weighted interest contributes less even when perfectly aligned.
	p := NewProfile([]WeightedVector{
		{Vector: []float32{1, 0}, Weight: 0.25},
		{Vector: []float32{0, 1}, Weight: 1.0},
	})
	if m := p.Match([]float32{1, 0}); !almost(m, 0.25) {
		t.Errorf("weighted match = %v, want 0.25 (cosine 1 * weight 0.25)", m)
	}
}

func TestFactors_Bounds(t *testing.T) {
	// Recency: ~1 when fresh, decays toward 0, never above 1 or below 0.
	if f := RecencyFactor(0); !almost(f, 1) {
		t.Errorf("recency at age 0 = %v, want 1", f)
	}
	if f := RecencyFactor(recencyHalfLifeHours); !almost(f, 0.5) {
		t.Errorf("recency at one half-life = %v, want 0.5", f)
	}
	if f := RecencyFactor(1e9); f < 0 || f > 1 {
		t.Errorf("recency at huge age = %v, want within [0,1]", f)
	}
	if f := RecencyFactor(-100); f != 1 {
		t.Errorf("recency for a future timestamp = %v, want clamped to 1", f)
	}

	// Encounter: 1 at a single sighting, monotone increasing, >1 for more.
	if f := EncounterFactor(1); !almost(f, 1) {
		t.Errorf("encounter factor at 1 = %v, want 1", f)
	}
	if EncounterFactor(8) <= EncounterFactor(2) {
		t.Error("encounter factor should increase with more sightings")
	}
	if f := EncounterFactor(0); f < 1 {
		t.Errorf("encounter factor at 0 = %v, want >= 1 (never penalize)", f)
	}

	// Engagement: 1 when never engaged, a boost when engaged.
	if EngagementFactor(false) != 1 {
		t.Error("never-engaged factor should be 1")
	}
	if EngagementFactor(true) <= 1 {
		t.Error("engaged factor should boost above 1")
	}
}

func TestBuildProfile(t *testing.T) {
	calls := 0
	embed := func(_ context.Context, text string) ([]float32, error) {
		calls++
		return []float32{float32(len(text)), 0}, nil
	}
	// Blank phrases are skipped.
	p, err := BuildProfile(context.Background(), embed, []string{"go async", "  ", "databases"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if p.Size() != 2 || calls != 2 {
		t.Errorf("profile size=%d calls=%d, want 2 and 2 (blank skipped)", p.Size(), calls)
	}
}

func TestBuildProfile_EmbedErrorAborts(t *testing.T) {
	embed := func(_ context.Context, _ string) ([]float32, error) { return nil, errors.New("down") }
	if _, err := BuildProfile(context.Background(), embed, []string{"x"}); err == nil {
		t.Error("an embed failure should abort profile construction")
	}
}

func TestScore_CombinesFactors(t *testing.T) {
	bd := Score(Inputs{Match: 0.5, AgeHours: 0, Encounters: 1, Engaged: false, SourceWeight: 2})
	// score = match(0.5) * recency(1) * encounter(1) * engagement(1) * weight(2) = 1.0
	if !almost(bd.Score, 1.0) {
		t.Errorf("score = %v, want 1.0", bd.Score)
	}
	if bd.ProfileMatch != 0.5 || bd.SourceWeight != 2 {
		t.Errorf("breakdown did not record inputs: %+v", bd)
	}
	// A zero profile match zeroes the score regardless of the multipliers.
	if z := Score(Inputs{Match: 0, AgeHours: 0, Encounters: 5, SourceWeight: 3}); z.Score != 0 {
		t.Errorf("score with zero match = %v, want 0", z.Score)
	}
}

func TestScore_NegativeSourceWeightFloored(t *testing.T) {
	// A negative source weight (from a future yield feedback loop) must not invert
	// the ranking: it floors to 0, zeroing the score rather than going negative.
	bd := Score(Inputs{Match: 1, AgeHours: 0, Encounters: 1, SourceWeight: -2})
	if bd.Score != 0 || bd.SourceWeight != 0 {
		t.Errorf("score with negative weight = %+v, want score and weight floored to 0", bd)
	}
}

func TestScore_NonFiniteSourceWeightSanitized(t *testing.T) {
	// NaN/±Inf source weight must not produce a non-finite score (which would
	// break JSON marshaling and the relevance_score DESC ordering).
	for _, w := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		bd := Score(Inputs{Match: 1, AgeHours: 0, Encounters: 1, SourceWeight: w})
		if math.IsNaN(bd.Score) || math.IsInf(bd.Score, 0) {
			t.Errorf("score with weight %v = %v, want finite", w, bd.Score)
		}
		if bd.SourceWeight != 0 {
			t.Errorf("non-finite weight %v should sanitize to 0, got %v", w, bd.SourceWeight)
		}
	}
}

func TestBuildProfile_RejectsInconsistentDims(t *testing.T) {
	// The model returns a 2-dim vector for the first phrase and 3-dim for the
	// second; construction must abort so Dim() is a real invariant.
	embed := func(_ context.Context, text string) ([]float32, error) {
		if text == "a" {
			return []float32{1, 0}, nil
		}
		return []float32{1, 0, 0}, nil
	}
	if _, err := BuildProfile(context.Background(), embed, []string{"a", "b"}); err == nil {
		t.Error("inconsistent interest-vector dimensions must abort profile construction")
	}
}

func TestProfile_DimAndWeightClamp(t *testing.T) {
	if d := NewProfile(nil).Dim(); d != 0 {
		t.Errorf("empty profile Dim = %d, want 0", d)
	}
	p := NewProfile([]WeightedVector{{Vector: []float32{1, 0, 0}, Weight: 4}})
	if d := p.Dim(); d != 3 {
		t.Errorf("Dim = %d, want 3", d)
	}
	// A weight above 1 on a perfectly-aligned interest is capped by the clamp.
	if m := p.Match([]float32{1, 0, 0}); !almost(m, 1) {
		t.Errorf("match with weight 4 = %v, want 1 (clamped)", m)
	}
}

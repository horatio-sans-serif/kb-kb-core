package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"kb/internal/kb"
	"kb/internal/relevance"
)

type fakeSignals struct {
	sig     relevance.Signals
	err     error
	engaged [][]float32
}

func (f fakeSignals) Signals(_ context.Context, _ string) (relevance.Signals, error) {
	return f.sig, f.err
}

func (f fakeSignals) EngagedVectors(_ context.Context, _ string) ([][]float32, error) {
	return f.engaged, nil
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestRelevance_ScoresAndPatches(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	prof := relevance.NewProfile([]relevance.WeightedVector{{Vector: []float32{1, 0}, Weight: 1}})
	r := NewRelevance(fakeSignals{sig: relevance.Signals{
		Embedding: []float32{1, 0}, Encounters: 2, SourceWeight: 1,
	}}, prof)
	r.now = fixedClock(now)

	a := &kb.Artifact{ID: "01ART", LastSeenAt: now.Format(time.RFC3339Nano)}
	res, err := r.Enrich(context.Background(), a)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	// match=1, recency=1 (age 0), encounter(2)=2, engagement=1, weight=1 → 2.0
	if res.Patch == nil || res.Patch.RelevanceScore == nil {
		t.Fatal("expected a RelevanceScore patch")
	}
	if got := *res.Patch.RelevanceScore; math.Abs(got-2.0) > 1e-6 {
		t.Errorf("relevance score = %v, want 2.0", got)
	}
	if len(res.Enrichments) != 1 || res.Enrichments[0].Kind != "relevance" {
		t.Fatalf("enrichments = %+v, want one 'relevance' row", res.Enrichments)
	}
	var bd relevance.Breakdown
	if err := json.Unmarshal([]byte(res.Enrichments[0].Payload), &bd); err != nil {
		t.Fatalf("breakdown payload not JSON: %v", err)
	}
	if bd.ProfileMatch != 1 || bd.Encounter != 2 {
		t.Errorf("breakdown = %+v, want match=1 encounter=2", bd)
	}
}

func TestRelevance_NoEmbeddingIsNoop(t *testing.T) {
	r := NewRelevance(fakeSignals{sig: relevance.Signals{Embedding: nil, Encounters: 1, SourceWeight: 1}},
		relevance.NewProfile(nil))
	res, err := r.Enrich(context.Background(), &kb.Artifact{ID: "01ART"})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if res.Patch != nil || len(res.Enrichments) != 0 {
		t.Errorf("no embedding should be a no-op, got %+v", res)
	}
}

func TestRelevance_LivingProfileBoostsMatch(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// Static interest {0,1}; the artifact's embedding {1,0} is orthogonal, so the
	// static-only match is 0 → score 0.
	prof := relevance.NewProfile([]relevance.WeightedVector{{Vector: []float32{0, 1}, Weight: 1}})
	a := &kb.Artifact{ID: "01ART", LastSeenAt: now.Format(time.RFC3339Nano)}
	sig := relevance.Signals{Embedding: []float32{1, 0}, Encounters: 1, SourceWeight: 1}

	noEngage := NewRelevance(fakeSignals{sig: sig}, prof)
	noEngage.now = fixedClock(now)
	res0, err := noEngage.Enrich(context.Background(), a)
	if err != nil {
		t.Fatalf("enrich (no engagement): %v", err)
	}
	s0 := *res0.Patch.RelevanceScore

	// An engaged artifact aligned with this one lifts its match (and score).
	withEngage := NewRelevance(fakeSignals{sig: sig, engaged: [][]float32{{1, 0}}}, prof)
	withEngage.now = fixedClock(now)
	res1, err := withEngage.Enrich(context.Background(), a)
	if err != nil {
		t.Fatalf("enrich (engaged): %v", err)
	}
	s1 := *res1.Patch.RelevanceScore

	if !(s1 > s0) {
		t.Errorf("score with engaged interest = %v, want > static-only %v (living profile)", s1, s0)
	}
}

func TestRelevance_DimMismatchIsError(t *testing.T) {
	// Profile vectors are 2-dim; the artifact embedding is 3-dim (e.g. the embed
	// model changed). This must error rather than silently score 0.
	prof := relevance.NewProfile([]relevance.WeightedVector{{Vector: []float32{1, 0}, Weight: 1}})
	r := NewRelevance(fakeSignals{sig: relevance.Signals{Embedding: []float32{1, 0, 0}, Encounters: 1, SourceWeight: 1}}, prof)
	if _, err := r.Enrich(context.Background(), &kb.Artifact{ID: "01ART"}); err == nil {
		t.Error("a profile/embedding dimension mismatch must be a stage error")
	}
}

func TestRelevance_ProviderErrorPropagates(t *testing.T) {
	r := NewRelevance(fakeSignals{err: errors.New("db down")}, relevance.NewProfile(nil))
	if _, err := r.Enrich(context.Background(), &kb.Artifact{ID: "01ART"}); err == nil {
		t.Error("a provider error must propagate")
	}
}

func TestRelevance_Contract(t *testing.T) {
	r := NewRelevance(fakeSignals{}, relevance.NewProfile(nil))
	if r.Name() != "relevance" {
		t.Errorf("name = %q, want relevance", r.Name())
	}
	if len(r.DependsOn()) != 1 || r.DependsOn()[0] != "embed" {
		t.Errorf("DependsOn = %v, want [embed]", r.DependsOn())
	}
	// Pure local computation: safe for every tier.
	for _, tier := range []kb.PrivacyTier{kb.Public, kb.Personal, kb.Sensitive, kb.Restricted} {
		if !r.PrivacyOK(tier) {
			t.Errorf("PrivacyOK(%s) = false, want true (no external call)", tier)
		}
	}
}

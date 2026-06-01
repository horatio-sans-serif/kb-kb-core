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
	sig relevance.Signals
	err error
}

func (f fakeSignals) Signals(_ context.Context, _ string) (relevance.Signals, error) {
	return f.sig, f.err
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

package enrich

import (
	"context"
	"testing"

	"kb/internal/kb"
)

// fakeClaimReader serves canned claim sets for the conflicts stage.
type fakeClaimReader struct {
	mine         []*kb.Claim
	others       []*kb.Claim
	gotCloudSafe bool
}

func (f *fakeClaimReader) ListByArtifact(_ context.Context, _ string) ([]*kb.Claim, error) {
	return f.mine, nil
}
func (f *fakeClaimReader) ListExcludingArtifact(_ context.Context, _ string, cloudSafe bool) ([]*kb.Claim, error) {
	f.gotCloudSafe = cloudSafe
	return f.others, nil
}

func TestConflicts_Contract(t *testing.T) {
	c := NewConflicts(&fakeClaimReader{}, &fakeLLM{}, "m", true)
	if c.Name() != "conflicts" {
		t.Errorf("Name = %q", c.Name())
	}
	if len(c.DependsOn()) != 1 || c.DependsOn()[0] != "claims" {
		t.Errorf("DependsOn = %v, want [claims]", c.DependsOn())
	}
	if !c.PrivacyOK(kb.Sensitive) {
		t.Error("local conflicts should allow sensitive")
	}
	if NewConflicts(&fakeClaimReader{}, &fakeLLM{}, "m", false).PrivacyOK(kb.Sensitive) {
		t.Error("remote conflicts must gate sensitive")
	}
}

func TestConflicts_DetectsSimilarContradiction(t *testing.T) {
	reader := &fakeClaimReader{
		mine: []*kb.Claim{{ID: "M1", Statement: "The sky is blue", Embedding: []float32{1, 0}}},
		others: []*kb.Claim{
			{ID: "O1", Statement: "The sky is green", Embedding: []float32{1, 0}}, // cosine 1.0 → candidate
			{ID: "O2", Statement: "Cats are mammals", Embedding: []float32{0, 1}}, // cosine 0 → not a candidate
		},
	}
	judge := &fakeLLM{resp: `{"contradicts":true,"rationale":"different colors asserted"}`}
	c := NewConflicts(reader, judge, "m", true)

	res, err := c.Enrich(context.Background(), &kb.Artifact{ID: "01ART", PrivacyTier: kb.Public})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("conflicts = %d, want 1 (only the similar pair judged): %+v", len(res.Conflicts), res.Conflicts)
	}
	cf := res.Conflicts[0]
	if cf.ClaimA != "M1" || cf.ClaimB != "O1" || cf.Rationale == "" {
		t.Errorf("conflict = %+v, want M1<->O1 with a rationale", cf)
	}
	// Only the high-cosine candidate was sent to the judge.
	if judge.calls != 1 {
		t.Errorf("judge calls = %d, want 1 (the dissimilar pair is pre-filtered out)", judge.calls)
	}
	// Local → not cloud-safe-restricted.
	if reader.gotCloudSafe {
		t.Error("a local judge should not restrict the candidate set")
	}
}

func TestConflicts_NoContradiction(t *testing.T) {
	reader := &fakeClaimReader{
		mine:   []*kb.Claim{{ID: "M1", Statement: "Water boils at 100C", Embedding: []float32{1, 0}}},
		others: []*kb.Claim{{ID: "O1", Statement: "Water boils at sea level at 100C", Embedding: []float32{1, 0}}},
	}
	judge := &fakeLLM{resp: `{"contradicts":false,"rationale":"consistent"}`}
	res, err := NewConflicts(reader, judge, "m", true).Enrich(context.Background(), &kb.Artifact{ID: "01ART", PrivacyTier: kb.Public})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("agreeing claims should produce no conflict, got %+v", res.Conflicts)
	}
}

func TestConflicts_RemoteRestrictsCandidates(t *testing.T) {
	reader := &fakeClaimReader{mine: []*kb.Claim{{ID: "M1", Statement: "x", Embedding: []float32{1}}}}
	c := NewConflicts(reader, &fakeLLM{resp: `{"contradicts":false}`}, "m", false) // remote
	if _, err := c.Enrich(context.Background(), &kb.Artifact{ID: "01ART", PrivacyTier: kb.Public}); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if !reader.gotCloudSafe {
		t.Error("a remote judge must request the cloud-safe candidate set")
	}
}

func TestConflicts_NoClaimsNoOp(t *testing.T) {
	c := NewConflicts(&fakeClaimReader{}, &fakeLLM{}, "m", true)
	res, err := c.Enrich(context.Background(), &kb.Artifact{ID: "01ART", PrivacyTier: kb.Public})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("no claims → no conflicts, got %+v", res.Conflicts)
	}
}

package pipeline

import (
	"context"
	"testing"

	"kb/internal/jobs"
	"kb/internal/kb"
	"kb/internal/storetest"
)

type fakeEnricher struct {
	name     string
	deps     []string
	okFn     func(kb.PrivacyTier) bool
	onEnrich func(*kb.Artifact)
}

func (f *fakeEnricher) Name() string        { return f.name }
func (f *fakeEnricher) DependsOn() []string { return f.deps }
func (f *fakeEnricher) PrivacyOK(t kb.PrivacyTier) bool {
	if f.okFn != nil {
		return f.okFn(t)
	}
	return true
}
func (f *fakeEnricher) Enrich(_ context.Context, a *kb.Artifact) (kb.EnrichResult, error) {
	if f.onEnrich != nil {
		f.onEnrich(a)
	}
	return kb.EnrichResult{
		Enrichments: []kb.Enrichment{{ArtifactID: a.ID, Kind: f.name, Payload: "{}", Model: "test", PromptVersion: "v1"}},
	}, nil
}

func TestPipeline_Validate(t *testing.T) {
	p := New()
	p.Register(&fakeEnricher{name: "b", deps: []string{"a"}})
	if err := p.Validate(); err == nil {
		t.Error("a dependency on an unregistered stage should fail validation")
	}

	cyc := New()
	cyc.Register(&fakeEnricher{name: "a", deps: []string{"b"}})
	cyc.Register(&fakeEnricher{name: "b", deps: []string{"a"}})
	if err := cyc.Validate(); err == nil {
		t.Error("a dependency cycle should fail validation")
	}

	ok := New()
	ok.Register(&fakeEnricher{name: "a"})
	ok.Register(&fakeEnricher{name: "b", deps: []string{"a"}})
	if err := ok.Validate(); err != nil {
		t.Errorf("a valid DAG should validate: %v", err)
	}
}

func TestPipeline_EnqueueAndRun_RespectsOrder(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()

	var order []string
	p := New()
	p.Register(&fakeEnricher{name: "a", onEnrich: func(*kb.Artifact) { order = append(order, "a") }})
	p.Register(&fakeEnricher{name: "b", deps: []string{"a"}, onEnrich: func(*kb.Artifact) { order = append(order, "b") }})

	a, err := st.Artifacts.UpsertByCanonical(ctx, &kb.Artifact{Type: kb.Note, Title: "x"})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if err := p.Enqueue(ctx, st, a.ID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	r := jobs.NewRunner(st)
	p.RegisterHandlers(st, r)
	if _, err := r.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Errorf("execution order = %v, want [a b] (depends_on wired)", order)
	}
	ens, err := st.Enrichments.ListForArtifact(ctx, a.ID)
	if err != nil {
		t.Fatalf("list enrichments: %v", err)
	}
	if len(ens) != 2 {
		t.Errorf("persisted %d enrichments, want 2", len(ens))
	}
	if got, _ := st.Artifacts.Get(ctx, a.ID); got.Status != kb.StatusReady {
		t.Errorf("artifact status = %q, want ready after the pipeline completes", got.Status)
	}
}

func TestPipeline_PrivacyGate(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()

	calls := 0
	cloud := &fakeEnricher{
		name:     "summarize",
		okFn:     func(tier kb.PrivacyTier) bool { return tier.AllowsCloud() },
		onEnrich: func(*kb.Artifact) { calls++ },
	}
	p := New()
	p.Register(cloud)
	r := jobs.NewRunner(st)
	p.RegisterHandlers(st, r)

	// Sensitive artifact: the gate must skip the cloud enricher entirely.
	sens, _ := st.Artifacts.UpsertByCanonical(ctx, &kb.Artifact{Type: kb.Note, Title: "secret", PrivacyTier: kb.Sensitive})
	if err := p.Enqueue(ctx, st, sens.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("cloud enricher ran on a sensitive artifact (%d calls) — privacy gate failed", calls)
	}
	if ens, _ := st.Enrichments.ListForArtifact(ctx, sens.ID); len(ens) != 0 {
		t.Errorf("sensitive artifact got %d enrichments, want 0", len(ens))
	}

	// Personal artifact: the gate allows it.
	pers, _ := st.Artifacts.UpsertByCanonical(ctx, &kb.Artifact{Type: kb.Note, Title: "ok", PrivacyTier: kb.Personal})
	if err := p.Enqueue(ctx, st, pers.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("cloud enricher should run on a personal artifact, calls=%d", calls)
	}
}

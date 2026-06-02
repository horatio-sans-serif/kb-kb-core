package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"kb/internal/events"
	"kb/internal/jobs"
	"kb/internal/kb"
	"kb/internal/store"
	"kb/internal/storetest"
)

type fakeEnricher struct {
	name      string
	deps      []string
	okFn      func(kb.PrivacyTier) bool
	onEnrich  func(*kb.Artifact)
	enrichErr error
	extra     []kb.Enrichment // appended to the result (e.g. a bad-FK row to force a tx rollback)
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
	if f.enrichErr != nil {
		return kb.EnrichResult{}, f.enrichErr
	}
	ens := []kb.Enrichment{{ArtifactID: a.ID, Kind: f.name, Payload: "{}", Model: "test", PromptVersion: "v1"}}
	ens = append(ens, f.extra...)
	return kb.EnrichResult{Enrichments: ens}, nil
}

// failFastRunner builds a runner that fails a job on its first error (no retry
// backoff) so failure-path tests do not wait on exponential backoff.
func failFastRunner(st *store.Store) *jobs.Runner {
	return jobs.NewRunner(st, jobs.WithMaxAttempts(1), jobs.WithBackoff(func(int) time.Duration { return 0 }))
}

// drainEvents collects the events buffered on sub without blocking.
func drainEvents(sub <-chan events.Event) []events.Event {
	var out []events.Event
	for {
		select {
		case e := <-sub:
			out = append(out, e)
		default:
			return out
		}
	}
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

func TestPipeline_Register_RejectsReservedAndEmpty(t *testing.T) {
	mustPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: expected panic", name)
			}
		}()
		fn()
	}
	mustPanic("empty name", func() { New().Register(&fakeEnricher{name: ""}) })
	mustPanic("reserved @ prefix", func() { New().Register(&fakeEnricher{name: "@ready"}) })
	mustPanic("duplicate", func() {
		p := New()
		p.Register(&fakeEnricher{name: "a"})
		p.Register(&fakeEnricher{name: "a"})
	})
}

func TestPipeline_EnqueueAndRun_RespectsOrder(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	bus := events.NewBus()
	sub, unsub := bus.Subscribe()
	defer unsub()

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
	p.RegisterHandlers(st, r, bus)
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
	// Exactly one terminal artifact.ready event, and no artifact.failed.
	var ready, failed int
	for _, e := range drainEvents(sub) {
		switch e.Type {
		case events.ArtifactReady:
			ready++
		case events.ArtifactFailed:
			failed++
		}
	}
	if ready != 1 || failed != 0 {
		t.Errorf("events: ready=%d failed=%d, want ready=1 failed=0", ready, failed)
	}
}

func TestPipeline_StageFailure_FinalizesArtifactFailed(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	bus := events.NewBus()
	sub, unsub := bus.Subscribe()
	defer unsub()

	// a -> b(fails) -> c. b's failure cascade-fails c; the artifact must still
	// reach a terminal 'failed' status (the keystone: a failed stage formerly
	// stranded the artifact in 'enriching' forever via the @ready sentinel).
	p := New()
	p.Register(&fakeEnricher{name: "a"})
	p.Register(&fakeEnricher{name: "b", deps: []string{"a"}, enrichErr: errors.New("boom")})
	p.Register(&fakeEnricher{name: "c", deps: []string{"b"}})

	a, err := st.Artifacts.UpsertByCanonical(ctx, &kb.Artifact{Type: kb.Note, Title: "x"})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if err := p.Enqueue(ctx, st, a.ID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	r := failFastRunner(st)
	p.RegisterHandlers(st, r, bus)
	if _, err := r.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	got, _ := st.Artifacts.Get(ctx, a.ID)
	if got.Status != kb.StatusFailed {
		t.Errorf("artifact status = %q, want failed after a stage fails (not stranded in enriching)", got.Status)
	}
	var ready, failed int
	for _, e := range drainEvents(sub) {
		switch e.Type {
		case events.ArtifactReady:
			ready++
		case events.ArtifactFailed:
			failed++
		}
	}
	if failed != 1 || ready != 0 {
		t.Errorf("events: ready=%d failed=%d, want ready=0 failed=1", ready, failed)
	}
}

func TestPipeline_HandlerCompletesAtomically(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	e := &fakeEnricher{name: "a"}
	p := New()
	p.Register(e)

	art, err := st.Artifacts.UpsertByCanonical(ctx, &kb.Artifact{Type: kb.Note, Title: "x"})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if err := p.Enqueue(ctx, st, art.ID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Claim the stage's job (→ running) and run the handler directly — no runner,
	// so nothing but the handler's own transaction can mark it done.
	job, err := st.Jobs.Claim(ctx, store.FormatTime(time.Now()))
	if err != nil || job == nil {
		t.Fatalf("claim: job=%v err=%v", job, err)
	}
	if err := p.handler(st, e)(ctx, *job); err != nil {
		t.Fatalf("handler: %v", err)
	}

	// The job is done — committed atomically with the enrichment, without the
	// runner's separate MarkDone.
	got, err := st.Jobs.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != "done" {
		t.Errorf("job status = %q, want done (handler marks done in its effect tx)", got.Status)
	}
	if ens, _ := st.Enrichments.ListForArtifact(ctx, art.ID); len(ens) != 1 {
		t.Errorf("enrichments = %d, want 1", len(ens))
	}

	// A reclaim cannot resurrect a done job, so the stage is never re-run and its
	// enrichment can never be duplicated (exactly-once).
	if n, err := st.Jobs.ReclaimStuck(ctx, store.FormatTime(time.Now())); err != nil || n != 0 {
		t.Errorf("reclaimed %d (err %v), want 0 — a done job must not be re-run", n, err)
	}
}

func TestPipeline_HandlerRollsBackOnWriteError(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	// The enricher returns a valid row plus one with a bogus artifact_id, which
	// violates the enrichments FK — the second Create fails mid-transaction.
	e := &fakeEnricher{name: "a", extra: []kb.Enrichment{
		{ArtifactID: "01NONEXISTENT", Kind: "bad", Payload: "{}", Model: "test", PromptVersion: "v1"},
	}}
	p := New()
	p.Register(e)

	art, err := st.Artifacts.UpsertByCanonical(ctx, &kb.Artifact{Type: kb.Note, Title: "x"})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if err := p.Enqueue(ctx, st, art.ID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	job, err := st.Jobs.Claim(ctx, store.FormatTime(time.Now()))
	if err != nil || job == nil {
		t.Fatalf("claim: job=%v err=%v", job, err)
	}

	// The handler must fail, and the whole transaction must roll back: neither the
	// valid enrichment nor the MarkDone may survive (atomicity).
	if err := p.handler(st, e)(ctx, *job); err == nil {
		t.Fatal("handler should error when an enrichment write fails")
	}
	got, _ := st.Jobs.Get(ctx, job.ID)
	if got.Status != "running" {
		t.Errorf("job status = %q, want running (MarkDone rolled back with the failed write)", got.Status)
	}
	if ens, _ := st.Enrichments.ListForArtifact(ctx, art.ID); len(ens) != 0 {
		t.Errorf("enrichments = %d, want 0 (the valid row rolled back with the bad one)", len(ens))
	}
}

func TestPipeline_Reconcile(t *testing.T) {
	run := func(t *testing.T, fail bool, wantStatus kb.ArtifactStatus, wantEvt events.EventType) {
		t.Helper()
		st := storetest.New(t)
		ctx := context.Background()
		bus := events.NewBus()
		sub, unsub := bus.Subscribe()
		defer unsub()

		p := New()
		p.Register(&fakeEnricher{name: "a"})
		art, err := st.Artifacts.UpsertByCanonical(ctx, &kb.Artifact{Type: kb.Note, Title: "x"})
		if err != nil {
			t.Fatalf("create artifact: %v", err)
		}
		if err := p.Enqueue(ctx, st, art.ID); err != nil { // → status enriching, 1 pending job
			t.Fatalf("enqueue: %v", err)
		}

		// Drive the job terminal WITHOUT the runner's finalize hook — simulating a
		// crash between the job's commit and its finalize.
		job, _ := st.Jobs.Claim(ctx, store.FormatTime(time.Now()))
		if fail {
			if err := st.Jobs.MarkFailed(ctx, job.ID, "boom"); err != nil {
				t.Fatalf("mark failed: %v", err)
			}
		} else if err := st.Jobs.MarkDone(ctx, job.ID); err != nil {
			t.Fatalf("mark done: %v", err)
		}
		if got, _ := st.Artifacts.Get(ctx, art.ID); got.Status != kb.StatusEnriching {
			t.Fatalf("precondition: artifact status = %q, want enriching (finalize was skipped)", got.Status)
		}

		if err := p.Reconcile(ctx, st, bus); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if got, _ := st.Artifacts.Get(ctx, art.ID); got.Status != wantStatus {
			t.Errorf("after reconcile status = %q, want %q", got.Status, wantStatus)
		}
		var saw bool
		for _, ev := range drainEvents(sub) {
			if ev.Type == wantEvt {
				saw = true
			}
		}
		if !saw {
			t.Errorf("reconcile did not publish %q", wantEvt)
		}
	}

	t.Run("all done → ready", func(t *testing.T) { run(t, false, kb.StatusReady, events.ArtifactReady) })
	t.Run("a failure → failed", func(t *testing.T) { run(t, true, kb.StatusFailed, events.ArtifactFailed) })
}

func TestPipeline_PrivacyGate(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	bus := events.NewBus()

	calls := 0
	cloud := &fakeEnricher{
		name:     "summarize",
		okFn:     func(tier kb.PrivacyTier) bool { return tier.AllowsCloud() },
		onEnrich: func(*kb.Artifact) { calls++ },
	}
	p := New()
	p.Register(cloud)
	r := jobs.NewRunner(st)
	p.RegisterHandlers(st, r, bus)

	// Sensitive artifact: the gate must skip the cloud enricher entirely, but
	// record an auditable "skipped" enrichment and still finalize the artifact
	// (a privacy skip is a deliberate no-op, not a failure).
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
	ens, _ := st.Enrichments.ListForArtifact(ctx, sens.ID)
	if len(ens) != 1 || ens[0].Kind != "skipped" {
		t.Errorf("sensitive artifact enrichments = %+v, want exactly one 'skipped' row", ens)
	}
	if got, _ := st.Artifacts.Get(ctx, sens.ID); got.Status != kb.StatusReady {
		t.Errorf("skipped artifact status = %q, want ready (a privacy skip is not a failure)", got.Status)
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

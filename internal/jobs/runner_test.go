package jobs

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"kb/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store.New(db)
}

func TestBackoff_Increasing(t *testing.T) {
	b := ExpBackoff(time.Second, time.Hour)
	if !(b(1) < b(2) && b(2) < b(3)) {
		t.Errorf("backoff not increasing: %v, %v, %v", b(1), b(2), b(3))
	}
	if b(100) != time.Hour {
		t.Errorf("backoff(100) = %v, want cap %v", b(100), time.Hour)
	}
}

func TestRunner_DAGOrder(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	var order []string
	r := NewRunner(st)
	r.Handle("a", func(context.Context, store.Job) error { order = append(order, "a"); return nil })
	r.Handle("b", func(context.Context, store.Job) error { order = append(order, "b"); return nil })

	a, err := st.Jobs.Enqueue(ctx, store.EnqueueSpec{Type: "a"})
	if err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	if _, err := st.Jobs.Enqueue(ctx, store.EnqueueSpec{Type: "b", DependsOn: []string{a}}); err != nil {
		t.Fatalf("enqueue b: %v", err)
	}

	n, err := r.Drain(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 2 {
		t.Errorf("processed %d jobs, want 2", n)
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Errorf("execution order = %v, want [a b]", order)
	}
}

func TestRunner_RetryThenSucceed(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	r := NewRunner(st, WithBackoff(func(int) time.Duration { return 0 }), WithMaxAttempts(5))
	calls := 0
	r.Handle("flaky", func(context.Context, store.Job) error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})

	id, err := st.Jobs.Enqueue(ctx, store.EnqueueSpec{Type: "flaky"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := r.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	job, err := st.Jobs.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Status != "done" {
		t.Errorf("status = %q, want done", job.Status)
	}
	if job.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", job.Attempts)
	}
}

func TestRunner_ExhaustsRetriesThenFails(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	r := NewRunner(st, WithBackoff(func(int) time.Duration { return 0 }), WithMaxAttempts(3))
	r.Handle("bad", func(context.Context, store.Job) error { return errors.New("always") })

	id, err := st.Jobs.Enqueue(ctx, store.EnqueueSpec{Type: "bad"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := r.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	job, err := st.Jobs.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Status != "failed" {
		t.Errorf("status = %q, want failed", job.Status)
	}
	if job.Attempts != 3 {
		t.Errorf("attempts = %d, want 3 (maxAttempts)", job.Attempts)
	}
	if job.LastError == nil || *job.LastError != "always" {
		t.Errorf("last_error = %v, want \"always\"", job.LastError)
	}
}

func TestRunner_MaxAttemptsOne_RunsExactlyOnce(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	r := NewRunner(st, WithMaxAttempts(1), WithBackoff(func(int) time.Duration { return 0 }))
	calls := 0
	r.Handle("bad", func(context.Context, store.Job) error {
		calls++
		return errors.New("nope")
	})

	id, err := st.Jobs.Enqueue(ctx, store.EnqueueSpec{Type: "bad"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := r.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	job, err := st.Jobs.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if calls != 1 {
		t.Errorf("handler called %d times, want exactly 1 (no off-by-one)", calls)
	}
	if job.Status != "failed" || job.Attempts != 1 {
		t.Errorf("job status=%q attempts=%d, want failed/1", job.Status, job.Attempts)
	}
}

func TestRunner_CascadeFailsDependents(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	r := NewRunner(st, WithMaxAttempts(1), WithBackoff(func(int) time.Duration { return 0 }))
	r.Handle("a", func(context.Context, store.Job) error { return errors.New("boom") })
	bRan := false
	r.Handle("b", func(context.Context, store.Job) error { bRan = true; return nil })

	a, err := st.Jobs.Enqueue(ctx, store.EnqueueSpec{Type: "a"})
	if err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	b, err := st.Jobs.Enqueue(ctx, store.EnqueueSpec{Type: "b", DependsOn: []string{a}})
	if err != nil {
		t.Fatalf("enqueue b: %v", err)
	}

	if _, err := r.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	ja, _ := st.Jobs.Get(ctx, a)
	jb, _ := st.Jobs.Get(ctx, b)
	if ja.Status != "failed" {
		t.Errorf("a status = %q, want failed", ja.Status)
	}
	if jb.Status != "failed" {
		t.Errorf("b status = %q, want failed (cascade)", jb.Status)
	}
	if bRan {
		t.Error("b's handler ran, but it should have been cascade-failed without running")
	}
}

func TestRunner_UnknownTypeFails(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	r := NewRunner(st)
	id, err := st.Jobs.Enqueue(ctx, store.EnqueueSpec{Type: "no-handler"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := r.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	job, err := st.Jobs.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Status != "failed" {
		t.Errorf("status = %q, want failed (no handler)", job.Status)
	}
}

func TestRunner_Run_ProcessesEnqueued(t *testing.T) {
	st := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := NewRunner(st)
	done := make(chan string, 1)
	r.Handle("x", func(_ context.Context, j store.Job) error { done <- j.ID; return nil })

	id, err := st.Jobs.Enqueue(context.Background(), store.EnqueueSpec{Type: "x"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	go func() { _ = r.Run(ctx, nil, 5*time.Millisecond) }()

	select {
	case got := <-done:
		if got != id {
			t.Errorf("processed job %s, want %s", got, id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run loop did not process the enqueued job")
	}
}

func TestRunner_ResumeAfterCrash(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	id, err := st.Jobs.Enqueue(ctx, store.EnqueueSpec{Type: "ingest"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Claim leaves the job 'running' and is never completed — a crash.
	if _, err := st.Jobs.Claim(ctx, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Restart: a fresh runner reclaims orphaned jobs, then drains.
	r := NewRunner(st)
	ran := false
	r.Handle("ingest", func(context.Context, store.Job) error { ran = true; return nil })

	reclaimed, err := r.ReclaimStuck(ctx)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if reclaimed != 1 {
		t.Errorf("reclaimed %d, want 1", reclaimed)
	}
	if _, err := r.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	job, err := st.Jobs.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ran || job.Status != "done" {
		t.Errorf("after resume: ran=%v status=%q, want ran=true status=done", ran, job.Status)
	}
}

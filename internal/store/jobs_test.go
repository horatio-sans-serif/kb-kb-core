package store

import (
	"context"
	"errors"
	"testing"
)

func TestJobs_TransactionalEnqueue(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	boom := errors.New("boom")

	err := st.Tx(ctx, func(tx *Store) error {
		if _, err := tx.Jobs.Enqueue(ctx, EnqueueSpec{Type: "ingest"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx error = %v, want boom", err)
	}

	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if n != 0 {
		t.Errorf("rolled-back enqueue left %d jobs, want 0", n)
	}
}

func TestJobs_ClaimReadyAndComplete(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, err := st.Jobs.Enqueue(ctx, EnqueueSpec{Type: "ingest"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	job, err := st.Jobs.Claim(ctx, now())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job == nil {
		t.Fatal("claim returned nil for a ready job")
	}
	if job.ID != id || job.Status != "running" || job.Attempts != 1 {
		t.Errorf("claimed job = %+v, want id=%s status=running attempts=1", job, id)
	}

	if err := st.Jobs.MarkDone(ctx, id); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	again, err := st.Jobs.Claim(ctx, now())
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if again != nil {
		t.Errorf("claim returned %+v after the only job was done, want nil", again)
	}
}

func TestJobs_ClaimBlockedByDependency(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	a, err := st.Jobs.Enqueue(ctx, EnqueueSpec{Type: "a"})
	if err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	b, err := st.Jobs.Enqueue(ctx, EnqueueSpec{Type: "b", DependsOn: []string{a}})
	if err != nil {
		t.Fatalf("enqueue b: %v", err)
	}

	first, err := st.Jobs.Claim(ctx, now())
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if first == nil || first.ID != a {
		t.Fatalf("first claim = %v, want job a (%s); b must be blocked", first, a)
	}

	blocked, err := st.Jobs.Claim(ctx, now())
	if err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if blocked != nil {
		t.Errorf("claim returned %s while its dependency is unfinished, want nil", blocked.ID)
	}

	if err := st.Jobs.MarkDone(ctx, a); err != nil {
		t.Fatalf("mark a done: %v", err)
	}
	unblocked, err := st.Jobs.Claim(ctx, now())
	if err != nil {
		t.Fatalf("claim 3: %v", err)
	}
	if unblocked == nil || unblocked.ID != b {
		t.Errorf("after dependency done, claim = %v, want job b (%s)", unblocked, b)
	}
}

func TestJobs_ReclaimStuck(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, err := st.Jobs.Enqueue(ctx, EnqueueSpec{Type: "ingest"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Claim leaves it 'running' (simulating a crash before completion).
	if _, err := st.Jobs.Claim(ctx, now()); err != nil {
		t.Fatalf("claim: %v", err)
	}

	n, err := st.Jobs.ReclaimStuck(ctx)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 1 {
		t.Errorf("reclaimed %d jobs, want 1", n)
	}

	job, err := st.Jobs.Claim(ctx, now())
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if job == nil || job.ID != id {
		t.Errorf("reclaimed job not claimable again: %v", job)
	}
}

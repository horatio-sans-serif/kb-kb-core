package store

import (
	"context"
	"errors"
	"sync"
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

	n, err := st.Jobs.ReclaimStuck(ctx, now())
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

func TestJobs_CascadeFailOnDependencyFailure(t *testing.T) {
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
	c, err := st.Jobs.Enqueue(ctx, EnqueueSpec{Type: "c", DependsOn: []string{b}})
	if err != nil {
		t.Fatalf("enqueue c: %v", err)
	}

	if _, err := st.Jobs.Claim(ctx, now()); err != nil { // claims a (→running)
		t.Fatalf("claim: %v", err)
	}
	if err := st.Jobs.MarkFailed(ctx, a, "boom"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	// b (direct) and c (transitive) must both be cascade-failed.
	for _, id := range []string{b, c} {
		j, err := st.Jobs.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if j.Status != "failed" {
			t.Errorf("job %s status = %q, want failed (cascade)", id, j.Status)
		}
	}
}

func TestJobs_Blocked(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	b, err := st.Jobs.Enqueue(ctx, EnqueueSpec{Type: "b", DependsOn: []string{"01MISSINGDEP"}})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	blocked, err := st.Jobs.Blocked(ctx)
	if err != nil {
		t.Fatalf("blocked: %v", err)
	}
	if len(blocked) != 1 || blocked[0].ID != b {
		t.Errorf("blocked = %v, want [%s] (missing dependency)", blocked, b)
	}
}

func TestJobs_ReclaimByLease(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, err := st.Jobs.Enqueue(ctx, EnqueueSpec{Type: "x"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := st.Jobs.Claim(ctx, "2026-05-31T12:00:00Z"); err != nil { // stamps claimed_at
		t.Fatalf("claim: %v", err)
	}

	n, err := st.Jobs.ReclaimStuck(ctx, "2026-05-31T11:00:00Z") // cutoff before claim
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 0 {
		t.Errorf("reclaimed %d with cutoff before the claim, want 0 (lease not expired)", n)
	}

	n, err = st.Jobs.ReclaimStuck(ctx, "2026-05-31T13:00:00Z") // cutoff after claim
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 1 {
		t.Errorf("reclaimed %d with cutoff after the claim, want 1", n)
	}
}

func TestJobs_Reschedule(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, err := st.Jobs.Enqueue(ctx, EnqueueSpec{Type: "x"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := st.Jobs.Claim(ctx, now()); err != nil { // -> running
		t.Fatalf("claim: %v", err)
	}

	future := "2999-01-01T00:00:00Z"
	if err := st.Jobs.Reschedule(ctx, id, &future, "boom"); err != nil {
		t.Fatalf("reschedule: %v", err)
	}
	job, err := st.Jobs.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Status != "pending" || job.RunAfter == nil || *job.RunAfter != future ||
		job.LastError == nil || *job.LastError != "boom" {
		t.Errorf("job = %+v, want pending/run_after=future/last_error=boom", job)
	}
	if c, _ := st.Jobs.Claim(ctx, now()); c != nil {
		t.Errorf("claimed a future-scheduled job: %+v", c)
	}
}

func TestJobs_GetCorruptRow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO jobs(id, type, depends_on, status, created_at)
		 VALUES ('01BAD', 'x', 'not json', 'pending', '2026-01-01')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.Jobs.Get(ctx, "01BAD"); err == nil {
		t.Error("expected an error parsing a malformed depends_on blob")
	}
}

func TestJobs_Purge(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	rows := []struct{ id, status, created string }{
		{"01OLDDONE", "done", "2020-01-01T00:00:00Z"},
		{"01OLDFAIL", "failed", "2020-01-01T00:00:00Z"},
		{"01NEWDONE", "done", "2999-01-01T00:00:00Z"},
		{"01PENDING", "pending", "2020-01-01T00:00:00Z"},
	}
	for _, r := range rows {
		if _, err := st.db.ExecContext(ctx,
			`INSERT INTO jobs(id, type, depends_on, status, created_at) VALUES (?, 'x', '[]', ?, ?)`,
			r.id, r.status, r.created); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	n, err := st.Jobs.Purge(ctx, "2021-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 2 {
		t.Errorf("purged %d, want 2 (old done + old failed only)", n)
	}
	for _, id := range []string{"01NEWDONE", "01PENDING"} {
		if _, err := st.Jobs.Get(ctx, id); err != nil {
			t.Errorf("%s should survive purge: %v", id, err)
		}
	}
}

func TestJobs_ClaimFIFOOrder(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	const n = 10
	ids := make([]string, n)
	for i := range ids {
		id, err := st.Jobs.Enqueue(ctx, EnqueueSpec{Type: "x"})
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		ids[i] = id
	}
	for i := range ids {
		job, err := st.Jobs.Claim(ctx, now())
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if job == nil || job.ID != ids[i] {
			t.Fatalf("claim %d = %v, want %s (FIFO by created_at,id)", i, job, ids[i])
		}
		if err := st.Jobs.MarkDone(ctx, job.ID); err != nil {
			t.Fatalf("mark done: %v", err)
		}
	}
}

func TestJobs_ConcurrentClaim_NoDoubleDispatch(t *testing.T) {
	st := newTestStore(t)
	st.db.SetMaxOpenConns(8) // override the single-writer pool to force real concurrency
	ctx := context.Background()

	const m = 50
	for range m {
		if _, err := st.Jobs.Enqueue(ctx, EnqueueSpec{Type: "x"}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	var mu sync.Mutex
	seen := make(map[string]int)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				job, err := st.Jobs.Claim(ctx, now())
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if job == nil {
					return
				}
				mu.Lock()
				seen[job.ID]++
				mu.Unlock()
				if err := st.Jobs.MarkDone(ctx, job.ID); err != nil {
					t.Errorf("mark done: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if len(seen) != m {
		t.Errorf("claimed %d distinct jobs, want %d", len(seen), m)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("job %s claimed %d times, want exactly 1", id, c)
		}
	}
}

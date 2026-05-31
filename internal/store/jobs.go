package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Job is one unit of the enrichment pipeline (design 6.11). The jobs table is
// local-only runtime state — it is not synced (6.0).
type Job struct {
	ID         string
	Type       string
	ArtifactID *string
	DependsOn  []string // job ids that must be 'done' before this runs
	Payload    string   // JSON
	Status     string   // pending | running | done | failed
	Attempts   int
	LastError  *string
	RunAfter   *string // ISO timestamp; claimable only when now >= run_after
	CreatedAt  string
}

// EnqueueSpec describes a job to enqueue.
type EnqueueSpec struct {
	Type       string
	ArtifactID *string
	DependsOn  []string
	Payload    string
	RunAfter   *string
}

type JobRepo struct{ q Querier }

// jobCols is the column list for SELECT/RETURNING; scanJob depends on this exact
// order. claimed_at is intentionally omitted — it is internal lease state.
const jobCols = `id, type, artifact_id, depends_on, payload, status, attempts, last_error, run_after, created_at`

// Enqueue inserts a pending job. Call it inside Store.Tx alongside the write
// that creates the work (transactional enqueue, design Decision 4).
func (r *JobRepo) Enqueue(ctx context.Context, spec EnqueueSpec) (string, error) {
	id := NewID()
	deps := "[]"
	if len(spec.DependsOn) > 0 {
		b, err := json.Marshal(spec.DependsOn)
		if err != nil {
			return "", fmt.Errorf("marshal depends_on: %w", err)
		}
		deps = string(b)
	}
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO jobs(id, type, artifact_id, depends_on, payload, status, run_after, created_at)
		 VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)`,
		id, spec.Type, spec.ArtifactID, deps, nullIfEmpty(spec.Payload), spec.RunAfter, now())
	if err != nil {
		return "", fmt.Errorf("enqueue job: %w", err)
	}
	return id, nil
}

// Claim atomically selects the oldest runnable job — pending, past its run_after,
// and with every dependency 'done' — marks it running, stamps claimed_at,
// increments attempts, and returns it. Returns (nil, nil) when nothing is
// runnable. The single UPDATE ... RETURNING makes claiming race-free across
// concurrent workers (verified with a multi-connection pool).
//
// A job whose dependency is 'failed' or references a missing id is never
// runnable; cascade-fail (MarkFailed) handles the former and Blocked surfaces
// the latter.
func (r *JobRepo) Claim(ctx context.Context, nowTS string) (*Job, error) {
	row := r.q.QueryRowContext(ctx,
		`UPDATE jobs SET status = 'running', attempts = attempts + 1, claimed_at = ?
		 WHERE id = (
		   SELECT j.id FROM jobs j
		   WHERE j.status = 'pending'
		     AND (j.run_after IS NULL OR j.run_after <= ?)
		     AND NOT EXISTS (
		       SELECT 1 FROM json_each(j.depends_on) dep
		       LEFT JOIN jobs d ON d.id = dep.value
		       WHERE d.status IS NULL OR d.status <> 'done')
		   ORDER BY j.created_at, j.id
		   LIMIT 1)
		 RETURNING `+jobCols, nowTS, nowTS)

	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // nothing runnable
	}
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	return job, nil
}

// MarkDone marks a running job complete. Scoped to status='running' so it cannot
// resurrect a terminal job; a lost claim (0 rows) is a no-op.
func (r *JobRepo) MarkDone(ctx context.Context, id string) error {
	if _, err := r.q.ExecContext(ctx,
		`UPDATE jobs SET status = 'done', claimed_at = NULL WHERE id = ? AND status = 'running'`, id); err != nil {
		return fmt.Errorf("mark job %q done: %w", id, err)
	}
	return nil
}

// Reschedule returns a running job to pending for retry, recording the error and
// an optional run_after (nil = immediately runnable). Scoped to status='running'.
func (r *JobRepo) Reschedule(ctx context.Context, id string, runAfter *string, errMsg string) error {
	if _, err := r.q.ExecContext(ctx,
		`UPDATE jobs SET status = 'pending', run_after = ?, last_error = ?, claimed_at = NULL
		 WHERE id = ? AND status = 'running'`,
		runAfter, errMsg, id); err != nil {
		return fmt.Errorf("reschedule job %q: %w", id, err)
	}
	return nil
}

// MarkFailed marks a running job permanently failed, then cascade-fails any
// pending jobs that (transitively) depend on a failed job, so a broken stage
// does not silently strand its downstream subtree as pending forever.
func (r *JobRepo) MarkFailed(ctx context.Context, id, errMsg string) error {
	if _, err := r.q.ExecContext(ctx,
		`UPDATE jobs SET status = 'failed', last_error = ?, claimed_at = NULL
		 WHERE id = ? AND status = 'running'`, errMsg, id); err != nil {
		return fmt.Errorf("mark job %q failed: %w", id, err)
	}
	return r.cascadeFailDependents(ctx)
}

// cascadeFailDependents repeatedly fails pending jobs that have a failed
// dependency until a fixpoint (the pending set strictly shrinks each pass).
func (r *JobRepo) cascadeFailDependents(ctx context.Context) error {
	for {
		res, err := r.q.ExecContext(ctx,
			`UPDATE jobs SET status = 'failed', last_error = 'dependency failed', claimed_at = NULL
			 WHERE status = 'pending' AND EXISTS (
			   SELECT 1 FROM json_each(jobs.depends_on) dep
			   JOIN jobs d ON d.id = dep.value
			   WHERE d.status = 'failed')`)
		if err != nil {
			return fmt.Errorf("cascade-fail dependents: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("cascade-fail dependents (rows affected): %w", err)
		}
		if n == 0 {
			return nil
		}
	}
}

// ReclaimStuck returns 'running' jobs claimed at or before olderThan to
// 'pending' (clearing the lease and run_after). On startup, pass the current
// time to reclaim every running job (no worker survived a crash). A continuous
// runner (Stage 5) passes now-minus-lease so it does not reclaim jobs a live
// worker still holds. Attempts are preserved: a crash consumes one retry.
func (r *JobRepo) ReclaimStuck(ctx context.Context, olderThan string) (int, error) {
	res, err := r.q.ExecContext(ctx,
		`UPDATE jobs SET status = 'pending', run_after = NULL, claimed_at = NULL
		 WHERE status = 'running' AND (claimed_at IS NULL OR claimed_at <= ?)`, olderThan)
	if err != nil {
		return 0, fmt.Errorf("reclaim stuck jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reclaim stuck jobs (rows affected): %w", err)
	}
	return int(n), nil
}

// Blocked returns pending jobs that can never become runnable because a
// dependency is 'failed' or references a missing job id. Cascade-fail converts
// the failed-dependency case to failed, so in practice this surfaces the
// missing-dependency (mis-enqueue) case for monitoring.
func (r *JobRepo) Blocked(ctx context.Context) ([]*Job, error) {
	rows, err := r.q.QueryContext(ctx,
		`SELECT `+jobCols+` FROM jobs j WHERE j.status = 'pending' AND EXISTS (
		   SELECT 1 FROM json_each(j.depends_on) dep
		   LEFT JOIN jobs d ON d.id = dep.value
		   WHERE d.id IS NULL OR d.status = 'failed')`)
	if err != nil {
		return nil, fmt.Errorf("query blocked jobs: %w", err)
	}
	defer rows.Close()

	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (r *JobRepo) Get(ctx context.Context, id string) (*Job, error) {
	job, err := scanJob(r.q.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get job %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get job %q: %w", id, err)
	}
	return job, nil
}

func scanJob(s scanner) (*Job, error) {
	var j Job
	var deps string
	var payload sql.NullString
	if err := s.Scan(
		&j.ID, &j.Type, &j.ArtifactID, &deps, &payload,
		&j.Status, &j.Attempts, &j.LastError, &j.RunAfter, &j.CreatedAt,
	); err != nil {
		return nil, err
	}
	j.Payload = payload.String
	if deps != "" {
		if err := json.Unmarshal([]byte(deps), &j.DependsOn); err != nil {
			return nil, fmt.Errorf("parse depends_on for job %q: %w", j.ID, err)
		}
	}
	return &j, nil
}

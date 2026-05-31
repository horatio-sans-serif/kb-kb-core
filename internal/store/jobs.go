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
// and with every dependency 'done' — marks it running, increments attempts, and
// returns it. Returns (nil, nil) when nothing is runnable. The single UPDATE ...
// RETURNING makes claiming race-free across workers.
func (r *JobRepo) Claim(ctx context.Context, nowTS string) (*Job, error) {
	row := r.q.QueryRowContext(ctx,
		`UPDATE jobs SET status = 'running', attempts = attempts + 1
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
		 RETURNING `+jobCols, nowTS)

	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // nothing runnable
	}
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	return job, nil
}

// MarkDone marks a job complete.
func (r *JobRepo) MarkDone(ctx context.Context, id string) error {
	if _, err := r.q.ExecContext(ctx, `UPDATE jobs SET status = 'done' WHERE id = ?`, id); err != nil {
		return fmt.Errorf("mark job %q done: %w", id, err)
	}
	return nil
}

// Reschedule returns a job to pending for retry, recording the error and an
// optional run_after (nil = immediately runnable).
func (r *JobRepo) Reschedule(ctx context.Context, id string, runAfter *string, errMsg string) error {
	if _, err := r.q.ExecContext(ctx,
		`UPDATE jobs SET status = 'pending', run_after = ?, last_error = ? WHERE id = ?`,
		runAfter, errMsg, id); err != nil {
		return fmt.Errorf("reschedule job %q: %w", id, err)
	}
	return nil
}

// MarkFailed marks a job permanently failed with the final error.
func (r *JobRepo) MarkFailed(ctx context.Context, id, errMsg string) error {
	if _, err := r.q.ExecContext(ctx,
		`UPDATE jobs SET status = 'failed', last_error = ? WHERE id = ?`, errMsg, id); err != nil {
		return fmt.Errorf("mark job %q failed: %w", id, err)
	}
	return nil
}

// ReclaimStuck returns all 'running' jobs to 'pending'. Called on startup: with
// no live worker, any 'running' job was orphaned by a crash. Attempts are
// preserved so the retry budget accounts for the crash.
func (r *JobRepo) ReclaimStuck(ctx context.Context) (int, error) {
	res, err := r.q.ExecContext(ctx, `UPDATE jobs SET status = 'pending' WHERE status = 'running'`)
	if err != nil {
		return 0, fmt.Errorf("reclaim stuck jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reclaim stuck jobs (rows affected): %w", err)
	}
	return int(n), nil
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

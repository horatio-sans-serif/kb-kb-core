// Package jobs is the durable, dependency-aware enrichment runner over the
// store's local-only jobs table (design Section 5 / Principle 4). It claims
// runnable jobs, dispatches them to per-type handlers, retries with backoff, and
// reclaims jobs orphaned by a crash.
package jobs

import (
	"context"
	"fmt"
	"time"

	"kb/internal/store"
)

// Handler processes one job. Returning an error reschedules the job (with
// backoff) until the attempt budget is exhausted, after which it is marked
// failed; the error never stops the runner.
type Handler func(ctx context.Context, job store.Job) error

// Runner dispatches jobs to handlers.
type Runner struct {
	store       *store.Store
	handlers    map[string]Handler
	maxAttempts int
	backoff     func(attempts int) time.Duration
}

// Option configures a Runner.
type Option func(*Runner)

// WithMaxAttempts sets how many times a job may run before being marked failed.
func WithMaxAttempts(n int) Option { return func(r *Runner) { r.maxAttempts = n } }

// WithBackoff sets the delay-before-retry function (input: attempts so far).
func WithBackoff(f func(attempts int) time.Duration) Option {
	return func(r *Runner) { r.backoff = f }
}

// NewRunner builds a Runner with sensible defaults (5 attempts, exponential
// backoff from 1s capped at 1h).
func NewRunner(s *store.Store, opts ...Option) *Runner {
	r := &Runner{
		store:       s,
		handlers:    make(map[string]Handler),
		maxAttempts: 5,
		backoff:     ExpBackoff(time.Second, time.Hour),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Handle registers the handler for a job type. The last registration wins.
func (r *Runner) Handle(jobType string, h Handler) { r.handlers[jobType] = h }

// RunOnce claims and processes a single runnable job. The bool reports whether a
// job was claimed (false = nothing runnable right now). A handler failure is
// recorded on the job, not returned; only infrastructure errors are returned.
func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	job, err := r.store.Jobs.Claim(ctx, nowTS())
	if err != nil {
		return false, err
	}
	if job == nil {
		return false, nil
	}
	if err := r.process(ctx, *job); err != nil {
		return true, err
	}
	return true, nil
}

// Drain runs jobs until none is currently runnable, returning the count
// processed. Jobs rescheduled into the future (backoff) are left for later.
func (r *Runner) Drain(ctx context.Context) (int, error) {
	n := 0
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		claimed, err := r.RunOnce(ctx)
		if err != nil {
			return n, err
		}
		if !claimed {
			return n, nil
		}
		n++
	}
}

// ReclaimStuck returns jobs left 'running' by a crash to 'pending'. Call once on
// startup before draining.
func (r *Runner) ReclaimStuck(ctx context.Context) (int, error) {
	return r.store.Jobs.ReclaimStuck(ctx)
}

func (r *Runner) process(ctx context.Context, job store.Job) error {
	h, ok := r.handlers[job.Type]
	if !ok {
		return r.store.Jobs.MarkFailed(ctx, job.ID, fmt.Sprintf("no handler for job type %q", job.Type))
	}

	if herr := h(ctx, job); herr != nil {
		// job.Attempts was incremented by Claim and counts this run.
		if job.Attempts >= r.maxAttempts {
			return r.store.Jobs.MarkFailed(ctx, job.ID, herr.Error())
		}
		var runAfter *string
		if d := r.backoff(job.Attempts); d > 0 {
			ts := time.Now().Add(d).UTC().Format(time.RFC3339Nano)
			runAfter = &ts
		}
		return r.store.Jobs.Reschedule(ctx, job.ID, runAfter, herr.Error())
	}
	return r.store.Jobs.MarkDone(ctx, job.ID)
}

func nowTS() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// ExpBackoff returns an exponential backoff (base * 2^(attempts-1)) capped at
// max. Overflow-safe.
func ExpBackoff(base, max time.Duration) func(attempts int) time.Duration {
	return func(attempts int) time.Duration {
		if attempts < 1 {
			attempts = 1
		}
		if attempts > 62 { // shifting past the width of int64 would overflow
			return max
		}
		d := base << (attempts - 1)
		if d <= 0 || d > max {
			return max
		}
		return d
	}
}

// Package jobs is the durable, dependency-aware enrichment runner over the
// store's local-only jobs table (design Section 5 / Principle 4). It claims
// runnable jobs, dispatches them to per-type handlers, retries with backoff, and
// reclaims jobs orphaned by a crash.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"kb/internal/store"
)

// Handler processes one job. Returning an error reschedules the job (with
// backoff) until the attempt budget is exhausted, after which it is marked
// failed (cascade-failing its dependents); the error never stops the runner.
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

// Handle registers the handler for a job type. Handlers are wired once at
// startup; a duplicate registration is a programming error and panics.
func (r *Runner) Handle(jobType string, h Handler) {
	if _, dup := r.handlers[jobType]; dup {
		panic(fmt.Sprintf("jobs: handler for type %q already registered", jobType))
	}
	r.handlers[jobType] = h
}

// RunOnce claims and processes a single runnable job. The bool reports whether a
// job was claimed (false = nothing runnable right now). A handler failure is
// recorded on the job, not returned; only infrastructure errors are returned
// (with the bool indicating whether a job had been claimed first).
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
// processed and any infrastructure errors joined together. A per-job infra error
// (e.g. a failed status write) is recorded and draining continues, so one
// artifact's hiccup does not halt the queue; a claim error (likely persistent,
// e.g. the DB is down) stops the drain. Context cancellation always stops it.
// Jobs rescheduled into the future (backoff) are left for a later drain.
func (r *Runner) Drain(ctx context.Context) (int, error) {
	n := 0
	var errs error
	for {
		if err := ctx.Err(); err != nil {
			return n, errors.Join(errs, err)
		}
		claimed, err := r.RunOnce(ctx)
		if err != nil {
			errs = errors.Join(errs, err)
			if !claimed {
				return n, errs // claim itself failed — stop rather than spin
			}
			n++ // a job was claimed; its failure is recorded, keep draining others
			continue
		}
		if !claimed {
			return n, errs
		}
		n++
	}
}

// ReclaimStuck returns jobs left 'running' by a crash to 'pending'. Call once on
// startup before draining: with no live worker, every running row is orphaned.
//
// TODO(Stage 5): a continuous runner must not reclaim jobs a live worker still
// holds — switch to a lease cutoff (now - lease) using the claimed_at column,
// rather than reclaiming everything.
func (r *Runner) ReclaimStuck(ctx context.Context) (int, error) {
	return r.store.Jobs.ReclaimStuck(ctx, nowTS())
}

// Run reclaims jobs orphaned by a prior crash once, then drains the queue on
// each interval tick until ctx is cancelled. A transient infra error from a
// drain is swallowed so the loop keeps running; only context cancellation (or a
// failed initial reclaim) returns.
func (r *Runner) Run(ctx context.Context, interval time.Duration) error {
	if _, err := r.ReclaimStuck(ctx); err != nil {
		return err
	}
	for {
		if _, err := r.Drain(ctx); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (r *Runner) process(ctx context.Context, job store.Job) error {
	h, ok := r.handlers[job.Type]
	if !ok {
		return r.store.Jobs.MarkFailed(ctx, job.ID, fmt.Sprintf("no handler for job type %q", job.Type))
	}

	if herr := h(ctx, job); herr != nil {
		// job.Attempts was incremented by Claim and counts this run.
		if job.Attempts >= r.maxAttempts {
			if err := r.store.Jobs.MarkFailed(ctx, job.ID, herr.Error()); err != nil {
				return fmt.Errorf("mark job %s failed after handler error %q: %w", job.ID, herr, err)
			}
			return nil
		}
		var runAfter *string
		if d := r.backoff(job.Attempts); d > 0 {
			ts := tsFormat(time.Now().Add(d))
			runAfter = &ts
		}
		if err := r.store.Jobs.Reschedule(ctx, job.ID, runAfter, herr.Error()); err != nil {
			return fmt.Errorf("reschedule job %s after handler error %q: %w", job.ID, herr, err)
		}
		return nil
	}
	if err := r.store.Jobs.MarkDone(ctx, job.ID); err != nil {
		return fmt.Errorf("mark job %s done: %w", job.ID, err)
	}
	return nil
}

// tsFormat is the single timestamp formatter for all run_after/claim writes, so
// the string comparison in Claim/ReclaimStuck can never break on a format drift.
func tsFormat(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func nowTS() string { return tsFormat(time.Now()) }

// ExpBackoff returns an exponential backoff (base * 2^(attempts-1)) capped at
// max. base<=0 falls back to 1s and max<base is raised to base, so the result is
// always positive and monotonic; overflow is clamped to max.
func ExpBackoff(base, max time.Duration) func(attempts int) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if max < base {
		max = base
	}
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

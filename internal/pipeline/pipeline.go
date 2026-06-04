// Package pipeline is the enrichment DAG above the dumb job runner (design §5 /
// §7). Enrichers declare their stage dependencies; the pipeline topologically
// expands an artifact into one job per stage with the queue's job-id
// depends_on wired from each enricher's DependsOn, and wraps each enricher as a
// job handler that enforces the privacy gate before dispatch.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"kb/internal/events"
	"kb/internal/jobs"
	"kb/internal/kb"
	"kb/internal/store"
)

// RescoreJobType is the job type for a standalone relevance re-score (§9). It is
// deliberately NOT a pipeline stage name: a re-score updates an already-enriched
// artifact's score and must not participate in enrichment-completion tracking
// (finalize and ArtifactPipelineState both ignore it), so it can never re-fire a
// lifecycle event or flip an artifact's terminal status.
const RescoreJobType = "rescore"

// Pipeline holds the registered enrichment stages.
type Pipeline struct {
	enrichers    map[string]kb.Enricher
	order        []string // registration order, for stable expansion
	rescoreStage string   // which stage's handler runs a re-score job (see SetRescoreStage)
}

// New returns an empty pipeline.
func New() *Pipeline {
	return &Pipeline{enrichers: make(map[string]kb.Enricher)}
}

// SetRescoreStage designates the registered stage whose handler runs standalone
// re-score jobs (the scoring stage, e.g. "relevance"). When set, RegisterHandlers
// wires that handler under RescoreJobType. A re-score re-runs only that stage.
func (p *Pipeline) SetRescoreStage(name string) { p.rescoreStage = name }

// Register adds an enricher (one pipeline stage). An empty name, the reserved
// '@' prefix (held for built-in pseudo-stages), or a duplicate stage name is a
// wiring bug and panics.
func (p *Pipeline) Register(e kb.Enricher) {
	name := e.Name()
	if name == "" {
		panic("pipeline: enricher has an empty name")
	}
	if name[0] == '@' {
		panic(fmt.Sprintf("pipeline: stage name %q uses the reserved '@' prefix", name))
	}
	if _, dup := p.enrichers[name]; dup {
		panic(fmt.Sprintf("pipeline: enricher %q already registered", name))
	}
	p.enrichers[name] = e
	p.order = append(p.order, name)
}

// Validate reports whether every DependsOn names a registered stage and the DAG
// is acyclic.
func (p *Pipeline) Validate() error {
	_, err := p.topoOrder()
	return err
}

// Enqueue runs EnqueueTx in its own transaction.
func (p *Pipeline) Enqueue(ctx context.Context, st *store.Store, artifactID string) error {
	return st.Tx(ctx, func(tx *store.Store) error {
		return p.EnqueueTx(ctx, tx, artifactID)
	})
}

// EnqueueTx expands the artifact into one job per stage within an existing
// transaction, wiring each job's depends_on to the job ids of the stages it
// depends on, and marks the artifact 'enriching'. Call it from the ingest
// transaction so the artifact, its encounter, and its pipeline are one atomic
// unit. With no registered stages the artifact has no work to do and is marked
// 'ready' immediately. Terminal status (ready/failed) for a non-empty pipeline
// is set later by the runner's on-complete hook (see finalize), not here.
func (p *Pipeline) EnqueueTx(ctx context.Context, tx *store.Store, artifactID string) error {
	order, err := p.topoOrder()
	if err != nil {
		return err
	}
	if len(order) == 0 {
		ready := kb.StatusReady
		return tx.Artifacts.Patch(ctx, artifactID, store.ArtifactPatch{Status: &ready})
	}
	enriching := kb.StatusEnriching
	if err := tx.Artifacts.Patch(ctx, artifactID, store.ArtifactPatch{Status: &enriching}); err != nil {
		return err
	}

	jobIDs := make(map[string]string, len(order)) // stage name -> job id
	for _, name := range order {
		deps := make([]string, 0, len(p.enrichers[name].DependsOn()))
		for _, dep := range p.enrichers[name].DependsOn() {
			deps = append(deps, jobIDs[dep])
		}
		id, err := tx.Jobs.Enqueue(ctx, store.EnqueueSpec{Type: name, ArtifactID: &artifactID, DependsOn: deps})
		if err != nil {
			return err
		}
		jobIDs[name] = id
	}
	return nil
}

// RegisterHandlers wires one job handler per stage into the runner and installs
// the finalize on-complete hook. Each handler loads the artifact, enforces the
// privacy gate (an enricher whose PrivacyOK rejects the tier is skipped without
// ever processing the artifact — the §8.17 floor: sensitive/restricted content
// never reaches a cloud enricher), runs Enrich, and persists the resulting
// enrichments.
func (p *Pipeline) RegisterHandlers(st *store.Store, runner *jobs.Runner, bus *events.Bus) {
	for _, name := range p.order {
		runner.Handle(name, p.handler(st, p.enrichers[name], bus))
	}
	// Re-score jobs reuse the designated scoring stage's handler under a distinct
	// type, so a re-score recomputes the score but stays out of completion
	// tracking (see RescoreJobType / finalize).
	if p.rescoreStage != "" {
		e, ok := p.enrichers[p.rescoreStage]
		if !ok {
			panic(fmt.Sprintf("pipeline: rescore stage %q is not registered", p.rescoreStage))
		}
		runner.Handle(RescoreJobType, p.handler(st, e, bus))
	}
	runner.SetOnComplete(p.finalize(st, bus))
}

// finalize transitions an artifact to its terminal status once every one of its
// pipeline jobs is terminal. It runs after each job completes (the runner's
// on-complete hook): while any stage is still pending or running it is a no-op;
// when the last one finishes it sets status 'ready' (all stages done) or
// 'failed' (at least one stage failed) and publishes the matching lifecycle
// event.
//
// This replaces a former built-in "@ready" sentinel job that depended on every
// stage: because cascade-fail fails the *dependents* of a failed job, any
// upstream failure also failed "@ready", whose handler then never ran — leaving
// the artifact stuck in 'enriching'. Deriving the terminal state from the job
// table after each completion has no such gap: a failed stage's own completion
// is what triggers the finalize.
func (p *Pipeline) finalize(st *store.Store, bus *events.Bus) func(context.Context, store.Job, bool) error {
	return func(ctx context.Context, job store.Job, _ bool) error {
		if job.ArtifactID == nil {
			return nil // non-artifact job: nothing to finalize
		}
		if job.Type == RescoreJobType {
			return nil // a re-score is not part of enrichment completion (§9)
		}
		return finalizeArtifact(ctx, st, bus, *job.ArtifactID)
	}
}

// finalizeArtifact sets an artifact's terminal status once all its jobs are
// terminal (a no-op while any remain), publishing the lifecycle event. It is
// idempotent — re-running it on an already-terminal artifact just re-patches the
// same status — so both the on-complete hook and the startup reconciler use it.
func finalizeArtifact(ctx context.Context, st *store.Store, bus *events.Bus, artifactID string) error {
	// Exclude re-score jobs: only the original enrichment generation determines
	// the artifact's terminal status.
	pending, anyFailed, err := st.Jobs.ArtifactPipelineState(ctx, artifactID, RescoreJobType)
	if err != nil {
		return err
	}
	if pending > 0 {
		return nil // stages remain; the artifact is not terminal yet
	}
	status, evt := kb.StatusReady, events.ArtifactReady
	if anyFailed {
		status, evt = kb.StatusFailed, events.ArtifactFailed
	}
	if err := st.Artifacts.Patch(ctx, artifactID, store.ArtifactPatch{Status: &status}); err != nil {
		return err
	}
	bus.Publish(events.New(evt, map[string]string{"artifact_id": artifactID}))
	return nil
}

// Reconcile finalizes any artifact stuck in 'enriching' whose jobs are all
// terminal — recovering the rare crash between a job's atomic commit and its
// post-completion finalize hook. Call it once at startup AFTER ReclaimStuck, so a
// reclaimed (re-pending) job keeps its artifact out of this sweep until it
// genuinely finishes.
func (p *Pipeline) Reconcile(ctx context.Context, st *store.Store, bus *events.Bus) error {
	ids, err := st.Artifacts.EnrichingIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := finalizeArtifact(ctx, st, bus, id); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pipeline) handler(st *store.Store, e kb.Enricher, bus *events.Bus) jobs.Handler {
	return func(ctx context.Context, job store.Job) error {
		if job.ArtifactID == nil {
			return fmt.Errorf("pipeline: %s job %s has no artifact", e.Name(), job.ID)
		}
		a, err := st.Artifacts.Get(ctx, *job.ArtifactID)
		if err != nil {
			return fmt.Errorf("pipeline: load artifact for %s: %w", e.Name(), err)
		}

		// Resolve the stage's effects. The slow work (fetch, LLM) happens here,
		// OUTSIDE any transaction, so it never holds the single-writer connection.
		var res kb.EnrichResult
		if !e.PrivacyOK(a.PrivacyTier) {
			// Privacy gate: this enricher may not process this tier. Record the
			// skip as an auditable enrichment row — a deliberate no-op, not a
			// failure, so it must not fail the artifact.
			res = kb.EnrichResult{Enrichments: []kb.Enrichment{{
				ArtifactID:    a.ID,
				Kind:          "skipped",
				Payload:       skippedPayload(e.Name(), a.PrivacyTier),
				PromptVersion: "1",
			}}}
		} else if res, err = e.Enrich(ctx, a); err != nil {
			return err
		}

		// Commit the effects and the job's completion in ONE short transaction, so
		// they are atomic: a crash before commit re-runs the stage cleanly (nothing
		// was written), and a commit lands the rows together with the 'done' status
		// so a reclaim can never re-run a stage that already wrote — exactly-once
		// enrichment writes. (The runner's own MarkDone afterward is then a no-op.)
		if err := st.Tx(ctx, func(tx *store.Store) error {
			for i := range res.Enrichments {
				if err := tx.Enrichments.Create(ctx, &res.Enrichments[i]); err != nil {
					return err
				}
			}
			if res.Patch != nil {
				if err := tx.Artifacts.Patch(ctx, a.ID, *res.Patch); err != nil {
					return err
				}
			}
			for i := range res.Claims {
				if err := tx.Claims.Create(ctx, &res.Claims[i]); err != nil {
					return err
				}
			}
			for i := range res.Conflicts {
				if err := tx.Conflicts.Create(ctx, &res.Conflicts[i]); err != nil {
					return err
				}
			}
			// Graph mutations (entity links, edges) run in the same tx so they are
			// atomic with the stage's completion: a rolled-back attempt leaves no
			// graph rows, and only the committing attempt's effects ever land.
			if res.ApplyGraph != nil {
				if err := res.ApplyGraph(ctx, tx.Graph); err != nil {
					return err
				}
			}
			return tx.Jobs.MarkDone(ctx, job.ID)
		}); err != nil {
			return err
		}

		// After the effects commit, announce detected contradictions so the surfacing
		// layer can raise them for human resolution (§8.13). Published post-commit so
		// a subscriber never sees a conflict that a rolled-back tx didn't persist.
		if len(res.Conflicts) > 0 {
			bus.Publish(events.New(events.ConflictDetected, map[string]string{
				"artifact_id": a.ID,
				"count":       fmt.Sprintf("%d", len(res.Conflicts)),
			}))
		}
		return nil
	}
}

// skippedPayload is the JSON body recorded for a privacy-gated stage skip.
func skippedPayload(stage string, tier kb.PrivacyTier) string {
	b, err := json.Marshal(map[string]string{
		"stage": stage, "reason": "privacy_gate", "tier": string(tier),
	})
	if err != nil { // a string map cannot fail to marshal; defensive fallback
		return `{"reason":"privacy_gate"}`
	}
	return string(b)
}

// topoOrder returns the enrichers in a dependency-respecting order (Kahn's
// algorithm), stable by registration order, or an error for an unknown
// dependency or a cycle.
func (p *Pipeline) topoOrder() ([]string, error) {
	indeg := make(map[string]int, len(p.enrichers))
	for _, name := range p.order {
		for _, dep := range p.enrichers[name].DependsOn() {
			if _, ok := p.enrichers[dep]; !ok {
				return nil, fmt.Errorf("pipeline: stage %q depends on unregistered stage %q", name, dep)
			}
			indeg[name]++
		}
	}

	queue := make([]string, 0, len(p.order))
	for _, name := range p.order {
		if indeg[name] == 0 {
			queue = append(queue, name)
		}
	}

	order := make([]string, 0, len(p.order))
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		order = append(order, n)
		for _, name := range p.order { // reduce in-degree of n's dependents
			for _, dep := range p.enrichers[name].DependsOn() {
				if dep == n {
					if indeg[name]--; indeg[name] == 0 {
						queue = append(queue, name)
					}
				}
			}
		}
	}
	if len(order) != len(p.enrichers) {
		return nil, fmt.Errorf("pipeline: dependency cycle among enrichers")
	}
	return order, nil
}

// Package pipeline is the enrichment DAG above the dumb job runner (design §5 /
// §7). Enrichers declare their stage dependencies; the pipeline topologically
// expands an artifact into one job per stage with the queue's job-id
// depends_on wired from each enricher's DependsOn, and wraps each enricher as a
// job handler that enforces the privacy gate before dispatch.
package pipeline

import (
	"context"
	"fmt"

	"kb/internal/jobs"
	"kb/internal/kb"
	"kb/internal/store"
)

// Pipeline holds the registered enrichment stages.
type Pipeline struct {
	enrichers map[string]kb.Enricher
	order     []string // registration order, for stable expansion
}

// New returns an empty pipeline.
func New() *Pipeline {
	return &Pipeline{enrichers: make(map[string]kb.Enricher)}
}

// Register adds an enricher (one pipeline stage). A duplicate stage name is a
// wiring bug and panics.
func (p *Pipeline) Register(e kb.Enricher) {
	name := e.Name()
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

// Enqueue creates one job per stage for the artifact in a single transaction,
// wiring each job's depends_on to the job ids of the stages it depends on.
func (p *Pipeline) Enqueue(ctx context.Context, st *store.Store, artifactID string) error {
	order, err := p.topoOrder()
	if err != nil {
		return err
	}
	return st.Tx(ctx, func(tx *store.Store) error {
		jobIDs := make(map[string]string, len(order)) // stage name -> job id
		for _, name := range order {
			deps := make([]string, 0, len(p.enrichers[name].DependsOn()))
			for _, dep := range p.enrichers[name].DependsOn() {
				deps = append(deps, jobIDs[dep])
			}
			id, err := tx.Jobs.Enqueue(ctx, store.EnqueueSpec{
				Type: name, ArtifactID: &artifactID, DependsOn: deps,
			})
			if err != nil {
				return err
			}
			jobIDs[name] = id
		}
		return nil
	})
}

// RegisterHandlers wires one job handler per stage into the runner. Each handler
// loads the artifact, enforces the privacy gate (an enricher whose PrivacyOK
// rejects the tier is skipped without ever processing the artifact — this is the
// §8.17 floor: sensitive/restricted content never reaches a cloud enricher),
// runs Enrich, and persists the resulting enrichments.
func (p *Pipeline) RegisterHandlers(st *store.Store, runner *jobs.Runner) {
	for _, name := range p.order {
		runner.Handle(name, p.handler(st, p.enrichers[name]))
	}
}

func (p *Pipeline) handler(st *store.Store, e kb.Enricher) jobs.Handler {
	return func(ctx context.Context, job store.Job) error {
		if job.ArtifactID == nil {
			return fmt.Errorf("pipeline: %s job %s has no artifact", e.Name(), job.ID)
		}
		a, err := st.Artifacts.Get(ctx, *job.ArtifactID)
		if err != nil {
			return fmt.Errorf("pipeline: load artifact for %s: %w", e.Name(), err)
		}
		if !e.PrivacyOK(a.PrivacyTier) {
			return nil // privacy gate: skip, never process this tier with this enricher
		}
		results, err := e.Enrich(ctx, a)
		if err != nil {
			return err
		}
		if len(results) == 0 {
			return nil
		}
		return st.Tx(ctx, func(tx *store.Store) error {
			for i := range results {
				if err := tx.Enrichments.Create(ctx, &results[i]); err != nil {
					return err
				}
			}
			return nil
		})
	}
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

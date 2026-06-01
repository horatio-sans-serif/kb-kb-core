// Package relsignals adapts the store into the relevance.Signals the relevance
// enricher consumes, keeping that enricher a pure plugin with no store import. It
// satisfies enrich.SignalProvider structurally.
package relsignals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"kb/internal/kb"
	"kb/internal/relevance"
	"kb/internal/store"
)

// Provider gathers an artifact's relevance signals from the store.
type Provider struct{ st *store.Store }

// New builds a Provider over the store.
func New(st *store.Store) *Provider { return &Provider{st: st} }

// Signals reads the artifact's embedding (the latest `embedding` enrichment), its
// encounter count, and its most-authoritative source weight. A missing embedding
// yields an empty vector — not an error — so the relevance stage simply no-ops
// until one exists.
func (p *Provider) Signals(ctx context.Context, artifactID string) (relevance.Signals, error) {
	var vec []float32
	e, err := p.st.Enrichments.LatestByKind(ctx, artifactID, "embedding")
	switch {
	case err == nil:
		var ep kb.EmbeddingPayload
		if uerr := json.Unmarshal([]byte(e.Payload), &ep); uerr != nil {
			return relevance.Signals{}, fmt.Errorf("relsignals: parse embedding for %q: %w", artifactID, uerr)
		}
		vec = ep.Vector
	case errors.Is(err, store.ErrNotFound):
		// no embedding yet — leave vec nil so the relevance stage no-ops
	default:
		return relevance.Signals{}, err
	}

	count, err := p.st.Encounters.CountForArtifact(ctx, artifactID)
	if err != nil {
		return relevance.Signals{}, err
	}
	weight, err := p.st.Encounters.MaxSourceWeightForArtifact(ctx, artifactID)
	if err != nil {
		return relevance.Signals{}, err
	}
	return relevance.Signals{Embedding: vec, Encounters: count, SourceWeight: weight}, nil
}

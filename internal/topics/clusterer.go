// Package topics is the topic-clustering engine (design §8.7): it groups artifacts
// into emergent topics by embedding similarity, using incremental leader
// clustering — a new artifact joins the nearest existing topic if it is close
// enough, otherwise it founds a new one. Run as a periodic sweep so centroid
// running-means stay single-threaded (exactly-once per assignment).
package topics

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"kb/internal/kb"
	"kb/internal/relevance"
	"kb/internal/store"
)

const (
	// defaultThreshold is the cosine a new artifact must reach with a topic's
	// centroid to join it rather than found a new topic.
	defaultThreshold = 0.75
	// defaultBatch bounds how many artifacts one sweep clusters.
	defaultBatch = 200
	// maxLabelLen caps a derived topic label.
	maxLabelLen = 80
)

// Clusterer assigns ready, embedded artifacts to topics over the store.
type Clusterer struct {
	st        *store.Store
	threshold float64
	batch     int
}

// New returns a Clusterer with default tuning.
func New(st *store.Store) *Clusterer { return NewWithThreshold(st, defaultThreshold, defaultBatch) }

// NewWithThreshold is New with an explicit join threshold and batch size (tests).
func NewWithThreshold(st *store.Store, threshold float64, batch int) *Clusterer {
	if batch <= 0 {
		batch = defaultBatch
	}
	return &Clusterer{st: st, threshold: threshold, batch: batch}
}

// Sweep clusters the unclustered artifacts and returns how many it assigned. A
// failure on one artifact is collected and the rest proceed (best-effort).
func (c *Clusterer) Sweep(ctx context.Context) (int, error) {
	cands, err := c.st.Topics.UnclusteredEmbeddings(ctx, c.batch)
	if err != nil {
		return 0, err
	}
	var errs []error
	n := 0
	for _, cand := range cands {
		if err := c.assign(ctx, cand); err != nil {
			if ctx.Err() != nil {
				return n, err // shutdown: stop
			}
			errs = append(errs, fmt.Errorf("cluster %q: %w", cand.ArtifactID, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// assign places one candidate: it joins the nearest topic above the threshold
// (updating that topic's running-mean centroid) or founds a new topic. The read of
// the centroids and the write of the assignment happen in one transaction so the
// running mean is consistent and the assignment is exactly-once.
func (c *Clusterer) assign(ctx context.Context, cand store.ClusterCandidate) error {
	return c.st.Tx(ctx, func(tx *store.Store) error {
		topics, err := tx.Topics.ListWithCentroids(ctx)
		if err != nil {
			return err
		}
		best, sim := nearest(topics, cand.Embedding)
		if best != nil && sim >= c.threshold {
			n, err := tx.Topics.MemberCount(ctx, best.ID)
			if err != nil {
				return err
			}
			if err := tx.Topics.Assign(ctx, cand.ArtifactID, best.ID, sim); err != nil {
				return err
			}
			return tx.Topics.SetCentroid(ctx, best.ID, runningMean(best.Centroid, n, cand.Embedding))
		}
		topic := &kb.Topic{Label: label(cand.Title), Centroid: cand.Embedding}
		if err := tx.Topics.Create(ctx, topic); err != nil {
			return err
		}
		return tx.Topics.Assign(ctx, cand.ArtifactID, topic.ID, 1.0)
	})
}

// nearest returns the topic whose centroid is most cosine-similar to vec, and that
// similarity. Topics whose centroid dimension differs (a different embed model) are
// skipped — Cosine returns 0 for them, so they never win.
func nearest(topics []*kb.Topic, vec []float32) (*kb.Topic, float64) {
	var best *kb.Topic
	bestSim := -1.0
	for _, t := range topics {
		if len(t.Centroid) != len(vec) {
			continue
		}
		if sim := relevance.Cosine(t.Centroid, vec); sim > bestSim {
			best, bestSim = t, sim
		}
	}
	return best, bestSim
}

// runningMean folds vec into a centroid that currently averages n members:
// (centroid*n + vec)/(n+1). Returns vec when the dimensions disagree (cannot
// happen for a chosen-by-cosine topic, but defensive).
func runningMean(centroid []float32, n int, vec []float32) []float32 {
	if len(centroid) != len(vec) || n < 0 {
		return vec
	}
	out := make([]float32, len(centroid))
	nf := float32(n)
	for i := range centroid {
		out[i] = (centroid[i]*nf + vec[i]) / (nf + 1)
	}
	return out
}

// label derives a topic label from a founding artifact's title.
func label(title string) string {
	t := strings.Join(strings.Fields(title), " ")
	if t == "" {
		return "untitled"
	}
	if len(t) > maxLabelLen {
		t = t[:maxLabelLen]
	}
	return t
}

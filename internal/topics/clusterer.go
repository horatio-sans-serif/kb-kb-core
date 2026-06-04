// Package topics is the topic-clustering engine (design §8.7): it groups artifacts
// into emergent topics by embedding similarity, using incremental leader
// clustering — a new artifact joins the nearest existing topic if it is close
// enough, otherwise it founds a new one. Run as a periodic sweep so centroid
// running-means stay single-threaded.
//
// Leader clustering is order-dependent by design: the result depends on the order
// artifacts are processed (oldest-first here, so the first of a similar group
// founds the topic and shapes its label), and a topic's running-mean centroid
// drifts as members accumulate, so an early member can end up cosine-distant from
// the drifted centroid yet stays assigned (assignments are not re-evaluated). A
// periodic full re-cluster is out of scope for now. The join threshold should be
// in (0, 1]; at or below 0 a negative-cosine topic could be "joined".
package topics

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

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
// (updating that topic's running-mean centroid) or founds a new topic. The
// membership re-check, the centroid read, and the write all happen in one
// transaction so the running mean is consistent and the whole operation is
// idempotent — re-running it for an already-assigned artifact is a no-op, so a
// candidate that somehow appears in two overlapping sweeps cannot double-count the
// centroid (the INSERT-OR-IGNORE membership alone would not prevent that).
func (c *Clusterer) assign(ctx context.Context, cand store.ClusterCandidate) error {
	return c.st.Tx(ctx, func(tx *store.Store) error {
		if existing, err := tx.Topics.TopicsForArtifact(ctx, cand.ArtifactID); err != nil {
			return err
		} else if len(existing) > 0 {
			return nil // already clustered — do not fold its vector in again
		}
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

// label derives a topic label from a founding artifact's title, capped at
// maxLabelLen bytes without splitting a UTF-8 rune.
func label(title string) string {
	t := strings.Join(strings.Fields(title), " ")
	if t == "" {
		return "untitled"
	}
	if len(t) > maxLabelLen {
		t = t[:maxLabelLen]
		for len(t) > 0 && !utf8.ValidString(t) {
			t = t[:len(t)-1] // back off to the last whole rune
		}
	}
	return t
}

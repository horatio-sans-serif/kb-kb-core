// Package ingest is the shared write path that turns a piece of source content
// into a stored, deduplicated artifact with an encounter and (on a fresh insert)
// an enrichment pipeline. Both the HTTP /ingest handler and the pull scheduler
// use it, so push and pull sources land identically (Principle 1).
package ingest

import (
	"context"
	"fmt"
	"unicode/utf8"

	"kb/internal/events"
	"kb/internal/kb"
	"kb/internal/store"
)

// Enqueuer expands a freshly-inserted artifact into its enrichment-pipeline jobs
// within the caller's transaction (satisfied by *pipeline.Pipeline). nil = no
// pipeline.
type Enqueuer interface {
	EnqueueTx(ctx context.Context, tx *store.Store, artifactID string) error
}

// RescoreFunc re-enqueues an artifact's relevance scoring within the caller's
// transaction — used when a re-encounter changes its relevance inputs (§9). nil =
// no re-score.
type RescoreFunc func(ctx context.Context, tx *store.Store, artifactID string) error

// Item is validated, canonicalized content ready to ingest. Exactly one of
// CanonicalURL (a webpage, deduped by URL) or ContentHash (a note, deduped by
// content fingerprint) identifies the artifact.
type Item struct {
	SourceID     string
	Type         kb.ArtifactType
	Title        string
	CanonicalURL string         // set for URL items (dedup key)
	RawURL       string         // the as-seen URL, recorded on the encounter
	Body         string         // set for note items
	ContentHash  string         // set for note items (dedup key)
	Tier         kb.PrivacyTier // resolved privacy tier
}

// Ingester writes artifacts via the shared path.
type Ingester struct {
	st      *store.Store
	enqueue Enqueuer
	rescore RescoreFunc
	bus     *events.Bus
}

// New builds an Ingester. enqueue, rescore, and bus may each be nil (no pipeline,
// no re-score, no event published, respectively).
func New(st *store.Store, enqueue Enqueuer, rescore RescoreFunc, bus *events.Bus) *Ingester {
	return &Ingester{st: st, enqueue: enqueue, rescore: rescore, bus: bus}
}

// Ingest upserts the artifact, records the encounter, and — atomically — starts
// the pipeline on a fresh insert or re-scores on a re-encounter. It publishes
// artifact.ingested only after the transaction commits (a rolled-back ingest
// emits no event). Returns the artifact id.
func (i *Ingester) Ingest(ctx context.Context, item Item) (string, error) {
	// The dedup key is the artifact's identity. Without one, the note path would
	// insert content_hash="" — a live value under the partial unique index — so
	// unrelated empty-keyed items would silently collapse into one artifact. Make
	// the "exactly one key" invariant real for every caller of this shared seam.
	if item.CanonicalURL == "" && item.ContentHash == "" {
		return "", fmt.Errorf("ingest: item has neither a canonical URL nor a content hash")
	}

	var artifactID string
	err := i.st.Tx(ctx, func(tx *store.Store) error {
		a := &store.Artifact{Type: item.Type, Title: item.Title, PrivacyTier: item.Tier}
		var art *store.Artifact
		var uerr error
		if item.CanonicalURL != "" {
			a.CanonicalURL = &item.CanonicalURL
			art, uerr = tx.Artifacts.UpsertByCanonical(ctx, a)
		} else {
			a.BodyMarkdown = item.Body
			a.ContentHash = &item.ContentHash
			art, uerr = tx.Artifacts.UpsertByContentHash(ctx, a)
		}
		if uerr != nil {
			return uerr
		}
		artifactID = art.ID

		var rawURL *string
		if item.RawURL != "" {
			rawURL = &item.RawURL
		}
		if err := tx.Encounters.Record(ctx, &store.Encounter{
			ArtifactID: art.ID, SourceID: item.SourceID, RawURL: rawURL,
		}); err != nil {
			return err
		}

		// A fresh INSERT starts the pipeline; a re-encounter of known content
		// re-scores (the encounter factor changed) but does not re-enrich. The
		// upsert leaves created_at==updated_at only on insert.
		inserted := art.CreatedAt == art.UpdatedAt
		switch {
		case inserted && i.enqueue != nil:
			return i.enqueue.EnqueueTx(ctx, tx, art.ID)
		case !inserted && i.rescore != nil:
			return i.rescore(ctx, tx, art.ID)
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	if i.bus != nil {
		i.bus.Publish(events.New(events.ArtifactIngested,
			map[string]string{"artifact_id": artifactID, "source_id": item.SourceID}))
	}
	return artifactID, nil
}

// ValidText rejects invalid UTF-8 and control characters (a stored-injection
// guard for the renderers). Tab is always allowed; newlines/CR only when
// allowNewlines (a Markdown body, not a title).
func ValidText(s string, allowNewlines bool) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r == '\t', allowNewlines && (r == '\n' || r == '\r'):
			continue
		case r < 0x20 || r == 0x7f:
			return false
		}
	}
	return true
}

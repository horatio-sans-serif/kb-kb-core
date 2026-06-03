package render

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"kb/internal/kb"
	"kb/internal/store"
)

// ErrUnknownFormat is returned when a render is requested in a format that no
// registered renderer handles.
var ErrUnknownFormat = errors.New("render: unknown format")

// ErrInvalidCeiling is returned when PrivacyCeiling is non-empty but not a valid
// tier. The export filter fails closed: an unparseable ceiling is rejected rather
// than silently including every tier.
var ErrInvalidCeiling = errors.New("render: invalid privacy ceiling")

// ErrBusy is returned when the concurrent-render limit is saturated. A pandoc/TeX
// render is CPU- and memory-bound, so the dispatcher caps how many run at once;
// callers should retry (the API answers 503 + Retry-After).
var ErrBusy = errors.New("render: too many concurrent renders")

// ErrNoEligibleArtifacts is returned when a request named artifacts but none
// survived resolution — all were missing/tombstoned or excluded by the privacy
// ceiling. Signalled rather than silently producing an empty document, so the
// caller learns nothing was eligible (the API answers 422).
var ErrNoEligibleArtifacts = errors.New("render: no eligible artifacts")

// Request describes one render: which artifacts, in which format, with an
// optional document title, a provenance label, and an export privacy ceiling.
// ArtifactIDs is used as given — the caller owns the order, and duplicates are
// preserved (the layer does not dedup, §8.14: the caller decides the set).
type Request struct {
	Format         string
	ArtifactIDs    []string       // the artifacts to include, in order
	Title          string         // document H1; renderer-specific default if empty
	SourceQuery    string         // provenance label recorded on the render row
	PrivacyCeiling kb.PrivacyTier // export filter: drop artifacts above this tier; empty = include all, invalid = error
}

// Service is the render dispatcher: it resolves the artifacts, applies the
// privacy export filter, runs the chosen renderer, and persists a provenance row.
type Service struct {
	st  *store.Store
	reg *Registry
	sem chan struct{} // bounds concurrent renders; nil means unbounded
}

// NewService wires the dispatcher to the store and renderer registry, bounding
// concurrent renders to the CPU count (a pandoc/TeX render is CPU/memory-bound).
func NewService(st *store.Store, reg *Registry) *Service {
	return NewServiceWithConcurrency(st, reg, runtime.NumCPU())
}

// NewServiceWithConcurrency is NewService with an explicit concurrency limit
// (n <= 0 means unbounded). Used by tests to force saturation.
func NewServiceWithConcurrency(st *store.Store, reg *Registry, n int) *Service {
	s := &Service{st: st, reg: reg}
	if n > 0 {
		s.sem = make(chan struct{}, n)
	}
	return s
}

// Formats lists the registered output formats (for the API to advertise).
func (s *Service) Formats() []string { return s.reg.Formats() }

// Render produces an output file for req and records it. Resolution drops
// artifacts that are missing/tombstoned (consistent with retrieval) and, when a
// ceiling is set, those more sensitive than it (the §8.14 export filter). The
// render id names the output file, so the file and the provenance row share one
// identity.
func (s *Service) Render(ctx context.Context, req Request) (*kb.RenderRecord, error) {
	renderer, ok := s.reg.Get(req.Format)
	if !ok {
		return nil, fmt.Errorf("%w %q (have: %v)", ErrUnknownFormat, req.Format, s.reg.Formats())
	}
	if req.PrivacyCeiling != "" && !req.PrivacyCeiling.Valid() {
		return nil, fmt.Errorf("%w %q", ErrInvalidCeiling, req.PrivacyCeiling)
	}

	// Cap concurrent renders so a burst of pandoc/TeX subprocesses can't exhaust
	// CPU/memory. Fail fast rather than queue: holding the connection for an
	// unbounded wait is worse than a retryable 503.
	if s.sem != nil {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		default:
			return nil, ErrBusy
		}
	}

	arts, ids, err := s.resolve(ctx, req.ArtifactIDs, req.PrivacyCeiling)
	if err != nil {
		return nil, err
	}
	// A request that named artifacts but resolved to none (all missing/tombstoned
	// or filtered by the ceiling) is a "nothing eligible" signal, not an empty doc.
	if len(req.ArtifactIDs) > 0 && len(arts) == 0 {
		return nil, ErrNoEligibleArtifacts
	}

	id := store.NewID()
	opts := kb.RenderOpts{Format: req.Format, Options: map[string]string{"name": id, "title": req.Title}}
	rendered, err := renderer.Render(ctx, arts, opts)
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", req.Format, err)
	}

	rec := &kb.RenderRecord{
		ID:          id,
		Format:      req.Format,
		SourceQuery: req.SourceQuery,
		ArtifactIDs: ids,
		OutputPath:  rendered.OutputPath,
		PrivacyMax:  rendered.PrivacyMax,
	}
	if err := s.st.Renders.Create(ctx, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// resolve loads the requested artifacts in order, skipping ones that no longer
// exist and (when ceiling is a valid tier) ones above it. Returns the kept
// artifacts and their ids in the same order.
func (s *Service) resolve(ctx context.Context, requested []string, ceiling kb.PrivacyTier) ([]*kb.Artifact, []string, error) {
	arts := make([]*kb.Artifact, 0, len(requested))
	ids := make([]string, 0, len(requested))
	for _, id := range requested {
		a, err := s.st.Artifacts.GetLive(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // dropped/tombstoned between selection and render
			}
			return nil, nil, fmt.Errorf("load artifact %q: %w", id, err)
		}
		if ceiling.Valid() && !a.PrivacyTier.AtMost(ceiling) {
			continue // export filter: too sensitive for this render
		}
		arts = append(arts, a)
		ids = append(ids, a.ID)
	}
	return arts, ids, nil
}

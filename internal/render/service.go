package render

import (
	"context"
	"errors"
	"fmt"

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
}

// NewService wires the dispatcher to the store and renderer registry.
func NewService(st *store.Store, reg *Registry) *Service {
	return &Service{st: st, reg: reg}
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

	arts, ids, err := s.resolve(ctx, req.ArtifactIDs, req.PrivacyCeiling)
	if err != nil {
		return nil, err
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

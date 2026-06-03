package render

import (
	"fmt"
	"sort"

	"kb/internal/kb"
)

// Registry maps a format name to its Renderer (design §7: a new output format is
// a new Renderer plus a registration; the core does not change). It mirrors the
// source-connector registry.
type Registry struct {
	m map[string]kb.Renderer
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{m: make(map[string]kb.Renderer)}
}

// Register adds rr under its Format(). A duplicate format is a wiring bug and
// panics (renderers are registered once at startup).
func (r *Registry) Register(rr kb.Renderer) {
	f := rr.Format()
	if _, dup := r.m[f]; dup {
		panic(fmt.Sprintf("render: format %q already registered", f))
	}
	r.m[f] = rr
}

// Get returns the renderer for format.
func (r *Registry) Get(format string) (kb.Renderer, bool) {
	rr, ok := r.m[format]
	return rr, ok
}

// Formats returns the registered format names, sorted.
func (r *Registry) Formats() []string {
	out := make([]string, 0, len(r.m))
	for f := range r.m {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

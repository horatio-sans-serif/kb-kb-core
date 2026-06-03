// Package sources holds the connector registry: a mapping from a connector name
// to its SourceConnector implementation (design §7 / §8.1). Phase 4 registers
// real connectors at startup; the daemon validates that every enabled source's
// connector is registered before scheduling it. The registry is the seam now;
// connectors arrive with the PULL/PUSH/AMBIENT sources.
package sources

import (
	"fmt"
	"sort"

	"kb/internal/kb"
)

// Registry maps connector names to implementations.
type Registry struct {
	m map[string]kb.SourceConnector
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{m: make(map[string]kb.SourceConnector)}
}

// Register adds c under name. A duplicate name is a wiring bug and panics
// (connectors are registered once at startup).
func (r *Registry) Register(name string, c kb.SourceConnector) {
	if _, dup := r.m[name]; dup {
		panic(fmt.Sprintf("sources: connector %q already registered", name))
	}
	r.m[name] = c
}

// Get returns the connector registered under name.
func (r *Registry) Get(name string) (kb.SourceConnector, bool) {
	c, ok := r.m[name]
	return c, ok
}

// Has reports whether a connector is registered under name.
func (r *Registry) Has(name string) bool {
	_, ok := r.m[name]
	return ok
}

// Names returns the registered connector names, sorted.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.m))
	for n := range r.m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Kind returns the kind (PULL/PUSH/AMBIENT) of the named connector, so a source's
// declared kind can be validated against its connector at create time.
func (r *Registry) Kind(name string) (kb.SourceKind, bool) {
	c, ok := r.m[name]
	if !ok {
		return "", false
	}
	return c.Kind(), true
}

package sources

import (
	"context"
	"testing"

	"kb/internal/kb"
)

type fakeConnector struct{ id string }

func (f fakeConnector) ID() string          { return f.id }
func (f fakeConnector) Kind() kb.SourceKind { return kb.Pull }
func (f fakeConnector) Fetch(context.Context, kb.Cursor) ([]kb.RawItem, kb.Cursor, error) {
	return nil, "", nil
}
func (f fakeConnector) HealthCheck(context.Context) (kb.Health, error) {
	return kb.Health{OK: true}, nil
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if r.Has("rss") {
		t.Error("empty registry should not have rss")
	}
	r.Register("rss", fakeConnector{id: "rss-1"})
	if !r.Has("rss") {
		t.Error("rss should be registered")
	}
	if c, ok := r.Get("rss"); !ok || c.ID() != "rss-1" {
		t.Errorf("Get(rss) = %v, %v; want the registered connector", c, ok)
	}
	r.Register("hn", fakeConnector{id: "hn-1"})
	if got := r.Names(); len(got) != 2 || got[0] != "hn" || got[1] != "rss" {
		t.Errorf("Names() = %v, want [hn rss]", got)
	}
}

func TestRegistry_DuplicatePanics(t *testing.T) {
	r := NewRegistry()
	r.Register("rss", fakeConnector{})
	defer func() {
		if recover() == nil {
			t.Error("duplicate Register should panic")
		}
	}()
	r.Register("rss", fakeConnector{})
}

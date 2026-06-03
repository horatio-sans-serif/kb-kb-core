package render

import (
	"context"
	"reflect"
	"testing"

	"kb/internal/kb"
)

// stubRenderer is a no-op Renderer for registry tests.
type stubRenderer struct{ format string }

func (s stubRenderer) Format() string { return s.format }
func (s stubRenderer) Render(context.Context, []*kb.Artifact, kb.RenderOpts) (kb.Rendered, error) {
	return kb.Rendered{Format: s.format}, nil
}

func TestRegistry_RegisterGet(t *testing.T) {
	reg := NewRegistry()
	reg.Register(stubRenderer{"html"})
	reg.Register(stubRenderer{"pdf"})

	if _, ok := reg.Get("html"); !ok {
		t.Error("html should be registered")
	}
	if _, ok := reg.Get("nope"); ok {
		t.Error("unknown format should not resolve")
	}
	if got := reg.Formats(); !reflect.DeepEqual(got, []string{"html", "pdf"}) {
		t.Errorf("Formats = %v, want sorted [html pdf]", got)
	}
}

func TestRegistry_DuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("registering a duplicate format should panic")
		}
	}()
	reg := NewRegistry()
	reg.Register(stubRenderer{"html"})
	reg.Register(stubRenderer{"html"})
}

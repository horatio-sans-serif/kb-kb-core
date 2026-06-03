package render

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kb/internal/kb"
)

func TestMarkdown_RenderWritesFile(t *testing.T) {
	dir := t.TempDir()
	m := NewMarkdown(dir)
	if m.Format() != "markdown" {
		t.Fatalf("Format = %q, want markdown", m.Format())
	}

	arts := []*kb.Artifact{
		{Title: "A", BodyMarkdown: "alpha", PrivacyTier: kb.Public},
		{Title: "B", BodyMarkdown: "beta", PrivacyTier: kb.Sensitive},
	}
	out, err := m.Render(context.Background(), arts, kb.RenderOpts{
		Options: map[string]string{"name": "01HX", "title": "Doc"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	wantPath := filepath.Join(dir, "01HX.md")
	if out.OutputPath != wantPath {
		t.Errorf("OutputPath = %q, want %q", out.OutputPath, wantPath)
	}
	if out.Format != "markdown" {
		t.Errorf("Format = %q, want markdown", out.Format)
	}
	if out.PrivacyMax != kb.Sensitive {
		t.Errorf("PrivacyMax = %q, want sensitive (the most sensitive included)", out.PrivacyMax)
	}

	b, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, "# Doc\n") || !strings.Contains(got, "## A\n") || !strings.Contains(got, "alpha") {
		t.Errorf("output file missing assembled content:\n%s", got)
	}
}

func TestMarkdown_OutputFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	out, err := NewMarkdown(dir).Render(context.Background(),
		[]*kb.Artifact{{Title: "x", PrivacyTier: kb.Restricted}},
		kb.RenderOpts{Options: map[string]string{"name": "r"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	info, err := os.Stat(out.OutputPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Renders can contain restricted content; the file must not be world-readable.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("output file mode = %o, want owner-only (no group/other bits)", perm)
	}
}

func TestMarkdown_NameDefaultsAndIsSanitized(t *testing.T) {
	dir := t.TempDir()
	m := NewMarkdown(dir)

	// A path-bearing name must not escape the output directory (traversal guard).
	out, err := m.Render(context.Background(), nil,
		kb.RenderOpts{Options: map[string]string{"name": "../../etc/passwd"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := filepath.Dir(out.OutputPath); got != filepath.Clean(dir) {
		t.Errorf("output escaped the render dir: %q is in %q", out.OutputPath, dir)
	}

	// An empty name falls back to a default basename rather than writing a dotfile.
	out2, err := m.Render(context.Background(), nil, kb.RenderOpts{})
	if err != nil {
		t.Fatalf("render (no name): %v", err)
	}
	if base := filepath.Base(out2.OutputPath); base == ".md" || !strings.HasSuffix(base, ".md") {
		t.Errorf("empty name produced a bad basename %q", base)
	}
}

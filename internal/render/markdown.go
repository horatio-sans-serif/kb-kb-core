package render

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"kb/internal/kb"
)

// Markdown is the dependency-free Renderer: it writes the assembled document
// straight to a .md file. Because every other format is "assembled Markdown ->
// pandoc -> format", this is both a usable output on its own and the reference
// for what the pandoc-backed renderers consume.
type Markdown struct {
	dir string // directory the .md file is written into
}

// NewMarkdown returns a Markdown renderer that writes into dir (created on first
// render).
func NewMarkdown(dir string) *Markdown { return &Markdown{dir: dir} }

// Format identifies this renderer in the registry.
func (m *Markdown) Format() string { return "markdown" }

// Render assembles arts into a Markdown document and writes it to
// <dir>/<name>.md, where name comes from opts.Options["name"] (the caller's
// render id) and the document title from opts.Options["title"]. The file is
// owner-only: a render may contain restricted-tier content.
func (m *Markdown) Render(_ context.Context, arts []*kb.Artifact, opts kb.RenderOpts) (kb.Rendered, error) {
	doc := Assemble(arts, AssembleOpts{Title: opts.Options["title"]})
	path, err := outputPath(m.dir, opts.Options["name"], "md")
	if err != nil {
		return kb.Rendered{}, err
	}
	if err := writeFile(path, []byte(doc)); err != nil {
		return kb.Rendered{}, err
	}
	return kb.Rendered{Format: m.Format(), OutputPath: path, PrivacyMax: maxTier(arts)}, nil
}

// maxTier is the most sensitive tier across arts — the privacy_max a render
// records (§8.14). Public for an empty set.
func maxTier(arts []*kb.Artifact) kb.PrivacyTier {
	tiers := make([]kb.PrivacyTier, 0, len(arts))
	for _, a := range arts {
		tiers = append(tiers, a.PrivacyTier)
	}
	return kb.MaxTier(tiers...)
}

// outputPath joins dir, a sanitized basename, and an extension. name is reduced
// to its last path element so a caller-supplied value cannot escape dir (path
// traversal); an empty name falls back to "render".
func outputPath(dir, name, ext string) (string, error) {
	base := filepath.Base(filepath.Clean("/" + name)) // strip any directory components
	if base == "." || base == "/" || base == "" {
		base = "render"
	}
	if dir == "" {
		return "", fmt.Errorf("render: no output directory configured")
	}
	return filepath.Join(dir, base+"."+ext), nil
}

// writeFile creates the parent directory (owner-only) and writes b to path with
// owner-only permissions.
func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("render: make output dir: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("render: write %q: %w", path, err)
	}
	return nil
}

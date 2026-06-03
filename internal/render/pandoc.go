package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"kb/internal/kb"
	"kb/internal/textutil"
)

// pandocBin is the converter binary name, resolved on PATH at render time.
const pandocBin = "pandoc"

// ErrRendererUnavailable is returned when the external tool a renderer needs
// (pandoc) is not installed, so the API can answer 503 rather than 500.
var ErrRendererUnavailable = errors.New("render: converter tool not available")

// runner executes the converter, feeding the assembled Markdown on stdin. The
// command writes the output file itself (via -o). It is a field so tests can
// substitute a fake without a real pandoc install.
type runner func(ctx context.Context, name string, args []string, stdin []byte) error

// pandocFormat describes how one target format maps onto a pandoc invocation.
type pandocFormat struct {
	writer     string // pandoc -t value; "" => infer from the output extension (pdf)
	ext        string // output file extension
	standalone bool   // pass -s for a full, self-contained document
}

// pandocFormats is the supported set. html is made standalone so the file opens
// on its own; pdf/docx/epub are inherently whole documents. pdf has no writer
// name — pandoc selects the PDF path from the .pdf extension.
var pandocFormats = map[string]pandocFormat{
	"html": {writer: "html", ext: "html", standalone: true},
	"pdf":  {writer: "", ext: "pdf", standalone: false},
	"docx": {writer: "docx", ext: "docx", standalone: false},
	"epub": {writer: "epub", ext: "epub", standalone: false},
}

// Pandoc renders assembled Markdown to a binary/markup format by piping it
// through the pandoc subprocess. One instance serves one format (the Renderer
// contract is single-format); the daemon constructs one per supported format.
type Pandoc struct {
	format string
	spec   pandocFormat
	dir    string
	run    runner
}

// NewPandoc returns a Pandoc renderer for format (html|pdf|docx|epub) writing
// into dir. It rejects an unsupported format.
func NewPandoc(format, dir string) (*Pandoc, error) {
	spec, ok := pandocFormats[format]
	if !ok {
		return nil, fmt.Errorf("render: unsupported pandoc format %q", format)
	}
	return &Pandoc{format: format, spec: spec, dir: dir, run: execPandoc}, nil
}

// Format identifies this renderer in the registry.
func (p *Pandoc) Format() string { return p.format }

// Render assembles the artifacts and runs pandoc to produce the output file.
// Content travels on stdin, never as arguments, so artifact text cannot inject
// command flags. The output is tightened to owner-only after the tool writes it
// (a render may contain restricted content).
func (p *Pandoc) Render(ctx context.Context, arts []*kb.Artifact, opts kb.RenderOpts) (kb.Rendered, error) {
	doc := Assemble(arts, AssembleOpts{Title: opts.Options["title"]})
	path, err := outputPath(p.dir, opts.Options["name"], p.spec.ext)
	if err != nil {
		return kb.Rendered{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return kb.Rendered{}, fmt.Errorf("render: make output dir: %w", err)
	}

	if err := p.run(ctx, pandocBin, p.args(path), []byte(doc)); err != nil {
		return kb.Rendered{}, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return kb.Rendered{}, fmt.Errorf("render: secure output %q: %w", path, err)
	}
	return kb.Rendered{Format: p.format, OutputPath: path, PrivacyMax: maxTier(arts)}, nil
}

// args builds the pandoc argument vector for writing to out.
func (p *Pandoc) args(out string) []string {
	a := []string{"-f", "markdown"}
	if p.spec.writer != "" {
		a = append(a, "-t", p.spec.writer)
	}
	if p.spec.standalone {
		a = append(a, "-s")
	}
	return append(a, "-o", out)
}

// execPandoc resolves and runs pandoc, returning ErrRendererUnavailable when the
// binary is absent and a stderr-bearing error when the conversion fails.
func execPandoc(ctx context.Context, name string, args []string, stdin []byte) error {
	bin, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("%w: %s not found on PATH", ErrRendererUnavailable, name)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("render: %s failed: %w: %s", name, err, textutil.TruncateBytes(stderr.String(), 512))
	}
	return nil
}

// PandocAvailable reports whether the pandoc binary is on PATH, so the daemon can
// warn at startup (and skip integration tests) when the converter is missing.
func PandocAvailable() bool {
	_, err := exec.LookPath(pandocBin)
	return err == nil
}

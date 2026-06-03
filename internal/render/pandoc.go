package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"kb/internal/kb"
	"kb/internal/textutil"
)

// pandocBin is the converter binary name, resolved on PATH at render time.
const pandocBin = "pandoc"

// renderTimeout bounds a single pandoc invocation so a wedged converter (or a
// looping TeX run from a pathological body) cannot hang a render indefinitely.
const renderTimeout = 60 * time.Second

// maxRenderBytes caps the size of a single rendered output, so a body that
// expands pathologically cannot fill the disk.
const maxRenderBytes = 64 << 20 // 64 MiB

// ErrRendererUnavailable is returned when the external tool a renderer needs
// (pandoc) is not installed, so the API can answer 503 rather than 500.
var ErrRendererUnavailable = errors.New("render: converter tool not available")

// errOutputOverflow aborts a render whose output exceeds maxRenderBytes.
var errOutputOverflow = errors.New("render: output too large")

// runner runs the converter, feeding the assembled Markdown on stdin and
// returning the rendered bytes (captured from stdout). It is a field so tests can
// substitute a fake without a real pandoc install.
type runner func(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error)

// pandocFormat describes how one target format maps onto a pandoc invocation.
type pandocFormat struct {
	writer     string // pandoc -t value
	ext        string // output file extension
	standalone bool   // pass -s for a full, self-contained document
}

// pandocFormats is the supported set. html is made standalone so the file opens
// on its own; pdf/docx/epub are inherently whole documents. Every format streams
// to stdout (-o -), so each needs an explicit -t writer (no extension to infer
// from), including pdf.
var pandocFormats = map[string]pandocFormat{
	"html": {writer: "html", ext: "html", standalone: true},
	"pdf":  {writer: "pdf", ext: "pdf", standalone: false},
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

// Render assembles the artifacts, runs pandoc to produce the output bytes, and
// writes them owner-only. Content travels on stdin (never argv, so artifact text
// cannot inject flags) and is captured from stdout (so this code — not the
// subprocess under the ambient umask — creates the file, closing any
// world-readable window for restricted content). A bounded context kills a
// wedged converter.
func (p *Pandoc) Render(ctx context.Context, arts []*kb.Artifact, opts kb.RenderOpts) (kb.Rendered, error) {
	doc := Assemble(arts, AssembleOpts{Title: opts.Options["title"]})
	path, err := outputPath(p.dir, opts.Options["name"], p.spec.ext)
	if err != nil {
		return kb.Rendered{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, renderTimeout)
	defer cancel()
	rendered, err := p.run(ctx, pandocBin, p.args(), []byte(doc))
	if err != nil {
		return kb.Rendered{}, err
	}
	if err := writeFile(path, rendered); err != nil {
		return kb.Rendered{}, err
	}
	return kb.Rendered{Format: p.format, OutputPath: path, PrivacyMax: maxTier(arts)}, nil
}

// args builds the pandoc argument vector. The reader is markdown with raw
// passthrough disabled (`-raw_tex-raw_html`): a hostile artifact body — bodies
// come partly from fetched web/RSS content — must not be able to inject raw
// LaTeX (which the local TeX engine would execute, reading arbitrary files via
// \input) or raw HTML (stored XSS if a render is opened in a browser). --sandbox
// confines pandoc's own file access as defense in depth.
func (p *Pandoc) args() []string {
	a := []string{"-f", "markdown-raw_tex-raw_html", "--sandbox"}
	a = append(a, "-t", p.spec.writer)
	if p.spec.standalone {
		a = append(a, "-s")
	}
	return append(a, "-o", "-") // write to stdout; this process persists the bytes
}

// execPandoc is the production runner.
func execPandoc(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error) {
	return runCommand(ctx, name, args, stdin, maxRenderBytes)
}

// runCommand resolves and runs name, feeding stdin and capturing at most maxBytes
// of stdout. It returns ErrRendererUnavailable when the binary is absent,
// errOutputOverflow when the output is too large, and a stderr-bearing error when
// the command fails.
func runCommand(ctx context.Context, name string, args []string, stdin []byte, maxBytes int) ([]byte, error) {
	bin, err := exec.LookPath(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s not found on PATH", ErrRendererUnavailable, name)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	// Belt-and-suspenders for the PDF/TeX path: even with raw_tex stripped at the
	// reader, refuse any file reads/writes the TeX engine might attempt. These are
	// kpathsea (TeX) variables; non-TeX writers ignore them.
	cmd.Env = append(os.Environ(), "openin_any=p", "openout_any=p")
	out := &limitedBuffer{max: maxBytes, cancel: cancel}
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if out.overflow {
		return nil, fmt.Errorf("render: %s produced more than %d bytes: %w", name, maxBytes, errOutputOverflow)
	}
	if runErr != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("render: %s cancelled: %w", name, ctx.Err())
		}
		return nil, fmt.Errorf("render: %s failed: %w: %s", name, runErr, textutil.TruncateBytes(stderr.String(), 512))
	}
	return out.buf.Bytes(), nil
}

// limitedBuffer captures stdout up to max bytes. On the first overflowing write it
// cancels the command's context so the subprocess is torn down promptly instead of
// blocking on a full pipe until the timeout.
type limitedBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
	cancel   context.CancelFunc
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.overflow {
		return 0, errOutputOverflow
	}
	if l.buf.Len()+len(p) > l.max {
		l.overflow = true
		l.cancel()
		return 0, errOutputOverflow
	}
	return l.buf.Write(p)
}

// PandocAvailable reports whether the pandoc binary is on PATH, so the daemon can
// warn at startup (and skip integration tests) when the converter is missing.
func PandocAvailable() bool {
	_, err := exec.LookPath(pandocBin)
	return err == nil
}

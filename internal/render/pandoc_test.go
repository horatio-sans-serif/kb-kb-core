package render

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kb/internal/kb"
)

// fakePandoc records the command it was asked to run and simulates pandoc by
// writing the output file named by the -o argument.
type fakePandoc struct {
	gotArgs  []string
	gotStdin string
	err      error
}

func (f *fakePandoc) run(_ context.Context, _ string, args []string, stdin []byte) error {
	f.gotArgs = args
	f.gotStdin = string(stdin)
	if f.err != nil {
		return f.err
	}
	// Simulate pandoc writing the output file given by -o.
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-o" {
			_ = os.WriteFile(args[i+1], []byte("rendered"), 0o644)
		}
	}
	return nil
}

func newFakePandoc(t *testing.T, format string) (*Pandoc, *fakePandoc) {
	t.Helper()
	p, err := NewPandoc(format, t.TempDir())
	if err != nil {
		t.Fatalf("NewPandoc(%q): %v", format, err)
	}
	fake := &fakePandoc{}
	p.run = fake.run
	return p, fake
}

func TestPandoc_HTMLArgsAndOutput(t *testing.T) {
	p, fake := newFakePandoc(t, "html")
	if p.Format() != "html" {
		t.Fatalf("Format = %q, want html", p.Format())
	}

	arts := []*kb.Artifact{{Title: "A", BodyMarkdown: "alpha", PrivacyTier: kb.Personal}}
	out, err := p.Render(context.Background(), arts, kb.RenderOpts{Options: map[string]string{"name": "01HX", "title": "Doc"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// The body/title travel via stdin (not argv) so content can never be argument-injected.
	if !strings.Contains(fake.gotStdin, "# Doc") || !strings.Contains(fake.gotStdin, "alpha") {
		t.Errorf("assembled markdown not piped to stdin: %q", fake.gotStdin)
	}
	args := strings.Join(fake.gotArgs, " ")
	if !strings.Contains(args, "-f markdown") || !strings.Contains(args, "-t html") || !strings.Contains(args, "-s") {
		t.Errorf("html args missing -f markdown / -t html / -s: %v", fake.gotArgs)
	}
	wantPath := filepath.Join(p.dir, "01HX.html")
	if !strings.Contains(args, "-o "+wantPath) {
		t.Errorf("missing -o %s: %v", wantPath, fake.gotArgs)
	}
	if out.OutputPath != wantPath || out.Format != "html" || out.PrivacyMax != kb.Personal {
		t.Errorf("Rendered = %+v, want path %s, html, personal", out, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("output file not created: %v", err)
	}
}

func TestPandoc_PDFInfersFromExtension(t *testing.T) {
	// PDF has no pandoc writer name; the output extension selects it, so -t must
	// be omitted (passing "-t pdf" would error in real pandoc).
	p, fake := newFakePandoc(t, "pdf")
	if _, err := p.Render(context.Background(), nil, kb.RenderOpts{Options: map[string]string{"name": "r"}}); err != nil {
		t.Fatalf("render: %v", err)
	}
	args := strings.Join(fake.gotArgs, " ")
	if strings.Contains(args, "-t ") {
		t.Errorf("pdf must not pass a -t writer: %v", fake.gotArgs)
	}
	if !strings.Contains(args, "-o "+filepath.Join(p.dir, "r.pdf")) {
		t.Errorf("pdf output path wrong: %v", fake.gotArgs)
	}
}

func TestPandoc_OutputIsOwnerOnly(t *testing.T) {
	p, _ := newFakePandoc(t, "docx")
	out, err := p.Render(context.Background(), []*kb.Artifact{{Title: "x", PrivacyTier: kb.Restricted}}, kb.RenderOpts{Options: map[string]string{"name": "r"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	info, err := os.Stat(out.OutputPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// The fake wrote 0644; the renderer must tighten it (a render can be restricted).
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("output mode = %o, want owner-only", perm)
	}
}

func TestPandoc_ToolUnavailablePropagates(t *testing.T) {
	p, fake := newFakePandoc(t, "epub")
	fake.err = ErrRendererUnavailable
	_, err := p.Render(context.Background(), nil, kb.RenderOpts{Options: map[string]string{"name": "r"}})
	if !errors.Is(err, ErrRendererUnavailable) {
		t.Errorf("Render = %v, want ErrRendererUnavailable", err)
	}
}

func TestNewPandoc_UnknownFormat(t *testing.T) {
	if _, err := NewPandoc("rtf", t.TempDir()); err == nil {
		t.Error("NewPandoc(rtf) should reject an unsupported format")
	}
}

// TestPandoc_RealRoundTrip exercises the actual pandoc binary when present.
func TestPandoc_RealRoundTrip(t *testing.T) {
	if !PandocAvailable() {
		t.Skip("pandoc not installed")
	}
	p, err := NewPandoc("html", t.TempDir())
	if err != nil {
		t.Fatalf("NewPandoc: %v", err)
	}
	out, err := p.Render(context.Background(),
		[]*kb.Artifact{{Title: "Hello", BodyMarkdown: "world **bold**", PrivacyTier: kb.Public}},
		kb.RenderOpts{Options: map[string]string{"name": "rt", "title": "RT"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	b, err := os.ReadFile(out.OutputPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), "<strong>bold</strong>") {
		t.Errorf("pandoc html output missing rendered bold: %s", b)
	}
}

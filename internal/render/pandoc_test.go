package render

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"kb/internal/kb"
)

// fakePandoc records the command it was asked to run and returns canned output
// bytes (as the real runner returns captured stdout).
type fakePandoc struct {
	gotArgs     []string
	gotStdin    string
	hadDeadline bool
	out         []byte
	err         error
}

func (f *fakePandoc) run(ctx context.Context, _ string, args []string, stdin []byte) ([]byte, error) {
	f.gotArgs = args
	f.gotStdin = string(stdin)
	_, f.hadDeadline = ctx.Deadline()
	if f.err != nil {
		return nil, f.err
	}
	if f.out == nil {
		return []byte("rendered"), nil
	}
	return f.out, nil
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
	// Reader strips raw passthrough (the C1 fix), html gets a writer + standalone,
	// and output streams to stdout (-o -) so this process writes the file.
	for _, want := range []string{"-f markdown-raw_tex-raw_html", "--sandbox", "-t html", "-s", "-o -"} {
		if !strings.Contains(args, want) {
			t.Errorf("html args missing %q: %v", want, fake.gotArgs)
		}
	}
	if !fake.hadDeadline {
		t.Error("Render should pass a deadline-bounded context to the runner")
	}

	wantPath := outPath(p, "01HX")
	if out.OutputPath != wantPath || out.Format != "html" || out.PrivacyMax != kb.Personal {
		t.Errorf("Rendered = %+v, want path %s, html, personal", out, wantPath)
	}
	b, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(b) != "rendered" {
		t.Errorf("output file = %q, want the captured bytes", b)
	}
}

func outPath(p *Pandoc, name string) string {
	path, _ := outputPath(p.dir, name, p.spec.ext)
	return path
}

func TestPandoc_PDFUsesPDFWriter(t *testing.T) {
	// PDF streams to stdout like every other format, so it needs an explicit
	// `-t pdf` writer (there is no extension to infer from).
	p, fake := newFakePandoc(t, "pdf")
	if _, err := p.Render(context.Background(), nil, kb.RenderOpts{Options: map[string]string{"name": "r"}}); err != nil {
		t.Fatalf("render: %v", err)
	}
	args := strings.Join(fake.gotArgs, " ")
	if !strings.Contains(args, "-t pdf") || !strings.Contains(args, "-o -") {
		t.Errorf("pdf args wrong: %v", fake.gotArgs)
	}
	if got := outPath(p, "r"); !strings.HasSuffix(got, ".pdf") {
		t.Errorf("pdf output path = %q, want .pdf", got)
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
	// This process writes the file (0600), independent of the ambient umask.
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

func TestRunCommand_OutputBound(t *testing.T) {
	// runCommand must cap captured output and not return the bytes when exceeded.
	_, err := runCommand(context.Background(), "sh", []string{"-c", "printf 'aaaaaaaa'"}, nil, 3)
	if !errors.Is(err, errOutputOverflow) {
		t.Errorf("runCommand over the byte cap = %v, want errOutputOverflow", err)
	}
}

func TestRunCommand_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the command must not run to completion
	_, err := runCommand(ctx, "sh", []string{"-c", "sleep 5"}, nil, 1024)
	if err == nil {
		t.Error("a cancelled context should fail the command")
	}
}

func TestRunCommand_UnavailableBinary(t *testing.T) {
	_, err := runCommand(context.Background(), "definitely-not-a-real-binary-xyz", nil, nil, 1024)
	if !errors.Is(err, ErrRendererUnavailable) {
		t.Errorf("runCommand(missing) = %v, want ErrRendererUnavailable", err)
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

// TestPandoc_RawPassthroughStripped is the C1 regression: a hostile body's raw
// HTML/LaTeX must not survive into the output.
func TestPandoc_RawPassthroughStripped(t *testing.T) {
	if !PandocAvailable() {
		t.Skip("pandoc not installed")
	}
	p, err := NewPandoc("html", t.TempDir())
	if err != nil {
		t.Fatalf("NewPandoc: %v", err)
	}
	body := "safe text <script>alert('xss')</script> and \\input{/etc/hostname}"
	out, err := p.Render(context.Background(),
		[]*kb.Artifact{{Title: "Hostile", BodyMarkdown: body, PrivacyTier: kb.Public}},
		kb.RenderOpts{Options: map[string]string{"name": "h"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	b, _ := os.ReadFile(out.OutputPath)
	html := string(b)
	if strings.Contains(html, "<script>") {
		t.Errorf("raw <script> survived into HTML output (XSS):\n%s", html)
	}
	if strings.Contains(html, "safe text") == false {
		t.Errorf("legitimate text was lost:\n%s", html)
	}
}

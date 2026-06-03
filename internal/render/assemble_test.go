package render

import (
	"strings"
	"testing"

	"kb/internal/kb"
)

func ptr(s string) *string { return &s }

func TestAssemble_SingleArtifact(t *testing.T) {
	arts := []*kb.Artifact{{
		Title:        "Go Generics",
		CanonicalURL: ptr("https://go.dev/blog/generics"),
		BodyMarkdown: "Type parameters let you write **reusable** code.",
	}}

	got := Assemble(arts, AssembleOpts{Title: "My Export"})

	want := "# My Export\n\n" +
		"## Go Generics\n\n" +
		"<https://go.dev/blog/generics>\n\n" +
		"Type parameters let you write **reusable** code.\n"
	if got != want {
		t.Errorf("Assemble mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestAssemble_DefaultTitleAndUntitledArtifact(t *testing.T) {
	got := Assemble([]*kb.Artifact{{BodyMarkdown: "body"}}, AssembleOpts{})
	if !strings.HasPrefix(got, "# Knowledge Base Export\n") {
		t.Errorf("missing default document title: %q", got)
	}
	if !strings.Contains(got, "## (untitled)\n") {
		t.Errorf("an empty artifact title should render as (untitled): %q", got)
	}
}

func TestAssemble_TitleNewlinesCollapsed(t *testing.T) {
	// A heading must stay on one line; a multi-line title would otherwise break
	// the document structure (and any downstream pandoc parse).
	got := Assemble([]*kb.Artifact{{Title: "Line one\nLine two\tindented"}}, AssembleOpts{})
	if !strings.Contains(got, "## Line one Line two indented\n") {
		t.Errorf("title whitespace not collapsed to a single line: %q", got)
	}
	if strings.Contains(got, "## Line one\n") {
		t.Errorf("title newline leaked into the document: %q", got)
	}
}

func TestAssemble_URLWithSpaceIsNotABrokenAutolink(t *testing.T) {
	// A bare CommonMark autolink <...> may not contain spaces; a URL with one
	// must fall back to an inline link so the document still parses.
	got := Assemble([]*kb.Artifact{{Title: "T", CanonicalURL: ptr("https://ex.com/a b")}}, AssembleOpts{Title: "D"})
	if strings.Contains(got, "\n<https://ex.com/a b>\n") {
		t.Errorf("emitted an invalid space-bearing autolink on its own line: %q", got)
	}
	if !strings.Contains(got, "[https://ex.com/a b](<https://ex.com/a b>)") {
		t.Errorf("expected an inline link for a space-bearing URL: %q", got)
	}
}

func TestAssemble_URLWithAngleBracketIsEscaped(t *testing.T) {
	// A '>' would close an autolink early and spill the rest into body text.
	got := Assemble([]*kb.Artifact{{Title: "T", CanonicalURL: ptr("https://ex.com/a>b")}}, AssembleOpts{Title: "D"})
	if strings.Contains(got, "<https://ex.com/a>b>") {
		t.Errorf("'>' in URL left the autolink unescaped: %q", got)
	}
	if !strings.Contains(got, `\>`) {
		t.Errorf("expected the '>' in the link destination to be escaped: %q", got)
	}
}

func TestAssemble_NoURLNoBody(t *testing.T) {
	got := Assemble([]*kb.Artifact{{Title: "Just a heading"}}, AssembleOpts{Title: "Doc"})
	want := "# Doc\n\n## Just a heading\n"
	if got != want {
		t.Errorf("Assemble of a bodyless, urlless artifact:\n got %q\nwant %q", got, want)
	}
}

func TestAssemble_BodyTrimmed(t *testing.T) {
	got := Assemble([]*kb.Artifact{{Title: "T", BodyMarkdown: "\n\n  spaced body  \n\n"}}, AssembleOpts{Title: "D"})
	want := "# D\n\n## T\n\nspaced body\n"
	if got != want {
		t.Errorf("body not trimmed:\n got %q\nwant %q", got, want)
	}
}

func TestAssemble_EmptySet(t *testing.T) {
	got := Assemble(nil, AssembleOpts{Title: "Empty"})
	if got != "# Empty\n" {
		t.Errorf("empty render should be just the title: %q", got)
	}
}

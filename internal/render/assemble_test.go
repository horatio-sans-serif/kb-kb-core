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

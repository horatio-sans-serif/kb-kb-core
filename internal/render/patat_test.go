package render

import (
	"context"
	"os"
	"strings"
	"testing"

	"kb/internal/kb"
)

func TestPatat_DeckStructure(t *testing.T) {
	dir := t.TempDir()
	p := NewPatat(dir)
	if p.Format() != "patat" {
		t.Fatalf("Format = %q, want patat", p.Format())
	}

	arts := []*kb.Artifact{
		{Title: "One", BodyMarkdown: "first", PrivacyTier: kb.Public},
		{Title: "Two", BodyMarkdown: "second", PrivacyTier: kb.Sensitive},
	}
	out, err := p.Render(context.Background(), arts, kb.RenderOpts{Options: map[string]string{"name": "01HX", "title": "My Deck"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out.PrivacyMax != kb.Sensitive {
		t.Errorf("PrivacyMax = %q, want sensitive", out.PrivacyMax)
	}

	b, err := os.ReadFile(out.OutputPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	deck := string(b)

	// A YAML metadata block carrying the title and a patat config opens the deck.
	if !strings.HasPrefix(deck, "---\n") || !strings.Contains(deck, `title: "My Deck"`) || !strings.Contains(deck, "patat:") {
		t.Errorf("missing patat metadata header:\n%s", deck)
	}
	// Two artifacts => exactly one slide separator between them.
	if n := strings.Count(deck, "\n\n---\n\n"); n != 1 {
		t.Errorf("slide separators = %d, want 1 for two slides:\n%s", n, deck)
	}
	if !strings.Contains(deck, "## One") || !strings.Contains(deck, "first") ||
		!strings.Contains(deck, "## Two") || !strings.Contains(deck, "second") {
		t.Errorf("slide content missing:\n%s", deck)
	}
}

func TestPatat_BodyThematicBreakDoesNotSplitSlides(t *testing.T) {
	// A '---' rule inside an artifact body would otherwise be read by patat as a
	// slide break, fracturing one artifact across slides. It must be neutralized.
	dir := t.TempDir()
	p := NewPatat(dir)
	arts := []*kb.Artifact{{Title: "Solo", BodyMarkdown: "above\n\n---\n\nbelow", PrivacyTier: kb.Public}}
	out, err := p.Render(context.Background(), arts, kb.RenderOpts{Options: map[string]string{"name": "r"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	deck, _ := os.ReadFile(out.OutputPath)
	if n := strings.Count(string(deck), "\n\n---\n\n"); n != 0 {
		t.Errorf("a single artifact must yield no slide separators, got %d:\n%s", n, deck)
	}
	if !strings.Contains(string(deck), "above") || !strings.Contains(string(deck), "below") {
		t.Errorf("body content lost during neutralization:\n%s", deck)
	}
}

func TestPatat_SetextUnderlineNotNeutralized(t *testing.T) {
	// A '---' directly under a text line is a setext H2 underline, not a break.
	// Neutralizing it would silently demote a heading to plain text.
	dir := t.TempDir()
	out, err := NewPatat(dir).Render(context.Background(),
		[]*kb.Artifact{{Title: "T", BodyMarkdown: "My Heading\n---\n\nbody", PrivacyTier: kb.Public}},
		kb.RenderOpts{Options: map[string]string{"name": "r"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	deck, _ := os.ReadFile(out.OutputPath)
	if !strings.Contains(string(deck), "My Heading\n---") {
		t.Errorf("setext underline was wrongly neutralized:\n%s", deck)
	}
}

func TestPatat_ThematicBreakVariantsNeutralized(t *testing.T) {
	cases := map[string]string{
		"stars":         "para\n\n***\n\nmore",
		"underscores":   "para\n\n___\n\nmore",
		"spaced dashes": "para\n\n- - -\n\nmore",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := NewPatat(t.TempDir()).Render(context.Background(),
				[]*kb.Artifact{{Title: "T", BodyMarkdown: body, PrivacyTier: kb.Public}},
				kb.RenderOpts{Options: map[string]string{"name": "r"}})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			deck, _ := os.ReadFile(out.OutputPath)
			if n := strings.Count(string(deck), "\n\n---\n\n"); n != 0 {
				t.Errorf("a body break (%s) leaked a slide separator:\n%s", name, deck)
			}
		})
	}
}

func TestPatat_IndentedRuleIsCodeNotBreak(t *testing.T) {
	// Four-space-indented content is a code block; the line must be left as-is.
	out, err := NewPatat(t.TempDir()).Render(context.Background(),
		[]*kb.Artifact{{Title: "T", BodyMarkdown: "para\n\n    ---\n\nmore", PrivacyTier: kb.Public}},
		kb.RenderOpts{Options: map[string]string{"name": "r"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	deck, _ := os.ReadFile(out.OutputPath)
	if !strings.Contains(string(deck), "    ---") {
		t.Errorf("indented code line was wrongly neutralized:\n%s", deck)
	}
}

func TestPatat_EmptyDeckIsHeaderOnly(t *testing.T) {
	out, err := NewPatat(t.TempDir()).Render(context.Background(), nil,
		kb.RenderOpts{Options: map[string]string{"name": "r", "title": "Empty"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	deck, _ := os.ReadFile(out.OutputPath)
	if strings.Contains(string(deck), "\n\n---\n\n") || strings.Contains(string(deck), "##") {
		t.Errorf("empty deck should be the metadata header only:\n%s", deck)
	}
	if !strings.Contains(string(deck), `title: "Empty"`) {
		t.Errorf("empty deck missing title:\n%s", deck)
	}
}

func TestPatat_SlideHasSourceLink(t *testing.T) {
	out, err := NewPatat(t.TempDir()).Render(context.Background(),
		[]*kb.Artifact{{Title: "Linked", CanonicalURL: ptr("https://ex.com/x"), BodyMarkdown: "b", PrivacyTier: kb.Public}},
		kb.RenderOpts{Options: map[string]string{"name": "r"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	deck, _ := os.ReadFile(out.OutputPath)
	if !strings.Contains(string(deck), "<https://ex.com/x>") {
		t.Errorf("slide missing source link:\n%s", deck)
	}
}

func TestPatat_TitleIsYAMLQuoted(t *testing.T) {
	// A title with a colon/quote would break the YAML header if not quoted/escaped.
	dir := t.TempDir()
	out, err := NewPatat(dir).Render(context.Background(), nil,
		kb.RenderOpts{Options: map[string]string{"name": "r", "title": `Ratios: "1:2"`}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	deck, _ := os.ReadFile(out.OutputPath)
	if !strings.Contains(string(deck), `title: "Ratios: \"1:2\""`) {
		t.Errorf("title not YAML-escaped:\n%s", deck)
	}
}

func TestPatat_OutputIsOwnerOnly(t *testing.T) {
	out, err := NewPatat(t.TempDir()).Render(context.Background(),
		[]*kb.Artifact{{Title: "x", PrivacyTier: kb.Restricted}},
		kb.RenderOpts{Options: map[string]string{"name": "r"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	info, err := os.Stat(out.OutputPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("output mode = %o, want owner-only", perm)
	}
}

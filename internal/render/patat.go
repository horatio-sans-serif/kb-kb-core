package render

import (
	"context"
	"strings"

	"kb/internal/kb"
)

// Patat renders artifacts as a patat terminal-slide deck (design §8.14): a
// Markdown file with a YAML metadata header (title + patat config) and one slide
// per artifact, slides separated by horizontal rules. It writes the deck the user
// then presents with `patat <file>` — patat itself is an interactive presenter,
// not a converter, so there is no subprocess here, only assembly.
type Patat struct {
	dir string
}

// NewPatat returns a patat-deck renderer writing into dir.
func NewPatat(dir string) *Patat { return &Patat{dir: dir} }

// Format identifies this renderer in the registry.
func (p *Patat) Format() string { return "patat" }

// Render assembles a slide deck and writes it owner-only to <dir>/<name>.patat.md.
func (p *Patat) Render(_ context.Context, arts []*kb.Artifact, opts kb.RenderOpts) (kb.Rendered, error) {
	path, err := outputPath(p.dir, opts.Options["name"], "patat.md")
	if err != nil {
		return kb.Rendered{}, err
	}
	if err := writeFile(path, []byte(p.deck(arts, opts.Options["title"]))); err != nil {
		return kb.Rendered{}, err
	}
	return kb.Rendered{Format: p.Format(), OutputPath: path, PrivacyMax: maxTier(arts)}, nil
}

// deck builds the full patat document: metadata header, then the slides.
func (p *Patat) deck(arts []*kb.Artifact, title string) string {
	var b strings.Builder
	// patat reads a leading YAML metadata block; `wrap: true` reflows long lines to
	// the terminal width. The block's own --- delimiters are consumed as metadata,
	// not slide breaks.
	b.WriteString("---\ntitle: ")
	b.WriteString(yamlString(oneLine(title)))
	b.WriteString("\npatat:\n  wrap: true\n---\n")

	slides := make([]string, 0, len(arts))
	for _, a := range arts {
		slides = append(slides, slide(a))
	}
	if len(slides) > 0 {
		b.WriteString("\n")
		// Slides are joined by a blank-line-padded horizontal rule, patat's slide
		// separator. Body thematic breaks are neutralized in slide(), so the only
		// rules in the document are these separators.
		b.WriteString(strings.Join(slides, "\n\n---\n\n"))
	}
	b.WriteString("\n")
	return b.String()
}

// slide renders one artifact as a single slide: an H2 title, the source link, and
// the body with any internal thematic break neutralized so it cannot split slides.
func slide(a *kb.Artifact) string {
	heading := oneLine(a.Title)
	if heading == "" {
		heading = "(untitled)"
	}
	parts := []string{"## " + heading}
	if a.CanonicalURL != nil {
		if link := urlMarkdown(*a.CanonicalURL); link != "" {
			parts = append(parts, link)
		}
	}
	if body := strings.TrimSpace(a.BodyMarkdown); body != "" {
		parts = append(parts, neutralizeRules(body))
	}
	return strings.Join(parts, "\n\n")
}

// neutralizeRules replaces any line that is a Markdown thematic break with a
// visual em-dash rule that is not a break, so a body's own horizontal rule does
// not register as a patat slide separator. A break is only neutralized when the
// preceding line is blank (or it is the first line) — that is the only position a
// rule can be a thematic break / slide separator; a `---` directly under a text
// line is a setext heading underline and must be left intact.
func neutralizeRules(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		precededByBlank := i == 0 || strings.TrimSpace(lines[i-1]) == ""
		if precededByBlank && isThematicBreak(line) {
			lines[i] = "———"
		}
	}
	return strings.Join(lines, "\n")
}

// isThematicBreak reports whether line is a CommonMark thematic break: at most 3
// leading spaces (4+ or a leading tab is indented code, not a break), then three
// or more of a single character among -, _, * with optional internal spaces/tabs.
func isThematicBreak(line string) bool {
	body := strings.TrimLeft(line, " ")
	if len(line)-len(body) > 3 || strings.HasPrefix(line, "\t") {
		return false
	}
	stripped := strings.NewReplacer(" ", "", "\t", "").Replace(strings.TrimRight(body, " \t"))
	if len(stripped) < 3 {
		return false
	}
	c := stripped[0]
	if c != '-' && c != '_' && c != '*' {
		return false
	}
	return strings.Count(stripped, string(c)) == len(stripped)
}

// yamlString renders s as a double-quoted YAML scalar. It is self-sufficient: it
// escapes backslashes and quotes, encodes newlines/tabs/CR as their YAML escapes
// (keeping the scalar on one line), and drops other control characters — so any
// title is safe in the single-line metadata header regardless of the caller.
func yamlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				b.WriteByte(' ') // drop other control chars
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

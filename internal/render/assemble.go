// Package render is the output layer (design §8.14): it turns a set of artifacts
// into a single Markdown document, then hands that document to a Renderer for the
// requested format. Because every artifact body is already Markdown, the whole
// layer is built on one contract — artifact(s) -> assembled Markdown -> format —
// so a new output format is just a new Renderer over the same assembled document.
package render

import (
	"strings"
	"unicode"

	"kb/internal/kb"
)

// defaultDocTitle is the document H1 when the caller names none.
const defaultDocTitle = "Knowledge Base Export"

// AssembleOpts controls how a document is assembled from artifacts.
type AssembleOpts struct {
	Title string // the document H1; defaults to defaultDocTitle when empty
}

// Assemble renders artifacts into a single Markdown document: an H1 document
// title, then one section per artifact — an H2 of the artifact title, the source
// URL when present, and the artifact body. Blocks are separated by blank lines so
// the result is valid CommonMark (and parses cleanly downstream in pandoc). The
// artifact order is preserved; the caller decides it.
//
// Bodies are emitted verbatim (they are trusted, authored Markdown), so a body's
// own headings keep their authored level — a body starting with `# Foo` yields a
// second H1 alongside the document title. That is content fidelity, not a bug;
// downstream formats that need a single H1 should set their own title.
func Assemble(arts []*kb.Artifact, opts AssembleOpts) string {
	title := strings.TrimSpace(opts.Title)
	if title == "" {
		title = defaultDocTitle
	}

	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(oneLine(title))
	b.WriteString("\n")

	for _, a := range arts {
		heading := oneLine(a.Title)
		if heading == "" {
			heading = "(untitled)"
		}
		b.WriteString("\n## ")
		b.WriteString(heading)
		b.WriteString("\n")

		if a.CanonicalURL != nil {
			if link := urlMarkdown(*a.CanonicalURL); link != "" {
				b.WriteString("\n")
				b.WriteString(link)
				b.WriteString("\n")
			}
		}

		if body := strings.TrimSpace(a.BodyMarkdown); body != "" {
			b.WriteString("\n")
			b.WriteString(body)
			b.WriteString("\n")
		}
	}

	return b.String()
}

// oneLine collapses all runs of whitespace (including newlines and tabs) to a
// single space and trims the ends, so a value used in a Markdown heading cannot
// break out of its line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// urlMarkdown renders a source URL as valid CommonMark. A clean URL becomes a
// bare autolink (<url>); one that an autolink can't hold — a space or an angle
// bracket would either be illegal or close the autolink early and spill into body
// text — falls back to an inline link with an angle-bracket destination, escaping
// every character that could break out of the link. Returns "" for a blank URL.
func urlMarkdown(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	if safeAutolinkURL(u) {
		return "<" + u + ">"
	}
	return "[" + escapeLinkText(u) + "](<" + escapeAngleDest(u) + ">)"
}

// safeAutolinkURL reports whether u may be emitted as a bare CommonMark autolink:
// an absolute URI (has a scheme) with no whitespace, control characters, or angle
// brackets.
func safeAutolinkURL(u string) bool {
	if !strings.Contains(u, ":") {
		return false
	}
	for _, r := range u {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '<' || r == '>' {
			return false
		}
	}
	return true
}

// escapeLinkText backslash-escapes the characters that would terminate or
// restructure inline link text.
func escapeLinkText(s string) string {
	return strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`).Replace(s)
}

// escapeAngleDest escapes a URL for an angle-bracket link destination `(<...>)`:
// line endings are illegal there (dropped), and `\`, `<`, `>` are backslash-
// escaped. Spaces are permitted inside `<>`, so they are left intact.
func escapeAngleDest(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return strings.NewReplacer(`\`, `\\`, `<`, `\<`, `>`, `\>`).Replace(s)
}

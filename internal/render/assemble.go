// Package render is the output layer (design §8.14): it turns a set of artifacts
// into a single Markdown document, then hands that document to a Renderer for the
// requested format. Because every artifact body is already Markdown, the whole
// layer is built on one contract — artifact(s) -> assembled Markdown -> format —
// so a new output format is just a new Renderer over the same assembled document.
package render

import (
	"strings"

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
// URL as an autolink when present, and the artifact body. Blocks are separated by
// blank lines so the result is valid CommonMark (and parses cleanly downstream in
// pandoc). The artifact order is preserved; the caller decides it.
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
			if u := strings.TrimSpace(*a.CanonicalURL); u != "" {
				b.WriteString("\n<")
				b.WriteString(u)
				b.WriteString(">\n")
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

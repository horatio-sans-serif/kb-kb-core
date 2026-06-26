package topics

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"kb/internal/kb"
	"kb/internal/llm"
	"kb/internal/store"
	"kb/internal/textutil"
)

const (
	// labelMembers is how many representative member artifacts inform a topic's
	// LLM-derived label — enough signal without overrunning the context window.
	labelMembers = 8
	// maxMemberExcerptBytes caps each member's title fed to the model.
	maxMemberExcerptBytes = 200
)

// Labeler (re)labels a topic with a concise, human-readable label derived by the
// on-device LLM from the topic's representative member artifacts, replacing the
// founding artifact's truncated title (design §8.7 backlog).
//
// It mirrors the summarize/rag privacy discipline: an LLM-derived label is only
// produced when the configured endpoint is on-device (local). A topic's members
// can include sensitive/restricted artifacts, so when the endpoint is remote the
// stage skips entirely and the title-derived fallback label is kept — no member
// text leaves the machine. It is best-effort: with no LLM client, an LLM error,
// or an empty/unusable model response, the fallback label is left in place.
type Labeler struct {
	st     *store.Store
	client llm.Client
	model  string
	local  bool
}

// NewLabeler builds a topic labeler. local must reflect whether client's endpoint
// is on-device (see config.Config.OllamaIsLocal); it governs the privacy gate.
// A nil client cleanly yields the title fallback (Label is a no-op).
func NewLabeler(st *store.Store, client llm.Client, model string, local bool) *Labeler {
	return &Labeler{st: st, client: client, model: model, local: local}
}

// Label asks the on-device LLM for a concise label for topicID using its member
// artifacts and, on success, updates the topic's stored label. fallback is the
// title-derived label to keep when LLM labeling is unavailable or fails; it is
// never overwritten in those cases (the caller has already stored it). Returns nil
// in the fallback paths — labeling is best-effort and must not fail a sweep.
func (l *Labeler) Label(ctx context.Context, topicID, fallback string) error {
	// Privacy + availability gates: a remote endpoint or absent client means no
	// member text may be sent, so keep the title fallback already on the topic.
	if l.client == nil || !l.local {
		return nil
	}

	members, err := l.st.Topics.Members(ctx, topicID, labelMembers, kb.Restricted)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return nil // nothing to label from
	}

	out, err := l.generate(ctx, members)
	if err != nil {
		return nil // best-effort: keep the fallback label
	}
	clean := clampLabel(out)
	if clean == "" {
		return nil // unusable model output: keep the fallback
	}
	_ = fallback // fallback is already persisted; we only overwrite on success
	return l.st.Topics.SetLabel(ctx, topicID, clean)
}

func (l *Labeler) generate(ctx context.Context, members []store.TopicMember) (string, error) {
	var titles strings.Builder
	for i, m := range members {
		title := strings.TrimSpace(m.Title)
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&titles, "%d. %s\n", i+1, textutil.TruncateBytes(title, maxMemberExcerptBytes))
	}
	system := "You name clusters of related documents. Given a list of document titles " +
		"from one cluster, return a single concise topic label (a short noun phrase, " +
		"Title Case, no quotes, no trailing punctuation) that captures their common theme. " +
		"Reply with ONLY the label."
	prompt := fmt.Sprintf("Documents in this cluster:\n%s\nTopic label:", titles.String())
	return l.client.Generate(ctx, llm.GenerateRequest{Model: l.model, System: system, Prompt: prompt})
}

// clampLabel sanitizes a model's label to a single line of at most maxLabelLen
// bytes: it takes the first non-blank line, strips surrounding quotes and
// whitespace, collapses internal whitespace, and truncates on a rune boundary.
// Returns "" when nothing usable remains (the caller then keeps the fallback).
func clampLabel(s string) string {
	line := ""
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			line = ln
			break
		}
	}
	line = strings.TrimSpace(line)
	line = strings.Trim(line, `"'`)
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return ""
	}
	if len(line) > maxLabelLen {
		line = line[:maxLabelLen]
		for len(line) > 0 && !utf8.ValidString(line) {
			line = line[:len(line)-1] // back off to the last whole rune
		}
		line = strings.TrimSpace(line)
	}
	return line
}

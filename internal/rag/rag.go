// Package rag is retrieval-augmented question answering over the knowledge base
// (design §8.8): retrieve relevant artifacts, ground a local LLM on their content,
// and synthesize an answer with mandatory citations back to the source artifacts.
//
// It requires an on-device LLM: synthesis sends artifact bodies AND the user's
// question to the model, exactly the content the privacy gate keeps off a remote
// endpoint (§8.17). A non-local endpoint is refused rather than partially
// answered, since silently dropping sensitive sources would yield a misleading
// answer.
package rag

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"kb/internal/llm"
	"kb/internal/search"
	"kb/internal/store"
)

const (
	defaultK = 5
	maxK     = 20
	// maxExcerptBytes caps each source's body in the prompt so a few long
	// artifacts don't blow the context window. Trimmed on a rune boundary.
	maxExcerptBytes = 4000
)

// ErrLocalRequired is returned when the LLM endpoint is not on-device: answering
// would send artifact bodies and the question to a remote model. The API maps it
// to 503.
var ErrLocalRequired = errors.New("rag: question answering requires an on-device LLM endpoint")

// Retriever returns ranked artifacts for a query (satisfied by *search.Searcher).
type Retriever interface {
	Hybrid(ctx context.Context, query string, k int) ([]search.Result, error)
}

// Source is one artifact the answer is grounded on; its position in Answer.Sources
// (1-based) is the [n] the synthesized answer cites.
type Source struct {
	ArtifactID string  `json:"artifact_id"`
	Title      string  `json:"title,omitempty"`
	URL        string  `json:"url,omitempty"`
	Score      float64 `json:"score"`
}

// Answer is a synthesized response plus the sources it cites.
type Answer struct {
	Query   string   `json:"query"`
	Answer  string   `json:"answer"`
	Sources []Source `json:"sources"`
}

// Engine answers questions over retrieved artifacts.
type Engine struct {
	retriever Retriever
	store     *store.Store
	client    llm.Client
	model     string
	local     bool
}

// NewEngine builds a RAG engine. local must reflect whether the LLM endpoint is
// on-device (see config.Config.OllamaIsLocal); it governs whether answering is
// permitted at all.
func NewEngine(retriever Retriever, st *store.Store, client llm.Client, model string, local bool) *Engine {
	return &Engine{retriever: retriever, store: st, client: client, model: model, local: local}
}

// Answer retrieves the top-k artifacts for the query, grounds the local LLM on
// their content, and returns the synthesized answer with its cited sources. With
// no relevant sources it returns a stock answer and never calls the model.
func (e *Engine) Answer(ctx context.Context, query string, k int) (Answer, error) {
	if !e.local {
		return Answer{}, ErrLocalRequired
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return Answer{}, fmt.Errorf("rag: empty query")
	}
	switch {
	case k <= 0:
		k = defaultK
	case k > maxK:
		k = maxK
	}

	hits, err := e.retriever.Hybrid(ctx, query, k)
	if err != nil {
		return Answer{}, err
	}

	var sources []Source
	var prompt strings.Builder
	for _, h := range hits {
		a, err := e.store.Artifacts.Get(ctx, h.ArtifactID)
		if err != nil {
			continue // artifact gone between ranking and load — drop it
		}
		n := len(sources) + 1
		src := Source{ArtifactID: a.ID, Title: a.Title, Score: h.Score}
		if a.CanonicalURL != nil {
			src.URL = *a.CanonicalURL
		}
		sources = append(sources, src)
		fmt.Fprintf(&prompt, "[%d] %s\n%s\n\n", n, a.Title, truncateBytes(a.BodyMarkdown, maxExcerptBytes))
	}

	if len(sources) == 0 {
		return Answer{Query: query, Answer: "I don't have any relevant sources to answer that.", Sources: nil}, nil
	}

	answer, err := e.synthesize(ctx, query, prompt.String())
	if err != nil {
		return Answer{}, err
	}
	return Answer{Query: query, Answer: answer, Sources: sources}, nil
}

func (e *Engine) synthesize(ctx context.Context, query, sources string) (string, error) {
	system := "You answer questions using ONLY the numbered sources provided. " +
		"Cite every claim inline with [n] referencing the source number. If the sources " +
		"do not contain the answer, say you don't have enough information. Be concise and factual."
	prompt := fmt.Sprintf("Sources:\n%s\nQuestion: %s\n\nAnswer (with inline [n] citations):", sources, query)
	return e.client.Generate(ctx, llm.GenerateRequest{Model: e.model, System: system, Prompt: prompt})
}

// truncateBytes caps s to n bytes without splitting a UTF-8 rune.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.RuneStart(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	if r, size := utf8.DecodeLastRuneInString(s); r == utf8.RuneError && size <= 1 {
		s = s[:len(s)-size]
	}
	return s
}

// Package llm is the local large-language-model client (design Decision 2 /
// §8.5). Enrichers depend on the Client interface, not a concrete server, so they
// test against a fake while the daemon wires a local Ollama. Local by default:
// on-device generation means even sensitive/restricted artifacts can be processed
// without anything leaving the machine (Principle 7).
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxResponseBytes caps how much of a model response is read, bounding memory
// against a runaway or hostile local server.
const maxResponseBytes = 16 << 20

// GenerateRequest is one completion request. Format is passed straight through to
// Ollama's "format" field: the JSON token `"json"` for free-form JSON, or a JSON
// schema object for structured output; nil for plain text.
type GenerateRequest struct {
	Model  string
	System string
	Prompt string
	Format json.RawMessage
}

// Client is the minimal local-LLM surface the enrichers use.
type Client interface {
	Generate(ctx context.Context, req GenerateRequest) (string, error)
}

// Ollama calls a local Ollama server's /api/generate endpoint.
type Ollama struct {
	baseURL string
	client  *http.Client
}

// Option configures an Ollama client.
type Option func(*Ollama)

// WithHTTPClient injects the HTTP client (tests pass an httptest client).
func WithHTTPClient(c *http.Client) Option { return func(o *Ollama) { o.client = c } }

// NewOllama builds a client for the Ollama server at baseURL (e.g.
// http://127.0.0.1:11434). The default timeout is generous because local
// generation on CPU can be slow.
func NewOllama(baseURL string, opts ...Option) *Ollama {
	o := &Ollama{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 5 * time.Minute},
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

type ollamaGenerateRequest struct {
	Model  string          `json:"model"`
	Prompt string          `json:"prompt"`
	System string          `json:"system,omitempty"`
	Stream bool            `json:"stream"`
	Format json.RawMessage `json:"format,omitempty"`
}

type ollamaGenerateResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
	Error    string `json:"error,omitempty"`
}

// Generate runs a single non-streaming completion and returns the model's text.
// An empty model is rejected before any network call. A non-2xx status, an
// `error` field in a 200 body, and decode failures are all errors.
func (o *Ollama) Generate(ctx context.Context, req GenerateRequest) (string, error) {
	if req.Model == "" {
		return "", fmt.Errorf("llm: model is required")
	}
	body, err := json.Marshal(ollamaGenerateRequest{
		Model:  req.Model,
		Prompt: req.Prompt,
		System: req.System,
		Stream: false,
		Format: req.Format,
	})
	if err != nil {
		return "", fmt.Errorf("llm: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("llm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("llm: call ollama: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("llm: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm: ollama status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out ollamaGenerateResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("llm: decode response: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("llm: ollama error: %s", out.Error)
	}
	return out.Response, nil
}

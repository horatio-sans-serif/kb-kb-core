package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOllama_Generate(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":"hello world","done":true}`)
	}))
	defer srv.Close()

	c := NewOllama(srv.URL, WithHTTPClient(srv.Client()))
	out, err := c.Generate(context.Background(), GenerateRequest{
		Model:  "test-model",
		System: "be terse",
		Prompt: "say hi",
		Format: json.RawMessage(`"json"`),
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if out != "hello world" {
		t.Errorf("response = %q, want %q", out, "hello world")
	}
	if gotPath != "/api/generate" {
		t.Errorf("path = %q, want /api/generate", gotPath)
	}
	if gotBody["model"] != "test-model" || gotBody["prompt"] != "say hi" || gotBody["system"] != "be terse" {
		t.Errorf("request body = %v, want model/prompt/system set", gotBody)
	}
	if stream, ok := gotBody["stream"].(bool); !ok || stream {
		t.Errorf("stream = %v, want false (non-streaming)", gotBody["stream"])
	}
	if gotBody["format"] != "json" {
		t.Errorf("format = %v, want \"json\" passed through", gotBody["format"])
	}
}

func TestOllama_Generate_RequiresModel(t *testing.T) {
	c := NewOllama("http://127.0.0.1:0")
	if _, err := c.Generate(context.Background(), GenerateRequest{Prompt: "x"}); err == nil {
		t.Error("an empty model should be an error before any network call")
	}
}

func TestOllama_Generate_Non200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"model not found"}`)
	}))
	defer srv.Close()
	c := NewOllama(srv.URL, WithHTTPClient(srv.Client()))
	_, err := c.Generate(context.Background(), GenerateRequest{Model: "m", Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "model not found") {
		t.Errorf("err = %v, want it to surface the server error body", err)
	}
}

func TestOllama_Generate_BodyErrorField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"error":"out of memory"}`)
	}))
	defer srv.Close()
	c := NewOllama(srv.URL, WithHTTPClient(srv.Client()))
	if _, err := c.Generate(context.Background(), GenerateRequest{Model: "m", Prompt: "x"}); err == nil {
		t.Error("a 200 response carrying an error field should be an error")
	}
}

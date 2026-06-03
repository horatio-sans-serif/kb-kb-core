package config

import (
	"strings"
	"testing"
)

func envFunc(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load(envFunc(map[string]string{
		"KB_DB":        "/tmp/kb.db",
		"KB_API_TOKEN": "0123456789abcdef0123456789abcdef",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DBPath != "/tmp/kb.db" || cfg.APIToken != "0123456789abcdef0123456789abcdef" {
		t.Errorf("got %+v, want DBPath/APIToken from env", cfg)
	}
	if cfg.ListenAddr != "127.0.0.1:8087" {
		t.Errorf("ListenAddr = %q, want the localhost default", cfg.ListenAddr)
	}
	if cfg.DefaultPrivacy != "personal" {
		t.Errorf("DefaultPrivacy = %q, want personal", cfg.DefaultPrivacy)
	}
	if cfg.CloudLLMEnabled {
		t.Error("CloudLLMEnabled = true, want false by default")
	}
	if cfg.OllamaURL != "http://127.0.0.1:11434" {
		t.Errorf("OllamaURL = %q, want the localhost Ollama default", cfg.OllamaURL)
	}
	if cfg.SummarizeModel != "llama3.2" {
		t.Errorf("SummarizeModel = %q, want the default model", cfg.SummarizeModel)
	}
	if cfg.EmbedModel != "nomic-embed-text" {
		t.Errorf("EmbedModel = %q, want the default embed model", cfg.EmbedModel)
	}
}

func TestConfig_RenderDirDefaultsBesideDB(t *testing.T) {
	cfg, err := Load(envFunc(map[string]string{
		"KB_DB":        "/var/lib/kb/kb.db",
		"KB_API_TOKEN": "0123456789abcdef0123456789abcdef",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RenderDir != "/var/lib/kb/renders" {
		t.Errorf("RenderDir = %q, want a 'renders' dir beside the DB", cfg.RenderDir)
	}
}

func TestConfig_RenderDirOverride(t *testing.T) {
	cfg, err := Load(envFunc(map[string]string{
		"KB_DB":         "/tmp/kb.db",
		"KB_API_TOKEN":  "0123456789abcdef0123456789abcdef",
		"KB_RENDER_DIR": "/exports",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RenderDir != "/exports" {
		t.Errorf("RenderDir = %q, want the override", cfg.RenderDir)
	}
}

func TestConfig_MissingTokenFails(t *testing.T) {
	_, err := Load(envFunc(map[string]string{"KB_DB": "/tmp/kb.db"}))
	if err == nil {
		t.Fatal("expected an error when KB_API_TOKEN is unset")
	}
	if !strings.Contains(err.Error(), "KB_API_TOKEN") {
		t.Errorf("error should name the missing variable: %v", err)
	}
}

func TestConfig_ShortTokenFails(t *testing.T) {
	_, err := Load(envFunc(map[string]string{"KB_DB": "/tmp/kb.db", "KB_API_TOKEN": "short"}))
	if err == nil || !strings.Contains(err.Error(), "KB_API_TOKEN") {
		t.Errorf("expected a min-length error for a short token, got %v", err)
	}
}

func TestConfig_MissingDBFails(t *testing.T) {
	_, err := Load(envFunc(map[string]string{"KB_API_TOKEN": "0123456789abcdef0123456789abcdef"}))
	if err == nil || !strings.Contains(err.Error(), "KB_DB") {
		t.Errorf("expected an error naming KB_DB, got %v", err)
	}
}

func TestConfig_InvalidPrivacyFails(t *testing.T) {
	_, err := Load(envFunc(map[string]string{
		"KB_DB":              "/tmp/kb.db",
		"KB_API_TOKEN":       "0123456789abcdef0123456789abcdef",
		"KB_DEFAULT_PRIVACY": "bogus",
	}))
	if err == nil || !strings.Contains(err.Error(), "KB_DEFAULT_PRIVACY") {
		t.Errorf("expected an error naming KB_DEFAULT_PRIVACY, got %v", err)
	}
}

func TestConfig_CloudLLMParsed(t *testing.T) {
	cfg, err := Load(envFunc(map[string]string{
		"KB_DB":        "/tmp/kb.db",
		"KB_API_TOKEN": "0123456789abcdef0123456789abcdef",
		"KB_CLOUD_LLM": "true",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.CloudLLMEnabled {
		t.Error("CloudLLMEnabled = false, want true when KB_CLOUD_LLM=true")
	}
}

func TestConfig_DefaultOllamaIsLocal(t *testing.T) {
	cfg, err := Load(envFunc(map[string]string{
		"KB_DB":        "/tmp/kb.db",
		"KB_API_TOKEN": "0123456789abcdef0123456789abcdef",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.OllamaIsLocal {
		t.Error("OllamaIsLocal = false for the loopback default, want true")
	}
}

func TestConfig_RemoteOllamaRejectedWithoutOptIn(t *testing.T) {
	_, err := Load(envFunc(map[string]string{
		"KB_DB":         "/tmp/kb.db",
		"KB_API_TOKEN":  "0123456789abcdef0123456789abcdef",
		"KB_OLLAMA_URL": "http://ollama.example.com:11434",
	}))
	if err == nil || !strings.Contains(err.Error(), "KB_ALLOW_REMOTE_OLLAMA") {
		t.Errorf("a non-loopback Ollama URL should fail and name the opt-in, got %v", err)
	}
}

func TestConfig_RemoteOllamaAllowedWithOptIn(t *testing.T) {
	cfg, err := Load(envFunc(map[string]string{
		"KB_DB":                  "/tmp/kb.db",
		"KB_API_TOKEN":           "0123456789abcdef0123456789abcdef",
		"KB_OLLAMA_URL":          "http://ollama.example.com:11434",
		"KB_ALLOW_REMOTE_OLLAMA": "true",
	}))
	if err != nil {
		t.Fatalf("Load with opt-in: %v", err)
	}
	if cfg.OllamaIsLocal {
		t.Error("OllamaIsLocal = true for a remote URL, want false (gates sensitive tiers)")
	}
}

func TestConfig_InterestsParsed(t *testing.T) {
	cfg, err := Load(envFunc(map[string]string{
		"KB_DB":        "/tmp/kb.db",
		"KB_API_TOKEN": "0123456789abcdef0123456789abcdef",
		"KB_INTERESTS": " go async ; ; distributed systems ;",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Interests) != 2 || cfg.Interests[0] != "go async" || cfg.Interests[1] != "distributed systems" {
		t.Errorf("Interests = %q, want [go async, distributed systems] (trimmed, empties dropped)", cfg.Interests)
	}
}

func TestConfig_InvalidOllamaURLFails(t *testing.T) {
	_, err := Load(envFunc(map[string]string{
		"KB_DB":         "/tmp/kb.db",
		"KB_API_TOKEN":  "0123456789abcdef0123456789abcdef",
		"KB_OLLAMA_URL": "ftp://nope",
	}))
	if err == nil || !strings.Contains(err.Error(), "KB_OLLAMA_URL") {
		t.Errorf("a non-http Ollama URL should fail naming KB_OLLAMA_URL, got %v", err)
	}
}

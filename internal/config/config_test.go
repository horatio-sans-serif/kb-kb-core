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
		"KB_API_TOKEN": "secret",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DBPath != "/tmp/kb.db" || cfg.APIToken != "secret" {
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

func TestConfig_MissingDBFails(t *testing.T) {
	_, err := Load(envFunc(map[string]string{"KB_API_TOKEN": "secret"}))
	if err == nil || !strings.Contains(err.Error(), "KB_DB") {
		t.Errorf("expected an error naming KB_DB, got %v", err)
	}
}

func TestConfig_InvalidPrivacyFails(t *testing.T) {
	_, err := Load(envFunc(map[string]string{
		"KB_DB":              "/tmp/kb.db",
		"KB_API_TOKEN":       "secret",
		"KB_DEFAULT_PRIVACY": "bogus",
	}))
	if err == nil || !strings.Contains(err.Error(), "KB_DEFAULT_PRIVACY") {
		t.Errorf("expected an error naming KB_DEFAULT_PRIVACY, got %v", err)
	}
}

func TestConfig_CloudLLMParsed(t *testing.T) {
	cfg, err := Load(envFunc(map[string]string{
		"KB_DB":        "/tmp/kb.db",
		"KB_API_TOKEN": "secret",
		"KB_CLOUD_LLM": "true",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.CloudLLMEnabled {
		t.Error("CloudLLMEnabled = false, want true when KB_CLOUD_LLM=true")
	}
}

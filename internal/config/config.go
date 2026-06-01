// Package config loads daemon configuration from the environment, failing fast
// with a clear message when a required value is missing or invalid.
package config

import (
	"fmt"
	"strings"
)

// Config is the daemon's runtime configuration.
type Config struct {
	DBPath          string // KB_DB (required)
	ListenAddr      string // KB_LISTEN_ADDR (default 127.0.0.1:8087; binding beyond loopback exposes the token-guarded API, e.g. on a Tailscale interface)
	APIToken        string // KB_API_TOKEN (required)
	DefaultPrivacy  string // KB_DEFAULT_PRIVACY (default personal)
	CloudLLMEnabled bool   // KB_CLOUD_LLM (default false)
	OllamaURL       string // KB_OLLAMA_URL (default http://127.0.0.1:11434)
	SummarizeModel  string // KB_SUMMARIZE_MODEL (default llama3.2)
}

// Load reads configuration via getenv (pass os.Getenv in production). It returns
// an error naming every missing required variable and rejects an invalid
// default-privacy tier.
func Load(getenv func(string) string) (*Config, error) {
	cfg := &Config{
		DBPath:          getenv("KB_DB"),
		ListenAddr:      orDefault(getenv("KB_LISTEN_ADDR"), "127.0.0.1:8087"),
		APIToken:        getenv("KB_API_TOKEN"),
		DefaultPrivacy:  orDefault(getenv("KB_DEFAULT_PRIVACY"), "personal"),
		CloudLLMEnabled: parseBool(getenv("KB_CLOUD_LLM")),
		OllamaURL:       orDefault(getenv("KB_OLLAMA_URL"), "http://127.0.0.1:11434"),
		SummarizeModel:  orDefault(getenv("KB_SUMMARIZE_MODEL"), "llama3.2"),
	}

	var missing []string
	if cfg.DBPath == "" {
		missing = append(missing, "KB_DB")
	}
	if cfg.APIToken == "" {
		missing = append(missing, "KB_API_TOKEN")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}

	const minTokenLen = 16
	if len(cfg.APIToken) < minTokenLen {
		return nil, fmt.Errorf("KB_API_TOKEN must be at least %d characters (use e.g. `openssl rand -hex 32`)", minTokenLen)
	}

	switch cfg.DefaultPrivacy {
	case "public", "personal", "sensitive", "restricted":
	default:
		return nil, fmt.Errorf("invalid KB_DEFAULT_PRIVACY %q (want public|personal|sensitive|restricted)", cfg.DefaultPrivacy)
	}

	return cfg, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

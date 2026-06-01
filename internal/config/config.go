// Package config loads daemon configuration from the environment, failing fast
// with a clear message when a required value is missing or invalid.
package config

import (
	"fmt"
	"net"
	"net/url"
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
	EmbedModel      string // KB_EMBED_MODEL (default nomic-embed-text)
	OllamaIsLocal   bool   // computed: true when OllamaURL is a loopback endpoint
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
		EmbedModel:      orDefault(getenv("KB_EMBED_MODEL"), "nomic-embed-text"),
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

	// The local-LLM privacy guarantee (Principle 7 / §8.17) rests on Ollama being
	// on-device. Verify that rather than assume it: a non-loopback endpoint would
	// send artifact bodies off-box, so it requires an explicit opt-in. When opted
	// in, OllamaIsLocal stays false and the summarize stage gates sensitive/
	// restricted artifacts off the remote model.
	local, err := ollamaIsLocal(cfg.OllamaURL)
	if err != nil {
		return nil, fmt.Errorf("invalid KB_OLLAMA_URL %q: %w", cfg.OllamaURL, err)
	}
	cfg.OllamaIsLocal = local
	if !local && !parseBool(getenv("KB_ALLOW_REMOTE_OLLAMA")) {
		return nil, fmt.Errorf("KB_OLLAMA_URL %q is not a loopback endpoint; sending artifacts there bypasses the on-device privacy guarantee. Set KB_ALLOW_REMOTE_OLLAMA=true to allow it (sensitive/restricted artifacts will then be skipped, not sent)", cfg.OllamaURL)
	}

	return cfg, nil
}

// ollamaIsLocal reports whether rawURL targets a loopback host (localhost or a
// loopback IP literal). It does not resolve DNS — a hostname other than
// "localhost" is treated as non-local, which is the safe default.
func ollamaIsLocal(rawURL string) (bool, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false, fmt.Errorf("scheme %q (want http/https)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return false, fmt.Errorf("missing host")
	}
	if host == "localhost" {
		return true, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback(), nil
	}
	return false, nil
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

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

func TestConfig_SyncOptions(t *testing.T) {
	cfg, err := Load(envFunc(map[string]string{
		"KB_DB":            "/tmp/kb.db",
		"KB_API_TOKEN":     "0123456789abcdef0123456789abcdef",
		"KB_CRSQLITE_PATH": "/opt/crsqlite.dylib",
		"KB_SYNC_PEERS":    "http://host-a:8087; http://host-b:8087",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CRSqlitePath != "/opt/crsqlite.dylib" {
		t.Errorf("CRSqlitePath = %q, want the configured path", cfg.CRSqlitePath)
	}
	if len(cfg.SyncPeers) != 2 || cfg.SyncPeers[0] != "http://host-a:8087" || cfg.SyncPeers[1] != "http://host-b:8087" {
		t.Errorf("SyncPeers = %v, want the two trimmed peer URLs", cfg.SyncPeers)
	}
}

func TestConfig_SyncDefaultsOff(t *testing.T) {
	cfg, err := Load(envFunc(map[string]string{
		"KB_DB":        "/tmp/kb.db",
		"KB_API_TOKEN": "0123456789abcdef0123456789abcdef",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CRSqlitePath != "" {
		t.Errorf("CRSqlitePath = %q, want empty (single-node) by default", cfg.CRSqlitePath)
	}
	if len(cfg.SyncPeers) != 0 {
		t.Errorf("SyncPeers = %v, want none by default", cfg.SyncPeers)
	}
	if cfg.PurgeRetention != 90 {
		t.Errorf("PurgeRetention = %d, want 90 days by default", cfg.PurgeRetention)
	}
	if cfg.AuditRetention != 365 {
		t.Errorf("AuditRetention = %d, want 365 days by default", cfg.AuditRetention)
	}
	if cfg.EmbedDim != 768 {
		t.Errorf("EmbedDim = %d, want 768 by default", cfg.EmbedDim)
	}
}

func TestConfig_ReadToken(t *testing.T) {
	const owner = "0123456789abcdef0123456789abcdef"
	base := map[string]string{"KB_DB": "/tmp/kb.db", "KB_API_TOKEN": owner}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	// No read token → single-owner, defaults.
	if cfg, err := Load(envFunc(base)); err != nil || cfg.ReadToken != "" {
		t.Errorf("no read token: cfg=%+v err=%v, want empty ReadToken", cfg, err)
	}
	// Configured read token + tier.
	if cfg, err := Load(envFunc(with("KB_READ_TOKEN", "readonly-token-1234567890", "KB_READ_TIER", "personal"))); err != nil || cfg.ReadToken == "" || cfg.ReadTier != "personal" {
		t.Errorf("read token: cfg=%+v err=%v, want token + personal tier", cfg, err)
	}
	// Default tier is public.
	if cfg, err := Load(envFunc(with("KB_READ_TOKEN", "readonly-token-1234567890"))); err != nil || cfg.ReadTier != "public" {
		t.Errorf("default read tier = %q (err %v), want public", cfg.ReadTier, err)
	}
	// Rejections: too short, equal to owner, invalid tier.
	if _, err := Load(envFunc(with("KB_READ_TOKEN", "short"))); err == nil {
		t.Error("a too-short read token should fail")
	}
	if _, err := Load(envFunc(with("KB_READ_TOKEN", owner))); err == nil {
		t.Error("a read token equal to the owner token should fail")
	}
	if _, err := Load(envFunc(with("KB_READ_TOKEN", "readonly-token-1234567890", "KB_READ_TIER", "bogus"))); err == nil {
		t.Error("an invalid read tier should fail")
	}
	// "restricted" is the owner's ceiling; accepting it for the read token would void
	// the read ceiling entirely, so it must be rejected.
	if _, err := Load(envFunc(with("KB_READ_TOKEN", "readonly-token-1234567890", "KB_READ_TIER", "restricted"))); err == nil {
		t.Error("KB_READ_TIER=restricted must be rejected (it grants the read token unfiltered access)")
	}
}

func TestConfig_EmbedDim(t *testing.T) {
	base := map[string]string{"KB_DB": "/tmp/kb.db", "KB_API_TOKEN": "0123456789abcdef0123456789abcdef"}
	with := func(v string) map[string]string {
		m := map[string]string{}
		for k, val := range base {
			m[k] = val
		}
		m["KB_EMBED_DIM"] = v
		return m
	}
	if cfg, err := Load(envFunc(with("1024"))); err != nil || cfg.EmbedDim != 1024 {
		t.Errorf("EmbedDim 1024 = %v (err %v), want 1024", cfg, err)
	}
	if _, err := Load(envFunc(with("0"))); err == nil {
		t.Error("zero embed dim should fail (must be positive)")
	}
	if _, err := Load(envFunc(with("nope"))); err == nil {
		t.Error("non-numeric embed dim should fail")
	}
}

func TestConfig_PurgeRetention(t *testing.T) {
	base := map[string]string{"KB_DB": "/tmp/kb.db", "KB_API_TOKEN": "0123456789abcdef0123456789abcdef"}
	with := func(v string) map[string]string {
		m := map[string]string{}
		for k, val := range base {
			m[k] = val
		}
		m["KB_PURGE_RETENTION_DAYS"] = v
		return m
	}
	if cfg, err := Load(envFunc(with("0"))); err != nil || cfg.PurgeRetention != 0 {
		t.Errorf("retention 0 = %v (err %v), want 0 (compaction disabled)", cfg, err)
	}
	if cfg, err := Load(envFunc(with("30"))); err != nil || cfg.PurgeRetention != 30 {
		t.Errorf("retention 30 = %v (err %v), want 30", cfg, err)
	}
	if _, err := Load(envFunc(with("-5"))); err == nil {
		t.Error("negative retention should fail")
	}
	if _, err := Load(envFunc(with("notanumber"))); err == nil {
		t.Error("non-numeric retention should fail")
	}
}

func TestConfig_AuditRetention(t *testing.T) {
	base := map[string]string{"KB_DB": "/tmp/kb.db", "KB_API_TOKEN": "0123456789abcdef0123456789abcdef"}
	with := func(v string) map[string]string {
		m := map[string]string{}
		for k, val := range base {
			m[k] = val
		}
		m["KB_AUDIT_RETENTION_DAYS"] = v
		return m
	}
	if cfg, err := Load(envFunc(with("0"))); err != nil || cfg.AuditRetention != 0 {
		t.Errorf("retention 0 = %v (err %v), want 0 (audit pruning disabled)", cfg, err)
	}
	if cfg, err := Load(envFunc(with("30"))); err != nil || cfg.AuditRetention != 30 {
		t.Errorf("retention 30 = %v (err %v), want 30", cfg, err)
	}
	if _, err := Load(envFunc(with("-5"))); err == nil {
		t.Error("negative audit retention should fail")
	}
	if _, err := Load(envFunc(with("notanumber"))); err == nil {
		t.Error("non-numeric audit retention should fail")
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

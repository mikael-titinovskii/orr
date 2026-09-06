package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var runtimeEnvNames = []string{
	"OPENROUTER_API_KEY", "ORR_LISTEN", "ORR_UPSTREAM", "ORR_LOG_REQUESTS",
	"ORR_TUI", "ORR_PIN_TTL", "ORR_PROVIDERS_FILE",
	"ORR_UPDATE_MAX_PROVIDERS", "ORR_UPDATE_CACHE_ONLY",
	"ORR_429_FAILOVER_THRESHOLD",
}

func clearRuntimeEnv(t *testing.T) {
	t.Helper()
	for _, name := range runtimeEnvNames {
		value, existed := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		name, value, existed := name, value, existed
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(name, value)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
}

func writeProvidersFile(t *testing.T, path string) {
	t.Helper()
	content := `version: 1
models:
  moonshotai/kimi-k3:
    order: [fireworks, deepinfra]
    allow_fallbacks: false
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDotEnvConfig(t *testing.T) {
	clearRuntimeEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := `ORR_LISTEN=127.0.0.1:9999
ORR_UPSTREAM=https://example.test/v1
ORR_LOG_REQUESTS=false
ORR_TUI=false
ORR_PIN_TTL=2h
ORR_PROVIDERS_FILE=routes.yaml
ORR_429_FAILOVER_THRESHOLD=4
OPENROUTER_API_KEY=test-key
`
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	writeProvidersFile(t, filepath.Join(dir, "routes.yaml"))

	cfg, err := loadConfig(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9999" || cfg.Upstream != "https://example.test/v1" {
		t.Fatalf("environment settings not loaded: %#v", cfg)
	}
	if cfg.LogRequests || cfg.TUI == nil || *cfg.TUI || cfg.PinTTL != 2*time.Hour {
		t.Fatalf("typed settings not loaded: %#v", cfg)
	}
	if cfg.OpenRouterAPIKey != "test-key" {
		t.Fatal("OpenRouter key not loaded from dotenv")
	}
	if cfg.RateLimitFailoverThreshold != 4 {
		t.Fatalf("429 failover threshold = %d, want 4", cfg.RateLimitFailoverThreshold)
	}
	if got := cfg.Models["moonshotai/kimi-k3"].Order; len(got) != 2 || got[0] != "fireworks" {
		t.Fatalf("providers file not loaded: %#v", cfg.Models)
	}
}

func TestUpdateSettingsFromDotEnv(t *testing.T) {
	clearRuntimeEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := `ORR_UPDATE_MAX_PROVIDERS=5
ORR_UPDATE_CACHE_ONLY=true
`
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	writeProvidersFile(t, filepath.Join(dir, "providers.yaml"))

	cfg, err := loadConfig(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpdateMaxProviders != 5 || !cfg.UpdateCacheOnly {
		t.Fatalf("update settings not loaded: %#v", cfg)
	}
}

func TestUpdateSettingsDefaultsAndValidation(t *testing.T) {
	clearRuntimeEnv(t)
	dir := t.TempDir()
	writeProvidersFile(t, filepath.Join(dir, "providers.yaml"))
	cfg, err := loadConfig(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpdateMaxProviders != 20 || cfg.UpdateCacheOnly || cfg.RateLimitFailoverThreshold != defaultRateLimitFailoverThreshold {
		t.Fatalf("unexpected update defaults: %#v", cfg)
	}

	invalid := []string{
		"ORR_UPDATE_MAX_PROVIDERS=0\n",
		"ORR_UPDATE_MAX_PROVIDERS=-1\n",
		"ORR_UPDATE_MAX_PROVIDERS=abc\n",
		"ORR_UPDATE_CACHE_ONLY=maybe\n",
		"ORR_429_FAILOVER_THRESHOLD=0\n",
		"ORR_429_FAILOVER_THRESHOLD=-1\n",
		"ORR_429_FAILOVER_THRESHOLD=abc\n",
	}
	for _, content := range invalid {
		t.Run(content, func(t *testing.T) {
			clearRuntimeEnv(t)
			dir := t.TempDir()
			envPath := filepath.Join(dir, ".env")
			if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			writeProvidersFile(t, filepath.Join(dir, "providers.yaml"))
			if _, err := loadConfig(envPath); err == nil {
				t.Fatalf("invalid update setting accepted: %q", content)
			}
		})
	}
}

func TestDotEnvDefaultsAndOptionalFile(t *testing.T) {
	clearRuntimeEnv(t)
	dir := t.TempDir()
	writeProvidersFile(t, filepath.Join(dir, "providers.yaml"))
	cfg, err := loadConfig(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8787" || cfg.Upstream != openRouterAPI || !cfg.LogRequests || cfg.PinTTL != time.Hour {
		t.Fatalf("defaults not applied: %#v", cfg)
	}
}

func TestLoadConfigCreatesMissingProvidersFile(t *testing.T) {
	clearRuntimeEnv(t)
	dir := t.TempDir()
	providersPath := filepath.Join(dir, "providers.yaml")
	cfg, err := loadConfig(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.providersPath != providersPath || len(cfg.Models) != 0 {
		t.Fatalf("unexpected empty providers config: %#v", cfg)
	}
	if _, err := os.Stat(providersPath); err != nil {
		t.Fatalf("providers file was not created: %v", err)
	}
}

func TestLoadConfigRejectsUnversionedProvidersFile(t *testing.T) {
	clearRuntimeEnv(t)
	dir := t.TempDir()
	providersPath := filepath.Join(dir, "providers.yaml")
	if err := os.WriteFile(providersPath, []byte("models:\n  a/b: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(filepath.Join(dir, ".env")); err == nil {
		t.Fatal("unversioned providers file accepted")
	}
}

func TestLoadConfigRejectsUnsupportedProvidersVersion(t *testing.T) {
	clearRuntimeEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "providers.yaml"), []byte("version: 2\nmodels: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(filepath.Join(dir, ".env")); err == nil {
		t.Fatal("unsupported providers version accepted")
	}
}

func TestLoadConfigRejectsUnknownProviderField(t *testing.T) {
	clearRuntimeEnv(t)
	dir := t.TempDir()
	content := "version: 1\nmodels:\n  a/b:\n    allow_fallback: false\n"
	if err := os.WriteFile(filepath.Join(dir, "providers.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(filepath.Join(dir, ".env")); err == nil {
		t.Fatal("unknown provider field accepted")
	}
}

func TestProcessEnvironmentOverridesDotEnv(t *testing.T) {
	clearRuntimeEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("ORR_LISTEN=127.0.0.1:1111\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeProvidersFile(t, filepath.Join(dir, "providers.yaml"))
	t.Setenv("ORR_LISTEN", "127.0.0.1:2222")

	cfg, err := loadConfig(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:2222" {
		t.Fatalf("process environment did not win: %q", cfg.Listen)
	}
}

func TestReadDotEnvQuotesCommentsAndExport(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "export SIMPLE=value # comment\nSINGLE='a # b'\nDOUBLE=\"line\\nvalue\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := readDotEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["SIMPLE"] != "value" || values["SINGLE"] != "a # b" || values["DOUBLE"] != "line\nvalue" {
		t.Fatalf("unexpected dotenv values: %#v", values)
	}
}

func TestRejectsInvalidDotEnvSettings(t *testing.T) {
	for _, content := range []string{
		"ORR_TUI=maybe\n",
		"ORR_PIN_TTL=tomorrow\n",
	} {
		t.Run(content, func(t *testing.T) {
			clearRuntimeEnv(t)
			dir := t.TempDir()
			envPath := filepath.Join(dir, ".env")
			if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			writeProvidersFile(t, filepath.Join(dir, "providers.yaml"))
			if _, err := loadConfig(envPath); err == nil {
				t.Fatalf("invalid setting accepted: %q", content)
			}
		})
	}
}

func TestPinTTLDefaultAndValidation(t *testing.T) {
	cfg := config{Listen: "127.0.0.1:1", Upstream: "https://example.test", Models: map[string]providerConfig{"a/b": {}}, UpdateMaxProviders: 20, RateLimitFailoverThreshold: defaultRateLimitFailoverThreshold}
	if err := cfg.validate(); err != nil {
		t.Fatalf("zero pin TTL should be valid: %v", err)
	}
	cfg.PinTTL = -1
	if err := cfg.validate(); err == nil {
		t.Fatal("negative pin TTL accepted")
	}
}

func TestOpenRouterKeyFallback(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "from-environment")
	cfg := config{}
	if got := cfg.openRouterKey(); got != "from-environment" {
		t.Fatalf("fallback key = %q", got)
	}
	cfg.OpenRouterAPIKey = "from-dotenv"
	if got := cfg.openRouterKey(); got != "from-dotenv" {
		t.Fatalf("dotenv key did not take precedence: %q", got)
	}
}

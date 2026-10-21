package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var runtimeEnvNames = []string{
	"OPENROUTER_API_KEY", "ORR_LISTEN", "ORR_UPSTREAM", "ORR_LOG_REQUESTS",
	"ORR_TUI", "ORR_PIN_TTL", "ORR_DB_FILE",
	"ORR_UPDATE_MAX_PROVIDERS", "ORR_UPDATE_CACHE_ONLY",
	"ORR_429_FAILOVER_THRESHOLD",
	"ORR_DEGRADATION", "ORR_DEGRADATION_THRESHOLD", "ORR_DEGRADATION_WINDOW",
	"ORR_DEGRADATION_MIN_SAMPLES", "ORR_DEGRADATION_COOLDOWN", "ORR_DEGRADATION_TIMEOUT",
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

func TestLoadDotEnvConfig(t *testing.T) {
	clearRuntimeEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := `ORR_LISTEN=127.0.0.1:9999
ORR_UPSTREAM=https://example.test/v1
ORR_LOG_REQUESTS=false
ORR_TUI=false
ORR_PIN_TTL=2h
ORR_DB_FILE=routes.db
ORR_429_FAILOVER_THRESHOLD=4
OPENROUTER_API_KEY=test-key
`
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

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
	if want := filepath.Join(dir, "routes.db"); cfg.dbPath != want {
		t.Fatalf("database path = %q, want %q", cfg.dbPath, want)
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
			if _, err := loadConfig(envPath); err == nil {
				t.Fatalf("invalid update setting accepted: %q", content)
			}
		})
	}
}

func TestDotEnvDefaultsAndOptionalFile(t *testing.T) {
	clearRuntimeEnv(t)
	dir := t.TempDir()
	cfg, err := loadConfig(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8787" || cfg.Upstream != openRouterAPI || !cfg.LogRequests || cfg.PinTTL != time.Hour {
		t.Fatalf("defaults not applied: %#v", cfg)
	}
}

func TestProcessEnvironmentOverridesDotEnv(t *testing.T) {
	clearRuntimeEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("ORR_LISTEN=127.0.0.1:1111\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORR_LISTEN", "127.0.0.1:2222")

	cfg, err := loadConfig(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:2222" {
		t.Fatalf("process environment did not win: %q", cfg.Listen)
	}
}

func TestDotEnvCanOverrideProcessEnvironment(t *testing.T) {
	clearRuntimeEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := "ORR_LISTEN=127.0.0.1:1111\nORR_DB_FILE=file-orr.db\n"
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORR_LISTEN", "127.0.0.1:2222")
	t.Setenv("ORR_DB_FILE", "environment-orr.db")
	t.Setenv("ORR_UPSTREAM", "https://environment.example/v1")

	cfg, err := loadConfigWithOptions(envPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:1111" {
		t.Fatalf("dotenv listen did not win: %q", cfg.Listen)
	}
	if cfg.dbPath != filepath.Join(dir, "file-orr.db") {
		t.Fatalf("dotenv database path did not win: %q", cfg.dbPath)
	}
	if cfg.Upstream != "https://environment.example/v1" {
		t.Fatalf("missing dotenv value did not fall back to process environment: %q", cfg.Upstream)
	}
}

func TestPreferredEmptyDotEnvValueOverridesProcessEnvironment(t *testing.T) {
	clearRuntimeEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("OPENROUTER_API_KEY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTER_API_KEY", "from-environment")

	cfg, err := loadConfigWithOptions(envPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.openRouterKey(); got != "" {
		t.Fatalf("explicit empty dotenv key did not win: %q", got)
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
			if _, err := loadConfig(envPath); err == nil {
				t.Fatalf("invalid setting accepted: %q", content)
			}
		})
	}
}

func TestDegradationSettings(t *testing.T) {
	clearRuntimeEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := `ORR_DEGRADATION=false
ORR_DEGRADATION_THRESHOLD=0.75
ORR_DEGRADATION_WINDOW=90s
ORR_DEGRADATION_MIN_SAMPLES=5
ORR_DEGRADATION_COOLDOWN=3m
ORR_DEGRADATION_TIMEOUT=4s
`
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.degradationDefault() {
		t.Fatal("ORR_DEGRADATION=false was ignored")
	}
	if cfg.DegradationThreshold != 0.75 || cfg.DegradationWindow != 90*time.Second ||
		cfg.DegradationMinSamples != 5 || cfg.DegradationCooldown != 3*time.Minute ||
		cfg.DegradationTimeout != 4*time.Second {
		t.Fatalf("degradation settings = %#v", cfg)
	}

	// Unset, the watch starts on and its tuning matches the documented defaults.
	defaultsPath := filepath.Join(dir, "defaults.env")
	if err := os.WriteFile(defaultsPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defaults, err := loadConfig(defaultsPath)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Degradation != nil || !defaults.degradationDefault() {
		t.Fatal("unset ORR_DEGRADATION does not start the watch on")
	}
	if defaults.DegradationThreshold != defaultDegradationThreshold ||
		defaults.DegradationWindow != defaultDegradationWindow ||
		defaults.DegradationMinSamples != defaultDegradationMinSamples ||
		defaults.DegradationCooldown != defaultDegradationCooldown ||
		defaults.DegradationTimeout != defaultDegradationTimeout {
		t.Fatalf("degradation defaults = %#v", defaults)
	}
}

func TestDegradationRejectsInvalidSettings(t *testing.T) {
	for _, content := range []string{
		"ORR_DEGRADATION_THRESHOLD=0\n",
		"ORR_DEGRADATION_THRESHOLD=1.5\n",
		"ORR_DEGRADATION_THRESHOLD=high\n",
		"ORR_DEGRADATION_WINDOW=soon\n",
		"ORR_DEGRADATION_WINDOW=0s\n",
		"ORR_DEGRADATION_WINDOW=30m\n",
		"ORR_DEGRADATION_MIN_SAMPLES=0\n",
		"ORR_DEGRADATION_MIN_SAMPLES=two\n",
		"ORR_DEGRADATION_COOLDOWN=-1m\n",
		"ORR_DEGRADATION_COOLDOWN=0s\n",
		"ORR_DEGRADATION_TIMEOUT=0s\n",
	} {
		t.Run(content, func(t *testing.T) {
			clearRuntimeEnv(t)
			dir := t.TempDir()
			envPath := filepath.Join(dir, ".env")
			if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadConfig(envPath); err == nil {
				t.Fatalf("invalid setting accepted: %q", content)
			}
		})
	}
}

func TestPinTTLDefaultAndValidation(t *testing.T) {
	cfg := config{
		Listen: "127.0.0.1:1", Upstream: "https://example.test", Models: map[string]providerConfig{"a/b": {}},
		UpdateMaxProviders: 20, RateLimitFailoverThreshold: defaultRateLimitFailoverThreshold,
		DegradationThreshold: defaultDegradationThreshold, DegradationWindow: defaultDegradationWindow,
		DegradationMinSamples: defaultDegradationMinSamples, DegradationCooldown: defaultDegradationCooldown,
		DegradationTimeout: defaultDegradationTimeout,
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("zero pin TTL should be valid: %v", err)
	}
	cfg.PinTTL = -1
	if err := cfg.validate(); err == nil {
		t.Fatal("negative pin TTL accepted")
	}
}

func TestValidateModelsRejectsUnsupportedValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model providerConfig
	}{
		{"sort", providerConfig{Sort: "pricey"}},
		{"data_collection", providerConfig{DataCollection: "maybe"}},
	} {
		if err := validateModels(map[string]providerConfig{"author/model": tc.model}); err == nil {
			t.Fatalf("%s accepted", tc.name)
		}
	}
	if err := validateModels(map[string]providerConfig{"author/model": {Sort: "price", DataCollection: "deny"}}); err != nil {
		t.Fatalf("valid routing rejected: %v", err)
	}
	if err := validateModels(map[string]providerConfig{"bad-id": {}}); err == nil {
		t.Fatal("model id without author/model form accepted")
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

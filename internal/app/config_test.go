package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	"ORR_LOOP_DETECTION", "ORR_LOOP_MIN_PERIOD", "ORR_LOOP_MIN_REPEAT",
	"ORR_LOOP_MAX_CONTINUATIONS", "ORR_LOOP_PENALTY",
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

// loadLoopConfig loads a config from one dotenv file and hands back only the
// loop settings, so each test can state one variable at a time.
func loadLoopConfig(t *testing.T, content string) loopConfig {
	t.Helper()
	clearRuntimeEnv(t)
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(envPath)
	if err != nil {
		t.Fatalf("loadConfig(%q): %v", content, err)
	}
	return cfg.Loop
}

func TestLoopSettingsDefaults(t *testing.T) {
	// Unset, loop detection runs in recover mode with the documented tuning.
	got := loadLoopConfig(t, "")
	want := loopConfig{
		Mode:             loopModeRecover,
		MinPeriod:        32,
		MinRepeat:        512,
		MaxContinuations: 1,
		Penalty:          0.4,
	}
	if got != want {
		t.Fatalf("loop defaults = %#v, want %#v", got, want)
	}
	if got.Mode != defaultLoopMode || got.MinPeriod != defaultLoopMinPeriod ||
		got.MinRepeat != defaultLoopMinRepeat || got.MaxContinuations != defaultLoopMaxContinuations ||
		got.Penalty != defaultLoopPenalty {
		t.Fatalf("loop defaults drift from their constants: %#v", got)
	}
}

func TestLoopSettingsOverrides(t *testing.T) {
	for _, mode := range []loopMode{loopModeOff, loopModeObserve, loopModeAbort, loopModeRecover} {
		t.Run("mode "+string(mode), func(t *testing.T) {
			got := loadLoopConfig(t, "ORR_LOOP_DETECTION="+string(mode)+"\n")
			if got.Mode != mode {
				t.Fatalf("ORR_LOOP_DETECTION=%s read back as %q", mode, got.Mode)
			}
		})
	}

	// Each numeric variable on its own, so a swapped name cannot hide behind
	// the others.
	for _, tc := range []struct {
		name    string
		content string
		want    loopConfig
	}{
		{
			name:    "min period",
			content: "ORR_LOOP_MIN_PERIOD=16\n",
			want:    loopConfig{Mode: loopModeRecover, MinPeriod: 16, MinRepeat: 512, MaxContinuations: 1, Penalty: 0.4},
		},
		{
			name:    "min repeat",
			content: "ORR_LOOP_MIN_REPEAT=1024\n",
			want:    loopConfig{Mode: loopModeRecover, MinPeriod: 32, MinRepeat: 1024, MaxContinuations: 1, Penalty: 0.4},
		},
		{
			name:    "max continuations",
			content: "ORR_LOOP_MAX_CONTINUATIONS=3\n",
			want:    loopConfig{Mode: loopModeRecover, MinPeriod: 32, MinRepeat: 512, MaxContinuations: 3, Penalty: 0.4},
		},
		{
			name:    "penalty",
			content: "ORR_LOOP_PENALTY=1.5\n",
			want:    loopConfig{Mode: loopModeRecover, MinPeriod: 32, MinRepeat: 512, MaxContinuations: 1, Penalty: 1.5},
		},
		{
			name: "all five",
			content: `ORR_LOOP_DETECTION=abort
ORR_LOOP_MIN_PERIOD=24
ORR_LOOP_MIN_REPEAT=768
ORR_LOOP_MAX_CONTINUATIONS=0
ORR_LOOP_PENALTY=0
`,
			want: loopConfig{Mode: loopModeAbort, MinPeriod: 24, MinRepeat: 768, MaxContinuations: 0, Penalty: 0},
		},
		{
			// Every bound accepted at its own edge: the shortest period tier 1
			// considers, the detector's shortest repeat, 0, and 2.
			name: "bounds",
			content: `ORR_LOOP_MIN_PERIOD=8
ORR_LOOP_MIN_REPEAT=512
ORR_LOOP_MAX_CONTINUATIONS=0
ORR_LOOP_PENALTY=2
`,
			want: loopConfig{Mode: loopModeRecover, MinPeriod: 8, MinRepeat: 512, MaxContinuations: 0, Penalty: 2},
		},
		{
			// The detector's ring is the top of the range: a longer minimum could
			// never be confirmed, so this is the largest value accepted.
			name:    "min repeat ceiling",
			content: fmt.Sprintf("ORR_LOOP_MIN_REPEAT=%d\n", maxLoopMinRepeat),
			want:    loopConfig{Mode: loopModeRecover, MinPeriod: 32, MinRepeat: maxLoopMinRepeat, MaxContinuations: 1, Penalty: 0.4},
		},
		{
			// The longest period tier 1 considers, so this is the largest
			// ORR_LOOP_MIN_PERIOD accepted; its repeat has to clear twice that.
			name:    "min period ceiling",
			content: fmt.Sprintf("ORR_LOOP_MIN_PERIOD=%d\nORR_LOOP_MIN_REPEAT=%d\n", maxLoopMinPeriod, 2*maxLoopMinPeriod),
			want:    loopConfig{Mode: loopModeRecover, MinPeriod: maxLoopMinPeriod, MinRepeat: 2 * maxLoopMinPeriod, MaxContinuations: 1, Penalty: 0.4},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := loadLoopConfig(t, tc.content); got != tc.want {
				t.Fatalf("loop settings = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestLoopRejectsInvalidSettings(t *testing.T) {
	for _, content := range []string{
		"ORR_LOOP_DETECTION=stop\n",
		"ORR_LOOP_MIN_PERIOD=7\n",
		"ORR_LOOP_MIN_PERIOD=0\n",
		"ORR_LOOP_MIN_PERIOD=-32\n",
		"ORR_LOOP_MIN_PERIOD=period\n",
		fmt.Sprintf("ORR_LOOP_MIN_PERIOD=%d\nORR_LOOP_MIN_REPEAT=%d\n", maxLoopMinPeriod+1, 2*(maxLoopMinPeriod+1)),
		"ORR_LOOP_MIN_REPEAT=40\n",
		"ORR_LOOP_MIN_REPEAT=0\n",
		"ORR_LOOP_MIN_REPEAT=repeat\n",
		fmt.Sprintf("ORR_LOOP_MIN_REPEAT=%d\n", minLoopMinRepeat-1),
		"ORR_LOOP_MIN_PERIOD=64\nORR_LOOP_MIN_REPEAT=120\n",
		// The relative rule still binds once both absolute bounds are cleared.
		"ORR_LOOP_MIN_PERIOD=1024\nORR_LOOP_MIN_REPEAT=1024\n",
		fmt.Sprintf("ORR_LOOP_MIN_REPEAT=%d\n", maxLoopMinRepeat+1),
		"ORR_LOOP_MAX_CONTINUATIONS=-1\n",
		"ORR_LOOP_MAX_CONTINUATIONS=many\n",
		"ORR_LOOP_PENALTY=-0.1\n",
		"ORR_LOOP_PENALTY=2.5\n",
		"ORR_LOOP_PENALTY=NaN\n",
		"ORR_LOOP_PENALTY=high\n",
	} {
		t.Run(content, func(t *testing.T) {
			clearRuntimeEnv(t)
			envPath := filepath.Join(t.TempDir(), ".env")
			if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadConfig(envPath); err == nil {
				t.Fatalf("invalid setting accepted: %q", content)
			}
		})
	}
}

func TestLoopMinRepeatCeilingError(t *testing.T) {
	// The error has to tell the user why the value is refused: above the ring a
	// minimum repetition is not stricter, it disables confirmation outright.
	clearRuntimeEnv(t)
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte(fmt.Sprintf("ORR_LOOP_MIN_REPEAT=%d\n", maxLoopMinRepeat+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfig(envPath)
	if err == nil {
		t.Fatalf("min repeat of %d accepted above the %d-byte ring", maxLoopMinRepeat+1, maxLoopMinRepeat)
	}
	for _, want := range []string{"ORR_LOOP_MIN_REPEAT", strconv.Itoa(maxLoopMinRepeat), "tier 2", "silently never fires"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestLoopMinPeriodCeilingError(t *testing.T) {
	// The error has to tell the user why the value is refused: above tier 1's
	// ceiling a longer period is discarded, so nothing is ever detected.
	clearRuntimeEnv(t)
	envPath := filepath.Join(t.TempDir(), ".env")
	content := fmt.Sprintf("ORR_LOOP_MIN_PERIOD=%d\nORR_LOOP_MIN_REPEAT=%d\n", maxLoopMinPeriod+1, 2*(maxLoopMinPeriod+1))
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfig(envPath)
	if err == nil {
		t.Fatalf("min period of %d accepted above the %d-byte ceiling", maxLoopMinPeriod+1, maxLoopMinPeriod)
	}
	for _, want := range []string{"ORR_LOOP_MIN_PERIOD", strconv.Itoa(maxLoopMinPeriod), "tier 1", "silently never fires"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestLoopMinRepeatFloorError(t *testing.T) {
	// A minimum repetition below the detector's suffix floor is not a tighter
	// setting: tier 2 raises it, so the error has to say the value would not be
	// the one in force.
	clearRuntimeEnv(t)
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte(fmt.Sprintf("ORR_LOOP_MIN_REPEAT=%d\n", minLoopMinRepeat-1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfig(envPath)
	if err == nil {
		t.Fatalf("min repeat of %d accepted below the %d-byte floor", minLoopMinRepeat-1, minLoopMinRepeat)
	}
	for _, want := range []string{"ORR_LOOP_MIN_REPEAT", strconv.Itoa(minLoopMinRepeat), "never looks back"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestLoopModeErrorNamesAcceptedValues(t *testing.T) {
	clearRuntimeEnv(t)
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("ORR_LOOP_DETECTION=stop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfig(envPath)
	if err == nil {
		t.Fatal("unknown loop mode accepted")
	}
	for _, mode := range []loopMode{loopModeOff, loopModeObserve, loopModeAbort, loopModeRecover} {
		if !strings.Contains(err.Error(), string(mode)) {
			t.Fatalf("error %q does not name the accepted mode %q", err, mode)
		}
	}
}

func TestPinTTLDefaultAndValidation(t *testing.T) {
	cfg := config{
		Listen: "127.0.0.1:1", Upstream: "https://example.test", Models: map[string]providerConfig{"a/b": {}},
		UpdateMaxProviders: 20, RateLimitFailoverThreshold: defaultRateLimitFailoverThreshold,
		DegradationThreshold: defaultDegradationThreshold, DegradationWindow: defaultDegradationWindow,
		DegradationMinSamples: defaultDegradationMinSamples, DegradationCooldown: defaultDegradationCooldown,
		DegradationTimeout: defaultDegradationTimeout,
		Loop: loopConfig{
			Mode:             defaultLoopMode,
			MinPeriod:        defaultLoopMinPeriod,
			MinRepeat:        defaultLoopMinRepeat,
			MaxContinuations: defaultLoopMaxContinuations,
			Penalty:          defaultLoopPenalty,
		},
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

// The default 429 threshold is one: the first provider-attributed rate limit
// fails over, because the client — an agent subagent, for instance — may never
// retry on its own. The environment override still wins, and still rejects
// anything below one.
func TestRateLimitFailoverThresholdDefaultsToImmediateFailover(t *testing.T) {
	clearRuntimeEnv(t)
	if defaultRateLimitFailoverThreshold != 1 {
		t.Fatalf("default 429 failover threshold = %d, want 1", defaultRateLimitFailoverThreshold)
	}
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	cfg, err := loadConfig(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RateLimitFailoverThreshold != 1 {
		t.Fatalf("429 failover threshold = %d, want the immediate default", cfg.RateLimitFailoverThreshold)
	}

	if err := os.WriteFile(envPath, []byte("ORR_429_FAILOVER_THRESHOLD=3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RateLimitFailoverThreshold != 3 {
		t.Fatalf("overridden 429 failover threshold = %d, want 3", cfg.RateLimitFailoverThreshold)
	}
}

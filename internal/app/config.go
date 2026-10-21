package app

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mikael-titinovskii/orr/internal/store"
)

type config struct {
	Listen                     string
	Upstream                   string
	Models                     map[string]providerConfig
	LogRequests                bool
	OpenRouterAPIKey           string
	TUI                        *bool
	PinTTL                     time.Duration
	UpdateMaxProviders         int
	UpdateCacheOnly            bool
	RateLimitFailoverThreshold int
	Degradation                *bool
	DegradationThreshold       float64
	DegradationWindow          time.Duration
	DegradationMinSamples      int
	DegradationCooldown        time.Duration
	DegradationTimeout         time.Duration
	db                         *store.Store
	dbPath                     string
	preferEnvFile              bool
}

const defaultRateLimitFailoverThreshold = 2

// Degradation recovery ships on: it only ever acts on an automatic pin, and
// its cooldown bounds how often it may spend a benchmark. The threshold is the
// share of a provider's own baseline vitals that still counts as healthy.
const (
	defaultDegradationThreshold  = 0.6
	defaultDegradationWindow     = 2 * time.Minute
	defaultDegradationMinSamples = 3
	defaultDegradationCooldown   = 10 * time.Minute
	defaultDegradationTimeout    = 10 * time.Second
)

// providerConfig is the routing configuration the application works with; the
// store owns its definition and its persistence.
type providerConfig = store.Model

func hasRouting(p providerConfig) bool {
	return len(p.Order) > 0 || len(p.Only) > 0 || len(p.Ignore) > 0 || p.AllowFallbacks != nil ||
		p.RequireParameters != nil || p.DataCollection != "" || p.ZDR != nil || p.Sort != ""
}

func loadConfig(envPath string) (config, error) {
	return loadConfigWithOptions(envPath, false)
}

func loadConfigWithOptions(envPath string, preferEnvFile bool) (config, error) {
	cfg := config{
		Listen:                     "127.0.0.1:8787",
		Upstream:                   openRouterAPI,
		LogRequests:                true,
		PinTTL:                     time.Hour,
		UpdateMaxProviders:         20,
		UpdateCacheOnly:            false,
		RateLimitFailoverThreshold: defaultRateLimitFailoverThreshold,
		DegradationThreshold:       defaultDegradationThreshold,
		DegradationWindow:          defaultDegradationWindow,
		DegradationMinSamples:      defaultDegradationMinSamples,
		DegradationCooldown:        defaultDegradationCooldown,
		DegradationTimeout:         defaultDegradationTimeout,
		preferEnvFile:              preferEnvFile,
	}

	fileEnv, err := readDotEnv(envPath)
	if err != nil {
		return config{}, err
	}
	value := func(name string) string { return envValue(fileEnv, name, preferEnvFile) }
	if listen := value("ORR_LISTEN"); listen != "" {
		cfg.Listen = listen
	}
	if upstream := value("ORR_UPSTREAM"); upstream != "" {
		cfg.Upstream = upstream
	}
	if raw := value("ORR_LOG_REQUESTS"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return config{}, fmt.Errorf("parse ORR_LOG_REQUESTS: %w", err)
		}
		cfg.LogRequests = parsed
	}
	if raw := value("ORR_TUI"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return config{}, fmt.Errorf("parse ORR_TUI: %w", err)
		}
		cfg.TUI = &parsed
	}
	if raw := value("ORR_PIN_TTL"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return config{}, fmt.Errorf("parse ORR_PIN_TTL: %w", err)
		}
		cfg.PinTTL = parsed
	}
	if raw := value("ORR_UPDATE_MAX_PROVIDERS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return config{}, fmt.Errorf("parse ORR_UPDATE_MAX_PROVIDERS: must be a positive integer")
		}
		cfg.UpdateMaxProviders = parsed
	}
	if raw := value("ORR_UPDATE_CACHE_ONLY"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return config{}, fmt.Errorf("parse ORR_UPDATE_CACHE_ONLY: %w", err)
		}
		cfg.UpdateCacheOnly = parsed
	}
	if raw := value("ORR_429_FAILOVER_THRESHOLD"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return config{}, fmt.Errorf("parse ORR_429_FAILOVER_THRESHOLD: must be a positive integer")
		}
		cfg.RateLimitFailoverThreshold = parsed
	}
	if raw := value("ORR_DEGRADATION"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return config{}, fmt.Errorf("parse ORR_DEGRADATION: %w", err)
		}
		cfg.Degradation = &parsed
	}
	if raw := value("ORR_DEGRADATION_THRESHOLD"); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 || parsed > 1 {
			return config{}, fmt.Errorf("parse ORR_DEGRADATION_THRESHOLD: must be a fraction above 0 and at most 1")
		}
		cfg.DegradationThreshold = parsed
	}
	if raw := value("ORR_DEGRADATION_WINDOW"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return config{}, fmt.Errorf("parse ORR_DEGRADATION_WINDOW: %w", err)
		}
		cfg.DegradationWindow = parsed
	}
	if raw := value("ORR_DEGRADATION_MIN_SAMPLES"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return config{}, fmt.Errorf("parse ORR_DEGRADATION_MIN_SAMPLES: must be a positive integer")
		}
		cfg.DegradationMinSamples = parsed
	}
	if raw := value("ORR_DEGRADATION_COOLDOWN"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return config{}, fmt.Errorf("parse ORR_DEGRADATION_COOLDOWN: %w", err)
		}
		cfg.DegradationCooldown = parsed
	}
	if raw := value("ORR_DEGRADATION_TIMEOUT"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return config{}, fmt.Errorf("parse ORR_DEGRADATION_TIMEOUT: %w", err)
		}
		cfg.DegradationTimeout = parsed
	}
	cfg.OpenRouterAPIKey = value("OPENROUTER_API_KEY")

	cfg.Models = make(map[string]providerConfig)
	dbPath, err := configuredDBPathWithOptions(envPath, fileEnv, preferEnvFile)
	if err != nil {
		return config{}, fmt.Errorf("locate database: %w", err)
	}
	cfg.dbPath = dbPath

	if err := cfg.validate(); err != nil {
		return config{}, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

func readDotEnv(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read environment file %s: %w", path, err)
	}
	defer file.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		name, raw, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || !validEnvName(name) {
			return nil, fmt.Errorf("parse environment file %s:%d: expected NAME=VALUE", path, lineNumber)
		}
		parsed, err := parseDotEnvValue(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("parse environment file %s:%d: %w", path, lineNumber, err)
		}
		values[name] = parsed
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read environment file %s: %w", path, err)
	}
	return values, nil
}

func validEnvName(name string) bool {
	for i, char := range name {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || char == '_' || (i > 0 && char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return name != ""
}

func parseDotEnvValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if raw[0] == '\'' {
		if len(raw) < 2 || raw[len(raw)-1] != '\'' {
			return "", fmt.Errorf("unterminated single-quoted value")
		}
		return raw[1 : len(raw)-1], nil
	}
	if raw[0] == '"' {
		if len(raw) < 2 || raw[len(raw)-1] != '"' {
			return "", fmt.Errorf("unterminated double-quoted value")
		}
		value, err := strconv.Unquote(raw)
		if err != nil {
			return "", fmt.Errorf("invalid double-quoted value: %w", err)
		}
		return value, nil
	}
	for i, char := range raw {
		if char == '#' && i > 0 && (raw[i-1] == ' ' || raw[i-1] == '\t') {
			return strings.TrimSpace(raw[:i]), nil
		}
	}
	return raw, nil
}

func envValue(fileEnv map[string]string, name string, preferEnvFile bool) string {
	if preferEnvFile {
		if value, ok := fileEnv[name]; ok {
			return strings.TrimSpace(value)
		}
	}
	if value, ok := os.LookupEnv(name); ok {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(fileEnv[name])
}

func (c config) openRouterKey() string {
	if key := strings.TrimSpace(c.OpenRouterAPIKey); key != "" {
		return key
	}
	if c.preferEnvFile {
		return ""
	}
	return strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
}

// degradationDefault is the state degradation recovery starts in before any dashboard
// keystroke has been saved. ORR_DEGRADATION seeds it; an unset variable leaves
// the feature on.
func (c config) degradationDefault() bool {
	if c.Degradation != nil {
		return *c.Degradation
	}
	return true
}

func (c config) validate() error {
	if strings.TrimSpace(c.Listen) == "" {
		return fmt.Errorf("listen cannot be empty")
	}
	u, err := url.Parse(c.Upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("upstream must be a valid http(s) URL")
	}
	if c.PinTTL < 0 {
		return fmt.Errorf("ORR_PIN_TTL cannot be negative")
	}
	if c.UpdateMaxProviders < 1 {
		return fmt.Errorf("ORR_UPDATE_MAX_PROVIDERS must be at least 1")
	}
	if c.RateLimitFailoverThreshold < 1 {
		return fmt.Errorf("ORR_429_FAILOVER_THRESHOLD must be at least 1")
	}
	if c.DegradationThreshold <= 0 || c.DegradationThreshold > 1 {
		return fmt.Errorf("ORR_DEGRADATION_THRESHOLD must be above 0 and at most 1")
	}
	if c.DegradationWindow <= 0 || c.DegradationWindow >= degradationBaselineWindow {
		return fmt.Errorf("ORR_DEGRADATION_WINDOW must be positive and shorter than the %s baseline window", degradationBaselineWindow)
	}
	if c.DegradationMinSamples < 1 {
		return fmt.Errorf("ORR_DEGRADATION_MIN_SAMPLES must be at least 1")
	}
	if c.DegradationCooldown <= 0 {
		return fmt.Errorf("ORR_DEGRADATION_COOLDOWN must be positive")
	}
	if c.DegradationTimeout <= 0 {
		return fmt.Errorf("ORR_DEGRADATION_TIMEOUT must be positive")
	}
	return validateModels(c.Models)
}

// validateModels rejects stored routing values the upstream API would refuse, so
// a hand-edited row fails at startup instead of turning every request for that
// model into a 400.
func validateModels(models map[string]providerConfig) error {
	for model, provider := range models {
		parts := strings.SplitN(model, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("model %q must use author/model format", model)
		}
		if provider.DataCollection != "" && provider.DataCollection != "allow" && provider.DataCollection != "deny" {
			return fmt.Errorf("model %s: data_collection must be allow or deny", model)
		}
		if provider.Sort != "" && provider.Sort != "price" && provider.Sort != "throughput" && provider.Sort != "latency" {
			return fmt.Errorf("model %s: sort must be price, throughput, or latency", model)
		}
	}
	return nil
}

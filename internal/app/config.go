package app

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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
	providersPath              string
}

const defaultRateLimitFailoverThreshold = 2

type providersFile struct {
	Version int                       `yaml:"version"`
	Models  map[string]providerConfig `yaml:"models"`
}

const providersFileVersion = 1

type providerConfig struct {
	Order             []string  `json:"order,omitempty" yaml:"order,omitempty"`
	Only              []string  `json:"only,omitempty" yaml:"only,omitempty"`
	Ignore            []string  `json:"ignore,omitempty" yaml:"ignore,omitempty"`
	AllowFallbacks    *bool     `json:"allow_fallbacks,omitempty" yaml:"allow_fallbacks,omitempty"`
	RequireParameters *bool     `json:"require_parameters,omitempty" yaml:"require_parameters,omitempty"`
	DataCollection    string    `json:"data_collection,omitempty" yaml:"data_collection,omitempty"`
	ZDR               *bool     `json:"zdr,omitempty" yaml:"zdr,omitempty"`
	Sort              string    `json:"sort,omitempty" yaml:"sort,omitempty"`
	ManualPin         string    `json:"-" yaml:"manual_pin,omitempty"`
	UpdatedAt         time.Time `json:"-" yaml:"updated_at,omitempty"`
	// Blocked are the providers OpenRouter refuses to route to for this
	// account, discovered from a routing refusal. It is orr's own bookkeeping
	// and never part of the upstream provider block, hence `json:"-"`.
	Blocked []blockedProvider `json:"-" yaml:"blocked,omitempty"`
}

func (p providerConfig) hasRouting() bool {
	return len(p.Order) > 0 || len(p.Only) > 0 || len(p.Ignore) > 0 || p.AllowFallbacks != nil ||
		p.RequireParameters != nil || p.DataCollection != "" || p.ZDR != nil || p.Sort != ""
}

func loadConfig(envPath string) (config, error) {
	cfg := config{
		Listen:                     "127.0.0.1:8787",
		Upstream:                   openRouterAPI,
		LogRequests:                true,
		PinTTL:                     time.Hour,
		UpdateMaxProviders:         20,
		UpdateCacheOnly:            false,
		RateLimitFailoverThreshold: defaultRateLimitFailoverThreshold,
	}

	fileEnv, err := readDotEnv(envPath)
	if err != nil {
		return config{}, err
	}
	value := func(name string) string { return envValue(fileEnv, name) }
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
	cfg.OpenRouterAPIKey = value("OPENROUTER_API_KEY")

	providersPath := configuredProvidersPath(envPath, fileEnv)
	providerData, providerErr := os.ReadFile(providersPath)
	if errors.Is(providerErr, os.ErrNotExist) {
		if err := writeProvidersFileAtomic(providersPath, map[string]providerConfig{}); err != nil {
			return config{}, err
		}
		providerData, providerErr = os.ReadFile(providersPath)
	}
	if providerErr != nil {
		return config{}, fmt.Errorf("read providers file %s: %w", providersPath, providerErr)
	}
	var saved providersFile
	decoder := yaml.NewDecoder(bytes.NewReader(providerData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&saved); err != nil {
		return config{}, fmt.Errorf("parse providers file %s: %w", providersPath, err)
	}
	if saved.Version != providersFileVersion {
		return config{}, fmt.Errorf("unsupported providers file version %d (expected %d)", saved.Version, providersFileVersion)
	}
	cfg.Models = saved.Models
	if cfg.Models == nil {
		cfg.Models = make(map[string]providerConfig)
	}
	cfg.providersPath = providersPath

	if err := cfg.validate(); err != nil {
		return config{}, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// configuredProvidersPath resolves the providers file without loading or
// creating it. Commands such as reset need the same environment precedence as
// serve while leaving the file untouched until they intentionally act on it.
func configuredProvidersPath(envPath string, fileEnv map[string]string) string {
	providersPath := envValue(fileEnv, "ORR_PROVIDERS_FILE")
	if providersPath == "" {
		providersPath = "providers.yaml"
	}
	if !filepath.IsAbs(providersPath) {
		providersPath = filepath.Join(filepath.Dir(envPath), providersPath)
	}
	return filepath.Clean(providersPath)
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

func envValue(fileEnv map[string]string, name string) string {
	if value, ok := os.LookupEnv(name); ok {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(fileEnv[name])
}

func (c config) openRouterKey() string {
	if key := strings.TrimSpace(c.OpenRouterAPIKey); key != "" {
		return key
	}
	return strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
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
	for model, provider := range c.Models {
		parts := strings.SplitN(model, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("model %q must use author/model format", model)
		}
		if provider.DataCollection != "" && provider.DataCollection != "allow" && provider.DataCollection != "deny" {
			return fmt.Errorf("models.%s.data_collection must be allow or deny", model)
		}
		if provider.Sort != "" && provider.Sort != "price" && provider.Sort != "throughput" && provider.Sort != "latency" {
			return fmt.Errorf("models.%s.sort must be price, throughput, or latency", model)
		}
	}
	return nil
}

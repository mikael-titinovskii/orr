package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// registryTTL bounds how long a fetched OpenRouter catalog is reused before the
// next /registry.json request refetches it. The catalog changes rarely, so a
// short TTL keeps each kimi-code /models refresh from hammering OpenRouter while
// still picking up newly published models within minutes.
const registryTTL = 5 * time.Minute

const registryRetryDelay = 30 * time.Second

// modelRegistry serves the OpenRouter model catalog in kimi-code's
// models.dev-style custom-registry format. See the kimi-code config docs for the
// expected schema: a JSON object keyed by provider id, each entry carrying id,
// name, api, type, and a models map.
type modelRegistry struct {
	client   *http.Client
	upstream string
	apiKey   string
	ttl      time.Duration
	baseURL  string

	mu       sync.Mutex
	document map[string]any
	fetched  time.Time
	retryAt  time.Time
	lastErr  error
	now      func() time.Time
}

func newModelRegistry(client *http.Client, upstream, apiKey, listen string, ttl time.Duration) *modelRegistry {
	return &modelRegistry{
		client:   client,
		upstream: strings.TrimRight(upstream, "/"),
		apiKey:   apiKey,
		ttl:      ttl,
		baseURL:  registryBaseURL(listen),
		now:      time.Now,
	}
}

// registryBaseURL derives the proxy's own /v1 base URL from the listen address
// so kimi-code can route requests back through the proxy.
func registryBaseURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err == nil {
		if host == "" || host == "0.0.0.0" {
			host = "127.0.0.1"
		} else if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
			host = "::1"
		}
		listen = net.JoinHostPort(host, port)
	}
	return "http://" + listen + "/v1"
}

// ServeJSON writes the registry document as application/json, fetching and
// caching the upstream catalog on a cache miss or after the TTL elapses.
func (r *modelRegistry) ServeJSON(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "only GET and HEAD are supported")
		return
	}
	doc, err := r.documentFor(req.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "OpenRouter catalog unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if req.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(w).Encode(doc)
}

// documentFor returns the current registry document, refreshing it from the
// upstream catalog when the cache is empty or stale.
func (r *modelRegistry) documentFor(ctx context.Context) (map[string]any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.document != nil && r.now().Sub(r.fetched) < r.ttl {
		return r.document, nil
	}
	if r.now().Before(r.retryAt) {
		if r.document != nil {
			return r.document, nil
		}
		return nil, r.lastErr
	}
	doc, err := r.fetch(ctx)
	if err != nil {
		// A cancelled caller must not suppress a healthy subsequent refresh.
		if ctx.Err() == nil {
			r.lastErr = err
			r.retryAt = r.now().Add(registryRetryDelay)
		}
		// Keep serving a stale document on a transient upstream failure rather
		// than failing the whole refresh.
		if r.document != nil {
			return r.document, nil
		}
		return nil, err
	}
	r.document = doc
	r.fetched = r.now()
	r.retryAt = time.Time{}
	r.lastErr = nil
	return doc, nil
}

// fetch pulls the OpenRouter catalog and transforms it into the registry
// document keyed by the "openrouter" provider id.
func (r *modelRegistry) fetch(ctx context.Context) (map[string]any, error) {
	var catalog struct {
		Data []registryCatalogEntry `json:"data"`
	}
	if err := fetchQualityJSON(ctx, r.client, r.upstream+"/models", r.apiKey, &catalog); err != nil {
		return nil, err
	}
	models := make(map[string]any, len(catalog.Data))
	for _, entry := range catalog.Data {
		model, ok := toRegistryModel(entry)
		if !ok {
			continue
		}
		models[entry.ID] = model
	}
	// Kimi removes models absent from the registry. Never replace a good
	// catalog with an empty or structurally invalid upstream response.
	if len(models) == 0 {
		return nil, errors.New("OpenRouter catalog contains no usable models")
	}
	provider := map[string]any{
		"id":     "openrouter",
		"name":   "OpenRouter (orr)",
		"api":    r.baseURL,
		"type":   "openai",
		"models": models,
	}
	return map[string]any{"openrouter": provider}, nil
}

// registryCatalogEntry is the subset of an OpenRouter /api/v1/models catalog
// entry the registry needs.
type registryCatalogEntry struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	ContextLength       int      `json:"context_length"`
	SupportedParameters []string `json:"supported_parameters"`
	TopProvider         struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
	Architecture struct {
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
	Reasoning struct {
		SupportedEfforts []string `json:"supported_efforts"`
		DefaultEffort    string   `json:"default_effort"`
	} `json:"reasoning"`
}

// toRegistryModel maps one catalog entry to kimi-code's model schema. It returns
// false when the entry has no usable id.
func toRegistryModel(entry registryCatalogEntry) (map[string]any, bool) {
	if strings.TrimSpace(entry.ID) == "" {
		return nil, false
	}
	limit := make(map[string]any)
	if entry.ContextLength > 0 {
		limit["context"] = entry.ContextLength
	}
	if entry.TopProvider.MaxCompletionTokens > 0 {
		limit["output"] = entry.TopProvider.MaxCompletionTokens
	}
	model := map[string]any{
		"id":        entry.ID,
		"name":      entry.Name,
		"tool_call": containsString(entry.SupportedParameters, "tools"),
		"reasoning": containsString(entry.SupportedParameters, "reasoning"),
	}
	if len(limit) > 0 {
		model["limit"] = limit
	}
	if len(entry.Architecture.InputModalities) > 0 || len(entry.Architecture.OutputModalities) > 0 {
		model["modalities"] = map[string]any{
			"input":  entry.Architecture.InputModalities,
			"output": entry.Architecture.OutputModalities,
		}
	}
	if len(entry.Reasoning.SupportedEfforts) > 0 {
		model["support_efforts"] = entry.Reasoning.SupportedEfforts
	}
	if entry.Reasoning.DefaultEffort != "" {
		model["default_effort"] = entry.Reasoning.DefaultEffort
	}
	return model, true
}

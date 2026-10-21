package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func endpointTag(ep modelEndpoint) string { return ep.Tag }

func TestRankUpdateEndpointsMatchesProviderRanking(t *testing.T) {
	params := []string{"tools", "tool_choice"}
	endpoints := []modelEndpoint{
		{Tag: "fast", Status: 0, SupportedParameters: params, Throughput: &percentiles{P50: 100}, Pricing: endpointPricing{InputCacheRead: 0.000003}, Latency: &percentiles{P50: 300}},
		{Tag: "cheap", Status: 0, SupportedParameters: params, Throughput: &percentiles{P50: 100}, Pricing: endpointPricing{InputCacheRead: 0.000001}, Latency: &percentiles{P50: 500}},
		{Tag: "low-latency", Status: 0, SupportedParameters: params, Throughput: &percentiles{P50: 100}, Pricing: endpointPricing{InputCacheRead: 0.000001}, Latency: &percentiles{P50: 100}},
	}
	got := rankUpdateEndpoints(endpoints, false)
	want := []string{"low-latency", "cheap", "fast"}
	for i, endpoint := range got {
		if endpoint.Tag != want[i] {
			t.Fatalf("ranked endpoint %d = %q, want %q", i, endpoint.Tag, want[i])
		}
	}
}

func TestWriteProvidersFileDoesNotRewriteDotEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	original := []byte("ORR_LISTEN=127.0.0.1:8787\n")
	if err := os.WriteFile(envPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	providersPath := filepath.Join(dir, "providers.yaml")
	models := map[string]providerConfig{
		"moonshotai/kimi-k3": {Order: []string{"fireworks"}, AllowFallbacks: boolPtr(false)},
	}
	if err := writeProvidersFileAtomic(providersPath, models); err != nil {
		t.Fatal(err)
	}

	gotEnv, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotEnv, original) {
		t.Fatalf(".env was rewritten: %q", gotEnv)
	}
	clearRuntimeEnv(t)
	loaded, err := loadConfig(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Models, models) {
		t.Fatalf("written providers file did not round-trip: %#v", loaded.Models)
	}
}

func TestRankUpdateEndpointsCacheOnlyFilters(t *testing.T) {
	params := []string{"tools", "tool_choice"}
	endpoints := []modelEndpoint{
		{Tag: "fast-no-cache", Status: 0, SupportedParameters: params, Throughput: &percentiles{P50: 100}},
		{Tag: "slow-cache", Status: 0, SupportedParameters: params, Throughput: &percentiles{P50: 10}, Pricing: endpointPricing{InputCacheRead: 0.000001}},
	}
	got := rankUpdateEndpoints(endpoints, true)
	if len(got) != 1 || got[0].Tag != "slow-cache" {
		t.Fatalf("unexpected cache-only order: %#v", got)
	}
}

func TestReplaceOrdersClearsExistingValues(t *testing.T) {
	cfg := config{Models: map[string]providerConfig{
		"moonshotai/kimi-k3": {
			Order:          []string{"old-a", "old-b", "old-c"},
			AllowFallbacks: boolPtr(false),
			ManualPin:      "old-b",
		},
	}}
	replaceOrders(&cfg, map[string][]string{
		"moonshotai/kimi-k3": {"new-a", "new-b"},
	})
	routing := cfg.Models["moonshotai/kimi-k3"]
	if !reflect.DeepEqual(routing.Order, []string{"new-a", "new-b", "old-b"}) {
		t.Fatalf("order was not replaced: %#v", routing.Order)
	}
	if routing.AllowFallbacks == nil || *routing.AllowFallbacks {
		t.Fatal("unrelated routing settings changed")
	}
	if routing.ManualPin != "old-b" {
		t.Fatalf("manual pin changed: %q", routing.ManualPin)
	}
	if routing.UpdatedAt.IsZero() || time.Since(routing.UpdatedAt) > time.Minute {
		t.Fatalf("updated_at not stamped: %v", routing.UpdatedAt)
	}
}

func TestReplaceOrdersDoesNotDuplicatePinnedProvider(t *testing.T) {
	cfg := config{Models: map[string]providerConfig{
		"a/b": {Order: []string{"old"}, ManualPin: "pinned"},
	}}
	replaceOrders(&cfg, map[string][]string{"a/b": {"fast", "pinned"}})
	if got := cfg.Models["a/b"].Order; !reflect.DeepEqual(got, []string{"fast", "pinned"}) {
		t.Fatalf("pinned provider duplicated: %#v", got)
	}
}

func TestUpdateSkipsAliasAndNonToolModels(t *testing.T) {
	clearRuntimeEnv(t)
	const model = "moonshotai/kimi-k3"
	params := []string{"tools", "tool_choice"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := endpointList{}
		switch {
		case strings.HasSuffix(r.URL.Path, "/models/"+model+"/endpoints"):
			result.Data.Endpoints = []modelEndpoint{
				{Tag: "slow", ProviderName: "Slow", Status: 0, SupportedParameters: params, Throughput: &percentiles{P50: 10}},
				{Tag: "fast", ProviderName: "Fast", Status: 0, SupportedParameters: params, Throughput: &percentiles{P50: 100}},
			}
		case strings.HasSuffix(r.URL.Path, "/models/scaledown/summarize/endpoints"):
			result.Data.Endpoints = []modelEndpoint{
				{Tag: "scaledown", ProviderName: "ScaleDown", Status: 0, SupportedParameters: []string{"max_tokens"}},
			}
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	writeTestFile(t, envPath, "OPENROUTER_API_KEY=test\nORR_UPSTREAM="+upstream.URL+"\n")
	writeTestFile(t, filepath.Join(dir, "providers.yaml"), "version: 1\nmodels:\n"+
		"    ~anthropic/claude-fable-latest: {}\n"+
		"    "+model+": {}\n"+
		"    scaledown/summarize:\n"+
		"        order:\n"+
		"            - old\n"+
		"        manual_pin: old\n")

	var output strings.Builder
	if err := runUpdateWithOptions(envPath, false, 20, false, &output); err != nil {
		t.Fatalf("update returned error: %v", err)
	}
	if !strings.Contains(output.String(), "~anthropic/claude-fable-latest: skipped (alias model)") {
		t.Fatalf("alias skip not reported:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "scaledown/summarize: skipped (no tool-capable endpoints)") {
		t.Fatalf("tool filter skip not reported:\n%s", output.String())
	}
	cfg, err := loadConfig(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Models[model].Order; !reflect.DeepEqual(got, []string{"fast", "slow"}) {
		t.Fatalf("updated order = %#v, want [fast slow]", got)
	}
	if got := cfg.Models["~anthropic/claude-fable-latest"]; len(got.Order) != 0 || !got.UpdatedAt.IsZero() {
		t.Fatalf("alias model was updated: %#v", got)
	}
	if got := cfg.Models["scaledown/summarize"]; !reflect.DeepEqual(got.Order, []string{"old"}) || got.ManualPin != "old" {
		t.Fatalf("skipped model lost its order or pin: %#v", got)
	}
}

func TestUpdateWritesSuccessfulOrdersWhenModelsFail(t *testing.T) {
	clearRuntimeEnv(t)
	const (
		good  = "moonshotai/kimi-k3"
		empty = "openai/gpt-6-luna"
		mixed = "z-ai/glm-5.3"
	)
	params := []string{"tools", "tool_choice"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := endpointList{}
		switch {
		case strings.HasSuffix(r.URL.Path, "/models/"+good+"/endpoints"):
			result.Data.Endpoints = []modelEndpoint{
				{Tag: "fast", ProviderName: "Fast", Status: 0, SupportedParameters: params, Throughput: &percentiles{P50: 100}},
			}
		case strings.HasSuffix(r.URL.Path, "/models/"+mixed+"/endpoints"):
			result.Data.Endpoints = []modelEndpoint{
				{Tag: "plain", ProviderName: "Plain", Status: 0, SupportedParameters: []string{"max_tokens"}},
				{Tag: "tools-down", ProviderName: "Tools Down", Status: 1, SupportedParameters: params},
			}
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	writeTestFile(t, envPath, "OPENROUTER_API_KEY=test\nORR_UPSTREAM="+upstream.URL+"\n")
	writeTestFile(t, filepath.Join(dir, "providers.yaml"), "version: 1\nmodels:\n"+
		"    "+good+": {}\n"+
		"    "+empty+":\n"+
		"        order:\n"+
		"            - kept\n"+
		"        manual_pin: kept\n"+
		"    "+mixed+": {}\n")

	err := runUpdateWithOptions(envPath, false, 20, false, io.Discard)
	if err == nil {
		t.Fatal("update did not report the failing models")
	}
	emptyIndex := strings.Index(err.Error(), empty)
	mixedIndex := strings.Index(err.Error(), mixed)
	if emptyIndex < 0 || mixedIndex < 0 || emptyIndex > mixedIndex {
		t.Fatalf("error = %v, want %s before %s in sorted order", err, empty, mixed)
	}
	cfg, loadErr := loadConfig(envPath)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got := cfg.Models[good].Order; !reflect.DeepEqual(got, []string{"fast"}) {
		t.Fatalf("successful order was not written: %#v", got)
	}
	if got := cfg.Models[empty]; !reflect.DeepEqual(got.Order, []string{"kept"}) || got.ManualPin != "kept" {
		t.Fatalf("empty-endpoint model lost its order or pin: %#v", got)
	}
	if got := cfg.Models[mixed]; len(got.Order) != 0 || !got.UpdatedAt.IsZero() {
		t.Fatalf("outage model was updated: %#v", got)
	}
}

func TestFetchUpdateOrderCacheOnlyRequiresCachingEndpoint(t *testing.T) {
	params := []string{"tools", "tool_choice"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		result := endpointList{}
		result.Data.Endpoints = []modelEndpoint{
			{Tag: "fast", ProviderName: "Fast", Status: 0, SupportedParameters: params, Throughput: &percentiles{P50: 100}},
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer upstream.Close()

	result := fetchUpdateOrder(upstream.Client(), upstream.URL, "", "moonshotai/kimi-k3", 20, true)
	if result.err == nil || !strings.Contains(result.err.Error(), "no cache-capable endpoints") {
		t.Fatalf("cache-only result = %#v, want a cache-capable error", result)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

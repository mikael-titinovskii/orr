package app

import (
	"os"
	"path/filepath"
	"reflect"
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

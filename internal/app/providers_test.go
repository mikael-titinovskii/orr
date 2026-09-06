package app

import (
	"bytes"
	"strings"
	"testing"
)

func floatPtr(value float64) *float64 { return &value }

func TestPrintEndpoints(t *testing.T) {
	endpoints := []modelEndpoint{
		{
			Tag:          "modal/mxfp4",
			ProviderName: "Modal",
			Throughput:   &percentiles{P50: 93},
			Latency:      &percentiles{P50: 741},
			Uptime:       floatPtr(99.9),
		},
	}
	var output bytes.Buffer
	printEndpoints(&output, endpoints)
	for _, wanted := range []string{"ENDPOINT", "modal/mxfp4", "93.0", "741 ms", "99.9%"} {
		if !strings.Contains(output.String(), wanted) {
			t.Fatalf("output does not contain %q:\n%s", wanted, output.String())
		}
	}
}

func TestSupports(t *testing.T) {
	endpoint := modelEndpoint{SupportedParameters: []string{"tools", "tool_choice"}}
	if !supports(endpoint, "tools") || supports(endpoint, "audio") {
		t.Fatal("unexpected support result")
	}
}

func TestCompatibleEndpointsSortsAndFilters(t *testing.T) {
	params := []string{"tools", "tool_choice"}
	endpoints := []modelEndpoint{
		{Tag: "slow", Status: 0, SupportedParameters: params, Throughput: &percentiles{P50: 10}},
		{Tag: "fast-no-tools", Status: 0, Throughput: &percentiles{P50: 100}},
		{Tag: "unhealthy", Status: 1, SupportedParameters: params, Throughput: &percentiles{P50: 200}},
		{Tag: "fast", Status: 0, SupportedParameters: params, Throughput: &percentiles{P50: 50}},
	}
	got := compatibleEndpoints(endpoints)
	if len(got) != 2 || got[0].Tag != "fast" || got[1].Tag != "slow" {
		t.Fatalf("unexpected endpoints: %#v", got)
	}
}

func TestCompatibleEndpointsUsesSharedRanking(t *testing.T) {
	params := []string{"tools", "tool_choice"}
	endpoints := []modelEndpoint{
		{Tag: "fast", Status: 0, SupportedParameters: params, Throughput: &percentiles{P50: 100}, Pricing: endpointPricing{InputCacheRead: 0.000003}, Latency: &percentiles{P50: 300}},
		{Tag: "cheap", Status: 0, SupportedParameters: params, Throughput: &percentiles{P50: 100}, Pricing: endpointPricing{InputCacheRead: 0.000001}, Latency: &percentiles{P50: 500}},
		{Tag: "low-latency", Status: 0, SupportedParameters: params, Throughput: &percentiles{P50: 100}, Pricing: endpointPricing{InputCacheRead: 0.000001}, Latency: &percentiles{P50: 100}},
		{Tag: "slower", Status: 0, SupportedParameters: params, Throughput: &percentiles{P50: 50}, Pricing: endpointPricing{InputCacheRead: 0.0000001}, Latency: &percentiles{P50: 50}},
	}
	got := compatibleEndpoints(endpoints)
	if len(got) != 4 {
		t.Fatalf("unexpected endpoint count: %#v", got)
	}
	want := []string{"low-latency", "cheap", "fast", "slower"}
	for i, endpoint := range got {
		if endpoint.Tag != want[i] {
			t.Fatalf("ranked endpoint %d = %q, want %q", i, endpoint.Tag, want[i])
		}
	}
}

func TestRankEndpointsPutsMissingPricesAndLatencyLast(t *testing.T) {
	endpoints := []modelEndpoint{
		{Tag: "missing", Throughput: &percentiles{P50: 100}},
		{Tag: "known", Throughput: &percentiles{P50: 100}, Pricing: endpointPricing{InputCacheRead: 0.000001}, Latency: &percentiles{P50: 100}},
		{Tag: "missing-latency", Throughput: &percentiles{P50: 100}, Pricing: endpointPricing{InputCacheRead: 0.000001}},
	}
	got := rankEndpoints(endpoints, false)
	want := []string{"known", "missing-latency", "missing"}
	for i, endpoint := range got {
		if endpoint.Tag != want[i] {
			t.Fatalf("ranked endpoint %d = %q, want %q", i, endpoint.Tag, want[i])
		}
	}
}

func TestEndpointPricingSupportsCaching(t *testing.T) {
	if (endpointPricing{}).supportsCaching() {
		t.Fatal("empty pricing should not support caching")
	}
	if !(endpointPricing{InputCacheRead: 0.000001}).supportsCaching() {
		t.Fatal("cache read pricing should indicate caching support")
	}
	if !(endpointPricing{InputCacheWrite: 0.000001}).supportsCaching() {
		t.Fatal("cache write pricing should indicate caching support")
	}
}

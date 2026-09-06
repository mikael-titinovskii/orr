package e2e_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type routedRequest struct {
	Order          []string
	Only           []string
	AllowFallbacks *bool
}

func TestRoutingOrdinaryRequestsCreateAndKeepStrictAutomaticPin(t *testing.T) {
	requests := make(chan routedRequest, 2)
	prefetched := make(chan struct{})
	var signalPrefetch sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEndpointCatalog(w, "preferred", "secondary")
			signalPrefetch.Do(func() { close(prefetched) })
			return
		}
		requests <- decodeRouting(t, r)
		writeCompletion(w, "preferred")
	}))
	defer upstream.Close()

	orr := startServer(t, upstream.URL+"/api/v1", configuredProviders)
	select {
	case <-prefetched:
	case <-time.After(2 * time.Second):
		t.Fatal("orr did not prefetch provider endpoints")
	}

	postCompletion(t, orr.baseURL, "first")
	postCompletion(t, orr.baseURL, "second")
	first := <-requests
	second := <-requests
	assertStrictPin(t, first, "preferred")
	assertStrictPin(t, second, "preferred")
}

func TestRoutingManualPinIsStrictFromFirstRequest(t *testing.T) {
	providers := `version: 1
models:
  author/model:
    order: [preferred, manual]
    manual_pin: manual
    updated_at: UPDATED_AT
`
	requests := make(chan routedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEndpointCatalog(w, "preferred", "manual")
			return
		}
		requests <- decodeRouting(t, r)
		writeCompletion(w, "manual")
	}))
	defer upstream.Close()

	orr := startServer(t, upstream.URL+"/api/v1", providers)
	postCompletion(t, orr.baseURL, "manual pin")
	assertStrictPin(t, <-requests, "manual")
}

func TestRoutingDailyBenchmarkPinsMeasuredWinner(t *testing.T) {
	providers := `version: 1
models:
  author/model:
    order: [slow, fast]
    updated_at: 2000-01-01T00:00:00Z
`
	liveRequest := make(chan routedRequest, 2)
	var benchmarkRequests int
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// Catalog measurements deliberately rank slow first. The live request
			// can select fast only if the real daily benchmark updates the pin.
			writeRankedEndpointCatalog(w)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		if _, benchmark := body["max_tokens"]; benchmark {
			routing := routingFromBody(body)
			provider := ""
			if len(routing.Only) > 0 {
				provider = routing.Only[0]
			}
			if provider == "slow" {
				time.Sleep(60 * time.Millisecond)
			}
			mu.Lock()
			benchmarkRequests++
			mu.Unlock()
			writeCompletion(w, provider)
			return
		}
		liveRequest <- routingFromBody(body)
		writeCompletion(w, "fast")
	}))
	defer upstream.Close()

	orr := startServerWithEnv(t, upstream.URL+"/api/v1", providers, map[string]string{
		"OPENROUTER_API_KEY": "benchmark-key",
	})
	postCompletion(t, orr.baseURL, "trigger daily benchmark")
	assertStrictPin(t, <-liveRequest, "slow")
	waitForPersistedOrder(t, orr.providersPath, "fast", "slow")
	postCompletion(t, orr.baseURL, "use daily benchmark winner")
	assertStrictPin(t, <-liveRequest, "fast")

	mu.Lock()
	gotBenchmarkRequests := benchmarkRequests
	mu.Unlock()
	if gotBenchmarkRequests != 6 {
		t.Fatalf("benchmark requests = %d, want 3 rounds for each of 2 providers", gotBenchmarkRequests)
	}
	persisted, err := os.ReadFile(orr.providersPath)
	if err != nil {
		t.Fatal(err)
	}
	if fast := strings.Index(string(persisted), "- fast"); fast < 0 || strings.Index(string(persisted), "- slow") < fast {
		t.Fatalf("daily winner was not persisted first:\n%s", persisted)
	}
}

func TestRoutingDailyBenchmarkCannotReplaceManualPin(t *testing.T) {
	providers := `version: 1
models:
  author/model:
    order: [manual, fast]
    manual_pin: manual
    updated_at: 2000-01-01T00:00:00Z
`
	liveRequest := make(chan routedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeRankedEndpointCatalog(w)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		if _, benchmark := body["max_tokens"]; benchmark {
			routing := routingFromBody(body)
			provider := routing.Only[0]
			if provider == "manual" {
				time.Sleep(60 * time.Millisecond)
			}
			writeCompletion(w, provider)
			return
		}
		liveRequest <- routingFromBody(body)
		writeCompletion(w, "manual")
	}))
	defer upstream.Close()

	orr := startServerWithEnv(t, upstream.URL+"/api/v1", providers, map[string]string{
		"OPENROUTER_API_KEY": "benchmark-key",
	})
	postCompletion(t, orr.baseURL, "manual pin precedence")
	assertStrictPin(t, <-liveRequest, "manual")
	persisted, err := os.ReadFile(orr.providersPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(persisted), "manual_pin: manual") {
		t.Fatalf("daily benchmark removed manual pin:\n%s", persisted)
	}
}

// Selection weighs a discount against the speed it costs. The catalog prices
// "cheap" at exactly half of "fast" on every axis, so these cases pin down the
// proportionality rule: half the price buys up to half the throughput, and no
// more.
func TestRoutingTradesThroughputForAProportionalDiscount(t *testing.T) {
	tests := []struct {
		name        string
		cheapTokens int
		want        string
	}{
		{
			// A tenth of the throughput given up for half the price.
			name:        "half price justifies a small throughput sacrifice",
			cheapTokens: 90,
			want:        "cheap",
		},
		{
			// Less than half the throughput for half the price is no longer
			// worth it, even though the provider is still fast enough to use.
			name:        "half price does not justify a large throughput sacrifice",
			cheapTokens: 40,
			want:        "fast",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			providers := `version: 1
models:
  author/model:
    order: [fast, cheap]
    updated_at: 2000-01-01T00:00:00Z
`
			liveRequest := make(chan routedRequest, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					writeTradeoffEndpointCatalog(w)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode upstream body: %v", err)
					return
				}
				if _, benchmark := body["max_tokens"]; benchmark {
					routing := routingFromBody(body)
					provider := routing.Only[0]
					tokens := 100
					if provider == "cheap" {
						tokens = test.cheapTokens
					}
					// Equal response time makes completion-token counts a stable proxy
					// for the measured throughput ratio.
					time.Sleep(80 * time.Millisecond)
					writeCompletionTokens(w, provider, tokens)
					return
				}
				liveRequest <- routingFromBody(body)
				writeCompletion(w, test.want)
			}))
			defer upstream.Close()

			orr := startServerWithEnv(t, upstream.URL+"/api/v1", providers, map[string]string{
				"OPENROUTER_API_KEY": "benchmark-key",
			})
			postCompletion(t, orr.baseURL, "test the price and speed trade-off")
			assertStrictPin(t, <-liveRequest, "fast")
			other := "fast"
			if test.want == "fast" {
				other = "cheap"
			}
			waitForPersistedOrder(t, orr.providersPath, test.want, other)
			postCompletion(t, orr.baseURL, "use measured price and speed winner")
			assertStrictPin(t, <-liveRequest, test.want)

			persisted, err := os.ReadFile(orr.providersPath)
			if err != nil {
				t.Fatal(err)
			}
			winner := strings.Index(string(persisted), "- "+test.want)
			if winner < 0 || strings.Index(string(persisted), "- "+other) < winner {
				t.Fatalf("winner %q was not persisted first:\n%s", test.want, persisted)
			}
		})
	}
}

func waitForPersistedOrder(t *testing.T, path string, providers ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []byte
	for {
		data, err := os.ReadFile(path)
		last = data
		if err == nil {
			matches := true
			previous := -1
			for _, provider := range providers {
				index := strings.Index(string(data), "- "+provider)
				if index < 0 || index <= previous {
					matches = false
					break
				}
				previous = index
			}
			if matches && !strings.Contains(string(data), "2000-01-01") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider order did not become %v:\n%s", providers, last)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func postCompletion(t *testing.T, baseURL, content string) {
	t.Helper()
	body := `{"model":"author/model","messages":[{"role":"user","content":` + string(mustJSON(t, content)) + `}]}`
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("completion status = %d", response.StatusCode)
	}
}

func decodeRouting(t *testing.T, r *http.Request) routedRequest {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decode upstream body: %v", err)
		return routedRequest{}
	}
	return routingFromBody(body)
}

func routingFromBody(body map[string]any) routedRequest {
	provider, _ := body["provider"].(map[string]any)
	result := routedRequest{
		Order: stringSlice(provider["order"]),
		Only:  stringSlice(provider["only"]),
	}
	if allow, ok := provider["allow_fallbacks"].(bool); ok {
		result.AllowFallbacks = &allow
	}
	return result
}

func stringSlice(value any) []string {
	raw, _ := value.([]any)
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func assertStrictPin(t *testing.T, routing routedRequest, provider string) {
	t.Helper()
	if len(routing.Order) != 0 || len(routing.Only) != 1 || routing.Only[0] != provider || routing.AllowFallbacks == nil || *routing.AllowFallbacks {
		t.Fatalf("routing = %#v, want strict %q pin", routing, provider)
	}
}

func writeEndpointCatalog(w http.ResponseWriter, providers ...string) {
	endpoints := make([]map[string]any, 0, len(providers))
	for _, provider := range providers {
		endpoints = append(endpoints, map[string]any{
			"tag": provider, "provider_name": provider, "status": 0,
			"supported_parameters": []string{"tools", "tool_choice"},
			"throughput_last_30m":  map[string]float64{"p50": 100},
			"latency_last_30m":     map[string]float64{"p50": 1},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"endpoints": endpoints}})
}

func writeRankedEndpointCatalog(w http.ResponseWriter) {
	endpoints := []map[string]any{
		{"tag": "slow", "provider_name": "slow", "status": 0, "supported_parameters": []string{"tools", "tool_choice"}, "throughput_last_30m": map[string]float64{"p50": 200}, "latency_last_30m": map[string]float64{"p50": 1}},
		{"tag": "fast", "provider_name": "fast", "status": 0, "supported_parameters": []string{"tools", "tool_choice"}, "throughput_last_30m": map[string]float64{"p50": 20}, "latency_last_30m": map[string]float64{"p50": 1}},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"endpoints": endpoints}})
}

func writeTradeoffEndpointCatalog(w http.ResponseWriter) {
	endpoints := []map[string]any{
		{
			"tag": "fast", "provider_name": "fast", "status": 0,
			"supported_parameters": []string{"tools", "tool_choice"},
			"throughput_last_30m":  map[string]float64{"p50": 200},
			"latency_last_30m":     map[string]float64{"p50": 1},
			"pricing":              map[string]string{"input_cache_read": "0.02", "completion": "0.04", "prompt": "0.01"},
		},
		{
			"tag": "cheap", "provider_name": "cheap", "status": 0,
			"supported_parameters": []string{"tools", "tool_choice"},
			"throughput_last_30m":  map[string]float64{"p50": 20},
			"latency_last_30m":     map[string]float64{"p50": 1},
			"pricing":              map[string]string{"input_cache_read": "0.01", "completion": "0.02", "prompt": "0.005"},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"endpoints": endpoints}})
}

func writeCompletion(w http.ResponseWriter, provider string) {
	writeCompletionTokens(w, provider, 10)
}

func writeCompletionTokens(w http.ResponseWriter, provider string, completionTokens int) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model": "author/model", "provider": provider,
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": completionTokens},
	})
}

func mustJSON(t *testing.T, value string) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

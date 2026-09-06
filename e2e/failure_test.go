package e2e_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const e2eGuardrailBody = `{"error":{"code":404,"metadata":{"input_endpoint_count":1,"ineligibility_reasons":[{"reason":"zdr-violation-by-guardrail"}],"failed_routing_step":"Filter by Guardrails"}}}`
const e2eProviderUnavailableBody = `{"error":{"message":"No allowed providers are available for the selected model. Providers serving author/model: healthy, but your request's provider.only preference permits only: stale.","code":404}}`

func TestDisappearedAutomaticProviderRetriesHealthyEndpoint(t *testing.T) {
	providers := `version: 1
models:
  author/model:
    order: [stale, healthy]
    updated_at: UPDATED_AT
`
	var mu sync.Mutex
	var attempts []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEndpointCatalog(w, "healthy")
			return
		}
		routing := decodeRouting(t, r)
		provider := routing.Only[0]
		mu.Lock()
		attempts = append(attempts, provider)
		mu.Unlock()
		if provider == "stale" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(e2eProviderUnavailableBody))
			return
		}
		writeCompletion(w, "healthy")
	}))
	defer upstream.Close()

	orr := startServer(t, upstream.URL+"/api/v1", providers)
	postCompletion(t, orr.baseURL, "recover disappeared provider")
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"stale", "healthy"}; !equalStrings(attempts, want) {
		t.Fatalf("attempts = %v, want %v", attempts, want)
	}
}

func TestAutomaticRoutingPersistsConsecutiveRefusalsAcrossRestart(t *testing.T) {
	providers := `version: 1
models:
  author/model:
    order: [blocked-one, blocked-two, healthy]
    updated_at: UPDATED_AT
`
	var mu sync.Mutex
	var attempts []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEndpointCatalog(w, "blocked-one", "blocked-two", "healthy")
			return
		}
		routing := decodeRouting(t, r)
		provider := ""
		if len(routing.Only) == 1 {
			provider = routing.Only[0]
		} else if len(routing.Order) > 0 {
			provider = routing.Order[0]
		}
		mu.Lock()
		attempts = append(attempts, provider)
		mu.Unlock()
		if strings.HasPrefix(provider, "blocked-") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(e2eGuardrailBody))
			return
		}
		writeCompletion(w, "healthy")
	}))
	defer upstream.Close()

	orr := startServer(t, upstream.URL+"/api/v1", providers)
	postCompletion(t, orr.baseURL, "discover consecutive refusals")
	mu.Lock()
	firstAttempts := append([]string(nil), attempts...)
	mu.Unlock()
	if want := []string{"blocked-one", "blocked-two", "healthy"}; !equalStrings(firstAttempts, want) {
		t.Fatalf("first request attempts = %v, want %v", firstAttempts, want)
	}

	data, err := os.ReadFile(orr.providersPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Models map[string]struct {
			Order   []string `yaml:"order"`
			Blocked []struct {
				Provider string `yaml:"provider"`
			} `yaml:"blocked"`
		} `yaml:"models"`
	}
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatalf("parse persisted providers: %v\n%s", err, data)
	}
	model := saved.Models["author/model"]
	if len(model.Order) != 3 || model.Order[0] != "healthy" || len(model.Blocked) != 2 {
		t.Fatalf("persisted refusal state = %+v", model)
	}

	mu.Lock()
	attempts = nil
	mu.Unlock()
	restarted := restartServer(t, orr, upstream.URL+"/api/v1", nil)
	postCompletion(t, restarted.baseURL, "after restart")
	mu.Lock()
	restartAttempts := append([]string(nil), attempts...)
	mu.Unlock()
	if want := []string{"healthy"}; !equalStrings(restartAttempts, want) {
		t.Fatalf("restart attempts = %v, want blocked providers skipped: %v", restartAttempts, want)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func TestDailyBenchmarkSelectsHealthyProviderWhenAnotherFails(t *testing.T) {
	live := make(chan routedRequest, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEndpointCatalog(w, "broken", "healthy")
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		if _, benchmark := body["max_tokens"]; benchmark {
			provider := routingFromBody(body).Only[0]
			if provider == "broken" {
				http.Error(w, "failed", http.StatusBadGateway)
				return
			}
			writeCompletion(w, provider)
			return
		}
		live <- routingFromBody(body)
		writeCompletion(w, "healthy")
	}))
	defer upstream.Close()

	orr := startServerWithEnv(t, upstream.URL+"/api/v1", staleProviders("broken", "healthy"), map[string]string{"OPENROUTER_API_KEY": "benchmark-key"})
	postCompletion(t, orr.baseURL, "partial benchmark failure")
	assertStrictPin(t, <-live, "broken")
	waitForPersistedOrder(t, orr.providersPath, "healthy", "broken")
	postCompletion(t, orr.baseURL, "use healthy measured provider")
	assertStrictPin(t, <-live, "healthy")
}

func TestDailyBenchmarkAllFailuresKeepPreviousWinner(t *testing.T) {
	live := make(chan routedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEndpointCatalog(w, "original", "backup")
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		if _, benchmark := body["max_tokens"]; benchmark {
			http.Error(w, "failed", http.StatusBadGateway)
			return
		}
		live <- routingFromBody(body)
		writeCompletion(w, "original")
	}))
	defer upstream.Close()

	orr := startServerWithEnv(t, upstream.URL+"/api/v1", staleProviders("original", "backup"), map[string]string{"OPENROUTER_API_KEY": "benchmark-key"})
	postCompletion(t, orr.baseURL, "all benchmarks fail")
	// Every benchmark failed, so the refreshed catalog order retains the
	// previous winner and its head becomes the automatic pin.
	assertStrictPin(t, <-live, "original")
	waitForPersistedOrder(t, orr.providersPath, "original", "backup")
	persisted, err := os.ReadFile(orr.providersPath)
	if err != nil {
		t.Fatal(err)
	}
	if original := strings.Index(string(persisted), "- original"); original < 0 || strings.Index(string(persisted), "- backup") < original {
		t.Fatalf("all-failure benchmark replaced previous order:\n%s", persisted)
	}
}

func TestDailyBenchmarkCancelsStalledProviderAndUsesPartialResults(t *testing.T) {
	live := make(chan routedRequest, 1)
	stalledCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEndpointCatalog(w, "winner", "healthy", "stalled")
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		if _, benchmark := body["max_tokens"]; benchmark {
			provider := routingFromBody(body).Only[0]
			if provider == "stalled" {
				<-r.Context().Done()
				select {
				case <-stalledCanceled:
				default:
					close(stalledCanceled)
				}
				return
			}
			if provider == "healthy" {
				time.Sleep(30 * time.Millisecond)
			}
			writeCompletionTokens(w, provider, 20)
			return
		}
		live <- routingFromBody(body)
		writeCompletion(w, "winner")
	}))
	defer upstream.Close()

	orr := startServerWithEnv(t, upstream.URL+"/api/v1", staleProviders("winner", "healthy", "stalled"), map[string]string{"OPENROUTER_API_KEY": "benchmark-key"})
	started := time.Now()
	postCompletion(t, orr.baseURL, "stalled benchmark")
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("daily benchmark delayed the user request by %v", elapsed)
	}
	// The gate released before the benchmark applied a measured winner, so the
	// refreshed catalog head becomes the automatic pin.
	assertStrictPin(t, <-live, "winner")
	select {
	case <-stalledCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("stalled provider request was not canceled")
	}
}

func TestMalformedCatalogLeavesProviderFileAndOrderIntact(t *testing.T) {
	providers := staleProviders("original", "backup")
	live := make(chan routedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"not":"closed"`))
			return
		}
		live <- decodeRouting(t, r)
		writeCompletion(w, "original")
	}))
	defer upstream.Close()

	orr := startServer(t, upstream.URL+"/api/v1", providers)
	before, err := os.ReadFile(orr.providersPath)
	if err != nil {
		t.Fatal(err)
	}
	postCompletion(t, orr.baseURL, "malformed catalog")
	routing := <-live
	assertStrictPin(t, routing, "original")
	after, err := os.ReadFile(orr.providersPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("malformed catalog changed providers file:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func staleProviders(providers ...string) string {
	return "version: 1\nmodels:\n  author/model:\n    order: [" + strings.Join(providers, ", ") + "]\n    updated_at: 2000-01-01T00:00:00Z\n"
}

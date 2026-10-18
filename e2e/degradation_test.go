package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The watchdog judges a provider against its own earlier traffic, so an e2e
// test needs three things the production defaults do not give it inside a test
// budget: a window short enough to separate a baseline from a collapse, a scan
// throttle short enough to be waited out, and no scheduled daily benchmark
// competing for the same provider measurements. The first two are environment
// settings; the third is a seeded stats file.
const (
	degradationWindow    = 300 * time.Millisecond
	degradationIdleGap   = 1200 * time.Millisecond // clears the 1s request-path scan throttle
	degradationProviders = `version: 1
models:
  author/model:
    order: [degrading, healthy]
    allow_fallbacks: false
    updated_at: UPDATED_AT
`
	degradationSingleProvider = `version: 1
models:
  author/model:
    order: [degrading]
    allow_fallbacks: false
    updated_at: UPDATED_AT
`
	degradationManualProvider = `version: 1
models:
  author/model:
    order: [degrading, healthy]
    manual_pin: degrading
    allow_fallbacks: false
    updated_at: UPDATED_AT
`
)

func degradationEnv(overrides map[string]string) map[string]string {
	env := map[string]string{
		"OPENROUTER_API_KEY":          "benchmark-key",
		"ORR_DEGRADATION":             "true",
		"ORR_DEGRADATION_WINDOW":      degradationWindow.String(),
		"ORR_DEGRADATION_MIN_SAMPLES": "1",
		"ORR_DEGRADATION_THRESHOLD":   "0.6",
		"ORR_DEGRADATION_COOLDOWN":    "10m",
		"ORR_DEGRADATION_TIMEOUT":     "5s",
	}
	for name, value := range overrides {
		env[name] = value
	}
	return env
}

// seedBenchmarkRunForToday records a benchmark for the model in the stats file
// the app is about to load, so the once-per-day automatic run does not fire and
// every benchmark request the upstream sees came from the watchdog. The stats
// path follows the platform's user cache directory, and the harness points
// every one of those variables at the test directory.
func seedBenchmarkRunForToday(t *testing.T, dir, model string) {
	t.Helper()
	body := fmt.Sprintf(`{"version":1,"benchmark_runs":{%q:%q}}`, model, time.Now().Format(time.RFC3339))
	for _, path := range []string{
		filepath.Join(dir, "orr", "stats.json"),
		filepath.Join(dir, ".cache", "orr", "stats.json"),
		filepath.Join(dir, ".local", "share", "orr", "stats.json"),
		filepath.Join(dir, "Library", "Caches", "orr", "stats.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// degradationUpstream stands in for OpenRouter. In normal mode both providers
// answer; after degrade() every request to "degrading" fails, including the
// benchmark rounds, so a watchdog benchmark can only pick "healthy". catalog is
// what the endpoint discovery is allowed to offer, which is how a test keeps a
// refresh from adding a provider it started without.
type degradationUpstream struct {
	mu           sync.Mutex
	degraded     bool
	catalog      []string
	benchmarks   chan string
	requests     chan routedRequest
	benchmarkCnt atomic.Int32
}

func newDegradationUpstream(catalog ...string) *degradationUpstream {
	if len(catalog) == 0 {
		catalog = []string{"degrading", "healthy"}
	}
	return &degradationUpstream{
		catalog:    catalog,
		benchmarks: make(chan string, 64),
		requests:   make(chan routedRequest, 64),
	}
}

func (u *degradationUpstream) degrade() {
	u.mu.Lock()
	u.degraded = true
	u.mu.Unlock()
}

func (u *degradationUpstream) failing(provider string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.degraded && provider == "degrading"
}

func (u *degradationUpstream) serve(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEndpointCatalog(w, u.catalog...)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		routing := routingFromBody(body)
		provider := ""
		if len(routing.Only) > 0 {
			provider = routing.Only[0]
		}
		if _, benchmark := body["max_tokens"]; benchmark {
			u.benchmarkCnt.Add(1)
			u.benchmarks <- provider
			if u.failing(provider) {
				http.Error(w, "failed", http.StatusBadGateway)
				return
			}
			writeStreamedCompletion(w, provider, 5*time.Millisecond, 40*time.Millisecond, 40)
			return
		}
		u.requests <- routing
		if u.failing(provider) {
			http.Error(w, "failed", http.StatusBadGateway)
			return
		}
		writeStreamedCompletion(w, provider, 25*time.Millisecond, 75*time.Millisecond, 100)
	}
}

// writeStreamedCompletion answers with a stream whose first chunk and total
// generation time are fixed, so throughput and first-token latency are stable
// numbers the watchdog can compare instead of the near-zero generation time of
// a single JSON body.
func writeStreamedCompletion(w http.ResponseWriter, provider string, first, generation time.Duration, tokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	time.Sleep(first)
	_, _ = io.WriteString(w, `data: {"model":"author/model","provider":"`+provider+`","choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
	flush()
	time.Sleep(generation)
	_, _ = io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":`+strconv.Itoa(tokens)+`}}`+"\n\n")
	flush()
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flush()
}

// postCompletionStatus posts one ordinary chat completion and returns its
// status, tolerating a failing provider so the watchdog can be observed.
func postCompletionStatus(t *testing.T, baseURL string) int {
	t.Helper()
	body := `{"model":"author/model","messages":[{"role":"user","content":"ping"}]}`
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return response.StatusCode
}

func nextRoutedRequest(t *testing.T, requests <-chan routedRequest) routedRequest {
	t.Helper()
	select {
	case routing := <-requests:
		return routing
	case <-time.After(5 * time.Second):
		t.Fatal("no request reached the upstream")
		return routedRequest{}
	}
}

// waitForBenchmarkedProviders waits until the upstream has seen a benchmark
// round for every named provider. Any benchmark request is proof the watchdog
// spent billed work.
func waitForBenchmarkedProviders(t *testing.T, upstream *degradationUpstream, providers ...string) {
	t.Helper()
	wanted := make(map[string]bool, len(providers))
	for _, provider := range providers {
		wanted[provider] = true
	}
	deadline := time.After(10 * time.Second)
	for len(wanted) > 0 {
		select {
		case provider := <-upstream.benchmarks:
			delete(wanted, provider)
		case <-deadline:
			t.Fatalf("watchdog did not benchmark %v", wanted)
		}
	}
}

func TestDegradationBenchmarksAndMovesAutomaticPin(t *testing.T) {
	upstream := newDegradationUpstream()
	server := httptest.NewServer(upstream.serve(t))
	defer server.Close()

	orr := startServerWithSetup(t, server.URL+"/api/v1", degradationProviders, degradationEnv(nil), func(dir string) {
		seedBenchmarkRunForToday(t, dir, "author/model")
	})

	// Baseline: three answered client requests make "degrading" the automatic
	// pin and give the watchdog something to compare against.
	for range 3 {
		if status := postCompletionStatus(t, orr.baseURL); status != http.StatusOK {
			t.Fatalf("baseline status = %d, want 200", status)
		}
		assertStrictPin(t, nextRoutedRequest(t, upstream.requests), "degrading")
	}
	if got := upstream.benchmarkCnt.Load(); got != 0 {
		t.Fatalf("ordinary traffic started %d benchmark rounds, want 0", got)
	}

	// Move the baseline outside the 300ms window and clear the scan throttle.
	time.Sleep(degradationIdleGap)
	upstream.degrade()

	if status := postCompletionStatus(t, orr.baseURL); status != http.StatusBadGateway {
		t.Fatalf("degraded status = %d, want 502", status)
	}
	assertStrictPin(t, nextRoutedRequest(t, upstream.requests), "degrading")

	// The collapse is below the threshold, so the watchdog refreshes the catalog
	// and benchmarks both providers. Only "healthy" answers.
	waitForBenchmarkedProviders(t, upstream, "degrading", "healthy")
	if got := upstream.benchmarkCnt.Load(); got == 0 {
		t.Fatal("degradation benchmark did not reach the upstream")
	}
	// The ranking lands a moment after the last benchmark round, so keep sending
	// ordinary traffic until the automatic pin has moved to the provider that
	// still answers.
	waitForStrictPin(t, orr.baseURL, upstream.requests, "healthy")
}

// waitForStrictPin keeps making ordinary requests until the upstream is reached
// with a strict pin on provider, proving the routing state requests actually
// follow, not just the benchmark's verdict.
func waitForStrictPin(t *testing.T, baseURL string, requests <-chan routedRequest, provider string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status := postCompletionStatus(t, baseURL)
		routing := nextRoutedRequest(t, requests)
		if len(routing.Only) == 1 && routing.Only[0] == provider {
			assertStrictPin(t, routing, provider)
			if status != http.StatusOK {
				t.Fatalf("status for %s = %d, want 200", provider, status)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pin never moved to %q; last routing = %#v", provider, routing)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestDegradationCooldownStopsRepeatedBenchmarks(t *testing.T) {
	// One provider only: after the failed benchmark the pin has nowhere better to
	// go, so the same route keeps failing and the cooldown is what must stop the
	// second run.
	upstream := newDegradationUpstream("degrading")
	server := httptest.NewServer(upstream.serve(t))
	defer server.Close()

	orr := startServerWithSetup(t, server.URL+"/api/v1", degradationSingleProvider, degradationEnv(nil), func(dir string) {
		seedBenchmarkRunForToday(t, dir, "author/model")
	})

	if status := postCompletionStatus(t, orr.baseURL); status != http.StatusOK {
		t.Fatalf("baseline status = %d, want 200", status)
	}
	assertStrictPin(t, nextRoutedRequest(t, upstream.requests), "degrading")

	time.Sleep(degradationIdleGap)
	upstream.degrade()
	if status := postCompletionStatus(t, orr.baseURL); status != http.StatusBadGateway {
		t.Fatalf("degraded status = %d, want 502", status)
	}
	// One run, three benchmark rounds against the only provider.
	waitForBenchmarkedProviders(t, upstream, "degrading")
	if got := upstream.benchmarkCnt.Load(); got == 0 {
		t.Fatal("degradation did not benchmark the failing provider")
	}
	// Let the run finish so its cooldown is stamped, then fail again after the
	// scan throttle has expired.
	time.Sleep(500 * time.Millisecond)
	settled := upstream.benchmarkCnt.Load()
	time.Sleep(degradationIdleGap)
	if status := postCompletionStatus(t, orr.baseURL); status != http.StatusBadGateway {
		t.Fatalf("second degraded status = %d, want 502", status)
	}
	time.Sleep(700 * time.Millisecond)
	if got := upstream.benchmarkCnt.Load(); got != settled {
		t.Fatalf("cooldown let a second benchmark run: %d rounds, want %d", got, settled)
	}
}

func TestDegradationDisabledLeavesRouteAlone(t *testing.T) {
	upstream := newDegradationUpstream()
	server := httptest.NewServer(upstream.serve(t))
	defer server.Close()

	orr := startServerWithSetup(t, server.URL+"/api/v1", degradationProviders, degradationEnv(map[string]string{
		"ORR_DEGRADATION": "false",
	}), func(dir string) {
		seedBenchmarkRunForToday(t, dir, "author/model")
	})

	for range 3 {
		if status := postCompletionStatus(t, orr.baseURL); status != http.StatusOK {
			t.Fatalf("baseline status = %d, want 200", status)
		}
		assertStrictPin(t, nextRoutedRequest(t, upstream.requests), "degrading")
	}
	time.Sleep(degradationIdleGap)
	upstream.degrade()
	if status := postCompletionStatus(t, orr.baseURL); status != http.StatusBadGateway {
		t.Fatalf("degraded status = %d, want 502", status)
	}
	assertStrictPin(t, nextRoutedRequest(t, upstream.requests), "degrading")
	time.Sleep(700 * time.Millisecond)
	if got := upstream.benchmarkCnt.Load(); got != 0 {
		t.Fatalf("switched-off watchdog ran %d benchmark rounds, want 0", got)
	}
}

func TestDegradationLeavesManualPinsAlone(t *testing.T) {
	upstream := newDegradationUpstream()
	server := httptest.NewServer(upstream.serve(t))
	defer server.Close()

	orr := startServerWithSetup(t, server.URL+"/api/v1", degradationManualProvider, degradationEnv(nil), func(dir string) {
		seedBenchmarkRunForToday(t, dir, "author/model")
	})

	for range 3 {
		if status := postCompletionStatus(t, orr.baseURL); status != http.StatusOK {
			t.Fatalf("baseline status = %d, want 200", status)
		}
		assertStrictPin(t, nextRoutedRequest(t, upstream.requests), "degrading")
	}
	time.Sleep(degradationIdleGap)
	upstream.degrade()
	if status := postCompletionStatus(t, orr.baseURL); status != http.StatusBadGateway {
		t.Fatalf("degraded status = %d, want 502", status)
	}
	assertStrictPin(t, nextRoutedRequest(t, upstream.requests), "degrading")
	time.Sleep(700 * time.Millisecond)
	if got := upstream.benchmarkCnt.Load(); got != 0 {
		t.Fatalf("watchdog acted on a manual pin with %d benchmark rounds", got)
	}
}

func TestDegradationIgnoresHealthyTraffic(t *testing.T) {
	upstream := newDegradationUpstream()
	server := httptest.NewServer(upstream.serve(t))
	defer server.Close()

	orr := startServerWithSetup(t, server.URL+"/api/v1", degradationProviders, degradationEnv(nil), func(dir string) {
		seedBenchmarkRunForToday(t, dir, "author/model")
	})

	for range 3 {
		if status := postCompletionStatus(t, orr.baseURL); status != http.StatusOK {
			t.Fatalf("baseline status = %d, want 200", status)
		}
		assertStrictPin(t, nextRoutedRequest(t, upstream.requests), "degrading")
	}
	time.Sleep(degradationIdleGap)
	// Same provider, same shape of answer: an unchanged provider must not spend
	// a benchmark just because the baseline window has moved on.
	for range 3 {
		if status := postCompletionStatus(t, orr.baseURL); status != http.StatusOK {
			t.Fatalf("steady status = %d, want 200", status)
		}
		assertStrictPin(t, nextRoutedRequest(t, upstream.requests), "degrading")
	}
	time.Sleep(700 * time.Millisecond)
	if got := upstream.benchmarkCnt.Load(); got != 0 {
		t.Fatalf("healthy traffic ran %d benchmark rounds, want 0", got)
	}
}

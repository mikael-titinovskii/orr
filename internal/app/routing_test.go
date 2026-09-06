package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// pingSample builds a provider benchmark outcome the way a real
// three-round ping-pong test reports one. The ping generates so few tokens
// that its duration is essentially the wait for the first token, so the two
// measurements are the same here.
func pingSample(tps float64, latency time.Duration) providerTestSample {
	return providerTestSample{tps: tps, ttft: latency, latency: latency, samples: providerTestRounds}
}

// benchmarkResult is one ranking input with both measurements selection needs.
func benchmarkResult(provider string, tps float64, ttft time.Duration) providerTestResult {
	return newProviderTestResult(provider, pingSample(tps, ttft), nil)
}

// testPricing is a complete price list in dollars per token, given in the
// dollars-per-million-tokens units the catalog displays.
func testPricing(inputPerM, outputPerM, cacheReadPerM float64) endpointPricing {
	return endpointPricing{
		Prompt:         decimal(inputPerM / 1_000_000),
		Completion:     decimal(outputPerM / 1_000_000),
		InputCacheRead: decimal(cacheReadPerM / 1_000_000),
	}
}

func TestAutoPinnedProviderExpiresAfterTTL(t *testing.T) {
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.Local)
	routing := newRoutingState(map[string]providerConfig{"a/b": {}}, time.Hour)
	routing.now = func() time.Time { return now }
	routing.autoPin("a/b", "fireworks")
	if got := routing.pinnedProvider("a/b"); got != "fireworks" {
		t.Fatalf("pin not active immediately: %q", got)
	}
	now = now.Add(time.Hour)
	if got := routing.pinnedProvider("a/b"); got != "fireworks" {
		t.Fatalf("pin expired at the inclusive TTL boundary: %q", got)
	}
	now = now.Add(time.Nanosecond)
	if got := routing.pinnedProvider("a/b"); got != "" {
		t.Fatalf("pin should have expired: %q", got)
	}
}

func TestManualPinSurvivesAutomaticChangesErrorsBlocksTimeAndRestart(t *testing.T) {
	const model = "author/model"
	now := time.Date(2026, time.September, 3, 23, 55, 0, 0, time.Local)
	path := filepath.Join(t.TempDir(), "providers.yaml")
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"manual", "measured", "fallback"},
	}}, time.Hour)
	routing.now = func() time.Time { return now }
	routing.setProvidersPath(path)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "manual", Pricing: testPricing(1, 4, .1)},
		{Tag: "measured", Pricing: testPricing(.5, 2, .05)},
		{Tag: "fallback", Pricing: testPricing(2, 8, .2)},
	}
	if err := routing.setManualPin(model, "manual"); err != nil {
		t.Fatal(err)
	}

	// Automatic measurements, a recent provider error, and an account-policy
	// refusal may update fallback state, but none may reinterpret a manual pin.
	results := []providerTestResult{
		benchmarkResult("manual", 20, 3*time.Second),
		benchmarkResult("measured", 300, 100*time.Millisecond),
		benchmarkResult("fallback", 100, 500*time.Millisecond),
	}
	best, autoSelected, err := routing.applyProviderTestResultsAtRevision(
		model, results, benchmarkReferenceProfile, routing.modelRevision(model), map[string]float64{"manual": 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if best != "measured" || autoSelected {
		t.Fatalf("benchmark result = %q auto=%v, want measured retained only as automatic state", best, autoSelected)
	}
	if _, err := routing.markBlocked(model, "manual", "zdr-violation-by-guardrail"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(48*time.Hour + time.Nanosecond)
	if provider, manual := routing.pinInfo(model); provider != "manual" || !manual {
		t.Fatalf("pin after TTL/day change = %q manual=%v", provider, manual)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved providersFile
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	restarted := newRoutingState(saved.Models, time.Hour)
	restarted.now = func() time.Time { return now }
	if provider, manual := restarted.pinInfo(model); provider != "manual" || !manual {
		t.Fatalf("restarted pin = %q manual=%v, want unchanged manual pin", provider, manual)
	}
}

func TestDailyDiscoveryRunsAgainAfterCalendarDayChanges(t *testing.T) {
	const model = "author/model"
	now := time.Date(2026, time.September, 3, 23, 59, 0, 0, time.Local)
	routing := newRoutingState(map[string]providerConfig{model: {
		Order:     []string{"old"},
		UpdatedAt: now,
	}}, time.Hour)
	routing.now = func() time.Time { return now }
	if done := routing.maybeRefreshModelOrder(model, http.DefaultClient, "http://unused", "", 20, false); done != nil {
		t.Fatal("same-day request unexpectedly started discovery")
	}

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		result := endpointList{}
		result.Data.Endpoints = []modelEndpoint{{
			Tag: "new", ProviderName: "New", Status: 0,
			SupportedParameters: []string{"tools", "tool_choice"},
			Throughput:          &percentiles{P50: 100},
		}}
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer upstream.Close()

	now = now.Add(2 * time.Minute)
	done := routing.maybeRefreshModelOrder(model, upstream.Client(), upstream.URL, "", 20, false)
	if done == nil {
		t.Fatal("first request on the next day did not start discovery")
	}
	<-done
	if got := calls.Load(); got != 1 {
		t.Fatalf("catalog calls = %d, want one next-day refresh", got)
	}
	cfg, _ := routing.modelConfig(model)
	if len(cfg.Order) != 1 || cfg.Order[0] != "new" || cfg.UpdatedAt.Format(time.DateOnly) != now.Format(time.DateOnly) {
		t.Fatalf("next-day config = %+v", cfg)
	}
	if done := routing.maybeRefreshModelOrder(model, upstream.Client(), upstream.URL, "", 20, false); done != nil {
		t.Fatal("second request on the refreshed day repeated discovery")
	}
}

func TestProviderTagMatchesPinnedVariantForSameProviderName(t *testing.T) {
	const model = "google/gemini-3.7-flash"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"google-ai-studio", "google-ai-studio/flex", "google-ai-studio/priority"}}}, time.Hour)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "google-ai-studio/flex", ProviderName: "Google AI Studio"},
		{Tag: "google-ai-studio", ProviderName: "Google AI Studio"},
		{Tag: "google-ai-studio/priority", ProviderName: "Google AI Studio"},
	}
	routing.nameMap[model] = map[string]string{"Google AI Studio": "google-ai-studio"}

	// When google-ai-studio/flex is pinned, "Google AI Studio" in response should resolve to google-ai-studio/flex
	routing.pin(model, "google-ai-studio/flex")
	if tag, _ := routing.resolveProvider(model, "Google AI Studio"); tag != "google-ai-studio/flex" {
		t.Fatalf("resolveProvider = %q, want google-ai-studio/flex", tag)
	}

	// When google-ai-studio is pinned, "Google AI Studio" in response should resolve to google-ai-studio
	routing.pin(model, "google-ai-studio")
	if tag, _ := routing.resolveProvider(model, "Google AI Studio"); tag != "google-ai-studio" {
		t.Fatalf("resolveProvider = %q, want google-ai-studio", tag)
	}

	// When unpinned, it defaults to the base tag google-ai-studio
	routing.unpin(model)
	if tag, _ := routing.resolveProvider(model, "Google AI Studio"); tag != "google-ai-studio" {
		t.Fatalf("resolveProvider = %q, want google-ai-studio", tag)
	}
}

func TestRateLimitFailoverMovesOnlyAutomaticPin(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"limited", "blocked", "healthy"},
		Blocked: []blockedProvider{{
			Provider: "blocked",
			Reason:   "guardrail",
		}},
	}}, time.Hour)
	routing.autoPin(model, "limited")
	if replacement, changed := routing.markProviderUnavailableAndFailover(model, "limited"); !changed || replacement != "healthy" {
		t.Fatalf("automatic failover = %q, %v; want healthy, true", replacement, changed)
	}
	if provider, manual := routing.pinInfo(model); provider != "healthy" || manual {
		t.Fatalf("replacement pin = %q manual=%v, want healthy automatic", provider, manual)
	}

	routing.pin(model, "limited")
	if replacement, changed := routing.markProviderUnavailableAndFailover(model, "limited"); changed || replacement != "" {
		t.Fatalf("manual pin was failed over to %q", replacement)
	}
	if provider, manual := routing.pinInfo(model); provider != "limited" || !manual {
		t.Fatalf("manual pin changed: %q manual=%v", provider, manual)
	}
}

func TestRateLimitFailoverClearsAutomaticPinWithoutCandidate(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"only"},
	}}, time.Hour)
	routing.autoPin(model, "only")

	if replacement, accepted := routing.markProviderUnavailableAndFailover(model, "only"); !accepted || replacement != "" {
		t.Fatalf("rate-limit failover = %q, %v; want empty, true", replacement, accepted)
	}
	if provider, manual := routing.pinInfo(model); provider != "" || manual {
		t.Fatalf("pin after exhausted failover = %q manual=%v, want no pin", provider, manual)
	}
}

func TestLateSuccessDoesNotClearRateLimitFailure(t *testing.T) {
	const model = "author/model"
	detectedAt := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"limited", "healthy"},
	}}, time.Hour)
	routing.now = func() time.Time { return detectedAt }
	routing.autoPin(model, "limited")
	if _, accepted := routing.markProviderUnavailableAndFailover(model, "limited"); !accepted {
		t.Fatal("rate-limit failure was not accepted")
	}

	routing.clearProviderUnavailable(model, "limited", detectedAt.Add(-time.Second))
	if !routing.isTemporarilyUnavailable(model, "limited") {
		t.Fatal("request started before failover cleared the rate-limit state")
	}
	routing.clearProviderUnavailable(model, "limited", detectedAt.Add(time.Second))
	if routing.isTemporarilyUnavailable(model, "limited") {
		t.Fatal("request started after failover did not clear the rate-limit state")
	}
}

func TestBenchmarkStartedBeforeRateLimitCannotRestoreFailedProvider(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"fast-limited", "healthy"},
	}}, time.Hour)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "fast-limited", Pricing: testPricing(.1, .4, .01)},
		{Tag: "healthy", Pricing: testPricing(1, 4, .1)},
	}
	routing.autoPin(model, "fast-limited")
	revision := routing.modelRevision(model)
	if replacement, accepted := routing.markProviderUnavailableAndFailover(model, "fast-limited"); !accepted || replacement != "healthy" {
		t.Fatalf("rate-limit failover = %q, %v; want healthy, true", replacement, accepted)
	}

	best, updated, err := routing.applyProviderTestResultsAtRevision(model, []providerTestResult{
		benchmarkResult("fast-limited", 1000, 50*time.Millisecond),
		benchmarkResult("healthy", 100, 300*time.Millisecond),
	}, benchmarkReferenceProfile, revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !updated || best != "healthy" {
		t.Fatalf("benchmark winner = %q updated=%v, want healthy true", best, updated)
	}
	if provider, manual := routing.pinInfo(model); provider != "healthy" || manual {
		t.Fatalf("benchmark restored rate-limited provider: %q manual=%v", provider, manual)
	}
	// The threshold counter may observe the old pin immediately before a
	// concurrent benchmark installs the replacement. The health transition
	// must return that new automatic pin instead of leaking the threshold 429.
	if replacement, accepted := routing.markProviderUnavailableAndFailover(model, "fast-limited"); !accepted || replacement != "healthy" {
		t.Fatalf("concurrent replacement = %q, %v; want healthy, true", replacement, accepted)
	}
	cfg, _ := routing.modelConfig(model)
	if want := []string{"healthy", "fast-limited"}; !equalSlices(cfg.Order, want) {
		t.Fatalf("provider order = %v, want %v", cfg.Order, want)
	}
}

func TestProviderResolutionUsesConfiguredOnlyAndNameMap(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"ordered"},
		Only:  []string{"only"},
	}}, time.Hour)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "ordered", ProviderName: "Ordered Provider"},
		{Tag: "only", ProviderName: "Only Provider"},
		{Tag: "mapped", ProviderName: "Mapped Provider"},
	}
	routing.nameMap[model] = map[string]string{"Mapped Provider": "mapped"}

	for _, test := range []struct {
		name, input, want string
	}{
		{name: "order", input: "Ordered Provider", want: "ordered"},
		{name: "only", input: "Only Provider", want: "only"},
		{name: "name map", input: "Mapped Provider", want: "mapped"},
		{name: "unresolved", input: "Unknown Provider", want: "Unknown Provider"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, resolved := routing.resolveProvider(model, test.input)
			if got != test.want || resolved != (test.name != "unresolved") {
				t.Fatalf("resolveProvider(%q) = %q, %v; want %q, %v", test.input, got, resolved, test.want, test.name != "unresolved")
			}
		})
	}
}

func TestProviderResolutionPrefersPinnedEndpoint(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"provider/base"}}}, time.Hour)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "provider/base", ProviderName: "Provider"},
		{Tag: "provider/flex", ProviderName: "Provider"},
	}
	routing.pin(model, "provider/flex")
	if got, resolved := routing.resolveProvider(model, "Provider"); got != "provider/flex" || !resolved {
		t.Fatalf("pinned provider resolution = %q, %v; want provider/flex, true", got, resolved)
	}
}

func TestRestorePinsSkipsUnresolvedAutomaticDisplayName(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {}}, time.Hour)
	routing.restorePins(map[string]persistedPin{model: {Provider: "Unresolved Provider"}})
	if got := routing.pinnedProvider(model); got != "" {
		t.Fatalf("unresolved automatic pin restored as %q", got)
	}
}

func TestRestorePinResolvesAfterEndpointsFetched(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {}}, time.Hour)
	routing.restorePins(map[string]persistedPin{model: {Provider: "Fireworks"}})
	if got := routing.pinnedProvider(model); got != "" {
		t.Fatalf("pin active before endpoints are known: %q", got)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := endpointList{}
		response.Data.Endpoints = []modelEndpoint{{Tag: "fireworks", ProviderName: "Fireworks"}}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	// refreshEndpoints populates the endpoint cache and resolves the pending pin.
	routing.refreshEndpoints(model, &http.Client{Timeout: 5 * time.Second}, server.URL, "")
	if got := routing.pinnedProvider(model); got != "fireworks" {
		t.Fatalf("pin not resolved after endpoints fetched: %q", got)
	}
}

func TestManualPinBlocksPendingPinResolution(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {}}, time.Hour)
	routing.restorePins(map[string]persistedPin{model: {Provider: "Fireworks"}})
	if err := routing.setManualPin(model, "deepinfra"); err != nil {
		t.Fatal(err)
	}
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "fireworks", ProviderName: "Fireworks"},
	}
	routing.nameMap[model] = map[string]string{"Fireworks": "fireworks"}
	routing.resolvePendingPins(model)
	provider, manual := routing.pinInfo(model)
	if provider != "deepinfra" || !manual {
		t.Fatalf("manual pin overridden by pending pin: %q manual=%v", provider, manual)
	}
}

func TestRefreshModelOrderDoesNotOverwriteNewerManualUpdate(t *testing.T) {
	const model = "author/model"
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		response := endpointList{}
		response.Data.Endpoints = []modelEndpoint{{Tag: "stale", ProviderName: "Stale", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}}}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "providers.yaml")
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"manual-order"}}}, time.Hour)
	routing.setProvidersPath(path)
	client := &http.Client{Timeout: 5 * time.Second}
	done := make(chan struct{})
	go func() {
		routing.refreshModelOrder(model, client, server.URL, "", 20, false)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	if err := routing.setManualPin(model, "manual-order"); err != nil {
		t.Fatal(err)
	}
	close(block)
	<-done

	cfg, _ := routing.modelConfig(model)
	if len(cfg.Order) != 1 || cfg.Order[0] != "manual-order" {
		t.Fatalf("stale refresh overwrote manual order: %#v", cfg.Order)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved providersFile
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if got := saved.Models[model].Order; len(got) != 1 || got[0] != "manual-order" {
		t.Fatalf("stale refresh overwrote persisted order: %#v", got)
	}
}

func TestManualPinRoundTripsThroughProvidersFile(t *testing.T) {
	const model = "a/b"
	path := filepath.Join(t.TempDir(), "providers.yaml")
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"fireworks", "together"}}}, time.Hour)
	routing.setProvidersPath(path)
	if err := routing.setManualPin(model, "together"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved providersFile
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	restarted := newRoutingState(saved.Models, time.Hour)
	if provider, manual := restarted.pinInfo(model); provider != "together" || !manual {
		t.Fatalf("restored manual pin = %q manual=%v", provider, manual)
	}
	if err := routing.setManualPin(model, ""); err != nil {
		t.Fatal(err)
	}
	if provider := routing.pinnedProvider(model); provider != "" {
		t.Fatalf("manual pin remained after clear: %q", provider)
	}
}

func TestManualPinRejectsUnresolvedProvider(t *testing.T) {
	const model = "a/b"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"fireworks"}}}, time.Hour)
	path := filepath.Join(t.TempDir(), "providers.yaml")
	routing.setProvidersPath(path)
	if err := routing.setManualPin(model, "Unknown Provider"); err == nil {
		t.Fatal("unresolved provider accepted")
	}
	if got := routing.pinnedProvider(model); got != "" {
		t.Fatalf("unresolved provider was pinned: %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("provider file changed after rejected pin: %v", err)
	}
}

func TestManualPinPersistsAcrossModelSwitchAndTTL(t *testing.T) {
	routing := newRoutingState(map[string]providerConfig{"a/b": {}, "c/d": {}}, time.Hour)
	routing.pin("a/b", "fireworks")
	routing.mu.Lock()
	pin := routing.pins["a/b"]
	pin.pinnedAt = time.Now().Add(-2 * time.Hour)
	routing.pins["a/b"] = pin
	routing.mu.Unlock()

	routing.setCurrentModel("c/d")
	routing.setCurrentModel("a/b")
	provider, manual := routing.pinInfo("a/b")
	if provider != "fireworks" || !manual {
		t.Fatalf("manual pin did not survive model switch: %q manual=%v", provider, manual)
	}
}

func TestPinnedProviderSurvivesWithinTTL(t *testing.T) {
	routing := newRoutingState(map[string]providerConfig{"a/b": {}}, time.Hour)
	routing.pin("a/b", "fireworks")
	if got := routing.pinnedProvider("a/b"); got != "fireworks" {
		t.Fatalf("pin should be active: %q", got)
	}
}

func TestPinTTLDefaultIsOneHour(t *testing.T) {
	routing := newRoutingState(map[string]providerConfig{"a/b": {}}, 0)
	routing.pin("a/b", "fireworks")
	if got := routing.pinnedProvider("a/b"); got != "fireworks" {
		t.Fatalf("default pin TTL should be 1h: %q", got)
	}
}

func TestAutoPinIsNotManual(t *testing.T) {
	routing := newRoutingState(map[string]providerConfig{"a/b": {}}, time.Hour)
	routing.autoPin("a/b", "fireworks")
	provider, manual := routing.pinInfo("a/b")
	if provider != "fireworks" || manual {
		t.Fatalf("expected auto pin, got %q manual=%v", provider, manual)
	}
}

func TestManualPinOverridesAutoPin(t *testing.T) {
	routing := newRoutingState(map[string]providerConfig{"a/b": {}}, time.Hour)
	routing.autoPin("a/b", "fireworks")
	routing.pin("a/b", "together")
	provider, manual := routing.pinInfo("a/b")
	if provider != "together" || !manual {
		t.Fatalf("expected manual override, got %q manual=%v", provider, manual)
	}
}

func TestAutoPinDoesNotOverrideManualPin(t *testing.T) {
	routing := newRoutingState(map[string]providerConfig{"a/b": {}}, time.Hour)
	routing.pin("a/b", "together")
	routing.autoPin("a/b", "fireworks")
	provider, manual := routing.pinInfo("a/b")
	if provider != "together" || !manual {
		t.Fatalf("manual pin should not be overridden, got %q manual=%v", provider, manual)
	}
}

func TestEnsureAutoPinCreatesOnlyMissingAutomaticChoice(t *testing.T) {
	const model = "a/b"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"first", "second"},
	}}, time.Hour)
	if !routing.ensureAutoPin(model, "first") {
		t.Fatal("ranked head did not create an automatic pin")
	}
	if provider, manual := routing.pinInfo(model); provider != "first" || manual {
		t.Fatalf("automatic choice = %q manual=%v, want first", provider, manual)
	}
	if routing.ensureAutoPin(model, "second") {
		t.Fatal("live automatic pin was unexpectedly replaced")
	}
	routing.pin(model, "second")
	if routing.ensureAutoPin(model, "first") {
		t.Fatal("manual pin was unexpectedly replaced")
	}
}

func TestEnsureAutoPinReplacesBlockedAutomaticChoice(t *testing.T) {
	const model = "a/b"
	now := time.Now()
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"healthy", "blocked"},
		Blocked: []blockedProvider{{
			Provider: "blocked", Reason: "zdr-violation-by-guardrail", DetectedAt: now,
		}},
	}}, time.Hour)
	routing.autoPin(model, "blocked")
	// Install the stale choice directly because autoPin correctly refuses to
	// create a known-blocked pin in normal operation.
	routing.mu.Lock()
	routing.pins[model] = pinnedProvider{provider: "blocked", pinnedAt: now}
	routing.mu.Unlock()

	if !routing.ensureAutoPin(model, "healthy") {
		t.Fatal("blocked automatic pin did not move to the healthy ranked head")
	}
	if provider, manual := routing.pinInfo(model); provider != "healthy" || manual {
		t.Fatalf("replacement pin = %q manual=%v, want automatic healthy", provider, manual)
	}
}

func TestEnsureModelReturnsNewFlag(t *testing.T) {
	routing := newRoutingState(map[string]providerConfig{"a/b": {}}, time.Hour)
	isNew, err := routing.ensureModel("a/b")
	if err != nil || isNew {
		t.Fatalf("existing model reported as new: isNew=%v err=%v", isNew, err)
	}
	isNew, err = routing.ensureModel("c/d")
	if err != nil || !isNew {
		t.Fatalf("new model not reported as new: isNew=%v err=%v", isNew, err)
	}
	isNew, err = routing.ensureModel("c/d")
	if err != nil || isNew {
		t.Fatalf("newly added model reported as new again: isNew=%v err=%v", isNew, err)
	}
}

func TestRefreshModelOrderPopulatesOrderAndCache(t *testing.T) {
	const model = "author/model"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models/author/model/endpoints" {
			http.NotFound(w, r)
			return
		}
		response := endpointList{}
		response.Data.Endpoints = []modelEndpoint{
			{Tag: "fast", ProviderName: "Fast", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}, Throughput: &percentiles{P50: 100}},
			{Tag: "slow", ProviderName: "Slow", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}, Throughput: &percentiles{P50: 10}},
			{Tag: "no-tools", ProviderName: "NoTools", Status: 0, Throughput: &percentiles{P50: 200}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "providers.yaml")
	routing := newRoutingState(map[string]providerConfig{model: {}}, time.Hour)
	routing.setProvidersPath(path)

	client := &http.Client{Timeout: 5 * time.Second}
	routing.refreshModelOrder(model, client, server.URL, "", 20, false)

	cfg, ok := routing.modelConfig(model)
	if !ok {
		t.Fatal("model disappeared")
	}
	if len(cfg.Order) != 2 || cfg.Order[0] != "fast" || cfg.Order[1] != "slow" {
		t.Fatalf("unexpected order: %#v", cfg.Order)
	}
	endpoints := routing.endpoints(model)
	if len(endpoints) != 3 {
		t.Fatalf("expected 3 cached endpoints, got %d", len(endpoints))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved providersFile
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if got := saved.Models[model].Order; len(got) != 2 || got[0] != "fast" || got[1] != "slow" {
		t.Fatalf("order not persisted: %#v", got)
	}
	if saved.Models[model].UpdatedAt.IsZero() {
		t.Fatal("updated_at was not persisted")
	}
}

func TestRefreshModelOrderPreservesManualPin(t *testing.T) {
	const model = "author/model"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := endpointList{}
		response.Data.Endpoints = []modelEndpoint{
			{Tag: "fast", ProviderName: "Fast", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}, Throughput: &percentiles{P50: 100}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "providers.yaml")
	routing := newRoutingState(map[string]providerConfig{model: {ManualPin: "pinned"}}, time.Hour)
	routing.setProvidersPath(path)

	client := &http.Client{Timeout: 5 * time.Second}
	routing.refreshModelOrder(model, client, server.URL, "", 20, false)

	cfg, _ := routing.modelConfig(model)
	if len(cfg.Order) != 2 || cfg.Order[0] != "fast" || cfg.Order[1] != "pinned" {
		t.Fatalf("manual pin not preserved: %#v", cfg.Order)
	}
}

func TestProviderBenchmarksRespectManualPin(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order:     []string{"slow", "manual"},
		ManualPin: "manual",
	}}, time.Hour)
	best, autoSelected, err := routing.applyProviderTestResults(model, []providerTestResult{
		{provider: "slow", tps: 10, latency: time.Second},
		{provider: "fast", tps: 100, latency: 100 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	if best != "fast" || autoSelected {
		t.Fatalf("best = %q auto=%v, want fast without auto selection", best, autoSelected)
	}
	if provider, manual := routing.pinInfo(model); provider != "manual" || !manual {
		t.Fatalf("manual pin changed to %q manual=%v", provider, manual)
	}
	cfg, _ := routing.modelConfig(model)
	if !containsString(cfg.Order, "manual") {
		t.Fatalf("manual pin missing from refreshed order: %v", cfg.Order)
	}
}

func TestProviderBenchmarksIgnoreAllInvalidResults(t *testing.T) {
	const model = "author/model"
	want := []string{"first", "second"}
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: append([]string(nil), want...),
	}}, time.Hour)
	routing.autoPin(model, "first")

	best, autoSelected, err := routing.applyProviderTestResults(model, []providerTestResult{
		{provider: "failed", err: context.DeadlineExceeded},
		{provider: "zero"},
		{provider: "negative", tps: -1, latency: time.Second},
		{provider: "nan", tps: math.NaN(), latency: time.Second},
		{provider: "infinite", tps: math.Inf(1), latency: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if best != "" || autoSelected {
		t.Fatalf("best = %q auto=%v, want no selection", best, autoSelected)
	}
	cfg, _ := routing.modelConfig(model)
	if !equalSlices(cfg.Order, want) {
		t.Fatalf("invalid results changed order to %v, want %v", cfg.Order, want)
	}
	if provider, manual := routing.pinInfo(model); provider != "first" || manual {
		t.Fatalf("invalid results changed pin to %q manual=%v", provider, manual)
	}
}

// A failed or timed-out fresh test must not drop a provider that earlier runs
// already measured. Its pooled rounds still rank it.
func TestProviderBenchmarksUsePooledRoundsWhenFreshTestFails(t *testing.T) {
	const model = "author/model"
	now := time.Now()
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"coreweave/fp8", "streamlake/fp8"},
	}}, time.Hour)
	s := newStats()
	for i := range 3 {
		at := now.Add(-time.Duration(i+1) * time.Minute)
		s.record(requestRecord{Time: at, Model: model, Provider: "coreweave/fp8", Status: 200,
			Duration: time.Second, TTFT: 600 * time.Millisecond, CompletionTokens: 64, Benchmark: true})
		s.record(requestRecord{Time: at, Model: model, Provider: "streamlake/fp8", Status: 200,
			Duration: 700 * time.Millisecond, TTFT: 500 * time.Millisecond, CompletionTokens: 64, Benchmark: true})
	}
	results := mergeMeasuredProviderResults([]providerTestResult{
		{provider: "coreweave/fp8", tps: 150, latency: time.Second},
		{provider: "streamlake/fp8", err: context.DeadlineExceeded},
	}, s.benchmarkPool(model), []string{"coreweave/fp8", "streamlake/fp8"}, now)

	best, _, err := routing.applyProviderTestResults(model, results)
	if err != nil {
		t.Fatal(err)
	}
	if best != "streamlake/fp8" {
		t.Fatalf("best = %q, want the pooled winner streamlake/fp8", best)
	}
}

// dealFixture is a provider as the catalog and the benchmark describe it.
type dealFixture struct {
	provider string
	tps      float64
	ttft     time.Duration
	input    float64 // $/M input tokens
	output   float64 // $/M output tokens
	cache    float64 // $/M cache-read tokens
}

// selectFrom ranks fixtures against profile and returns the winner and the
// full order, wiring up the endpoint catalog the way a refresh would.
func selectFrom(t *testing.T, profile requestProfile, incumbent string, fixtures ...dealFixture) (string, []string) {
	t.Helper()
	const model = "author/model"
	order := make([]string, 0, len(fixtures))
	for _, fixture := range fixtures {
		order = append(order, fixture.provider)
	}
	routing := newRoutingState(map[string]providerConfig{model: {Order: order}}, time.Hour)
	results := make([]providerTestResult, 0, len(fixtures))
	for _, fixture := range fixtures {
		routing.endpointCache[model] = append(routing.endpointCache[model], endpointMeta{
			Tag:     fixture.provider,
			Pricing: testPricing(fixture.input, fixture.output, fixture.cache),
		})
		results = append(results, benchmarkResult(fixture.provider, fixture.tps, fixture.ttft))
	}
	if incumbent != "" {
		routing.autoPin(model, incumbent)
	}
	best, _, err := routing.applyProviderTestResultsAtRevision(model, results, profile, routing.modelRevision(model), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := routing.modelConfig(model)
	return best, cfg.Order
}

// A provider that is far ahead on throughput used to be the only member of the
// performance-leading group, which made its own latency the baseline its
// latency was tested against: it could not be recognized as an outlier however
// slow it became. Pricing and timing one reference request removes the
// self-comparison — an eight-second wait is eight seconds whether or not
// anything else is close on throughput.
func TestSelectionRejectsLatencyOutlierThatLeadsOnThroughput(t *testing.T) {
	field := []dealFixture{
		{provider: "streamlake/fp8", tps: 466, ttft: 8000 * time.Millisecond, input: 0.50, output: 2.00, cache: 0.05},
		{provider: "relace/fp4", tps: 188, ttft: 668 * time.Millisecond, input: 0.55, output: 2.20, cache: 0.06},
		{provider: "baidu/fp8", tps: 162, ttft: 903 * time.Millisecond, input: 0.60, output: 2.40, cache: 0.06},
		{provider: "coreweave/fp8", tps: 134, ttft: 647 * time.Millisecond, input: 0.58, output: 2.30, cache: 0.06},
	}
	best, order := selectFrom(t, benchmarkReferenceProfile, "", field...)
	if best == "streamlake/fp8" {
		t.Fatalf("best = %q, want the eight-second leader rejected", best)
	}
	if order[len(order)-1] != "streamlake/fp8" {
		t.Fatalf("order = %v, want the outlier last", order)
	}

	// The same provider at a normal first token is the best deal in the field:
	// the guardrail reacts to the latency, not to the provider.
	field[0].ttft = 1584 * time.Millisecond
	best, _ = selectFrom(t, benchmarkReferenceProfile, "", field...)
	if best != "streamlake/fp8" {
		t.Fatalf("best = %q, want streamlake/fp8 once its latency is normal", best)
	}
}

// A small throughput sacrifice is worth taking when the discount is
// proportionally larger, which is the case the hard throughput band could not
// express: the cheaper provider fell outside the band and was never priced.
func TestSelectionTradesSpeedForAProportionallyLargerDiscount(t *testing.T) {
	best, order := selectFrom(t, benchmarkReferenceProfile, "",
		dealFixture{provider: "premium", tps: 200, ttft: 400 * time.Millisecond, input: 1.00, output: 4.00, cache: 0.10},
		// A fifth of the throughput would be too slow to be worth any
		// discount, but a quarter less speed for less than half the price is
		// the better deal.
		dealFixture{provider: "value", tps: 150, ttft: 400 * time.Millisecond, input: 0.40, output: 1.60, cache: 0.04},
	)
	if best != "value" {
		t.Fatalf("best = %q, want value", best)
	}
	if order[0] != "value" || order[1] != "premium" {
		t.Fatalf("order = %v, want value ahead of premium", order)
	}
}

// The mirror image: the fastest provider does not win by being fastest. A
// premium that buys proportionally less speed than it costs is refused, which
// is what keeps selection from paying any price to hold the top of the table.
func TestSelectionRefusesAPremiumThatBuysTooLittleSpeed(t *testing.T) {
	best, _ := selectFrom(t, benchmarkReferenceProfile, "",
		// 10% more throughput for 80% more money.
		dealFixture{provider: "fastest", tps: 220, ttft: 400 * time.Millisecond, input: 1.80, output: 7.20, cache: 0.18},
		dealFixture{provider: "balanced", tps: 200, ttft: 400 * time.Millisecond, input: 1.00, output: 4.00, cache: 0.10},
	)
	if best != "balanced" {
		t.Fatalf("best = %q, want balanced", best)
	}
}

// A premium is accepted when it does buy proportionally more speed, so the
// mechanism is not simply biased toward the cheapest provider.
func TestSelectionPaysAPremiumThatBuysProportionallyMoreSpeed(t *testing.T) {
	best, _ := selectFrom(t, benchmarkReferenceProfile, "",
		// Twice the throughput for 40% more money.
		dealFixture{provider: "fast", tps: 400, ttft: 300 * time.Millisecond, input: 1.40, output: 5.60, cache: 0.14},
		dealFixture{provider: "cheap", tps: 200, ttft: 300 * time.Millisecond, input: 1.00, output: 4.00, cache: 0.10},
	)
	if best != "fast" {
		t.Fatalf("best = %q, want fast", best)
	}
}

// An exaggerated provider must not set the terms for the rest of the field.
// One provider far faster than everyone else would tighten a ceiling anchored
// only to the fastest duration until the balanced middle fell outside it, so
// the ceiling never drops below the field median.
func TestSelectionKeepsBalancedProvidersViableBesideAnExaggeratedLeader(t *testing.T) {
	best, order := selectFrom(t, benchmarkReferenceProfile, "",
		dealFixture{provider: "exaggerated", tps: 2000, ttft: 200 * time.Millisecond, input: 9.00, output: 36.00, cache: 0.90},
		dealFixture{provider: "balanced", tps: 190, ttft: 500 * time.Millisecond, input: 0.90, output: 3.60, cache: 0.09},
		dealFixture{provider: "value", tps: 180, ttft: 550 * time.Millisecond, input: 0.50, output: 2.00, cache: 0.05},
	)
	if best != "value" {
		t.Fatalf("best = %q, want value", best)
	}
	// Being ten times slower than the exaggerated leader must not push the
	// rest of the field out of contention.
	if order[1] != "balanced" {
		t.Fatalf("order = %v, want balanced still ranked as a candidate", order)
	}
}

// The other exaggeration: a provider cheap enough to look attractive on the
// cost-delay product but too slow to use. The time ceiling bounds how much
// wall-clock any discount can buy.
func TestSelectionRejectsAnUnusablySlowBargain(t *testing.T) {
	best, order := selectFrom(t, benchmarkReferenceProfile, "",
		dealFixture{provider: "quick", tps: 300, ttft: 400 * time.Millisecond, input: 1.00, output: 4.00, cache: 0.10},
		dealFixture{provider: "quick-too", tps: 280, ttft: 450 * time.Millisecond, input: 1.05, output: 4.20, cache: 0.11},
		// A tenth of the price, but nine times the wait.
		dealFixture{provider: "glacial", tps: 30, ttft: 500 * time.Millisecond, input: 0.10, output: 0.40, cache: 0.01},
	)
	if best != "quick" {
		t.Fatalf("best = %q, want quick", best)
	}
	if order[len(order)-1] != "glacial" {
		t.Fatalf("order = %v, want glacial last", order)
	}
}

// The time ceiling is anchored to the median as well as to the fastest
// provider, so it can never exclude the whole field: at least half of it is
// always viable and there is always something to select.
func TestSelectionAlwaysLeavesAViableProvider(t *testing.T) {
	best, order := selectFrom(t, benchmarkReferenceProfile, "",
		dealFixture{provider: "slow-a", tps: 12, ttft: 9 * time.Second, input: 1.00, output: 4.00, cache: 0.10},
		dealFixture{provider: "slow-b", tps: 11, ttft: 11 * time.Second, input: 1.00, output: 4.00, cache: 0.10},
		dealFixture{provider: "slow-c", tps: 10, ttft: 13 * time.Second, input: 1.00, output: 4.00, cache: 0.10},
	)
	if best == "" {
		t.Fatal("no provider selected from an entirely slow field")
	}
	if best != "slow-a" {
		t.Fatalf("best = %q, want the least slow provider", best)
	}
	if len(order) != 3 {
		t.Fatalf("order = %v, want every provider retained", order)
	}
}

// Selection prices the client's own traffic. A model whose requests are almost
// entirely cached prompt is priced on the cache-read rate, so a provider that
// is cheap where the traffic actually lands wins over one that is cheap
// elsewhere.
func TestSelectionPricesTheObservedTrafficShape(t *testing.T) {
	cacheHeavy := requestProfile{promptTokens: 500, cachedTokens: 40_000, completionTokens: 200, observed: true}
	outputHeavy := requestProfile{promptTokens: 500, cachedTokens: 1_000, completionTokens: 8_000, observed: true}
	field := []dealFixture{
		{provider: "cheap-cache", tps: 200, ttft: 500 * time.Millisecond, input: 1.00, output: 8.00, cache: 0.02},
		{provider: "cheap-output", tps: 200, ttft: 500 * time.Millisecond, input: 1.00, output: 1.00, cache: 0.40},
	}
	if best, _ := selectFrom(t, cacheHeavy, "", field...); best != "cheap-cache" {
		t.Fatalf("cache-heavy traffic selected %q, want cheap-cache", best)
	}
	if best, _ := selectFrom(t, outputHeavy, "", field...); best != "cheap-output" {
		t.Fatalf("output-heavy traffic selected %q, want cheap-output", best)
	}
}

// An incomplete price list cannot be costed, and a provider that cannot be
// costed must never be mistaken for a cheap one.
func TestSelectionRanksIncompletePricingBehindPricedProviders(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"no-output-price", "priced"},
	}}, time.Hour)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "no-output-price", Pricing: endpointPricing{Prompt: decimal(0.0000001), InputCacheRead: decimal(0.00000001)}},
		{Tag: "priced", Pricing: testPricing(1.00, 4.00, 0.10)},
	}
	best, _, err := routing.applyProviderTestResultsAtRevision(model, []providerTestResult{
		benchmarkResult("no-output-price", 200, 400*time.Millisecond),
		benchmarkResult("priced", 200, 400*time.Millisecond),
	}, benchmarkReferenceProfile, routing.modelRevision(model), nil)
	if err != nil {
		t.Fatal(err)
	}
	if best != "priced" {
		t.Fatalf("best = %q, want priced", best)
	}
}

// A provider whose responsiveness has not been measured is timed optimistically
// — its duration counts only generation — so it must not outrank a provider
// that has been measured.
func TestSelectionPrefersMeasuredResponsivenessOverUnknown(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"unmeasured-cheap", "measured"},
	}}, time.Hour)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "unmeasured-cheap", Pricing: testPricing(0.10, 0.40, 0.01)},
		{Tag: "measured", Pricing: testPricing(1.00, 4.00, 0.10)},
	}
	best, _, err := routing.applyProviderTestResultsAtRevision(model, []providerTestResult{
		{provider: "unmeasured-cheap", tps: 200},
		benchmarkResult("measured", 200, 400*time.Millisecond),
	}, benchmarkReferenceProfile, routing.modelRevision(model), nil)
	if err != nil {
		t.Fatal(err)
	}
	if best != "measured" {
		t.Fatalf("best = %q, want measured", best)
	}
}

// A single first-token measurement is not evidence of a slow provider. Until
// benchmarkMinLatencySamples measurements exist the figure is treated as
// unknown, so one transient spike cannot demote anything.
func TestSelectionIgnoresASingleUntrustedLatencySample(t *testing.T) {
	spike := providerTestResult{provider: "spiked", tps: 200, ttft: 20 * time.Second, latency: 20 * time.Second, samples: 1}
	if spike.hasResponsiveness() {
		t.Fatal("a single sample should not be trusted as responsiveness")
	}
	sustained := providerTestResult{provider: "spiked", tps: 200, ttft: 20 * time.Second, latency: 20 * time.Second, samples: benchmarkMinLatencySamples}
	if !sustained.hasResponsiveness() {
		t.Fatalf("%d samples should be trusted as responsiveness", benchmarkMinLatencySamples)
	}
}

// The provider holding the automatic pin keeps it against a challenger whose
// deal is only marginally better, and loses it to one that is clearly better.
// Without the margin, providers a fraction of a percent apart would trade the
// pin on every benchmark.
func TestSelectionHoldsThePinAgainstAMarginalChallenger(t *testing.T) {
	incumbent := dealFixture{provider: "incumbent", tps: 200, ttft: 500 * time.Millisecond, input: 1.00, output: 4.00, cache: 0.10}
	marginal := dealFixture{provider: "challenger", tps: 200, ttft: 500 * time.Millisecond, input: 0.99, output: 3.96, cache: 0.099}
	best, order := selectFrom(t, benchmarkReferenceProfile, "incumbent", incumbent, marginal)
	if best != "incumbent" {
		t.Fatalf("best = %q, want the pin held by incumbent", best)
	}
	if order[0] != "incumbent" {
		t.Fatalf("order = %v, want incumbent first so the table matches the pin", order)
	}

	// The same challenger at a real discount takes the pin.
	clear := marginal
	clear.input, clear.output, clear.cache = 0.70, 2.80, 0.07
	best, _ = selectFrom(t, benchmarkReferenceProfile, "incumbent", incumbent, clear)
	if best != "challenger" {
		t.Fatalf("best = %q, want challenger to take the pin on a real discount", best)
	}
}

func TestManualPinPersistenceFailureLeavesPreviousPinAuthoritative(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order:     []string{"original", "replacement"},
		ManualPin: "original",
	}}, time.Hour)
	// Atomic replacement cannot replace a directory with the providers file.
	// The failed write must not commit a different in-memory instruction.
	routing.setProvidersPath(t.TempDir())
	if err := routing.setManualPin(model, "replacement"); err == nil {
		t.Fatal("manual pin update unexpectedly survived a persistence failure")
	}
	if provider, manual := routing.pinInfo(model); provider != "original" || !manual {
		t.Fatalf("pin after failed persistence = %q manual=%v", provider, manual)
	}
	cfg, _ := routing.modelConfig(model)
	if cfg.ManualPin != "original" {
		t.Fatalf("config pin after failed persistence = %q", cfg.ManualPin)
	}
}

func TestSelectionBoundaryContracts(t *testing.T) {
	t.Run("viability ceiling is inclusive at exactly 1.5x median", func(t *testing.T) {
		profile := requestProfile{completionTokens: 1}
		results := []providerTestResult{
			{provider: "median-a", tps: 1, samples: benchmarkMinLatencySamples},
			{provider: "median-b", tps: 1, samples: benchmarkMinLatencySamples},
			{provider: "boundary", tps: 2.0 / 3.0, samples: benchmarkMinLatencySamples},
		}
		deals := rankProviderDeals(results, profile, nil, map[string]int{"median-a": 0, "median-b": 1, "boundary": 2}, "")
		byProvider := make(map[string]providerDeal, len(deals))
		for _, deal := range deals {
			byProvider[deal.result.provider] = deal
		}
		if !byProvider["boundary"].viable {
			t.Fatal("provider at exactly 1.5x the median was rejected")
		}

		results[2].tps = 0.666666
		deals = rankProviderDeals(results, profile, nil, map[string]int{"median-a": 0, "median-b": 1, "boundary": 2}, "")
		for _, deal := range deals {
			if deal.result.provider == "boundary" && deal.viable {
				t.Fatal("provider beyond 1.5x the median remained viable")
			}
		}
	})

	t.Run("challenger must improve the incumbent by at least five percent", func(t *testing.T) {
		deal := func(provider string, score float64) providerDeal {
			return providerDeal{
				result: providerTestResult{provider: provider},
				score:  score, viable: true, priced: true, timed: true,
			}
		}
		incumbent := deal("incumbent", 100)
		if got := applyChurnMargin([]providerDeal{deal("challenger", 95.01), incumbent}, "incumbent"); got[0].result.provider != "incumbent" {
			t.Fatalf("4.99%% improvement replaced incumbent: %q", got[0].result.provider)
		}
		if got := applyChurnMargin([]providerDeal{deal("challenger", 95), incumbent}, "incumbent"); got[0].result.provider != "challenger" {
			t.Fatalf("exact 5%% improvement did not replace incumbent: %q", got[0].result.provider)
		}
	})
}

// Stickiness must not survive the incumbent going bad: once it fails the time
// ceiling it loses the pin immediately, with no margin to hide behind.
func TestSelectionDropsAPinnedProviderThatStopsBeingViable(t *testing.T) {
	best, _ := selectFrom(t, benchmarkReferenceProfile, "incumbent",
		dealFixture{provider: "incumbent", tps: 200, ttft: 30 * time.Second, input: 0.50, output: 2.00, cache: 0.05},
		dealFixture{provider: "healthy", tps: 190, ttft: 500 * time.Millisecond, input: 1.00, output: 4.00, cache: 0.10},
		dealFixture{provider: "healthy-too", tps: 180, ttft: 600 * time.Millisecond, input: 1.10, output: 4.40, cache: 0.11},
	)
	if best != "healthy" {
		t.Fatalf("best = %q, want healthy once the pinned provider stops being viable", best)
	}
}

func TestSelectionPersistsOrderAndRefreshesAutoPin(t *testing.T) {
	const model = "author/model"
	path := filepath.Join(t.TempDir(), "providers.yaml")
	routing := newRoutingState(map[string]providerConfig{model: {
		Order:     []string{"old", "expensive", "best-deal"},
		UpdatedAt: time.Now(),
	}}, time.Hour)
	routing.setProvidersPath(path)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "expensive", Pricing: testPricing(2.00, 8.00, 0.20)},
		{Tag: "best-deal", Pricing: testPricing(1.00, 4.00, 0.10)},
		{Tag: "old"},
	}
	routing.autoPin(model, "old")

	best, autoSelected, err := routing.applyProviderTestResultsAtRevision(model, []providerTestResult{
		{provider: "old", err: fmt.Errorf("failed")},
		benchmarkResult("expensive", 200, 500*time.Millisecond),
		benchmarkResult("best-deal", 200, 500*time.Millisecond),
	}, benchmarkReferenceProfile, routing.modelRevision(model), nil)
	if err != nil {
		t.Fatal(err)
	}
	if best != "best-deal" || !autoSelected {
		t.Fatalf("best = %q auto=%v, want best-deal auto", best, autoSelected)
	}
	want := []string{"best-deal", "expensive", "old"}
	cfg, _ := routing.modelConfig(model)
	if !equalSlices(cfg.Order, want) {
		t.Fatalf("benchmarked order = %v, want %v", cfg.Order, want)
	}
	if provider, manual := routing.pinInfo(model); provider != "best-deal" || manual {
		t.Fatalf("pin = %q manual=%v, want refreshed auto pin", provider, manual)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved providersFile
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if got := saved.Models[model].Order; !equalSlices(got, want) {
		t.Fatalf("persisted benchmark order = %v, want %v", got, want)
	}
}

func TestSelectionIgnoresInvalidResults(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"valid", "nan", "infinite", "zero", "failed"},
	}}, time.Hour)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "valid", Pricing: testPricing(1.00, 4.00, 0.10)},
		{Tag: "nan", Pricing: testPricing(0.01, 0.04, 0.001)},
		{Tag: "infinite", Pricing: testPricing(0.01, 0.04, 0.001)},
		{Tag: "zero", Pricing: testPricing(0.01, 0.04, 0.001)},
	}
	best, _, err := routing.applyProviderTestResultsAtRevision(model, []providerTestResult{
		{provider: "", tps: 1000, ttft: time.Millisecond},
		{provider: "failed", err: context.DeadlineExceeded},
		{provider: "nan", tps: math.NaN(), ttft: time.Millisecond},
		{provider: "infinite", tps: math.Inf(1), ttft: time.Millisecond},
		{provider: "zero", ttft: time.Millisecond},
		benchmarkResult("valid", 200, 500*time.Millisecond),
	}, benchmarkReferenceProfile, routing.modelRevision(model), nil)
	if err != nil {
		t.Fatal(err)
	}
	if best != "valid" {
		t.Fatalf("best = %q, want valid", best)
	}
	cfg, _ := routing.modelConfig(model)
	if cfg.Order[0] != "valid" {
		t.Fatalf("order = %v, want valid first", cfg.Order)
	}
}

func TestSelectionScenarioMatrix(t *testing.T) {
	const model = "author/model"
	tests := []struct {
		name      string
		fixtures  []dealFixture
		incumbent string
		want      []string
	}{
		{
			name:     "single provider",
			fixtures: []dealFixture{{provider: "only", tps: 100, ttft: time.Second, input: 1, output: 4, cache: 0.1}},
			want:     []string{"only"},
		},
		{
			name: "equal speed ranks by price",
			fixtures: []dealFixture{
				{provider: "dear", tps: 100, ttft: time.Second, input: 2, output: 8, cache: 0.2},
				{provider: "cheap", tps: 100, ttft: time.Second, input: 1, output: 4, cache: 0.1},
			},
			want: []string{"cheap", "dear"},
		},
		{
			name: "equal price ranks by duration",
			fixtures: []dealFixture{
				{provider: "slower", tps: 100, ttft: 2 * time.Second, input: 1, output: 4, cache: 0.1},
				{provider: "quicker", tps: 100, ttft: time.Second, input: 1, output: 4, cache: 0.1},
			},
			want: []string{"quicker", "slower"},
		},
		{
			name: "throughput and first token trade off against each other",
			fixtures: []dealFixture{
				// Same price, and the same total wait for the reference
				// request: 1000 tokens at 250/s after a 3s wait, against
				// 1000 tokens at 125/s after no wait at all.
				{provider: "slow-start", tps: 250, ttft: 3 * time.Second, input: 1, output: 4, cache: 0.1},
				{provider: "slow-generation", tps: 125, ttft: 0, input: 1, output: 4, cache: 0.1},
			},
			// The measured provider wins the tie: slow-generation reports no
			// first-token measurement at all.
			want: []string{"slow-start", "slow-generation"},
		},
		{
			name: "complete tie preserves configured order",
			fixtures: []dealFixture{
				{provider: "configured-first", tps: 100, ttft: time.Second, input: 1, output: 4, cache: 0.1},
				{provider: "configured-second", tps: 100, ttft: time.Second, input: 1, output: 4, cache: 0.1},
			},
			want: []string{"configured-first", "configured-second"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			best, order := selectFrom(t, benchmarkReferenceProfile, tt.incumbent, tt.fixtures...)
			if best != tt.want[0] {
				t.Fatalf("best = %q, want %q", best, tt.want[0])
			}
			if !equalSlices(order, tt.want) {
				t.Fatalf("order = %v, want %v", order, tt.want)
			}
			_ = model
		})
	}
}

func TestSelectionDuplicateResultsKeepTheFirstSuccess(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"recovered", "ordinary"},
	}}, time.Hour)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "recovered", Pricing: testPricing(0.50, 2.00, 0.05)},
		{Tag: "ordinary", Pricing: testPricing(1.00, 4.00, 0.10)},
	}
	best, _, err := routing.applyProviderTestResultsAtRevision(model, []providerTestResult{
		{provider: "recovered", err: context.DeadlineExceeded},
		benchmarkResult("ordinary", 200, 500*time.Millisecond),
		benchmarkResult("recovered", 200, 500*time.Millisecond),
	}, benchmarkReferenceProfile, routing.modelRevision(model), nil)
	if err != nil {
		t.Fatal(err)
	}
	if best != "recovered" {
		t.Fatalf("best = %q, want the recovered provider's successful result used", best)
	}

	// The reverse: a later failure does not discard an earlier success.
	routing = newRoutingState(map[string]providerConfig{model: {
		Order: []string{"ordinary", "successful"},
	}}, time.Hour)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "ordinary", Pricing: testPricing(1.00, 4.00, 0.10)},
		{Tag: "successful", Pricing: testPricing(0.50, 2.00, 0.05)},
	}
	best, _, err = routing.applyProviderTestResultsAtRevision(model, []providerTestResult{
		benchmarkResult("successful", 200, 500*time.Millisecond),
		benchmarkResult("ordinary", 200, 500*time.Millisecond),
		{provider: "successful", err: context.DeadlineExceeded},
	}, benchmarkReferenceProfile, routing.modelRevision(model), nil)
	if err != nil {
		t.Fatal(err)
	}
	if best != "successful" {
		t.Fatalf("best = %q, want successful", best)
	}
}

func TestProviderBenchmarksDoNotOverwriteNewerModelRevision(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"slow", "fast"},
	}}, time.Hour)
	benchmarkedRevision := routing.modelRevision(model)

	if err := routing.setManualPin(model, "manual"); err != nil {
		t.Fatal(err)
	}
	best, autoSelected, err := routing.applyProviderTestResultsAtRevision(model, []providerTestResult{
		benchmarkResult("fast", 200, 100*time.Millisecond),
		benchmarkResult("slow", 20, time.Second),
	}, benchmarkReferenceProfile, benchmarkedRevision, nil)
	if err != nil {
		t.Fatal(err)
	}
	if best != "" || autoSelected {
		t.Fatalf("stale benchmark applied: best=%q auto=%v", best, autoSelected)
	}
	cfg, _ := routing.modelConfig(model)
	if want := []string{"slow", "fast"}; !equalSlices(cfg.Order, want) {
		t.Fatalf("stale benchmark changed order to %v, want %v", cfg.Order, want)
	}
	if cfg.ManualPin != "manual" {
		t.Fatalf("newer manual pin = %q, want manual", cfg.ManualPin)
	}
}

func TestProvidersRanksFromEndpointCacheWhenOrderMissing(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {}}, time.Hour)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "slow", Throughput: 10, Latency: 500, Pricing: endpointPricing{}, Compatible: true},
		{Tag: "fast", Throughput: 100, Latency: 200, Pricing: endpointPricing{}, Compatible: true},
		{Tag: "cheap-cache", Throughput: 80, Latency: 300, Pricing: endpointPricing{InputCacheRead: 0.0000001}, Compatible: true},
		// Best metrics but not compatible: excluded, matching the `r` refresh.
		{Tag: "incompatible", Throughput: 1000, Latency: 1},
	}

	got := routing.providers(model, []string{"observed-a", "observed-b"})
	want := []string{"fast", "cheap-cache", "slow"}
	if !equalSlices(got, want) {
		t.Fatalf("providers() = %v, want %v", got, want)
	}
}

func TestProvidersFallsBackToObservedWhenCacheEmpty(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {}}, time.Hour)

	got := routing.providers(model, []string{"observed-a", "observed-b"})
	want := []string{"observed-a", "observed-b"}
	if !equalSlices(got, want) {
		t.Fatalf("providers() = %v, want %v", got, want)
	}
}

func TestProvidersFallsBackToObservedWhenNothingCompatible(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {}}, time.Hour)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "incompatible", Throughput: 1000, Latency: 1},
	}

	got := routing.providers(model, []string{"observed-a", "observed-b"})
	want := []string{"observed-a", "observed-b"}
	if !equalSlices(got, want) {
		t.Fatalf("providers() = %v, want %v", got, want)
	}
}

func TestProvidersOrderMatchesCachedRanking(t *testing.T) {
	// When Order is populated, providers() must use it directly — both the
	// manual-pin path and the catalog-rank path share the same source.
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"custom", "ranked"}}}, time.Hour)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "ranked", Throughput: 100, Latency: 100},
		{Tag: "ignored", Throughput: 1000, Latency: 1},
	}

	got := routing.providers(model, []string{"observed"})
	want := []string{"custom", "ranked"}
	if !equalSlices(got, want) {
		t.Fatalf("providers() = %v, want %v", got, want)
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMaybeRefreshModelOrderSkipsAlreadyUpdatedToday(t *testing.T) {
	const model = "author/model"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.NotFound(w, r)
	}))
	defer server.Close()

	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"existing"}, UpdatedAt: time.Now()}}, time.Hour)
	client := &http.Client{Timeout: 5 * time.Second}
	routing.maybeRefreshModelOrder(model, client, server.URL, "", 20, false)
	time.Sleep(50 * time.Millisecond)
	if calls != 0 {
		t.Fatalf("expected no refresh for model updated today, got %d calls", calls)
	}
}

func TestMaybeRefreshModelOrderTriggersForStaleModel(t *testing.T) {
	const model = "author/model"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := endpointList{}
		response.Data.Endpoints = []modelEndpoint{
			{Tag: "fast", ProviderName: "Fast", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}, Throughput: &percentiles{P50: 100}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"old"}, UpdatedAt: time.Now().Add(-48 * time.Hour)}}, time.Hour)
	client := &http.Client{Timeout: 5 * time.Second}
	routing.maybeRefreshModelOrder(model, client, server.URL, "", 20, false)

	// Wait for the background goroutine.
	for i := 0; i < 50; i++ {
		routing.mu.RLock()
		updating := routing.modelOrderUpdating[model]
		routing.mu.RUnlock()
		if !updating {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cfg, _ := routing.modelConfig(model)
	if len(cfg.Order) != 1 || cfg.Order[0] != "fast" {
		t.Fatalf("stale model was not refreshed: %#v", cfg.Order)
	}
	if cfg.UpdatedAt.Format(time.DateOnly) != time.Now().Format(time.DateOnly) {
		t.Fatalf("updated_at not set to today: %v", cfg.UpdatedAt)
	}
}

func TestMaybeRefreshModelOrderPreventsConcurrentUpdates(t *testing.T) {
	const model = "author/model"
	var calls atomic.Int32
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-block
		response := endpointList{}
		response.Data.Endpoints = []modelEndpoint{
			{Tag: "fast", ProviderName: "Fast", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}, Throughput: &percentiles{P50: 100}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	routing := newRoutingState(map[string]providerConfig{model: {}}, time.Hour)
	client := &http.Client{Timeout: 5 * time.Second}

	for i := 0; i < 5; i++ {
		go routing.maybeRefreshModelOrder(model, client, server.URL, "", 20, false)
	}
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 concurrent refresh, got %d", got)
	}
	close(block)

	// Wait for completion.
	for i := 0; i < 50; i++ {
		routing.mu.RLock()
		updating := routing.modelOrderUpdating[model]
		routing.mu.RUnlock()
		if !updating {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cfg, _ := routing.modelConfig(model)
	if len(cfg.Order) != 1 || cfg.Order[0] != "fast" {
		t.Fatalf("model was not refreshed: %#v", cfg.Order)
	}
}

func TestForceRefreshModelOrderBypassesDailyCheck(t *testing.T) {
	const model = "author/model"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		response := endpointList{}
		response.Data.Endpoints = []modelEndpoint{
			{Tag: "fast", ProviderName: "Fast", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}, Throughput: &percentiles{P50: 100}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"old"}, UpdatedAt: time.Now()}}, time.Hour)
	client := &http.Client{Timeout: 5 * time.Second}
	routing.forceRefreshModelOrder(model, client, server.URL, "", 20, false)

	// Wait for the background goroutine.
	for i := 0; i < 50; i++ {
		routing.mu.RLock()
		updating := routing.modelOrderUpdating[model]
		routing.mu.RUnlock()
		if !updating {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 refresh despite today's update, got %d", got)
	}
	cfg, _ := routing.modelConfig(model)
	if len(cfg.Order) != 1 || cfg.Order[0] != "fast" {
		t.Fatalf("manual refresh did not replace today's order: %#v", cfg.Order)
	}
}

// The first-token figure must come from benchmark rounds only. Every provider
// answers the identical ping, while client traffic reaches whichever provider
// is pinned and carries far larger prompts, so mixing the two would report the
// pinned provider's prompt sizes as a provider being slow to respond.
func TestMeasuredSampleTakesFirstTokenFromBenchmarkRoundsOnly(t *testing.T) {
	const model = "author/model"
	now := time.Now()
	records := []requestRecord{
		// Real client traffic: a large prompt, so a long wait for the first
		// token that says nothing about how this provider compares.
		{Time: now.Add(-time.Minute), Model: model, Provider: "pinned", Status: 200,
			Duration: 30 * time.Second, TTFT: 4 * time.Second, CompletionTokens: 5_000},
		{Time: now.Add(-time.Minute), Model: model, Provider: "pinned", Status: 200,
			Duration: 28 * time.Second, TTFT: 5 * time.Second, CompletionTokens: 4_600},
		// Benchmark rounds against the same ping every provider answers.
		{Time: now.Add(-time.Minute), Model: model, Provider: "pinned", Status: 200,
			Duration: 700 * time.Millisecond, TTFT: 600 * time.Millisecond, CompletionTokens: 16, Benchmark: true},
		{Time: now.Add(-time.Minute), Model: model, Provider: "pinned", Status: 200,
			Duration: 900 * time.Millisecond, TTFT: 800 * time.Millisecond, CompletionTokens: 16, Benchmark: true},
	}
	s := newStats()
	for _, record := range records {
		s.record(record)
	}

	// The pool ranking reads holds only the benchmark rounds.
	pool := s.benchmarkPool(model)
	if len(pool["pinned"]) != 2 {
		t.Fatalf("pool = %+v, want only the 2 benchmark rounds", pool["pinned"])
	}
	result, ok := pooledProviderResult("pinned", pool["pinned"], now)
	if !ok {
		t.Fatal("pooled result missing")
	}
	if result.ttft != 700*time.Millisecond {
		t.Fatalf("ttft = %v, want 700ms (the median of the benchmark rounds)", result.ttft)
	}
	// 16 tokens over a 100ms generation window, not the 5000-token client
	// requests, which would read as a far faster provider.
	if result.tps > 300 {
		t.Fatalf("tps = %v, want the ping's throughput, not the client traffic's", result.tps)
	}

	// The display still reports lived experience, so the client's own requests
	// pull it away from the benchmark-only figure the pool reports.
	tps, latency := providerMeasuredMetrics(records, model, "pinned", now)
	if tps <= result.tps {
		t.Fatalf("displayed mTs = %v, want the client's faster requests counted alongside the pool's %v",
			tps, result.tps)
	}
	if latency <= 900 {
		t.Fatalf("displayed mLat = %v ms, want the client's long requests counted", latency)
	}

	// A provider with no benchmark rounds has no comparable measurement at
	// all, rather than one borrowed from client traffic.
	clientOnly := newStats()
	for _, record := range records[:2] {
		clientOnly.record(record)
	}
	if got := clientOnly.benchmarkPool(model); len(got["pinned"]) != 0 {
		t.Fatalf("client traffic entered the benchmark pool: %+v", got["pinned"])
	}
}

// The pool must survive a restart. Without that, every restart puts ranking
// back to judging providers on a single run's measurement, which is what
// overpays.
func TestBenchmarkPoolSurvivesSaveAndLoad(t *testing.T) {
	const model = "author/model"
	now := time.Now()
	s := newStats()
	for i := range 4 {
		s.record(requestRecord{
			Time: now.Add(-time.Duration(i) * time.Minute), Model: model, Provider: "p", Status: 200,
			Duration: 800 * time.Millisecond, TTFT: 600 * time.Millisecond, CompletionTokens: 64, Benchmark: true,
		})
	}
	// A sample too old to describe the provider any more.
	s.record(requestRecord{
		Time: now.Add(-2 * benchmarkSampleTTL), Model: model, Provider: "p", Status: 200,
		Duration: 800 * time.Millisecond, TTFT: 9 * time.Second, CompletionTokens: 64, Benchmark: true,
	})

	path := filepath.Join(t.TempDir(), "stats.json")
	if err := s.save(path); err != nil {
		t.Fatal(err)
	}
	restored := newStats()
	if err := restored.load(path); err != nil {
		t.Fatal(err)
	}
	pool := restored.benchmarkPool(model)
	if len(pool["p"]) != 4 {
		t.Fatalf("restored pool = %+v, want the 4 recent samples and not the expired one", pool["p"])
	}
	result, ok := pooledProviderResult("p", pool["p"], now)
	if !ok || result.ttft != 600*time.Millisecond {
		t.Fatalf("restored result = %+v ok=%v, want a 600ms median first token", result, ok)
	}
	if !result.hasResponsiveness() {
		t.Fatalf("restored result = %+v, want trusted responsiveness", result)
	}
}

// The pool keeps only the most recent rounds, so a provider that genuinely
// changes is re-ranked within a couple of runs instead of being averaged
// against a week of stale measurements.
func TestBenchmarkPoolKeepsOnlyRecentRounds(t *testing.T) {
	const model = "author/model"
	now := time.Now()
	s := newStats()
	for i := range benchmarkSamplesPerProvider + 7 {
		s.record(requestRecord{
			Time: now.Add(time.Duration(i) * time.Second), Model: model, Provider: "p", Status: 200,
			Duration: 800 * time.Millisecond, TTFT: time.Duration(100+i) * time.Millisecond,
			CompletionTokens: 64, Benchmark: true,
		})
	}
	pool := s.benchmarkPool(model)
	if len(pool["p"]) != benchmarkSamplesPerProvider {
		t.Fatalf("pool holds %d samples, want the cap of %d", len(pool["p"]), benchmarkSamplesPerProvider)
	}
	// The oldest rounds are the ones dropped.
	if pool["p"][0].TTFTms < 107 {
		t.Fatalf("oldest retained sample = %v ms, want the early rounds dropped", pool["p"][0].TTFTms)
	}
}

func TestRequestProfileSummarizesClientTrafficNotBenchmarks(t *testing.T) {
	const model = "author/model"
	now := time.Now()
	benchmarks := []requestRecord{
		{Time: now, Model: model, Provider: "p", Status: 200, PromptTokens: 2, CompletionTokens: 16, Benchmark: true},
		{Time: now, Model: model, Provider: "p", Status: 200, PromptTokens: 2, CompletionTokens: 16, Benchmark: true},
		{Time: now, Model: model, Provider: "p", Status: 200, PromptTokens: 2, CompletionTokens: 16, Benchmark: true},
	}
	if profile := newRequestProfile(benchmarks, model, now); profile.observed {
		t.Fatalf("benchmark rounds produced an observed profile: %+v", profile)
	}
	if profile := newRequestProfile(benchmarks, model, now); profile != benchmarkReferenceProfile {
		t.Fatalf("profile = %+v, want the reference profile", profile)
	}

	traffic := append(benchmarks,
		requestRecord{Time: now, Model: model, Provider: "p", Status: 200,
			PromptTokens: 30_000, CachedTokens: 28_000, CompletionTokens: 900},
		requestRecord{Time: now, Model: model, Provider: "p", Status: 200,
			PromptTokens: 30_000, CachedTokens: 28_000, CompletionTokens: 1_100},
	)
	profile := newRequestProfile(traffic, model, now)
	if !profile.observed {
		t.Fatalf("profile = %+v, want observed", profile)
	}
	if profile.promptTokens != 2_000 || profile.cachedTokens != 28_000 {
		t.Fatalf("profile prompt/cached = %v/%v, want 2000/28000", profile.promptTokens, profile.cachedTokens)
	}
	if profile.completionTokens != 1_000 {
		t.Fatalf("profile completion = %v, want the 1000-token median", profile.completionTokens)
	}
	// Stale traffic outside the measured window is not the client's traffic
	// any more.
	stale := []requestRecord{
		{Time: now.Add(-2 * measuredMetricsTTL), Model: model, Provider: "p", Status: 200, PromptTokens: 999, CompletionTokens: 999},
		{Time: now.Add(-2 * measuredMetricsTTL), Model: model, Provider: "p", Status: 200, PromptTokens: 999, CompletionTokens: 999},
	}
	if profile := newRequestProfile(stale, model, now); profile.observed {
		t.Fatalf("stale traffic produced an observed profile: %+v", profile)
	}
}

// A provider the pool has nothing for — a model's very first benchmark, or a
// provider tested for the first time — is ranked on the fresh result rather
// than dropped.
func TestMergeUsesFreshResultWhenPoolIsEmpty(t *testing.T) {
	merged := mergeMeasuredProviderResults(
		[]providerTestResult{benchmarkResult("p", 150, 400*time.Millisecond)},
		nil, []string{"p"}, time.Now())
	if len(merged) != 1 {
		t.Fatalf("merged = %+v, want one result", merged)
	}
	if merged[0].tps != 150 || merged[0].ttft != 400*time.Millisecond {
		t.Fatalf("merged = %+v, want the fresh benchmark's measurements", merged[0])
	}
	if !merged[0].hasResponsiveness() {
		t.Fatalf("merged = %+v, want trusted responsiveness", merged[0])
	}
}

// Pooled rounds take precedence over the single fresh run, because that is the
// whole point: one run's median is not a good enough measurement to rank on.
func TestMergePrefersPooledRoundsOverTheFreshRun(t *testing.T) {
	const model = "author/model"
	now := time.Now()
	s := newStats()
	for i := range 3 {
		s.record(requestRecord{Time: now.Add(-time.Duration(i+1) * time.Minute),
			Model: model, Provider: "p", Status: 200,
			Duration: time.Second, TTFT: 900 * time.Millisecond, CompletionTokens: 64, Benchmark: true})
	}
	merged := mergeMeasuredProviderResults(
		// A fresh run that happened to measure a suspiciously fast first token.
		[]providerTestResult{benchmarkResult("p", 5000, 10*time.Millisecond)},
		s.benchmarkPool(model), []string{"p"}, now)
	if len(merged) != 1 {
		t.Fatalf("merged = %+v, want one result", merged)
	}
	if merged[0].ttft != 900*time.Millisecond {
		t.Fatalf("merged ttft = %v, want the pool's 900ms rather than the fresh outlier",
			merged[0].ttft)
	}
	if merged[0].tps > 1000 {
		t.Fatalf("merged tps = %v, want the pool's measurement", merged[0].tps)
	}
}

func TestMedianFloatIgnoresASingleOutlyingRound(t *testing.T) {
	if got := medianFloat([]float64{1, 2, 100}); got != 2 {
		t.Fatalf("median = %v, want 2", got)
	}
	if got := medianFloat(nil); got != 0 {
		t.Fatalf("median of nothing = %v, want 0", got)
	}
	if got := providerTestQuorum(3); got != 2 {
		t.Fatalf("quorum(3) = %d, want 2 so one round may fail", got)
	}
	if got := providerTestQuorum(1); got != 1 {
		t.Fatalf("quorum(1) = %d, want 1", got)
	}
}

// The two guardrails do different jobs, and this is the case that needs both.
// A provider that spikes to eight seconds while the field answers in half a
// second is cheap enough that the cost-delay product alone would take it. The
// time ceiling refuses it, because a wait that far outside the field is not
// something any discount should be able to buy.
func TestSelectionRefusesASpikedProviderEvenWhenItIsCheapest(t *testing.T) {
	spiked := dealFixture{provider: "spiked", tps: 400, ttft: 8 * time.Second, input: 0.10, output: 0.40, cache: 0.01}
	field := []dealFixture{
		spiked,
		{provider: "normal-a", tps: 380, ttft: 500 * time.Millisecond, input: 0.55, output: 2.20, cache: 0.06},
		{provider: "normal-b", tps: 360, ttft: 600 * time.Millisecond, input: 0.60, output: 2.40, cache: 0.06},
	}

	// The discount really is large enough to win on the product alone.
	spikedCost, _ := benchmarkReferenceProfile.cost(testPricing(spiked.input, spiked.output, spiked.cache))
	spikedSeconds, _ := benchmarkReferenceProfile.seconds(benchmarkResult(spiked.provider, spiked.tps, spiked.ttft))
	normalCost, _ := benchmarkReferenceProfile.cost(testPricing(0.55, 2.20, 0.06))
	normalSeconds, _ := benchmarkReferenceProfile.seconds(benchmarkResult("normal-a", 380, 500*time.Millisecond))
	if spikedCost*spikedSeconds >= normalCost*normalSeconds {
		t.Fatalf("fixture no longer exercises the ceiling: spiked score %.5f is not the lowest",
			spikedCost*spikedSeconds)
	}

	best, order := selectFrom(t, benchmarkReferenceProfile, "", field...)
	if best != "normal-a" {
		t.Fatalf("best = %q, want normal-a", best)
	}
	if order[len(order)-1] != "spiked" {
		t.Fatalf("order = %v, want the spiked provider last", order)
	}

	// Once the spike clears, the discount is exactly the kind of deal
	// selection should take.
	field[0].ttft = 500 * time.Millisecond
	if best, _ := selectFrom(t, benchmarkReferenceProfile, "", field...); best != "spiked" {
		t.Fatalf("best = %q, want the cheap provider once it responds normally", best)
	}
}

// Automatic mode only selects providers that answer. The benchmark pool holds
// successes only, so a provider failing every recent request would otherwise
// win the pin on its last good week; any API error in the window disqualifies
// it from the pin and from the head of the order, without dropping it from
// the fallbacks.
func TestProviderWithRecentAPIErrorsCannotBeAutoSelected(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"erroring", "steady"},
	}}, time.Hour)
	results := []providerTestResult{
		{provider: "erroring", tps: 200, ttft: 100 * time.Millisecond, latency: 300 * time.Millisecond, samples: 3},
		{provider: "steady", tps: 50, ttft: 400 * time.Millisecond, latency: time.Second, samples: 3},
	}
	apiErrs := map[string]float64{"erroring": 0.83, "steady": 0}

	best, autoSelected, err := routing.applyProviderTestResultsAtRevision(model, results, benchmarkReferenceProfile, routing.modelRevision(model), apiErrs)
	if err != nil {
		t.Fatal(err)
	}
	if best != "steady" || !autoSelected {
		t.Fatalf("best = %q auto=%v, want steady selected over the erroring provider", best, autoSelected)
	}
	cfg, _ := routing.modelConfig(model)
	if cfg.Order[0] != "steady" {
		t.Fatalf("order head = %q, want the erroring provider off the head: %v", cfg.Order[0], cfg.Order)
	}
	if !containsString(cfg.Order, "erroring") {
		t.Fatalf("erroring provider dropped from the order entirely: %v", cfg.Order)
	}

	// A manual pin is the user's instruction: errors do not move it.
	if err := routing.setManualPin(model, "erroring"); err != nil {
		t.Fatal(err)
	}
	best, autoSelected, err = routing.applyProviderTestResultsAtRevision(model, results, benchmarkReferenceProfile, routing.modelRevision(model), apiErrs)
	if err != nil {
		t.Fatal(err)
	}
	if best != "steady" || autoSelected {
		t.Fatalf("best = %q auto=%v under a manual pin, want steady without auto selection", best, autoSelected)
	}
	if provider, manual := routing.pinInfo(model); provider != "erroring" || !manual {
		t.Fatalf("manual pin moved to %q manual=%v", provider, manual)
	}
}

// With every provider erroring there is nobody clean to select; the order is
// kept so the dashboard still describes the field, and no pin is made.
func TestNoSelectionWhenEveryProviderErrors(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"a", "b"},
	}}, time.Hour)
	routing.autoPin(model, "a")
	best, autoSelected, err := routing.applyProviderTestResultsAtRevision(model, []providerTestResult{
		{provider: "a", tps: 200, ttft: 100 * time.Millisecond, latency: 300 * time.Millisecond, samples: 3},
		{provider: "b", tps: 50, ttft: 400 * time.Millisecond, latency: time.Second, samples: 3},
	}, benchmarkReferenceProfile, routing.modelRevision(model), map[string]float64{"a": 1, "b": 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if best != "" || !autoSelected {
		t.Fatalf("best = %q auto-updated=%v, want the failing automatic pin cleared", best, autoSelected)
	}
	if provider, _ := routing.pinInfo(model); provider != "" {
		t.Fatalf("automatic pin survived with every provider erroring: %q", provider)
	}
}

func FuzzAutomaticSelectionNeverChoosesIneligibleProvider(f *testing.F) {
	f.Add(uint8(10), uint8(20), uint8(30), uint8(0), false)
	f.Add(uint8(250), uint8(1), uint8(100), uint8(1), false)
	f.Add(uint8(5), uint8(5), uint8(5), uint8(2), true)
	f.Fuzz(func(t *testing.T, speedA, speedB, speedC, flags uint8, manual bool) {
		const model = "author/model"
		now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.Local)
		providers := []string{"a", "b", "c"}
		cfg := providerConfig{Order: append([]string(nil), providers...)}
		for i, provider := range providers {
			if flags&(1<<i) != 0 {
				cfg.Blocked = append(cfg.Blocked, blockedProvider{Provider: provider, DetectedAt: now})
			}
		}
		routing := newRoutingState(map[string]providerConfig{model: cfg}, time.Hour)
		routing.now = func() time.Time { return now }
		routing.endpointCache[model] = []endpointMeta{
			{Tag: "a", Pricing: testPricing(1, 4, .1)},
			{Tag: "b", Pricing: testPricing(1, 4, .1)},
			{Tag: "c", Pricing: testPricing(1, 4, .1)},
		}
		if manual {
			if err := routing.setManualPin(model, "a"); err != nil {
				t.Fatal(err)
			}
		}
		apiErrors := make(map[string]float64)
		for i, provider := range providers {
			if flags&(1<<(i+3)) != 0 {
				apiErrors[provider] = 1
			}
		}
		results := []providerTestResult{
			benchmarkResult("a", float64(speedA)+1, 100*time.Millisecond),
			benchmarkResult("b", float64(speedB)+1, 200*time.Millisecond),
			benchmarkResult("c", float64(speedC)+1, 300*time.Millisecond),
		}
		best, autoSelected, err := routing.applyProviderTestResultsAtRevision(
			model, results, benchmarkReferenceProfile, routing.modelRevision(model), apiErrors,
		)
		if err != nil {
			t.Fatal(err)
		}
		if best != "" {
			if !containsString(providers, best) {
				t.Fatalf("selected unknown provider %q", best)
			}
			if routing.isBlocked(model, best) || apiErrors[best] > 0 {
				t.Fatalf("selected ineligible provider %q (blocked=%v errors=%v)", best, routing.isBlocked(model, best), apiErrors[best])
			}
		}
		if manual {
			if autoSelected {
				t.Fatal("automatic selection reported an update under a manual pin")
			}
			if provider, isManual := routing.pinInfo(model); provider != "a" || !isManual {
				t.Fatalf("manual pin changed to %q manual=%v", provider, isManual)
			}
		}
	})
}

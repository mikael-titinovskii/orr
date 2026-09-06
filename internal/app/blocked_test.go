package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"gopkg.in/yaml.v3"
)

// guardrailBody is the response OpenRouter returns when a request names one
// endpoint that the account's guardrails exclude, captured verbatim from the
// live API so the parser is tested against the real shape.
const guardrailBody = `{"error":{"message":"0 endpoints out of 1 requested are available matching your guardrail restrictions and data policy. We removed them for the following reasons (an endpoint may have matched multiple reasons):\nZDR violation (guardrail): 1 endpoint excluded; configurable at https://openrouter.ai/workspaces/w/guardrails","code":404,"metadata":{"input_endpoint_count":1,"ineligibility_reasons":[{"reason":"zdr-violation-by-guardrail","endpoint_count":1,"configure_url":"https://openrouter.ai/workspaces/w/guardrails"}],"routing_funnel":[{"step":"Initial Endpoints","endpoint_count":30},{"step":"Filter by Allowed Providers","endpoint_count":1}],"failed_routing_step":"Filter by Guardrails"}}}`

// rateLimitBody is a transient upstream failure. It must not be mistaken for a
// policy refusal: the provider is reachable and will answer later.
const rateLimitBody = `{"error":{"message":"Provider returned error","code":429,"metadata":{"raw":"temporarily rate-limited upstream","provider_name":"Reka"}}}`

const providerUnavailableBody = `{"error":{"message":"No allowed providers are available for the selected model. Providers serving author/model: healthy, but your request's provider.only preference permits only: stale.","code":404}}`

func TestParseProviderRateLimitRequiresProviderAttribution(t *testing.T) {
	if !parseProviderRateLimit([]byte(rateLimitBody)) {
		t.Fatal("provider-attributed 429 was not recognized")
	}
	for name, body := range map[string]string{
		"global limit": `{"error":{"message":"Too many requests","code":429}}`,
		"other error":  `{"error":{"message":"Provider returned error","code":502,"metadata":{"provider_name":"Reka"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if parseProviderRateLimit([]byte(body)) {
				t.Fatal("response was incorrectly attributed as a provider 429")
			}
		})
	}
}

func TestParseProviderUnavailableRequiresCanonical404(t *testing.T) {
	for name, body := range map[string]string{
		"json": providerUnavailableBody,
		"sse":  "data: " + providerUnavailableBody + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			if !parseProviderUnavailable([]byte(body)) {
				t.Fatal("provider-unavailable response was not recognized")
			}
		})
	}
	for name, body := range map[string]string{
		"wrong status": `{"error":{"message":"No allowed providers are available for the selected model.","code":429}}`,
		"generic 404":  `{"error":{"message":"Model not found","code":404}}`,
		"guardrail":    guardrailBody,
	} {
		t.Run(name, func(t *testing.T) {
			if parseProviderUnavailable([]byte(body)) {
				t.Fatal("response was incorrectly classified as a disappeared provider")
			}
		})
	}
}

func TestParseBlockedReasonReadsGuardrailRefusal(t *testing.T) {
	refusal, blocked := parseBlockedReason([]byte(guardrailBody))
	if !blocked {
		t.Fatal("guardrail refusal not recognized")
	}
	if refusal.Reason != "zdr-violation-by-guardrail" {
		t.Fatalf("reason = %q", refusal.Reason)
	}
	if refusal.RequestedEndpoints != 1 {
		t.Fatalf("requested endpoints = %d, want 1", refusal.RequestedEndpoints)
	}
}

func TestParseBlockedReasonKeepsEveryReason(t *testing.T) {
	body := `{"error":{"code":404,"metadata":{"input_endpoint_count":1,"ineligibility_reasons":[
		{"reason":"zdr-violation-by-guardrail"},
		{"reason":"paid-model-training-violation-by-account"},
		{"reason":"paid-model-training-violation-by-guardrail"}]}}}`
	refusal, blocked := parseBlockedReason([]byte(body))
	if !blocked {
		t.Fatal("multi-reason refusal not recognized")
	}
	// Each reason names a different setting to change, so none may be dropped.
	want := "paid-model-training-violation-by-account, paid-model-training-violation-by-guardrail, zdr-violation-by-guardrail"
	if refusal.Reason != want {
		t.Fatalf("reason = %q, want %q", refusal.Reason, want)
	}
}

func TestParseBlockedReasonIgnoresTransientFailures(t *testing.T) {
	for name, body := range map[string]string{
		"rate limit":  rateLimitBody,
		"empty":       "",
		"not json":    "upstream timeout",
		"no metadata": `{"error":{"message":"boom","code":502}}`,
		"success":     `{"id":"gen-1","choices":[{"message":{"content":"hi"}}]}`,
	} {
		if _, blocked := parseBlockedReason([]byte(body)); blocked {
			t.Fatalf("%s was read as a policy refusal", name)
		}
	}
}

func TestParseBlockedReasonReadsStreamedRefusal(t *testing.T) {
	body := ": OPENROUTER PROCESSING\n\ndata: " + guardrailBody + "\n\ndata: [DONE]\n"
	refusal, blocked := parseBlockedReason([]byte(body))
	if !blocked || refusal.Reason != "zdr-violation-by-guardrail" {
		t.Fatalf("streamed refusal not recognized: %+v %v", refusal, blocked)
	}
}

func TestParseBlockedReasonAcceptsUnitemizedGuardrailStep(t *testing.T) {
	body := `{"error":{"code":404,"metadata":{"input_endpoint_count":1,"failed_routing_step":"Filter by Guardrails"}}}`
	refusal, blocked := parseBlockedReason([]byte(body))
	if !blocked || refusal.Reason != "guardrail" {
		t.Fatalf("unitemized refusal not recognized: %+v %v", refusal, blocked)
	}
}

func TestParseBlockedReasonRejectsRequestSpecificIneligibility(t *testing.T) {
	body := `{"error":{"code":404,"metadata":{"input_endpoint_count":1,"ineligibility_reasons":[{"reason":"unsupported-parameter"}],"failed_routing_step":"Filter by Supported Parameters"}}}`
	if refusal, blocked := parseBlockedReason([]byte(body)); blocked {
		t.Fatalf("request-specific incompatibility was persisted as an account block: %+v", refusal)
	}
}

func TestShortBlockReasonNamesTheSettingToChange(t *testing.T) {
	cases := map[string]string{
		"zdr-violation-by-guardrail":                                           "zdr",
		"paid-model-training-violation-by-account":                             "training",
		"paid-model-training-violation-by-account, zdr-violation-by-guardrail": "zdr+training",
		"something-else": "something-else",
		"":               "policy",
	}
	for reason, want := range cases {
		if got := shortBlockReason(reason); got != want {
			t.Fatalf("shortBlockReason(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestBlockedSetExpiresStaleRefusals(t *testing.T) {
	now := time.Now()
	cfg := providerConfig{Blocked: []blockedProvider{
		{Provider: "fresh", Reason: "zdr-violation-by-guardrail", DetectedAt: now.Add(-time.Hour)},
		{Provider: "stale", Reason: "zdr-violation-by-guardrail", DetectedAt: now.Add(-2 * blockedProviderTTL)},
		{Provider: "pinned-by-hand", Reason: "zdr-violation-by-guardrail"},
	}}
	set := blockedSet(cfg, now)
	if _, ok := set["fresh"]; !ok {
		t.Fatal("recent refusal dropped")
	}
	if _, ok := set["stale"]; ok {
		t.Fatal("expired refusal still blocking; the provider would never be re-probed")
	}
	// A hand-written entry has no timestamp and blocks indefinitely.
	if _, ok := set["pinned-by-hand"]; !ok {
		t.Fatal("timestampless refusal dropped")
	}
}

func TestBlockedProviderTTLBoundary(t *testing.T) {
	detected := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.Local)
	block := blockedProvider{Provider: "provider", DetectedAt: detected}
	if !block.fresh(detected.Add(blockedProviderTTL - time.Nanosecond)) {
		t.Fatal("provider block expired before its 24-hour TTL")
	}
	if block.fresh(detected.Add(blockedProviderTTL)) {
		t.Fatal("provider block remained active at the 24-hour boundary")
	}
}

func TestPartitionBlockedKeepsRelativeOrder(t *testing.T) {
	blocked := map[string]blockedProvider{"b": {Provider: "b"}, "d": {Provider: "d"}}
	routable, unroutable := partitionBlocked([]string{"a", "b", "c", "d", "e"}, blocked)
	if !equalSlices(routable, []string{"a", "c", "e"}) {
		t.Fatalf("routable = %v", routable)
	}
	if !equalSlices(unroutable, []string{"b", "d"}) {
		t.Fatalf("unroutable = %v", unroutable)
	}
}

func TestPolicyRefusalsDoNotCountAsProviderHealthErrors(t *testing.T) {
	now := time.Now()
	records := []requestRecord{
		{Time: now.Add(-time.Minute), Model: "author/model", Provider: "recovered", Status: http.StatusNotFound, Err: true, Blocked: true},
		{Time: now, Model: "author/model", Provider: "recovered", Status: http.StatusOK},
	}
	rates := providerAPIErrorRates(records, "author/model", now)
	if rate := rates["recovered"]; rate != 0 {
		t.Fatalf("provider health error rate = %.2f, want 0 after excluding the policy refusal", rate)
	}
}

func TestMarkBlockedDemotesProviderAndPromotesAutoPin(t *testing.T) {
	const model = "author/model"
	path := filepath.Join(t.TempDir(), "providers.yaml")
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"blocked", "second", "third"},
	}}, time.Hour)
	routing.setProvidersPath(path)
	routing.autoPin(model, "blocked")

	discovered, err := routing.markBlocked(model, "blocked", "zdr-violation-by-guardrail")
	if err != nil {
		t.Fatal(err)
	}
	if !discovered {
		t.Fatal("first refusal was not reported as newly discovered")
	}

	cfg, _ := routing.modelConfig(model)
	if want := []string{"second", "third", "blocked"}; !equalSlices(cfg.Order, want) {
		t.Fatalf("order = %v, want %v — a blocked provider must not head the order", cfg.Order, want)
	}
	if got := routing.pinnedProvider(model); got != "second" {
		t.Fatalf("auto pin = %q, want the next ranked provider", got)
	}

	// Repeating the same refusal is not news and must not churn the file.
	discovered, err = routing.markBlocked(model, "blocked", "zdr-violation-by-guardrail")
	if err != nil || discovered {
		t.Fatalf("repeat refusal: discovered=%v err=%v", discovered, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved providersFile
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	blocked := saved.Models[model].Blocked
	if len(blocked) != 1 || blocked[0].Provider != "blocked" || blocked[0].Reason != "zdr-violation-by-guardrail" {
		t.Fatalf("refusal not persisted: %#v", blocked)
	}
	if blocked[0].DetectedAt.IsZero() {
		t.Fatal("refusal persisted without a timestamp, so it could never expire")
	}
}

func TestRenewedExpiredBlockIsDemotedAgain(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"blocked", "healthy"},
		Blocked: []blockedProvider{{
			Provider:   "blocked",
			Reason:     "zdr-violation-by-guardrail",
			DetectedAt: time.Now().Add(-blockedProviderTTL - time.Hour),
		}},
	}}, time.Hour)

	discovered, err := routing.markBlocked(model, "blocked", "zdr-violation-by-guardrail")
	if err != nil {
		t.Fatal(err)
	}
	if !discovered {
		t.Fatal("renewed refusal was not treated as a new effective block")
	}
	cfg, _ := routing.modelConfig(model)
	if want := []string{"healthy", "blocked"}; !equalSlices(cfg.Order, want) {
		t.Fatalf("order = %v, want %v after renewing the expired block", cfg.Order, want)
	}
	if len(cfg.Blocked) != 1 || !cfg.Blocked[0].fresh(time.Now()) {
		t.Fatalf("renewed block is not fresh: %#v", cfg.Blocked)
	}
}

func TestClearBlockedRestoresProviderAfterItAnswers(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"a", "b"}}}, time.Hour)
	if _, err := routing.markBlocked(model, "a", "zdr-violation-by-guardrail"); err != nil {
		t.Fatal(err)
	}
	if !routing.isBlocked(model, "a") {
		t.Fatal("provider not blocked")
	}
	if err := routing.clearBlocked(model, "a"); err != nil {
		t.Fatal(err)
	}
	if routing.isBlocked(model, "a") {
		t.Fatal("provider still blocked after answering")
	}
}

func TestClearAllBlockedReprobesEverythingAfterARefresh(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"a", "b"}}}, time.Hour)
	for _, provider := range []string{"a", "b"} {
		if _, err := routing.markBlocked(model, provider, "zdr-violation-by-guardrail"); err != nil {
			t.Fatal(err)
		}
	}
	if err := routing.clearAllBlocked(model); err != nil {
		t.Fatal(err)
	}
	if len(routing.blockedProviders(model)) != 0 {
		t.Fatal("refusals survived a manual refresh, so a loosened guardrail would never be noticed")
	}
}

// A manual pin is an instruction. Pinning to a provider known to be refused
// is allowed and honoured: the user asked for that provider, so requests go
// there and the refusal comes back, marked, rather than orr quietly answering
// a different question with a different provider.
func TestManualPinToBlockedProviderIsHonoured(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"blocked", "ok"}}}, time.Hour)
	routing.endpointCache[model] = []endpointMeta{{Tag: "blocked"}, {Tag: "ok"}}
	if _, err := routing.markBlocked(model, "blocked", "zdr-violation-by-guardrail"); err != nil {
		t.Fatal(err)
	}
	if err := routing.setManualPin(model, "blocked"); err != nil {
		t.Fatalf("manual pin to a blocked provider was refused: %v", err)
	}
	if got := routing.pinnedProvider(model); got != "blocked" {
		t.Fatalf("routed provider = %q, want the pin obeyed", got)
	}
	if provider, manual := routing.pinInfo(model); provider != "blocked" || !manual {
		t.Fatalf("pinInfo = (%q, %v), want the manual pin", provider, manual)
	}
}

// An automatic pin is orr's own choice, so a refusal moves it to the next
// provider in the ranked order.
func TestBlockedAutoPinPromotesRankedFallback(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"chosen", "other"}}}, time.Hour)
	routing.endpointCache[model] = []endpointMeta{{Tag: "chosen"}, {Tag: "other"}}
	routing.autoPin(model, "chosen")
	if _, err := routing.markBlocked(model, "chosen", "zdr-violation-by-guardrail"); err != nil {
		t.Fatal(err)
	}
	if got := routing.pinnedProvider(model); got != "other" {
		t.Fatalf("routed provider = %q, want the ranked fallback", got)
	}
}

func TestBlockedAutoPinIsDroppedWithoutARoutableFallback(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"chosen"}}}, time.Hour)
	routing.autoPin(model, "chosen")
	if _, err := routing.markBlocked(model, "chosen", "zdr-violation-by-guardrail"); err != nil {
		t.Fatal(err)
	}
	if got := routing.pinnedProvider(model); got != "" {
		t.Fatalf("routed provider = %q, want no pin when every provider is blocked", got)
	}
}

func TestAutoPinSkipsBlockedProvider(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"a"}}}, time.Hour)
	if _, err := routing.markBlocked(model, "a", "zdr-violation-by-guardrail"); err != nil {
		t.Fatal(err)
	}
	if routing.autoPin(model, "a") {
		t.Fatal("automatic pin landed on a blocked provider")
	}
}

// A provider blocked today can still hold real benchmark samples from before
// the guardrail changed. Those numbers are genuine, which is exactly why they
// are dangerous: ranking on them hands the pin to a provider no request can
// reach. This is the regression that put a 100%-refusing provider at the head
// of the order while the dashboard showed it as healthy.
func TestSelectionNeverPinsABlockedProviderOnStaleMeasurements(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"blocked-bargain", "runner-up"},
	}}, time.Hour)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "blocked-bargain", Pricing: testPricing(0.10, 0.25, 0.05)},
		{Tag: "runner-up", Pricing: testPricing(2.00, 8.00, 0.20)},
	}
	if _, err := routing.markBlocked(model, "blocked-bargain", "zdr-violation-by-guardrail"); err != nil {
		t.Fatal(err)
	}

	best, autoSelected, err := routing.applyProviderTestResultsAtRevision(model, []providerTestResult{
		// The blocked provider looks like the best deal available.
		benchmarkResult("blocked-bargain", 500, 100*time.Millisecond),
		benchmarkResult("runner-up", 50, 900*time.Millisecond),
	}, benchmarkReferenceProfile, routing.modelRevision(model), nil)
	if err != nil {
		t.Fatal(err)
	}
	if best != "runner-up" || !autoSelected {
		t.Fatalf("best = %q auto=%v, want runner-up", best, autoSelected)
	}
	cfg, _ := routing.modelConfig(model)
	if want := []string{"runner-up", "blocked-bargain"}; !equalSlices(cfg.Order, want) {
		t.Fatalf("order = %v, want %v", cfg.Order, want)
	}
	if got := routing.pinnedProvider(model); got != "runner-up" {
		t.Fatalf("pin = %q, want runner-up", got)
	}
}

// A guardrail that excludes many endpoints must not eat the provider budget:
// counting refusals against maxProviders is how a model ends up with a short
// list of mostly unreachable providers.
func TestRefreshModelOrderHoldsBlockedProvidersOutOfTheBudget(t *testing.T) {
	const model = "author/model"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := endpointList{}
		for i := 0; i < 6; i++ {
			response.Data.Endpoints = append(response.Data.Endpoints, modelEndpoint{
				Tag:                 fmt.Sprintf("p%d", i),
				ProviderName:        fmt.Sprintf("P%d", i),
				SupportedParameters: []string{"tools", "tool_choice"},
				Throughput:          &percentiles{P50: float64(100 - i)},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	routing := newRoutingState(map[string]providerConfig{model: {}}, time.Hour)
	routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
	// The two fastest endpoints are refused for this account.
	for _, provider := range []string{"p0", "p1"} {
		if _, err := routing.markBlocked(model, provider, "zdr-violation-by-guardrail"); err != nil {
			t.Fatal(err)
		}
	}

	routing.refreshModelOrder(model, &http.Client{Timeout: 5 * time.Second}, server.URL, "", 2, false)

	cfg, _ := routing.modelConfig(model)
	// Two routable providers fill the budget; the refused ones trail it.
	if want := []string{"p2", "p3", "p0", "p1"}; !equalSlices(cfg.Order, want) {
		t.Fatalf("order = %v, want %v", cfg.Order, want)
	}
}

// With every endpoint refused there is nothing to hold out, and dropping them
// all would leave the dashboard with an empty table and no explanation.
func TestRefreshModelOrderKeepsTheRankingWhenEverythingIsBlocked(t *testing.T) {
	const model = "author/model"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := endpointList{}
		response.Data.Endpoints = []modelEndpoint{
			{Tag: "a", ProviderName: "A", SupportedParameters: []string{"tools", "tool_choice"}, Throughput: &percentiles{P50: 10}},
			{Tag: "b", ProviderName: "B", SupportedParameters: []string{"tools", "tool_choice"}, Throughput: &percentiles{P50: 5}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	routing := newRoutingState(map[string]providerConfig{model: {}}, time.Hour)
	routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
	for _, provider := range []string{"a", "b"} {
		if _, err := routing.markBlocked(model, provider, "zdr-violation-by-guardrail"); err != nil {
			t.Fatal(err)
		}
	}

	routing.refreshModelOrder(model, &http.Client{Timeout: 5 * time.Second}, server.URL, "", 20, false)

	cfg, _ := routing.modelConfig(model)
	if want := []string{"a", "b"}; !equalSlices(cfg.Order, want) {
		t.Fatalf("order = %v, want %v", cfg.Order, want)
	}
}

// The dashboard must not show a provider the account cannot reach as a live
// service with data. Its measured columns hold real successes from before the
// refusal, and leaving them on screen is what made a permanently refused
// provider read as healthy.
func TestDashboardShowsBlockedProviderAsBroken(t *testing.T) {
	const model = "deepseek/deepseek-v4-flash-0731"
	stats := newStats()
	now := time.Now()
	// Traffic from before the guardrail changed: this provider really was
	// fast, and 100 completion tokens in one second is 100 measured tok/s.
	for i := 0; i < 3; i++ {
		stats.record(requestRecord{
			Time: now.Add(-time.Duration(i) * time.Second), Model: model, Provider: "alibaba",
			Status: 200, Duration: 1500 * time.Millisecond, TTFT: 500 * time.Millisecond,
			PromptTokens: 100, CompletionTokens: 100,
		})
	}
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"alibaba", "siliconflow/fp8"},
	}}, time.Hour)
	routing.setCurrentModel(model)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "alibaba", Pricing: testPricing(0.35, 1.05, 0.03), Throughput: 84, Latency: 1702},
		{Tag: "siliconflow/fp8", Pricing: testPricing(0.22, 0.66, 0.03), Throughput: 69, Latency: 1752},
	}

	dashboard := newDashboard(config{}, stats, routing)
	before := routingPanel(t, dashboard)
	beforeRow := panelRow(t, before, "alibaba")
	if strings.Contains(beforeRow, "⊘") {
		t.Fatalf("unblocked provider already marked: %q", beforeRow)
	}
	if !strings.Contains(beforeRow, "100") {
		t.Fatalf("expected the measured throughput before the block: %q", beforeRow)
	}

	if _, err := routing.markBlocked(model, "alibaba", "zdr-violation-by-guardrail"); err != nil {
		t.Fatal(err)
	}

	after := routingPanel(t, dashboard)
	afterRow := panelRow(t, ansi.Strip(after), "alibaba")
	if !strings.Contains(afterRow, "⊘") || !strings.Contains(afterRow, "zdr") {
		t.Fatalf("blocked row does not read as broken: %q", afterRow)
	}
	// Nothing can be measured through a refusal, so the throughput it managed
	// beforehand must be gone rather than left standing as evidence of health.
	if strings.Contains(afterRow, "100") {
		t.Fatalf("blocked row still reports measured throughput: %q", afterRow)
	}
	// The row is tinted, not merely annotated, so it reads as broken at a glance.
	prefix, _ := stylePrefixSuffix(errorStyle)
	for _, line := range strings.Split(after, "\n") {
		if strings.Contains(ansi.Strip(line), "alibaba") && !strings.Contains(line, prefix) {
			t.Fatalf("blocked row is not tinted: %q", line)
		}
	}
}

// routingPanel renders just the Routing panel, so assertions are not confused
// by the other panels sharing each line in the three-column layout.
func routingPanel(t *testing.T, dashboard dashboardModel) string {
	t.Helper()
	updated, _ := dashboard.Update(tea.WindowSizeMsg{Width: 190, Height: 60})
	// A tick is what adopts the routing state's current model.
	updated, _ = updated.(dashboardModel).Update(tickMsg(time.Now()))
	model := updated.(dashboardModel)
	return model.renderRouting(model.currentSnap(), 76, 34)
}

// panelRow returns a provider's compact block (label plus metric lines),
// excluding the panel's own summary line.
func panelRow(t *testing.T, panel, provider string) string {
	t.Helper()
	lines := strings.Split(ansi.Strip(panel), "\n")
	for i, line := range lines {
		if strings.Contains(line, provider) && !strings.Contains(line, "blocked by") {
			return strings.Join(lines[i:min(i+3, len(lines))], "\n")
		}
	}
	t.Fatalf("no row for %q in:\n%s", provider, ansi.Strip(panel))
	return ""
}

// A benchmark is the reliable place to learn about a refusal: it pins exactly
// one provider with fallbacks off, so a policy 404 names that provider and
// nothing else. The refusal must be recorded and the remaining rounds skipped.
func TestProviderTestRecordsRefusalAndStopsRetrying(t *testing.T) {
	const model = "moonshotai/kimi-k2.5"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-or-v1-test"
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
	rounds := 0
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rounds++
		return response(http.StatusNotFound, guardrailBody), nil
	})

	if _, err := p.testProvider(model, "fireworks", providerTestRounds); err == nil {
		t.Fatal("a refused provider reported a usable sample")
	}
	// Retrying a policy refusal cannot change the answer.
	if rounds != 1 {
		t.Fatalf("rounds = %d, want 1", rounds)
	}
	if !p.routing.isBlocked(model, "fireworks") {
		t.Fatal("refusal was not recorded against the provider")
	}
	entry := p.routing.blockedProviders(model)["fireworks"]
	if entry.Reason != "zdr-violation-by-guardrail" {
		t.Fatalf("recorded reason = %q", entry.Reason)
	}

	// The request record has to carry the distinction too, so the log shows a
	// policy refusal rather than an unexplained failure.
	records := p.stats.snapshot().Records
	if len(records) == 0 {
		t.Fatal("no request recorded")
	}
	last := records[len(records)-1]
	if !last.Blocked || last.BlockedReason != "zdr-violation-by-guardrail" || !last.apiError() {
		t.Fatalf("record = %+v, want a blocked api error", last)
	}
}

// Once a provider is known to be refused, spending benchmark rounds on it only
// buys the same refusal — and worse, those refusals would satisfy the 70%
// completion threshold in place of real measurements.
func TestBenchmarkSkipsBlockedProviders(t *testing.T) {
	const model = "moonshotai/kimi-k2.5"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-or-v1-test"
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
	if _, err := p.routing.markBlocked(model, "blocked", "zdr-violation-by-guardrail"); err != nil {
		t.Fatal(err)
	}

	var tested []string
	var mu sync.Mutex
	p.dailyTestProvider = func(_ context.Context, _, provider string, _ int) (providerTestSample, error) {
		mu.Lock()
		tested = append(tested, provider)
		mu.Unlock()
		return pingSample(100, 200*time.Millisecond), nil
	}

	results, complete := p.benchmarkProviders(model, []string{"blocked", "ok"}, time.Now().Add(5*time.Second), false, 2)
	if !complete {
		t.Fatal("benchmark did not complete")
	}
	if len(tested) != 1 || tested[0] != "ok" {
		t.Fatalf("tested = %v, want only the routable provider", tested)
	}
	if len(results) != 1 || results[0].provider != "ok" {
		t.Fatalf("results = %+v, want only the routable provider", results)
	}
}

// With nothing routable left there is no point skipping: probing the refused
// providers is how the block gets re-tested once its TTL lapses.
func TestBenchmarkStillProbesWhenEveryProviderIsBlocked(t *testing.T) {
	const model = "moonshotai/kimi-k2.5"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-or-v1-test"
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
	if _, err := p.routing.markBlocked(model, "only", "zdr-violation-by-guardrail"); err != nil {
		t.Fatal(err)
	}
	p.dailyTestProvider = func(_ context.Context, _, provider string, _ int) (providerTestSample, error) {
		return pingSample(100, 200*time.Millisecond), nil
	}

	results, _ := p.benchmarkProviders(model, []string{"only"}, time.Now().Add(5*time.Second), false, 1)
	if len(results) != 1 {
		t.Fatalf("results = %+v, want the blocked provider probed anyway", results)
	}
}

// A provider that answers again must lose its block without a restart, which
// is what makes a loosened guardrail take effect on its own.
func TestSuccessfulRoundClearsRecordedRefusal(t *testing.T) {
	const model = "moonshotai/kimi-k2.5"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-or-v1-test"
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
	if _, err := p.routing.markBlocked(model, "fireworks", "zdr-violation-by-guardrail"); err != nil {
		t.Fatal(err)
	}
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusOK, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"completion_tokens\":8}}\n\ndata: [DONE]\n"), nil
	})

	if _, err := p.testProvider(model, "fireworks", 1); err != nil {
		t.Fatal(err)
	}
	if p.routing.isBlocked(model, "fireworks") {
		t.Fatal("refusal survived a successful round")
	}
}

// When orr chose the provider itself, discovering it is blocked must not cost
// the client a failed request: the refusal is final for that provider and says
// nothing about the others, so the request is re-routed rather than forwarded.
func TestClientRequestReroutesAcrossConsecutiveBlockedProviders(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"blocked-one", "blocked-two", "healthy"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
	p.routing.endpointCache[model] = []endpointMeta{{Tag: "blocked-one"}, {Tag: "blocked-two"}, {Tag: "healthy"}}
	// An automatic pin makes the request name exactly one endpoint, which is
	// what lets the refusal be attributed to that provider — and it is orr's
	// own choice, so it may be reconsidered.
	p.routing.autoPin(model, "blocked-one")
	p.stats.setAutoPin(model, "blocked-one")

	var attempts []string
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var payload struct {
			Provider struct {
				Only  []string `json:"only"`
				Order []string `json:"order"`
			} `json:"provider"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		// A pinned request names one endpoint; an unpinned one sends the
		// ranked order and OpenRouter takes the head of it.
		target := ""
		if len(payload.Provider.Only) > 0 {
			target = payload.Provider.Only[0]
		} else if len(payload.Provider.Order) > 0 {
			target = payload.Provider.Order[0]
		}
		attempts = append(attempts, target)
		if strings.HasPrefix(target, "blocked-") {
			return response(http.StatusNotFound, guardrailBody), nil
		}
		return response(http.StatusOK, `{"id":"ok","provider":"healthy"}`), nil
	})

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions",
		strings.NewReader(`{"model":"author/model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("client saw status %d, want the re-routed success:\n%s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"id":"ok"`) {
		t.Fatalf("client did not get the re-routed response: %s", res.Body.String())
	}
	if want := []string{"blocked-one", "blocked-two", "healthy"}; !equalSlices(attempts, want) {
		t.Fatalf("attempts = %v, want consecutive refusals followed by a re-route", attempts)
	}
	if !p.routing.isBlocked(model, "blocked-one") || !p.routing.isBlocked(model, "blocked-two") {
		t.Fatal("both refusals were not recorded")
	}
	if provider, manual := p.routing.pinInfo(model); provider != "healthy" || manual {
		t.Fatalf("replacement pin = %q manual=%v, want healthy auto", provider, manual)
	}
	if persisted := p.stats.providerPinsSnapshot()[model]; persisted.Provider != "healthy" || persisted.PinnedAt.IsZero() {
		t.Fatalf("persisted replacement pin = %#v, want healthy", persisted)
	}
	// The client sent one request and received one answer, so the log holds
	// the re-routed success and nothing else: recording the refused attempt
	// too would show an error the client never saw. The refusal is still
	// visible where it is acted on — the refusal list itself.
	records := p.stats.snapshot().Records
	if len(records) != 1 || records[0].Status != http.StatusOK || records[0].Blocked {
		t.Fatalf("records = %+v, want exactly one record for the re-routed success", records)
	}
}

// A failure that is not a policy refusal must be forwarded as-is: retrying it
// would spend the client's time to receive the same answer.
func TestClientRequestDoesNotRetryOrdinaryFailures(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {Order: []string{"a", "b"}, UpdatedAt: time.Now()}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
	attempts := 0
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		return response(http.StatusTooManyRequests, rateLimitBody), nil
	})

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions",
		strings.NewReader(`{"model":"author/model","messages":[]}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)

	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream failure forwarded", res.Code)
	}
	if !strings.Contains(res.Body.String(), "rate-limited") {
		t.Fatalf("error body was not forwarded: %s", res.Body.String())
	}
	if p.routing.isBlocked(model, "a") {
		t.Fatal("a rate limit was recorded as a policy block")
	}
}

// The benchmark is what discovers refusals, so discoveries land while it is
// still running. They must not invalidate the run — a guardrail change is
// exactly when several arrive at once, and throwing away the measurements
// would leave the order unranked for another cycle.
func TestRefusalDiscoveredMidBenchmarkKeepsTheMeasurements(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"doomed", "cheap", "pricey"},
	}}, time.Hour)
	routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "doomed", Pricing: testPricing(0.10, 0.25, 0.01)},
		{Tag: "cheap", Pricing: testPricing(1.00, 4.00, 0.10)},
		{Tag: "pricey", Pricing: testPricing(2.00, 8.00, 0.20)},
	}
	revision := routing.modelRevision(model)

	// The run measured every provider; "doomed" looked like the best deal and
	// only then came back refused.
	results := []providerTestResult{
		benchmarkResult("doomed", 400, 100*time.Millisecond),
		benchmarkResult("cheap", 200, 300*time.Millisecond),
		benchmarkResult("pricey", 100, 400*time.Millisecond),
	}
	if _, err := routing.markBlocked(model, "doomed", "zdr-violation-by-guardrail"); err != nil {
		t.Fatal(err)
	}

	best, autoSelected, err := routing.applyProviderTestResultsAtRevision(model, results, benchmarkReferenceProfile, revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	if best != "cheap" || !autoSelected {
		t.Fatalf("best = %q auto=%v, want the measurements applied to cheap", best, autoSelected)
	}
	cfg, _ := routing.modelConfig(model)
	if want := []string{"cheap", "pricey", "doomed"}; !equalSlices(cfg.Order, want) {
		t.Fatalf("order = %v, want %v", cfg.Order, want)
	}
	if got := routing.pinnedProvider(model); got != "cheap" {
		t.Fatalf("pin = %q, want cheap", got)
	}
	// The refusal must survive the order rewrite.
	if !routing.isBlocked(model, "doomed") {
		t.Fatal("refusal was lost when the benchmark order was written")
	}
}

// A manually pinned provider is never routed around, even once it is known to
// be refused. The user named that provider; substituting another one would
// answer a question they did not ask and hide the refusal they were looking at.
func TestClientRequestNeverRoutesAroundAManualPin(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"chosen", "healthy"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
	p.routing.endpointCache[model] = []endpointMeta{{Tag: "chosen"}, {Tag: "healthy"}}
	if err := p.routing.setManualPin(model, "chosen"); err != nil {
		t.Fatal(err)
	}

	var attempts []string
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var payload struct {
			Provider struct {
				Only  []string `json:"only"`
				Order []string `json:"order"`
			} `json:"provider"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		target := ""
		if len(payload.Provider.Only) > 0 {
			target = payload.Provider.Only[0]
		} else if len(payload.Provider.Order) > 0 {
			target = payload.Provider.Order[0]
		}
		attempts = append(attempts, target)
		if target == "chosen" {
			return response(http.StatusNotFound, guardrailBody), nil
		}
		return response(http.StatusOK, `{"id":"substituted"}`), nil
	})

	send := func() *memoryResponseWriter {
		req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions",
			strings.NewReader(`{"model":"author/model","messages":[]}`))
		res := newMemoryResponseWriter()
		p.ServeHTTP(res, req)
		return res
	}

	res := send()
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want the refusal forwarded to the client", res.Code)
	}
	if strings.Contains(res.Body.String(), "substituted") {
		t.Fatalf("a manual pin was routed around: %s", res.Body.String())
	}
	// And it stays that way once the block is on record: the pin still rules.
	if !p.routing.isBlocked(model, "chosen") {
		t.Fatal("refusal was not recorded")
	}
	res = send()
	if res.Code != http.StatusNotFound {
		t.Fatalf("second status = %d, want the pin still obeyed", res.Code)
	}
	for _, target := range attempts {
		if target != "chosen" {
			t.Fatalf("attempts = %v, want only the pinned provider", attempts)
		}
	}
}

// The sample pool holds successes only and keeps them for a week, so a
// provider that has since started refusing every request must not go on
// presenting last week's throughput as its current state. This is what made a
// provider that answers nothing but 404s still read as a live service with
// data in the dashboard.
func TestFreshFailureBeatsAStalePooledSuccess(t *testing.T) {
	now := time.Now()
	pool := map[string][]benchmarkSample{
		"answered":  {{Time: now.Add(-6 * 24 * time.Hour), TPS: 400, TTFTms: 200}},
		"cancelled": {{Time: now.Add(-6 * 24 * time.Hour), TPS: 300, TTFTms: 250}},
	}
	results := []providerTestResult{
		// The provider itself replied with an error.
		newProviderTestResult("answered", providerTestSample{}, fmt.Errorf("%w: provider returned HTTP 404", errProviderAnswered)),
		// orr ran out of time; this measured nothing about the provider.
		newProviderTestResult("cancelled", providerTestSample{}, context.DeadlineExceeded),
	}

	merged := mergeMeasuredProviderResults(results, pool, []string{"answered", "cancelled"}, now)
	byProvider := make(map[string]providerTestResult, len(merged))
	for _, result := range merged {
		byProvider[result.provider] = result
	}

	if got := byProvider["answered"]; got.err == nil || got.tps != 0 {
		t.Fatalf("answered = %+v, want the fresh failure kept, not week-old throughput", got)
	}
	// A cancelled test still must not throw away what the pool knows, or the
	// 70%-completion deadline would wipe every provider it did not reach.
	if got := byProvider["cancelled"]; got.err != nil || got.tps != 300 {
		t.Fatalf("cancelled = %+v, want the pooled measurement preserved", got)
	}
}

// And the consequence for selection: a provider currently answering with
// errors cannot hold the automatic pin on the strength of old samples.
func TestProviderAnsweringWithErrorsLosesThePin(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"failing", "working"},
	}}, time.Hour)
	routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "failing", Pricing: testPricing(0.10, 0.25, 0.01)},
		{Tag: "working", Pricing: testPricing(1.00, 4.00, 0.10)},
	}
	routing.autoPin(model, "failing")

	now := time.Now()
	pool := map[string][]benchmarkSample{
		"failing": {{Time: now.Add(-time.Hour), TPS: 500, TTFTms: 100}},
		"working": {{Time: now.Add(-time.Hour), TPS: 100, TTFTms: 400}},
	}
	results := mergeMeasuredProviderResults([]providerTestResult{
		newProviderTestResult("failing", providerTestSample{}, fmt.Errorf("%w: provider returned HTTP 500", errProviderAnswered)),
		benchmarkResult("working", 100, 400*time.Millisecond),
	}, pool, []string{"failing", "working"}, now)

	best, autoSelected, err := routing.applyProviderTestResultsAtRevision(model, results, benchmarkReferenceProfile, routing.modelRevision(model), nil)
	if err != nil {
		t.Fatal(err)
	}
	if best != "working" || !autoSelected {
		t.Fatalf("best = %q auto=%v, want working", best, autoSelected)
	}
	if got := routing.pinnedProvider(model); got != "working" {
		t.Fatalf("pin = %q, want it moved off the failing provider", got)
	}
}

// A refusal must apply even when it cannot be written to disk: dropping it
// would send the very next request back to a provider already known to refuse
// it. Only the persistence is lost, and the error is reported.
func TestBlockAppliesEvenWhenItCannotBePersisted(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"a", "b"}}}, time.Hour)
	// A directory where a file must go makes the atomic write fail.
	unwritable := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.Mkdir(unwritable, 0o700); err != nil {
		t.Fatal(err)
	}
	routing.setProvidersPath(unwritable)
	routing.autoPin(model, "a")

	discovered, err := routing.markBlocked(model, "a", "zdr-violation-by-guardrail")
	if err == nil {
		t.Fatal("a failed persist was not reported")
	}
	if !discovered {
		t.Fatal("the block was discarded because it could not be written")
	}
	if !routing.isBlocked(model, "a") {
		t.Fatal("the block is not in force in memory")
	}
	if got := routing.pinnedProvider(model); got != "b" {
		t.Fatalf("auto pin = %q, want the in-memory fallback despite the write failure", got)
	}
}

// A refusal discovered against one provider must not cancel a startup-restored
// pin still waiting to resolve to a different provider.
func TestMarkBlockedKeepsPendingPinForAnotherProvider(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"a"}}}, time.Hour)
	routing.restorePins(map[string]persistedPin{model: {Provider: "b", PinnedAt: time.Now()}})
	if _, ok := routing.pendingPins[model]; !ok {
		t.Fatal("pin did not stay pending before endpoints were known")
	}

	if _, err := routing.markBlocked(model, "a", "zdr-violation-by-guardrail"); err != nil {
		t.Fatal(err)
	}
	pending, ok := routing.pendingPins[model]
	if !ok || pending.Provider != "b" {
		t.Fatalf("pending pin = %+v ok=%v, want it untouched by an unrelated block", pending, ok)
	}
}

// A refusal against the provider a pending pin names cancels that pin:
// restoring it after endpoints arrive would pin requests to a refusal.
func TestMarkBlockedDropsPendingPinForTheBlockedProvider(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"a"}}}, time.Hour)
	routing.restorePins(map[string]persistedPin{model: {Provider: "b", PinnedAt: time.Now()}})
	if _, ok := routing.pendingPins[model]; !ok {
		t.Fatal("pin did not stay pending before endpoints were known")
	}

	if _, err := routing.markBlocked(model, "b", "zdr-violation-by-guardrail"); err != nil {
		t.Fatal(err)
	}
	if _, ok := routing.pendingPins[model]; ok {
		t.Fatal("pending pin on the blocked provider survived the block")
	}
}

// A persisted automatic pin on a provider the account cannot route to is never
// honored, so restoring it would only show a pin requests never use.
func TestRestorePinsDropsABlockedProvider(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order:   []string{"blocked", "healthy"},
		Blocked: []blockedProvider{{Provider: "blocked", Reason: "zdr-violation-by-guardrail", DetectedAt: time.Now()}},
	}}, time.Hour)

	routing.restorePins(map[string]persistedPin{model: {Provider: "blocked", PinnedAt: time.Now()}})
	if _, ok := routing.pins[model]; ok {
		t.Fatal("a blocked auto pin was restored")
	}
	if _, ok := routing.pendingPins[model]; ok {
		t.Fatal("a blocked auto pin was left pending")
	}
}

// A provider answering an ordinary request cannot still be refused, so the
// answer retires the block: a loosened guardrail takes effect on the next
// success instead of waiting for the block to expire.
func TestSuccessfulRequestClearsABlock(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:          []string{"healthy"},
		AllowFallbacks: boolPtr(false),
		UpdatedAt:      time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
	p.routing.endpointCache[model] = []endpointMeta{{Tag: "healthy"}}
	if _, err := p.routing.markBlocked(model, "healthy", "zdr-violation-by-guardrail"); err != nil {
		t.Fatal(err)
	}

	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"id":"ok","provider":"healthy"}`), nil
	})
	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions",
		strings.NewReader(`{"model":"author/model","messages":[]}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("client saw status %d, want the success: %s", res.Code, res.Body.String())
	}
	if p.routing.isBlocked(model, "healthy") {
		t.Fatal("the block survived a successful request through the provider")
	}
}

// Automatic selection is strictly pinned before forwarding, so a refusal is
// attributable to that one endpoint and routing can try the next ranked pin.
func TestAutomaticPinRefusalsAreAttributedAndExhaustFallbacks(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"first", "second"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
	p.routing.endpointCache[model] = []endpointMeta{{Tag: "first"}, {Tag: "second"}}

	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusNotFound, guardrailBody), nil
	})
	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions",
		strings.NewReader(`{"model":"author/model","messages":[]}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("client saw status %d, want the forwarded refusal", res.Code)
	}
	if !p.routing.isBlocked(model, "first") || !p.routing.isBlocked(model, "second") {
		t.Fatal("strict automatic pin refusals did not block both attempted providers")
	}
}

func TestBlockedFirstOnlyProviderCreatesVisibleHealthyAutoPin(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Only: []string{"blocked", "healthy"},
		Blocked: []blockedProvider{{
			Provider: "blocked", Reason: "zdr-violation-by-guardrail", DetectedAt: time.Now(),
		}},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.endpointCache[model] = []endpointMeta{{Tag: "blocked"}, {Tag: "healthy"}}
	defer p.waitForDailyBenchmarks()
	routed := make(chan struct {
		only           []string
		allowFallbacks *bool
	}, 1)
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			catalog := endpointList{}
			catalog.Data.Endpoints = []modelEndpoint{
				{Tag: "blocked", ProviderName: "Blocked", Status: 0},
				{Tag: "healthy", ProviderName: "Healthy", Status: 0},
			}
			data, _ := json.Marshal(catalog)
			return response(http.StatusOK, string(data)), nil
		}
		var payload struct {
			Provider struct {
				Only           []string `json:"only"`
				AllowFallbacks *bool    `json:"allow_fallbacks"`
			} `json:"provider"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			return nil, err
		}
		routed <- struct {
			only           []string
			allowFallbacks *bool
		}{payload.Provider.Only, payload.Provider.AllowFallbacks}
		return response(http.StatusOK, `{"model":"author/model","provider":"healthy","usage":{}}`), nil
	})

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions",
		strings.NewReader(`{"model":"author/model","messages":[]}`))
	p.ServeHTTP(newMemoryResponseWriter(), req)
	got := <-routed
	if len(got.only) != 1 || got.only[0] != "healthy" || got.allowFallbacks == nil || *got.allowFallbacks {
		t.Fatalf("upstream routing = only %v allow_fallbacks %v, want strict healthy pin", got.only, got.allowFallbacks)
	}
	if provider, manual := p.routing.pinInfo(model); provider != "healthy" || manual {
		t.Fatalf("visible pin = %q manual=%v, want automatic healthy", provider, manual)
	}
	if persisted := p.stats.providerPinsSnapshot()[model]; persisted.Provider != "healthy" {
		t.Fatalf("healthy automatic pin was not persisted: %#v", persisted)
	}
}

func TestStreamedRefusalIsRecordedAndDoesNotClearBlock(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"blocked", "healthy"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
	p.routing.endpointCache[model] = []endpointMeta{{Tag: "blocked"}, {Tag: "healthy"}}
	p.routing.autoPin(model, "blocked")
	p.stats.setAutoPin(model, "blocked")
	stream := "data: " + guardrailBody + "\n\ndata: [DONE]\n"
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusOK, stream), nil
	})

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions",
		strings.NewReader(`{"model":"author/model","messages":[],"stream":true}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)

	if res.Code != http.StatusOK || res.Body.String() != stream {
		t.Fatalf("streamed response changed: status=%d body=%q", res.Code, res.Body.String())
	}
	if !p.routing.isBlocked(model, "blocked") {
		t.Fatal("streamed refusal was not recorded as a block")
	}
	if provider := p.routing.pinnedProvider(model); provider != "healthy" {
		t.Fatalf("auto pin = %q, want healthy after streamed refusal", provider)
	}
	last := p.stats.snapshot().Last
	if !last.Blocked || !last.Err || last.BlockedReason != "zdr-violation-by-guardrail" {
		t.Fatalf("streamed refusal record = %+v", last)
	}
}

func TestProviderBenchmarkDetectsStreamedRefusal(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusOK, "data: "+guardrailBody+"\n\ndata: [DONE]\n"), nil
	})

	if _, err := p.testProvider(model, "blocked", 1); err == nil {
		t.Fatal("streamed benchmark refusal was reported as a usable response")
	}
	if !p.routing.isBlocked(model, "blocked") {
		t.Fatal("streamed benchmark refusal was not recorded as a block")
	}
	last := p.stats.snapshot().Last
	if !last.Blocked || !last.Err {
		t.Fatalf("streamed benchmark record = %+v", last)
	}
}

// The drained error prefix is only a prefix: whatever the classification cap
// cut off still has to reach the client.
func TestOversizedErrorBodyIsForwardedInFull(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:          []string{"first"},
		AllowFallbacks: boolPtr(false),
		UpdatedAt:      time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.endpointCache[model] = []endpointMeta{{Tag: "first"}}

	body := `{"error":{"message":"` + strings.Repeat("x", 128<<10) + `"},"code":500}`
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusInternalServerError, body), nil
	})
	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions",
		strings.NewReader(`{"model":"author/model","messages":[]}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)

	if res.Code != http.StatusInternalServerError {
		t.Fatalf("client saw status %d, want the forwarded failure", res.Code)
	}
	if got := res.Body.String(); got != body {
		t.Fatalf("client received %d bytes, want the full %d-byte error body", len(got), len(body))
	}
}

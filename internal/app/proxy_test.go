package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

type timedChunksBody struct {
	now      *time.Time
	chunks   [][]byte
	advances []time.Duration
	index    int
}

func (b *timedChunksBody) Read(p []byte) (int, error) {
	if b.index < len(b.advances) {
		*b.now = b.now.Add(b.advances[b.index])
	}
	if b.index >= len(b.chunks) {
		return 0, io.EOF
	}
	n := copy(p, b.chunks[b.index])
	b.index++
	return n, nil
}

func (*timedChunksBody) Close() error { return nil }

func boolPtr(value bool) *bool { return &value }

// injectProvider is a test helper that runs prepareRequest against a fresh
// routing state built from models.
func injectProvider(body io.Reader, models map[string]providerConfig) ([]byte, error) {
	modified, _, _, _, _, _, _, err := prepareRequest(body, newRoutingState(models, time.Hour), "/v1/chat/completions")
	return modified, err
}

func routingOnly(body map[string]any) []string {
	provider, _ := body["provider"].(map[string]any)
	raw, _ := provider["only"].([]any)
	result := make([]string, 0, len(raw))
	for _, value := range raw {
		if provider, ok := value.(string); ok {
			result = append(result, provider)
		}
	}
	return result
}

func testConfig(upstream string) config {
	return config{
		Listen:                     "127.0.0.1:8787",
		Upstream:                   upstream,
		RateLimitFailoverThreshold: defaultRateLimitFailoverThreshold,
		Models: map[string]providerConfig{
			"moonshotai/kimi-k2.5": {
				Order:          []string{"fireworks"},
				AllowFallbacks: boolPtr(false),
				UpdatedAt:      time.Now(),
			},
		},
	}
}

func TestCompletionInjectsProviderAndPreservesRequest(t *testing.T) {
	var received map[string]any
	var auth string

	p, err := newProxy(testConfig("https://upstream.test/api/v1"))
	if err != nil {
		t.Fatal(err)
	}
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/v1/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		return response(http.StatusOK, `{"id":"ok"}`), nil
	})
	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"moonshotai/kimi-k2.5","messages":[],"provider":{"order":["other"]}}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	if auth != "Bearer sk-test" {
		t.Fatalf("authorization was not forwarded: %q", auth)
	}
	if received["model"] != "moonshotai/kimi-k2.5" {
		t.Fatalf("model changed: %#v", received["model"])
	}
	provider := received["provider"].(map[string]any)
	only := provider["only"].([]any)
	if len(only) != 1 || only[0] != "fireworks" || provider["allow_fallbacks"] != false {
		t.Fatalf("unexpected provider: %#v", provider)
	}
	if received["usage"].(map[string]any)["include"] != true {
		t.Fatalf("usage was not injected: %#v", received)
	}
}

func TestPrepareRequestRejectsJSONNull(t *testing.T) {
	routing := newRoutingState(map[string]providerConfig{}, time.Hour)
	if _, _, _, _, _, _, _, err := prepareRequest(strings.NewReader(`null`), routing, "/v1/chat/completions"); err == nil {
		t.Fatal("JSON null request accepted")
	}
}

func TestProxyRejectsJSONNullWithBadRequest(t *testing.T) {
	p, err := newProxy(testConfig("https://upstream.test/api/v1"))
	if err != nil {
		t.Fatal(err)
	}
	called := false
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		return response(http.StatusOK, `{}`), nil
	})
	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`null`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest || called {
		t.Fatalf("null request status=%d upstreamCalled=%v", res.Code, called)
	}
}

func TestModelsPassesThroughUnchanged(t *testing.T) {
	p, _ := newProxy(testConfig("https://upstream.test/api/v1"))
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/v1/models" || r.URL.Query().Get("supported_parameters") != "tools" {
			t.Fatalf("unexpected target: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		return response(http.StatusOK, `{"data":[{"id":"model-a"}]}`), nil
	})
	req, _ := http.NewRequest(http.MethodGet, "http://proxy.test/v1/models?supported_parameters=tools", nil)
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)

	if got := strings.TrimSpace(res.Body.String()); got != `{"data":[{"id":"model-a"}]}` {
		t.Fatalf("response changed: %s", got)
	}
}

type memoryResponseWriter struct {
	header http.Header
	Body   strings.Builder
	Code   int
}

func newMemoryResponseWriter() *memoryResponseWriter {
	return &memoryResponseWriter{header: make(http.Header), Code: http.StatusOK}
}

func (w *memoryResponseWriter) Header() http.Header { return w.header }

func (w *memoryResponseWriter) Write(data []byte) (int, error) {
	return w.Body.Write(data)
}

func (w *memoryResponseWriter) WriteHeader(status int) { w.Code = status }

func (w *memoryResponseWriter) Flush() {}

func TestPrepareRequestCapturesReasoningEffort(t *testing.T) {
	routing := newRoutingState(map[string]providerConfig{}, time.Hour)
	body, _, _, _, effort, _, _, err := prepareRequest(strings.NewReader(`{"model":"m","reasoning":{"effort":"xhigh"}}`), routing, "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if effort != "xhigh" {
		t.Fatalf("reasoning effort = %q, want xhigh", effort)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["reasoning"].(map[string]any)["effort"] != "xhigh" {
		t.Fatalf("request body lost reasoning effort: %s", body)
	}
	if _, _, _, _, effort, _, _, err := prepareRequest(strings.NewReader(`{"model":"m"}`), routing, "/v1/chat/completions"); err != nil || effort != "" {
		t.Fatalf("missing reasoning effort: effort = %q, err = %v", effort, err)
	}
}

func TestPrepareRequestCapturesTopLevelReasoningEffort(t *testing.T) {
	routing := newRoutingState(map[string]providerConfig{}, time.Hour)
	_, _, _, _, effort, _, _, err := prepareRequest(strings.NewReader(`{"model":"m","reasoning_effort":"high"}`), routing, "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if effort != "high" {
		t.Fatalf("reasoning effort = %q, want high", effort)
	}
}

func TestMessagesAndResponsesAreMutable(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			body, err := injectProvider(strings.NewReader(`{"model":"moonshotai/kimi-k2.5"}`), testConfig("http://example.test").Models)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), `"fireworks"`) {
				t.Fatalf("provider not injected: %s", body)
			}
			if !mutablePaths[path] {
				t.Fatalf("path is not mutable: %s", path)
			}
		})
	}
}

func TestUnconfiguredModelPreservesProviderAndInjectsUsage(t *testing.T) {
	original := `{"model":"other/model","messages":[],"provider":{"order":["client-choice"]}}`
	body, err := injectProvider(strings.NewReader(original), testConfig("http://example.test").Models)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	provider := payload["provider"].(map[string]any)
	if provider["order"].([]any)[0] != "client-choice" {
		t.Fatalf("client provider changed: %s", body)
	}
	if payload["usage"].(map[string]any)["include"] != true {
		t.Fatalf("usage was not injected: %s", body)
	}
}

func TestUnknownModelIsPersistedWithoutChangingFirstRequest(t *testing.T) {
	models := map[string]providerConfig{"known/model": {Order: []string{"fireworks"}}}
	routing, db := testRoutingWithStore(t, models)
	original := `{"model":"new/model","provider":{"order":["client-choice"]}}`
	body, _, _, _, _, _, _, err := prepareRequest(strings.NewReader(original), routing, "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	provider := payload["provider"].(map[string]any)
	if provider["order"].([]any)[0] != "client-choice" {
		t.Fatalf("first request provider changed: %s", body)
	}
	saved, err := db.Models()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved["new/model"]; !ok {
		t.Fatalf("unknown model was not persisted: %#v", saved)
	}
}

func TestEmptyProviderEntryPreservesClientRouting(t *testing.T) {
	models := map[string]providerConfig{"new/model": {}}
	original := `{"model":"new/model","provider":{"order":["client-choice"]}}`
	body, err := injectProvider(strings.NewReader(original), models)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["provider"].(map[string]any)["order"].([]any)[0] != "client-choice" {
		t.Fatalf("empty entry changed client routing: %s", body)
	}
}

func TestProviderPinStrictlyAppliesToNextRequest(t *testing.T) {
	cfg := testConfig("http://example.test")
	routing := newRoutingState(cfg.Models, time.Hour)
	routing.pin("moonshotai/kimi-k2.5", "deepinfra")
	body, model, provider, _, _, _, _, err := prepareRequest(strings.NewReader(`{"model":"moonshotai/kimi-k2.5"}`), routing, "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if model != "moonshotai/kimi-k2.5" || provider != "deepinfra" {
		t.Fatalf("model/provider = %q/%q", model, provider)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	route := payload["provider"].(map[string]any)
	only := route["only"].([]any)
	if len(only) != 1 || only[0] != "deepinfra" || route["allow_fallbacks"] != false {
		t.Fatalf("strict pin missing: %s", body)
	}
	if _, exists := route["order"]; exists {
		t.Fatalf("pinned request retained provider order: %s", body)
	}
}

func TestProviderPinAppliesToDynamicallyAddedModel(t *testing.T) {
	routing := newRoutingState(map[string]providerConfig{}, time.Hour)
	const model = "google/gemini-3.7-flash"
	routing.pin(model, "google-ai-studio/flex")
	body, _, provider, _, _, _, _, err := prepareRequest(strings.NewReader(`{"model":"google/gemini-3.7-flash"}`), routing, "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if provider != "google-ai-studio/flex" {
		t.Fatalf("provider = %q, want google-ai-studio/flex", provider)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	route, ok := payload["provider"].(map[string]any)
	if !ok {
		t.Fatalf("provider object not injected into request: %s", body)
	}
	only := route["only"].([]any)
	if len(only) != 1 || only[0] != "google-ai-studio/flex" || route["allow_fallbacks"] != false {
		t.Fatalf("unexpected provider routing: %s", body)
	}
}

func TestProxyCreatesPersistsAndUsesRankedHeadAutoPin(t *testing.T) {
	cfg := testConfig("https://upstream.test/api/v1")
	providerCfg := cfg.Models["moonshotai/kimi-k2.5"]
	providerCfg.Order = []string{"fireworks", "together"}
	cfg.Models["moonshotai/kimi-k2.5"] = providerCfg
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		route := payload["provider"].(map[string]any)
		only := route["only"].([]any)
		if len(only) != 1 || only[0] != "fireworks" || route["allow_fallbacks"] != false {
			t.Fatalf("first request did not strictly use its automatic pin: %#v", route)
		}
		return response(http.StatusOK, `{"model":"moonshotai/kimi-k2.5","provider":"together","usage":{"prompt_tokens":10,"completion_tokens":5}}`), nil
	})
	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"moonshotai/kimi-k2.5"}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)

	provider, manual := p.routing.pinInfo("moonshotai/kimi-k2.5")
	if provider != "fireworks" || manual {
		t.Fatalf("request created pin = %q manual=%v, want automatic fireworks", provider, manual)
	}
	if got := p.stats.snapshot().Last.Provider; got != "together" {
		t.Fatalf("recorded provider = %q, want response provider", got)
	}
	if persisted := p.stats.providerPinsSnapshot()["moonshotai/kimi-k2.5"]; persisted.Provider != "fireworks" {
		t.Fatalf("automatic pin was not persisted: %#v", persisted)
	}
	dashboard := newDashboard(cfg, p.stats, p.routing)
	dashboard.syncModel()
	view := ansi.Strip(dashboard.renderRouting(p.stats.snapshot(), 80, 20))
	if !strings.Contains(view, "Pin    fireworks (auto") {
		t.Fatalf("proxy's automatic route is not visible as a pin:\n%s", view)
	}
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		route := payload["provider"].(map[string]any)
		only := route["only"].([]any)
		if len(only) != 1 || only[0] != "fireworks" || route["allow_fallbacks"] != false {
			t.Fatalf("subsequent request did not keep the automatic pin: %#v", route)
		}
		return response(http.StatusOK, `{"model":"moonshotai/kimi-k2.5","provider":"together","usage":{}}`), nil
	})
	second, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"moonshotai/kimi-k2.5"}`))
	p.ServeHTTP(newMemoryResponseWriter(), second)
	if provider := p.routing.pinnedProvider("moonshotai/kimi-k2.5"); provider != "fireworks" {
		t.Fatalf("response metadata changed automatic pin to %q", provider)
	}
}

func TestAutoModeReelectsCurrentRankedHeadAfterPinTTL(t *testing.T) {
	const model = "author/model"
	current := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.Local)
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.PinTTL = time.Hour
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"first", "second"},
		UpdatedAt: current,
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.setClock(func() time.Time { return current })
	var routed []string
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		only := routingOnly(body)
		if len(only) != 1 {
			t.Fatalf("route = %v, want one strict provider", body["provider"])
		}
		routed = append(routed, only[0])
		return response(http.StatusOK, fmt.Sprintf(`{"model":%q,"provider":%q}`, model, only[0])), nil
	})

	send := func() {
		req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions",
			strings.NewReader(`{"model":"author/model","messages":[]}`))
		if _, err := p.forward(newMemoryResponseWriter(), req); err != nil {
			t.Fatal(err)
		}
	}
	send()
	if provider, manual := p.routing.pinInfo(model); provider != "first" || manual {
		t.Fatalf("initial auto pin = %q manual=%v", provider, manual)
	}

	// Discovery may refresh the ranked order while the sticky auto-pin is
	// alive. Once its TTL passes, the next request must elect the new head.
	p.routing.mu.Lock()
	modelConfig := p.routing.models[model]
	modelConfig.Order = []string{"second", "first"}
	p.routing.models[model] = modelConfig
	p.routing.mu.Unlock()
	current = current.Add(time.Hour + time.Nanosecond)
	send()
	if want := []string{"first", "second"}; !equalSlices(routed, want) {
		t.Fatalf("routes across TTL = %v, want %v", routed, want)
	}
	if provider, manual := p.routing.pinInfo(model); provider != "second" || manual {
		t.Fatalf("post-TTL auto pin = %q manual=%v, want second", provider, manual)
	}
	if persisted := p.stats.providerPinsSnapshot()[model]; persisted.Provider != "second" || !persisted.PinnedAt.Equal(current) {
		t.Fatalf("post-TTL persisted pin = %#v", persisted)
	}
}

func TestProxyUnresolvedResponseDoesNotChangeAutomaticPin(t *testing.T) {
	cfg := testConfig("https://upstream.test/api/v1")
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"model":"moonshotai/kimi-k2.5","provider":"Unknown Provider"}`), nil
	})
	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"moonshotai/kimi-k2.5"}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)

	if provider := p.routing.pinnedProvider("moonshotai/kimi-k2.5"); provider != "fireworks" {
		t.Fatalf("unresolved response changed automatic pin to %q", provider)
	}
	if got := p.stats.snapshot().Last.Provider; got != "Unknown Provider" {
		t.Fatalf("recorded provider = %q, want display name", got)
	}
}

func TestProxyAutoPinsBeforeResponseProviderMetadata(t *testing.T) {
	cfg := testConfig("https://upstream.test/api/v1")
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"model":"moonshotai/kimi-k2.5","usage":{}}`), nil
	})
	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"moonshotai/kimi-k2.5"}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)

	if provider := p.routing.pinnedProvider("moonshotai/kimi-k2.5"); provider != "fireworks" {
		t.Fatalf("request without response metadata has automatic pin %q", provider)
	}
	if got := p.stats.snapshot().Last.Provider; got != "fireworks" {
		t.Fatalf("recorded provider = %q, want selected provider", got)
	}
}

func TestProxyAutoPinsBeforeUpstreamError(t *testing.T) {
	cfg := testConfig("https://upstream.test/api/v1")
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusInternalServerError, `{}`), nil
	})
	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"moonshotai/kimi-k2.5"}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)

	provider, _ := p.routing.pinInfo("moonshotai/kimi-k2.5")
	if provider != "fireworks" {
		t.Fatalf("upstream error request has automatic pin %q", provider)
	}
}

func TestExtractResponseSummaryFromSSE(t *testing.T) {
	stream := "data: {\"model\":\"moonshotai/kimi-k2.5\",\"provider\":\"fireworks\"}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"cost\":0.0012}}\n\n" +
		"data: [DONE]\n"
	got := extractResponseSummary([]byte(stream))
	if got.model != "moonshotai/kimi-k2.5" || got.provider != "fireworks" || got.promptTokens != 100 || got.completionTokens != 50 {
		t.Fatalf("unexpected summary: %#v", got)
	}
	if !got.hasCost || got.cost != 0.0012 {
		t.Fatalf("cost not extracted: %#v", got)
	}
}

func TestExtractResponseSummaryFromTruncatedJSONTail(t *testing.T) {
	tail := []byte(`content that began before the tap buffer","usage":{"prompt_tokens":12,"completion_tokens":7,"cost":"0.42"},"provider":"deepinfra"}`)
	got := extractResponseSummary(tail)
	if got.promptTokens != 12 || got.completionTokens != 7 || got.cost != .42 || got.provider != "deepinfra" {
		t.Fatalf("unexpected summary: %#v", got)
	}
}

func TestExtractResponseSummaryIncludesCacheTokens(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":800,"cache_write_tokens":0}}}`)
	got := extractResponseSummary(body)
	if got.promptTokens != 1000 || got.completionTokens != 50 {
		t.Fatalf("token counts wrong: %#v", got)
	}
	if got.cachedTokens != 800 || got.cacheWriteTokens != 0 {
		t.Fatalf("cache tokens wrong: %#v", got)
	}
}

func TestExtractResponseSummaryIncludesReasoningTokens(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":500,"completion_tokens_details":{"reasoning_tokens":300}}}`)
	got := extractResponseSummary(body)
	if got.promptTokens != 1000 || got.completionTokens != 500 {
		t.Fatalf("token counts wrong: %#v", got)
	}
	if got.reasoningTokens != 300 {
		t.Fatalf("reasoning tokens wrong: %#v", got)
	}
}

func TestExtractResponseSummaryFallsBackToOutputTokensDetails(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":500,"output_tokens":25,"output_tokens_details":{"reasoning_tokens":10}}}`)
	got := extractResponseSummary(body)
	if got.promptTokens != 500 || got.completionTokens != 25 {
		t.Fatalf("token counts wrong: %#v", got)
	}
	if got.reasoningTokens != 10 {
		t.Fatalf("reasoning tokens wrong: %#v", got)
	}
}

func TestExtractResponseSummaryFallsBackToInputTokensDetails(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":500,"output_tokens":25,"input_tokens_details":{"cached_tokens":400,"cache_write_tokens":100}}}`)
	got := extractResponseSummary(body)
	if got.promptTokens != 500 || got.completionTokens != 25 {
		t.Fatalf("token counts wrong: %#v", got)
	}
	if got.cachedTokens != 400 || got.cacheWriteTokens != 100 {
		t.Fatalf("cache tokens wrong: %#v", got)
	}
}

func TestFormatRecordOmitsPathAndShowsCache(t *testing.T) {
	record := requestRecord{
		Time:         time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC),
		Model:        "a/b",
		Provider:     "fireworks",
		Status:       200,
		Duration:     1234 * time.Millisecond,
		PromptTokens: 1000,
		CachedTokens: 800,
		Cost:         0.001234,
	}
	got := formatRecord(record)
	if strings.Contains(got, "/v1/chat/completions") {
		t.Fatalf("log should not contain path: %s", got)
	}
	if !strings.Contains(got, "cache 800 (80%)") {
		t.Fatalf("log should show cache info: %s", got)
	}
}

func TestFormatRecordShowsInputOutputTokens(t *testing.T) {
	record := requestRecord{
		Time:             time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC),
		Model:            "a/b",
		Provider:         "fireworks",
		Status:           200,
		Duration:         1234 * time.Millisecond,
		PromptTokens:     50_000,
		CompletionTokens: 1500,
		Cost:             0.001234,
	}
	got := formatRecord(record)
	if !strings.Contains(got, "in 50000 out 1500") {
		t.Fatalf("log should show in/out tokens as plain numbers: %s", got)
	}
}

func TestFormatRecordShowsCacheWrite(t *testing.T) {
	record := requestRecord{
		Time:             time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC),
		Model:            "a/b",
		Provider:         "fireworks",
		Status:           200,
		Duration:         1234 * time.Millisecond,
		PromptTokens:     1000,
		CacheWriteTokens: 1000,
		Cost:             0.001234,
	}
	got := formatRecord(record)
	if !strings.Contains(got, "cache write 1000") {
		t.Fatalf("log should show cache write as plain numbers: %s", got)
	}
}

func TestFormatRecordShowsCacheWriteAndReadPercentage(t *testing.T) {
	record := requestRecord{
		Time:             time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC),
		Model:            "a/b",
		Provider:         "fireworks",
		Status:           200,
		Duration:         1234 * time.Millisecond,
		PromptTokens:     1000,
		CachedTokens:     800,
		CacheWriteTokens: 100,
	}
	got := formatRecord(record)
	if !strings.Contains(got, "cache write 100 read 800 (80%)") {
		t.Fatalf("log should show cache-write and cached percentage: %s", got)
	}
}

func TestFormatRecordFormatsCacheTokensAsPlainNumbers(t *testing.T) {
	record := requestRecord{
		Time:         time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC),
		Model:        "a/b",
		Provider:     "fireworks",
		Status:       200,
		Duration:     1234 * time.Millisecond,
		PromptTokens: 1_000_000,
		CachedTokens: 2_000_000,
		Cost:         0.001234,
	}
	got := formatRecord(record)
	if !strings.Contains(got, "cache 2000000 (200%)") {
		t.Fatalf("log should show tokens as plain numbers: %s", got)
	}
}

func TestTargetPathComposition(t *testing.T) {
	p, err := newProxy(testConfig("https://openrouter.ai/api/v1"))
	if err != nil {
		t.Fatal(err)
	}
	wanted, _ := url.Parse("https://openrouter.ai/api/v1/models")
	target := *p.upstream
	target.Path = strings.TrimRight(p.upstream.Path, "/") + strings.TrimPrefix("/v1/models", "/v1")
	if target.String() != wanted.String() {
		t.Fatalf("target = %s, want %s", target.String(), wanted.String())
	}
}

func TestProxyRecordsToolCallsFromRequestBody(t *testing.T) {
	cfg := testConfig("https://upstream.test/api/v1")
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"model":"moonshotai/kimi-k2.5","provider":"fireworks","usage":{"prompt_tokens":10,"completion_tokens":5}}`), nil
	})
	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"moonshotai/kimi-k2.5","tools":[{"type":"function","function":{"name":"a"}},{"type":"function","function":{"name":"b"}}]}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)

	if got := p.stats.snapshot().Last.ToolCalls; got != 2 {
		t.Fatalf("tool calls = %d, want 2", got)
	}
}

func TestProxyTestProviderMeasuresTPSAndLatency(t *testing.T) {
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stream := "data: {\"model\":\"moonshotai/kimi-k2.5\",\"provider\":\"fireworks\"}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"p\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"ong\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":3,\"cost\":0.0012}}\n\n" +
		"data: [DONE]\n"
	calls := 0
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/api/v1/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Fatalf("authorization = %q, want Bearer sk-test", r.Header.Get("Authorization"))
		}
		time.Sleep(2 * time.Millisecond)
		return response(http.StatusOK, stream), nil
	})
	sample, err := p.testProvider("moonshotai/kimi-k2.5", "fireworks", 2)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("rounds = %d, want 2", calls)
	}
	if sample.tps <= 0 {
		t.Fatalf("tps = %v, want > 0", sample.tps)
	}
	if sample.latency <= 0 {
		t.Fatalf("latency = %v, want > 0", sample.latency)
	}
	if sample.ttft <= 0 {
		t.Fatalf("ttft = %v, want > 0", sample.ttft)
	}
	if sample.samples != 2 {
		t.Fatalf("samples = %d, want 2", sample.samples)
	}
	// Test rounds are billed requests: they must be recorded into stats, while
	// staying out of the client request counters.
	snap := p.stats.snapshot()
	if len(snap.Records) != 2 || snap.RequestCount != 0 {
		t.Fatalf("stats records = %d request count = %d, want 2 recorded rounds and no client requests", len(snap.Records), snap.RequestCount)
	}
	for _, record := range snap.Records {
		if record.Model != "moonshotai/kimi-k2.5" || record.Provider != "fireworks" {
			t.Fatalf("recorded %q/%q, want moonshotai/kimi-k2.5/fireworks", record.Model, record.Provider)
		}
		if record.Status != http.StatusOK || record.Err {
			t.Fatalf("recorded status = %d err = %v", record.Status, record.Err)
		}
		if record.CompletionTokens != 3 || record.Cost != 0.0012 {
			t.Fatalf("recorded tokens/cost = %d/$%.4f, want 3/$0.0012", record.CompletionTokens, record.Cost)
		}
	}
}

func TestProxyMeasuresTTFTDurationAndThroughputAtStreamBoundaries(t *testing.T) {
	const model = "author/model"
	current := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.Local)
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"preferred"},
		UpdatedAt: current,
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.setClock(func() time.Time { return current })
	p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: &timedChunksBody{
				now: &current,
				chunks: [][]byte{
					[]byte("data: {\"model\":\"author/model\",\"provider\":\"preferred\"}\n\n"),
					[]byte("data: {\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":100,\"cost\":0.25}}\n\ndata: [DONE]\n\n"),
				},
				advances: []time.Duration{200 * time.Millisecond, 800 * time.Millisecond},
			},
		}, nil
	})
	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions",
		strings.NewReader(`{"model":"author/model","messages":[]}`))
	record, err := p.forward(newMemoryResponseWriter(), req)
	if err != nil {
		t.Fatal(err)
	}
	if record.TTFT != 200*time.Millisecond || record.Duration != time.Second {
		t.Fatalf("stream timing = ttft %v duration %v, want 200ms/1s", record.TTFT, record.Duration)
	}
	if record.PromptTokens != 20 || record.CompletionTokens != 100 || record.Cost != .25 {
		t.Fatalf("stream usage = %+v", record)
	}
	if got := record.tokensPerSecond(); got != 125 {
		t.Fatalf("stream tokens/s = %v, want 125", got)
	}
}

func TestProxyTestProviderRequiresAPIKey(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = ""
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.testProvider("moonshotai/kimi-k2.5", "fireworks", 1); err == nil {
		t.Fatal("provider test without an API key should fail gracefully")
	}
}

func TestProxyTestProviderRejectsHTTPError(t *testing.T) {
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusBadGateway, `{}`), nil
	})
	if _, err := p.testProvider("moonshotai/kimi-k2.5", "fireworks", 1); err == nil {
		t.Fatal("provider test should fail on an upstream HTTP error")
	}
}

func TestApplyProviderResultsReplacesIneligibleAutoPinWithRankedFallback(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"failing", "untested"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.autoPin(model, "failing")
	p.stats.setAutoPin(model, "failing")
	p.stats.record(requestRecord{
		Time: time.Now(), Model: model, Provider: "failing",
		Status: http.StatusBadGateway, Err: true,
	})

	p.applyProviderTestResults(model, []providerTestResult{
		benchmarkResult("failing", 100, 200*time.Millisecond),
	}, p.routing.modelRevision(model))

	if provider, manual := p.routing.pinInfo(model); provider != "untested" || manual {
		t.Fatalf("replacement pin = %q manual=%v, want automatic untested", provider, manual)
	}
	if persisted := p.stats.providerPinsSnapshot()[model]; persisted.Provider != "untested" {
		t.Fatalf("replacement automatic pin was not persisted: %#v", persisted)
	}
}

func TestDailyBenchmarkRunsWhenCatalogIsFreshButBenchmarkIsDue(t *testing.T) {
	const model = "moonshotai/kimi-k2.5"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		catalog := endpointList{}
		catalog.Data.Endpoints = []modelEndpoint{{
			Tag: "fireworks", ProviderName: "Fireworks", Status: 0,
			SupportedParameters: []string{"tools", "tool_choice"},
			Throughput:          &percentiles{P50: 100},
		}}
		data, _ := json.Marshal(catalog)
		return response(http.StatusOK, string(data)), nil
	})
	var calls atomic.Int32
	p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		calls.Add(1)
		return pingSample(100, 300*time.Millisecond), nil
	}

	ready := p.prepareDailyModel(model)
	if ready == nil {
		t.Fatal("fresh catalog suppressed a due daily benchmark")
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("daily benchmark did not finish")
	}
	p.waitForDailyBenchmarks()
	if got := calls.Load(); got != 1 {
		t.Fatalf("benchmark calls = %d, want one provider tested", got)
	}
	if provider := p.routing.pinnedProvider(model); provider != "fireworks" {
		t.Fatalf("measured provider was not pinned: %q", provider)
	}
	if !p.stats.benchmarkRanToday(model, time.Now()) {
		t.Fatal("completed daily benchmark was not recorded")
	}
	if ready := p.prepareDailyModel(model); ready != nil {
		t.Fatal("recorded daily benchmark ran twice")
	}
}

// A manual pin is the user's choice of provider, so there is nothing for a daily
// run to decide: no catalog read, no benchmark pings, no rewritten order. The
// pin is enforced after a run too, but spending provider requests on a decision
// that cannot change is the part this guards.
func TestManualPinSkipsDailyProviderCheckups(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"pinned", "other"},
		ManualPin: "pinned",
		UpdatedAt: time.Now().Add(-24 * time.Hour),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var catalogCalls atomic.Int32
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			catalogCalls.Add(1)
			return response(http.StatusOK, `{"data":{"endpoints":[]}}`), nil
		}
		return response(http.StatusOK, `{"id":"ok"}`), nil
	})
	var pings atomic.Int32
	p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		pings.Add(1)
		return pingSample(100, 100*time.Millisecond), nil
	}

	if ready := p.prepareDailyModel(model); ready != nil {
		t.Fatal("a manual pin started a daily provider checkup")
	}
	p.waitForDailyBenchmarks()
	if got := pings.Load(); got != 0 {
		t.Fatalf("benchmark pings = %d, want none under a manual pin", got)
	}
	if got := catalogCalls.Load(); got != 0 {
		t.Fatalf("catalog reads = %d, want none under a manual pin", got)
	}
	if provider, manual := p.routing.pinInfo(model); provider != "pinned" || !manual {
		t.Fatalf("pin = %q manual=%v, want the manual pin untouched", provider, manual)
	}

	// Removing the pin hands the decision back to orr, so the next request is a
	// measurement again.
	if err := p.routing.setManualPin(model, ""); err != nil {
		t.Fatal(err)
	}
	ready := p.prepareDailyModel(model)
	if ready == nil {
		t.Fatal("an unpinned model still skipped its daily run")
	}
	p.waitForDailyBenchmarks()
}

// A run that spent pings and still learned nothing is the same spend as a
// successful one, so it takes the full freshness window: retrying a slow or
// failing provider set a minute later would only spend more.
func TestFailedPingsConsumeTheFreshnessWindow(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.UpdateMaxProviders = 20
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"provider"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var pings atomic.Int32
	p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		pings.Add(1)
		return providerTestSample{}, errProviderAnswered
	}

	ready := p.prepareDailyModel(model)
	if ready == nil {
		t.Fatal("a due model did not start a run")
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("run never released the request gate")
	}
	p.waitForDailyBenchmarks()
	if got := pings.Load(); got == 0 {
		t.Fatal("the run never pinged a provider")
	}
	if !p.stats.benchmarkedWithin(model, p.now(), benchmarkFreshness) {
		t.Fatal("a run that spent pings did not consume the freshness window")
	}
	if p.stats.benchmarkAbortedWithin(model, p.now(), benchmarkRetryAfterAbort) {
		t.Fatal("a run that spent pings armed the short abort floor as well")
	}
	if again := p.prepareDailyModel(model); again != nil {
		p.waitForDailyBenchmarks()
		t.Fatal("the next request relaunched a run that had just spent pings")
	}
}

// A failing catalog must not be re-fetched by every request, and the model must
// not be relaunched before the refresh floor expires.
func TestFailedCatalogRefreshIsNotRetriedPerRequest(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.UpdateMaxProviders = 20
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"provider"},
		UpdatedAt: time.Now().Add(-24 * time.Hour),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var catalogCalls atomic.Int32
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			catalogCalls.Add(1)
			return response(http.StatusInternalServerError, "catalog down"), nil
		}
		return response(http.StatusOK, `{"id":"ok"}`), nil
	})
	var pings atomic.Int32
	p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		pings.Add(1)
		return pingSample(100, time.Millisecond), nil
	}

	ready := p.prepareDailyModel(model)
	if ready == nil {
		t.Fatal("a due run with a failing catalog was never attempted")
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("aborted run never released the request gate")
	}
	p.waitForDailyBenchmarks()
	if got := pings.Load(); got != 0 {
		t.Fatalf("benchmark pings = %d after the catalog failed, want none", got)
	}
	if p.stats.benchmarkedWithin(model, p.now(), benchmarkFreshness) {
		t.Fatal("a run that never pinged consumed the five-minute freshness window")
	}
	if !p.stats.benchmarkAbortedWithin(model, p.now(), benchmarkRetryAfterAbort) {
		t.Fatal("a run that never pinged did not arm the short abort floor")
	}
	for range 3 {
		if again := p.prepareDailyModel(model); again != nil {
			p.waitForDailyBenchmarks()
			t.Fatal("a subsequent request relaunched a run that could not measure")
		}
	}
	if got := catalogCalls.Load(); got != 1 {
		t.Fatalf("catalog reads = %d, want the failed refresh not retried per request", got)
	}

	// Past the floor the model is retried rather than hidden until the five-minute
	// window expires. The gate is time.Now-based while the injected clock is a
	// minute ahead, so it has to be wide enough not to expire.
	now := time.Now().Add(benchmarkRetryAfterAbort + time.Second)
	p.dailyGate = 2 * benchmarkRetryAfterAbort
	p.setClock(func() time.Time { return now })
	retried := p.prepareDailyModel(model)
	if retried == nil {
		t.Fatal("the failed run was not retried once the floor expired")
	}
	select {
	case <-retried:
	case <-time.After(2 * time.Second):
		t.Fatal("retry never released the request gate")
	}
	p.waitForDailyBenchmarks()
	if got := catalogCalls.Load(); got != 2 {
		t.Fatalf("catalog reads = %d, want one retry after the floor", got)
	}
}

// The recovery cooldown is persisted with the rest of the stats file: a restart
// must not hand the same failing provider a fresh benchmark allowance.
func TestProviderRecoveryCooldownSurvivesRestart(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"provider"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.client.Transport = degradationCatalogTransport("provider")
	p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		return pingSample(100, time.Millisecond), nil
	}
	ready := p.startProviderRecovery(model)
	if ready == nil {
		t.Fatal("recovery did not start a benchmark")
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery run never finished")
	}
	p.waitForDailyBenchmarks()
	if p.stats.recoveryLastRun(model).IsZero() {
		t.Fatal("recovery run did not consume its cooldown")
	}
	db := openTestStore(t)
	if err := p.stats.save(db); err != nil {
		t.Fatal(err)
	}

	restarted, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.stats.load(db); err != nil {
		t.Fatal(err)
	}
	if again := restarted.startProviderRecovery(model); again != nil {
		restarted.waitForDailyBenchmarks()
		t.Fatal("restart re-ran recovery inside the cooldown")
	}
	restarted.waitForDailyBenchmarks()
}

func TestConfiguredConsecutive429sFailOverAndRunFastRecovery(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.RateLimitFailoverThreshold = 3
	cfg.UpdateMaxProviders = 20
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"limited", "healthy"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the ordinary daily scheduler out of this test: the 429 threshold,
	// not a due daily run, must be what launches discovery.
	p.stats.markBenchmarkRun(model, time.Now())

	var catalogCalls atomic.Int32
	var attemptsMu sync.Mutex
	var attempts []string
	var benchmarked []string
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			catalogCalls.Add(1)
			catalog := endpointList{}
			catalog.Data.Endpoints = []modelEndpoint{
				{Tag: "limited", ProviderName: "Limited", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}},
				{Tag: "healthy", ProviderName: "Healthy", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}},
			}
			data, _ := json.Marshal(catalog)
			return response(http.StatusOK, string(data)), nil
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		provider := routingOnly(body)[0]
		attemptsMu.Lock()
		attempts = append(attempts, provider)
		attemptsMu.Unlock()
		if provider == "limited" {
			return response(http.StatusTooManyRequests, rateLimitBody), nil
		}
		return response(http.StatusOK, `{"id":"ok"}`), nil
	})
	p.dailyTestProvider = func(_ context.Context, _ string, provider string, _ int) (providerTestSample, error) {
		attemptsMu.Lock()
		benchmarked = append(benchmarked, provider)
		attemptsMu.Unlock()
		if provider == "limited" {
			return pingSample(1000, 100*time.Millisecond), nil
		}
		return pingSample(100, 300*time.Millisecond), nil
	}

	request := func() *memoryResponseWriter {
		req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"author/model","messages":[]}`))
		res := newMemoryResponseWriter()
		p.ServeHTTP(res, req)
		return res
	}
	if res := request(); res.Code != http.StatusTooManyRequests {
		t.Fatalf("first response = %d, want 429", res.Code)
	}
	if res := request(); res.Code != http.StatusTooManyRequests {
		t.Fatalf("second response = %d, want 429", res.Code)
	}
	if provider := p.routing.pinnedProvider(model); provider != "limited" {
		t.Fatalf("provider changed before configured threshold: %q", provider)
	}
	if got := catalogCalls.Load(); got != 0 {
		t.Fatalf("recovery started before threshold: %d catalog calls", got)
	}
	if res := request(); res.Code != http.StatusOK {
		t.Fatalf("threshold response = %d, want successful internal failover: %s", res.Code, res.Body.String())
	}
	if provider := p.routing.pinnedProvider(model); provider != "healthy" {
		t.Fatalf("threshold did not immediately fail over: %q", provider)
	}
	p.waitForDailyBenchmarks()
	if got := catalogCalls.Load(); got != 1 {
		t.Fatalf("recovery catalog calls = %d, want 1", got)
	}
	if order, _ := p.routing.modelConfig(model); len(order.Order) != 2 || order.Order[0] != "healthy" {
		t.Fatalf("recovered provider order = %v, want healthy first", order.Order)
	}
	attemptsMu.Lock()
	defer attemptsMu.Unlock()
	if want := []string{"limited", "limited", "limited", "healthy"}; !equalSlices(attempts, want) {
		t.Fatalf("client attempts = %v, want %v", attempts, want)
	}
	if want := []string{"healthy"}; !equalSlices(benchmarked, want) {
		t.Fatalf("recovery benchmarked = %v, want only %v", benchmarked, want)
	}
}

func TestOpenRouterWide429DoesNotFailOverAProvider(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.RateLimitFailoverThreshold = 1
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"first", "second"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// No background work is needed in this attribution test. If the global 429
	// is misclassified, the immediate pin change remains directly observable.
	p.stopDailyBenchmarks()
	p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusTooManyRequests, `{"error":{"message":"Too many requests","code":429}}`), nil
	})

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"author/model","messages":[]}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("response = %d, want 429", res.Code)
	}
	if provider := p.routing.pinnedProvider(model); provider != "first" {
		t.Fatalf("OpenRouter-wide 429 failed over to %q", provider)
	}
}

func TestUnavailableAutomaticProviderFailsOverImmediatelyAndRecovers(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	// A disappeared endpoint is definitive and must not consume the 429
	// allowance before moving on.
	cfg.RateLimitFailoverThreshold = 99
	cfg.UpdateMaxProviders = 20
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"stale", "healthy"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.stats.markBenchmarkRun(model, time.Now())

	var mu sync.Mutex
	var attempts, benchmarked []string
	var catalogCalls atomic.Int32
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			catalogCalls.Add(1)
			catalog := endpointList{}
			catalog.Data.Endpoints = []modelEndpoint{{
				Tag: "healthy", ProviderName: "Healthy", Status: 0,
				SupportedParameters: []string{"tools", "tool_choice"},
			}}
			data, _ := json.Marshal(catalog)
			return response(http.StatusOK, string(data)), nil
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		provider := routingOnly(body)[0]
		mu.Lock()
		attempts = append(attempts, provider)
		mu.Unlock()
		if provider == "stale" {
			return response(http.StatusNotFound, providerUnavailableBody), nil
		}
		return response(http.StatusOK, `{"id":"ok"}`), nil
	})
	p.dailyTestProvider = func(_ context.Context, _ string, provider string, _ int) (providerTestSample, error) {
		mu.Lock()
		benchmarked = append(benchmarked, provider)
		mu.Unlock()
		return pingSample(100, 100*time.Millisecond), nil
	}

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"author/model","messages":[]}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("response = %d, want successful internal failover: %s", res.Code, res.Body.String())
	}
	p.waitForDailyBenchmarks()
	if got := catalogCalls.Load(); got != 1 {
		t.Fatalf("recovery catalog calls = %d, want 1", got)
	}
	if provider := p.routing.pinnedProvider(model); provider != "healthy" {
		t.Fatalf("automatic pin = %q, want healthy", provider)
	}
	if persisted := p.stats.providerPinsSnapshot()[model]; persisted.Provider != "healthy" {
		t.Fatalf("persisted automatic pin = %q, want healthy", persisted.Provider)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"stale", "healthy"}; !equalSlices(attempts, want) {
		t.Fatalf("client attempts = %v, want %v", attempts, want)
	}
	if want := []string{"healthy"}; !equalSlices(benchmarked, want) {
		t.Fatalf("recovery benchmarked = %v, want %v", benchmarked, want)
	}
}

func TestUnavailableOnlyProviderReturns404AndClearsAutomaticPin(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"stale"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.stopDailyBenchmarks()
	p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusNotFound, providerUnavailableBody), nil
	})

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"author/model","messages":[]}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("response = %d, want original 404", res.Code)
	}
	if provider, manual := p.routing.pinInfo(model); provider != "" || manual {
		t.Fatalf("exhausted automatic pin = %q manual=%v, want none", provider, manual)
	}
	if persisted := p.stats.providerPinsSnapshot()[model]; persisted.Provider != "" {
		t.Fatalf("persisted exhausted pin = %q, want none", persisted.Provider)
	}
}

func TestUnavailableManualProviderRemainsPinned(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"stale", "healthy"},
		ManualPin: "stale",
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.stopDailyBenchmarks()
	var attempts int
	p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return response(http.StatusNotFound, providerUnavailableBody), nil
	})

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"author/model","messages":[]}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound || attempts != 1 {
		t.Fatalf("manual response = %d after %d attempts, want 404 after 1", res.Code, attempts)
	}
	if provider, manual := p.routing.pinInfo(model); provider != "stale" || !manual {
		t.Fatalf("manual pin changed: %q manual=%v", provider, manual)
	}
}

func encryptedContentBody(creator string) string {
	return `{"error":{"message":"Your request contains encrypted reasoning or compaction content that can only be processed by the provider that created it (` + creator + `), but your request's provider.only preference excludes that provider. Allow ` + creator + ` or start a new conversation without the encrypted items.","code":400}}`
}

func newEncryptedContentProxy(t *testing.T, models map[string]providerConfig, attempts *[]string) *proxy {
	t.Helper()
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = models
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.stopDailyBenchmarks()
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		provider := routingOnly(body)[0]
		*attempts = append(*attempts, provider)
		if provider != "azure/eu" {
			return response(http.StatusBadRequest, encryptedContentBody("azure/eu")), nil
		}
		return response(http.StatusOK, `{"id":"ok"}`), nil
	})
	return p
}

func TestEncryptedContentRetriesOnCreatingProviderWithoutMovingAutomaticPin(t *testing.T) {
	const model = "author/model"
	var attempts []string
	p := newEncryptedContentProxy(t, map[string]providerConfig{model: {
		Order:     []string{"bedrock", "azure/eu"},
		UpdatedAt: time.Now(),
	}}, &attempts)

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", strings.NewReader(`{"model":"author/model","input":[]}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("response = %d, want retried success: %s", res.Code, res.Body.String())
	}
	if want := []string{"bedrock", "azure/eu"}; !equalSlices(attempts, want) {
		t.Fatalf("attempts = %v, want %v", attempts, want)
	}
	if provider := p.routing.pinnedProvider(model); provider != "bedrock" {
		t.Fatalf("automatic pin = %q, want bedrock", provider)
	}
	if rates := providerAPIErrorRates(p.stats.snapshot().Records, model, time.Now()); rates["bedrock"] != 0 || rates["azure/eu"] != 0 {
		t.Fatalf("API error rates = %v, want no errors", rates)
	}
}

func TestEncryptedContentKeepsManualPinAndSparesItsErrorRate(t *testing.T) {
	const model = "author/model"
	var attempts []string
	p := newEncryptedContentProxy(t, map[string]providerConfig{model: {
		Order:     []string{"bedrock", "azure/eu"},
		ManualPin: "bedrock",
		UpdatedAt: time.Now(),
	}}, &attempts)

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", strings.NewReader(`{"model":"author/model","input":[]}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest || len(attempts) != 1 {
		t.Fatalf("manual response = %d after %v, want 400 after one attempt", res.Code, attempts)
	}
	if rates := providerAPIErrorRates(p.stats.snapshot().Records, model, time.Now()); rates["bedrock"] != 0 {
		t.Fatalf("bedrock API error rate = %v, want none", rates["bedrock"])
	}
}

func TestEncryptedContentKeepsClientRoutingWithoutPin(t *testing.T) {
	const model = "author/model"
	var attempts []string
	p := newEncryptedContentProxy(t, map[string]providerConfig{model: {UpdatedAt: time.Now()}}, &attempts)

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", strings.NewReader(`{"model":"author/model","input":[],"provider":{"only":["bedrock"]}}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest || !equalSlices(attempts, []string{"bedrock"}) {
		t.Fatalf("response = %d after %v, want 400 after one bedrock attempt", res.Code, attempts)
	}
}

func TestEncryptedContentRetryPreservesJSONNumbers(t *testing.T) {
	const model = "author/model"
	var attempts []string
	p := newEncryptedContentProxy(t, map[string]providerConfig{model: {
		Order:     []string{"bedrock", "azure/eu"},
		UpdatedAt: time.Now(),
	}}, &attempts)
	if !p.routing.ensureAutoPin(model, "bedrock") {
		t.Fatal("automatic pin was not created")
	}

	retry := p.encryptedContentRetry([]byte(`{"model":"author/model","tools":[{"enum":[9007199254740993]}]}`), model, "bedrock", "azure/eu")
	if !strings.Contains(string(retry), "9007199254740993") {
		t.Fatalf("retry body = %s, want the client's number unchanged", retry)
	}
}

// A continuation is orr's own second request towards the same answer, so it has
// to reach the endpoint that answered the first leg. The body orr prepared names
// the model's whole order — or, for an unpinned model, whatever the client sent —
// and a continuation re-routed along it would show the entire conversation to a
// second endpoint just to finish the first one's answer.
func TestLoopContinuationIsPinnedToTheAnsweringProvider(t *testing.T) {
	p, err := newProxy(testConfig("https://upstream.test/api/v1"))
	if err != nil {
		t.Fatal(err)
	}
	p.stopDailyBenchmarks()
	var sent map[string]any
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("decode continuation: %v", err)
		}
		return response(http.StatusInternalServerError, `{}`), nil
	})
	target := *p.upstream
	target.Path = "/api/v1/chat/completions"
	send := p.loopContinuation(context.Background(), "/v1/chat/completions", target, make(http.Header),
		[]byte(`{"model":"moonshotai/kimi-k2.5","messages":[{"role":"user","content":"hi"}],"stream":true,"provider":{"order":["fireworks","other"],"allow_fallbacks":true}}`),
		"fireworks")
	if _, err := send(&loopVerdict{Channel: loopChannelContent, Prefix: "half an answer"}); err == nil {
		t.Fatal("an unsuccessful continuation was reported as a spliceable stream")
	}
	if sent == nil {
		t.Fatal("continuation never reached the upstream")
	}
	if only := routingOnly(sent); !equalSlices(only, []string{"fireworks"}) {
		t.Fatalf("continuation provider.only = %v, want only fireworks", only)
	}
	provider, _ := sent["provider"].(map[string]any)
	if provider["allow_fallbacks"] != false {
		t.Fatalf("continuation allow_fallbacks = %v, want false", provider["allow_fallbacks"])
	}
	if _, ok := provider["order"]; ok {
		t.Fatalf("continuation kept the fallback order: %#v", provider)
	}
	// The conversation must still be continued, not restarted: pinning only
	// narrows where the request goes.
	messages, _ := sent["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("continuation messages = %d, want the client's turn plus the two continuation turns", len(messages))
	}
}

// An unknown model's first request chooses no endpoint: there is nothing to pin,
// so the continuation keeps the body it was prepared with.
func TestLoopContinuationWithoutProviderIsSentUnpinned(t *testing.T) {
	p, err := newProxy(testConfig("https://upstream.test/api/v1"))
	if err != nil {
		t.Fatal(err)
	}
	p.stopDailyBenchmarks()
	sent := map[string]any{}
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("decode continuation: %v", err)
		}
		return response(http.StatusInternalServerError, `{}`), nil
	})
	target := *p.upstream
	target.Path = "/api/v1/chat/completions"
	body := []byte(`{"model":"author/unknown","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	send := p.loopContinuation(context.Background(), "/v1/chat/completions", target, make(http.Header), body, "")
	if _, err := send(&loopVerdict{Channel: loopChannelContent, Prefix: "half an answer"}); err == nil {
		t.Fatal("an unsuccessful continuation was reported as a spliceable stream")
	}
	if _, ok := sent["provider"]; ok {
		t.Fatalf("unpinned continuation invented a provider block: %#v", sent["provider"])
	}
}

func TestGeneric404DoesNotFailOverAutomaticProvider(t *testing.T) {
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
	p.stopDailyBenchmarks()
	var attempts int
	p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return response(http.StatusNotFound, `{"error":{"message":"Model not found","code":404}}`), nil
	})

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"author/model","messages":[]}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound || attempts != 1 {
		t.Fatalf("generic 404 = %d after %d attempts, want 404 after 1", res.Code, attempts)
	}
	if provider := p.routing.pinnedProvider(model); provider != "first" {
		t.Fatalf("generic 404 moved automatic pin to %q", provider)
	}
}

func TestUnavailableFailoverSkips429ReplacementWithoutThresholdDelay(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.RateLimitFailoverThreshold = 3
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"stale", "limited", "healthy"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.stopDailyBenchmarks()
	var attempts []string
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		provider := routingOnly(body)[0]
		attempts = append(attempts, provider)
		switch provider {
		case "stale":
			return response(http.StatusNotFound, providerUnavailableBody), nil
		case "limited":
			return response(http.StatusTooManyRequests, rateLimitBody), nil
		default:
			return response(http.StatusOK, `{"id":"ok"}`), nil
		}
	})

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"author/model","messages":[]}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("response = %d, want successful mixed failover: %s", res.Code, res.Body.String())
	}
	if want := []string{"stale", "limited", "healthy"}; !equalSlices(attempts, want) {
		t.Fatalf("attempts = %v, want %v", attempts, want)
	}
}

func TestStreamingUnavailableProviderMovesNextRequest(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"stale", "healthy"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.stopDailyBenchmarks()
	stream := "data: {\"model\":\"author/model\",\"provider\":\"Stale Display Name\"}\n\n" +
		"data: " + providerUnavailableBody + "\n\n"
	p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, stream), nil
	})

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"author/model","messages":[],"stream":true}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("stream status = %d, want upstream status already sent", res.Code)
	}
	if provider := p.routing.pinnedProvider(model); provider != "healthy" {
		t.Fatalf("next-request pin = %q, want healthy", provider)
	}
}

func TestStreaming429UsesSelectedEndpointForNextRequestFailover(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.RateLimitFailoverThreshold = 1
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"first-tag", "second-tag"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.stopDailyBenchmarks()
	stream := "data: {\"model\":\"author/model\",\"provider\":\"Provider Display Name\"}\n\n" +
		"data: " + rateLimitBody + "\n\n"
	p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, stream), nil
	})

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"author/model","messages":[],"stream":true}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("stream status = %d, want 200 already sent by upstream", res.Code)
	}
	if provider := p.routing.pinnedProvider(model); provider != "second-tag" {
		t.Fatalf("streaming 429 failover pin = %q, want second-tag", provider)
	}
}

func TestThresholdRequestTriesEachRateLimitedFallbackOnce(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.RateLimitFailoverThreshold = 1
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"first", "second", "third"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.stopDailyBenchmarks()
	var attempts []string
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		provider := routingOnly(body)[0]
		attempts = append(attempts, provider)
		if provider != "third" {
			return response(http.StatusTooManyRequests, rateLimitBody), nil
		}
		return response(http.StatusOK, `{"id":"ok"}`), nil
	})

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"author/model","messages":[]}`))
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("response = %d, want fallback success: %s", res.Code, res.Body.String())
	}
	if want := []string{"first", "second", "third"}; !equalSlices(attempts, want) {
		t.Fatalf("attempts = %v, want %v", attempts, want)
	}
	if provider := p.routing.pinnedProvider(model); provider != "third" {
		t.Fatalf("final automatic pin = %q, want third", provider)
	}
}

func TestNon429ResponseResetsConsecutiveRateLimitCount(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.RateLimitFailoverThreshold = 2
	cfg.Models = map[string]providerConfig{model: {Order: []string{"first", "second"}}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.autoPin(model, "first")
	rateLimited := requestRecord{Model: model, Provider: "first", Status: http.StatusTooManyRequests, Err: true, RateLimited: true}
	p.observeProviderFailure(rateLimited)
	p.observeProviderFailure(requestRecord{Model: model, Provider: "first", Status: http.StatusBadGateway, Err: true})
	p.observeProviderFailure(rateLimited)
	if provider := p.routing.pinnedProvider(model); provider != "first" {
		t.Fatalf("non-429 response did not reset the streak; pin = %q", provider)
	}
}

func TestLate429CannotEraseReplacementProviderStreak(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.RateLimitFailoverThreshold = 2
	cfg.Models = map[string]providerConfig{model: {Order: []string{"first", "second", "third"}}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.autoPin(model, "first")
	if _, reached := p.rateLimitReplacement(model, "first", false); reached {
		t.Fatal("first provider reached threshold after one 429")
	}
	if replacement, reached := p.rateLimitReplacement(model, "first", false); !reached || replacement != "second" {
		t.Fatalf("first failover = %q, %v; want second, true", replacement, reached)
	}
	if _, reached := p.rateLimitReplacement(model, "second", false); reached {
		t.Fatal("second provider reached threshold after one 429")
	}
	// This response was already in flight when first failed over. It must not
	// erase second's independently accumulated strike.
	if _, reached := p.rateLimitReplacement(model, "first", false); reached {
		t.Fatal("late response from the old provider triggered another failover")
	}
	if replacement, reached := p.rateLimitReplacement(model, "second", false); !reached || replacement != "third" {
		t.Fatalf("second failover = %q, %v; want third, true", replacement, reached)
	}
}

func TestRateLimitFailoverClearsPersistedPinWithoutCandidate(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.RateLimitFailoverThreshold = 1
	cfg.Models = map[string]providerConfig{model: {Order: []string{"only"}}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.autoPin(model, "only")
	p.stats.setAutoPin(model, "only")

	if replacement, reached := p.rateLimitReplacement(model, "only", false); !reached || replacement != "" {
		t.Fatalf("rate-limit failover = %q, %v; want empty, true", replacement, reached)
	}
	if pins := p.stats.providerPinsSnapshot(); pins[model].Provider != "" {
		t.Fatalf("persisted pin after exhausted failover = %q, want empty", pins[model].Provider)
	}
}

func TestSuccessfulManualBenchmarkRehabilitatesRateLimitedProvider(t *testing.T) {
	const model = "author/model"
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {Order: []string{"limited", "healthy"}}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.setClock(func() time.Time { return now })
	p.routing.autoPin(model, "limited")
	if _, accepted := p.routing.markProviderUnavailableAndFailover(model, "limited", true); !accepted {
		t.Fatal("rate-limit failure was not accepted")
	}
	now = now.Add(time.Second)
	p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		return pingSample(100, 100*time.Millisecond), nil
	}

	results, complete := p.benchmarkProviders(model, []string{"limited"}, now.Add(time.Second), false, 1)
	if !complete || len(results) != 1 || results[0].err != nil {
		t.Fatalf("manual benchmark = %#v complete=%v, want one success", results, complete)
	}
	if p.routing.isTemporarilyUnavailable(model, "limited") {
		t.Fatal("successful manual benchmark did not rehabilitate provider")
	}
}

func TestBenchmarkStartedBeforeFailoverCannotRehabilitateProvider(t *testing.T) {
	const model = "author/model"
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {Order: []string{"limited", "healthy"}}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.setClock(func() time.Time { return now })
	p.routing.autoPin(model, "limited")
	entered := make(chan struct{})
	release := make(chan struct{})
	p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		close(entered)
		<-release
		return pingSample(100, 100*time.Millisecond), nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.benchmarkProviders(model, []string{"limited"}, time.Now().Add(time.Second), false, 1)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("benchmark did not start")
	}
	now = now.Add(time.Second)
	if _, accepted := p.routing.markProviderUnavailableAndFailover(model, "limited", true); !accepted {
		t.Fatal("rate-limit failure was not accepted")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("benchmark did not finish")
	}
	if !p.routing.isTemporarilyUnavailable(model, "limited") {
		t.Fatal("stale benchmark success rehabilitated provider")
	}
}

func TestRateLimitRecoveryCoalescesAndCoolsDownPerModel(t *testing.T) {
	const model = "author/model"
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.Local)
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.UpdateMaxProviders = 20
	cfg.Models = map[string]providerConfig{model: {Order: []string{"provider"}, UpdatedAt: now}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.setClock(func() time.Time { return now })

	entered := make(chan struct{})
	release := make(chan struct{})
	var catalogCalls atomic.Int32
	p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		if catalogCalls.Add(1) == 1 {
			close(entered)
		}
		<-release
		catalog := endpointList{}
		catalog.Data.Endpoints = []modelEndpoint{{
			Tag: "provider", ProviderName: "Provider", Status: 0,
			SupportedParameters: []string{"tools", "tool_choice"},
		}}
		data, _ := json.Marshal(catalog)
		return response(http.StatusOK, string(data)), nil
	})
	p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		return pingSample(100, 100*time.Millisecond), nil
	}

	first := p.startProviderRecovery(model)
	if first == nil {
		t.Fatal("first recovery did not start")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("recovery catalog refresh did not start")
	}
	if second := p.startProviderRecovery(model); second != first {
		t.Fatal("concurrent recovery was not coalesced with the in-flight run")
	}
	close(release)
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("coalesced recovery did not finish")
	}
	p.waitForDailyBenchmarks()
	if got := catalogCalls.Load(); got != 1 {
		t.Fatalf("coalesced catalog calls = %d, want 1", got)
	}
	if next := p.startProviderRecovery(model); next != nil {
		t.Fatal("recovery cooldown allowed an immediate repeat run")
	}
}

func TestRateLimitRecoveryCoalescedWithDailyRunStartsCooldown(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.Models = map[string]providerConfig{model: {Order: []string{"provider"}, UpdatedAt: time.Now()}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	benchmarkStarted := make(chan struct{})
	release := make(chan struct{})
	p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		close(benchmarkStarted)
		<-release
		return pingSample(100, 100*time.Millisecond), nil
	}
	refreshDone := make(chan struct{})
	close(refreshDone)
	ready := make(chan struct{})
	p.dailyMu.Lock()
	p.dailyReady[model] = ready
	p.dailyWG.Add(1)
	p.dailyMu.Unlock()
	go p.runDailyBenchmark(model, refreshDone, ready)
	select {
	case <-benchmarkStarted:
	case <-time.After(time.Second):
		t.Fatal("daily benchmark did not start")
	}
	if coalesced := p.startProviderRecovery(model); coalesced != ready {
		t.Fatal("rate-limit recovery did not coalesce with daily benchmark")
	}
	close(release)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("daily benchmark did not finish")
	}
	p.waitForDailyBenchmarks()
	if next := p.startProviderRecovery(model); next != nil {
		t.Fatal("coalesced daily run did not start the recovery cooldown")
	}
}

// A daily run ends at its deadline, not at a completion share: it waits for
// every provider the budget allows and cancels whoever is still running. The
// providers that answered are ranked; the ones that were canceled contribute
// nothing but their own missing row.
func TestDailyBenchmarkWaitsForTheDeadlineAndCancelsTheRest(t *testing.T) {
	const model = "author/model"
	providers := []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9"}
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.Models = map[string]providerConfig{model: {Order: providers, UpdatedAt: time.Now()}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.dailyGate = 300 * time.Millisecond
	blocked := make(chan struct{})
	scores := map[string]float64{"p0": 1, "p1": 2, "p2": 3, "p3": 4, "p4": 5, "p5": 6, "p6": 7, "p7": 8, "p8": 9, "p9": 10}
	p.dailyTestProvider = func(ctx context.Context, _, provider string, _ int) (providerTestSample, error) {
		if provider == "p7" || provider == "p8" || provider == "p9" {
			select {
			case <-blocked:
			case <-ctx.Done():
				return providerTestSample{}, ctx.Err()
			}
		}
		return pingSample(scores[provider], time.Second), nil
	}

	refreshDone := make(chan struct{})
	close(refreshDone)
	ready := make(chan struct{})
	done := make(chan struct{})
	started := time.Now()
	p.dailyWG.Add(1)
	go func() {
		p.runDailyBenchmark(model, refreshDone, ready)
		close(done)
	}()

	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("daily gate never released")
	}
	if elapsed := time.Since(started); elapsed < p.dailyGate {
		t.Fatalf("run finished after %v, before its %v deadline: a completion share ended it", elapsed, p.dailyGate)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("remaining benchmark requests were not canceled")
	}
	partial, _ := p.routing.modelConfig(model)
	if partial.Order[0] != "p6" {
		t.Fatalf("partial best = %q, want p6", partial.Order[0])
	}
	if provider, manual := p.routing.pinInfo(model); provider != "p6" || manual {
		t.Fatalf("daily benchmark pin = %q manual=%v, want automatic p6 pin", provider, manual)
	}
	if persisted := p.stats.providerPinsSnapshot()[model]; persisted.Provider != "p6" || persisted.PinnedAt.IsZero() {
		t.Fatalf("daily benchmark did not persist p6 auto pin: %#v", persisted)
	}
}

func TestDailyBenchmarkGateStaysThreeSeconds(t *testing.T) {
	if dailyBenchmarkGate != 3*time.Second {
		t.Fatalf("production daily benchmark gate = %v, want exactly 3s", dailyBenchmarkGate)
	}
}

func TestAPIErrorsAffectAutomaticChoiceOnlyInsideMetricsWindow(t *testing.T) {
	const model = "author/model"
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.Local)
	records := []requestRecord{
		{Time: now.Add(-measuredMetricsTTL - time.Nanosecond), Model: model, Provider: "expired", Status: http.StatusBadGateway, Err: true},
		{Time: now.Add(-measuredMetricsTTL), Model: model, Provider: "boundary", Status: http.StatusBadGateway, Err: true},
		{Time: now.Add(-measuredMetricsTTL + time.Nanosecond), Model: model, Provider: "failing", Status: http.StatusBadGateway, Err: true},
		{Time: now, Model: model, Provider: "healthy", Status: http.StatusOK},
		{Time: now, Model: model, Provider: "policy-blocked", Status: http.StatusNotFound, Err: true, Blocked: true},
		{Time: now, Model: model, Provider: "cancelled", Status: 0, Err: true},
	}
	rates := providerAPIErrorRates(records, model, now)
	if rates["failing"] != 1 || rates["boundary"] != 1 || rates["healthy"] != 0 {
		t.Fatalf("error rates = %v, want failing=1 boundary=1 healthy=0", rates)
	}
	for _, excluded := range []string{"expired", "policy-blocked", "cancelled"} {
		if _, ok := rates[excluded]; ok {
			t.Fatalf("%s unexpectedly affected provider health: %v", excluded, rates)
		}
	}

	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {Order: []string{"failing", "healthy"}}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.setClock(func() time.Time { return now })
	p.routing.endpointCache[model] = []endpointMeta{
		{Tag: "failing", Pricing: testPricing(.1, .4, .01)},
		{Tag: "healthy", Pricing: testPricing(1, 4, .1)},
	}
	for _, record := range records {
		p.stats.record(record)
	}
	p.routing.autoPin(model, "failing")
	p.stats.setAutoPinAt(model, "failing", now)
	p.applyProviderTestResults(model, []providerTestResult{
		benchmarkResult("failing", 500, 100*time.Millisecond),
		benchmarkResult("healthy", 100, 500*time.Millisecond),
	}, p.routing.modelRevision(model))
	if provider, manual := p.routing.pinInfo(model); provider != "healthy" || manual {
		t.Fatalf("error-aware pin = %q manual=%v, want healthy automatic", provider, manual)
	}
}

func TestDailyBenchmarkGateReleasesAfterThreeSecondEquivalentTimeout(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.Models = map[string]providerConfig{model: {Order: []string{"p0", "p1"}, UpdatedAt: time.Now()}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.dailyGate = 25 * time.Millisecond
	blocked := make(chan struct{})
	p.dailyTestProvider = func(ctx context.Context, _, _ string, _ int) (providerTestSample, error) {
		select {
		case <-blocked:
			return pingSample(1, time.Second), nil
		case <-ctx.Done():
			return providerTestSample{}, ctx.Err()
		}
	}
	refreshDone := make(chan struct{})
	ready := make(chan struct{})
	done := make(chan struct{})
	p.dailyWG.Add(1)
	started := time.Now()
	go func() {
		p.runDailyBenchmark(model, refreshDone, ready)
		close(done)
	}()

	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("daily gate did not release on timeout")
	}
	if elapsed := time.Since(started); elapsed < p.dailyGate {
		t.Fatalf("gate released early after %v, want at least %v", elapsed, p.dailyGate)
	}
	select {
	case <-done:
		t.Fatal("daily refresh unexpectedly finished before its catalog request")
	default:
	}
	close(refreshDone)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("daily refresh did not stop after the expired gate")
	}
	close(blocked)
}

func TestDailyBenchmarkTimeoutPersistsCompletedPartialResults(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"slow-one", "winner", "slow-two"},
		UpdatedAt: time.Now(),
	}}
	db := openTestStore(t)
	cfg.db = db
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.endpointCache[model] = []endpointMeta{{Tag: "winner", Pricing: testPricing(1, 4, .1)}}
	p.dailyGate = 30 * time.Millisecond
	p.dailyTestProvider = func(ctx context.Context, _, provider string, _ int) (providerTestSample, error) {
		if provider == "winner" {
			return pingSample(200, 100*time.Millisecond), nil
		}
		<-ctx.Done()
		return providerTestSample{}, ctx.Err()
	}
	refreshDone := make(chan struct{})
	close(refreshDone)
	ready := make(chan struct{})
	done := make(chan struct{})
	p.dailyWG.Add(1)
	go func() {
		p.runDailyBenchmark(model, refreshDone, ready)
		close(done)
	}()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("daily timeout did not release")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("partial benchmark did not finish applying results")
	}

	updated, _ := p.routing.modelConfig(model)
	if len(updated.Order) == 0 || updated.Order[0] != "winner" {
		t.Fatalf("partial order = %v, want completed winner first", updated.Order)
	}
	saved, err := db.Models()
	if err != nil {
		t.Fatal(err)
	}
	if order := saved[model].Order; len(order) == 0 || order[0] != "winner" {
		t.Fatalf("persisted partial order = %v", order)
	}
	if provider, manual := p.routing.pinInfo(model); provider != "winner" || manual {
		t.Fatalf("partial-result pin = %q manual=%v", provider, manual)
	}
}

func TestAutomaticProviderBenchmarkStartsAllAndCancelsWorkers(t *testing.T) {
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var active atomic.Int32
	var peak atomic.Int32
	p.dailyTestProvider = func(ctx context.Context, _, _ string, _ int) (providerTestSample, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		<-ctx.Done()
		return providerTestSample{}, ctx.Err()
	}
	providers := []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9"}
	results, reachedThreshold := p.benchmarkProviders("author/model", providers, time.Now().Add(25*time.Millisecond), true, len(providers))
	if reachedThreshold {
		t.Fatal("benchmark reported full completion after every worker was canceled")
	}
	if len(results) != 0 {
		t.Fatalf("canceled benchmark returned results: %v", results)
	}
	if got := peak.Load(); got != int32(len(providers)) {
		t.Fatalf("peak concurrency = %d, want all %d providers", got, len(providers))
	}
	if got := active.Load(); got != 0 {
		t.Fatalf("benchmark returned with %d workers still mutating stats", got)
	}
}

func TestBenchmarkDeadlineWaitsForPoolInvalidation(t *testing.T) {
	const model = "author/model"
	const provider = "stale"
	p, err := newProxy(testConfig("https://upstream.test/api/v1"))
	if err != nil {
		t.Fatal(err)
	}
	p.stats.mu.Lock()
	p.stats.addBenchmarkSampleLocked(model, provider, benchmarkSample{
		Time: time.Now(), TPS: 200, TTFTms: 300,
	})
	p.stats.mu.Unlock()
	p.dailyTestProvider = func(ctx context.Context, _, _ string, _ int) (providerTestSample, error) {
		<-ctx.Done()
		// Make the old race deterministic: benchmarkProviders used to return
		// while this worker still had the stale pool in place.
		time.Sleep(20 * time.Millisecond)
		p.stats.clearBenchmarkSamples(model, provider)
		return providerTestSample{}, fmt.Errorf("%w: current benchmark failed", errProviderAnswered)
	}

	results, reachedThreshold := p.benchmarkProviders(model, []string{provider}, time.Now().Add(10*time.Millisecond), false, 1)
	if reachedThreshold || len(results) != 0 {
		t.Fatalf("deadline result = %v target=%v, want no completed provider", results, reachedThreshold)
	}
	if samples := p.stats.benchmarkPool(model)[provider]; len(samples) != 0 {
		t.Fatalf("benchmark returned before stale pool invalidation: %#v", samples)
	}
}

func TestStopDailyBenchmarksPreventsNewWork(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"provider"},
		UpdatedAt: time.Now().Add(-24 * time.Hour),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.stopDailyBenchmarks()
	if ready := p.prepareDailyModel(model); ready != nil {
		t.Fatal("daily benchmark started after admission was closed")
	}
	p.waitForDailyBenchmarks()
}

func TestProviderBenchmarkTargetsEveryProvider(t *testing.T) {
	p, err := newProxy(testConfig("https://upstream.test/api/v1"))
	if err != nil {
		t.Fatal(err)
	}
	p.dailyTestProvider = func(_ context.Context, _, provider string, _ int) (providerTestSample, error) {
		return pingSample(100, time.Millisecond), nil
	}
	providers := []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9"}

	// Automatic and manual runs share one rule now: measure the whole field or
	// run out of budget.
	automatic, reachedTarget := p.benchmarkProviders("author/model", providers, time.Now().Add(time.Second), true, len(providers))
	if !reachedTarget || len(automatic) != len(providers) {
		t.Fatalf("automatic results = %d target=%v, want all %d", len(automatic), reachedTarget, len(providers))
	}
	manual, reachedTarget := p.benchmarkProviders("author/model", providers, time.Now().Add(time.Second), false, len(providers))
	if !reachedTarget || len(manual) != len(providers) {
		t.Fatalf("manual results = %d target=%v, want all %d", len(manual), reachedTarget, len(providers))
	}
}

// Measurement follows the models requests name, and a stale run is what asks for
// a new one: a model whose last run is older than the freshness window is
// measured on the request path, a model measured moments ago is not, and no
// other model is touched either way.
func TestRequestMeasuresAModelWhoseRunIsStale(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.Models = map[string]providerConfig{model: {Order: []string{"provider"}, UpdatedAt: time.Now()}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.client.Transport = degradationCatalogTransport("provider")
	var benchmarks atomic.Int32
	p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		benchmarks.Add(1)
		return pingSample(100, time.Millisecond), nil
	}
	p.stats.markBenchmarkRun(model, p.now().Add(-time.Hour))

	ready := p.prepareDailyModel(model)
	if ready == nil {
		t.Fatal("a request for a model whose measurement is an hour old did not start a run")
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("run never finished")
	}
	p.waitForDailyBenchmarks()
	if got := benchmarks.Load(); got == 0 {
		t.Fatal("the run measured nothing")
	}
	if !p.stats.benchmarkedWithin(model, p.now(), benchmarkFreshness) {
		t.Fatal("the run did not record its benchmark")
	}
	if again := p.prepareDailyModel(model); again != nil {
		p.waitForDailyBenchmarks()
		t.Fatal("the next request started a second run for a model just measured")
	}
}

// A refresh that outlives the gate must not consume the budget the pings need:
// the run refreshes, then measures on a fresh clock. Otherwise a model whose
// catalog is slow keeps its refreshed order and is never measured at all.
func TestSlowRefreshStillBenchmarksProviders(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.Models = map[string]providerConfig{model: {Order: []string{"slow-catalog"}, UpdatedAt: time.Now()}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.dailyGate = 100 * time.Millisecond
	var benchmarks atomic.Int32
	p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		benchmarks.Add(1)
		return pingSample(100, 100*time.Millisecond), nil
	}
	refreshDone := make(chan struct{})
	ready := make(chan struct{})
	slowRefresh := make(chan struct{})
	go func() {
		defer close(slowRefresh)
		time.Sleep(250 * time.Millisecond)
		close(refreshDone)
	}()
	p.dailyWG.Add(1)
	go p.runDailyBenchmark(model, refreshDone, ready)
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("gate was never released behind a slow refresh")
	}
	<-slowRefresh
	p.waitForDailyBenchmarks()
	if got := benchmarks.Load(); got == 0 {
		t.Fatal("a refresh slower than the gate left the providers unmeasured")
	}
}

func TestManualProviderBenchmarkStartsEveryProviderSimultaneously(t *testing.T) {
	p, err := newProxy(testConfig("https://upstream.test/api/v1"))
	if err != nil {
		t.Fatal(err)
	}
	providers := []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9"}
	var active atomic.Int32
	var peak atomic.Int32
	release := make(chan struct{})
	p.dailyTestProvider = func(ctx context.Context, _, _ string, _ int) (providerTestSample, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		select {
		case <-release:
			return pingSample(100, time.Millisecond), nil
		case <-ctx.Done():
			return providerTestSample{}, ctx.Err()
		}
	}
	type outcome struct {
		results []providerTestResult
		full    bool
	}
	done := make(chan outcome, 1)
	go func() {
		results, full := p.benchmarkProviders("author/model", providers, time.Now().Add(time.Second), false, len(providers))
		done <- outcome{results: results, full: full}
	}()
	for deadline := time.Now().Add(time.Second); peak.Load() < int32(len(providers)) && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if got := peak.Load(); got != int32(len(providers)) {
		close(release)
		t.Fatalf("manual peak concurrency = %d, want all %d providers", got, len(providers))
	}
	close(release)
	result := <-done
	if !result.full || len(result.results) != len(providers) {
		t.Fatalf("manual results = %d full=%v, want all %d", len(result.results), result.full, len(providers))
	}
}

func TestDailyBenchmarkDoesNotDelayTriggeringRequestAndUpdatesNextRequest(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.UpdateMaxProviders = 20
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"slow", "fast"},
		UpdatedAt: time.Now().Add(-24 * time.Hour),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	releaseBenchmark := make(chan struct{})
	p.dailyTestProvider = func(ctx context.Context, _, provider string, _ int) (providerTestSample, error) {
		select {
		case <-releaseBenchmark:
		case <-ctx.Done():
			return providerTestSample{}, ctx.Err()
		}
		if provider == "fast" {
			return pingSample(200, 100*time.Millisecond), nil
		}
		return pingSample(20, time.Second), nil
	}
	routed := make(chan string, 2)
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			catalog := endpointList{}
			catalog.Data.Endpoints = []modelEndpoint{
				{Tag: "slow", ProviderName: "Slow", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}, Throughput: &percentiles{P50: 100}},
				{Tag: "fast", ProviderName: "Fast", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}, Throughput: &percentiles{P50: 10}},
			}
			data, _ := json.Marshal(catalog)
			return response(http.StatusOK, string(data)), nil
		}
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(data, &body)
		provider, _ := body["provider"].(map[string]any)
		only, _ := provider["only"].([]any)
		if len(only) > 0 {
			routed <- only[0].(string)
		} else {
			routed <- ""
		}
		return response(http.StatusOK, `{"model":"author/model","provider":"fast","usage":{"prompt_tokens":1,"completion_tokens":1}}`), nil
	})

	requestDone := make(chan struct{})
	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"author/model","messages":[{"role":"user","content":"hello"}]}`))
		p.ServeHTTP(newMemoryResponseWriter(), req)
		close(requestDone)
	}()
	select {
	case <-requestDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("triggering request waited for the daily benchmark")
	}
	if got := <-routed; got != "slow" {
		t.Fatalf("triggering request routed to %q, want existing automatic pin slow", got)
	}

	close(releaseBenchmark)
	p.waitForDailyBenchmarks()
	second, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(`{"model":"author/model","messages":[{"role":"user","content":"again"}]}`))
	p.ServeHTTP(newMemoryResponseWriter(), second)
	if got := <-routed; got != "fast" {
		t.Fatalf("request after benchmark routed to %q, want measured winner fast", got)
	}
}

// A three-round benchmark tolerates one failed round, so a single timeout does
// not throw away two good measurements and drop the provider out of ranking.
func TestProxyTestProviderToleratesOneFailedRound(t *testing.T) {
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":3}}\n\n" +
		"data: [DONE]\n"
	calls := 0
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return response(http.StatusBadGateway, `{}`), nil
		}
		time.Sleep(2 * time.Millisecond)
		return response(http.StatusOK, stream), nil
	})
	sample, err := p.testProvider("moonshotai/kimi-k2.5", "fireworks", 3)
	if err != nil {
		t.Fatalf("one failed round should not fail the benchmark: %v", err)
	}
	if sample.samples != 2 {
		t.Fatalf("samples = %d, want the 2 successful rounds", sample.samples)
	}
	if sample.tps <= 0 || sample.ttft <= 0 {
		t.Fatalf("sample = %+v, want usable measurements", sample)
	}
	// Every round is a recorded, billed request, failures included — but a ping
	// is orr's own probe, so it stays out of the client request counters.
	snap := p.stats.snapshot()
	if len(snap.Records) != 3 || snap.RequestCount != 0 {
		t.Fatalf("records = %d request count = %d, want 3 recorded rounds and no client requests", len(snap.Records), snap.RequestCount)
	}
}

func TestProxyTestProviderFailsWhenTooManyRoundsFail(t *testing.T) {
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":3}}\n\n" +
		"data: [DONE]\n"
	calls := 0
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls <= 2 {
			return response(http.StatusBadGateway, `{}`), nil
		}
		return response(http.StatusOK, stream), nil
	})
	if _, err := p.testProvider("moonshotai/kimi-k2.5", "fireworks", 3); err == nil {
		t.Fatal("two failed rounds should fail the benchmark")
	}
}

// Benchmark rounds must be marked, because they are the only first-token
// measurements that can be compared across providers.
func TestProxyTestProviderMarksRoundsAsBenchmarks(t *testing.T) {
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":3}}\n\n" +
		"data: [DONE]\n"
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		time.Sleep(2 * time.Millisecond)
		return response(http.StatusOK, stream), nil
	})
	if _, err := p.testProvider("moonshotai/kimi-k2.5", "fireworks", 2); err != nil {
		t.Fatal(err)
	}
	snap := p.stats.snapshot()
	if len(snap.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(snap.Records))
	}
	for _, record := range snap.Records {
		if !record.Benchmark {
			t.Fatalf("benchmark round was not marked: %+v", record)
		}
	}
}

// A reply can arrive faster than the platform clock resolves — over loopback
// on Windows the round measures exactly zero — and dividing a token count by
// that duration used to yield an infinite throughput that the ranking then
// discarded, throwing away the fastest provider. A round too fast to measure
// must still be a usable measurement.
func TestProxyTestProviderMeasuresARoundTooFastToTime(t *testing.T) {
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":3}}\n\n" +
		"data: [DONE]\n"
	// No sleep anywhere: the whole round can land inside one clock tick.
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusOK, stream), nil
	})
	sample, err := p.testProvider("moonshotai/kimi-k2.5", "fireworks", 3)
	if err != nil {
		t.Fatalf("an immeasurably fast provider failed its benchmark: %v", err)
	}
	if sample.samples != 3 {
		t.Fatalf("samples = %d, want all 3 rounds counted", sample.samples)
	}
	if sample.tps <= 0 || math.IsNaN(sample.tps) || math.IsInf(sample.tps, 0) {
		t.Fatalf("tps = %v, want a finite positive throughput", sample.tps)
	}
	// A finite throughput is what keeps the provider in the ranking at all.
	if _, ok := benchmarkReferenceProfile.seconds(newProviderTestResult("fireworks", sample, nil)); !ok && sample.ttft > 0 {
		t.Fatalf("sample %+v could not be timed", sample)
	}
}

// The `O` probe leaves provider preferences off the request so OpenRouter's
// default routing chooses, then reports the endpoint it picked.
func TestProbeDefaultProviderReportsServingProvider(t *testing.T) {
	const model = "moonshotai/kimi-k2.5"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.routing.endpointCache[model] = []endpointMeta{{Tag: "fireworks", ProviderName: "Fireworks"}}
	stream := "data: {\"provider\":\"fireworks\",\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":3}}\n\n" +
		"data: [DONE]\n"
	var body []byte
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ = io.ReadAll(r.Body)
		return response(http.StatusOK, stream), nil
	})
	provider, err := p.probeDefaultProvider(model)
	if err != nil {
		t.Fatal(err)
	}
	if provider != "fireworks" {
		t.Fatalf("probe provider = %q, want fireworks", provider)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["provider"]; ok {
		t.Fatalf("probe sent a provider block, so OpenRouter was not choosing: %s", body)
	}
	// The probe is orr's own request, so it stays out of the client counters but
	// is still recorded as a benchmark round.
	snap := p.stats.snapshot()
	if len(snap.Records) != 1 || snap.RequestCount != 0 || !snap.Records[0].Benchmark {
		t.Fatalf("records = %d request count = %d, want one benchmark record", len(snap.Records), snap.RequestCount)
	}
}

func TestDailyRefreshWithoutKeyUpdatesProvisionalAutomaticChoice(t *testing.T) {
	const model = "author/model"
	t.Setenv("OPENROUTER_API_KEY", "")
	cfg := testConfig("https://upstream.test/api/v1")
	// No OpenRouterAPIKey: catalog refresh still works, while benchmarks do not.
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"old-head", "new-head"},
		UpdatedAt: time.Now().Add(-24 * time.Hour),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	routed := make(chan string, 2)
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			catalog := endpointList{}
			catalog.Data.Endpoints = []modelEndpoint{
				{Tag: "old-head", ProviderName: "Old", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}, Throughput: &percentiles{P50: 10}},
				{Tag: "new-head", ProviderName: "New", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}, Throughput: &percentiles{P50: 100}},
			}
			data, _ := json.Marshal(catalog)
			return response(http.StatusOK, string(data)), nil
		}
		var payload struct {
			Provider struct {
				Only []string `json:"only"`
			} `json:"provider"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			return nil, err
		}
		if len(payload.Provider.Only) == 1 {
			routed <- payload.Provider.Only[0]
		} else {
			routed <- ""
		}
		return response(http.StatusOK, `{"model":"author/model","usage":{}}`), nil
	})

	request := func() {
		req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions",
			strings.NewReader(`{"model":"author/model","messages":[]}`))
		p.ServeHTTP(newMemoryResponseWriter(), req)
	}
	request()
	if provider := <-routed; provider != "old-head" {
		t.Fatalf("triggering request used %q, want current head old-head", provider)
	}
	p.waitForDailyBenchmarks()
	request()
	if provider := <-routed; provider != "new-head" {
		t.Fatalf("request after unauthenticated refresh used %q, want refreshed head new-head", provider)
	}
	if provider, manual := p.routing.pinInfo(model); provider != "new-head" || manual {
		t.Fatalf("refreshed pin = %q manual=%v, want automatic new-head", provider, manual)
	}
}

// Skipping the run while pinned must not cost the model its measurement once the
// pin is gone: the next request measures the field again and installs the
// winner, so unpinning is not a state the app has to be restarted out of.
func TestUnpinningRestoresAutomaticMeasurement(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.UpdateMaxProviders = 20
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"slow", "fast"},
		ManualPin: "slow",
		UpdatedAt: time.Now().Add(-24 * time.Hour),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			catalog := endpointList{}
			catalog.Data.Endpoints = []modelEndpoint{
				{Tag: "slow", ProviderName: "Slow", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}},
				{Tag: "fast", ProviderName: "Fast", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}},
			}
			data, _ := json.Marshal(catalog)
			return response(http.StatusOK, string(data)), nil
		}
		return response(http.StatusOK, `{"id":"ok"}`), nil
	})
	p.dailyTestProvider = func(_ context.Context, _, provider string, _ int) (providerTestSample, error) {
		if provider == "fast" {
			return pingSample(200, 100*time.Millisecond), nil
		}
		return pingSample(20, time.Second), nil
	}

	if ready := p.prepareDailyModel(model); ready != nil {
		t.Fatal("a manual pin measured the field")
	}
	if err := p.routing.setManualPin(model, ""); err != nil {
		t.Fatal(err)
	}
	if provider, manual := p.routing.pinInfo(model); provider != "" || manual {
		t.Fatalf("pin after unpinning = %q manual=%v, want none", provider, manual)
	}
	ready := p.prepareDailyModel(model)
	if ready == nil {
		t.Fatal("an unpinned model did not start measuring again")
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("run never released the request gate")
	}
	p.waitForDailyBenchmarks()
	if provider := p.routing.pinnedProvider(model); provider != "fast" {
		t.Fatalf("automatic pin = %q, want the measured winner fast", provider)
	}
}

// Pinning a model must not stop orr from keeping its endpoint metadata current:
// the order is not used to choose a provider while pinned, but pricing, quality,
// and the dashboard still read that cache.
func TestManualPinKeepsEndpointMetadataFresh(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"pinned", "other"},
		ManualPin: "pinned",
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.trackEndpoints = true
	var catalogCalls atomic.Int32
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			catalogCalls.Add(1)
			catalog := endpointList{}
			catalog.Data.Endpoints = []modelEndpoint{
				{Tag: "pinned", ProviderName: "Pinned", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}},
				{Tag: "other", ProviderName: "Other", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}},
			}
			data, _ := json.Marshal(catalog)
			return response(http.StatusOK, string(data)), nil
		}
		return response(http.StatusOK, `{"model":"author/model","usage":{}}`), nil
	})

	req, _ := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions",
		strings.NewReader(`{"model":"author/model","messages":[]}`))
	p.ServeHTTP(newMemoryResponseWriter(), req)
	deadline := time.Now().Add(2 * time.Second)
	for len(p.routing.endpoints(model)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a manually pinned model never refreshed its endpoint metadata")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := catalogCalls.Load(); got == 0 {
		t.Fatal("endpoint catalog was never fetched for a pinned model")
	}
}

// A manual pin is a pin of one provider, so a 429 on it is answered rather than
// routed around. The gate reads the pin the same way routing does, so the
// provider the client is pinned to is the provider the failover decision sees.
func TestRateLimitDoesNotFailOverAManualPin(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.RateLimitFailoverThreshold = 1
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"pinned", "other"},
		ManualPin: "pinned",
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}

	replacement, failover := p.rateLimitReplacement(model, "pinned", false)
	if failover || replacement != "" {
		t.Fatalf("429 failover = (replacement %q, %v), want none for a manual pin", replacement, failover)
	}
	p.rateLimitMu.Lock()
	streak := p.rateLimitRuns[rateLimitKey{model: model, provider: "pinned"}]
	p.rateLimitMu.Unlock()
	if streak != 0 {
		t.Fatalf("consecutive 429 count = %d, want the pinned provider's streak dropped", streak)
	}
	if provider := p.routing.pinnedProvider(model); provider != "pinned" {
		t.Fatalf("routed provider = %q, want the pinned provider", provider)
	}
	if provider, manual := p.routing.pinInfo(model); provider != "pinned" || !manual {
		t.Fatalf("pin = %q manual=%v, want pinned manual", provider, manual)
	}
}

// A run that could not test every provider inside the gate still consumes the
// freshness window. The unmeasured providers keep whatever the pool holds and
// the next request must not relaunch the run just because it finished partial.
func TestPartialBenchmarkStillConsumesTheFreshnessWindow(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.UpdateMaxProviders = 20
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"winner", "slow"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.dailyGate = 200 * time.Millisecond
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return response(http.StatusOK, `{"id":"ok"}`), nil
		}
		catalog := endpointList{}
		catalog.Data.Endpoints = []modelEndpoint{
			{Tag: "winner", ProviderName: "Winner", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}},
			{Tag: "slow", ProviderName: "Slow", Status: 0, SupportedParameters: []string{"tools", "tool_choice"}},
		}
		data, _ := json.Marshal(catalog)
		return response(http.StatusOK, string(data)), nil
	})
	var measured atomic.Int32
	p.dailyTestProvider = func(ctx context.Context, _, provider string, _ int) (providerTestSample, error) {
		if provider == "winner" {
			measured.Add(1)
			return pingSample(200, 100*time.Millisecond), nil
		}
		<-ctx.Done()
		return providerTestSample{}, ctx.Err()
	}

	ready := p.prepareDailyModel(model)
	if ready == nil {
		t.Fatal("a due model did not start a run")
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("partial run never released the request gate")
	}
	p.waitForDailyBenchmarks()
	if got := measured.Load(); got == 0 {
		t.Fatal("the run measured nothing")
	}
	pool := p.stats.benchmarkPool(model)
	if len(pool["slow"]) != 0 {
		t.Fatalf("slow provider was measured despite the gate: %#v", pool["slow"])
	}
	if !p.stats.benchmarkedWithin(model, p.now(), benchmarkFreshness) {
		t.Fatal("a partial run left the freshness window open")
	}
	if again := p.prepareDailyModel(model); again != nil {
		p.waitForDailyBenchmarks()
		t.Fatal("a partial run was relaunched by the next request")
	}
}

// An alias id has no endpoint list to fetch and nothing to measure, so a request
// for one must not start a run at all — otherwise it cycles a no-op run for as
// long as the client keeps using the alias.
func TestAliasModelSkipsTheDailyRun(t *testing.T) {
	const model = "~author/family-latest"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"provider"},
		UpdatedAt: time.Now().Add(-24 * time.Hour),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var catalogCalls, pings atomic.Int32
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			catalogCalls.Add(1)
		}
		return response(http.StatusOK, `{"id":"ok"}`), nil
	})
	p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		pings.Add(1)
		return pingSample(100, time.Millisecond), nil
	}

	if ready := p.prepareDailyModel(model); ready != nil {
		p.waitForDailyBenchmarks()
		t.Fatal("an alias model started a daily run")
	}
	p.waitForDailyBenchmarks()
	if got := pings.Load(); got != 0 {
		t.Fatalf("benchmark pings = %d for an alias model", got)
	}
	if got := catalogCalls.Load(); got != 0 {
		t.Fatalf("catalog reads = %d for an alias model", got)
	}
}

// A run that pings nothing because every provider is in a 429 backoff spends no
// provider request, but its catalog refresh is still a request, so it settles the
// schedule for the freshness window instead of re-fetching once a minute.
func TestNoEligibleProvidersConsumesTheFreshnessWindow(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.UpdateMaxProviders = 20
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"provider"},
		UpdatedAt: time.Now(),
	}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var catalogCalls, pings atomic.Int32
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return response(http.StatusOK, `{"id":"ok"}`), nil
		}
		catalogCalls.Add(1)
		catalog := endpointList{}
		catalog.Data.Endpoints = []modelEndpoint{{
			Tag: "provider", ProviderName: "Provider", Status: 0,
			SupportedParameters: []string{"tools", "tool_choice"},
		}}
		data, _ := json.Marshal(catalog)
		return response(http.StatusOK, string(data)), nil
	})
	p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		pings.Add(1)
		return pingSample(100, time.Millisecond), nil
	}
	// The only provider exhausted its capacity, which is what keeps an automatic
	// run from pinging anything.
	p.routing.mu.Lock()
	p.routing.temporarilyUnavailable[model] = map[string]unavailableMark{"provider": {detectedAt: p.now(), rateLimit: true}}
	p.routing.mu.Unlock()

	ready := p.prepareDailyModel(model)
	if ready == nil {
		t.Fatal("a due model did not start a run")
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("run never released the request gate")
	}
	p.waitForDailyBenchmarks()
	if got := pings.Load(); got != 0 {
		t.Fatalf("benchmark pings = %d for a provider in backoff", got)
	}
	if !p.stats.benchmarkedWithin(model, p.now(), benchmarkFreshness) {
		t.Fatal("a run that refreshed the catalog did not consume the freshness window")
	}
	if p.stats.benchmarkAbortedWithin(model, p.now(), benchmarkRetryAfterAbort) {
		t.Fatal("a run that refreshed the catalog also armed the short abort floor")
	}
	if again := p.prepareDailyModel(model); again != nil {
		p.waitForDailyBenchmarks()
		t.Fatal("the next request fetched the same catalog again")
	}
}

// loopProxy answers every generation request with body as an event stream, so
// the loop stage runs through forward the way it does in production.
func loopProxy(t *testing.T, mode loopMode, body string) (*proxy, *memoryResponseWriter) {
	t.Helper()
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Loop = streamConfig(mode)
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Type", "text/event-stream")
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	return p, newMemoryResponseWriter()
}

// loopForward runs one chat completion through forward.
func loopForward(t *testing.T, p *proxy, w http.ResponseWriter) requestRecord {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions",
		strings.NewReader(`{"model":"moonshotai/kimi-k2.5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	record, err := p.forward(w, req)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestProxyCutsAndRecordsALoop(t *testing.T) {
	chunks, line := streamLoopBody("/v1/chat/completions")
	full := strings.Join(chunks, "")
	p, w := loopProxy(t, loopModeAbort, full)
	record := loopForward(t, p, w)

	if record.Status != http.StatusOK {
		t.Fatalf("status = %d, want the response to stay 200", record.Status)
	}
	if record.Err {
		t.Fatal("a repetition is not a request error")
	}
	if !record.Loop || !record.LoopAborted {
		t.Fatalf("record loop/aborted = %v/%v, want both set", record.Loop, record.LoopAborted)
	}
	if record.LoopRecovered {
		t.Fatal("abort mode recorded a recovery")
	}
	if record.LoopPeriod != len(line) || record.LoopRepeat < loopMinRepeatCount {
		t.Fatalf("recorded period/repeat = %d/%d, want %d and at least %d", record.LoopPeriod, record.LoopRepeat, len(line), loopMinRepeatCount)
	}
	// A repetition is not a provider fault, so nothing may be attributed to it.
	if record.Blocked || record.RateLimited || record.ProviderUnavailable || record.ProviderFailureHandled {
		t.Fatalf("the cut touched provider health: %+v", record)
	}
	out := w.Body.String()
	if !strings.HasSuffix(out, string(loopTerminal("/v1/chat/completions", 0))) {
		t.Fatal("the cut response does not end with the chat terminal sequence")
	}
	if strings.Count(out, "data: [DONE]") != 1 {
		t.Fatalf("%d done sentinels, want one", strings.Count(out, "data: [DONE]"))
	}
	if len(out) >= len(full)/2 {
		t.Fatalf("client received %d of %d bytes, want the loop cut short", len(out), len(full))
	}
}

func TestProxyObserveKeepsTheWholeLoopingStream(t *testing.T) {
	chunks, line := streamLoopBody("/v1/chat/completions")
	full := strings.Join(chunks, "")
	p, w := loopProxy(t, loopModeObserve, full)
	record := loopForward(t, p, w)

	if !record.Loop || record.LoopAborted {
		t.Fatalf("record loop/aborted = %v/%v, want the loop marked and nothing aborted", record.Loop, record.LoopAborted)
	}
	if record.LoopPeriod != len(line) {
		t.Fatalf("recorded period = %d, want %d", record.LoopPeriod, len(line))
	}
	if w.Body.String() != full {
		t.Fatalf("observe released %d bytes, want all %d", w.Body.Len(), len(full))
	}
}

func TestProxyOffModeLeavesALoopingStreamAlone(t *testing.T) {
	chunks, _ := streamLoopBody("/v1/chat/completions")
	body := strings.Join(chunks, "") +
		`data: {"usage":{"prompt_tokens":7,"completion_tokens":11,"cost":0.5}}` + "\n\n" + "data: [DONE]\n\n"
	p, w := loopProxy(t, loopModeOff, body)
	record := loopForward(t, p, w)

	if record.Loop || record.LoopAborted || record.LoopPeriod != 0 || record.LoopRepeat != 0 {
		t.Fatalf("off mode recorded a loop: %+v", record)
	}
	if w.Body.String() != body {
		t.Fatalf("off mode released %d bytes, want all %d", w.Body.Len(), len(body))
	}
	// The usage parsers still read the response's end through the stage.
	if record.PromptTokens != 7 || record.CompletionTokens != 11 || record.Cost != 0.5 {
		t.Fatalf("usage = %d/%d/$%v, want 7/11/$0.5", record.PromptTokens, record.CompletionTokens, record.Cost)
	}
}

// loopStreamAnswer is an event-stream response carrying body.
func loopStreamAnswer(body string) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "text/event-stream")
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

// loopSpliceProxy answers the first generation request with looping and every
// continuation request with answer. The continuation is recognised by the
// intervention turn orr appends to it, which is the one thing the two legs do not
// share. continuations counts only those requests, so catalog and endpoint refreshes
// in the background cannot confuse the count, and sent holds the last continuation
// body for the routing assertions.
func loopSpliceProxy(t *testing.T, cfg loopConfig, looping string, answer func() (*http.Response, error)) (*proxy, *memoryResponseWriter, *int, *string) {
	t.Helper()
	configuration := testConfig("https://upstream.test/api/v1")
	configuration.Loop = cfg
	p, err := newProxy(configuration)
	if err != nil {
		t.Fatal(err)
	}
	continuations := 0
	var sent string
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			return loopStreamAnswer(""), nil
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("reading the upstream request: %v", err)
		}
		if !strings.Contains(string(body), loopIntervention) && !strings.Contains(string(body), loopRestart) {
			return loopStreamAnswer(looping), nil
		}
		continuations++
		sent = string(body)
		return answer()
	})
	return p, newMemoryResponseWriter(), &continuations, &sent
}

// loopRecoveryPost runs one streaming chat completion through the whole handler, so
// the record it produces is the one the dashboard and the stats see. It also asserts
// the client request was counted once, which is where a continuation must not appear.
func loopRecoveryPost(t *testing.T, p *proxy, w http.ResponseWriter) requestRecord {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions",
		strings.NewReader(`{"model":"moonshotai/kimi-k2.5","messages":[{"role":"user","content":"write a story"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	p.ServeHTTP(w, req)
	snapshot := p.stats.snapshot()
	if !snapshot.HasLast {
		t.Fatal("the request produced no record")
	}
	if snapshot.RequestCount != 1 {
		t.Fatalf("stats counted %d client requests, want the one the client sent", snapshot.RequestCount)
	}
	return snapshot.Last
}

func TestProxyRecoversALoopAndAddsTheContinuationToTheRecord(t *testing.T) {
	chunks, line := streamLoopBody("/v1/chat/completions")
	looping := streamStrings(chunks)
	continuation := streamContinuation("/v1/chat/completions")
	p, w, continuations, sent := loopSpliceProxy(t, streamConfig(loopModeRecover), looping, func() (*http.Response, error) {
		return loopStreamAnswer(continuation), nil
	})
	record := loopRecoveryPost(t, p, w)

	if record.Status != http.StatusOK || record.Err {
		t.Fatalf("status/err = %d/%v, want a 200 answer", record.Status, record.Err)
	}
	// Cut and recovered are both true: the repetition was stopped, and the answer the
	// client received continues from it.
	if !record.Loop || !record.LoopAborted || !record.LoopRecovered {
		t.Fatalf("record loop/aborted/recovered = %v/%v/%v, want all set", record.Loop, record.LoopAborted, record.LoopRecovered)
	}
	if record.LoopPeriod != len(line) || record.LoopRepeat < loopMinRepeatCount {
		t.Fatalf("recorded period/repeat = %d/%d, want %d and at least %d", record.LoopPeriod, record.LoopRepeat, len(line), loopMinRepeatCount)
	}
	// A repetition is not a provider fault, and the continuation is not a second
	// client request.
	if record.Blocked || record.RateLimited || record.ProviderUnavailable || record.ProviderFailureHandled {
		t.Fatalf("the recovery touched provider health: %+v", record)
	}
	if *continuations != 1 {
		t.Fatalf("upstream saw %d continuations, want exactly one", *continuations)
	}
	// The continuation's own usage is added to the record: the cut leg never reached
	// a usage frame of its own.
	if record.PromptTokens != 5 || record.CompletionTokens != 17 || record.Cost != 0.002 {
		t.Fatalf("usage = %d/%d/$%v, want 5/17/$0.002 from the continuation",
			record.PromptTokens, record.CompletionTokens, record.Cost)
	}
	// The continuation is built from the prepared request, so it keeps the provider
	// block orr injected and resumes the text the client already has.
	if !strings.Contains(*sent, `"only":["fireworks"]`) {
		t.Fatalf("continuation body lost the injected provider block:\n%s", *sent)
	}
	if !strings.Contains(*sent, `"role":"assistant"`) || !strings.Contains(*sent, "Here is the summary you asked for. ") {
		t.Fatalf("continuation body does not resume the clean prefix:\n%s", *sent)
	}
	out := w.Body.String()
	if count := strings.Count(out, "data: [DONE]"); count != 1 {
		t.Fatalf("%d done sentinels, want exactly one", count)
	}
	if count := strings.Count(out, `"finish_reason":"stop"`); count != 1 {
		t.Fatalf("%d finish_reason frames, want exactly one", count)
	}
	if !strings.Contains(out, streamContinuationText) {
		t.Fatal("the continuation's text never reached the client")
	}
	if count := strings.Count(out, line); count >= 20 {
		t.Fatalf("the client received %d repetitions, want the loop cut short", count)
	}
}

func TestProxyRecoveryEndsTheAnswerWhenTheContinuationIsRefused(t *testing.T) {
	chunks, _ := streamLoopBody("/v1/chat/completions")
	looping := streamStrings(chunks)
	p, w, continuations, _ := loopSpliceProxy(t, streamConfig(loopModeRecover), looping, func() (*http.Response, error) {
		// A rate limit on the continuation. The client's own request was answered, so
		// this must not be attributed to the provider or turn the request into an error.
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":429,"message":"rate limited"}}`)),
		}, nil
	})
	record := loopRecoveryPost(t, p, w)

	if *continuations != 1 {
		t.Fatalf("upstream saw %d continuations, want one attempt", *continuations)
	}
	if record.Err {
		t.Fatal("a refused continuation turned an answered request into an error")
	}
	if !record.Loop || !record.LoopAborted {
		t.Fatalf("record loop/aborted = %v/%v, want the cut recorded", record.Loop, record.LoopAborted)
	}
	if record.LoopRecovered {
		t.Fatal("a refused continuation was recorded as recovered")
	}
	if record.RateLimited || record.Blocked || record.ProviderUnavailable || record.ProviderFailureHandled {
		t.Fatalf("the refused continuation touched provider health: %+v", record)
	}
	out := w.Body.String()
	if count := strings.Count(out, "data: [DONE]"); count != 1 {
		t.Fatalf("%d done sentinels, want the cut closed exactly once", count)
	}
	if strings.Contains(out, streamContinuationText) {
		t.Fatal("the client saw text from a continuation that never answered")
	}
	if record.CompletionTokens != 0 || record.Cost != 0 {
		t.Fatalf("usage = %d/$%v, want nothing from a continuation that never answered",
			record.CompletionTokens, record.Cost)
	}
}

func TestProxyRecoveryEstimatesTheContinuationWithoutUsage(t *testing.T) {
	chunks, _ := streamLoopBody("/v1/chat/completions")
	looping := streamStrings(chunks)
	// A leg that streams text and ends without a usage frame: what it generated is
	// still part of the answer, so it is accounted with the estimate.
	continuation := loopChatFrame(streamContinuationText) +
		loopChatFrame("A second sentence carries the answer on. ") + "data: [DONE]\n\n"
	p, w, continuations, _ := loopSpliceProxy(t, streamConfig(loopModeRecover), looping, func() (*http.Response, error) {
		return loopStreamAnswer(continuation), nil
	})
	record := loopRecoveryPost(t, p, w)

	if *continuations != 1 {
		t.Fatalf("upstream saw %d continuations, want exactly one", *continuations)
	}
	if !record.LoopRecovered {
		t.Fatalf("record = %+v, want the answer recorded as recovered", record)
	}
	if record.CompletionTokens == 0 {
		t.Fatal("the continuation's output was not accounted at all")
	}
	if record.Cost != 0 {
		t.Fatalf("cost = %v, want nothing for a leg that reported no cost", record.Cost)
	}
	if !strings.Contains(w.Body.String(), streamContinuationText) {
		t.Fatal("the continuation's text never reached the client")
	}
}

// logCapture collects the standard logger's output for the rest of the test, so a
// test can assert what an operator would read in the plain log.
func logCapture(t *testing.T) *strings.Builder {
	t.Helper()
	var output strings.Builder
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &output
}

func TestProxyRecoveryLogsWhyASplicedAnswerEndedEarly(t *testing.T) {
	chunks, _ := streamLoopBody("/v1/chat/completions")
	looping := streamStrings(chunks)
	// The continuation repeats itself too. Its text is spliced in, so the request is
	// a recovery and the row reads ↺+; the answer then ends where that repetition was
	// confirmed, and the log has to say so — the marks cannot.
	legChunks, _ := streamLoopBody("/v1/chat/completions")
	logs := logCapture(t)
	p, w, continuations, _ := loopSpliceProxy(t, streamConfig(loopModeRecover), looping, func() (*http.Response, error) {
		return loopStreamAnswer(streamStrings(legChunks)), nil
	})
	record := loopRecoveryPost(t, p, w)

	if *continuations != 1 {
		t.Fatalf("upstream saw %d continuations, want the repeating one to end the answer", *continuations)
	}
	if !record.Loop || !record.LoopAborted || !record.LoopRecovered {
		t.Fatalf("record loop/aborted/recovered = %v/%v/%v, want the recovered mark", record.Loop, record.LoopAborted, record.LoopRecovered)
	}
	if !strings.Contains(logs.String(), "stream recovered, then ended: continuation repeated itself") {
		t.Fatalf("log = %q, want the reason the splice ended early", logs.String())
	}
	if count := strings.Count(w.Body.String(), "data: [DONE]"); count != 1 {
		t.Fatalf("%d done sentinels, want exactly one", count)
	}
}

// accountWide429Body is a 429 OpenRouter raises for the whole account: it names
// no endpoint, so no provider can be blamed for it and none can be routed around.
const accountWide429Body = `{"error":{"message":"Too many requests","code":429}}`

// thinkingLoopBody is a response whose thinking repeats itself and whose answer has
// generated nothing yet, which is where a model that keeps re-planning degenerates.
func thinkingLoopBody(prefix string) (string, string) {
	line := prefix + "I'll write the response. I'll check the comments and report.\n"
	var body strings.Builder
	body.WriteString(loopChatReasoningFrame("Checking the comments first.\n"))
	for range 40 {
		body.WriteString(loopChatReasoningFrame(line))
	}
	return body.String(), line
}

func TestProxyRestartsAThinkingLoopThatProducedNoAnswer(t *testing.T) {
	looping, line := thinkingLoopBody("")
	p, w, continuations, sent := loopSpliceProxy(t, streamConfig(loopModeRecover), looping,
		func() (*http.Response, error) {
			return loopStreamAnswer(streamContinuation("/v1/chat/completions")), nil
		})
	record := loopRecoveryPost(t, p, w)

	if *continuations != 1 {
		t.Fatalf("upstream saw %d continuations, want exactly one restart", *continuations)
	}
	if !record.Loop || !record.LoopAborted || !record.LoopRecovered {
		t.Fatalf("record loop/aborted/recovered = %v/%v/%v, want all set", record.Loop, record.LoopAborted, record.LoopRecovered)
	}
	// The restart asks for the answer; the thought that looped is never handed back.
	if !strings.Contains(*sent, loopRestart) {
		t.Fatalf("the restart request carries no restart instruction:\n%s", *sent)
	}
	if strings.Contains(*sent, line) {
		t.Fatalf("the looping thought was re-sent to the model:\n%s", *sent)
	}
	out := w.Body.String()
	if !strings.Contains(out, streamContinuationText) {
		t.Fatal("the restarted answer never reached the client")
	}
	if count := strings.Count(out, "data: [DONE]"); count != 1 {
		t.Fatalf("%d done sentinels, want the response closed exactly once", count)
	}
	if strings.Contains(out, `"type":"message_start"`) {
		t.Fatal("the restart's own message identity reached the client")
	}
}

func TestProxyCutsAThinkingLoopOnceTheAnswerStarted(t *testing.T) {
	looping, _ := thinkingLoopBody("")
	// The same loop, but the answer has already begun. Restarting would append a
	// second answer to prose the client is reading, so the answer ends at the cut.
	p, w, continuations, _ := loopSpliceProxy(t, streamConfig(loopModeRecover),
		loopChatFrame("Here is the first part of the answer.\n")+looping,
		func() (*http.Response, error) {
			return loopStreamAnswer(streamContinuation("/v1/chat/completions")), nil
		})
	record := loopRecoveryPost(t, p, w)

	if *continuations != 0 {
		t.Fatalf("upstream saw %d continuations, want none once an answer exists", *continuations)
	}
	if !record.Loop || !record.LoopAborted {
		t.Fatalf("record loop/aborted = %v/%v, want the cut recorded", record.Loop, record.LoopAborted)
	}
	if record.LoopRecovered {
		t.Fatal("a thinking loop after the answer started was recorded as recovered")
	}
	out := w.Body.String()
	if strings.Contains(out, streamContinuationText) {
		t.Fatal("the client saw text from a restart that must not have been requested")
	}
	if !strings.Contains(out, "Here is the first part of the answer.") {
		t.Fatal("the answer text generated before the loop was dropped with it")
	}
	if count := strings.Count(out, "data: [DONE]"); count != 1 {
		t.Fatalf("%d done sentinels, want the cut closed exactly once", count)
	}
}

// rateLimitProxy builds a proxy whose transport answers chat completions through
// answer, recording the provider each attempt named. Background work is closed so
// a recovery run cannot race the assertions.
func rateLimitProxy(t *testing.T, cfg config, answer func(attempt int, provider string) *http.Response) (*proxy, *[]string) {
	t.Helper()
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.stopDailyBenchmarks()
	attempts := make([]string, 0, 4)
	var mu sync.Mutex
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return response(http.StatusInternalServerError, `{}`), nil
		}
		named := routingOnly(body)
		provider := ""
		if len(named) > 0 {
			provider = named[0]
		}
		mu.Lock()
		attempts = append(attempts, provider)
		attempt := len(attempts)
		mu.Unlock()
		return answer(attempt, provider), nil
	})
	return p, &attempts
}

func rateLimitedRequest(t *testing.T, ctx context.Context) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://proxy.test/v1/chat/completions",
		strings.NewReader(`{"model":"author/model","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// The default threshold makes the first provider-attributed 429 fail over, so a
// client that never retries — an agent subagent — receives an answer instead of a
// rate limit it would have to retry itself.
func TestDefaultRateLimitThresholdFailsOverOnTheFirst429(t *testing.T) {
	const model = "author/model"
	if defaultRateLimitFailoverThreshold != 1 {
		t.Fatalf("default 429 failover threshold = %d, want 1", defaultRateLimitFailoverThreshold)
	}
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"limited", "healthy"},
		UpdatedAt: time.Now(),
	}}
	p, attempts := rateLimitProxy(t, cfg, func(_ int, provider string) *http.Response {
		if provider == "limited" {
			return response(http.StatusTooManyRequests, rateLimitBody)
		}
		return response(http.StatusOK, `{"id":"ok"}`)
	})

	res := newMemoryResponseWriter()
	p.ServeHTTP(res, rateLimitedRequest(t, context.Background()))
	if res.Code != http.StatusOK {
		t.Fatalf("response = %d, want the failover answer, body %s", res.Code, res.Body.String())
	}
	if want := []string{"limited", "healthy"}; !equalSlices(*attempts, want) {
		t.Fatalf("provider attempts = %v, want %v", *attempts, want)
	}
}

// A raised threshold is still the client's own choice: it asks to see the first
// 429, so that response is forwarded without a replay.
func TestRaisedRateLimitThresholdForwardsTheFirst429(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.RateLimitFailoverThreshold = 2
	cfg.Models = map[string]providerConfig{model: {Order: []string{"only"}, UpdatedAt: time.Now()}}
	p, attempts := rateLimitProxy(t, cfg, func(_ int, _ string) *http.Response {
		return response(http.StatusTooManyRequests, rateLimitBody)
	})

	res := newMemoryResponseWriter()
	p.ServeHTTP(res, rateLimitedRequest(t, context.Background()))
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("response = %d, want the 429 the client asked to see", res.Code)
	}
	if len(*attempts) != 1 {
		t.Fatalf("provider attempts = %v, want no replay below the raised threshold", *attempts)
	}
}

// A rate-limited provider with nothing to fail over to is asked once more instead
// of handing the client a 429, and the successful replay is recorded as the plain
// success it was.
func TestRateLimitedProviderWithoutAlternativeIsRetriedForTheClient(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {Order: []string{"only"}, UpdatedAt: time.Now()}}
	p, attempts := rateLimitProxy(t, cfg, func(attempt int, _ string) *http.Response {
		if attempt == 1 {
			return response(http.StatusTooManyRequests, rateLimitBody)
		}
		return response(http.StatusOK, `{"id":"ok"}`)
	})

	res := newMemoryResponseWriter()
	p.ServeHTTP(res, rateLimitedRequest(t, context.Background()))
	if res.Code != http.StatusOK {
		t.Fatalf("response = %d, want the retried answer, body %s", res.Code, res.Body.String())
	}
	if len(*attempts) != 2 {
		t.Fatalf("provider attempts = %v, want one replay", *attempts)
	}
	snap := p.stats.snapshot()
	if !snap.HasLast {
		t.Fatal("the request was not recorded")
	}
	if snap.Last.Status != http.StatusOK || snap.Last.Err || snap.Last.RateLimited || snap.Last.ProviderFailureHandled {
		t.Fatalf("record = %+v, want a normal success", snap.Last)
	}
}

// One replay per request, not one per 429: a provider that answers 429 again is
// the answer the client receives, with the upstream body intact.
func TestRepeatedRateLimitIsForwardedToTheClient(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {Order: []string{"only"}, UpdatedAt: time.Now()}}
	p, attempts := rateLimitProxy(t, cfg, func(_ int, _ string) *http.Response {
		return response(http.StatusTooManyRequests, rateLimitBody)
	})

	res := newMemoryResponseWriter()
	p.ServeHTTP(res, rateLimitedRequest(t, context.Background()))
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("response = %d, want 429", res.Code)
	}
	if got := res.Body.String(); got != rateLimitBody {
		t.Fatalf("forwarded body = %q, want the upstream body", got)
	}
	if len(*attempts) != 2 {
		t.Fatalf("provider attempts = %v, want exactly one replay", *attempts)
	}
}

// An account-wide 429 names no endpoint, so no provider can be blamed or routed
// around; the burst is absorbed by one replay of the same request instead.
func TestAccountWideRateLimitIsRetriedOnce(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"first", "second"},
		UpdatedAt: time.Now(),
	}}
	p, attempts := rateLimitProxy(t, cfg, func(attempt int, _ string) *http.Response {
		if attempt == 1 {
			return response(http.StatusTooManyRequests, accountWide429Body)
		}
		return response(http.StatusOK, `{"id":"ok"}`)
	})
	p.routing.autoPin(model, "first")

	res := newMemoryResponseWriter()
	p.ServeHTTP(res, rateLimitedRequest(t, context.Background()))
	if res.Code != http.StatusOK {
		t.Fatalf("response = %d, want the retried answer, body %s", res.Code, res.Body.String())
	}
	if len(*attempts) != 2 {
		t.Fatalf("attempts = %v, want one replay on the same route", *attempts)
	}
	if provider := p.routing.pinnedProvider(model); provider != "first" {
		t.Fatalf("routed provider = %q, want the unattributed 429 not to fail over", provider)
	}
}

// A repeated account-wide 429 still reaches the client unchanged, and it is still
// not attributed to any provider.
func TestRepeatedAccountWideRateLimitReachesTheClient(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"first", "second"},
		UpdatedAt: time.Now(),
	}}
	p, attempts := rateLimitProxy(t, cfg, func(_ int, _ string) *http.Response {
		return response(http.StatusTooManyRequests, accountWide429Body)
	})
	p.routing.autoPin(model, "first")

	res := newMemoryResponseWriter()
	p.ServeHTTP(res, rateLimitedRequest(t, context.Background()))
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("response = %d, want 429", res.Code)
	}
	if got := res.Body.String(); got != accountWide429Body {
		t.Fatalf("forwarded body = %q, want the upstream body", got)
	}
	if len(*attempts) != 2 {
		t.Fatalf("attempts = %v, want exactly one replay", *attempts)
	}
	if provider := p.routing.pinnedProvider(model); provider != "first" {
		t.Fatalf("routed provider = %q, want the unattributed 429 not to fail over", provider)
	}
	snap := p.stats.snapshot()
	if !snap.HasLast || snap.Last.RateLimited || snap.Last.ProviderFailureHandled {
		t.Fatalf("record = %+v, want an unattributed 429 recorded against no provider", snap.Last)
	}
}

// A manual pin is the user's choice of provider, so a 429 on it is answered by the
// same provider: retried there, never rerouted.
func TestManualPinIsRetriedOnTheSameProvider(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {
		Order:     []string{"pinned", "other"},
		ManualPin: "pinned",
		UpdatedAt: time.Now(),
	}}
	p, attempts := rateLimitProxy(t, cfg, func(attempt int, _ string) *http.Response {
		if attempt == 1 {
			return response(http.StatusTooManyRequests, rateLimitBody)
		}
		return response(http.StatusOK, `{"id":"ok"}`)
	})

	res := newMemoryResponseWriter()
	p.ServeHTTP(res, rateLimitedRequest(t, context.Background()))
	if res.Code != http.StatusOK {
		t.Fatalf("response = %d, want the retried answer, body %s", res.Code, res.Body.String())
	}
	if want := []string{"pinned", "pinned"}; !equalSlices(*attempts, want) {
		t.Fatalf("provider attempts = %v, want %v", *attempts, want)
	}
	if provider, manual := p.routing.pinInfo(model); provider != "pinned" || !manual {
		t.Fatalf("pin = %q manual=%v, want the manual pin untouched", provider, manual)
	}
}

// Retry-After inside the ceiling is the wait that is taken.
func TestRateLimitRetryHonoursRetryAfterWithinCeiling(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {Order: []string{"only"}, UpdatedAt: time.Now()}}
	p, attempts := rateLimitProxy(t, cfg, func(attempt int, _ string) *http.Response {
		if attempt == 1 {
			resp := response(http.StatusTooManyRequests, rateLimitBody)
			resp.Header.Set("Retry-After", "1")
			return resp
		}
		return response(http.StatusOK, `{"id":"ok"}`)
	})

	started := time.Now()
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, rateLimitedRequest(t, context.Background()))
	elapsed := time.Since(started)
	if res.Code != http.StatusOK || len(*attempts) != 2 {
		t.Fatalf("response = %d after %v attempts, want the honoured replay", res.Code, len(*attempts))
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("replay after %v, want the advertised one second", elapsed)
	}
}

// A Retry-After beyond the ceiling closes the quota window: replaying inside it
// would spend the client's one allowed retry on a request the provider has
// already said it will refuse, so the 429 is forwarded immediately — no wait,
// and no second upstream request.
func TestRateLimitRetryForwardsALongRetryAfterWithoutReplay(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {Order: []string{"only"}, UpdatedAt: time.Now()}}
	p, attempts := rateLimitProxy(t, cfg, func(_ int, _ string) *http.Response {
		resp := response(http.StatusTooManyRequests, rateLimitBody)
		resp.Header.Set("Retry-After", "120")
		return resp
	})

	started := time.Now()
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, rateLimitedRequest(t, context.Background()))
	elapsed := time.Since(started)
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("response = %d, want 429", res.Code)
	}
	if got := res.Body.String(); got != rateLimitBody {
		t.Fatalf("forwarded body = %q, want the upstream 429 body", got)
	}
	if len(*attempts) != 1 {
		t.Fatalf("attempts = %v, want the 429 forwarded with no replay", *attempts)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("request took %v, want the long Retry-After not waited out", elapsed)
	}
}

// An automatic benchmark run skips providers that just exhausted their capacity,
// because every round would spend a request to be told the same thing. That skip
// lasts for the rate-limit cooldown only: once it lapses the provider is measured
// again, while a provider the model no longer serves stays skipped.
func TestAutomaticBenchmarkReprobesRateLimitedProviderAfterCooldown(t *testing.T) {
	const model = "author/model"
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {Order: []string{"limited", "gone", "healthy"}}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.setClock(func() time.Time { return now })
	p.stopDailyBenchmarks()
	var mu sync.Mutex
	probed := make(map[string]bool, 3)
	p.dailyTestProvider = func(_ context.Context, _ string, provider string, _ int) (providerTestSample, error) {
		mu.Lock()
		probed[provider] = true
		mu.Unlock()
		return pingSample(100, 10*time.Millisecond), nil
	}
	run := func() map[string]bool {
		t.Helper()
		if _, complete := p.benchmarkProviders(model, []string{"limited", "gone", "healthy"}, now.Add(time.Second), true, 3); !complete {
			t.Fatal("automatic benchmark did not complete inside its deadline")
		}
		mu.Lock()
		defer mu.Unlock()
		measured := probed
		probed = make(map[string]bool, 3)
		return measured
	}
	if _, accepted := p.routing.markProviderUnavailableAndFailover(model, "limited", true); !accepted {
		t.Fatal("rate-limit failure was not accepted")
	}
	if _, accepted := p.routing.markProviderUnavailableAndFailover(model, "gone", false); !accepted {
		t.Fatal("unavailability failure was not accepted")
	}

	if measured := run(); len(measured) != 1 || !measured["healthy"] {
		t.Fatalf("providers measured inside the cooldowns = %v, want the healthy provider alone", measured)
	}

	now = now.Add(rateLimitUnavailableCooldown + time.Second)
	if measured := run(); len(measured) != 2 || !measured["limited"] || !measured["healthy"] {
		t.Fatalf("providers measured after the rate-limit cooldown = %v, want limited and healthy", measured)
	}
}

func TestRateLimitRetryDelayHonoursRetryAfterWithinCeiling(t *testing.T) {
	if rateLimitRetryBackoff != 250*time.Millisecond {
		t.Fatalf("default 429 replay backoff = %v, want 250ms", rateLimitRetryBackoff)
	}
	if rateLimitRetryAfterCeiling != 3*time.Second {
		t.Fatalf("Retry-After ceiling = %v, want 3s", rateLimitRetryAfterCeiling)
	}
	now := time.Date(2026, time.November, 4, 12, 0, 0, 0, time.UTC)
	p, err := newProxy(testConfig("https://upstream.test/api/v1"))
	if err != nil {
		t.Fatal(err)
	}
	p.setClock(func() time.Time { return now })

	cases := []struct {
		name   string
		header string
		want   time.Duration
		// replay is whether the 429 is absorbed at all. Only a hint longer than
		// the ceiling closes the window; everything else is a burst.
		replay bool
	}{
		{name: "absent", header: "", want: rateLimitRetryBackoff, replay: true},
		{name: "zero", header: "0", want: 0, replay: true},
		{name: "one second", header: "1", want: time.Second, replay: true},
		{name: "at the ceiling", header: "3", want: rateLimitRetryAfterCeiling, replay: true},
		{name: "above the ceiling", header: "4", replay: false},
		{name: "quota window", header: "120", replay: false},
		{name: "negative", header: "-1", want: rateLimitRetryBackoff, replay: true},
		{name: "unparseable", header: "soon", want: rateLimitRetryBackoff, replay: true},
		{name: "http date inside the ceiling", header: now.Add(2 * time.Second).Format(http.TimeFormat), want: 2 * time.Second, replay: true},
		{name: "http date in the past", header: now.Add(-time.Minute).Format(http.TimeFormat), want: rateLimitRetryBackoff, replay: true},
		{name: "http date beyond the ceiling", header: now.Add(time.Minute).Format(http.TimeFormat), replay: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header := make(http.Header)
			if tc.header != "" {
				header.Set("Retry-After", tc.header)
			}
			got, replay := p.rateLimitRetryDelay(header)
			if replay != tc.replay {
				t.Fatalf("replay for %q = %v, want %v", tc.header, replay, tc.replay)
			}
			if got != tc.want {
				t.Fatalf("delay for %q = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

// Waiting is orr's convenience, so a client that has already given up is not held
// for a replay nobody will read.
func TestRateLimitRetryYieldsToTheClientContext(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {Order: []string{"only"}, UpdatedAt: time.Now()}}
	p, attempts := rateLimitProxy(t, cfg, func(_ int, _ string) *http.Response {
		resp := response(http.StatusTooManyRequests, rateLimitBody)
		resp.Header.Set("Retry-After", "3")
		return resp
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	res := newMemoryResponseWriter()
	p.ServeHTTP(res, rateLimitedRequest(t, ctx))
	elapsed := time.Since(started)
	if elapsed > time.Second {
		t.Fatalf("request took %v, want the wait abandoned with the client", elapsed)
	}
	if len(*attempts) != 1 {
		t.Fatalf("attempts = %v, want no replay for an abandoned request", *attempts)
	}
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("response = %d, want 429", res.Code)
	}
}

// An absorbed 429 must not outlive its own replay: the failover accounting marks
// the provider unavailable and drops its automatic pin before the bounded replay
// runs, and a replay that answers disproves exactly that conclusion.
//
// The clock is separated on purpose. The success-clear in observeProviderFailure
// compares the request start with the moment the mark was recorded and skips a
// request that started first, so on a coarse platform clock the mark would be
// cleared by accident; here the mark is recorded 20ms after the request started,
// which is what a real round trip looks like.
// A concurrent request can record a newer mark for the same provider while the
// replay is in flight. That verdict is evidence the replay does not contradict, so
// it must survive both the clear and the pin restore.
func TestAbsorbedReplayKeepsANewerConcurrentMark(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {Order: []string{"only"}, UpdatedAt: time.Now()}}

	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	move := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
	var holder *proxy
	p, attempts := rateLimitProxy(t, cfg, func(attempt int, _ string) *http.Response {
		if attempt == 1 {
			// The mark this request causes lands after the request began.
			move(20 * time.Millisecond)
			return response(http.StatusTooManyRequests, rateLimitBody)
		}
		// While the replay is in flight, another request records a newer mark.
		move(time.Second)
		if _, accepted := holder.routing.markProviderUnavailableAndFailover(model, "only", true); !accepted {
			t.Error("the concurrent mark was not accepted")
		}
		return response(http.StatusOK, `{"id":"ok"}`)
	})
	holder = p
	p.setClock(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})
	p.routing.autoPin(model, "only")
	p.stats.setAutoPin(model, "only")

	res := newMemoryResponseWriter()
	p.ServeHTTP(res, rateLimitedRequest(t, context.Background()))
	if res.Code != http.StatusOK || len(*attempts) != 2 {
		t.Fatalf("response = %d after %v attempts, want the retried answer", res.Code, len(*attempts))
	}
	if !p.routing.isTemporarilyUnavailable(model, "only") {
		t.Fatal("the replay's success cleared a mark recorded after it began")
	}
	if provider, manual := p.routing.pinInfo(model); provider == "only" && !manual {
		t.Fatal("the automatic pin was restored to a provider a newer mark excludes")
	}
}

func TestAbsorbedRateLimitClearsTheCondemnationItCaused(t *testing.T) {
	const model = "author/model"
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.Models = map[string]providerConfig{model: {Order: []string{"only"}, UpdatedAt: time.Now()}}

	now := time.Now()
	var clockMu sync.Mutex
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}
	p, attempts := rateLimitProxy(t, cfg, func(attempt int, _ string) *http.Response {
		if attempt == 1 {
			// The 429 arrives 20ms into the request, so the provider-failure
			// mark it records is later than the request start.
			advance(20 * time.Millisecond)
			return response(http.StatusTooManyRequests, rateLimitBody)
		}
		return response(http.StatusOK, `{"id":"ok"}`)
	})
	p.setClock(func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	})
	p.routing.autoPin(model, "only")
	p.stats.setAutoPin(model, "only")

	res := newMemoryResponseWriter()
	p.ServeHTTP(res, rateLimitedRequest(t, context.Background()))
	if res.Code != http.StatusOK || len(*attempts) != 2 {
		t.Fatalf("response = %d after %v attempts, want the retried answer", res.Code, len(*attempts))
	}
	if p.routing.isTemporarilyUnavailable(model, "only") {
		t.Fatal("an absorbed 429 still excludes the provider that answered the replay")
	}
	if provider, manual := p.routing.pinInfo(model); provider != "only" || manual {
		t.Fatalf("pin = %q manual=%v, want the automatic pin the accounting dropped restored", provider, manual)
	}
	if pin := p.stats.providerPinsSnapshot()[model]; pin.Provider != "only" {
		t.Fatalf("persisted pin = %q, want the restored automatic route", pin.Provider)
	}
}

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"gopkg.in/yaml.v3"
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
	routing := newRoutingState(models, time.Hour)
	providersPath := filepath.Join(t.TempDir(), "providers.yaml")
	routing.setProvidersPath(providersPath)
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
	data, err := os.ReadFile(providersPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "new/model:") {
		t.Fatalf("unknown model was not persisted:\n%s", data)
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
	// Test rounds are billed requests: they must be recorded into stats.
	snap := p.stats.snapshot()
	if snap.RequestCount != 2 || len(snap.Records) != 2 {
		t.Fatalf("stats request count = %d, records = %d, want 2 each", snap.RequestCount, len(snap.Records))
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
	p.routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
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
	p.routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
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
	if _, accepted := p.routing.markProviderUnavailableAndFailover(model, "limited"); !accepted {
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
	if _, accepted := p.routing.markProviderUnavailableAndFailover(model, "limited"); !accepted {
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

func TestDailyBenchmarkReleasesAtSeventyPercentAndCancelsRemainder(t *testing.T) {
	const model = "author/model"
	providers := []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9"}
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.Models = map[string]providerConfig{model: {Order: providers, UpdatedAt: time.Now()}}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
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
	p.dailyWG.Add(1)
	go func() {
		p.runDailyBenchmark(model, refreshDone, ready)
		close(done)
	}()

	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("daily gate did not release after 70% completed")
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

	close(blocked)
	final, _ := p.routing.modelConfig(model)
	if final.Order[0] != "p6" {
		t.Fatalf("final best = %q, want p6 from completed results", final.Order[0])
	}
	if provider, manual := p.routing.pinInfo(model); provider != "p6" || manual {
		t.Fatalf("daily benchmark pin = %q manual=%v, want automatic p6 pin", provider, manual)
	}
	if persisted := p.stats.providerPinsSnapshot()[model]; persisted.Provider != "p6" || persisted.PinnedAt.IsZero() {
		t.Fatalf("daily benchmark did not persist p6 auto pin: %#v", persisted)
	}
}

func TestAutomaticBenchmarkCompletionTargetsRoundUpToSeventyPercent(t *testing.T) {
	tests := []struct {
		providers int
		want      int
	}{
		{0, 0}, {1, 1}, {2, 2}, {3, 3}, {4, 3}, {7, 5}, {10, 7}, {20, 14},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("providers_%d", tt.providers), func(t *testing.T) {
			if got := benchmarkCompletionTarget(tt.providers, true); got != tt.want {
				t.Fatalf("automatic target = %d, want %d", got, tt.want)
			}
			if got := benchmarkCompletionTarget(tt.providers, false); got != tt.providers {
				t.Fatalf("manual target = %d, want all %d", got, tt.providers)
			}
		})
	}
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
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "providers.yaml")
	p.routing.setProvidersPath(path)
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
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved providersFile
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if order := saved.Models[model].Order; len(order) == 0 || order[0] != "winner" {
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
		t.Fatal("benchmark reported 70% completion after every worker was canceled")
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

func TestManualProviderBenchmarkTargetsEveryProvider(t *testing.T) {
	p, err := newProxy(testConfig("https://upstream.test/api/v1"))
	if err != nil {
		t.Fatal(err)
	}
	p.dailyTestProvider = func(_ context.Context, _, provider string, _ int) (providerTestSample, error) {
		return pingSample(100, time.Millisecond), nil
	}
	providers := []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9"}

	automatic, reachedTarget := p.benchmarkProviders("author/model", providers, time.Now().Add(time.Second), true, len(providers))
	if !reachedTarget || len(automatic) != 7 {
		t.Fatalf("automatic results = %d target=%v, want 7 at 70%%", len(automatic), reachedTarget)
	}
	manual, reachedTarget := p.benchmarkProviders("author/model", providers, time.Now().Add(time.Second), false, len(providers))
	if !reachedTarget || len(manual) != len(providers) {
		t.Fatalf("manual results = %d target=%v, want all %d", len(manual), reachedTarget, len(providers))
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
	// Every round is a billed request, failures included.
	if snap := p.stats.snapshot(); snap.RequestCount != 3 {
		t.Fatalf("recorded requests = %d, want 3", snap.RequestCount)
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

package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestToRegistryModel(t *testing.T) {
	entry := registryCatalogEntry{
		ID:            "deepseek/deepseek-v4.1-flash",
		Name:          "DeepSeek V4.1 Flash",
		ContextLength: 1048576,
		SupportedParameters: []string{
			"tools", "reasoning", "temperature",
		},
		Architecture: struct {
			InputModalities  []string `json:"input_modalities"`
			OutputModalities []string `json:"output_modalities"`
		}{
			InputModalities:  []string{"text", "image"},
			OutputModalities: []string{"text"},
		},
		Reasoning: struct {
			SupportedEfforts []string `json:"supported_efforts"`
			DefaultEffort    string   `json:"default_effort"`
		}{
			SupportedEfforts: []string{"low", "high", "max"},
			DefaultEffort:    "high",
		},
	}
	entry.TopProvider.MaxCompletionTokens = 384000

	model, ok := toRegistryModel(entry)
	if !ok {
		t.Fatal("expected entry to map")
	}
	if model["id"] != entry.ID {
		t.Errorf("id = %v, want %s", model["id"], entry.ID)
	}
	limit := model["limit"].(map[string]any)
	if limit["context"] != 1048576 {
		t.Errorf("limit.context = %v, want 1048576", limit["context"])
	}
	if limit["output"] != 384000 {
		t.Errorf("limit.output = %v, want 384000", limit["output"])
	}
	if model["tool_call"] != true {
		t.Errorf("tool_call = %v, want true", model["tool_call"])
	}
	if model["reasoning"] != true {
		t.Errorf("reasoning = %v, want true", model["reasoning"])
	}
	modalities := model["modalities"].(map[string]any)
	if got := modalities["input"].([]string); len(got) != 2 || got[0] != "text" || got[1] != "image" {
		t.Errorf("modalities.input = %v", got)
	}
	if got := model["support_efforts"].([]string); len(got) != 3 || got[0] != "low" {
		t.Errorf("support_efforts = %v", got)
	}
	if model["default_effort"] != "high" {
		t.Errorf("default_effort = %v, want high", model["default_effort"])
	}
}

func TestToRegistryModelDefaults(t *testing.T) {
	// No top_provider max_completion_tokens, no reasoning, no modalities.
	entry := registryCatalogEntry{ID: "x/y", Name: "X Y"}
	model, ok := toRegistryModel(entry)
	if !ok {
		t.Fatal("expected entry to map")
	}
	if _, present := model["limit"]; present {
		t.Error("unknown limits should be omitted rather than invented")
	}
	if model["tool_call"] != false || model["reasoning"] != false {
		t.Errorf("tool_call=%v reasoning=%v, want false/false", model["tool_call"], model["reasoning"])
	}
	if _, present := model["modalities"]; present {
		t.Error("modalities should be absent when no architecture data")
	}
	if _, present := model["support_efforts"]; present {
		t.Error("support_efforts should be absent when no reasoning data")
	}
}

func TestToRegistryModelEmptyID(t *testing.T) {
	if _, ok := toRegistryModel(registryCatalogEntry{ID: ""}); ok {
		t.Error("expected empty id to be skipped")
	}
}

func TestRegistryBaseURL(t *testing.T) {
	for listen, want := range map[string]string{
		"127.0.0.1:8787": "http://127.0.0.1:8787/v1",
		":8787":          "http://127.0.0.1:8787/v1",
		"0.0.0.0:8787":   "http://127.0.0.1:8787/v1",
		"[::]:8787":      "http://[::1]:8787/v1",
		"[::1]:8787":     "http://[::1]:8787/v1",
		"localhost:8787": "http://localhost:8787/v1",
	} {
		if got := registryBaseURL(listen); got != want {
			t.Errorf("registryBaseURL(%q) = %q, want %q", listen, got, want)
		}
	}
}

func TestRegistryRefreshAndFailureBackoff(t *testing.T) {
	for _, badBody := range []string{`{"data":[]}`, `{}`, `null`, `{"data":[{"id":" "}]}`, `invalid JSON`} {
		t.Run(badBody, func(t *testing.T) {
			now := time.Now()
			calls := 0
			body := `{"data":[{"id":"old"}]}`
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return response(http.StatusOK, body), nil
			})}
			reg := newModelRegistry(client, "https://example.com", "test", "localhost:8787", time.Minute)
			reg.now = func() time.Time { return now }
			request := func(want string) {
				t.Helper()
				rec := httptest.NewRecorder()
				reg.ServeJSON(rec, httptest.NewRequest(http.MethodGet, "/registry.json", nil))
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"`+want+`"`) {
					t.Fatalf("status=%d body=%s, want model %s", rec.Code, rec.Body.String(), want)
				}
			}
			request("old")
			now = now.Add(time.Minute)
			body = badBody
			request("old")
			request("old")
			if calls != 2 {
				t.Fatalf("calls = %d, want 2", calls)
			}
			now = now.Add(registryRetryDelay)
			body = `{"data":[{"id":"new"}]}`
			request("new")
			request("new")
			if calls != 3 {
				t.Fatalf("calls = %d, want 3", calls)
			}
		})
	}
}

func TestRegistryConcurrentColdFailure(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(http.StatusServiceUnavailable, "unavailable"), nil
	})}
	reg := newModelRegistry(client, "https://example.com", "test", "localhost:8787", time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			reg.ServeJSON(rec, httptest.NewRequest(http.MethodGet, "/registry.json", nil))
			if rec.Code != http.StatusBadGateway {
				t.Errorf("status = %d", rec.Code)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestRegistryCancelledFetchDoesNotBackoff(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		return response(http.StatusOK, `{"data":[{"id":"a/b"}]}`), nil
	})}
	reg := newModelRegistry(client, "https://example.com", "test", "localhost:8787", time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reg.documentFor(ctx); err == nil {
		t.Fatal("expected cancelled fetch to fail")
	}
	if _, err := reg.documentFor(context.Background()); err != nil {
		t.Fatalf("subsequent fetch: %v", err)
	}
}

func TestRegistryMethods(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(http.StatusOK, `{"data":[{"id":"a/b"}]}`), nil
	})}
	reg := newModelRegistry(client, "https://example.com", "test", "localhost:8787", time.Minute)
	rec := httptest.NewRecorder()
	reg.ServeJSON(rec, httptest.NewRequest(http.MethodPost, "/registry.json", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" || calls.Load() != 0 {
		t.Fatalf("POST status=%d allow=%q calls=%d", rec.Code, rec.Header().Get("Allow"), calls.Load())
	}
	rec = httptest.NewRecorder()
	reg.ServeJSON(rec, httptest.NewRequest(http.MethodHead, "/registry.json", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("HEAD status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestModelRegistryServeJSON(t *testing.T) {
	catalog := `{"data":[{"id":"deepseek/deepseek-v4.1-flash","name":"DeepSeek V4.1 Flash","context_length":1048576,"supported_parameters":["tools","reasoning"],"top_provider":{"max_completion_tokens":384000},"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},"reasoning":{"supported_efforts":["low","high","max"],"default_effort":"high"}}]}`
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.URL.Path != "/api/v1/models" {
			t.Errorf("unexpected path %q", req.URL.Path)
		}
		if auth := req.Header.Get("Authorization"); auth != "Bearer sk-test" {
			t.Errorf("Authorization = %q", auth)
		}
		return response(http.StatusOK, catalog), nil
	})}

	reg := newModelRegistry(client, "https://openrouter.ai/api/v1", "sk-test", "127.0.0.1:8787", time.Minute)

	// First request fetches.
	rec := httptest.NewRecorder()
	reg.ServeJSON(rec, httptest.NewRequest(http.MethodGet, "/registry.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var doc map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	provider, ok := doc["openrouter"]
	if !ok {
		t.Fatal("missing openrouter key")
	}
	if provider["api"] != "http://127.0.0.1:8787/v1" {
		t.Errorf("api = %v", provider["api"])
	}
	if provider["type"] != "openai" {
		t.Errorf("type = %v", provider["type"])
	}
	models := provider["models"].(map[string]any)
	if _, ok := models["deepseek/deepseek-v4.1-flash"]; !ok {
		t.Error("missing model entry")
	}
	if calls.Load() != 1 {
		t.Errorf("fetch calls = %d, want 1", calls.Load())
	}

	// Second request within TTL serves from cache.
	reg.ServeJSON(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/registry.json", nil))
	if calls.Load() != 1 {
		t.Errorf("fetch calls after cache = %d, want 1", calls.Load())
	}
}

func TestModelRegistryServeJSONStaleFallback(t *testing.T) {
	catalog := `{"data":[{"id":"a/b","name":"A B"}]}`
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return response(http.StatusOK, catalog), nil
		}
		return response(http.StatusInternalServerError, `{"error":"boom"}`), nil
	})}

	reg := newModelRegistry(client, "https://openrouter.ai/api/v1", "sk-test", "127.0.0.1:8787", time.Nanosecond)

	reg.ServeJSON(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/registry.json", nil))
	// TTL has elapsed, so the second request refetches and fails, but the stale
	// document is still served.
	rec := httptest.NewRecorder()
	reg.ServeJSON(rec, httptest.NewRequest(http.MethodGet, "/registry.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with stale fallback", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "a/b") {
		t.Errorf("stale body missing model: %s", rec.Body.String())
	}
}

func TestModelRegistryServeJSONFetchError(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return response(http.StatusInternalServerError, `{"error":"boom"}`), nil
	})}
	reg := newModelRegistry(client, "https://openrouter.ai/api/v1", "sk-test", "127.0.0.1:8787", time.Minute)
	rec := httptest.NewRecorder()
	reg.ServeJSON(rec, httptest.NewRequest(http.MethodGet, "/registry.json", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestProxyServeHTTPRegistryRoute(t *testing.T) {
	catalog := `{"data":[{"id":"a/b","name":"A B"}]}`
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return response(http.StatusOK, catalog), nil
	})}
	p := &proxy{
		cfg:      config{Listen: "127.0.0.1:8787"},
		registry: newModelRegistry(client, "https://openrouter.ai/api/v1", "sk-test", "127.0.0.1:8787", time.Minute),
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/registry.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), `"openrouter"`) {
		t.Errorf("response missing openrouter: %s", body)
	}
}

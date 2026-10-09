package e2e_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegistryEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			t.Errorf("upstream path = %q, want /api/v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek/deepseek-v4.1-flash","name":"DeepSeek V4.1 Flash","context_length":1048576,"supported_parameters":["tools","reasoning"],"top_provider":{"max_completion_tokens":384000},"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},"reasoning":{"supported_efforts":["low","high","max"],"default_effort":"high"}}]}`))
	}))
	defer upstream.Close()

	orr := startServer(t, upstream.URL+"/api/v1", "version: 1\nmodels: {}\n")

	response, err := http.Get(orr.baseURL + "/registry.json")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if ct := response.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var doc map[string]map[string]any
	if err := json.NewDecoder(response.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	provider, ok := doc["openrouter"]
	if !ok {
		t.Fatal("missing openrouter key")
	}
	if provider["api"] != orr.baseURL+"/v1" {
		t.Errorf("api = %v, want %s/v1", provider["api"], orr.baseURL)
	}
	if provider["type"] != "openai" {
		t.Errorf("type = %v", provider["type"])
	}
	models := provider["models"].(map[string]any)
	model, ok := models["deepseek/deepseek-v4.1-flash"]
	if !ok {
		t.Fatal("missing deepseek/deepseek-v4.1-flash")
	}
	entry := model.(map[string]any)
	limit := entry["limit"].(map[string]any)
	if limit["context"] != float64(1048576) {
		t.Errorf("context = %v", limit["context"])
	}
	if entry["tool_call"] != true || entry["reasoning"] != true {
		t.Errorf("tool_call=%v reasoning=%v", entry["tool_call"], entry["reasoning"])
	}
}

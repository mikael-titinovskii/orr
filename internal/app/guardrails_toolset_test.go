package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGuardrailCheckDeclaresToolsForToolCalls(t *testing.T) {
	var seen []guardrailCheckRequest
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var check guardrailCheckRequest
		if err := json.NewDecoder(r.Body).Decode(&check); err != nil {
			t.Errorf("decode: %v", err)
		}
		seen = append(seen, check)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"passed"}`))
	}))
	defer sidecar.Close()
	p := &proxy{
		cfg:              config{Guardrails: guardrailsConfig{Mode: guardrailsModeOn, Action: guardrailsActionBlock, URL: sidecar.URL}},
		guardrailsClient: &http.Client{},
	}
	messages := []guardrailMessage{{Role: "assistant"}}
	if _, err := p.guardrailCheck(context.Background(), "m", messages, []string{"tool_call"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.guardrailCheck(context.Background(), "m", messages, []string{"input"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("checks = %d, want 2", len(seen))
	}
	if len(seen[0].Tools) != 3 {
		t.Fatalf("tool_call check declared %d tools, want the three canonical classes", len(seen[0].Tools))
	}
	var names []string
	for _, declaration := range seen[0].Tools {
		function, _ := declaration["function"].(map[string]any)
		name, _ := function["name"].(string)
		names = append(names, name)
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"run_command", "read_file", "write_file"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("declared tools = %v, want %s", names, want)
		}
	}
	if len(seen[1].Tools) != 0 {
		t.Fatalf("an input check declared %d tools, want none", len(seen[1].Tools))
	}
}

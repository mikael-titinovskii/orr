package e2e_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Measurement follows the model a request names. Everything in providers.yaml is
// configured, but configured is not requested: a client that asks for one model
// must not pay for the provider pings of the others, and the model it did ask for
// must be measured without waiting on them.
func TestRequestMeasuresOnlyTheModelItNames(t *testing.T) {
	const (
		asked    = "author/model"
		unasked  = "author/other"
		pinGate  = 3 * time.Second
		graceFor = time.Second
	)
	askedProviders := []string{"alpha-1", "alpha-2"}
	unaskedProviders := []string{"beta-1", "beta-2"}

	var mu sync.Mutex
	pings := make(map[string]int)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// One catalog shape serves both models; the provider names carry the
			// model so the assertion below cannot confuse them.
			if strings.Contains(r.URL.Path, "other") {
				writeEndpointCatalog(w, unaskedProviders...)
				return
			}
			writeEndpointCatalog(w, askedProviders...)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		model, _ := body["model"].(string)
		provider := routingFromBody(body).Only[0]
		if _, benchmark := body["max_tokens"]; benchmark {
			mu.Lock()
			pings[model]++
			mu.Unlock()
			writeCompletion(w, provider)
			return
		}
		writeCompletion(w, provider)
	}))
	defer upstream.Close()

	providers := "version: 1\nmodels:\n" +
		"  " + asked + ":\n    order: [" + strings.Join(askedProviders, ", ") + "]\n    updated_at: 2000-01-01T00:00:00Z\n" +
		"  " + unasked + ":\n    order: [" + strings.Join(unaskedProviders, ", ") + "]\n    updated_at: 2000-01-01T00:00:00Z\n"
	orr := startServerWithEnv(t, upstream.URL+"/api/v1", providers, map[string]string{"OPENROUTER_API_KEY": "benchmark-key"})

	postModelCompletion(t, orr.baseURL, asked, "ask for one model")

	// Wait for the asked model's run to finish, then give the whole gate plus a
	// grace period for anything else to appear.
	deadline := time.Now().Add(pinGate + 3*time.Second)
	for {
		mu.Lock()
		got := pings[asked]
		mu.Unlock()
		if got >= len(askedProviders) {
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			defer mu.Unlock()
			t.Fatalf("the requested model was not measured: %v", pings)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(graceFor)

	mu.Lock()
	defer mu.Unlock()
	if pings[asked] < len(askedProviders) {
		t.Fatalf("asking for %q measured %d provider rounds, want at least one per provider: %v", asked, pings[asked], pings)
	}
	if got := pings[unasked]; got != 0 {
		t.Fatalf("%q was measured %d times although no request named it: %v", unasked, got, pings)
	}
}

func postModelCompletion(t *testing.T, baseURL, model, content string) {
	t.Helper()
	body := `{"model":` + string(mustJSON(t, model)) + `,"messages":[{"role":"user","content":` + string(mustJSON(t, content)) + `}]}`
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

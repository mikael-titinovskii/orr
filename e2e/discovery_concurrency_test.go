package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestUnknownModelPreservesFirstRoutingThenUsesDiscoveredProvider(t *testing.T) {
	releaseCatalog := make(chan struct{})
	requests := make(chan routedRequest, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			<-releaseCatalog
			writeEndpointCatalog(w, "discovered")
			return
		}
		requests <- decodeRouting(t, r)
		writeCompletion(w, "discovered")
	}))
	defer upstream.Close()

	orr := startServer(t, upstream.URL+"/api/v1", "version: 1\nmodels: {}\n")
	firstDone := make(chan error, 1)
	go func() {
		body := `{"model":"author/model","messages":[],"provider":{"only":["client-choice"]}}`
		firstDone <- sendCompletion(orr.baseURL, body)
	}()
	first := <-requests
	if len(first.Only) != 1 || first.Only[0] != "client-choice" {
		t.Fatalf("unknown model first routing = %#v, want client choice preserved", first)
	}
	close(releaseCatalog)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var latest []byte
	for {
		data, err := os.ReadFile(orr.providersPath)
		latest = data
		if err == nil && strings.Contains(string(data), "discovered") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("discovered provider was not persisted: %v\n%s\nprocess output:\n%s", err, latest, orr.output.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	postCompletion(t, orr.baseURL, "after discovery")
	// The first request keeps its client routing while discovery runs. Once the
	// discovered order exists, its head becomes a visible, strict auto-pin.
	assertStrictPin(t, <-requests, "discovered")
}

func TestConcurrentModelsKeepIndependentPinsAndValidProvidersFile(t *testing.T) {
	providers := `version: 1
models:
  author/one:
    order: [one-backup, one-pin]
    manual_pin: one-pin
    updated_at: UPDATED_AT
  author/two:
    order: [two-backup, two-pin]
    manual_pin: two-pin
    updated_at: UPDATED_AT
`
	type observed struct {
		model   string
		routing routedRequest
	}
	requests := make(chan observed, 24)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEndpointCatalog(w, "one-pin", "one-backup", "two-pin", "two-backup")
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		model, _ := body["model"].(string)
		requests <- observed{model, routingFromBody(body)}
		writeCompletion(w, strings.TrimPrefix(model, "author/")+"-pin")
	}))
	defer upstream.Close()

	orr := startServer(t, upstream.URL+"/api/v1", providers)
	const perModel = 12
	var wg sync.WaitGroup
	errors := make(chan error, perModel*2)
	start := make(chan struct{})
	for _, model := range []string{"author/one", "author/two"} {
		for range perModel {
			wg.Add(1)
			go func(model string) {
				defer wg.Done()
				<-start
				body := fmt.Sprintf(`{"model":%q,"messages":[]}`, model)
				errors <- sendCompletion(orr.baseURL, body)
			}(model)
		}
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	for range perModel * 2 {
		got := <-requests
		want := strings.TrimPrefix(got.model, "author/") + "-pin"
		assertStrictPin(t, got.routing, want)
	}

	data, err := os.ReadFile(orr.providersPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("providers file became invalid under concurrency: %v\n%s", err, data)
	}
}

func TestConcurrentUnknownModelsAreCreatedAtomicallyAndStayIsolated(t *testing.T) {
	type observed struct {
		model   string
		routing routedRequest
	}
	requests := make(chan observed, 20)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			provider := "one-provider"
			if strings.Contains(r.URL.Path, "/two/endpoints") {
				provider = "two-provider"
			}
			writeEndpointCatalog(w, provider)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		model, _ := body["model"].(string)
		requests <- observed{model, routingFromBody(body)}
		writeCompletion(w, strings.TrimPrefix(model, "author/")+"-provider")
	}))
	defer upstream.Close()

	orr := startServer(t, upstream.URL+"/api/v1", "version: 1\nmodels: {}\n")
	const perModel = 10
	var wg sync.WaitGroup
	errors := make(chan error, perModel*2)
	start := make(chan struct{})
	for _, model := range []string{"author/one", "author/two"} {
		for range perModel {
			wg.Add(1)
			go func(model string) {
				defer wg.Done()
				<-start
				errors <- sendCompletion(orr.baseURL, fmt.Sprintf(`{"model":%q,"messages":[]}`, model))
			}(model)
		}
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	for range perModel * 2 {
		got := <-requests
		want := strings.TrimPrefix(got.model, "author/") + "-provider"
		selected := got.routing.Only
		if len(selected) == 0 {
			selected = got.routing.Order
		}
		// Requests that raced model discovery preserve their client-supplied
		// routing (empty here). Once routing appears, it must belong to this
		// model and never leak the other model's provider.
		if len(selected) > 0 && (len(selected) != 1 || selected[0] != want) {
			t.Fatalf("model %q routing = %#v, want only its provider %q", got.model, got.routing, want)
		}
	}

	data, err := os.ReadFile(orr.providersPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Version int            `yaml:"version"`
		Models  map[string]any `yaml:"models"`
	}
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("providers file became invalid under concurrent creation: %v\n%s", err, data)
	}
	if decoded.Version != 1 || len(decoded.Models) != 2 || decoded.Models["author/one"] == nil || decoded.Models["author/two"] == nil {
		t.Fatalf("concurrent model creation produced unexpected file:\n%s", data)
	}
	waitForPersistedOrder(t, orr.providersPath, "one-provider")
	waitForPersistedOrder(t, orr.providersPath, "two-provider")
	for _, model := range []string{"author/one", "author/two"} {
		if err := sendCompletion(orr.baseURL, fmt.Sprintf(`{"model":%q,"messages":[]}`, model)); err != nil {
			t.Fatal(err)
		}
		got := <-requests
		want := strings.TrimPrefix(model, "author/") + "-provider"
		if got.model != model {
			t.Fatalf("received model %q, want %q", got.model, model)
		}
		assertStrictPin(t, got.routing, want)
	}
}

func sendCompletion(baseURL, body string) error {
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 8 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("completion status = %d", response.StatusCode)
	}
	return nil
}

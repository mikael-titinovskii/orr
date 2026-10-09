package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testManagementAPI(t *testing.T) *managementAPI {
	t.Helper()
	p, err := newProxy(config{Upstream: "https://example.invalid", OpenRouterAPIKey: "test-secret",
		PinTTL: time.Hour, Models: map[string]providerConfig{"author/model": {Order: []string{"first", "second"}}}})
	if err != nil {
		t.Fatal(err)
	}
	p.routing.setProvidersPath(filepath.Join(t.TempDir(), "providers.yaml"))
	a := &managementAPI{p: p}
	t.Cleanup(func() { a.stop(); a.wg.Wait() })
	return a
}

func apiRequest(a *managementAPI, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1/"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

func TestAPIResourcesWithoutClientCredentials(t *testing.T) {
	a := testManagementAPI(t)
	for _, key := range []string{"", "Bearer wrong", "test-secret"} {
		r := httptest.NewRequest("GET", "/api/v1/stats", nil)
		r.Header.Set("Authorization", key)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("WWW-Authenticate") != "" {
			t.Fatalf("local API must not require credentials: %d %s", w.Code, w.Body)
		}
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "stats", 200}, {"GET", "models", 200}, {"GET", "model?model=author/model", 200},
		{"GET", "model", 400}, {"GET", "model?model=author/missing", 404},
		{"GET", "model?model=author/model&model=author/model", 400},
		{"POST", "stats", 405}, {"GET", "unknown", 404}, {"GET", "jobs/missing", 404},
	} {
		w := apiRequest(a, tc.method, tc.path, "")
		if w.Code != tc.status {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
		if w.Header().Get("Cache-Control") != "no-store" || !json.Valid(w.Body.Bytes()) {
			t.Errorf("invalid response: %v %s", w.Header(), w.Body)
		}
		if tc.status == 405 && w.Header().Get("Allow") != "GET" {
			t.Error("missing Allow")
		}
		if strings.Contains(w.Body.String(), "test-secret") {
			t.Error("credential leaked")
		}
	}
	w := apiRequest(a, "GET", "stats", "")
	var s map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s["average_ttft_ms"] != nil || s["latency_p50_ms"] != nil {
		t.Fatalf("missing data must be null: %v", s)
	}
}

func TestAPIPinsPersistAndRestoreAutomaticWinner(t *testing.T) {
	a := testManagementAPI(t)
	a.p.routing.autoPin("author/model", "first")
	a.p.stats.setAutoPin("author/model", "first")
	w := apiRequest(a, "PUT", "pin?model=author/model", `{"provider":"second"}`)
	if w.Code != 200 {
		t.Fatalf("pin: %d %s", w.Code, w.Body)
	}
	data, err := os.ReadFile(a.p.routing.providersPath)
	if err != nil || !strings.Contains(string(data), "manual_pin: second") {
		t.Fatalf("persist: %s %v", data, err)
	}
	if pin, manual := a.p.routing.pinInfo("author/model"); pin != "second" || !manual {
		t.Fatalf("pin = %s %v", pin, manual)
	}
	for range 2 {
		w = apiRequest(a, "DELETE", "pin?model=author/model", "")
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		if pin, manual := a.p.routing.pinInfo("author/model"); pin != "first" || manual {
			t.Fatalf("restored pin = %s %v", pin, manual)
		}
	}
	data, err = os.ReadFile(a.p.routing.providersPath)
	if err != nil || strings.Contains(string(data), "manual_pin") {
		t.Fatalf("clear: %s %v", data, err)
	}
}

func TestAPIRejectsInvalidPinBodies(t *testing.T) {
	a := testManagementAPI(t)
	for _, body := range []string{"", "null", `{}`, `{"provider":"missing"}`, `{"provider":"first","extra":1}`, `{"provider":"first"} {}`, `{"provider":2}`} {
		if w := apiRequest(a, "PUT", "pin?model=author/model", body); w.Code != 400 {
			t.Errorf("body %q: %d", body, w.Code)
		}
	}
	w := apiRequest(a, "PUT", "pin?model=author/model", `{"provider":"`+strings.Repeat("a", 4096)+`"}`)
	if w.Code != 413 {
		t.Fatalf("oversize: %d", w.Code)
	}
	r := httptest.NewRequest("PUT", "/api/v1/pin?model=author/model", strings.NewReader(`{"provider":"first"}`))
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatalf("media type: %d", w.Code)
	}
	// Persistence failure leaves the active routing state unchanged.
	a.p.routing.setProvidersPath(t.TempDir())
	w = apiRequest(a, "PUT", "pin?model=author/model", `{"provider":"second"}`)
	if w.Code != 500 {
		t.Fatalf("persistence: %d %s", w.Code, w.Body)
	}
	if cfg, _ := a.p.routing.modelConfig("author/model"); cfg.ManualPin != "" {
		t.Fatal("failed pin committed")
	}
}

func TestAutomaticRestorationPreservesNewerManualPin(t *testing.T) {
	a := testManagementAPI(t)
	a.p.stats.setAutoPin("author/model", "first")
	if err := a.p.routing.setManualPin("author/model", "second"); err != nil {
		t.Fatal(err)
	}
	restoreAutomaticPin(a.p.routing, a.p.stats, "author/model")
	if provider, manual := a.p.routing.pinInfo("author/model"); provider != "second" || !manual {
		t.Fatalf("new manual choice lost: %s, %v", provider, manual)
	}
	if pin := a.p.stats.providerPinsSnapshot()["author/model"]; pin.Provider != "first" {
		t.Fatal("hidden automatic winner lost")
	}
}

func TestAPIJobsCoalesceAndDrain(t *testing.T) {
	a := testManagementAPI(t)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	a.p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		<-release
		return providerTestSample{}, errors.New("upstream secret body")
	}
	w := apiRequest(a, "POST", "benchmarks?model=author/model", "")
	if w.Code != 202 || w.Header().Get("Location") != "/api/v1/jobs/1" {
		t.Fatalf("submit: %d %s", w.Code, w.Body)
	}
	duplicate := apiRequest(a, "POST", "benchmarks?model=author/model", "")
	if duplicate.Code != 202 || duplicate.Header().Get("Location") != w.Header().Get("Location") {
		t.Fatal("duplicate not coalesced")
	}
	conflict := apiRequest(a, "POST", "refresh?model=author/model", "")
	if conflict.Code != 409 {
		t.Fatalf("conflict: %d", conflict.Code)
	}
	a.stop()
	if w := apiRequest(a, "POST", "benchmarks?model=author/model", ""); w.Code != 503 {
		t.Fatalf("shutdown admission: %d", w.Code)
	}
	close(release)
	a.wg.Wait()
	w = apiRequest(a, "GET", "jobs/1", "")
	var job apiJob
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Status != "failed" || job.FinishedAt == nil || job.Error == nil || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("job: %s", w.Body)
	}
}

func TestAPIRefreshJobsAndBoundedHistory(t *testing.T) {
	a := testManagementAPI(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()
	a.p.cfg.Upstream = upstream.URL
	for range 65 {
		if w := apiRequest(a, "POST", "refresh?model=author/model", ""); w.Code != 202 {
			t.Fatalf("submit: %d", w.Code)
		}
		a.wg.Wait()
	}
	if len(a.jobs) != 64 {
		t.Fatalf("history: %d", len(a.jobs))
	}
	if w := apiRequest(a, "GET", "jobs/1", ""); w.Code != 404 {
		t.Fatal("old job retained")
	}
	if a.jobs[63].Status != "failed" {
		t.Fatal("refresh failure reported as success")
	}
}

func TestAPISuccessfulJobsPreserveManualPin(t *testing.T) {
	a := testManagementAPI(t)
	if w := apiRequest(a, "PUT", "pin?model=author/model", `{"provider":"second"}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("refresh did not use the configured upstream key")
		}
		response := endpointList{}
		response.Data.Endpoints = []modelEndpoint{{Tag: "first", ProviderName: "First", Status: 0,
			SupportedParameters: []string{"tools", "tool_choice"}, Throughput: &percentiles{P50: 100}}}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer upstream.Close()
	a.p.cfg.Upstream = upstream.URL
	a.p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		return providerTestSample{tps: 100, ttft: time.Millisecond, latency: time.Second, samples: 3}, nil
	}
	for _, kind := range []string{"refresh", "benchmarks"} {
		w := apiRequest(a, "POST", kind+"?model=author/model", "")
		if w.Code != 202 {
			t.Fatalf("%s: %s", kind, w.Body)
		}
		a.wg.Wait()
		job := a.jobs[len(a.jobs)-1]
		if job.Status != "succeeded" {
			t.Fatalf("%s job: %+v", kind, job)
		}
		cfg, _ := a.p.routing.modelConfig("author/model")
		if cfg.ManualPin != "second" || !containsString(cfg.Order, "second") {
			t.Fatalf("lost manual pin: %+v", cfg)
		}
	}
}

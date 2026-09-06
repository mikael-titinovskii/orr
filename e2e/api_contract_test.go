package e2e_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPinnedRoutingAcrossSupportedGenerationAPIs(t *testing.T) {
	type captured struct {
		path string
		body map[string]any
	}
	requests := make(chan captured, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEndpointCatalog(w, "pinned")
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		requests <- captured{r.URL.Path, body}
		writeCompletion(w, "pinned")
	}))
	defer upstream.Close()

	providers := `version: 1
models:
  author/model:
    order: [pinned]
    manual_pin: pinned
    updated_at: UPDATED_AT
`
	orr := startServer(t, upstream.URL+"/api/v1", providers)
	tests := []struct {
		path string
		body string
	}{
		{"/v1/chat/completions", `{"model":"author/model","messages":[]}`},
		{"/v1/responses", `{"model":"author/model","input":"hello","include":["file_search_call.results"]}`},
		{"/v1/messages", `{"model":"author/model","messages":[]}`},
	}
	for _, test := range tests {
		response, err := http.Post(orr.baseURL+test.path, "application/json", strings.NewReader(test.body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", test.path, response.StatusCode)
		}
	}

	for _, test := range tests {
		got := <-requests
		if got.path != "/api"+test.path {
			t.Errorf("upstream path = %q, want %q", got.path, "/api"+test.path)
		}
		assertStrictPin(t, routingFromBody(got.body), "pinned")
		switch test.path {
		case "/v1/chat/completions":
			usage, _ := got.body["usage"].(map[string]any)
			if usage["include"] != true {
				t.Errorf("chat usage = %#v, want include=true", usage)
			}
		case "/v1/responses":
			include := stringSlice(got.body["include"])
			if len(include) != 2 || include[0] != "file_search_call.results" || include[1] != "usage" {
				t.Errorf("responses include = %v", include)
			}
		}
	}
}

func TestClientCancellationReachesUpstream(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEndpointCatalog(w, "preferred")
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	defer upstream.Close()

	orr := startServer(t, upstream.URL+"/api/v1", configuredProviders)
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, orr.baseURL+"/v1/chat/completions", strings.NewReader(`{"model":"author/model"}`))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach upstream")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("canceling client did not cancel upstream request")
	}
	if err := <-done; err == nil {
		t.Fatal("canceled client request unexpectedly succeeded")
	}
	releaseOnce.Do(func() { close(release) })
}

func TestProxyPreservesPassthroughResponseAndRequestMetadata(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" || r.URL.Query().Get("capability") != "tools" {
			t.Errorf("upstream URL = %s", r.URL)
		}
		if r.Header.Get("X-Test-Header") != "forward-me" {
			t.Errorf("request header was not forwarded")
		}
		w.Header().Set("X-Upstream-Header", "preserved")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "upstream body")
	}))
	defer upstream.Close()

	orr := startServer(t, upstream.URL+"/api/v1", "version: 1\nmodels: {}\n")
	request, _ := http.NewRequest(http.MethodGet, orr.baseURL+"/v1/models?capability=tools", nil)
	request.Header.Set("X-Test-Header", "forward-me")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusTeapot || response.Header.Get("X-Upstream-Header") != "preserved" || string(body) != "upstream body" {
		t.Fatalf("passthrough response status=%d header=%q body=%q", response.StatusCode, response.Header.Get("X-Upstream-Header"), body)
	}
}

func TestInvalidRequestsNeverReachUpstream(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	orr := startServer(t, upstream.URL+"/api/v1", "version: 1\nmodels: {}\n")

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		status int
	}{
		{"unsupported path", http.MethodGet, "/admin", "", http.StatusNotFound},
		{"dot segment", http.MethodGet, "/v1/%2e%2e/secret", "", http.StatusBadRequest},
		{"invalid JSON", http.MethodPost, "/v1/chat/completions", "{", http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(test.method, orr.baseURL+test.path, strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.status)
			}
		})
	}
	if calls := upstreamCalls.Load(); calls != 0 {
		t.Fatalf("invalid requests reached upstream %d times", calls)
	}
}

func TestOversizedGenerationRequestIsRejected(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	orr := startServer(t, upstream.URL+"/api/v1", "version: 1\nmodels: {}\n")

	const tooLarge = int64(64<<20 + 1)
	request, err := http.NewRequest(http.MethodPost, orr.baseURL+"/v1/chat/completions", io.LimitReader(repeatingReader('x'), tooLarge))
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = tooLarge
	client := &http.Client{Timeout: 60 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d, want 413", response.StatusCode)
	}
	if calls := upstreamCalls.Load(); calls != 0 {
		t.Fatalf("oversized request reached upstream %d times", calls)
	}
}

type repeatingReader byte

func (r repeatingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func TestEncodedDotSegmentIsRejectedWithoutClientNormalization(t *testing.T) {
	// Guard the URL shape used above: net/url must preserve its escaped form on
	// the wire while exposing the decoded path to the proxy's validation.
	u, err := url.Parse("http://example.test/v1/%2e%2e/secret")
	if err != nil || u.RawPath == "" || u.Path != "/v1/../secret" {
		t.Fatalf("unexpected encoded URL representation: %#v err=%v", u, err)
	}
}

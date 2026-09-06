package e2e_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const configuredProviders = `version: 1
models:
  author/model:
    order:
      - preferred
    updated_at: UPDATED_AT
`

func TestProxyInjectsRoutingAndForwardsCredentials(t *testing.T) {
	type capturedRequest struct {
		path          string
		authorization string
		body          map[string]any
	}
	captured := make(chan capturedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{"endpoints":[]}}`)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		captured <- capturedRequest{r.URL.Path, r.Header.Get("Authorization"), body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"response-id","model":"author/model","provider":"preferred","usage":{"prompt_tokens":2,"completion_tokens":3}}`)
	}))
	defer upstream.Close()

	orr := startServer(t, upstream.URL+"/api/v1", configuredProviders)
	body := `{"model":"author/model","messages":[{"role":"user","content":"private prompt"}],"provider":{"order":["client-choice"]}}`
	request, err := http.NewRequest(http.MethodPost, orr.baseURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer client-secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), `"response-id"`) {
		t.Fatalf("proxy response status=%d body=%s", response.StatusCode, responseBody)
	}

	got := <-captured
	if got.path != "/api/v1/chat/completions" {
		t.Errorf("upstream path = %q", got.path)
	}
	if got.authorization != "Bearer client-secret" {
		t.Errorf("authorization = %q", got.authorization)
	}
	provider, _ := got.body["provider"].(map[string]any)
	only, _ := provider["only"].([]any)
	if len(only) != 1 || only[0] != "preferred" || provider["allow_fallbacks"] != false {
		t.Errorf("provider routing = %#v, want strict preferred auto-pin", provider)
	}
	usage, _ := got.body["usage"].(map[string]any)
	if usage["include"] != true {
		t.Errorf("usage = %#v, want include=true", usage)
	}
	logs := orr.output.String()
	for _, secret := range []string{"client-secret", "private prompt"} {
		if strings.Contains(logs, secret) {
			t.Errorf("process logs leaked %q:\n%s", secret, logs)
		}
	}
}

func TestProxyStreamsWithoutWaitingForCompletion(t *testing.T) {
	firstSent := make(chan struct{})
	releaseSecond := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(releaseSecond) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{"endpoints":[]}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server does not support flushing")
			return
		}
		_, _ = io.WriteString(w, "data: first\n\n")
		flusher.Flush()
		close(firstSent)
		<-releaseSecond
		_, _ = io.WriteString(w, "data: second\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	orr := startServer(t, upstream.URL+"/api/v1", configuredProviders)
	request, err := http.NewRequest(http.MethodPost, orr.baseURL+"/v1/chat/completions", strings.NewReader(`{"model":"author/model","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	select {
	case <-firstSent:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not send its first event")
	}
	line := make(chan string, 1)
	go func() {
		value, _ := bufio.NewReader(response.Body).ReadString('\n')
		line <- value
	}()
	select {
	case got := <-line:
		if got != "data: first\n" {
			t.Fatalf("first streamed line = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy buffered the streaming response")
	}
	release.Do(func() { close(releaseSecond) })
}

func TestProxyStreamsLargeResponseCompletely(t *testing.T) {
	const chunk = "0123456789abcdef"
	const repetitions = 128 * 1024 // 2 MiB, substantially larger than inspection tail.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEndpointCatalog(w, "preferred")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for range repetitions {
			_, _ = io.WriteString(w, chunk)
		}
		_, _ = io.WriteString(w, "\ndata: {\"model\":\"author/model\",\"provider\":\"preferred\",\"usage\":{\"completion_tokens\":1}}\n\n")
	}))
	defer upstream.Close()

	orr := startServer(t, upstream.URL+"/api/v1", configuredProviders)
	response, err := http.Post(orr.baseURL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"author/model","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	count, err := io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	minimum := int64(len(chunk) * repetitions)
	if count <= minimum {
		t.Fatalf("streamed bytes = %d, want more than payload minimum %d", count, minimum)
	}
}

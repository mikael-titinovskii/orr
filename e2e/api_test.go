package e2e_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHeadlessManagementAPIPersistsPins(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"endpoints":[]}}`)
	}))
	defer upstream.Close()
	env := map[string]string{"OPENROUTER_API_KEY": "management-test-key"}
	s := startServerWithEnv(t, upstream.URL+"/api/v1", configuredProviders, env)
	client := &http.Client{Timeout: 5 * time.Second}
	for _, path := range []string{"/api/docs", "/api/openapi.json"} {
		response, err := client.Get(s.baseURL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("public docs %s: %d", path, response.StatusCode)
		}
	}
	request := func(method, path, body string) (int, string) {
		t.Helper()
		r, err := http.NewRequest(method, s.baseURL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(data)
	}
	if status, _ := request("GET", "/api/v1/stats", ""); status != 200 {
		t.Fatalf("local API without credentials: %d", status)
	}
	before, err := os.ReadFile(filepath.Join(s.tempDir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if status, body := request("PUT", "/api/v1/pin?model=author/model", `{"provider":"preferred"}`); status != 200 {
		t.Fatalf("pin: %d %s", status, body)
	}
	after, err := os.ReadFile(filepath.Join(s.tempDir, ".env"))
	if err != nil || string(before) != string(after) {
		t.Fatal("management API changed dotenv")
	}
	s = restartServer(t, s, upstream.URL+"/api/v1", env)
	if status, body := request("GET", "/api/v1/model?model=author/model", ""); status != 200 || !strings.Contains(body, `"manual_pin":"preferred"`) {
		t.Fatalf("restart pin: %d %s", status, body)
	}
	if status, body := request("GET", "/api/v1/stats", ""); status != 200 || !strings.Contains(body, `"session_request_count":0`) {
		t.Fatalf("management traffic counted as inference: %d %s", status, body)
	}
}

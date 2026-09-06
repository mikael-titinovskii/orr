package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServeRejectsIncompatiblePersistedStats(t *testing.T) {
	tempDir := t.TempDir()
	statsPath := filepath.Join(tempDir, "orr", "stats.json")
	if err := os.MkdirAll(filepath.Dir(statsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statsPath, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	providersPath := filepath.Join(tempDir, "providers.yaml")
	if err := os.WriteFile(providersPath, []byte("version: 1\nmodels: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(tempDir, ".env")
	env := fmt.Sprintf("ORR_LISTEN=%s\nORR_UPSTREAM=http://127.0.0.1:1/api/v1\nORR_PROVIDERS_FILE=%s\nORR_TUI=false\n", unusedAddress(t), providersPath)
	if err := os.WriteFile(envPath, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(orrBinary, "serve", "--env", envPath)
	command.Dir = tempDir
	command.Env = isolatedEnvironment(tempDir)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("serve accepted incompatible stats: %s", output)
	}
	if !strings.Contains(string(output), "unsupported version 99") {
		t.Fatalf("unexpected incompatible-stats error:\n%s", output)
	}
}

func TestRoutingRestoresUnexpiredAutomaticPin(t *testing.T) {
	testRestoredPin(t, time.Now(), "secondary")
}

func TestRoutingDropsExpiredAutomaticPin(t *testing.T) {
	testRestoredPin(t, time.Now().Add(-2*time.Hour), "preferred")
}

func TestRoutingManualPinSurvivesProcessRestartAndTinyTTL(t *testing.T) {
	providers := `version: 1
models:
  author/model:
    order: [preferred, manual]
    manual_pin: manual
    updated_at: UPDATED_AT
`
	requests := make(chan routedRequest, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEndpointCatalog(w, "preferred", "manual")
			return
		}
		requests <- decodeRouting(t, r)
		writeCompletion(w, "manual")
	}))
	defer upstream.Close()

	env := map[string]string{"ORR_PIN_TTL": "1ns"}
	orr := startServerWithEnv(t, upstream.URL+"/api/v1", providers, env)
	postCompletion(t, orr.baseURL, "before restart")
	assertStrictPin(t, <-requests, "manual")

	restarted := restartServer(t, orr, upstream.URL+"/api/v1", env)
	postCompletion(t, restarted.baseURL, "after restart")
	assertStrictPin(t, <-requests, "manual")
	data, err := os.ReadFile(restarted.providersPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "manual_pin: manual") {
		t.Fatalf("manual pin did not survive restart:\n%s", data)
	}
}

func testRestoredPin(t *testing.T, pinnedAt time.Time, want string) {
	t.Helper()
	requests := make(chan routedRequest, 1)
	prefetched := make(chan struct{})
	var signal sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEndpointCatalog(w, "preferred", "secondary")
			signal.Do(func() { close(prefetched) })
			return
		}
		requests <- decodeRouting(t, r)
		writeCompletion(w, want)
	}))
	defer upstream.Close()

	setup := func(tempDir string) {
		statsPath := filepath.Join(tempDir, "orr", "stats.json")
		if err := os.MkdirAll(filepath.Dir(statsPath), 0o700); err != nil {
			t.Fatal(err)
		}
		stats := map[string]any{
			"version": 1,
			"provider_pins": map[string]any{
				"author/model": map[string]any{"provider": "secondary", "pinned_at": pinnedAt},
			},
		}
		data, err := json.Marshal(stats)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statsPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	orr := startServerWithSetup(t, upstream.URL+"/api/v1", configuredProviders, nil, setup)
	select {
	case <-prefetched:
	case <-time.After(2 * time.Second):
		t.Fatal("orr did not fetch endpoints needed to resolve restored pin")
	}
	postCompletion(t, orr.baseURL, "restored pin")
	got := <-requests
	if want == "secondary" {
		assertStrictPin(t, got, want)
		return
	}
	assertStrictPin(t, got, want)
}

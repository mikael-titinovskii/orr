package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A fresh instance has to measure the whole field inside the three-second ping
// gate, while the first client request is already in flight: that request is
// answered from the stored order, the measurement replaces the pin afterwards,
// and a provider that has not answered when the gate closes is cancelled rather
// than waited for.
//
// Every fast provider must get its full round count. A completion share stops the
// run once most providers have reported, which leaves the slowest of them with
// rounds started and never finished — arrival counts catch that where completion
// counts do not, because a cancelled round still reaches the upstream.
//
// The catalog lists providers in reverse speed order, so the persisted order is
// [fast-0 … fast-9, slow] only when every fast provider was measured.
func TestFreshStartMeasuresEveryProviderInsideThePingGate(t *testing.T) {
	const (
		fastProviders = 10
		roundsPerProv = 3 // providerTestRounds
		gate          = 3 * time.Second
		slowHold      = gate + 2*time.Second
	)
	catalog := []string{"slow"}
	delays := make(map[string]time.Duration, fastProviders)
	for i := fastProviders - 1; i >= 0; i-- {
		name := fmt.Sprintf("fast-%d", i)
		catalog = append(catalog, name)
		// The field is split so a completion share has real casualties: eight
		// providers answer in 150ms, the last two need 1.2s. A run that stops once
		// most providers have reported cancels those two before they reach the
		// two-round quorum, so they are never measured — and, unmeasured, they
		// keep their catalog position behind the provider that never answers.
		if i < fastProviders-2 {
			delays[name] = 50 * time.Millisecond
			continue
		}
		delays[name] = 400 * time.Millisecond
	}
	ranked := make([]string, 0, fastProviders+1)
	for i := range fastProviders {
		ranked = append(ranked, fmt.Sprintf("fast-%d", i))
	}
	ranked = append(ranked, "slow")

	var mu sync.Mutex
	rounds := make(map[string]int, len(catalog))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEndpointCatalog(w, catalog...)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		provider := routingFromBody(body).Only[0]
		if _, benchmark := body["max_tokens"]; !benchmark {
			writeCompletion(w, provider)
			return
		}
		mu.Lock()
		rounds[provider]++
		mu.Unlock()
		if provider == "slow" {
			// Never answers inside the gate. The run must end without it, which
			// the persisted order proves: it can only be written once the run
			// returns.
			select {
			case <-r.Context().Done():
			case <-time.After(slowHold):
			}
			return
		}
		time.Sleep(delays[provider])
		writeCompletion(w, provider)
	}))
	defer upstream.Close()

	orr := startServerWithEnv(t, upstream.URL+"/api/v1", staleProviders(catalog...), map[string]string{"OPENROUTER_API_KEY": "benchmark-key"})
	started := time.Now()
	postCompletion(t, orr.baseURL, "first request")
	if elapsed := time.Since(started); elapsed > gate {
		t.Fatalf("client request waited %v behind the startup measurement", elapsed)
	}

	// The provider that never answers can only be last if every other provider
	// was measured: unmeasured providers keep their catalog position, and this
	// one comes first there.
	deadline := time.Now().Add(5 * time.Second)
	for {
		models := persistedModels(t, orr.dbPath)
		measured := false
		for _, model := range models {
			if order := model.Order; len(order) > 0 && order[len(order)-1] == "slow" && containsAll(order, "fast-8", "fast-9") {
				measured = true
				break
			}
		}
		if measured {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the never-answering provider was not pushed last, so the field was not fully measured: %+v", models)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if elapsed := time.Since(started); elapsed >= slowHold {
		t.Fatalf("run took %v, which is the slow provider's hold: the gate did not bound it", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range ranked[:fastProviders] {
		if got := rounds[name]; got != roundsPerProv {
			t.Fatalf("provider %q got %d of %d benchmark rounds: the run was cut before the field was measured (%v)",
				name, got, roundsPerProv, rounds)
		}
	}
}

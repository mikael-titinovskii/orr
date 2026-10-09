package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These fixtures declare expected decisions, not scores computed by the
// implementation. Prices are dollars per million tokens, quality scores are
// fractions, and error rates are percentages, matching the dashboard.
type autoSelectionSnapshot struct {
	Name              string `json:"name"`
	Why               string `json:"why"`
	Model             string `json:"model"`
	Incumbent         string `json:"incumbent"`
	ManualPin         string `json:"manual_pin"`
	QualityAgeMinutes int    `json:"quality_age_minutes"`
	Profile           *struct {
		Prompt     float64 `json:"prompt"`
		Cached     float64 `json:"cached"`
		Completion float64 `json:"completion"`
	} `json:"profile"`
	Providers []struct {
		Tag       string   `json:"tag"`
		Input     float64  `json:"input"`
		Output    float64  `json:"output"`
		Cache     float64  `json:"cache"`
		TPS       float64  `json:"tps"`
		TTFTms    float64  `json:"ttft_ms"`
		GPQA      *float64 `json:"gpqa"`
		Tau       *float64 `json:"tau"`
		ToolError *float64 `json:"tool_error"`
		JSONError *float64 `json:"json_error"`
		APIError  float64  `json:"api_error"`
		Blocked   bool     `json:"blocked"`
	} `json:"providers"`
	WantBest string `json:"want_best"`
	WantPin  string `json:"want_pin"`
}

func TestAutoSelectionSnapshots(t *testing.T) {
	paths, err := filepath.Glob("testdata/auto_selection*_snapshots.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("snapshot files: %v, err=%v", paths, err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var snapshots []autoSelectionSnapshot
		if err := json.Unmarshal(data, &snapshots); err != nil {
			t.Fatal(err)
		}
		for _, snapshot := range snapshots {
			t.Run(snapshot.Name, func(t *testing.T) {
				// Rotate and reverse the input to catch accidental dependence on
				// provider listing order. Each pass starts from the same pin state.
				for rotation := range len(snapshot.Providers) {
					for _, reverse := range []bool{false, true} {
						t.Run(fmt.Sprintf("rotation_%d/reverse_%t", rotation, reverse), func(t *testing.T) {
							testAutoSelectionSnapshot(t, snapshot, rotation, reverse)
						})
					}
				}
			})
		}
	}
}

func testAutoSelectionSnapshot(t *testing.T, snapshot autoSelectionSnapshot, rotation int, reverse bool) {
	t.Helper()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	model := snapshot.Model
	if model == "" {
		model = "author/model"
	}
	cfg := providerConfig{ManualPin: snapshot.ManualPin}
	var meta []endpointMeta
	var results []providerTestResult
	qualities := make(map[string]providerQuality)
	apiErrors := make(map[string]float64)
	for i := range len(snapshot.Providers) {
		index := (i + rotation) % len(snapshot.Providers)
		if reverse {
			index = len(snapshot.Providers) - 1 - index
		}
		p := snapshot.Providers[index]
		cfg.Order = append(cfg.Order, p.Tag)
		if p.Blocked {
			cfg.Blocked = append(cfg.Blocked, blockedProvider{Provider: p.Tag, DetectedAt: now})
		}
		meta = append(meta, endpointMeta{Tag: p.Tag, Pricing: testPricing(p.Input, p.Output, p.Cache)})
		// A blank measured throughput means no benchmark result exists.
		if p.TPS > 0 {
			results = append(results, benchmarkResult(p.Tag, p.TPS, time.Duration(p.TTFTms*float64(time.Millisecond))))
		}
		qualities[p.Tag] = providerQuality{GPQA: p.GPQA, Tau: p.Tau, ToolError: p.ToolError, StructuredError: p.JSONError}
		apiErrors[p.Tag] = p.APIError
	}
	routing := newRoutingState(map[string]providerConfig{model: cfg}, time.Hour)
	routing.now = func() time.Time { return now }
	routing.endpointCache[model] = meta
	routing.qualityCache[model] = providerQualityCache{values: qualities, fetchedAt: now.Add(-time.Duration(snapshot.QualityAgeMinutes) * time.Minute)}
	if snapshot.Incumbent != "" {
		routing.autoPin(model, snapshot.Incumbent)
	}
	profile := benchmarkReferenceProfile
	if snapshot.Profile != nil {
		profile = requestProfile{promptTokens: snapshot.Profile.Prompt, cachedTokens: snapshot.Profile.Cached, completionTokens: snapshot.Profile.Completion, observed: true}
	}
	// Reapplying the snapshot must stay stable rather than churn the pin.
	for pass := range 3 {
		best, updated, err := routing.applyProviderTestResultsAtRevision(model, results, profile, routing.modelRevision(model), apiErrors)
		if err != nil {
			t.Fatal(err)
		}
		pin, manual := routing.pinInfo(model)
		if best != snapshot.WantBest || pin != snapshot.WantPin || manual != (snapshot.ManualPin != "") {
			t.Fatalf("pass %d: best=%q pin=%q manual=%v; want best=%q pin=%q manual=%v: %s", pass, best, pin, manual, snapshot.WantBest, snapshot.WantPin, snapshot.ManualPin != "", snapshot.Why)
		}
		if snapshot.ManualPin != "" && updated {
			t.Fatal("automatic update reported under a manual pin")
		}
		if best != "" {
			order, _ := routing.modelConfig(model)
			if order.Order[0] != best {
				t.Fatalf("winner %q does not lead ranked order %v", best, order.Order)
			}
		}
	}
}

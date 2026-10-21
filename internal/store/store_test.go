package store_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mikael-titinovskii/orr/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orr.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orr.db")
	first, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	second, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer second.Close()
}

// A catalog refreshed at 01:30 at +03:00 is the previous day in UTC. The
// once-per-day refresh compares calendar days in the timestamp's own location,
// so the offset has to survive the round trip.
func TestUpdatedAtKeepsItsCalendarDay(t *testing.T) {
	s := openStore(t)
	zone := time.FixedZone("EAT", 3*60*60)
	refreshed := time.Date(2026, 5, 4, 1, 30, 0, 0, zone)
	if err := s.SaveModels(map[string]store.Model{
		"author/model": {Order: []string{"a"}, UpdatedAt: refreshed},
	}); err != nil {
		t.Fatalf("save models: %v", err)
	}
	got, err := s.Models()
	if err != nil {
		t.Fatalf("read models: %v", err)
	}
	updated := got["author/model"].UpdatedAt
	if day := updated.Format(time.DateOnly); day != "2026-05-04" {
		t.Fatalf("updated_at day = %s, want 2026-05-04 (%v)", day, updated)
	}
	if !updated.Equal(refreshed) {
		t.Fatalf("updated_at = %v, want the same instant as %v", updated, refreshed)
	}
}

func TestModelsRoundTrip(t *testing.T) {
	s := openStore(t)
	enabled := true
	disabled := false
	updated := time.Date(2026, 5, 4, 3, 2, 1, 0, time.UTC)

	want := map[string]store.Model{
		"moonshotai/kimi-k3": {
			Order:             []string{"fireworks", "deepinfra"},
			Only:              []string{"fireworks"},
			Ignore:            []string{"inflection"},
			AllowFallbacks:    &disabled,
			RequireParameters: &enabled,
			DataCollection:    "deny",
			ZDR:               &enabled,
			Sort:              "throughput",
			ManualPin:         "deepinfra",
			UpdatedAt:         updated,
			Blocked: []store.Blocked{
				{Provider: "azure", Reason: "no_zdr", DetectedAt: updated},
			},
		},
		"anthropic/claude-sonnet-5": {},
	}
	if err := s.SaveModels(want); err != nil {
		t.Fatalf("save models: %v", err)
	}

	got, err := s.Models()
	if err != nil {
		t.Fatalf("read models: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("models = %d entries, want %d", len(got), len(want))
	}
	for model, expected := range want {
		compareModel(t, model, got[model], expected)
	}
}

func compareModel(t *testing.T, model string, got, want store.Model) {
	t.Helper()
	if !reflect.DeepEqual(got.Order, want.Order) {
		t.Errorf("%s order = %v, want %v", model, got.Order, want.Order)
	}
	if !reflect.DeepEqual(got.Only, want.Only) {
		t.Errorf("%s only = %v, want %v", model, got.Only, want.Only)
	}
	if !reflect.DeepEqual(got.Ignore, want.Ignore) {
		t.Errorf("%s ignore = %v, want %v", model, got.Ignore, want.Ignore)
	}
	if !equalBool(got.AllowFallbacks, want.AllowFallbacks) {
		t.Errorf("%s allow_fallbacks = %v, want %v", model, got.AllowFallbacks, want.AllowFallbacks)
	}
	if !equalBool(got.RequireParameters, want.RequireParameters) {
		t.Errorf("%s require_parameters = %v, want %v", model, got.RequireParameters, want.RequireParameters)
	}
	if !equalBool(got.ZDR, want.ZDR) {
		t.Errorf("%s zdr = %v, want %v", model, got.ZDR, want.ZDR)
	}
	if got.DataCollection != want.DataCollection {
		t.Errorf("%s data_collection = %q, want %q", model, got.DataCollection, want.DataCollection)
	}
	if got.Sort != want.Sort {
		t.Errorf("%s sort = %q, want %q", model, got.Sort, want.Sort)
	}
	if got.ManualPin != want.ManualPin {
		t.Errorf("%s manual_pin = %q, want %q", model, got.ManualPin, want.ManualPin)
	}
	if !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("%s updated_at = %v, want %v", model, got.UpdatedAt, want.UpdatedAt)
	}
	if len(got.Blocked) != len(want.Blocked) {
		t.Fatalf("%s blocked = %v, want %v", model, got.Blocked, want.Blocked)
	}
	for i := range want.Blocked {
		if got.Blocked[i].Provider != want.Blocked[i].Provider ||
			got.Blocked[i].Reason != want.Blocked[i].Reason ||
			!got.Blocked[i].DetectedAt.Equal(want.Blocked[i].DetectedAt) {
			t.Errorf("%s blocked[%d] = %+v, want %+v", model, i, got.Blocked[i], want.Blocked[i])
		}
	}
}

func equalBool(left, right *bool) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func TestSaveModelsReplaces(t *testing.T) {
	s := openStore(t)
	if err := s.SaveModels(map[string]store.Model{
		"a/one": {Order: []string{"x"}},
		"b/two": {Order: []string{"y"}},
	}); err != nil {
		t.Fatalf("save models: %v", err)
	}
	if err := s.SaveModels(map[string]store.Model{"a/one": {Order: []string{"z"}}}); err != nil {
		t.Fatalf("replace models: %v", err)
	}
	got, err := s.Models()
	if err != nil {
		t.Fatalf("read models: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("models = %v, want only a/one", got)
	}
	if !reflect.DeepEqual(got["a/one"].Order, []string{"z"}) {
		t.Errorf("order = %v, want [z]", got["a/one"].Order)
	}
}

func TestStatsRoundTrip(t *testing.T) {
	s := openStore(t)
	weight := 0.4
	degradation := false
	moment := time.Date(2026, 5, 4, 3, 2, 1, 123456789, time.UTC)

	want := store.Stats{
		DailyCosts:    map[string]float64{"2026-05-04": 1.25},
		DailyTokens:   map[string]int64{"2026-05-04": 4096},
		RequestCount:  17,
		CreditSpend:   2.5,
		CreditMonth:   "2026-05",
		Budget:        10,
		BudgetReset:   "2026-06-01T00:00:00Z",
		ProviderPins:  map[string]store.Pin{"moonshotai/kimi-k3": {Provider: "deepinfra", PinnedAt: moment}},
		BenchmarkRuns: map[string]time.Time{"moonshotai/kimi-k3": moment},
		RecoveryRuns:  map[string]time.Time{"anthropic/claude-sonnet-5": moment},
		TimeWeight:    &weight,
		Degradation:   &degradation,
		BenchmarkSamples: map[string]map[string][]store.Sample{
			"moonshotai/kimi-k3": {
				"deepinfra": {{Time: moment, TPS: 55.5, TTFTMs: 240}},
			},
		},
	}
	if err := s.SaveStats(want); err != nil {
		t.Fatalf("save stats: %v", err)
	}
	got, err := s.LoadStats()
	if err != nil {
		t.Fatalf("load stats: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
}

func TestLoadStatsEmptyYieldsUsableMaps(t *testing.T) {
	s := openStore(t)
	got, err := s.LoadStats()
	if err != nil {
		t.Fatalf("load stats: %v", err)
	}
	if got.DailyCosts == nil || got.ProviderPins == nil || got.BenchmarkSamples == nil {
		t.Fatalf("empty stats returned nil maps: %+v", got)
	}
	if got.TimeWeight != nil || got.Degradation != nil {
		t.Fatalf("untoggled state = %+v, want nil pointers", got)
	}
}

func TestImportLegacyStats(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "stats.json")
	legacy := map[string]any{
		"version":       1,
		"daily_costs":   map[string]float64{"2026-05-04": 0.5},
		"request_count": 3,
		"credit_spend":  1.5,
		"credit_month":  "2026-05",
		"benchmark_runs": map[string]time.Time{
			"moonshotai/kimi-k3": time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC),
		},
		"benchmark_samples": map[string]map[string][]map[string]any{
			"moonshotai/kimi-k3": {"deepinfra": {{"t": time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC), "tps": 10.0, "ttft_ms": 100.0}}},
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("encode legacy stats: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write legacy stats: %v", err)
	}

	s := openStore(t)
	imported, err := s.ImportLegacyStats(path)
	if err != nil {
		t.Fatalf("import legacy stats: %v", err)
	}
	if !imported {
		t.Fatal("import reported nothing imported")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("legacy stats still present after import: %v", err)
	}
	if _, err := os.Stat(path + ".imported"); err != nil {
		t.Fatalf("imported stats missing: %v", err)
	}

	stats, err := s.LoadStats()
	if err != nil {
		t.Fatalf("load stats: %v", err)
	}
	if stats.RequestCount != 3 || stats.CreditSpend != 1.5 {
		t.Fatalf("imported stats = %+v", stats)
	}
	if len(stats.BenchmarkSamples["moonshotai/kimi-k3"]["deepinfra"]) != 1 {
		t.Fatalf("imported samples = %+v", stats.BenchmarkSamples)
	}
}

func TestImportLegacyStatsReplacesExistingBackup(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "stats.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"request_count":4}`), 0o600); err != nil {
		t.Fatalf("write legacy stats: %v", err)
	}
	// A previous import left its own backup behind; Windows refuses to rename
	// over it, which must not stop this import from completing.
	if err := os.WriteFile(path+".imported", []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatalf("write stale backup: %v", err)
	}

	s := openStore(t)
	imported, err := s.ImportLegacyStats(path)
	if err != nil {
		t.Fatalf("import legacy stats: %v", err)
	}
	if !imported {
		t.Fatal("import reported nothing imported")
	}
	data, err := os.ReadFile(path + ".imported")
	if err != nil {
		t.Fatalf("read imported backup: %v", err)
	}
	if !strings.Contains(string(data), `"request_count":4`) {
		t.Fatalf("backup is not the imported file: %s", data)
	}
}

func TestImportLegacyStatsSkipsPopulatedDatabase(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "stats.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"request_count":99}`), 0o600); err != nil {
		t.Fatalf("write legacy stats: %v", err)
	}
	s := openStore(t)
	if err := s.SaveStats(store.Stats{RequestCount: 5}); err != nil {
		t.Fatalf("save stats: %v", err)
	}

	imported, err := s.ImportLegacyStats(path)
	if err != nil {
		t.Fatalf("import legacy stats: %v", err)
	}
	if imported {
		t.Fatal("import overwrote a populated database")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("legacy stats should be left in place: %v", err)
	}
	stats, err := s.LoadStats()
	if err != nil {
		t.Fatalf("load stats: %v", err)
	}
	if stats.RequestCount != 5 {
		t.Fatalf("request count = %d, want 5", stats.RequestCount)
	}
}

func TestImportLegacyStatsRejectsUnknownVersion(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "stats.json")
	if err := os.WriteFile(path, []byte(`{"version":7}`), 0o600); err != nil {
		t.Fatalf("write legacy stats: %v", err)
	}
	s := openStore(t)
	if _, err := s.ImportLegacyStats(path); err == nil {
		t.Fatal("import accepted an unsupported version")
	}
}

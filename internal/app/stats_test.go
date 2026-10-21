package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The startup warm-up asks a different question from the daily schedule: not
// "did it run today" but "is what this process holds recent". A run ten minutes
// old still counts as today, and that is exactly the case the warm-up must not
// skip after a restart.
func TestBenchmarkedWithinUsesTheWarmWindow(t *testing.T) {
	now := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.Local)
	s := newStats()
	s.now = func() time.Time { return now }
	if s.benchmarkedWithin("author/model", now, 5*time.Minute) {
		t.Fatal("a model that never ran counted as recently benchmarked")
	}
	s.markBenchmarkRun("author/model", now.Add(-4*time.Minute))
	if !s.benchmarkedWithin("author/model", now, 5*time.Minute) {
		t.Fatal("a run four minutes old did not count as recent")
	}
	s.markBenchmarkRun("author/model", now.Add(-10*time.Minute))
	if s.benchmarkedWithin("author/model", now, 5*time.Minute) {
		t.Fatal("a run ten minutes old counted as recent, which is the daily rule the warm-up exists to avoid")
	}
	if !s.benchmarkedWithin("author/model", now, 15*time.Minute) {
		t.Fatal("a run ten minutes old did not count inside a fifteen-minute window")
	}
	// A latch ahead of the wall clock — a stats file written before the clock
	// was stepped back — is stale, not fresh.
	s.markBenchmarkRun("author/model", now.Add(time.Hour))
	if s.benchmarkedWithin("author/model", now, 5*time.Minute) {
		t.Fatal("a future latch counted as a fresh run")
	}
}

func TestReplaceFileFailurePreservesDestination(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "stats.json")
	if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(filepath.Join(dir, "missing"), destination); err == nil {
		t.Fatal("replacement unexpectedly succeeded")
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "old" {
		t.Fatalf("destination changed after failed replacement: %q", data)
	}
}

func TestStatsRecordAndSnapshot(t *testing.T) {
	now := time.Now()
	s := newStats()
	s.record(requestRecord{Time: now, Status: 200, Duration: 2 * time.Second, TTFT: 500 * time.Millisecond, PromptTokens: 125, CompletionTokens: 75, Cost: .25})
	s.record(requestRecord{Time: now, Status: 500, Duration: 4 * time.Second, Err: true, PromptTokens: 25, CompletionTokens: 10, Cost: .75})
	snap := s.snapshot()
	if snap.RequestCount != 2 || snap.SessionRequestCount != 2 || len(snap.Records) != 2 {
		t.Fatalf("unexpected counts: %#v", snap)
	}
	if got := snap.DailyCosts[now.Local().Format(time.DateOnly)]; got != 1 {
		t.Fatalf("daily cost = %v", got)
	}
	if got := snap.DailyTokens[now.Local().Format(time.DateOnly)]; got != 235 {
		t.Fatalf("daily tokens = %d, want 235", got)
	}
	if snap.LatencyP50 != 2*time.Second || snap.LatencyP95 != 4*time.Second {
		t.Fatalf("unexpected performance snapshot: %#v", snap)
	}
	if snap.Diagnostics.Windows[0].Total != 2 || snap.Diagnostics.Windows[0].APIErrors != 1 {
		t.Fatalf("unexpected diagnostics snapshot: %#v", snap.Diagnostics)
	}
	if snap.Last.Status != 500 {
		t.Fatalf("last record = %#v", snap.Last)
	}
}

func TestRequestMeasurementFormulasAndBoundaries(t *testing.T) {
	record := requestRecord{
		Duration:         2500 * time.Millisecond,
		TTFT:             500 * time.Millisecond,
		PromptTokens:     400,
		CompletionTokens: 100,
		CachedTokens:     300,
		ReasoningTokens:  25,
	}
	if got := record.tokensPerSecond(); got != 50 {
		t.Fatalf("tokens/s = %v, want 100 tokens / 2 generation seconds = 50", got)
	}
	if got := record.cachePercent(); got != 75 {
		t.Fatalf("cache percent = %v, want 75", got)
	}
	if got := record.thinkingPercent(); got != 25 {
		t.Fatalf("thinking percent = %v, want 25", got)
	}

	noGeneration := record
	noGeneration.TTFT = noGeneration.Duration
	if got := noGeneration.tokensPerSecond(); got != 0 {
		t.Fatalf("zero generation window produced %v tokens/s", got)
	}
	tooMuchReasoning := record
	tooMuchReasoning.ReasoningTokens = 200
	if got := tooMuchReasoning.thinkingPercent(); got != 100 {
		t.Fatalf("thinking percent was not clamped: %v", got)
	}

	values := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second}
	if got := percentile(values, .50); got != 2*time.Second {
		t.Fatalf("P50 = %v, want 2s", got)
	}
	if got := percentile(values, .95); got != 4*time.Second {
		t.Fatalf("P95 = %v, want 4s", got)
	}
}

func TestStatsInFlight(t *testing.T) {
	s := newStats()
	s.beginRequest()
	s.beginRequest()
	if got := s.snapshot().InFlight; got != 2 {
		t.Fatalf("in-flight = %d, want 2", got)
	}
	s.endRequest()
	if got := s.snapshot().InFlight; got != 1 {
		t.Fatalf("in-flight = %d, want 1", got)
	}
	s.endRequest()
	if got := s.snapshot().InFlight; got != 0 {
		t.Fatalf("in-flight = %d, want 0", got)
	}
	s.endRequest()
	if got := s.snapshot().InFlight; got != 0 {
		t.Fatalf("in-flight went negative = %d", got)
	}
}

func TestStatsSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orr", "stats.json")
	now := time.Now()
	s := newStats()
	s.record(requestRecord{Time: now.AddDate(0, 0, -2), Status: 200, PromptTokens: 1_000_000, CompletionTokens: 250_000, Cost: 1.25})
	s.record(requestRecord{Time: now.AddDate(0, 0, -1), Status: 200, PromptTokens: 2_000_000, CompletionTokens: 500_000, Cost: 2.50})
	s.record(requestRecord{Time: now, Status: 200, PromptTokens: 3_000_000, CompletionTokens: 750_000, Cost: 3.75})
	if err := s.save(path); err != nil {
		t.Fatal(err)
	}
	loaded := newStats()
	if err := loaded.load(path); err != nil {
		t.Fatal(err)
	}
	snap := loaded.snapshot()
	if snap.RequestCount != 3 || snap.SessionRequestCount != 0 {
		t.Fatalf("round-trip counts failed: %#v", snap)
	}
	for _, want := range []struct {
		time   time.Time
		cost   float64
		tokens int64
	}{
		{now.AddDate(0, 0, -2), 1.25, 1_250_000},
		{now.AddDate(0, 0, -1), 2.50, 2_500_000},
		{now, 3.75, 3_750_000},
	} {
		day := want.time.Local().Format(time.DateOnly)
		if got := snap.DailyCosts[day]; got != want.cost {
			t.Fatalf("daily cost for %s = %v, want %v: %#v", day, got, want.cost, snap.DailyCosts)
		}
		if got := snap.DailyTokens[day]; got != want.tokens {
			t.Fatalf("daily tokens for %s = %d, want %d: %#v", day, got, want.tokens, snap.DailyTokens)
		}
	}
}

// The speed/price exponent is a dashboard preference, so it has to survive a
// restart. Zero is the trap: it means "price alone" and is a real setting, so a
// plain omitempty field would drop it and silently restore the default.
func TestTimeWeightRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orr", "stats.json")
	s := newStats()
	s.setTimeWeight(benchmarkTimeWeightMin)
	if err := s.save(path); err != nil {
		t.Fatal(err)
	}
	loaded := newStats()
	if err := loaded.load(path); err != nil {
		t.Fatal(err)
	}
	if got := loaded.currentTimeWeight(); got != benchmarkTimeWeightMin {
		t.Fatalf("restored weight = %v, want %v", got, benchmarkTimeWeightMin)
	}
}

// The degradation switch follows the same rule as the speed/price stop, with
// two additions: "off" is a real setting, and the value is only written once the
// user has toggled it, so ORR_DEGRADATION can keep seeding every start until
// then.
func TestDegradationRoundTripAndSeeding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orr", "stats.json")
	// A process that never pressed `d` must not freeze the seeded value into the
	// stats file, or the environment variable could never seed it again.
	untouched := newStats()
	if err := untouched.save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "degradation") {
		t.Fatalf("an untouched switch was persisted: %s", data)
	}
	seeded := newStats()
	if err := seeded.load(path); err != nil {
		t.Fatal(err)
	}
	seeded.seedDegradation(false)
	if seeded.degradationEnabled() {
		t.Fatal("configured default did not seed a state the user never toggled")
	}

	// After a press, the saved switch is the last word.
	pressed := newStats()
	pressed.setDegradation(false)
	if err := pressed.save(path); err != nil {
		t.Fatal(err)
	}
	loaded := newStats()
	if err := loaded.load(path); err != nil {
		t.Fatal(err)
	}
	if loaded.degradationEnabled() {
		t.Fatal("saved off switch restored as on")
	}
	loaded.seedDegradation(true)
	if loaded.degradationEnabled() {
		t.Fatal("configured default overrode a saved toggle")
	}

	fresh := newStats()
	if !fresh.degradationEnabled() {
		t.Fatal("a fresh state does not start the watch on")
	}
}

// A file written before the field existed, and a hand-edited value outside what
// the slider can express, both land on a position the control can show.
func TestTimeWeightLoadDefaultsAndClamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	for _, test := range []struct {
		name string
		body string
		want float64
	}{
		{"absent", `{"version":1}`, defaultBenchmarkTimeWeight},
		{"out of range", `{"version":1,"time_weight":9}`, benchmarkTimeWeightMax},
		{"off a stop", `{"version":1,"time_weight":1.4}`, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			loaded := newStats()
			if err := loaded.load(path); err != nil {
				t.Fatal(err)
			}
			if got := loaded.currentTimeWeight(); got != test.want {
				t.Fatalf("weight = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCreditSpendRoundTripKeepsCurrentMonth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orr", "stats.json")
	s := newStats()
	s.updateKeyInfo(12.5, true, 75, "monthly")
	if err := s.save(path); err != nil {
		t.Fatal(err)
	}

	loaded := newStats()
	if err := loaded.load(path); err != nil {
		t.Fatal(err)
	}
	if got := loaded.snapshot().CreditSpend; got != 12.5 {
		t.Fatalf("credit spend = %v, want 12.5", got)
	}
}

// A poll can carry the cap and its reset without usage_monthly. That answer says
// nothing about spend, so the last polled figure has to stand rather than
// presenting a zero the key never reported.
func TestUpdateKeyInfoKeepsUsageWhenThePollOmitsIt(t *testing.T) {
	s := newStats()
	s.updateKeyInfo(12.5, true, 75, "monthly")
	s.updateKeyInfo(0, false, 90, "monthly")
	snap := s.snapshot()
	if snap.CreditSpend != 12.5 {
		t.Fatalf("credit spend = %v after a limit-only poll, want 12.5", snap.CreditSpend)
	}
	if snap.CreditMonth == "" {
		t.Fatal("a limit-only poll cleared the month that marks a polled spend")
	}
	if snap.Budget != 90 {
		t.Fatalf("budget = %v, want the limit-only poll's 90", snap.Budget)
	}
}

// A reset that differs from the stored one opens a new cycle even inside the
// same calendar month. Spend polled under the old cycle must not be reported
// against the new cap, so the row falls back to the proxy's own accounting until
// a poll carries usage again.
func TestUpdateKeyInfoDropsSpendWhenANewCycleStartsWithoutUsage(t *testing.T) {
	s := newStats()
	s.updateKeyInfo(12.5, true, 75, "2026-10-01T00:00:00Z")
	s.updateKeyInfo(0, false, 50, "2026-10-18T00:00:00Z")
	snap := s.snapshot()
	if snap.CreditSpend != 0 || snap.CreditMonth != "" {
		t.Fatalf("old cycle's spend survived a new cycle: spend %v month %q", snap.CreditSpend, snap.CreditMonth)
	}
	if snap.Budget != 50 {
		t.Fatalf("budget = %v, want the new cycle's 50", snap.Budget)
	}
	// A usage answer for the new cycle restores the polled figure.
	s.updateKeyInfo(1.5, true, 50, "2026-10-18T00:00:00Z")
	if snap := s.snapshot(); snap.CreditSpend != 1.5 {
		t.Fatalf("credit spend = %v, want the new cycle's 1.5", snap.CreditSpend)
	}
}

func TestRestoredProviderPinIsActiveAndListed(t *testing.T) {
	routing := newRoutingState(map[string]providerConfig{"moonshotai/kimi-k3": {}}, time.Hour)
	routing.endpointCache["moonshotai/kimi-k3"] = []endpointMeta{{Tag: "fireworks"}}
	routing.restorePins(map[string]persistedPin{"moonshotai/kimi-k3": {Provider: "fireworks", PinnedAt: time.Now()}})
	providers := routing.providers("moonshotai/kimi-k3", nil)
	if len(providers) != 1 || providers[0] != "fireworks" {
		t.Fatalf("restored provider list = %#v", providers)
	}
	if got := routing.active("moonshotai/kimi-k3", nil); got != "fireworks" {
		t.Fatalf("active provider = %q", got)
	}
}

func TestStatsRingBuffer(t *testing.T) {
	s := newStats()
	for i := 0; i < maxRequestRecords+5; i++ {
		s.record(requestRecord{Status: 200})
	}
	if got := len(s.snapshot().Records); got != maxRequestRecords {
		t.Fatalf("records = %d", got)
	}
}

func TestWindowRecordsRetainEightHourSpendWindow(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	s := newStats()
	s.windowRecords = make([]requestRecord, maxWindowRecords)
	s.windowRecords[0] = requestRecord{Time: now.Add(-2 * time.Hour)}
	s.record(requestRecord{Time: now})

	windowRecords := s.snapshot().WindowRecords
	if len(windowRecords) != 2 {
		t.Fatalf("window records = %d, want the 2h-old and current records", len(windowRecords))
	}
	if !windowRecords[0].Time.Equal(now.Add(-2 * time.Hour)) {
		t.Fatalf("2h-old spend record was pruned: %#v", windowRecords)
	}
}

func TestMetricsRecordsSurviveGeneralHistoryLimit(t *testing.T) {
	s := newStats()
	now := time.Now()
	for i := 0; i < maxRequestRecords+1; i++ {
		s.record(requestRecord{
			Time:             now.Add(time.Duration(i-maxRequestRecords) * time.Second),
			Model:            "a/b",
			Provider:         "p",
			Status:           200,
			Duration:         time.Second,
			TTFT:             100 * time.Millisecond,
			CompletionTokens: 10,
		})
	}
	snap := s.snapshot()
	if len(snap.Records) != maxRequestRecords {
		t.Fatalf("general records = %d, want %d", len(snap.Records), maxRequestRecords)
	}
	if len(snap.MetricsRecords) != maxRequestRecords+1 {
		t.Fatalf("metrics records = %d, want %d", len(snap.MetricsRecords), maxRequestRecords+1)
	}
}

func TestAutoPinSurvivesRestartAndManualPinWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orr", "stats.json")
	auto := newStats()
	auto.setAutoPin("moonshotai/kimi-k3", "fireworks")
	if err := auto.save(path); err != nil {
		t.Fatal(err)
	}
	loaded := newStats()
	if err := loaded.load(path); err != nil {
		t.Fatal(err)
	}
	pins := loaded.providerPinsSnapshot()
	pin, ok := pins["moonshotai/kimi-k3"]
	if !ok || pin.Provider != "fireworks" {
		t.Fatalf("auto pin did not survive restart: %#v", pins)
	}
}

func TestBenchmarkRunDateSurvivesRestart(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	path := filepath.Join(t.TempDir(), "orr", "stats.json")
	now := time.Now()
	original := newStats()
	original.markBenchmarkRun(model, now)
	if err := original.save(path); err != nil {
		t.Fatal(err)
	}

	loaded := newStats()
	if err := loaded.load(path); err != nil {
		t.Fatal(err)
	}
	if !loaded.benchmarkRanToday(model, now) {
		t.Fatal("saved benchmark run was not restored")
	}
	if loaded.benchmarkRanToday(model, now.Add(24*time.Hour)) {
		t.Fatal("yesterday's benchmark run suppressed a new day")
	}
}

func TestDiagnosticsWindows(t *testing.T) {
	now := time.Now()
	s := newStats()
	// One successful API request now.
	s.record(requestRecord{Time: now, Status: 200})
	s.record(requestRecord{Time: now, Status: 200})
	// One tool request that errored.
	s.record(requestRecord{Time: now, Status: 500, Err: true, ToolCalls: 2})
	// One successful tool request.
	s.record(requestRecord{Time: now, Status: 200, ToolCalls: 1})
	// One old request outside all windows.
	s.record(requestRecord{Time: now.Add(-61 * time.Minute), Status: 500, Err: true})

	snap := s.snapshot()
	if len(snap.Diagnostics.Windows) != 3 {
		t.Fatalf("expected 3 diagnostic windows, got %d", len(snap.Diagnostics.Windows))
	}
	for i, window := range snap.Diagnostics.Windows {
		if window.Total != 4 {
			t.Fatalf("window %d total = %d, want 4", i, window.Total)
		}
		if window.APIErrors != 1 {
			t.Fatalf("window %d api errors = %d, want 1", i, window.APIErrors)
		}
		if window.ToolReqs != 2 {
			t.Fatalf("window %d tool reqs = %d, want 2", i, window.ToolReqs)
		}
		if window.ToolErrs != 1 {
			t.Fatalf("window %d tool errs = %d, want 1", i, window.ToolErrs)
		}
	}
	if got := snap.Diagnostics.apiErrorRate(0); got != 25.0 {
		t.Fatalf("api error rate = %v, want 25", got)
	}
	if got := snap.Diagnostics.toolErrorRate(0); got != 50.0 {
		t.Fatalf("tool error rate = %v, want 50", got)
	}
}

func TestDiagnosticsEmptyWindows(t *testing.T) {
	s := newStats()
	snap := s.snapshot()
	if len(snap.Diagnostics.Windows) != 3 {
		t.Fatalf("expected 3 diagnostic windows, got %d", len(snap.Diagnostics.Windows))
	}
	for i := range snap.Diagnostics.Windows {
		if got := snap.Diagnostics.apiErrorRate(i); got >= 0 {
			t.Fatalf("empty api error rate = %v, want negative", got)
		}
		if got := snap.Diagnostics.toolErrorRate(i); got >= 0 {
			t.Fatalf("empty tool error rate = %v, want negative", got)
		}
	}
}

// The recovery cooldown is part of the persisted state: a restart must not hand
// the same failing provider a fresh benchmark allowance minutes apart.
func TestStatsPersistsTheRecoveryCooldown(t *testing.T) {
	const model = "author/model"
	now := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.Local)
	s := newStats()
	s.now = func() time.Time { return now }
	s.markRecoveryRun(model, now)
	path := filepath.Join(t.TempDir(), "stats.json")
	if err := s.save(path); err != nil {
		t.Fatal(err)
	}

	loaded := newStats()
	loaded.now = func() time.Time { return now }
	if err := loaded.load(path); err != nil {
		t.Fatal(err)
	}
	if got := loaded.recoveryLastRun(model); !got.Equal(now) {
		t.Fatalf("reloaded recovery run = %v, want %v", got, now)
	}
	// A latch ahead of the wall clock is stale, not a cooldown that outlives the
	// failure it belongs to.
	behind := now.Add(-time.Minute)
	loaded.now = func() time.Time { return behind }
	if got := loaded.recoveryLastRun(model); !got.IsZero() {
		t.Fatalf("a future recovery latch reported a cooldown: %v", got)
	}
}

// A stats file written before the recovery cooldown existed must still load: the
// field is additive, and a missing one means "no cooldown yet", not a decode
// error that would cost the user their spend and pin history on upgrade.
func TestStatsLoadsWithoutRecoveryRuns(t *testing.T) {
	const model = "author/model"
	now := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.Local)
	lastRun := now.Add(-time.Hour).Format(time.RFC3339Nano)
	legacy := `{
  "version": 1,
  "request_count": 7,
  "benchmark_runs": {"` + model + `": "` + lastRun + `"},
  "provider_pins": {"` + model + `": {"provider": "pinned", "pinned_at": "` + lastRun + `"}}
}
`
	path := filepath.Join(t.TempDir(), "stats.json")
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newStats()
	s.now = func() time.Time { return now }
	if err := s.load(path); err != nil {
		t.Fatalf("legacy stats file did not load: %v", err)
	}
	if !s.benchmarkedWithin(model, now, 2*time.Hour) {
		t.Fatal("legacy benchmark run was not restored")
	}
	if pin := s.providerPinsSnapshot()[model]; pin.Provider != "pinned" {
		t.Fatalf("legacy pin = %#v, want pinned", pin)
	}
	if got := s.recoveryLastRun(model); !got.IsZero() {
		t.Fatalf("absent recovery_runs reported a cooldown: %v", got)
	}
	// The file can be written back in the new shape without losing anything.
	if err := s.save(filepath.Join(t.TempDir(), "saved.json")); err != nil {
		t.Fatalf("stats with a legacy history did not save: %v", err)
	}
}

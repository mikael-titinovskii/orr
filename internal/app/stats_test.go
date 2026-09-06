package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

func TestCreditSpendRoundTripKeepsCurrentMonth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orr", "stats.json")
	s := newStats()
	s.updateKeyInfo(12.5, 75, "monthly")
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

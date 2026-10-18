package app

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// degradationModel is the model the watchdog tests route.
const degradationModel = "author/model"

// vitalsRecord builds one answered client request with the throughput and
// first-token latency the score reads. Throughput is derived the same way the
// dashboard derives it: output tokens over the time spent generating them.
func vitalsRecord(tps float64, ttft time.Duration, status int) requestRecord {
	record := requestRecord{Status: status, TTFT: ttft, Duration: ttft, PromptTokens: 100}
	if status < 200 || status >= 300 {
		record.Err = true
		return record
	}
	record.CompletionTokens = 100
	record.Duration = ttft + time.Duration(float64(time.Second)*100/tps)
	return record
}

func TestVitalsScoreComparesProviderWithItself(t *testing.T) {
	second := time.Second
	baseline := []requestRecord{
		vitalsRecord(100, second, http.StatusOK),
		vitalsRecord(100, second, http.StatusOK),
		vitalsRecord(100, second, http.StatusOK),
	}
	for _, test := range []struct {
		name     string
		recent   []requestRecord
		baseline []requestRecord
		want     float64
		ok       bool
	}{
		{name: "unchanged", recent: baseline, baseline: baseline, want: 1, ok: true},
		{
			// One vital at a quarter of baseline: 0.25^(1/3).
			name: "throughput collapse", ok: true, want: math.Cbrt(0.25), baseline: baseline,
			recent: []requestRecord{
				vitalsRecord(25, second, http.StatusOK),
				vitalsRecord(25, second, http.StatusOK),
				vitalsRecord(25, second, http.StatusOK),
			},
		},
		{
			// Four times the baseline first token is the same ratio inverted.
			name: "responsiveness collapse", ok: true, want: math.Cbrt(0.25), baseline: baseline,
			recent: []requestRecord{
				vitalsRecord(100, 4*second, http.StatusOK),
				vitalsRecord(100, 4*second, http.StatusOK),
				vitalsRecord(100, 4*second, http.StatusOK),
			},
		},
		{
			// Errors count toward reliability only; the two measured vitals stay
			// unchanged, so the score is 2/3 ^ (1/3).
			name: "reliability collapse", ok: true, want: math.Cbrt(2.0 / 3.0), baseline: baseline,
			recent: []requestRecord{
				vitalsRecord(100, second, http.StatusOK),
				vitalsRecord(100, second, http.StatusOK),
				vitalsRecord(0, 0, http.StatusBadGateway),
			},
		},
		{
			// A provider that outruns its own baseline is healthy, not amplified:
			// every ratio clamps at 1.
			name: "faster than baseline", ok: true, want: 1, baseline: baseline,
			recent: []requestRecord{
				vitalsRecord(300, second/2, http.StatusOK),
				vitalsRecord(300, second/2, http.StatusOK),
				vitalsRecord(300, second/2, http.StatusOK),
			},
		},
		{
			// Every request failing is the collapse, not an absence of evidence:
			// there is no successful recent request left to time.
			name: "total failure with a baseline", ok: true, want: 0, baseline: baseline,
			recent: []requestRecord{
				vitalsRecord(0, 0, http.StatusBadGateway),
				vitalsRecord(0, 0, http.StatusBadGateway),
				vitalsRecord(0, 0, http.StatusBadGateway),
			},
		},
		{name: "no earlier traffic", recent: baseline, ok: false},
		{name: "failures with no baseline", ok: false, recent: []requestRecord{vitalsRecord(0, 0, http.StatusBadGateway)}},
		{name: "no recent traffic", baseline: baseline, ok: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			score, ok := vitalsScore(test.recent, test.baseline)
			if ok != test.ok {
				t.Fatalf("ok = %t, want %t", ok, test.ok)
			}
			if !test.ok {
				return
			}
			if math.Abs(score-test.want) > 1e-9 {
				t.Fatalf("score = %v, want %v", score, test.want)
			}
		})
	}
}

func TestDegradationRecordsSplitRecentFromBaseline(t *testing.T) {
	const provider = "provider"
	now := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.Local)
	s := newStats()
	s.now = func() time.Time { return now }
	record := func(at time.Time, record requestRecord) {
		t.Helper()
		record.Time, record.Model, record.Provider = at, degradationModel, provider
		s.record(record)
	}
	record(now.Add(-20*time.Second), vitalsRecord(20, 4*time.Second, http.StatusOK))
	record(now.Add(-10*time.Second), vitalsRecord(0, 0, http.StatusBadGateway))
	// Benchmarks, policy refusals, another provider, and traffic older than the
	// baseline window all describe something other than this provider's vitals.
	benchmark := vitalsRecord(10, 4*time.Second, http.StatusOK)
	benchmark.Benchmark = true
	record(now.Add(-15*time.Second), benchmark)
	blocked := vitalsRecord(10, 4*time.Second, http.StatusOK)
	blocked.Blocked = true
	record(now.Add(-15*time.Second), blocked)
	other := vitalsRecord(10, 4*time.Second, http.StatusOK)
	other.Provider = "other"
	other.Time, other.Model = now, degradationModel
	s.record(other)
	record(now.Add(-degradationBaselineWindow-time.Minute), vitalsRecord(100, time.Second, http.StatusOK))
	record(now.Add(-10*time.Minute), vitalsRecord(100, time.Second, http.StatusOK))

	recent, baseline := s.degradationRecords(degradationModel, provider, now, 2*time.Minute)
	if len(recent) != 2 {
		t.Fatalf("recent records = %d, want the success and the failure: %#v", len(recent), recent)
	}
	if recent[0].Status == 0 || recent[1].Status == 0 {
		t.Fatalf("recent window kept an unanswered record: %#v", recent)
	}
	if len(baseline) != 1 {
		t.Fatalf("baseline records = %d, want only the success inside the window: %#v", len(baseline), baseline)
	}
	if baseline[0].apiError() || baseline[0].Provider != provider {
		t.Fatalf("baseline kept the wrong record: %#v", baseline[0])
	}
}

// A busy proxy must still judge its whole window. The request table keeps only
// the last 200 records, so reading the window from it would shrink a two-minute
// window to seconds under load and make a MIN_SAMPLES above 200 unreachable.
func TestDegradationWindowIsNotCappedByTheRequestTable(t *testing.T) {
	const provider = "provider"
	now := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.Local)
	s := newStats()
	s.now = func() time.Time { return now }
	answered := maxRequestRecords + 50
	for range answered {
		record := vitalsRecord(100, time.Second, http.StatusOK)
		record.Time, record.Model, record.Provider = now.Add(-time.Minute), degradationModel, provider
		s.record(record)
	}
	recent, _ := s.degradationRecords(degradationModel, provider, now, 2*time.Minute)
	if len(recent) != answered {
		t.Fatalf("recent records = %d, want all %d answered inside the window", len(recent), answered)
	}
}

// degradationCatalogTransport answers catalog refreshes with a single
// compatible endpoint, which is what the recovery and degradation paths fetch
// before benchmarking.
func degradationCatalogTransport(tag string) roundTripFunc {
	return func(*http.Request) (*http.Response, error) {
		catalog := endpointList{}
		catalog.Data.Endpoints = []modelEndpoint{{
			Tag: tag, ProviderName: tag, Status: 0,
			SupportedParameters: []string{"tools", "tool_choice"},
		}}
		data, _ := json.Marshal(catalog)
		return response(http.StatusOK, string(data)), nil
	}
}

func newDegradationProxy(t *testing.T, now time.Time) *proxy {
	t.Helper()
	cfg := testConfig("https://upstream.test/api/v1")
	cfg.OpenRouterAPIKey = "sk-test"
	cfg.Models = map[string]providerConfig{degradationModel: {Order: []string{"provider"}, UpdatedAt: now}}
	cfg.DegradationWindow = 2 * time.Minute
	cfg.DegradationMinSamples = defaultDegradationMinSamples
	cfg.DegradationThreshold = defaultDegradationThreshold
	cfg.DegradationCooldown = defaultDegradationCooldown
	cfg.DegradationTimeout = defaultDegradationTimeout
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.setClock(func() time.Time { return now })
	p.client.Transport = degradationCatalogTransport("provider")
	p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		return pingSample(100, 100*time.Millisecond), nil
	}
	if !p.routing.autoPin(degradationModel, "provider") {
		t.Fatal("automatic pin was not installed")
	}
	return p
}

// seedVitals stores count answered requests for the model's provider at one
// instant, with the throughput and first token the score should read.
func seedVitals(p *proxy, at time.Time, tps float64, ttft time.Duration, count int) {
	for range count {
		record := vitalsRecord(tps, ttft, http.StatusOK)
		record.Time, record.Model, record.Provider = at, degradationModel, "provider"
		p.stats.record(record)
	}
}

// A provider that still answers but far slower than it did is the case routing
// cannot see from old measurements. One trigger spends one bounded benchmark;
// the cooldown is what keeps it from spending another until it expires.
func TestDegradationTriggersOneBenchmarkPerCooldown(t *testing.T) {
	now := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.Local)
	p := newDegradationProxy(t, now)
	var benchmarks atomic.Int32
	p.dailyTestProvider = func(context.Context, string, string, int) (providerTestSample, error) {
		benchmarks.Add(1)
		return pingSample(100, 100*time.Millisecond), nil
	}
	seedVitals(p, now.Add(-10*time.Minute), 100, 200*time.Millisecond, defaultDegradationMinSamples)
	seedVitals(p, now.Add(-20*time.Second), 20, 2*time.Second, defaultDegradationMinSamples)

	score, triggered := p.checkDegradation(degradationModel)
	if !triggered {
		t.Fatalf("degradation did not trigger at score %v", score)
	}
	if score >= defaultDegradationThreshold {
		t.Fatalf("score = %v, want below %v", score, defaultDegradationThreshold)
	}
	p.waitForDailyBenchmarks()
	if got := benchmarks.Load(); got == 0 {
		t.Fatal("degradation did not benchmark the model")
	}
	p.degradationMu.Lock()
	last := p.degradationLast[degradationModel]
	p.degradationMu.Unlock()
	if last.IsZero() {
		t.Fatal("degradation benchmark did not consume its cooldown")
	}
	if _, again := p.checkDegradation(degradationModel); again {
		t.Fatal("cooldown allowed a second benchmark")
	}
	// Once the cooldown is over the watch may act again; the vitals are still
	// below the threshold, so nothing else has to change.
	p.degradationMu.Lock()
	p.degradationLast[degradationModel] = now.Add(-defaultDegradationCooldown - time.Second)
	p.degradationMu.Unlock()
	if _, again := p.checkDegradation(degradationModel); !again {
		t.Fatal("expired cooldown did not allow another benchmark")
	}
	p.waitForDailyBenchmarks()
}

// The watch judges the route the client's traffic actually reached, so a
// benchmark ping or a policy refusal — which touch providers the client never
// used — must not become the target.
func TestLastProviderIgnoresBenchmarkPingsAndRefusals(t *testing.T) {
	now := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.Local)
	s := newStats()
	s.now = func() time.Time { return now }
	client := vitalsRecord(100, time.Second, http.StatusOK)
	client.Model, client.Provider, client.Time = degradationModel, "client-route", now
	s.record(client)
	benchmark := vitalsRecord(100, time.Second, http.StatusOK)
	benchmark.Benchmark, benchmark.Model, benchmark.Provider = true, degradationModel, "pinged"
	benchmark.Time = now.Add(time.Second)
	s.record(benchmark)
	refused := vitalsRecord(0, 0, http.StatusForbidden)
	refused.Blocked, refused.Model, refused.Provider = true, degradationModel, "refused"
	refused.Time = now.Add(2 * time.Second)
	s.record(refused)
	if got := s.lastProvider(degradationModel); got != "client-route" {
		t.Fatalf("lastProvider = %q, want the route the client used", got)
	}
}

// Ranking reads the benchmark pool rather than one run's median, so the
// convicted provider's older healthy rounds have to be forgotten when the watch
// sends it back to the benchmark, or they outvote the fresh slow ones.
func TestDegradationForgetsTheConvictedProvidersPool(t *testing.T) {
	now := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.Local)
	p := newDegradationProxy(t, now)
	for range benchmarkSamplesPerProvider {
		p.stats.record(requestRecord{
			Time: now.Add(-48 * time.Hour), Model: degradationModel, Provider: "provider",
			Status: http.StatusOK, Duration: 800 * time.Millisecond, TTFT: 100 * time.Millisecond,
			CompletionTokens: 64, Benchmark: true,
		})
	}
	if len(p.stats.benchmarkPool(degradationModel)["provider"]) != benchmarkSamplesPerProvider {
		t.Fatal("pool was not seeded")
	}
	seedVitals(p, now.Add(-10*time.Minute), 100, 200*time.Millisecond, defaultDegradationMinSamples)
	seedVitals(p, now.Add(-20*time.Second), 20, 2*time.Second, defaultDegradationMinSamples)
	if _, triggered := p.checkDegradation(degradationModel); !triggered {
		t.Fatal("degradation did not trigger")
	}
	p.waitForDailyBenchmarks()
	if kept := p.stats.benchmarkPool(degradationModel)["provider"]; len(kept) != 0 {
		t.Fatalf("pool kept %d rounds of the convicted provider: %#v", len(kept), kept)
	}
}

func TestDegradationLeavesDisabledManualAndThinRoutesAlone(t *testing.T) {
	now := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.Local)
	for _, test := range []struct {
		name  string
		setup func(*proxy)
	}{
		{name: "switched off", setup: func(p *proxy) { p.routing.setDegradation(false) }},
		{name: "manual pin", setup: func(p *proxy) {
			if err := p.routing.setManualPin(degradationModel, "provider"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "too few samples", setup: func(p *proxy) {
			seedVitals(p, now.Add(-10*time.Minute), 100, 200*time.Millisecond, defaultDegradationMinSamples)
			seedVitals(p, now.Add(-20*time.Second), 20, 2*time.Second, defaultDegradationMinSamples-1)
		}},
		{name: "within threshold", setup: func(p *proxy) {
			seedVitals(p, now.Add(-10*time.Minute), 100, 200*time.Millisecond, defaultDegradationMinSamples)
			seedVitals(p, now.Add(-20*time.Second), 95, 210*time.Millisecond, defaultDegradationMinSamples)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newDegradationProxy(t, now)
			test.setup(p)
			if _, triggered := p.checkDegradation(degradationModel); triggered {
				t.Fatal("watchdog benchmarked a route it must leave alone")
			}
		})
	}
}

// A manual pin is still watched for the log, but the pin itself must not move
// when a benchmark finishes.
func TestDegradationNeverMovesAManualPin(t *testing.T) {
	now := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.Local)
	p := newDegradationProxy(t, now)
	if err := p.routing.setManualPin(degradationModel, "provider"); err != nil {
		t.Fatal(err)
	}
	seedVitals(p, now.Add(-10*time.Minute), 100, 200*time.Millisecond, defaultDegradationMinSamples)
	seedVitals(p, now.Add(-20*time.Second), 20, 2*time.Second, defaultDegradationMinSamples)
	if _, triggered := p.checkDegradation(degradationModel); triggered {
		t.Fatal("watchdog acted on a manual pin")
	}
	pin, manual := p.routing.pinInfo(degradationModel)
	if pin != "provider" || !manual {
		t.Fatalf("pin after the watch = %q manual=%t, want provider manual", pin, manual)
	}
}

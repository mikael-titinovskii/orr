package app

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/mikael-titinovskii/orr/internal/store"
)

const maxRequestRecords = 200
const maxMetricsRecords = 5000
const maxWindowRecords = 10000
const measuredMetricsTTL = 30 * time.Minute
const spendWindowRetention = 8 * time.Hour

var diagnosticWindows = []time.Duration{
	1 * time.Minute,
	10 * time.Minute,
	60 * time.Minute,
}

type requestRecord struct {
	Time             time.Time     `json:"time"`
	Method           string        `json:"method"`
	Path             string        `json:"path"`
	Model            string        `json:"model"`
	Provider         string        `json:"provider"`
	Status           int           `json:"status"`
	Duration         time.Duration `json:"duration"`
	TTFT             time.Duration `json:"ttft"`
	PromptTokens     int           `json:"prompt_tokens"`
	CompletionTokens int           `json:"completion_tokens"`
	ReasoningTokens  int           `json:"reasoning_tokens"`
	ReasoningEffort  string        `json:"reasoning_effort"`
	CachedTokens     int           `json:"cached_tokens"`
	CacheWriteTokens int           `json:"cache_write_tokens"`
	Cost             float64       `json:"cost"`
	Err              bool          `json:"error"`
	ToolCalls        int           `json:"tool_calls"`
	// Benchmark marks a provider ping-pong round rather than client traffic.
	// Every provider answers the identical ping, so those rounds are the only
	// comparable first-token measurements; client requests differ in prompt
	// size, which would make the pinned provider look slower than a provider
	// that has only ever answered pings.
	Benchmark bool `json:"benchmark,omitempty"`
	// Blocked marks a request OpenRouter refused to route on policy grounds:
	// the provider's endpoint fails the account's data policy or a workspace
	// guardrail. It is tracked apart from Err because it is not a provider
	// fault and no amount of retrying can clear it — only changing the
	// setting the refusal names.
	Blocked bool `json:"blocked,omitempty"`
	// BlockedReason is OpenRouter's machine-readable ineligibility reason,
	// such as zdr-violation-by-guardrail.
	BlockedReason string `json:"blocked_reason,omitempty"`
	// RateLimited is transient request-path evidence used to trigger automatic
	// failover. It is deliberately not persisted; surfaced 429s retain their
	// status normally, while internally recovered attempts stay out of the
	// client-facing request record.
	RateLimited bool `json:"-"`
	// ProviderUnavailable marks OpenRouter reporting that the single selected
	// endpoint no longer serves the model. Like RateLimited, it is routing-only
	// evidence and is never persisted.
	ProviderUnavailable bool `json:"-"`
	// ProviderFailureHandled prevents a provider failure handled inside the
	// forwarding loop from being processed again after the request is stored.
	ProviderFailureHandled bool `json:"-"`
}

func (r requestRecord) tokensPerSecond() float64 {
	generation := r.Duration - r.TTFT
	if generation <= 0 || r.CompletionTokens <= 0 {
		return 0
	}
	return float64(r.CompletionTokens) / generation.Seconds()
}

// benchmarkThroughput is a ping-pong round's generated tokens per second. It
// falls back to the whole round when the reply was too short to separate
// generating it from waiting for it, and floors the duration because a reply
// can arrive faster than the platform clock resolves — a round too fast to
// measure is a fast round, not a failed one.
func (r requestRecord) benchmarkThroughput() float64 {
	if r.CompletionTokens <= 0 {
		return 0
	}
	if tps := r.tokensPerSecond(); tps > 0 {
		return tps
	}
	return float64(r.CompletionTokens) / max(r.Duration, minMeasurableRound).Seconds()
}

func (r requestRecord) cachePercent() float64 {
	if r.PromptTokens <= 0 {
		return 0
	}
	return float64(r.CachedTokens) / float64(r.PromptTokens) * 100
}

// thinkingPercent is the share of output tokens spent on reasoning (thinking),
// as reported by providers that expose completion_tokens_details.reasoning_tokens.
// Some providers report reasoning tokens outside the completion count, so the
// share is clamped at 100%.
func (r requestRecord) thinkingPercent() float64 {
	if r.CompletionTokens <= 0 {
		return 0
	}
	pct := float64(r.ReasoningTokens) / float64(r.CompletionTokens) * 100
	if pct > 100 {
		return 100
	}
	return pct
}

func (r requestRecord) apiError() bool {
	return r.Err || r.Status < 200 || r.Status >= 300
}

type diagnosticWindow struct {
	Total     int
	APIErrors int
	ToolReqs  int
	ToolErrs  int
}

type diagnosticsSnapshot struct {
	Windows []diagnosticWindow // 1m, 10m, 60m
}

func (d diagnosticsSnapshot) apiErrorRate(index int) float64 {
	if index < 0 || index >= len(d.Windows) {
		return -1
	}
	w := d.Windows[index]
	if w.Total == 0 {
		return -1
	}
	return float64(w.APIErrors) / float64(w.Total) * 100
}

func (d diagnosticsSnapshot) toolErrorRate(index int) float64 {
	if index < 0 || index >= len(d.Windows) {
		return -1
	}
	w := d.Windows[index]
	if w.ToolReqs == 0 {
		return -1
	}
	return float64(w.ToolErrs) / float64(w.ToolReqs) * 100
}

type persistedPin struct {
	Provider string    `json:"provider"`
	PinnedAt time.Time `json:"pinned_at"`
}

// benchmarkSample is one ping-pong round's measurement of a provider.
//
// These are kept per model and provider across benchmark runs and across
// restarts, because a single run cannot measure a provider well enough to rank
// it. Measured against this model's real providers, one run's median of three
// rounds still moves the selected provider on roughly a fifth of runs, while
// pooling five runs moves it on well under one in a hundred. Pooling is also
// the only way to get there inside the daily benchmark's deadline: enough
// rounds to be accurate in a single run takes far longer than the deadline
// allows, and running them concurrently measures the provider serving our own
// concurrent requests instead of the provider.
type benchmarkSample struct {
	Time   time.Time `json:"t"`
	TPS    float64   `json:"tps"`
	TTFTms float64   `json:"ttft_ms"`
}

// benchmarkSamplesPerProvider bounds the pool to the most recent rounds, which
// at providerTestRounds per run is about five runs' worth: enough to be stable,
// few enough that a provider that genuinely changes is re-ranked within a
// couple of runs. benchmarkSampleTTL drops samples too old to describe a
// provider at all.
const benchmarkSamplesPerProvider = 15
const benchmarkSampleTTL = 7 * 24 * time.Hour

type stats struct {
	mu               sync.RWMutex
	saveMu           sync.Mutex
	records          []requestRecord
	metricsRecords   []requestRecord
	windowRecords    []requestRecord
	benchmarkSamples map[string]map[string][]benchmarkSample
	dailyCosts       map[string]float64
	dailyTokens      map[string]int64
	requestCount     uint64
	sessionCount     uint64
	observed         map[string]map[string]struct{}
	creditSpend      float64
	creditMonth      string
	budget           float64
	budgetReset      string
	providerPins     map[string]persistedPin
	benchmarkRuns    map[string]time.Time
	recoveryRuns     map[string]time.Time
	// benchmarkAborts is the last run per model that ended without measuring
	// anything. It is memory-only: a restart is itself a reason to try again.
	benchmarkAborts map[string]time.Time
	timeWeight      float64
	// degradation is the persisted watchdog switch. degradationSaved records
	// whether the database carried a value, so the .env default only seeds a
	// state the user has never toggled.
	degradation      bool
	degradationSaved bool
	inFlight         int
	mutations        uint64
	now              func() time.Time
}

type statsSnapshot struct {
	Records             []requestRecord
	MetricsRecords      []requestRecord
	WindowRecords       []requestRecord
	DailyCosts          map[string]float64
	DailyTokens         map[string]int64
	RequestCount        uint64
	SessionRequestCount uint64
	Observed            map[string][]string
	Last                requestRecord
	HasLast             bool
	AverageTTFT         time.Duration
	AverageTPS          float64
	LatencyP50          time.Duration
	LatencyP95          time.Duration
	Diagnostics         diagnosticsSnapshot
	CreditSpend         float64
	CreditMonth         string
	Budget              float64
	BudgetReset         string
	InFlight            int
}

func newStats() *stats {
	return &stats{
		dailyCosts:       make(map[string]float64),
		dailyTokens:      make(map[string]int64),
		observed:         make(map[string]map[string]struct{}),
		providerPins:     make(map[string]persistedPin),
		benchmarkRuns:    make(map[string]time.Time),
		recoveryRuns:     make(map[string]time.Time),
		benchmarkAborts:  make(map[string]time.Time),
		benchmarkSamples: make(map[string]map[string][]benchmarkSample),
		timeWeight:       defaultBenchmarkTimeWeight,
		degradation:      true,
		now:              time.Now,
	}
}

// currentTimeWeight is the persisted speed/price exponent, which seeds the live
// value routing selects with on startup.
func (s *stats) currentTimeWeight() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.timeWeight
}

func (s *stats) setTimeWeight(weight float64) {
	s.mu.Lock()
	s.timeWeight = clampTimeWeight(weight)
	s.mu.Unlock()
}

// degradationEnabled is the saved watchdog switch.
func (s *stats) degradationEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.degradation
}

func (s *stats) setDegradation(enabled bool) {
	s.mu.Lock()
	s.degradation = enabled
	s.degradationSaved = true
	s.mu.Unlock()
}

// seedDegradation applies the configured default only while the database has
// never recorded a toggle. A switch the user pressed is the last word, which is
// the same rule the speed/price stop follows.
func (s *stats) seedDegradation(value bool) {
	s.mu.Lock()
	if !s.degradationSaved {
		s.degradation = value
	}
	s.mu.Unlock()
}

// addBenchmarkSampleLocked appends one round's measurement to the model and
// provider's pool, dropping samples that are too old or beyond the cap. The
// caller holds s.mu.
func (s *stats) addBenchmarkSampleLocked(model, provider string, sample benchmarkSample) {
	if s.benchmarkSamples == nil {
		s.benchmarkSamples = make(map[string]map[string][]benchmarkSample)
	}
	if s.benchmarkSamples[model] == nil {
		s.benchmarkSamples[model] = make(map[string][]benchmarkSample)
	}
	cutoff := sample.Time.Add(-benchmarkSampleTTL)
	kept := make([]benchmarkSample, 0, benchmarkSamplesPerProvider)
	for _, existing := range s.benchmarkSamples[model][provider] {
		if !existing.Time.Before(cutoff) {
			kept = append(kept, existing)
		}
	}
	kept = append(kept, sample)
	if len(kept) > benchmarkSamplesPerProvider {
		kept = kept[len(kept)-benchmarkSamplesPerProvider:]
	}
	s.benchmarkSamples[model][provider] = kept
}

// benchmarkPool returns a copy of one model's pooled benchmark measurements.
func (s *stats) benchmarkPool(model string) map[string][]benchmarkSample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pool := make(map[string][]benchmarkSample, len(s.benchmarkSamples[model]))
	for provider, samples := range s.benchmarkSamples[model] {
		pool[provider] = append([]benchmarkSample(nil), samples...)
	}
	return pool
}

// clearBenchmarkSamples forgets one provider's pooled rounds. The pool holds
// successes only, so once the provider itself answers with an error those
// successes describe a provider that no longer exists; keeping them would
// resurrect it in the next ranking the moment a test is cancelled rather than
// answered. A blocked provider's samples are not cleared here: the refusal
// list already excludes it, and its history is wanted back if the block lifts.
func (s *stats) clearBenchmarkSamples(model, provider string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	providers, ok := s.benchmarkSamples[model]
	if !ok {
		return
	}
	if _, ok := providers[provider]; !ok {
		return
	}
	delete(providers, provider)
	s.mutations++
}

func (s *stats) record(record requestRecord) {
	if record.Time.IsZero() {
		record.Time = s.now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) == maxRequestRecords {
		copy(s.records, s.records[1:])
		s.records[len(s.records)-1] = record
	} else {
		s.records = append(s.records, record)
	}
	if !record.apiError() && record.Model != "" && record.Provider != "" {
		s.metricsRecords = append(s.metricsRecords, record)
		// Benchmark rounds also join the pool that ranking reads. Every
		// provider answers the identical ping, so these are the only
		// measurements comparable across providers.
		if record.Benchmark && record.TTFT > 0 {
			if tps := record.benchmarkThroughput(); tps > 0 {
				s.addBenchmarkSampleLocked(record.Model, record.Provider, benchmarkSample{
					Time:   record.Time,
					TPS:    tps,
					TTFTms: float64(record.TTFT.Microseconds()) / 1000,
				})
			}
		}
	}
	s.windowRecords = append(s.windowRecords, record)
	// Prune lazily, only when a slice crosses its cap, so the hot path stays
	// O(1) until the caps are reached.
	if len(s.metricsRecords) > maxMetricsRecords {
		s.metricsRecords = pruneRecords(s.metricsRecords, record.Time.Add(-measuredMetricsTTL), maxMetricsRecords)
	}
	if len(s.windowRecords) > maxWindowRecords {
		s.windowRecords = pruneRecords(s.windowRecords, record.Time.Add(-spendWindowRetention), maxWindowRecords)
	}
	day := record.Time.Local().Format(time.DateOnly)
	s.dailyCosts[day] += record.Cost
	if record.PromptTokens > 0 {
		s.dailyTokens[day] += int64(record.PromptTokens)
	}
	if record.CompletionTokens > 0 {
		s.dailyTokens[day] += int64(record.CompletionTokens)
	}
	// Benchmark pings are orr's own probes, not client requests: they count in
	// spend and tokens, but the request counters describe what the client sent.
	if !record.Benchmark {
		s.requestCount++
		s.sessionCount++
	}
	s.mutations++
	if record.Model != "" && record.Provider != "" {
		providers := s.observed[record.Model]
		if providers == nil {
			providers = make(map[string]struct{})
			s.observed[record.Model] = providers
		}
		providers[record.Provider] = struct{}{}
	}
}

// pruneRecords keeps the records not older than cutoff, capped at cap entries.
func pruneRecords(records []requestRecord, cutoff time.Time, cap int) []requestRecord {
	kept := records[:0]
	for _, sample := range records {
		if !sample.Time.Before(cutoff) {
			kept = append(kept, sample)
		}
	}
	if len(kept) > cap {
		kept = append([]requestRecord(nil), kept[len(kept)-cap:]...)
	}
	return kept
}

// providerAPIErrorRates reports each provider's recent client-traffic API error
// share for model, keyed by provider tag, over the same window the dashboard's
// performance table shows.
//
// Benchmark rounds are excluded, not because a failed ping is uninformative but
// because it is already spent: a round that answers with an error fails its own
// run, where the result is an error and the pooled history is cleared, so the
// provider cannot hold the pin from that measurement. Counting the same round in
// this window only extends that verdict: one bad probe would veto every later
// run for half an hour even after the provider answered its pings cleanly, which
// is how the cheapest fast provider loses the pin to a provider that merely
// probes well. A run that still reaches its two-of-three quorum with one failed
// round is treated the same way: the quorum exists to tolerate one straggling
// round — a 429 or a timeout while probing is not a verdict about the provider's
// health — so a partial failure ranks the answered rounds and leaves the client
// error window alone.
//
// Account-policy refusals are excluded because they are routing restrictions,
// not provider health failures; the separate blocked-provider state handles
// them. Only answered records count, on either side of the ratio: a request with no
// response status was cancelled by orr itself — the benchmark gate's
// deadline, a client disconnect — and says nothing about the provider.
func providerAPIErrorRates(records []requestRecord, model string, now time.Time) map[string]float64 {
	cutoff := now.Add(-measuredMetricsTTL)
	totals := make(map[string]int)
	errs := make(map[string]int)
	for _, r := range records {
		if r.Model != model || r.Provider == "" || r.Time.Before(cutoff) || r.Status == 0 || r.Blocked || r.Benchmark {
			continue
		}
		totals[r.Provider]++
		if r.apiError() {
			errs[r.Provider]++
		}
	}
	if len(totals) == 0 {
		return nil
	}
	rates := make(map[string]float64, len(totals))
	for provider, total := range totals {
		rates[provider] = float64(errs[provider]) / float64(total)
	}
	return rates
}

func (s *stats) beginRequest() {
	s.mu.Lock()
	s.inFlight++
	s.mutations++
	s.mu.Unlock()
}

func (s *stats) endRequest() {
	s.mu.Lock()
	s.inFlight--
	if s.inFlight < 0 {
		s.inFlight = 0
	} else {
		s.mutations++
	}
	s.mu.Unlock()
}

func (s *stats) snapshot() statsSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := statsSnapshot{
		Records:             append([]requestRecord(nil), s.records...),
		MetricsRecords:      append([]requestRecord(nil), s.metricsRecords...),
		WindowRecords:       append([]requestRecord(nil), s.windowRecords...),
		DailyCosts:          make(map[string]float64, len(s.dailyCosts)),
		DailyTokens:         make(map[string]int64, len(s.dailyTokens)),
		RequestCount:        s.requestCount,
		SessionRequestCount: s.sessionCount,
		Observed:            make(map[string][]string, len(s.observed)),
		CreditSpend:         s.creditSpend,
		CreditMonth:         s.creditMonth,
		Budget:              s.budget,
		BudgetReset:         s.budgetReset,
		InFlight:            s.inFlight,
		Diagnostics: diagnosticsSnapshot{
			Windows: make([]diagnosticWindow, len(diagnosticWindows)),
		},
	}
	for day, cost := range s.dailyCosts {
		snap.DailyCosts[day] = cost
	}
	for day, tokens := range s.dailyTokens {
		snap.DailyTokens[day] = tokens
	}
	for model, providers := range s.observed {
		for provider := range providers {
			snap.Observed[model] = append(snap.Observed[model], provider)
		}
		sort.Strings(snap.Observed[model])
	}
	if len(s.records) == 0 {
		return snap
	}
	snap.HasLast = true
	snap.Last = s.records[len(s.records)-1]
	latencies := make([]time.Duration, 0, len(s.records))
	var ttftTotal time.Duration
	var ttftCount int
	var tpsTotal float64
	var tpsCount int
	now := s.now()
	for _, record := range s.records {
		latencies = append(latencies, record.Duration)
		if record.TTFT > 0 {
			ttftTotal += record.TTFT
			ttftCount++
		}
		if tps := record.tokensPerSecond(); tps > 0 {
			tpsTotal += tps
			tpsCount++
		}
	}
	// Diagnostic windows are computed from the dedicated window ring so busy
	// proxies do not undercount the 60m window.
	for _, record := range s.windowRecords {
		for i, window := range diagnosticWindows {
			if !record.Time.Before(now.Add(-window)) {
				snap.Diagnostics.Windows[i].Total++
				if record.apiError() {
					snap.Diagnostics.Windows[i].APIErrors++
				}
				if record.ToolCalls > 0 {
					snap.Diagnostics.Windows[i].ToolReqs++
					if record.apiError() {
						snap.Diagnostics.Windows[i].ToolErrs++
					}
				}
			}
		}
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	snap.LatencyP50 = percentile(latencies, .50)
	snap.LatencyP95 = percentile(latencies, .95)
	if ttftCount > 0 {
		snap.AverageTTFT = ttftTotal / time.Duration(ttftCount)
	}
	if tpsCount > 0 {
		snap.AverageTPS = tpsTotal / float64(tpsCount)
	}
	return snap
}

func percentile(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := int(math.Ceil(float64(len(values))*p)) - 1
	if index < 0 {
		index = 0
	}
	return values[index]
}

func (s *stats) save(db *store.Store) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	snapshot := store.Stats{
		DailyCosts:       make(map[string]float64, len(s.dailyCosts)),
		DailyTokens:      make(map[string]int64, len(s.dailyTokens)),
		RequestCount:     s.requestCount,
		CreditSpend:      s.creditSpend,
		CreditMonth:      s.creditMonth,
		Budget:           s.budget,
		BudgetReset:      s.budgetReset,
		ProviderPins:     make(map[string]store.Pin, len(s.providerPins)),
		BenchmarkRuns:    make(map[string]time.Time, len(s.benchmarkRuns)),
		RecoveryRuns:     make(map[string]time.Time, len(s.recoveryRuns)),
		BenchmarkSamples: make(map[string]map[string][]store.Sample, len(s.benchmarkSamples)),
	}
	timeWeight := s.timeWeight
	snapshot.TimeWeight = &timeWeight
	// Only a switch the user actually toggled is saved. Writing the seeded value
	// on every save would freeze it in the database on the first run and make
	// ORR_DEGRADATION unchangeable from then on.
	if s.degradationSaved {
		degradation := s.degradation
		snapshot.Degradation = &degradation
	}
	for day, cost := range s.dailyCosts {
		snapshot.DailyCosts[day] = cost
	}
	for day, tokens := range s.dailyTokens {
		snapshot.DailyTokens[day] = tokens
	}
	for model, pin := range s.providerPins {
		snapshot.ProviderPins[model] = store.Pin{Provider: pin.Provider, PinnedAt: pin.PinnedAt}
	}
	for model, ranAt := range s.benchmarkRuns {
		snapshot.BenchmarkRuns[model] = ranAt
	}
	for model, ranAt := range s.recoveryRuns {
		snapshot.RecoveryRuns[model] = ranAt
	}
	for model, providers := range s.benchmarkSamples {
		pool := make(map[string][]store.Sample, len(providers))
		for provider, samples := range providers {
			converted := make([]store.Sample, len(samples))
			for i, sample := range samples {
				converted[i] = store.Sample{Time: sample.Time, TPS: sample.TPS, TTFTMs: sample.TTFTms}
			}
			pool[provider] = converted
		}
		snapshot.BenchmarkSamples[model] = pool
	}
	s.mu.RUnlock()

	if err := db.SaveStats(snapshot); err != nil {
		return fmt.Errorf("save stats: %w", err)
	}
	return nil
}

func (s *stats) load(db *store.Store) error {
	saved, err := db.LoadStats()
	if err != nil {
		return fmt.Errorf("load stats: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dailyCosts = saved.DailyCosts
	if s.dailyCosts == nil {
		s.dailyCosts = make(map[string]float64)
	}
	s.dailyTokens = saved.DailyTokens
	if s.dailyTokens == nil {
		s.dailyTokens = make(map[string]int64)
	}
	s.requestCount = saved.RequestCount
	s.creditMonth = saved.CreditMonth
	if saved.CreditMonth == s.now().Local().Format("2006-01") {
		s.creditSpend = saved.CreditSpend
	} else {
		s.creditSpend = 0
		s.creditMonth = ""
	}
	s.budget = saved.Budget
	s.budgetReset = saved.BudgetReset
	s.providerPins = make(map[string]persistedPin, len(saved.ProviderPins))
	for model, pin := range saved.ProviderPins {
		s.providerPins[model] = persistedPin{Provider: pin.Provider, PinnedAt: pin.PinnedAt}
	}
	s.benchmarkRuns = saved.BenchmarkRuns
	if s.benchmarkRuns == nil {
		s.benchmarkRuns = make(map[string]time.Time)
	}
	s.recoveryRuns = saved.RecoveryRuns
	if s.recoveryRuns == nil {
		s.recoveryRuns = make(map[string]time.Time)
	}
	// A recovery cooldown is read through a 30-second window, so anything older
	// than a day describes a failure that cannot recur; dropping it keeps the
	// database from accumulating one timestamp per model forever.
	recoveryCutoff := s.now().Add(-24 * time.Hour)
	for model, last := range s.recoveryRuns {
		if last.Before(recoveryCutoff) {
			delete(s.recoveryRuns, model)
		}
	}
	// Restore the benchmark pool, dropping samples that have aged out while
	// the proxy was not running.
	s.benchmarkSamples = make(map[string]map[string][]benchmarkSample, len(saved.BenchmarkSamples))
	cutoff := s.now().Add(-benchmarkSampleTTL)
	for model, providers := range saved.BenchmarkSamples {
		for provider, samples := range providers {
			kept := make([]benchmarkSample, 0, len(samples))
			for _, sample := range samples {
				if sample.Time.Before(cutoff) || sample.TPS <= 0 {
					continue
				}
				kept = append(kept, benchmarkSample{Time: sample.Time, TPS: sample.TPS, TTFTms: sample.TTFTMs})
			}
			if len(kept) == 0 {
				continue
			}
			if len(kept) > benchmarkSamplesPerProvider {
				kept = kept[len(kept)-benchmarkSamplesPerProvider:]
			}
			if s.benchmarkSamples[model] == nil {
				s.benchmarkSamples[model] = make(map[string][]benchmarkSample)
			}
			s.benchmarkSamples[model][provider] = kept
		}
	}
	// An absent value keeps the default; a present one is clamped into the
	// slider's range, so a hand-edited database cannot land the control on a
	// value it cannot be moved back from.
	if saved.TimeWeight != nil {
		s.timeWeight = clampTimeWeight(*saved.TimeWeight)
	}
	if saved.Degradation != nil {
		s.degradation = *saved.Degradation
		s.degradationSaved = true
	}
	return nil
}

func (s *stats) setAutoPin(model, provider string) {
	s.setAutoPinAt(model, provider, s.now())
}

func (s *stats) setAutoPinAt(model, provider string, pinnedAt time.Time) {
	if model == "" || provider == "" {
		return
	}
	if pinnedAt.IsZero() {
		pinnedAt = s.now()
	}
	s.mu.Lock()
	s.providerPins[model] = persistedPin{Provider: provider, PinnedAt: pinnedAt}
	s.mutations++
	s.mu.Unlock()
}

func (s *stats) clearAutoPin(model string) {
	if model == "" {
		return
	}
	s.mu.Lock()
	delete(s.providerPins, model)
	s.mutations++
	s.mu.Unlock()
}

func (s *stats) benchmarkRanToday(model string, now time.Time) bool {
	if model == "" {
		return false
	}
	s.mu.RLock()
	last := s.benchmarkRuns[model]
	s.mu.RUnlock()
	return !last.IsZero() && last.Local().Format(time.DateOnly) == now.Local().Format(time.DateOnly)
}

// benchmarkedWithin reports whether model was benchmarked inside window. It is
// what the startup warm-up asks: a fresh process holds no measurements of its
// own, so a run minutes before the restart still leaves the routing pane and the
// pin resting on data this process has never seen. A latch ahead of the wall
// clock — a timestamp written before the clock was stepped back — is stale, not
// fresh: its negative age would otherwise sit inside every window until the
// clock caught up.
func (s *stats) benchmarkedWithin(model string, now time.Time, window time.Duration) bool {
	if model == "" || window <= 0 {
		return false
	}
	s.mu.RLock()
	last := s.benchmarkRuns[model]
	s.mu.RUnlock()
	return !last.IsZero() && !last.After(now) && now.Sub(last) < window
}

func (s *stats) markBenchmarkRun(model string, now time.Time) {
	if model == "" || now.IsZero() {
		return
	}
	s.mu.Lock()
	s.benchmarkRuns[model] = now
	delete(s.benchmarkAborts, model)
	s.mutations++
	s.mu.Unlock()
}

// benchmarkAbortedWithin reports whether model's last run ended without a usable
// catalog inside window. It is the abort floor: a failure is retried
// on it instead of on the freshness window, which would hide the model from
// measurement for five minutes over a one-minute outage.
func (s *stats) benchmarkAbortedWithin(model string, now time.Time, window time.Duration) bool {
	if model == "" || window <= 0 {
		return false
	}
	s.mu.RLock()
	last := s.benchmarkAborts[model]
	s.mu.RUnlock()
	return !last.IsZero() && !last.After(now) && now.Sub(last) < window
}

// markBenchmarkAbort records a run that measured nothing. It deliberately leaves
// the freshness record alone: an aborted run is not evidence that a real
// measurement got younger, and deleting it would let an abort pull the next run
// forward into the five-minute window. A real run supersedes the abort through
// markBenchmarkRun.
func (s *stats) markBenchmarkAbort(model string, now time.Time) {
	if model == "" || now.IsZero() {
		return
	}
	s.mu.Lock()
	s.benchmarkAborts[model] = now
	s.mutations++
	s.mu.Unlock()
}

// recoveryLastRun reports when a provider-recovery benchmark last ran for model.
// A timestamp ahead of the wall clock — written before the clock was stepped
// back — is reported as zero rather than as a cooldown that outlives the
// failure it belongs to.
func (s *stats) recoveryLastRun(model string) time.Time {
	if model == "" {
		return time.Time{}
	}
	s.mu.RLock()
	last := s.recoveryRuns[model]
	s.mu.RUnlock()
	if last.After(s.now()) {
		return time.Time{}
	}
	return last
}

func (s *stats) markRecoveryRun(model string, now time.Time) {
	if model == "" || now.IsZero() {
		return
	}
	s.mu.Lock()
	s.recoveryRuns[model] = now
	s.mutations++
	s.mu.Unlock()
}

func (s *stats) providerPinsSnapshot() map[string]persistedPin {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pins := make(map[string]persistedPin, len(s.providerPins))
	for model, pin := range s.providerPins {
		pins[model] = pin
	}
	return pins
}

// updateKeyInfo applies one key poll. usagePolled says whether the response
// carried usage_monthly: the cap updates either way, but spend keeps its last
// polled value only while the billing cycle it was polled in still stands. A
// payload that names a different reset starts a new cycle, so the old figure
// belongs to the old cycle and is dropped in favour of the local fallback; an
// answer that omits usage then cannot present a zero the key never reported.
func (s *stats) updateKeyInfo(usageMonthly float64, usagePolled bool, budget float64, reset string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cycleChanged := reset != "" && reset != s.budgetReset
	if reset != "" {
		s.budgetReset = reset
	}
	s.budget = budget
	switch {
	case usagePolled:
		s.creditSpend = usageMonthly
		s.creditMonth = creditMonthFor(reset, s.now())
	case cycleChanged:
		s.creditSpend = 0
		s.creditMonth = ""
	}
	s.mutations++
}

// creditMonthFor derives the billing-cycle month from the OpenRouter
// limit_reset timestamp when it can be parsed, falling back to the calendar
// month of now for non-timestamp values such as "monthly".
func creditMonthFor(reset string, now time.Time) string {
	if t, err := time.Parse(time.RFC3339, reset); err == nil {
		return t.Local().Format("2006-01")
	}
	return now.Local().Format("2006-01")
}

// mutationsCount reports how many times the stats changed, so consumers can
// skip recomputing snapshots when nothing new arrived.
func (s *stats) mutationsCount() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mutations
}

func monthSpend(daily map[string]float64, now time.Time) float64 {
	prefix := now.Local().Format("2006-01-")
	var total float64
	for day, cost := range daily {
		if len(day) >= len(prefix) && day[:len(prefix)] == prefix {
			total += cost
		}
	}
	return total
}

package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

const maxRequestRecords = 200
const maxMetricsRecords = 5000
const maxWindowRecords = 10000
const measuredMetricsTTL = 30 * time.Minute
const spendWindowRetention = 8 * time.Hour
const statsFileVersion = 1

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

type persistedStats struct {
	Version       int                     `json:"version"`
	DailyCosts    map[string]float64      `json:"daily_costs,omitempty"`
	DailyTokens   map[string]int64        `json:"daily_tokens,omitempty"`
	RequestCount  uint64                  `json:"request_count"`
	CreditSpend   float64                 `json:"credit_spend,omitempty"`
	CreditMonth   string                  `json:"credit_month,omitempty"`
	Budget        float64                 `json:"budget,omitempty"`
	BudgetReset   string                  `json:"budget_reset,omitempty"`
	ProviderPins  map[string]persistedPin `json:"provider_pins,omitempty"`
	BenchmarkRuns map[string]time.Time    `json:"benchmark_runs,omitempty"`
	// BenchmarkSamples is keyed by model, then by provider. It is persisted so
	// the pool survives a restart; without that, every restart would put
	// selection back to judging providers on a single run's measurement.
	BenchmarkSamples map[string]map[string][]benchmarkSample `json:"benchmark_samples,omitempty"`
}

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
		benchmarkSamples: make(map[string]map[string][]benchmarkSample),
		now:              time.Now,
	}
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
	s.requestCount++
	s.sessionCount++
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

// providerAPIErrorRates reports each provider's recent API error share for
// model, keyed by provider tag, over the same window the dashboard's
// performance table shows. Records carry whatever attribution orr has —
// client requests, benchmark rounds, tests — because the question is not who
// sent the request but whether the provider answered it. The verdict drives
// automatic selection, so it has to be the same one the user reads.
//
// Account-policy refusals are excluded because they are routing restrictions,
// not provider health failures; the separate blocked-provider state handles
// them. Only answered records count, on either side of the ratio: a request with no
// response status was cancelled by orr itself — the benchmark gate's
// deadline, a client disconnect — and says nothing about the provider.
// Convicting a provider for rounds orr aborted would demote and blank the
// providers that were simply still working when the gate closed.
func providerAPIErrorRates(records []requestRecord, model string, now time.Time) map[string]float64 {
	cutoff := now.Add(-measuredMetricsTTL)
	totals := make(map[string]int)
	errs := make(map[string]int)
	for _, r := range records {
		if r.Model != model || r.Provider == "" || r.Time.Before(cutoff) || r.Status == 0 || r.Blocked {
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
	s.mu.Unlock()
}

func (s *stats) endRequest() {
	s.mu.Lock()
	s.inFlight--
	if s.inFlight < 0 {
		s.inFlight = 0
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

func (s *stats) save(path string) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	data, err := json.MarshalIndent(persistedStats{
		Version:          statsFileVersion,
		DailyCosts:       s.dailyCosts,
		DailyTokens:      s.dailyTokens,
		RequestCount:     s.requestCount,
		CreditSpend:      s.creditSpend,
		CreditMonth:      s.creditMonth,
		Budget:           s.budget,
		BudgetReset:      s.budgetReset,
		ProviderPins:     s.providerPins,
		BenchmarkRuns:    s.benchmarkRuns,
		BenchmarkSamples: s.benchmarkSamples,
	}, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("encode stats: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create stats directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".stats-*")
	if err != nil {
		return fmt.Errorf("create stats file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("chmod temporary stats file: %w", err)
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		return fmt.Errorf("write temporary stats file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync temporary stats file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary stats file: %w", err)
	}
	if err := replaceFile(tempPath, path); err != nil {
		return fmt.Errorf("replace stats file: %w", err)
	}
	return nil
}

func (s *stats) load(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read stats: %w", err)
	}
	var saved persistedStats
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("decode stats: top-level null is invalid")
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("decode stats: %w", err)
	}
	// Version 0 is the pre-versioning format, which is byte-compatible with
	// version 1; anything else is an incompatible schema.
	if saved.Version != 0 && saved.Version != statsFileVersion {
		return fmt.Errorf("decode stats: unsupported version %d (expected %d)", saved.Version, statsFileVersion)
	}
	if saved.CreditSpend != 0 && saved.CreditMonth == "" {
		return fmt.Errorf("decode stats: credit_month is required when credit_spend is set")
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
	s.providerPins = saved.ProviderPins
	if s.providerPins == nil {
		s.providerPins = make(map[string]persistedPin)
	}
	s.benchmarkRuns = saved.BenchmarkRuns
	if s.benchmarkRuns == nil {
		s.benchmarkRuns = make(map[string]time.Time)
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
				kept = append(kept, sample)
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

func (s *stats) markBenchmarkRun(model string, now time.Time) {
	if model == "" || now.IsZero() {
		return
	}
	s.mu.Lock()
	s.benchmarkRuns[model] = now
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

func (s *stats) updateKeyInfo(usageMonthly, budget float64, reset string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.budget = budget
	s.budgetReset = reset
	s.creditSpend = usageMonthly
	s.creditMonth = creditMonthFor(reset, s.now())
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

func statsFilePath() (string, error) {
	if runtime.GOOS != "windows" {
		if base := os.Getenv("XDG_DATA_HOME"); base != "" {
			return filepath.Join(base, "orr", "stats.json"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share", "orr", "stats.json"), nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "orr", "stats.json"), nil
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

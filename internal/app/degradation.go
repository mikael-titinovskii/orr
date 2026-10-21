package app

import (
	"log"
	"math"
	"time"
)

// The degradation watchdog answers a question the ranking cannot: the provider
// that wins on price, speed, and quality today may not be the provider the
// client is actually getting. Measurements that made it the winner can be
// minutes old, while its live traffic collapses — a cold pool, a throttled
// shard, a degraded deployment. Ranking already demotes a provider that fails
// outright; this watch is about a provider that still answers, just far worse
// than it did for the same client minutes ago.
//
// It deliberately judges one provider against itself. Comparing a provider to
// the field would trigger on someone else getting faster, which needs no
// benchmark spend to notice, and would fire on every genuinely fast newcomer.
const (
	// degradationBaselineWindow is how far back a provider's own earlier client
	// traffic counts as its baseline. It matches the window the performance
	// table reads, so the numbers the user sees are the numbers the watch uses.
	degradationBaselineWindow = measuredMetricsTTL
	// degradationCheckInterval bounds how often a model's vitals are re-read on
	// the request path. One scan per second per model is a rounding error next to
	// the request it follows, and keeps detection latency well under the window
	// it judges.
	degradationCheckInterval = time.Second
)

// vitalsScore summarizes how a provider's recent answered requests compare with
// its own earlier traffic. Every component is a ratio where 1.0 means unchanged
// and lower is worse, clamped at 1.0 so a burst of speed cannot average away a
// collapse in responsiveness. The score is their geometric mean: one vital at
// 21% of baseline trips a 0.6 threshold, two vitals at 46% each trip it, and
// three at 60% each trip it — escalation that keeps a single wobble from
// spending a benchmark.
//
// Errors enter as the share of recent answered requests that succeeded. The
// baseline's own error share is not used: it is kept for successes only, and a
// provider that has always failed is already demoted by the ranking and the
// failover threshold rather than by this watch. Reliability needs a measured
// baseline, so a provider orr has never seen answer is never convicted for
// failing.
//
// ok is false when there is no baseline to compare against — a provider orr has
// never seen answer before — because "x% of what it was" is undefined then.
func vitalsScore(recent, baseline []requestRecord) (float64, bool) {
	if len(recent) == 0 {
		return 0, false
	}
	ratios := make([]float64, 0, 3)
	recentTPS, baselineTPS := throughputSamples(recent), throughputSamples(baseline)
	if len(recentTPS) > 0 && len(baselineTPS) > 0 {
		ratios = append(ratios, clampVitalsRatio(medianFloat(recentTPS)/medianFloat(baselineTPS)))
	}
	recentTTFT, baselineTTFT := latencySamples(recent), latencySamples(baseline)
	if len(recentTTFT) > 0 && len(baselineTTFT) > 0 {
		// Latency is the one vital where lower is better, so its ratio is
		// inverted to keep every component "higher is healthier".
		ratios = append(ratios, clampVitalsRatio(medianFloat(baselineTTFT)/medianFloat(recentTTFT)))
	}
	answered, answering := 0, 0
	for _, record := range recent {
		if record.Benchmark || record.Status == 0 || record.Blocked {
			continue
		}
		answered++
		if !record.apiError() {
			answering++
		}
	}
	// Reliability needs a provider orr has measured healthy before, or "a share
	// of what it was" has nothing to divide by. With a baseline it is the whole
	// story when every request fails: there is no successful recent request left
	// to time, and that collapse still has to count.
	measured := len(baselineTPS) > 0 || len(baselineTTFT) > 0
	if measured && answered > 0 {
		ratios = append(ratios, clampVitalsRatio(float64(answering)/float64(answered)))
	}
	if len(ratios) == 0 {
		return 0, false
	}
	product := 1.0
	for _, ratio := range ratios {
		product *= ratio
	}
	return math.Pow(product, 1/float64(len(ratios))), true
}

func clampVitalsRatio(ratio float64) float64 {
	if math.IsNaN(ratio) || ratio < 0 {
		return 0
	}
	return math.Min(ratio, 1)
}

// throughputSamples collects the generated tokens per second of a provider's
// successful, answered requests.
func throughputSamples(records []requestRecord) []float64 {
	values := make([]float64, 0, len(records))
	for _, record := range records {
		if record.Benchmark || record.Status == 0 || record.apiError() {
			continue
		}
		if tps := record.tokensPerSecond(); tps > 0 {
			values = append(values, tps)
		}
	}
	return values
}

// latencySamples collects the time to first token of a provider's successful,
// answered requests.
func latencySamples(records []requestRecord) []float64 {
	values := make([]float64, 0, len(records))
	for _, record := range records {
		if record.Benchmark || record.Status == 0 || record.apiError() || record.TTFT <= 0 {
			continue
		}
		values = append(values, record.TTFT.Seconds())
	}
	return values
}

// degradationRecords splits one provider's client traffic for a model into the
// recent window the watch judges and the earlier traffic it judges against.
// Benchmark rounds are excluded on both sides: they are a fixed 16-token ping,
// so their throughput and latency describe the benchmark rather than the
// requests the watch is about.
//
// Everything recent — successes and failures alike — comes from the traffic
// ring, which keeps every record for hours, so both halves of the reliability
// score share one retention and a busy proxy still judges its whole window: the
// request table it replaces keeps only the last 200 records, which would shrink
// a two-minute window to seconds at load and cap the sample count the watch
// requires. Reading recent successes from the longer metrics ring instead would
// count failures against a much larger pool of successes, and the share of
// failed requests, the vital an outage shows up in first, would read far too
// healthy. The baseline is that same metrics ring's long history of successes,
// because it keeps no failures.
func (s *stats) degradationRecords(model, provider string, now time.Time, window time.Duration) (recent, baseline []requestRecord) {
	if window <= 0 {
		return nil, nil
	}
	recentCutoff := now.Add(-window)
	baselineCutoff := now.Add(-degradationBaselineWindow)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, record := range s.windowRecords {
		if record.Model != model || record.Provider != provider || record.Benchmark || record.Blocked || record.Status == 0 {
			continue
		}
		if !record.Time.Before(recentCutoff) {
			recent = append(recent, record)
		}
	}
	for _, record := range s.metricsRecords {
		if record.Model != model || record.Provider != provider || record.Benchmark || record.Blocked || record.Status == 0 || record.apiError() {
			continue
		}
		if record.Time.Before(recentCutoff) && !record.Time.Before(baselineCutoff) {
			baseline = append(baseline, record)
		}
	}
	return recent, baseline
}

// lastProvider is the provider the most recent answered client request for
// model reached. Without an automatic pin it is the route the watch should
// judge. Benchmark pings and policy refusals are not client traffic — they
// reach providers the client never used — so they are skipped here exactly as
// they are everywhere else in this file.
func (s *stats) lastProvider(model string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := len(s.records) - 1; i >= 0; i-- {
		record := s.records[i]
		if record.Model == model && record.Provider != "" && record.Status != 0 && !record.Benchmark && !record.Blocked {
			return record.Provider
		}
	}
	return ""
}

// degradationTarget resolves the provider the watch should judge: the automatic
// pin when one exists, otherwise the provider last traffic actually reached.
// A manually pinned provider is an explicit instruction and is never
// second-guessed here, so it has no target at all. The pin is read through the
// same predicate the other gates use, so the watch and the failure-recovery path
// cannot disagree about whether a model is pinned.
func (p *proxy) degradationTarget(model string) string {
	if p.routing.manualPin(model) {
		return ""
	}
	if pinned := p.routing.pinnedProvider(model); pinned != "" {
		return pinned
	}
	return p.stats.lastProvider(model)
}

// degradationDue throttles the request-path scan to one per interval per model.
func (p *proxy) degradationDue(model string, now time.Time) bool {
	p.degradationMu.Lock()
	defer p.degradationMu.Unlock()
	if last := p.degradationChecked[model]; !last.IsZero() && now.Sub(last) < degradationCheckInterval {
		return false
	}
	p.degradationChecked[model] = now
	return true
}

// checkDegradation judges the active route for model and starts one bounded
// refresh and benchmark when its vitals have collapsed below the threshold. It
// reports the measured score and whether an attempt was started. The dashboard
// calls it when the switch is turned on, so the pane answers immediately
// instead of waiting for the next request.
func (p *proxy) checkDegradation(model string) (float64, bool) {
	if model == "" || !p.routing.degradationEnabled() {
		return 0, false
	}
	provider := p.degradationTarget(model)
	if provider == "" {
		return 0, false
	}
	now := p.now()
	recent, baseline := p.stats.degradationRecords(model, provider, now, p.cfg.DegradationWindow)
	if len(recent) < p.cfg.DegradationMinSamples {
		return 0, false
	}
	score, ok := vitalsScore(recent, baseline)
	if !ok || score >= p.cfg.DegradationThreshold {
		return score, false
	}
	if !p.startDegradationBenchmark(model, provider) {
		return score, false
	}
	// One line, no prompts, keys, or bodies: the same policy as every other
	// routing log.
	log.Printf("degradation recovery: %s via %s at %.0f%% of baseline; refreshing providers", model, provider, score*100)
	return score, true
}

// observeProviderDegradation runs the watch after a client request lands, so
// the decision reads the record the request path just stored.
func (p *proxy) observeProviderDegradation(record requestRecord) {
	if record.Model == "" || !p.routing.degradationEnabled() {
		return
	}
	if !p.degradationDue(record.Model, p.now()) {
		return
	}
	p.checkDegradation(record.Model)
}

// degradationCooldownRemaining reports how much of the cooldown is left for
// model, or zero when the watch may act again.
func (p *proxy) degradationCooldownRemaining(model string) time.Duration {
	p.degradationMu.Lock()
	defer p.degradationMu.Unlock()
	last := p.degradationLast[model]
	if last.IsZero() {
		return 0
	}
	now := p.now()
	if last.After(now) {
		// A latch ahead of the wall clock — a step back in time — is not a cooldown
		// that outlives the failure it belongs to.
		return 0
	}
	return p.cfg.DegradationCooldown - now.Sub(last)
}

// startDegradationBenchmark runs the same bounded refresh and benchmark as
// automatic discovery, under its own cooldown so a provider that stays bad
// cannot spend credits on every request that notices. It reports whether this
// call started a run.
//
// The convicted provider's pooled benchmark rounds are forgotten first. Ranking
// reads that pool rather than one run's median, which is what keeps a single
// noisy round from moving the pin, but pooled rounds of a provider whose live
// traffic has just collapsed describe a provider that no longer exists: a week
// of healthy pings would outvote the slow rounds this run collects and leave
// the client on the provider the watch just convicted.
func (p *proxy) startDegradationBenchmark(model, provider string) bool {
	p.dailyMu.Lock()
	if p.dailyClosed {
		p.dailyMu.Unlock()
		return false
	}
	// The cooldown is checked first: a conviction that arrives while the watch is
	// already waiting must not push the next attempt back, or a stream of
	// convictions would keep it from ever acting.
	if remaining := p.degradationCooldownRemaining(model); remaining > 0 {
		p.dailyMu.Unlock()
		return false
	}
	if _, running := p.dailyReady[model]; running {
		// A refresh is already measuring this model, so its result is the answer and
		// no second run starts. The conviction is handed to that run only if it has not
		// cleared its pool yet — its own pre-ping pop decides that, and a conviction it
		// cannot answer is re-derived from unchanged vitals on the next scan.
		p.degradationWanted[model] = provider
		p.dailyMu.Unlock()
		return false
	}
	// The conviction is recorded before the launch so the run cannot reach its pop
	// first and measure while the stale rounds are still in place.
	p.degradationWanted[model] = provider
	refreshDone := p.routing.launchModelOrderRefresh(model, p.client, p.cfg.Upstream, p.cfg.openRouterKey(), p.cfg.UpdateMaxProviders, p.cfg.UpdateCacheOnly, false)
	if refreshDone == nil {
		// The catalog refresh is backing off after a failure, so no run starts, no
		// cooldown is consumed, and the provider keeps its samples: there would be
		// nothing to repopulate them with. The next attempt past the backoff decides
		// again.
		delete(p.degradationWanted, model)
		p.dailyMu.Unlock()
		return false
	}
	ready := make(chan struct{})
	p.dailyReady[model] = ready
	p.dailyWG.Add(1)
	p.dailyMu.Unlock()

	go p.runAutomaticBenchmark(model, refreshDone, ready, false, p.cfg.DegradationTimeout)
	return true
}

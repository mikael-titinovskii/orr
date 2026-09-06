package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const endpointCacheTTL = 5 * time.Minute

// Provider selection compares what one request actually costs the client and
// how long it actually takes, instead of ranking raw prices and raw speeds
// against each other. Both numbers come from the same reference request, so
// every provider is measured on identical work.
//
// benchmarkTimeSacrifice bounds how much wall-clock a cheaper provider may
// give up: a provider stays viable while one request takes no more than 1.5x
// the field median. The median is the anchor rather than the fastest provider
// because a single exaggerated provider — very fast and very expensive — would
// otherwise drag the ceiling down onto the balanced middle of the field and
// disqualify it. Scaling with the field is also what makes the ceiling detect
// an outlier: a provider that waits eight seconds is excluded outright where
// the field answers in one, and tolerated where the field itself takes seven.
//
// benchmarkChurnMargin is how much better a challenger's deal has to be before
// it takes the automatic pin from the provider that already holds it, so a
// provider hovering at a threshold does not hand the pin back and forth.
const benchmarkTimeSacrifice = 1.5
const benchmarkChurnMargin = 0.05

// benchmarkMinLatencySamples is how many first-token measurements a provider
// needs before its responsiveness is trusted, so one transient spike cannot
// demote it. benchmarkMinProfileRequests is the same idea for the reference
// request: a single request is too thin a description of the client's traffic.
const benchmarkMinLatencySamples = 2
const benchmarkMinProfileRequests = 2

// benchmarkTimeWeight is the exponent on seconds per request in the deal
// score. At 1.0 the score is the plain cost-delay product, which accepts a
// price cut only when it is proportionally larger than the speed it costs, and
// refuses a premium that buys proportionally less speed than it costs. Raise it
// to buy speed more aggressively; lower it to favor price.
const benchmarkTimeWeight = 1.0

// benchmarkReferenceProfile prices and times providers before a model has real
// traffic to measure. It is deliberately agent-shaped — a large, mostly cached
// prompt and a moderate answer — because that is what a coding client sends.
// Observed traffic replaces it as soon as there is any.
var benchmarkReferenceProfile = requestProfile{
	promptTokens:     2_000,
	cachedTokens:     18_000,
	completionTokens: 1_000,
}

// requestProfile is one representative request for a model: the token counts
// that every provider is priced and timed against. Using a single profile for
// the whole comparison is what makes provider costs and durations comparable.
type requestProfile struct {
	promptTokens     float64
	cachedTokens     float64
	cacheWriteTokens float64
	completionTokens float64
	observed         bool
}

// newRequestProfile summarizes a model's recent client traffic. Benchmark
// rounds are excluded: they are a fixed 16-token ping and would describe the
// benchmark rather than the client. Without enough real requests the reference
// profile is used, so selection behaves identically on a cold start.
func newRequestProfile(records []requestRecord, model string, now time.Time) requestProfile {
	cutoff := now.Add(-measuredMetricsTTL)
	prompts := make([]float64, 0, len(records))
	cached := make([]float64, 0, len(records))
	cacheWrites := make([]float64, 0, len(records))
	completions := make([]float64, 0, len(records))
	for _, record := range records {
		if record.Model != model || record.Benchmark || record.Time.Before(cutoff) || record.apiError() {
			continue
		}
		if record.PromptTokens <= 0 && record.CompletionTokens <= 0 {
			continue
		}
		uncached := max(record.PromptTokens-record.CachedTokens, 0)
		prompts = append(prompts, float64(uncached))
		cached = append(cached, float64(max(record.CachedTokens, 0)))
		cacheWrites = append(cacheWrites, float64(max(record.CacheWriteTokens, 0)))
		completions = append(completions, float64(max(record.CompletionTokens, 0)))
	}
	if len(prompts) < benchmarkMinProfileRequests {
		return benchmarkReferenceProfile
	}
	profile := requestProfile{
		promptTokens:     medianFloat(prompts),
		cachedTokens:     medianFloat(cached),
		cacheWriteTokens: medianFloat(cacheWrites),
		completionTokens: medianFloat(completions),
		observed:         true,
	}
	if profile.completionTokens <= 0 {
		// Without an answer to generate, throughput cannot be priced into the
		// duration. Keep the reference answer length rather than timing every
		// provider at zero.
		profile.completionTokens = benchmarkReferenceProfile.completionTokens
	}
	return profile
}

// cost is what this provider charges for the profile's request, in dollars.
// A provider with no cache-read price is billed for the cached tokens at its
// full input price, which is what the client would really pay there. ok is
// false when a price the request actually needs is missing, so an incomplete
// catalog entry is never mistaken for a cheap one.
func (p requestProfile) cost(pricing endpointPricing) (float64, bool) {
	prompt, cached, cacheWrite := p.promptTokens, p.cachedTokens, p.cacheWriteTokens
	if pricing.InputCacheRead.float() <= 0 {
		prompt += cached + cacheWrite
		cached, cacheWrite = 0, 0
	}
	total := max(pricing.Request.float(), 0)
	for _, component := range []struct {
		tokens float64
		price  float64
	}{
		{prompt, pricing.Prompt.float()},
		{cached, pricing.InputCacheRead.float()},
		{cacheWrite, pricing.cacheWrite().float()},
		{p.completionTokens, pricing.Completion.float()},
	} {
		if component.tokens <= 0 {
			continue
		}
		if component.price <= 0 || math.IsNaN(component.price) || math.IsInf(component.price, 0) {
			return 0, false
		}
		total += component.tokens * component.price
	}
	return total, total > 0
}

// seconds is how long the profile's request takes on this provider: the wait
// for the first token plus the time to generate the answer. Combining the two
// measurements into wall-clock is what lets throughput and responsiveness be
// traded off against each other instead of guarded separately.
func (p requestProfile) seconds(result providerTestResult) (float64, bool) {
	if result.tps <= 0 || math.IsNaN(result.tps) || math.IsInf(result.tps, 0) {
		return 0, false
	}
	seconds := p.completionTokens / result.tps
	if result.hasResponsiveness() {
		return seconds + result.ttft.Seconds(), true
	}
	// Timed without a trustworthy first-token measurement. The duration is
	// still usable for ranking, but a provider with a known first token is
	// preferred over this one at an equal score.
	return seconds, false
}

type pinnedProvider struct {
	provider string
	pinnedAt time.Time
	manual   bool
}

type endpointMeta struct {
	Tag                     string
	ProviderName            string
	Pricing                 endpointPricing
	SupportsImplicitCaching bool
	Throughput              float64
	Latency                 float64
	Quantization            string
	Compatible              bool
}

type routingState struct {
	mu                     sync.RWMutex
	persistMu              sync.Mutex
	refreshWG              sync.WaitGroup
	currentModel           string
	pins                   map[string]pinnedProvider
	pinTTL                 time.Duration
	models                 map[string]providerConfig
	providersPath          string
	endpointCache          map[string][]endpointMeta
	endpointFetchedAt      map[string]time.Time
	nameMap                map[string]map[string]string // ProviderName -> Tag per model
	modelRevisions         map[string]uint64
	modelOrderUpdating     map[string]bool
	modelOrderDone         map[string]chan struct{}
	pendingPins            map[string]persistedPin
	temporarilyUnavailable map[string]map[string]time.Time
	revision               uint64
	now                    func() time.Time
}

func newRoutingState(models map[string]providerConfig, pinTTL time.Duration) *routingState {
	if pinTTL <= 0 {
		pinTTL = time.Hour
	}
	copied := cloneProviderConfigs(models)
	routing := &routingState{
		models:                 copied,
		pins:                   make(map[string]pinnedProvider),
		pinTTL:                 pinTTL,
		endpointCache:          make(map[string][]endpointMeta),
		endpointFetchedAt:      make(map[string]time.Time),
		nameMap:                make(map[string]map[string]string),
		modelRevisions:         make(map[string]uint64),
		modelOrderUpdating:     make(map[string]bool),
		modelOrderDone:         make(map[string]chan struct{}),
		pendingPins:            make(map[string]persistedPin),
		temporarilyUnavailable: make(map[string]map[string]time.Time),
		now:                    time.Now,
	}
	for model, provider := range copied {
		if provider.ManualPin != "" {
			routing.pins[model] = pinnedProvider{provider: provider.ManualPin, pinnedAt: routing.now(), manual: true}
		}
	}
	return routing
}

func cloneProviderConfigs(models map[string]providerConfig) map[string]providerConfig {
	cloned := make(map[string]providerConfig, len(models))
	for model, provider := range models {
		provider.Order = append([]string(nil), provider.Order...)
		provider.Only = append([]string(nil), provider.Only...)
		provider.Ignore = append([]string(nil), provider.Ignore...)
		provider.Blocked = append([]blockedProvider(nil), provider.Blocked...)
		cloned[model] = provider
	}
	return cloned
}

func (r *routingState) setProvidersPath(path string) {
	r.mu.Lock()
	r.providersPath = path
	r.mu.Unlock()
}

func (r *routingState) modelConfig(model string) (providerConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	provider, ok := r.models[model]
	return provider, ok
}

func (r *routingState) modelRevision(model string) uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.modelRevisions[model]
}

func (r *routingState) modelConfigAtRevision(model string) (providerConfig, uint64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	provider, ok := r.models[model]
	return provider, r.modelRevisions[model], ok
}

// ensureModel adds model to the routing state and the providers file when it is
// not configured yet, returning whether the model was newly added. The
// providers-file write and the in-memory commit are serialized under persistMu
// and guarded by a revision counter, so a concurrent mutation can never be
// clobbered by a stale write.
func (r *routingState) ensureModel(model string) (bool, error) {
	if !validModelID(model) {
		return false, nil
	}
	for {
		// persistMu serializes every providers-file write together with its
		// commit, so concurrent mutations cannot leave the on-disk file missing
		// a change that memory already has.
		r.persistMu.Lock()
		r.mu.Lock()
		if _, exists := r.models[model]; exists {
			r.mu.Unlock()
			r.persistMu.Unlock()
			return false, nil
		}
		next := cloneProviderConfigs(r.models)
		next[model] = providerConfig{}
		revision := r.revision
		r.mu.Unlock()

		// Persist before committing so a failed write leaves no trace, but do
		// not hold the routing lock during disk I/O.
		if r.providersPath != "" {
			if err := writeProvidersFileAtomic(r.providersPath, next); err != nil {
				r.persistMu.Unlock()
				return false, fmt.Errorf("add model %s: %w", model, err)
			}
		}

		r.mu.Lock()
		if r.revision != revision {
			// A concurrent mutation committed while we were writing; retry
			// against the fresh state so we never clobber it.
			r.mu.Unlock()
			r.persistMu.Unlock()
			continue
		}
		r.models = next
		r.revision++
		r.mu.Unlock()
		r.persistMu.Unlock()
		return true, nil
	}
}

func (r *routingState) setManualPin(model, provider string) error {
	if !validModelID(model) {
		return fmt.Errorf("model %q must use author/model format", model)
	}
	for {
		r.persistMu.Lock()
		r.mu.Lock()
		if provider != "" {
			resolved, ok := r.resolveProviderLocked(model, provider)
			if !ok {
				// Once the endpoint cache is populated it is authoritative for
				// the model's providers, so an unresolved whitespace-free tag
				// is a typo and must be rejected. Before that (startup, no
				// fetches yet) accept the tag so it can be resolved once
				// endpoints arrive. Display names are always rejected.
				if len(r.endpointCache[model]) > 0 || strings.ContainsAny(provider, " \t\r\n") {
					r.mu.Unlock()
					r.persistMu.Unlock()
					return fmt.Errorf("provider %q could not be resolved for model %s", provider, model)
				}
				resolved = provider
			}
			provider = resolved
		}
		next := cloneProviderConfigs(r.models)
		modelConfig := next[model]
		modelConfig.ManualPin = provider
		next[model] = modelConfig
		revision := r.revision
		r.mu.Unlock()

		// Persist before committing, outside the routing lock.
		if r.providersPath != "" {
			if err := writeProvidersFileAtomic(r.providersPath, next); err != nil {
				r.persistMu.Unlock()
				return fmt.Errorf("persist manual provider pin: %w", err)
			}
		}

		r.mu.Lock()
		if r.revision != revision {
			r.mu.Unlock()
			r.persistMu.Unlock()
			continue
		}
		r.models = next
		r.revision++
		r.modelRevisions[model]++
		delete(r.pendingPins, model)
		if provider == "" {
			delete(r.pins, model)
		} else {
			r.pins[model] = pinnedProvider{provider: provider, pinnedAt: r.now(), manual: true}
		}
		r.mu.Unlock()
		r.persistMu.Unlock()
		return nil
	}
}

// mutateModelConfig applies mutate to model's configuration and persists the
// result, retrying if a concurrent change lands between the read and the
// write. mutate reports whether it changed anything; when it did not, nothing
// is written and no revision is burned, so a repeated refusal from a provider
// already known to be blocked costs no disk writes.
func (r *routingState) mutateModelConfig(model string, mutate func(*providerConfig) bool) (bool, error) {
	if !validModelID(model) {
		return false, fmt.Errorf("model %q must use author/model format", model)
	}
	for {
		r.persistMu.Lock()
		r.mu.Lock()
		next := cloneProviderConfigs(r.models)
		modelConfig := next[model]
		if !mutate(&modelConfig) {
			r.mu.Unlock()
			r.persistMu.Unlock()
			return false, nil
		}
		next[model] = modelConfig
		revision := r.revision
		r.mu.Unlock()

		// A failed write is reported but does not abandon the change: this
		// carries refusals, and dropping one because the file could not be
		// written would send the next request straight back to a provider
		// already known to refuse it. Losing it costs one rediscovery after a
		// restart; discarding it costs a failed request now.
		var persistErr error
		if r.providersPath != "" {
			if err := writeProvidersFileAtomic(r.providersPath, next); err != nil {
				persistErr = fmt.Errorf("persist provider config for %s: %w", model, err)
			}
		}

		r.mu.Lock()
		if r.revision != revision {
			r.mu.Unlock()
			r.persistMu.Unlock()
			continue
		}
		r.models = next
		r.revision++
		// modelRevisions is deliberately left alone. It guards a benchmark's
		// results against a concurrent change to the provider order, and a
		// refusal is not that kind of change: selection re-reads the refusal
		// list when it commits, so letting a discovery invalidate the run in
		// flight would only throw away good measurements — and a guardrail
		// change is precisely when several discoveries land at once.
		r.mu.Unlock()
		r.persistMu.Unlock()
		return true, persistErr
	}
}

// blockedProviders returns the refusals currently in force for model, keyed by
// provider tag. Entries past blockedProviderTTL are omitted, so a stale block
// stops excluding its provider and the next benchmark probes it again.
func (r *routingState) blockedProviders(model string) map[string]blockedProvider {
	r.mu.RLock()
	cfg := r.models[model]
	blocked := append([]blockedProvider(nil), cfg.Blocked...)
	r.mu.RUnlock()
	return blockedSet(providerConfig{Blocked: blocked}, r.now())
}

// markBlocked records that OpenRouter refused to route to provider for model
// on policy grounds. An automatic pin resting on it moves to the first
// routable provider in the ranked order, preserving the cache affinity the
// pin was meant to provide after the refused request is re-routed. It reports
// whether this is a newly discovered block, so the caller can log the
// discovery once rather than on every refusal.
func (r *routingState) markBlocked(model, provider, reason string) (bool, error) {
	if provider == "" {
		return false, nil
	}
	now := r.now()
	changed, err := r.mutateModelConfig(model, func(cfg *providerConfig) bool {
		for i, entry := range cfg.Blocked {
			if entry.Provider != provider {
				continue
			}
			if entry.Reason == reason && entry.fresh(now) {
				return false
			}
			cfg.Blocked[i] = blockedProvider{Provider: provider, Reason: reason, DetectedAt: now}
			// A stale refusal may have stopped affecting a subsequent catalog
			// refresh, allowing the provider back to the head of the order. Once
			// the refusal is observed again, restore the same demotion applied to
			// a newly discovered block.
			cfg.Order = demoteBlocked(cfg.Order, cfg.Blocked, now)
			return true
		}
		cfg.Blocked = append(cfg.Blocked, blockedProvider{Provider: provider, Reason: reason, DetectedAt: now})
		sort.Slice(cfg.Blocked, func(i, j int) bool { return cfg.Blocked[i].Provider < cfg.Blocked[j].Provider })
		// A blocked provider cannot hold the head of the routable order.
		// Moving it to the tail keeps it visible to the dashboard while the
		// providers ahead of it are the ones a request can actually reach.
		cfg.Order = demoteBlocked(cfg.Order, cfg.Blocked, now)
		return true
	})
	if !changed {
		return false, err
	}
	// The block is in force even if it could not be written to disk, so the
	// pin has to move regardless of err.
	//
	// An automatic pin is orr's own choice and must move. A manual pin is the
	// user's instruction and is left exactly where they put it: requests keep
	// going there and keep coming back refused, which is the answer to the
	// question they asked.
	r.mu.Lock()
	if pin, ok := r.pins[model]; ok && !pin.manual && pin.provider == provider {
		delete(r.pins, model)
		cfg := r.models[model]
		order := cfg.Order
		if len(order) == 0 {
			order = cfg.Only
		}
		for _, candidate := range order {
			if candidate == provider || isBlockedLocked(cfg, candidate, now) {
				continue
			}
			r.pins[model] = pinnedProvider{provider: candidate, pinnedAt: now}
			break
		}
	}
	// A startup-restored pin still waiting for its endpoints is dropped only
	// when it names the provider that was just refused; a pending pin for
	// another provider is unaffected by this block.
	if pending, ok := r.pendingPins[model]; ok {
		if tag, resolved := r.resolveProviderLocked(model, pending.Provider); pending.Provider == provider || (resolved && tag == provider) {
			delete(r.pendingPins, model)
		}
	}
	r.mu.Unlock()
	return true, err
}

// markProviderUnavailableAndFailover records a transient provider failure and
// moves an automatic pin away from it. This covers an exhausted rate limit and
// an endpoint that disappeared from the model between catalog refreshes.
// persistMu serializes this health transition with
// benchmark commits: either the benchmark applies first and this method fixes
// its pin afterward, or this method applies first and the benchmark sees the
// exclusion. Manual pins remain authoritative.
//
// The boolean reports that the failure was accepted for automatic routing.
// The replacement may already be current when a concurrent benchmark changed
// the pin first. An empty replacement means no eligible fallback was available
// in the current order.
func (r *routingState) markProviderUnavailableAndFailover(model, provider string) (string, bool) {
	if model == "" || provider == "" {
		return "", false
	}
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg := r.models[model]
	pin, pinned := r.pins[model]
	if pin.manual || cfg.ManualPin != "" {
		return "", false
	}
	now := r.now()
	limited := r.temporarilyUnavailable[model]
	if limited == nil {
		limited = make(map[string]time.Time)
		r.temporarilyUnavailable[model] = limited
	}
	limited[provider] = now
	if pinned && pin.provider != provider && !providerUnavailableLocked(limited, pin.provider, now) && !isBlockedLocked(cfg, pin.provider, now) {
		return pin.provider, true
	}
	order := cfg.Order
	if len(order) == 0 {
		order = cfg.Only
	}
	for _, candidate := range order {
		if candidate == "" || candidate == provider || providerUnavailableLocked(limited, candidate, now) || isBlockedLocked(cfg, candidate, now) {
			continue
		}
		delete(r.pendingPins, model)
		r.pins[model] = pinnedProvider{provider: candidate, pinnedAt: now}
		return candidate, true
	}
	delete(r.pins, model)
	delete(r.pendingPins, model)
	return "", true
}

func providerUnavailableLocked(providers map[string]time.Time, provider string, now time.Time) bool {
	detectedAt, ok := providers[provider]
	return ok && now.Sub(detectedAt) < measuredMetricsTTL
}

func (r *routingState) temporarilyUnavailableProviders(model string) map[string]bool {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	providers := r.temporarilyUnavailable[model]
	recent := make(map[string]bool, len(providers))
	for provider, detectedAt := range providers {
		if now.Sub(detectedAt) >= measuredMetricsTTL {
			delete(providers, provider)
			continue
		}
		recent[provider] = true
	}
	if len(providers) == 0 {
		delete(r.temporarilyUnavailable, model)
	}
	return recent
}

// clearProviderUnavailable forgets the transient exclusion only when the
// successful request began after the exclusion was recorded. An older request
// can finish late after concurrent failures have already moved the provider
// out; that stale success must not immediately rehabilitate it.
func (r *routingState) clearProviderUnavailable(model, provider string, requestStarted time.Time) {
	if model == "" || provider == "" || requestStarted.IsZero() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	providers := r.temporarilyUnavailable[model]
	detectedAt, ok := providers[provider]
	if !ok || requestStarted.Before(detectedAt) {
		return
	}
	delete(providers, provider)
	if len(providers) == 0 {
		delete(r.temporarilyUnavailable, model)
	}
}

// clearBlocked forgets a recorded refusal because provider answered for model.
func (r *routingState) clearBlocked(model, provider string) error {
	if provider == "" {
		return nil
	}
	_, err := r.mutateModelConfig(model, func(cfg *providerConfig) bool {
		kept := make([]blockedProvider, 0, len(cfg.Blocked))
		for _, entry := range cfg.Blocked {
			if entry.Provider != provider {
				kept = append(kept, entry)
			}
		}
		if len(kept) == len(cfg.Blocked) {
			return false
		}
		cfg.Blocked = kept
		return true
	})
	return err
}

// clearAllBlocked forgets every recorded refusal for model, so the next
// benchmark re-probes all of them. A manual refresh calls it: re-reading the
// catalog is the point at which a user who just edited a guardrail expects
// orr to find out.
func (r *routingState) clearAllBlocked(model string) error {
	_, err := r.mutateModelConfig(model, func(cfg *providerConfig) bool {
		if len(cfg.Blocked) == 0 {
			return false
		}
		cfg.Blocked = nil
		return true
	})
	return err
}

// demoteBlocked moves blocked providers to the end of order, preserving the
// relative order within each group.
func demoteBlocked(order []string, blocked []blockedProvider, now time.Time) []string {
	set := blockedSet(providerConfig{Blocked: blocked}, now)
	if len(set) == 0 || len(order) == 0 {
		return order
	}
	routable, unroutable := partitionBlocked(order, set)
	return append(routable, unroutable...)
}

// demoteFailing moves providers with any recent API error to the end of
// order, preserving the relative order within each group. They stay on the
// order as candidates — an erroring provider is still better than none — but
// they cannot become the next automatic selection.
func demoteFailing(order []string, apiErrors map[string]float64) []string {
	if len(apiErrors) == 0 || len(order) == 0 {
		return order
	}
	answering, failing := make([]string, 0, len(order)), make([]string, 0)
	for _, provider := range order {
		if apiErrors[provider] > 0 {
			failing = append(failing, provider)
			continue
		}
		answering = append(answering, provider)
	}
	return append(answering, failing...)
}

func demoteTemporarilyUnavailable(order []string, unavailable map[string]bool) []string {
	if len(unavailable) == 0 || len(order) == 0 {
		return order
	}
	available, limited := make([]string, 0, len(order)), make([]string, 0)
	for _, provider := range order {
		if unavailable[provider] {
			limited = append(limited, provider)
			continue
		}
		available = append(available, provider)
	}
	return append(available, limited...)
}

func (r *routingState) modelsSnapshot() map[string]providerConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneProviderConfigs(r.models)
}

func validModelID(model string) bool {
	parts := strings.SplitN(model, "/", 2)
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

func (r *routingState) setCurrentModel(model string) {
	if model == "" {
		return
	}
	r.mu.Lock()
	r.currentModel = model
	r.mu.Unlock()
}

func (r *routingState) current() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.currentModel
}

// pinnedProvider is the provider requests are routed to.
//
// A manual pin is an instruction, not a preference: it is returned even when
// the provider is known to be blocked, so the request goes there and the
// refusal comes back. Routing around it would answer a question the user did
// not ask and hide the one thing they were trying to see. An automatic pin is
// orr's own choice, so a blocked or temporarily unavailable one is dropped and
// the ranked order is used instead.
func (r *routingState) pinnedProvider(model string) string {
	provider, manual := r.pinInfo(model)
	if !manual && (r.isBlocked(model, provider) || r.isTemporarilyUnavailable(model, provider)) {
		return ""
	}
	return provider
}

func (r *routingState) isTemporarilyUnavailable(model, provider string) bool {
	if provider == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return providerUnavailableLocked(r.temporarilyUnavailable[model], provider, r.now())
}

// isBlocked reports whether provider is currently refused for model. It scans
// the recorded refusals in place: the list holds at most one entry per
// endpoint, and this sits on the request path.
func (r *routingState) isBlocked(model, provider string) bool {
	if provider == "" {
		return false
	}
	now := r.now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return isBlockedLocked(r.models[model], provider, now)
}

func isBlockedLocked(cfg providerConfig, provider string, now time.Time) bool {
	_, blocked := blockedEntryLocked(cfg, provider, now)
	return blocked
}

func blockedEntryLocked(cfg providerConfig, provider string, now time.Time) (blockedProvider, bool) {
	for _, entry := range cfg.Blocked {
		if entry.Provider == provider {
			return entry, entry.fresh(now)
		}
	}
	return blockedProvider{}, false
}

func (r *routingState) pinInfo(model string) (string, bool) {
	r.mu.RLock()
	pin, ok := r.pins[model]
	if !ok {
		r.mu.RUnlock()
		return "", false
	}
	if !pin.manual && r.now().Sub(pin.pinnedAt) > r.pinTTL {
		// Upgrade to a write lock only when an expired pin must be deleted.
		r.mu.RUnlock()
		r.mu.Lock()
		pin, ok = r.pins[model]
		if !ok {
			r.mu.Unlock()
			return "", false
		}
		if !pin.manual && r.now().Sub(pin.pinnedAt) > r.pinTTL {
			delete(r.pins, model)
		}
		r.mu.Unlock()
		return "", false
	}
	r.mu.RUnlock()
	return pin.provider, pin.manual
}

func (r *routingState) resolveProvider(model, provider string) (string, bool) {
	if provider == "" {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolveProviderLocked(model, provider)
}

func (r *routingState) pin(model, provider string) {
	if model == "" || provider == "" {
		return
	}
	r.mu.Lock()
	provider = r.providerTagLocked(model, provider)
	r.pins[model] = pinnedProvider{provider: provider, pinnedAt: r.now(), manual: true}
	r.mu.Unlock()
}

func (r *routingState) autoPin(model, provider string) bool {
	if model == "" || provider == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	resolved, ok := r.resolveProviderLocked(model, provider)
	if !ok {
		// Keep direct callers compatible with provider tags that have not been
		// observed yet. Proxy response handling checks resolution first.
		resolved = provider
	}
	// A manual user pin always overrides automatic sticky routing.
	if existing, ok := r.pins[model]; ok && existing.manual {
		return false
	}
	// Sticky routing to a provider this account cannot reach would turn every
	// following request into the same refusal.
	if isBlockedLocked(r.models[model], resolved, r.now()) {
		return false
	}
	if providerUnavailableLocked(r.temporarilyUnavailable[model], resolved, r.now()) {
		return false
	}
	delete(r.pendingPins, model)
	r.pins[model] = pinnedProvider{provider: resolved, pinnedAt: r.now(), manual: false}
	return true
}

// ensureAutoPin turns the provider selected from the ranked order into the
// model's sticky automatic route when no live pin exists. Automatic selection
// is a real pin, not an implicit preference: callers can therefore render the
// same state that requests use, and the request that creates it can disable
// fallbacks before it is sent upstream.
func (r *routingState) ensureAutoPin(model, provider string) bool {
	if model == "" || provider == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if existing, ok := r.pins[model]; ok {
		if existing.manual {
			return false
		}
		if now.Sub(existing.pinnedAt) <= r.pinTTL && !isBlockedLocked(r.models[model], existing.provider, now) && !providerUnavailableLocked(r.temporarilyUnavailable[model], existing.provider, now) {
			return false
		}
		delete(r.pins, model)
	}
	resolved, ok := r.resolveProviderLocked(model, provider)
	if !ok || isBlockedLocked(r.models[model], resolved, now) || providerUnavailableLocked(r.temporarilyUnavailable[model], resolved, now) {
		return false
	}
	delete(r.pendingPins, model)
	r.pins[model] = pinnedProvider{provider: resolved, pinnedAt: now}
	return true
}

func (r *routingState) unpin(model string) {
	if model == "" {
		return
	}
	r.mu.Lock()
	delete(r.pins, model)
	delete(r.pendingPins, model)
	r.mu.Unlock()
}

func (r *routingState) restorePins(pins map[string]persistedPin) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for model, pin := range pins {
		if model == "" || pin.Provider == "" {
			continue
		}
		if existing, ok := r.pins[model]; ok && existing.manual {
			continue
		}
		if pin.PinnedAt.IsZero() {
			// A pin persisted without a timestamp (for example from an older
			// stats file) would otherwise look expired immediately.
			pin.PinnedAt = r.now()
		}
		provider, resolved := r.resolveProviderLocked(model, pin.Provider)
		if !resolved {
			// The endpoint cache is not populated yet at startup; keep the pin
			// pending and resolve it once endpoints for the model are fetched.
			// A pin the persisted refusal list already refuses is dropped
			// instead: restoring it would show a pin requests never use.
			if isBlockedLocked(r.models[model], pin.Provider, r.now()) {
				continue
			}
			r.pendingPins[model] = pin
			continue
		}
		if isBlockedLocked(r.models[model], provider, r.now()) {
			continue
		}
		delete(r.pendingPins, model)
		r.pins[model] = pinnedProvider{provider: provider, pinnedAt: pin.PinnedAt}
	}
}

// resolvePendingPins moves a startup-restored automatic pin into the active
// pins once its provider can be resolved against the fetched endpoints.
func (r *routingState) resolvePendingPins(model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pin, ok := r.pendingPins[model]
	if !ok {
		return
	}
	provider, resolved := r.resolveProviderLocked(model, pin.Provider)
	if !resolved {
		return
	}
	delete(r.pendingPins, model)
	if existing, ok := r.pins[model]; ok && existing.manual {
		return
	}
	if isBlockedLocked(r.models[model], provider, r.now()) {
		// The provider was refused while the pin waited for its endpoints.
		return
	}
	r.pins[model] = pinnedProvider{provider: provider, pinnedAt: pin.PinnedAt}
}

// providerTagLocked is the lock-free variant for callers that already hold r.mu.
func (r *routingState) providerTagLocked(model, provider string) string {
	tag, _ := r.resolveProviderLocked(model, provider)
	return tag
}

func (r *routingState) resolveProviderLocked(model, provider string) (string, bool) {
	if provider == "" {
		return "", false
	}
	endpoints := r.endpointCache[model]
	for _, ep := range endpoints {
		if ep.Tag == provider {
			return provider, true
		}
	}

	resolveByName := func(tags []string) (string, bool) {
		for _, tag := range tags {
			for _, ep := range endpoints {
				if ep.Tag == tag && strings.EqualFold(ep.ProviderName, provider) {
					return ep.Tag, true
				}
			}
			if tag == provider {
				return tag, true
			}
		}
		return "", false
	}

	// Prefer the endpoint used by the current pin when the response only has
	// the provider display name. Expired automatic pins must not steer
	// resolution.
	if pin, ok := r.pins[model]; ok && pin.provider != "" && (pin.manual || r.now().Sub(pin.pinnedAt) <= r.pinTTL) {
		if tag, ok := resolveByName([]string{pin.provider}); ok {
			return tag, true
		}
	}
	if cfg, ok := r.models[model]; ok {
		if tag, ok := resolveByName(cfg.Order); ok {
			return tag, true
		}
		if tag, ok := resolveByName(cfg.Only); ok {
			return tag, true
		}
	}
	if names, ok := r.nameMap[model]; ok {
		if tag, ok := names[provider]; ok {
			return tag, true
		}
		for name, tag := range names {
			if strings.EqualFold(name, provider) {
				return tag, true
			}
		}
	}
	return provider, false
}

func (r *routingState) providers(model string, observed []string) []string {
	configured, _ := r.modelConfig(model)
	base := configured.Order
	if len(base) == 0 {
		base = configured.Only
	}
	if len(base) > 0 {
		providers := append([]string(nil), base...)
		providers = demoteBlocked(providers, configured.Blocked, r.now())
		providers = demoteTemporarilyUnavailable(providers, r.temporarilyUnavailableProviders(model))
		return includeProvider(providers, r.pinnedProvider(model))
	}
	// No ranked Order yet: rank compatible endpoints from the cached catalog
	// when available so the TUI shows the same order as a manual `r` refresh.
	// Fall back to observation order only when the catalog hasn't been
	// fetched yet.
	if ranked := r.rankedTagsFromCache(model); len(ranked) > 0 {
		return includeProvider(ranked, r.pinnedProvider(model))
	}
	providers := append([]string(nil), observed...)
	return includeProvider(providers, r.pinnedProvider(model))
}

// rankedTagsFromCache returns the cached endpoint tags for model ordered by
// throughput, cache-read price, and latency — the same ranking used by `r`,
// restricted to compatible endpoints. Returns nil when the cache is empty or
// holds no compatible endpoints so the caller can fall back to the observed
// provider list.
func (r *routingState) rankedTagsFromCache(model string) []string {
	r.mu.RLock()
	cached := append([]endpointMeta(nil), r.endpointCache[model]...)
	r.mu.RUnlock()
	if len(cached) == 0 {
		return nil
	}
	compatible := make([]endpointMeta, 0, len(cached))
	for _, ep := range cached {
		if ep.Compatible {
			compatible = append(compatible, ep)
		}
	}
	if len(compatible) == 0 {
		return nil
	}
	sort.SliceStable(compatible, func(i, j int) bool {
		return rankLess(metaRankFields(compatible[i]), metaRankFields(compatible[j]))
	})
	tags := make([]string, len(compatible))
	for i, ep := range compatible {
		tags[i] = ep.Tag
	}
	return tags
}

// metaRankFields adapts a cached endpoint to the shared ranking fields.
func metaRankFields(ep endpointMeta) rankFields {
	cachePrice := ep.Pricing.InputCacheRead.float()
	return rankFields{
		throughput: ep.Throughput,
		cachePrice: cachePrice,
		hasCache:   cachePrice > 0,
		latency:    ep.Latency,
	}
}

func (r *routingState) active(model string, observed []string) string {
	if selected := r.pinnedProvider(model); selected != "" {
		return selected
	}
	providers := r.providers(model, observed)
	if len(providers) > 0 {
		return providers[0]
	}
	return ""
}

func includeProvider(providers []string, provider string) []string {
	if provider == "" {
		return providers
	}
	for _, existing := range providers {
		if existing == provider {
			return providers
		}
	}
	return append([]string{provider}, providers...)
}

func (r *routingState) endpoints(model string) []endpointMeta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]endpointMeta(nil), r.endpointCache[model]...)
}

func (r *routingState) refreshEndpoints(model string, client *http.Client, upstream, apiKey string) {
	r.mu.Lock()
	if model == "" {
		r.mu.Unlock()
		return
	}
	if last, ok := r.endpointFetchedAt[model]; ok && r.now().Sub(last) < endpointCacheTTL {
		r.mu.Unlock()
		return
	}
	// Mark as fetched now so concurrent requests don't pile up.
	r.endpointFetchedAt[model] = r.now()
	r.mu.Unlock()

	result, err := fetchEndpoints(context.Background(), client, upstream, apiKey, model)
	if err != nil {
		// Do not burn the TTL on a failed fetch: clear the marker so the next
		// request retries instead of waiting out the full cache window.
		r.mu.Lock()
		delete(r.endpointFetchedAt, model)
		r.mu.Unlock()
		log.Printf("refresh endpoints for %s: %v", model, err)
		return
	}
	meta, nameMap := endpointMetaFromList(result)
	r.mu.Lock()
	r.endpointCache[model] = meta
	r.nameMap[model] = nameMap
	r.endpointFetchedAt[model] = r.now()
	r.mu.Unlock()
	r.resolvePendingPins(model)
}

func (r *routingState) refreshModelOrder(model string, client *http.Client, upstream, apiKey string, maxProviders int, cacheOnly bool) {
	if !validModelID(model) {
		return
	}
	r.mu.RLock()
	refreshRevision := r.modelRevisions[model]
	r.mu.RUnlock()
	result, err := fetchEndpoints(context.Background(), client, upstream, apiKey, model)
	if err != nil {
		log.Printf("refresh provider order for %s: %v", model, err)
		return
	}
	endpoints := compatibleEndpoints(result.Data.Endpoints)
	endpoints = rankUpdateEndpoints(endpoints, cacheOnly)
	if len(endpoints) == 0 {
		return
	}
	ranked := make([]string, len(endpoints))
	for i, endpoint := range endpoints {
		ranked[i] = endpoint.Tag
	}
	// Blocked providers are held out of the maxProviders budget rather than
	// counted against it. Letting them consume slots is how a model with a
	// strict guardrail ends up carrying a short list of mostly unreachable
	// providers while reachable ones are truncated away; they are appended
	// afterward so the dashboard can still show them, marked broken.
	r.mu.RLock()
	blocked := blockedSet(r.models[model], r.now())
	r.mu.RUnlock()
	routable, unroutable := partitionBlocked(ranked, blocked)
	if len(routable) == 0 {
		// Every endpoint this catalog offers is refused. Keep the ranking so
		// the dashboard has something to describe, and let the refusals be
		// re-probed when they expire.
		routable, unroutable = ranked, nil
	}
	limit := min(max(1, maxProviders), len(routable))
	order := append(append(make([]string, 0, limit+len(unroutable)), routable[:limit]...), unroutable...)

	meta, nameMap := endpointMetaFromList(result)

	// Serialize the commit and the providers-file write so a concurrent
	// mutation cannot leave the on-disk file missing a committed change.
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	r.mu.Lock()
	if r.modelRevisions[model] != refreshRevision {
		r.mu.Unlock()
		return
	}
	modelConfig := r.models[model]
	modelConfig.Order = order
	modelConfig.UpdatedAt = r.now()
	if modelConfig.ManualPin != "" && !containsString(order, modelConfig.ManualPin) {
		modelConfig.Order = append(modelConfig.Order, modelConfig.ManualPin)
	}
	r.models[model] = modelConfig
	r.endpointCache[model] = meta
	r.endpointFetchedAt[model] = r.now()
	r.nameMap[model] = nameMap
	r.modelRevisions[model]++
	r.revision++
	snapshot := cloneProviderConfigs(r.models)
	r.mu.Unlock()

	r.resolvePendingPins(model)
	// Persist outside the routing lock; a failed write only loses the refresh
	// until the next one, so log it instead of failing the request.
	if r.providersPath != "" {
		if err := writeProvidersFileAtomic(r.providersPath, snapshot); err != nil {
			log.Printf("persist provider order for %s: %v", model, err)
		}
	}
}

// providerTestResult is the measured outcome used to rank one provider after
// an explicit or automatic all-provider benchmark. tps and ttft are the two
// measurements selection needs: generation speed, and how long the provider
// takes to start answering. latency is the total request duration, kept for
// display because it also contains the generation time and so grows with the
// response length, which makes it useless for comparing providers.
type providerTestResult struct {
	provider string
	tps      float64
	ttft     time.Duration
	latency  time.Duration
	samples  int
	err      error
}

func newProviderTestResult(provider string, sample providerTestSample, err error) providerTestResult {
	return providerTestResult{
		provider: provider,
		tps:      sample.tps,
		ttft:     sample.ttft,
		latency:  sample.latency,
		samples:  sample.samples,
		err:      err,
	}
}

// hasResponsiveness reports whether ttft is trustworthy enough to time this
// provider with. A measurement summarizing fewer than
// benchmarkMinLatencySamples requests is treated as unknown, so one slow
// request cannot demote a provider.
func (r providerTestResult) hasResponsiveness() bool {
	return r.ttft > 0 && (r.samples <= 0 || r.samples >= benchmarkMinLatencySamples)
}

// answered reports whether this test failed because the provider replied with
// an error, rather than because orr cancelled it. Only the former is evidence
// about the provider, so only the former may override pooled measurements.
func (r providerTestResult) answered() bool {
	return r.err != nil && errors.Is(r.err, errProviderAnswered)
}

// mergeMeasuredProviderResults ranks providers on the pooled benchmark
// measurements rather than on the single run that just finished.
//
// The pool is benchmark rounds only, across runs. That is what makes the
// comparison valid as well as stable: client traffic reaches only the pinned
// provider and carries much longer generations than a ping, so measuring the
// incumbent on real traffic and its challengers on pings would report the
// difference in the work as a difference in the providers. Measured against
// this model's real providers, ranking on one run's median put the client on a
// provider costing 46% more per request than the best available; ranking on the
// pool brings that to a fraction of a percent.
//
// A fresh successful benchmark is used for any provider the pool has nothing
// for, and a test orr cancelled — a deadline, a shutdown — never discards
// pooled measurements, because it measured nothing.
//
// A test the provider itself answered with an error is the exception, and it
// has to be: the pool holds successes only and keeps them for a week, so a
// provider that has since started refusing every request would otherwise go on
// presenting last week's throughput as its current state. That is how a
// provider blocked by a guardrail could still read as a live service with
// data — the failure was replaced by the memory of it working.
func mergeMeasuredProviderResults(results []providerTestResult, pool map[string][]benchmarkSample, providers []string, now time.Time) []providerTestResult {
	byProvider := make(map[string]providerTestResult, len(results))
	for _, result := range results {
		if result.provider != "" {
			byProvider[result.provider] = result
		}
	}

	merged := make([]providerTestResult, 0, max(len(results), len(providers)))
	seen := make(map[string]bool, len(providers))
	for _, provider := range providers {
		if provider == "" || seen[provider] {
			continue
		}
		seen[provider] = true
		result, tested := byProvider[provider]
		if tested && result.answered() {
			merged = append(merged, result)
			continue
		}
		if pooled, ok := pooledProviderResult(provider, pool[provider], now); ok {
			merged = append(merged, pooled)
			continue
		}
		if tested {
			merged = append(merged, result)
		}
	}
	for _, result := range results {
		if !seen[result.provider] {
			merged = append(merged, result)
		}
	}
	return merged
}

// pooledProviderResult summarizes one provider's pooled benchmark rounds into
// a ranking input. Both figures are medians, so a single slow round among them
// changes nothing.
func pooledProviderResult(provider string, samples []benchmarkSample, now time.Time) (providerTestResult, bool) {
	cutoff := now.Add(-benchmarkSampleTTL)
	throughputs := make([]float64, 0, len(samples))
	ttfts := make([]float64, 0, len(samples))
	for _, sample := range samples {
		if sample.Time.Before(cutoff) || sample.TPS <= 0 {
			continue
		}
		throughputs = append(throughputs, sample.TPS)
		if sample.TTFTms > 0 {
			ttfts = append(ttfts, sample.TTFTms)
		}
	}
	if len(throughputs) == 0 {
		return providerTestResult{}, false
	}
	result := providerTestResult{
		provider: provider,
		tps:      medianFloat(throughputs),
		ttft:     time.Duration(medianFloat(ttfts) * float64(time.Millisecond)),
		samples:  len(ttfts),
	}
	return result, true
}

// applyProviderTestResults replaces the configured order with successful
// benchmark results first, ranked by the deal each provider offers on one
// reference request: its cost in dollars against its duration in seconds. See
// rankProviderDeals. Failed or unfinished providers keep their previous
// relative order. The automatic pin follows the winner; a manual pin is never
// changed. The boolean result reports that the automatic pin was either set or
// cleared, allowing callers to mirror the update into persisted stats.
func (r *routingState) applyProviderTestResults(model string, results []providerTestResult) (string, bool, error) {
	return r.applyProviderTestResultsAtRevision(model, results, benchmarkReferenceProfile, r.modelRevision(model), nil)
}

// applyProviderTestResultsAtRevision applies results only while the provider
// configuration is still the one that was benchmarked. A concurrent refresh
// or manual-pin change wins instead of being overwritten by stale samples.
//
// apiErrors carries each provider's recent API error share (0..1) as the
// dashboard reports it. Automatic mode only selects providers that answer: a
// single failed response in the window disqualifies a provider from the pin,
// however good its pooled benchmark looks, because the pool holds successes
// only and would otherwise keep presenting an erroring provider by its last
// good week. A disqualified provider keeps its measured place in the fallback
// order, and a manual pin is untouched — it is the user's instruction, and
// its failures are the answer to the question they asked.
func (r *routingState) applyProviderTestResultsAtRevision(model string, results []providerTestResult, profile requestProfile, expectedRevision uint64, apiErrors map[string]float64) (string, bool, error) {
	if !validModelID(model) || len(results) == 0 {
		return "", false, nil
	}

	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	r.mu.Lock()
	cfg, ok := r.models[model]
	if !ok || r.modelRevisions[model] != expectedRevision {
		r.mu.Unlock()
		return "", false, nil
	}
	meta := append([]endpointMeta(nil), r.endpointCache[model]...)
	metaByTag := make(map[string]endpointMeta, len(meta))
	for _, ep := range meta {
		metaByTag[ep.Tag] = ep
	}
	previous := append([]string(nil), cfg.Order...)
	now := r.now()
	blocked := blockedSet(cfg, now)
	effectiveErrors := make(map[string]float64, len(apiErrors)+len(r.temporarilyUnavailable[model]))
	for provider, rate := range apiErrors {
		effectiveErrors[provider] = rate
	}
	for provider, detectedAt := range r.temporarilyUnavailable[model] {
		if now.Sub(detectedAt) < measuredMetricsTTL {
			effectiveErrors[provider] = 1
		}
	}
	apiErrors = effectiveErrors
	// The provider that already holds the automatic pin gets the churn margin,
	// so an equally good challenger does not take the pin from it.
	incumbent := ""
	if pin, ok := r.pins[model]; ok && !pin.manual && now.Sub(pin.pinnedAt) <= r.pinTTL {
		incumbent = pin.provider
	}
	r.mu.Unlock()

	position := make(map[string]int, len(previous))
	for i, provider := range previous {
		position[provider] = i
	}
	successful := make([]providerTestResult, 0, len(results))
	seen := make(map[string]bool, len(results))
	for _, result := range results {
		if result.provider == "" || result.err != nil || result.tps <= 0 || math.IsNaN(result.tps) || math.IsInf(result.tps, 0) || seen[result.provider] {
			continue
		}
		// A blocked provider can still carry pooled measurements from before
		// it was refused, and those measurements are real — it was genuinely
		// that fast, last week. Ranking on them would hand the pin to a
		// provider no request can reach, so the refusal disqualifies it here
		// regardless of how good its numbers look.
		if _, isBlocked := blocked[result.provider]; isBlocked {
			continue
		}
		seen[result.provider] = true
		successful = append(successful, result)
	}
	if len(successful) == 0 {
		hasRecentErrors := false
		for _, rate := range apiErrors {
			if rate > 0 {
				hasRecentErrors = true
				break
			}
		}
		if !hasRecentErrors {
			// A fully cancelled run measured nothing and learned nothing about
			// provider health. Preserve the current order and automatic pin.
			return "", false, nil
		}
	}
	deals := rankProviderDeals(successful, profile, metaByTag, position, incumbent)

	// Providers with recent API errors cannot become the next automatic
	// selection, however well they benchmarked: the deal
	// ranking never sees their failures, so they are taken out of the ranking
	// here and re-appended at the tail, where they remain as fallbacks.
	// demoteFailing after this catches anything the disqualification missed.
	ranked := make([]providerDeal, 0, len(deals))
	var failing []string
	for _, deal := range deals {
		if apiErrors[deal.result.provider] > 0 {
			failing = append(failing, deal.result.provider)
			continue
		}
		ranked = append(ranked, deal)
	}

	order := make([]string, 0, len(previous))
	for _, deal := range ranked {
		order = append(order, deal.result.provider)
	}
	for _, provider := range previous {
		if !containsString(order, provider) && !containsString(failing, provider) {
			order = append(order, provider)
		}
	}
	order = append(order, failing...)
	if cfg.ManualPin != "" && !containsString(order, cfg.ManualPin) {
		order = append(order, cfg.ManualPin)
	}

	r.mu.Lock()
	next := cloneProviderConfigs(r.models)
	cfg = next[model]
	// Demote against the refusal list as it stands now, not as it stood when
	// the benchmark started: a refusal discovered during the run has to reach
	// the tail of the order it is about to write. Erroring providers follow:
	// they are kept on the order as fallbacks, but a provider failing every
	// recent request cannot be the head requests start from.
	cfg.Order = demoteBlocked(order, cfg.Blocked, now)
	if len(apiErrors) > 0 {
		cfg.Order = demoteFailing(cfg.Order, apiErrors)
	}
	next[model] = cfg
	r.mu.Unlock()
	if r.providersPath != "" {
		if err := writeProvidersFileAtomic(r.providersPath, next); err != nil {
			return "", false, fmt.Errorf("persist benchmarked provider order for %s: %w", model, err)
		}
	}

	r.mu.Lock()
	r.models = next
	r.modelRevisions[model]++
	r.revision++
	// The winner is the best deal among providers that were routable when the
	// run was ranked; ranked already excludes every provider with a recent API
	// error. A refusal that arrived since — the benchmark itself is what
	// discovers them — disqualifies it here too, rather than pinning the
	// client to a provider already known to be unreachable.
	best := ""
	for _, deal := range ranked {
		if isBlockedLocked(cfg, deal.result.provider, now) {
			continue
		}
		best = deal.result.provider
		break
	}
	autoUpdated := best != "" && cfg.ManualPin == ""
	if autoUpdated {
		delete(r.pendingPins, model)
		r.pins[model] = pinnedProvider{provider: best, pinnedAt: now}
	} else if cfg.ManualPin == "" {
		// A strict automatic pin overrides the fallback order. If its provider
		// has become ineligible and there is no clean measured replacement,
		// remove it so requests can use the newly demoted order and fallbacks.
		// Keep an otherwise-clean incumbent when this run merely failed to
		// measure a challenger.
		if pin, ok := r.pins[model]; ok && !pin.manual && (apiErrors[pin.provider] > 0 || isBlockedLocked(cfg, pin.provider, now)) {
			delete(r.pins, model)
			autoUpdated = true
		}
	}
	r.mu.Unlock()
	return best, autoUpdated, nil
}

// providerDeal is one provider priced and timed against the reference request.
// score is the cost-delay product: dollars per request multiplied by seconds
// per request. Lower is a better deal, and because both factors are relative,
// a provider only wins on price when the discount is proportionally larger than
// the speed it gives up, and only wins on speed when the time it saves is
// proportionally larger than the premium it charges. That is what keeps a
// marginally faster provider from being selected at any price, and what lets a
// slightly slower provider win on a real discount.
type providerDeal struct {
	result   providerTestResult
	cost     float64
	seconds  float64
	score    float64
	priced   bool
	timed    bool
	viable   bool
	position int
}

// rankProviderDeals orders benchmarked providers by the deal each offers.
//
// Every provider is priced and timed against the same reference request, so
// the comparison is between two real quantities — dollars and seconds — rather
// than between a throughput number and three price columns that have no common
// unit. Throughput and time to first token are combined into the request's
// duration, which is the only form in which the client experiences either.
//
// A viability ceiling bounds how much time a cheaper provider may cost:
// benchmarkTimeSacrifice times the field's median duration. Anchoring it to
// the median rather than to the fastest provider stops one exaggerated
// provider from setting the terms in either direction — a very fast, very
// expensive provider cannot pull the ceiling down onto the balanced middle of
// the field, and a provider far slower than everyone else cannot stay viable
// by being cheap. Because the ceiling is above the median by construction, at
// least one provider is always viable and the selection can never be left with
// nothing to choose.
//
// Providers with an incomplete price, an untrustworthy first-token
// measurement, or a duration past the ceiling rank behind the ones without
// those problems rather than being dropped: they are still working providers
// and remain available as fallbacks.
func rankProviderDeals(results []providerTestResult, profile requestProfile, metaByTag map[string]endpointMeta, position map[string]int, incumbent string) []providerDeal {
	deals := make([]providerDeal, 0, len(results))
	durations := make([]float64, 0, len(results))
	for _, result := range results {
		deal := providerDeal{result: result, position: position[result.provider]}
		deal.cost, deal.priced = profile.cost(metaByTag[result.provider].Pricing)
		deal.seconds, deal.timed = profile.seconds(result)
		if deal.seconds > 0 {
			deal.score = deal.cost * math.Pow(deal.seconds, benchmarkTimeWeight)
			durations = append(durations, deal.seconds)
		}
		deals = append(deals, deal)
	}

	ceiling := math.Inf(1)
	if len(durations) > 0 {
		sort.Float64s(durations)
		ceiling = percentileFloat(durations, 0.5) * benchmarkTimeSacrifice
	}
	for i := range deals {
		deals[i].viable = deals[i].seconds > 0 && deals[i].seconds <= ceiling
	}

	sort.SliceStable(deals, func(i, j int) bool {
		return deals[i].better(deals[j])
	})
	return applyChurnMargin(deals, incumbent)
}

// better reports whether this deal outranks other. The tiers ahead of the
// score keep an unmeasured or unpriced provider from winning on a number that
// is only optimistic because something is missing from it.
func (d providerDeal) better(other providerDeal) bool {
	if d.viable != other.viable {
		return d.viable
	}
	if !d.viable {
		// Both are past the time ceiling and cannot be selected on a deal.
		// Order them by how close they came, so the fallback chain stays
		// sorted by what the client would actually wait.
		if d.seconds != other.seconds {
			return lessWithUnknownLast(d.seconds, other.seconds)
		}
		return d.position < other.position
	}
	if d.priced != other.priced {
		return d.priced
	}
	if d.timed != other.timed {
		return d.timed
	}
	if d.priced && d.score != other.score {
		return d.score < other.score
	}
	if d.seconds != other.seconds {
		return lessWithUnknownLast(d.seconds, other.seconds)
	}
	if d.cost != other.cost {
		return lessWithUnknownLast(d.cost, other.cost)
	}
	return d.position < other.position
}

// applyChurnMargin keeps the automatic pin with the provider that already
// holds it unless a challenger's deal is meaningfully better. Without the
// margin two providers whose scores differ by a fraction of a percent would
// trade the pin on every benchmark. The incumbent keeps its lead only while it
// is still viable and still measured; once it fails the time ceiling or loses
// its price, the challenger takes over immediately.
func applyChurnMargin(deals []providerDeal, incumbent string) []providerDeal {
	if incumbent == "" || len(deals) < 2 || deals[0].result.provider == incumbent {
		return deals
	}
	held := -1
	for i, deal := range deals {
		if deal.result.provider == incumbent {
			held = i
			break
		}
	}
	if held < 0 {
		return deals
	}
	current, challenger := deals[held], deals[0]
	if !current.viable || !current.priced || !current.timed {
		return deals
	}
	if challenger.priced && challenger.timed && challenger.score <= current.score*(1-benchmarkChurnMargin) {
		return deals
	}
	reordered := append([]providerDeal{current}, deals[:held]...)
	return append(reordered, deals[held+1:]...)
}

// lessWithUnknownLast compares measurements ascending while sorting missing
// values (zero or negative) after every known one, so an absent measurement is
// never mistaken for the best one.
func lessWithUnknownLast(left, right float64) bool {
	leftMissing := left <= 0
	rightMissing := right <= 0
	if leftMissing != rightMissing {
		return !leftMissing
	}
	return left < right
}

func (r *routingState) maybeRefreshModelOrder(model string, client *http.Client, upstream, apiKey string, maxProviders int, cacheOnly bool) <-chan struct{} {
	if !validModelID(model) {
		return nil
	}
	r.mu.Lock()
	cfg, ok := r.models[model]
	if !ok {
		r.mu.Unlock()
		return nil
	}
	today := r.now().Format(time.DateOnly)
	if len(cfg.Order) > 0 && cfg.UpdatedAt.Format(time.DateOnly) == today {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	return r.launchModelOrderRefresh(model, client, upstream, apiKey, maxProviders, cacheOnly)
}

// forceRefreshModelOrder refreshes the provider order for model immediately,
// bypassing the once-per-day check. It is used for manual refreshes from the
// dashboard and still guards against concurrent refreshes of the same model.
func (r *routingState) forceRefreshModelOrder(model string, client *http.Client, upstream, apiKey string, maxProviders int, cacheOnly bool) {
	if !validModelID(model) {
		return
	}
	r.mu.Lock()
	if _, ok := r.models[model]; !ok {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	r.launchModelOrderRefresh(model, client, upstream, apiKey, maxProviders, cacheOnly)
}

func (r *routingState) launchModelOrderRefresh(model string, client *http.Client, upstream, apiKey string, maxProviders int, cacheOnly bool) <-chan struct{} {
	r.mu.Lock()
	if r.modelOrderUpdating[model] {
		done := r.modelOrderDone[model]
		r.mu.Unlock()
		return done
	}
	done := make(chan struct{})
	r.modelOrderUpdating[model] = true
	r.modelOrderDone[model] = done
	r.mu.Unlock()

	r.refreshWG.Add(1)
	go func() {
		defer r.refreshWG.Done()
		defer func() {
			r.mu.Lock()
			delete(r.modelOrderUpdating, model)
			delete(r.modelOrderDone, model)
			r.mu.Unlock()
			close(done)
		}()
		r.refreshModelOrder(model, client, upstream, apiKey, maxProviders, cacheOnly)
	}()
	return done
}

// waitForRefreshes blocks until in-flight provider-order refreshes finish, so
// shutdown does not race a providers.yaml write.
func (r *routingState) waitForRefreshes() {
	r.refreshWG.Wait()
}

// endpointMetaFromList converts a fetched endpoint list into the routing cache
// shape, sorted by tag, with a provider-name -> tag map that prefers the
// slash-less base tag and otherwise the lexicographically first variant.
func endpointMetaFromList(result endpointList) ([]endpointMeta, map[string]string) {
	meta := make([]endpointMeta, 0, len(result.Data.Endpoints))
	for _, ep := range result.Data.Endpoints {
		throughput, latency := 0.0, 0.0
		if ep.Throughput != nil {
			throughput = ep.Throughput.P50
		}
		if ep.Latency != nil {
			latency = ep.Latency.P50
		}
		meta = append(meta, endpointMeta{
			Tag:                     ep.Tag,
			ProviderName:            ep.ProviderName,
			Pricing:                 ep.Pricing,
			SupportsImplicitCaching: ep.SupportsImplicitCaching,
			Throughput:              throughput,
			Latency:                 latency,
			Quantization:            ep.Quantization,
			Compatible:              ep.Status == 0 && supports(ep, "tools") && supports(ep, "tool_choice"),
		})
	}
	sort.SliceStable(meta, func(i, j int) bool {
		return meta[i].Tag < meta[j].Tag
	})
	nameMap := make(map[string]string, len(meta))
	for _, ep := range meta {
		existing, exists := nameMap[ep.ProviderName]
		if !exists || (!strings.Contains(ep.Tag, "/") && strings.Contains(existing, "/")) {
			nameMap[ep.ProviderName] = ep.Tag
		}
	}
	return meta, nameMap
}

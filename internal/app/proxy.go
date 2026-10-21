package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var mutablePaths = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/responses":        true,
	"/v1/messages":         true,
}

const maxRequestBodyBytes = 64 << 20

var errRequestBodyTooLarge = errors.New("request body exceeds 64 MiB")

type proxy struct {
	cfg            config
	upstream       *url.URL
	client         *http.Client
	stats          *stats
	routing        *routingState
	registry       *modelRegistry
	trackEndpoints bool
	dailyMu        sync.Mutex
	dailyReady     map[string]chan struct{}
	dailyWG        sync.WaitGroup
	dailyClosed    bool
	recoveryWanted map[string]bool
	// degradationMu guards the watchdog's cooldown and scan throttle, which the
	// request path and the dashboard both touch.
	degradationMu      sync.Mutex
	degradationLast    map[string]time.Time
	degradationChecked map[string]time.Time
	// degradationWanted carries the provider a degradation run must re-measure: set
	// by the launcher, and also by a conviction that arrives while another run holds
	// the model. The run drops that provider's pooled rounds before its pings and
	// consumes the cooldown; a conviction it was too late to answer is dropped and
	// re-derived by the next scan.
	degradationWanted map[string]string
	rateLimitMu       sync.Mutex
	rateLimitRuns     map[rateLimitKey]int
	// Test seams for the daily benchmark coordinator. Production uses
	// testProvider and dailyBenchmarkGate when these are unset.
	dailyTestProvider func(context.Context, string, string, int) (providerTestSample, error)
	dailyGate         time.Duration
	now               func() time.Time
}

type rateLimitKey struct {
	model    string
	provider string
}

// providerTestSample is one provider's benchmark outcome: median throughput,
// median time to first token, median total latency, and how many rounds
// succeeded.
type providerTestSample struct {
	tps     float64
	ttft    time.Duration
	latency time.Duration
	samples int
}

func newProxy(cfg config) (*proxy, error) {
	u, err := url.Parse(strings.TrimRight(cfg.Upstream, "/"))
	if err != nil {
		return nil, err
	}
	if u.Scheme == "http" && cfg.OpenRouterAPIKey != "" {
		log.Printf("warning: forwarding the OpenRouter API key over plain http to %s", u.Host)
	}
	routing := newRoutingState(cfg.Models, cfg.PinTTL)
	routing.setProvidersPath(cfg.providersPath)
	p := &proxy{
		cfg:                cfg,
		upstream:           u,
		stats:              newStats(),
		routing:            routing,
		registry:           newModelRegistry(&http.Client{Timeout: 20 * time.Second}, cfg.Upstream, cfg.openRouterKey(), cfg.Listen, registryTTL),
		dailyReady:         make(map[string]chan struct{}),
		recoveryWanted:     make(map[string]bool),
		degradationLast:    make(map[string]time.Time),
		degradationChecked: make(map[string]time.Time),
		degradationWanted:  make(map[string]string),
		rateLimitRuns:      make(map[rateLimitKey]int),
		now:                time.Now,
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: time.Second,
		}},
	}
	return p, nil
}

// setClock points the proxy, routing, and stats clocks at the same source so
// tests control every time-dependent decision from one place instead of
// keeping three fields in sync by hand.
func (p *proxy) setClock(now func() time.Time) {
	p.now = now
	p.routing.now = now
	p.stats.now = now
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
		return
	}
	if r.URL.Path == "/registry.json" {
		p.registry.ServeJSON(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		writeError(w, http.StatusNotFound, "only /v1/* routes are supported")
		return
	}
	// Reject dot segments: the raw path is appended to the upstream path, so
	// "/v1/../key" would otherwise reach upstream endpoints outside /v1.
	if containsDotSegment(r.URL.Path) {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	record, err := p.forward(w, r)
	p.stats.record(record)
	p.observeProviderFailure(record)
	p.observeProviderDegradation(record)
	if err != nil && !errors.Is(err, r.Context().Err()) {
		log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	}
	if p.cfg.LogRequests {
		log.Printf("%s", formatRecord(record))
	}
}

func containsDotSegment(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

// forwardAttemptLimit bounds one client request to trying each configured
// provider at most once. More than one provider can be blocked by the same
// account policy, so stopping after a fixed second attempt can leak the second
// refusal even though a routable third provider remains. Provider-attributed
// 429s may also use this budget once their configured failover threshold is
// reached; every other non-policy failure is forwarded immediately.
func forwardAttemptLimit(routing *routingState, model, selected string) int {
	cfg, configured := routing.modelConfig(model)
	if !configured {
		return 1
	}
	providers := cfg.Order
	if len(providers) == 0 {
		providers = cfg.Only
	}
	unique := make(map[string]struct{}, len(providers)+1)
	for _, provider := range providers {
		if provider != "" {
			unique[provider] = struct{}{}
		}
	}
	if selected != "" {
		unique[selected] = struct{}{}
	}
	return max(1, len(unique))
}

func (p *proxy) forward(w http.ResponseWriter, incoming *http.Request) (requestRecord, error) {
	p.stats.beginRequest()
	defer p.stats.endRequest()
	started := p.now()
	record := requestRecord{Time: started, Method: incoming.Method, Path: incoming.URL.Path}
	var body io.Reader = incoming.Body
	// modified is the request with orr's provider block injected, retained so
	// the request can be prepared again if the chosen provider turns out to be
	// one this account cannot route to. It stays nil on pass-through paths,
	// where no provider was chosen and there is nothing to re-route.
	var modified []byte
	// namedEndpoints is how many endpoints the injected provider block names.
	// A refusal can be attributed to one provider only when there is exactly
	// one candidate.
	namedEndpoints := 0
	if mutablePaths[incoming.URL.Path] && incoming.Method == http.MethodPost {
		prepared, model, provider, toolCalls, reasoningEffort, _, named, err := prepareRequest(incoming.Body, p.routing, incoming.URL.Path)
		modified = prepared
		namedEndpoints = named
		record.Model, record.Provider, record.ToolCalls, record.ReasoningEffort = model, provider, toolCalls, reasoningEffort
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errRequestBodyTooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			writeError(w, status, err.Error())
			record.Status, record.Err, record.Duration = status, true, p.now().Sub(started)
			return record, nil
		}
		p.routing.setCurrentModel(model)
		// With no manual or measured pin, the first routable provider is the
		// automatic choice. prepareRequest reports the raw configured head, which
		// may be a provider already blocked for this account, so choose through
		// routing.active before making the pin real. Keep an empty candidate
		// empty: that is an unknown model's first request, whose client-supplied
		// routing must remain untouched until discovery completes.
		candidate := provider
		if candidate != "" {
			candidate = p.routing.active(model, nil)
		}
		if p.routing.ensureAutoPin(model, candidate) {
			p.stats.setAutoPin(model, candidate)
			var named int
			modified, model, provider, toolCalls, reasoningEffort, _, named, err = prepareRequest(bytes.NewReader(modified), p.routing, incoming.URL.Path)
			namedEndpoints = named
			record.Model, record.Provider, record.ToolCalls, record.ReasoningEffort = model, provider, toolCalls, reasoningEffort
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				record.Status, record.Err, record.Duration = http.StatusBadRequest, true, p.now().Sub(started)
				return record, nil
			}
		}
		// Daily discovery and benchmarks improve later requests. They must never
		// hold this user request behind catalog or benchmark latency.
		p.prepareDailyModel(model)
		body = bytes.NewReader(modified)
	}

	target := *p.upstream
	target.Path = strings.TrimRight(p.upstream.Path, "/") + strings.TrimPrefix(incoming.URL.Path, "/v1")
	target.RawQuery = incoming.URL.RawQuery
	maxAttempts := forwardAttemptLimit(p.routing, record.Model, record.Provider)

	var resp *http.Response
	providerFailoverActive := false
	// errorBody holds the drained prefix of an unsuccessful response, read to
	// classify it. It is forwarded to the client first, and whatever the
	// prefix cap cut off is streamed after it, so no error body is truncated.
	var errorBody []byte
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(incoming.Context(), incoming.Method, target.String(), body)
		if err != nil {
			writeError(w, http.StatusBadGateway, "could not create upstream request")
			record.Status, record.Err, record.Duration = http.StatusBadGateway, true, p.now().Sub(started)
			return record, err
		}
		copyHeaders(req.Header, incoming.Header)
		// Delete the client's Accept-Encoding so the transport adds its own gzip
		// header and transparently decompresses the upstream response, keeping the
		// bounded tail inspection able to read usage and provider metadata.
		req.Header.Del("Accept-Encoding")
		req.Header.Del("Content-Length")
		req.Host = p.upstream.Host

		resp, err = p.client.Do(req)
		if err != nil {
			// Log the upstream failure ourselves with the path only: the error
			// value embeds the full upstream URL including any query string.
			cause := err
			if unwrapped := errors.Unwrap(err); unwrapped != nil {
				cause = unwrapped
			}
			log.Printf("upstream %s %s: %v", req.Method, req.URL.Path, cause)
			writeError(w, http.StatusBadGateway, "OpenRouter request failed")
			record.Status, record.Err, record.Duration = http.StatusBadGateway, true, p.now().Sub(started)
			return record, nil
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			break
		}
		// An unsuccessful response is a small JSON document, so reading it
		// before answering the client costs nothing — and it is the only way
		// to tell a provider this account cannot route to from one that is
		// merely failing, while there is still a chance to route elsewhere.
		// The body is left open: unless the request is re-routed below, its
		// remainder is streamed to the client after the drained prefix.
		errorBody, _ = io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		refusal, blocked := parseBlockedReason(errorBody)
		if !blocked {
			attributable := namedEndpoints == 1 && record.Model != "" && record.Provider != ""
			providerRateLimited := attributable && resp.StatusCode == http.StatusTooManyRequests && parseProviderRateLimit(errorBody)
			providerUnavailable := attributable && resp.StatusCode == http.StatusNotFound && parseProviderUnavailable(errorBody)
			if !providerRateLimited && !providerUnavailable {
				break
			}
			record.RateLimited = providerRateLimited
			record.ProviderUnavailable = providerUnavailable
			record.ProviderFailureHandled = true
			var replacement string
			var failover bool
			if providerRateLimited {
				replacement, failover = p.rateLimitReplacement(record.Model, record.Provider, providerFailoverActive)
			} else {
				replacement, failover = p.providerUnavailableReplacement(record.Model, record.Provider)
			}
			if !failover {
				break
			}
			providerFailoverActive = true
			if attempt >= maxAttempts || modified == nil || replacement == "" {
				p.startProviderRecovery(record.Model)
				break
			}
			next, model, provider, toolCalls, reasoningEffort, _, named, prepareErr := prepareRequest(bytes.NewReader(modified), p.routing, incoming.URL.Path)
			if prepareErr != nil || provider == record.Provider {
				p.startProviderRecovery(record.Model)
				break
			}
			resp.Body.Close()
			modified = next
			namedEndpoints = named
			body = bytes.NewReader(modified)
			record.Model, record.Provider, record.ToolCalls, record.ReasoningEffort = model, provider, toolCalls, reasoningEffort
			record.RateLimited, record.ProviderUnavailable, record.ProviderFailureHandled = false, false, false
			errorBody = nil
			p.startProviderRecovery(record.Model)
			continue
		}
		record.Blocked, record.BlockedReason = true, refusal.Reason
		// Attribute the refusal only when the request named a single endpoint:
		// one covering a whole fallback list says none of them worked, not
		// which one to blame. The count prepared into the request decides;
		// OpenRouter's own count can narrow a multi-provider order down to one
		// after its filtering, so it is only a cross-check.
		if namedEndpoints != 1 || refusal.RequestedEndpoints != 1 || record.Model == "" || record.Provider == "" {
			break
		}
		p.noteBlocked(record.Model, record.Provider, refusal.Reason)
		if attempt >= maxAttempts || modified == nil {
			break
		}
		// A manually pinned provider is never routed around. The user asked
		// for that provider specifically, so the refusal is the answer to the
		// question they asked; substituting a different provider would hide
		// it. Only orr's own automatic choice may be reconsidered.
		if p.routing.manualPin(record.Model) {
			break
		}
		// The refusal is final for this provider but says nothing about the
		// others, and the block just recorded has taken it out of the order.
		// Preparing the request again re-routes it, so discovering a blocked
		// provider does not cost the client a failed request. The refused
		// attempt is not recorded: the client sent one request and gets one
		// answer, so the stats record describes the answer they received.
		next, model, provider, toolCalls, reasoningEffort, _, named, prepareErr := prepareRequest(bytes.NewReader(modified), p.routing, incoming.URL.Path)
		if prepareErr != nil || provider == record.Provider {
			// Nothing else to route to; the refusal is the honest answer.
			break
		}
		resp.Body.Close()
		modified = next
		namedEndpoints = named
		body = bytes.NewReader(modified)
		record.Model, record.Provider, record.ToolCalls, record.ReasoningEffort = model, provider, toolCalls, reasoningEffort
		record.Blocked, record.BlockedReason = false, ""
		errorBody = nil
	}

	record.Status = resp.StatusCode
	record.Err = resp.StatusCode < 200 || resp.StatusCode >= 300

	copyHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)

	tail := make([]byte, 0, 64*1024)
	defer resp.Body.Close()
	flusher, canFlush := w.(http.Flusher)
	if len(errorBody) > 0 {
		// An unsuccessful response was drained to classify it: forward what
		// was read, then fall through to stream whatever the cap cut off.
		if record.TTFT == 0 {
			record.TTFT = p.now().Sub(started)
		}
		tail = appendTail(tail, errorBody, 64*1024)
		if _, err := w.Write(errorBody); err != nil {
			record.Duration, record.Err = p.now().Sub(started), true
			return record, err
		}
		if canFlush {
			flusher.Flush()
		}
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if record.TTFT == 0 {
				record.TTFT = p.now().Sub(started)
			}
			tail = appendTail(tail, buf[:n], 64*1024)
			if _, err := w.Write(buf[:n]); err != nil {
				record.Duration, record.Err = p.now().Sub(started), true
				return record, err
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			record.Duration, record.Err = p.now().Sub(started), true
			return record, readErr
		}
	}
	record.Duration = p.now().Sub(started)
	routedModel, routedProvider := record.Model, record.Provider
	summary := extractResponseSummary(tail)
	if summary.model != "" {
		record.Model = summary.model
	}
	if summary.provider != "" {
		record.Provider = summary.provider
	}
	record.PromptTokens = summary.promptTokens
	record.CompletionTokens = summary.completionTokens
	record.ReasoningTokens = summary.reasoningTokens
	record.CachedTokens = summary.cachedTokens
	record.CacheWriteTokens = summary.cacheWriteTokens
	if summary.hasCost {
		record.Cost = summary.cost
	}
	// OpenRouter can accept a streaming request and then report a routing
	// refusal in an SSE frame. The status is already on the wire, so this
	// request cannot be retried, but remembering an attributable refusal keeps
	// subsequent requests off the provider. It must also remain an error so the
	// success path below does not erase an existing block.
	if refusal, blocked := parseBlockedReason(tail); blocked {
		record.Blocked, record.BlockedReason, record.Err = true, refusal.Reason, true
		if namedEndpoints == 1 && refusal.RequestedEndpoints == 1 && record.Model != "" && record.Provider != "" {
			p.noteBlocked(record.Model, record.Provider, refusal.Reason)
		}
	}
	if parseProviderRateLimit(tail) && namedEndpoints == 1 && routedModel != "" && routedProvider != "" {
		// A preceding stream frame can report a provider display name. Attribute
		// the 429 to the endpoint tag that this request actually selected so the
		// next request can route around it.
		record.Model, record.Provider = routedModel, routedProvider
		record.RateLimited = true
		record.Err = true
	}
	if parseProviderUnavailable(tail) && namedEndpoints == 1 && routedModel != "" && routedProvider != "" {
		record.Model, record.Provider = routedModel, routedProvider
		record.ProviderUnavailable = true
		record.Err = true
	}
	if summary.providerReported && record.Model != "" && record.Provider != "" && !record.Err {
		provider, _ := p.routing.resolveProvider(record.Model, record.Provider)
		record.Provider = provider
	}
	if !record.Err && record.Model != "" && record.Provider != "" {
		// A provider that just answered cannot still be refused: if a
		// guardrail was loosened since the block was recorded, it takes effect
		// here, without waiting for the block to expire or for a benchmark to
		// re-probe the provider.
		p.noteRoutable(record.Model, record.Provider)
	}
	if record.Model != "" && p.trackEndpoints {
		go p.routing.refreshEndpoints(record.Model, p.client, p.cfg.Upstream, p.cfg.openRouterKey())
	}
	return record, nil
}

const (
	dailyBenchmarkGate       = 3 * time.Second
	manualBenchmarkGate      = 10 * time.Second
	providerRecoveryCooldown = 30 * time.Second
)

// benchmarkFreshness is how old a model's last measurement may be before a
// request asks for another one. Measurement follows the models clients actually
// use: a model nobody asked for is never measured, and the one that is asked for
// gets a fresh field once the data behind its pin is stale. Five minutes is
// short enough that a restart is a measurement and long enough that a crash loop
// cannot hammer providers.
const benchmarkFreshness = 5 * time.Minute

// benchmarkRetryAfterAbort is how long a model waits after a run that could not
// get a usable catalog, and so never reached the pings. It is deliberately short:
// the cause is usually a catalog or configuration failure that clears quickly, and
// the freshness window would hide the model from measurement for five minutes over
// a one-minute outage. It also bounds the retries, so a failure cannot be relaunched
// by every request.
const benchmarkRetryAfterAbort = time.Minute

// prepareDailyModel starts the catalog refresh and benchmark a request for model
// asks for. Work is asynchronous with respect to user requests. The returned
// channel is used by tests and closes when every provider has finished or the
// gate has elapsed, whichever happens first; unfinished benchmark requests are
// then canceled.
//
// A manually pinned model has no daily run at all. The order underneath a manual
// pin decides nothing — orr is not choosing — so measuring the field would only
// spend provider requests on a pin that cannot move. The catalog refresh this
// run would carry is not lost by that: unpinning the model re-enters this path on
// the next request and refreshes then.
func (p *proxy) prepareDailyModel(model string) <-chan struct{} {
	// An alias id resolves to a model but exposes no endpoint list of its own, so a
	// run for it could never refresh or measure anything. Skipping it here keeps it
	// from cycling a no-op run for as long as a client uses the alias.
	if p.routing.manualPin(model) || isAliasModel(model) {
		return nil
	}
	p.dailyMu.Lock()
	if p.dailyClosed {
		p.dailyMu.Unlock()
		return nil
	}
	if ready, ok := p.dailyReady[model]; ok {
		p.dailyMu.Unlock()
		return ready
	}
	// The catalog keeps its own once-per-day schedule, independent of the benchmark
	// freshness window; a request only waits on it when a run is due anyway.
	refreshDone := p.routing.maybeRefreshModelOrder(model, p.client, p.cfg.Upstream, p.cfg.openRouterKey(), p.cfg.UpdateMaxProviders, p.cfg.UpdateCacheOnly)
	key := strings.TrimSpace(p.cfg.OpenRouterAPIKey) != ""
	now := p.now()
	measured := p.stats.benchmarkedWithin(model, now, benchmarkFreshness)
	// A run that could not get a usable catalog is retried on its own short floor.
	// Without it the next request would relaunch it, and with the freshness window
	// instead of the floor a model would stay unmeasured for five minutes over a
	// one-minute outage.
	retrying := p.stats.benchmarkAbortedWithin(model, now, benchmarkRetryAfterAbort)
	// Measurement follows the models clients actually use: a model measured inside
	// the freshness window needs no new run. With a key, that window is the whole
	// trigger. Without one there is nothing to ping, and only a refreshed catalog
	// can move the provisional automatic pin, so a stale catalog is the trigger.
	due := !measured && !retrying && (key || refreshDone != nil)
	if !due {
		p.dailyMu.Unlock()
		return nil
	}
	if refreshDone == nil {
		// The catalog and its endpoint metadata were already refreshed today,
		// but that does not prove a benchmark ran. Refresh it again to hydrate
		// this process's endpoint cache before pricing the measurements, while
		// keeping the benchmark schedule independent from the catalog date.
		refreshDone = p.routing.launchModelOrderRefresh(model, p.client, p.cfg.Upstream, p.cfg.openRouterKey(), p.cfg.UpdateMaxProviders, p.cfg.UpdateCacheOnly, false)
	}
	if refreshDone == nil {
		p.dailyMu.Unlock()
		return nil
	}
	ready := make(chan struct{})
	p.dailyReady[model] = ready
	p.dailyWG.Add(1)
	p.dailyMu.Unlock()

	go p.runDailyBenchmark(model, refreshDone, ready)
	return ready
}

// startProviderRecovery runs the same bounded catalog refresh and benchmark
// used for automatic discovery, bypassing the once-per-day schedule. dailyReady
// coalesces it with any other automatic run for this model, while the recovery
// cooldown prevents repeated provider failures from starting billable benchmark
// storms. That cooldown lives in the stats file, so a restart does not hand the
// same failure a fresh allowance.
//
// Recovery is deliberately not held back by the abort floor: a provider failure
// is an active fault, and waiting a minute to look at it is the wrong trade. A
// recovery run that measures nothing still arms the floor, so a storm of 429s
// that leaves nothing to ping also keeps the daily and degradation paths quiet.
func (p *proxy) startProviderRecovery(model string) <-chan struct{} {
	p.dailyMu.Lock()
	if p.dailyClosed {
		p.dailyMu.Unlock()
		return nil
	}
	if ready, ok := p.dailyReady[model]; ok {
		p.recoveryWanted[model] = true
		p.dailyMu.Unlock()
		return ready
	}
	now := p.now()
	if last := p.stats.recoveryLastRun(model); !last.IsZero() && !last.After(now) && now.Sub(last) < providerRecoveryCooldown {
		p.dailyMu.Unlock()
		return nil
	}
	refreshDone := p.routing.launchModelOrderRefresh(model, p.client, p.cfg.Upstream, p.cfg.openRouterKey(), p.cfg.UpdateMaxProviders, p.cfg.UpdateCacheOnly, false)
	if refreshDone == nil {
		p.dailyMu.Unlock()
		return nil
	}
	ready := make(chan struct{})
	p.dailyReady[model] = ready
	p.dailyWG.Add(1)
	p.dailyMu.Unlock()

	go p.runAutomaticBenchmark(model, refreshDone, ready, true, p.dailyGate)
	return ready
}

// runDailyBenchmark is prepareDailyModel's scheduled run.
func (p *proxy) runDailyBenchmark(model string, refreshDone <-chan struct{}, ready chan struct{}) {
	p.runAutomaticBenchmark(model, refreshDone, ready, false, p.dailyGate)
}

// runAutomaticBenchmark performs one bounded catalog refresh and provider
// benchmark. recovery says the run was started by a provider failure, so it
// consumes the recovery cooldown and also honors a recovery that arrived while it
// was running. A degradation conviction is carried in degradationWanted instead.
//
// The run settles the schedule on the state of its catalog rather than on what it
// managed to measure: with today's catalog in hand, the run has already paid for
// its refresh, so the five-minute freshness window applies whatever the pings
// returned. A run that could not get a usable catalog never reached the pings, is
// cheap to attempt again, and takes the short retry floor instead — that is what
// keeps a one-minute outage from hiding a model for five minutes, while still
// bounding the retries. A conviction is answered by the run that cleared the
// convicted provider's pooled rounds, and only that run consumes the cooldown.
func (p *proxy) runAutomaticBenchmark(model string, refreshDone <-chan struct{}, ready chan struct{}, recovery bool, gate time.Duration) {
	// catalogFresh records that the run started with today's catalog, which is what
	// decides whether its cost was spent or it failed cheaply.
	catalogFresh := false
	// convicted is the provider this run was asked to re-measure. It is popped once,
	// before the pings, because clearing the pool after the measurement would defeat
	// the ranking the clear exists to fix.
	var convicted string
	defer func() {
		now := p.now()
		if catalogFresh {
			p.stats.markBenchmarkRun(model, now)
		} else {
			p.stats.markBenchmarkAbort(model, now)
		}
		p.dailyMu.Lock()
		delete(p.dailyReady, model)
		wanted := p.recoveryWanted[model]
		delete(p.recoveryWanted, model)
		// A conviction that arrived after this run cleared its pool cannot be answered
		// by it: the run measured while the stale rounds were still in place. Dropping
		// it is safe — the watch re-convicts from unchanged vitals on its next scan,
		// and that run clears the pool before its own pings.
		delete(p.degradationWanted, model)
		if recovery || wanted {
			p.stats.markRecoveryRun(model, now)
		}
		p.dailyMu.Unlock()
		if convicted != "" {
			// The pooled rounds of the convicted provider are gone and this run has
			// re-measured the field, so the state the cooldown is about was addressed —
			// whether or not the provider itself answered.
			p.degradationMu.Lock()
			p.degradationLast[model] = now
			p.degradationMu.Unlock()
		}
		p.dailyWG.Done()
	}()
	if gate <= 0 {
		gate = dailyBenchmarkGate
	}
	// The refresh gets the gate first and the pings get a fresh one afterwards:
	// catalog latency is not provider latency, and spending the whole budget on a
	// refresh is how a model ends up with a refreshed order and no measurement at
	// all. The gate is still released when the refresh budget expires, so
	// coalesced callers are not held behind a slow catalog.
	refreshTimer := time.NewTimer(time.Until(time.Now().Add(gate)))
	defer refreshTimer.Stop()
	released := false
	release := func() {
		if !released {
			close(ready)
			released = true
		}
	}
	select {
	case <-refreshDone:
	case <-refreshTimer.C:
		release()
		<-refreshDone
	}
	deadline := time.Now().Add(gate)
	cfg, revision, ok := p.routing.modelConfigAtRevision(model)
	catalogFresh = ok && cfg.UpdatedAt.Format(time.DateOnly) == p.now().Format(time.DateOnly)
	if !catalogFresh {
		release()
		return
	}
	providers := append([]string(nil), cfg.Order...)
	if len(providers) == 0 {
		release()
		return
	}
	// The request that launched this work pinned the order that existed before
	// the refresh. Without an API key there is no benchmark to replace it, so
	// move the automatic pin to the refreshed catalog head now. autoPin keeps a
	// manual override authoritative.
	if p.cfg.openRouterKey() == "" {
		if p.routing.autoPin(model, providers[0]) {
			p.stats.setAutoPin(model, providers[0])
		}
		release()
		return
	}

	// A conviction this run was asked to answer is popped and acted on here, before
	// any ping: the provider's pooled rounds are stale evidence by definition, and
	// dropping them before the measurement is what keeps the run that answers the
	// conviction from merging them back in.
	p.dailyMu.Lock()
	convicted = p.degradationWanted[model]
	delete(p.degradationWanted, model)
	p.dailyMu.Unlock()
	if convicted != "" {
		p.stats.clearBenchmarkSamples(model, convicted)
	}

	completed, reachedThreshold := p.benchmarkProviders(model, providers, deadline, true, len(providers))
	if !reachedThreshold {
		// The request deadline is strict: release it before the providers-file
		// write. The measured partial order can finish persisting afterward.
		release()
	}
	p.applyProviderTestResults(model, completed, revision)
	release()
}

// observeProviderFailure maintains one bounded consecutive-429 counter per
// model and provider, and handles a selected endpoint disappearing from the
// model catalog. Only automatically routed provider responses trigger
// failover: a manual pin is an explicit instruction, and an un-attributed
// OpenRouter-wide failure must not condemn one provider. HTTP failures are
// handled inside the forwarding loop so the request can be replayed; this
// post-response path handles successful resets and errors found in a stream.
func (p *proxy) observeProviderFailure(record requestRecord) {
	if record.Benchmark || record.ProviderFailureHandled || record.Model == "" || record.Provider == "" {
		return
	}
	if record.ProviderUnavailable {
		if _, failedOver := p.providerUnavailableReplacement(record.Model, record.Provider); failedOver {
			p.startProviderRecovery(record.Model)
		}
		return
	}
	if record.RateLimited {
		if _, thresholdReached := p.rateLimitReplacement(record.Model, record.Provider, false); thresholdReached {
			p.startProviderRecovery(record.Model)
		}
		return
	}
	key := rateLimitKey{model: record.Model, provider: record.Provider}
	p.rateLimitMu.Lock()
	delete(p.rateLimitRuns, key)
	p.rateLimitMu.Unlock()
	if !record.Err {
		p.routing.clearProviderUnavailable(record.Model, record.Provider, record.Time)
	}
}

// rateLimitReplacement counts one provider-attributed 429 and moves the
// automatic pin once the configured threshold is reached. During an internal
// failover chain, force is true so another 429 immediately advances to the next
// candidate instead of spending the client's remaining retries on it.
func (p *proxy) rateLimitReplacement(model, provider string, force bool) (string, bool) {
	key := rateLimitKey{model: model, provider: provider}
	if p.routing.manualPin(model) {
		p.rateLimitMu.Lock()
		delete(p.rateLimitRuns, key)
		p.rateLimitMu.Unlock()
		return "", false
	}

	p.rateLimitMu.Lock()
	attempts := p.rateLimitRuns[key]
	if !force {
		attempts++
	}
	if !force && attempts < p.cfg.RateLimitFailoverThreshold {
		p.rateLimitRuns[key] = attempts
		p.rateLimitMu.Unlock()
		return "", false
	}
	delete(p.rateLimitRuns, key)
	p.rateLimitMu.Unlock()

	replacement, accepted := p.automaticProviderFailover(model, provider)
	if !accepted {
		return "", false
	}
	if replacement != "" {
		if force {
			log.Printf("provider %s also returned 429 during failover for %s; trying %s", provider, model, replacement)
		} else {
			log.Printf("provider %s returned %d consecutive 429s for %s; failing over to %s", provider, p.cfg.RateLimitFailoverThreshold, model, replacement)
		}
	} else {
		log.Printf("provider %s returned 429 for %s with no eligible failover; starting provider recovery", provider, model)
	}
	return replacement, true
}

// providerUnavailableReplacement immediately moves away from an endpoint that
// OpenRouter says no longer serves the selected model. Retrying the same dead
// endpoint cannot help, so this failure does not consume the configurable 429
// allowance.
func (p *proxy) providerUnavailableReplacement(model, provider string) (string, bool) {
	key := rateLimitKey{model: model, provider: provider}
	p.rateLimitMu.Lock()
	delete(p.rateLimitRuns, key)
	p.rateLimitMu.Unlock()
	replacement, accepted := p.automaticProviderFailover(model, provider)
	if !accepted {
		return "", false
	}
	if replacement != "" {
		log.Printf("provider %s is unavailable for %s; failing over to %s", provider, model, replacement)
	} else {
		log.Printf("provider %s is unavailable for %s with no eligible failover; starting provider recovery", provider, model)
	}
	return replacement, true
}

func (p *proxy) automaticProviderFailover(model, provider string) (string, bool) {
	replacement, accepted := p.routing.markProviderUnavailableAndFailover(model, provider)
	if !accepted {
		return "", false
	}
	if replacement != "" {
		p.stats.setAutoPin(model, replacement)
	} else {
		p.stats.clearAutoPin(model)
	}
	syncPersistedAutomaticPin(p.routing, p.stats, model)
	return replacement, true
}

// benchmarkProviders runs up to maxConcurrency provider tests at once and waits
// for all of them or the deadline, whichever comes first. The deadline is the
// only thing that ends a run early: a slow provider costs its own slot, not the
// coverage of the providers that already answered, and nothing is waiting on
// this automatic background work. Requests still in flight at the deadline are
// canceled. Optional quality metadata loads concurrently and may use the rest of
// the deadline (up to ten seconds) after provider tests finish.
func (p *proxy) benchmarkProviders(model string, providers []string, deadline time.Time, automatic bool, maxConcurrency int) ([]providerTestResult, bool) {
	if len(providers) == 0 || !p.now().Before(deadline) {
		return nil, false
	}
	// Providers known to be blocked are not measured: every round would spend
	// a request to be told again that the account cannot route there. They come
	// back into the rotation when the block expires.
	blocked := p.routing.blockedProviders(model)
	if routable, unroutable := partitionBlocked(providers, blocked); len(unroutable) > 0 && len(routable) > 0 {
		providers = routable
	}
	// Automatic runs do not immediately probe providers that just exhausted
	// their request capacity: every round would be an avoidable 429. Manual
	// benchmarks remain the explicit re-probe path.
	if automatic {
		unavailable := p.routing.temporarilyUnavailableProviders(model)
		eligible := make([]string, 0, len(providers))
		for _, provider := range providers {
			if !unavailable[provider] {
				eligible = append(eligible, provider)
			}
		}
		providers = eligible
		if len(providers) == 0 {
			return nil, false
		}
	}
	maxConcurrency = max(1, min(maxConcurrency, len(providers)))
	remaining := deadline.Sub(p.now())
	if remaining <= 0 {
		return nil, false
	}
	// Deadlines are expressed in the proxy's clock. Convert the remaining
	// duration to a real context timeout so tests and callers with an injected
	// clock do not accidentally compare two different time domains.
	ctx, cancel := context.WithTimeout(context.Background(), remaining)
	defer cancel()
	// Quality fetches run alongside the measurements, including without a TUI.
	// They share the run's deadline but not its early worker cancellation, so a
	// measure that ends at the deadline does not throw away almost-complete
	// quality metadata.
	if p.upstream != nil && p.upstream.Scheme == "https" && p.upstream.Host == "openrouter.ai" {
		qualityCtx, qualityCancel := context.WithTimeout(context.Background(), min(remaining, 10*time.Second))
		client := *p.client
		client.Timeout = 10 * time.Second
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		// Quality metadata is dashboard detail, so the ranking that applies these
		// measurements never waits for it: a slow quality fetch would otherwise
		// delay the pin this run exists to set.
		go func() {
			defer qualityCancel()
			_, _ = p.routing.refreshProviderQuality(qualityCtx, &client, "https://openrouter.ai", p.cfg.openRouterKey(), model)
		}()
	}
	testProvider := p.testProviderContext
	if p.dailyTestProvider != nil {
		testProvider = p.dailyTestProvider
	}
	jobs := make(chan string, len(providers))
	results := make(chan providerTestResult, len(providers))
	for _, provider := range providers {
		jobs <- provider
	}
	close(jobs)

	var workers sync.WaitGroup
	workers.Add(maxConcurrency)
	for range maxConcurrency {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case provider, ok := <-jobs:
					if !ok || ctx.Err() != nil {
						return
					}
					started := p.now()
					sample, err := testProvider(ctx, model, provider, providerTestRounds)
					if err == nil {
						p.routing.clearProviderUnavailable(model, provider, started)
					}
					select {
					case results <- newProviderTestResult(provider, sample, err):
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}

	// Wait for every provider or the deadline. Stopping at a share of providers
	// would rank a partial field while the budget for the rest was still there.
	completed := make([]providerTestResult, 0, len(providers))
	for len(completed) < len(providers) {
		select {
		case result := <-results:
			completed = append(completed, result)
		case <-ctx.Done():
			cancel()
			// testProvider records requests and may clear an obsolete benchmark
			// pool before returning. Do not rank until every canceled worker has
			// finished those mutations, or a provider can win on the stale pool
			// that its still-running test is about to invalidate.
			workers.Wait()
			return completed, false
		}
	}
	cancel()
	// Every provider reported inside the deadline. Their stats mutations must
	// land before the caller snapshots the pool and ranks it.
	workers.Wait()
	return completed, true
}

func (p *proxy) waitForDailyBenchmarks() {
	p.dailyWG.Wait()
}

// stopDailyBenchmarks closes benchmark admission. Once it returns, no handler
// can add new work to dailyWG, so waiting on that group during shutdown is safe.
func (p *proxy) stopDailyBenchmarks() {
	p.dailyMu.Lock()
	p.dailyClosed = true
	p.dailyMu.Unlock()
}

func (p *proxy) applyProviderTestResults(model string, results []providerTestResult, revision uint64) error {
	snap := p.stats.snapshot()
	providers := p.routing.providers(model, snap.Observed[model])
	now := p.now()
	results = mergeMeasuredProviderResults(results, p.stats.benchmarkPool(model), providers, now)
	profile := newRequestProfile(snap.MetricsRecords, model, now)
	apiErrs := providerAPIErrorRates(snap.Records, model, now)
	best, autoUpdated, err := p.routing.applyProviderTestResultsAtRevision(model, results, profile, revision, apiErrs)
	if err != nil {
		log.Printf("apply provider benchmarks for %s: %v", model, err)
		return err
	}
	if best != "" {
		// A manual pin may currently override this winner. Persist it anyway so
		// removing the manual override restores the latest measured route.
		p.stats.setAutoPin(model, best)
	} else if autoUpdated {
		p.stats.clearAutoPin(model)
		fallback := p.routing.active(model, snap.Observed[model])
		if p.routing.ensureAutoPin(model, fallback) {
			p.stats.setAutoPin(model, fallback)
		}
	}
	if best != "" || autoUpdated {
		syncPersistedAutomaticPin(p.routing, p.stats, model)
	}
	return nil
}

// syncPersistedAutomaticPin makes the stats copy follow the final serialized
// routing state. A benchmark and a provider failover both update routing before
// persisting the pin; without this final synchronized read, their stats writes
// could land in the opposite order and restore the wrong provider on restart.
// A manual pin deliberately leaves the hidden automatic winner untouched.
func syncPersistedAutomaticPin(routing *routingState, stats *stats, model string) {
	routing.persistMu.Lock()
	defer routing.persistMu.Unlock()
	routing.mu.RLock()
	pin, pinned := routing.pins[model]
	if pinned && pin.manual {
		routing.mu.RUnlock()
		return
	}
	if !pinned {
		if pending, ok := routing.pendingPins[model]; ok {
			pin = pinnedProvider{provider: pending.Provider, pinnedAt: pending.PinnedAt}
			pinned = true
		}
	}
	if pinned {
		stats.setAutoPinAt(model, pin.provider, pin.pinnedAt)
	} else {
		stats.clearAutoPin(model)
	}
	routing.mu.RUnlock()
}

func prepareRequest(body io.Reader, routing *routingState, path string) ([]byte, string, string, int, string, string, int, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxRequestBodyBytes+1))
	if err != nil {
		return nil, "", "", 0, "", path, 0, fmt.Errorf("read request body: %w", err)
	}
	if len(data) > maxRequestBodyBytes {
		return nil, "", "", 0, "", path, 0, errRequestBodyTooLarge
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, "", "", 0, "", path, 0, fmt.Errorf("request body must be valid JSON: %w", err)
	}
	if payload == nil {
		return nil, "", "", 0, "", path, 0, fmt.Errorf("request body must be a JSON object")
	}
	model, _ := payload["model"].(string)

	// Reasoning effort levels (e.g. "low", "medium", "high", "xhigh") are
	// requested per call, so they are captured from the request body. OpenAI
	// chat completions send it as a top-level reasoning_effort field, while the
	// Responses API nests it under reasoning.effort.
	reasoningEffort := ""
	if effort, ok := payload["reasoning_effort"].(string); ok {
		reasoningEffort = effort
	}
	if reasoningEffort == "" {
		if reasoning, ok := payload["reasoning"].(map[string]any); ok {
			if effort, ok := reasoning["effort"].(string); ok {
				reasoningEffort = effort
			}
		}
	}

	switch path {
	case "/v1/chat/completions":
		payload["usage"] = map[string]any{"include": true}
	case "/v1/responses":
		include, _ := payload["include"].([]any)
		hasUsage := false
		for _, item := range include {
			if s, ok := item.(string); ok && s == "usage" {
				hasUsage = true
				break
			}
		}
		if !hasUsage {
			payload["include"] = append(include, "usage")
		}
	}

	toolCalls := 0
	if tools, ok := payload["tools"].([]any); ok {
		toolCalls = len(tools)
	}

	provider, configured := routing.modelConfig(model)
	selected := routing.pinnedProvider(model)
	if !configured {
		if _, err := routing.ensureModel(model); err != nil {
			return nil, model, selected, toolCalls, reasoningEffort, path, 0, err
		}
		provider, configured = routing.modelConfig(model)
	}
	// namedEndpoints counts how many endpoints the injected provider block
	// names, so a routing refusal can be attributed only when there is exactly
	// one candidate to blame. A pin names one provider with fallbacks off; an
	// unpinned request names its whole order.
	namedEndpoints := 0
	if configured && (provider.hasRouting() || selected != "") {
		if selected != "" {
			provider.Order = nil
			provider.Only = []string{selected}
			allowFallbacks := false
			provider.AllowFallbacks = &allowFallbacks
			namedEndpoints = 1
		} else if len(provider.Order) > 0 {
			selected = provider.Order[0]
			namedEndpoints = len(provider.Order)
		} else if len(provider.Only) > 0 {
			selected = provider.Only[0]
			namedEndpoints = len(provider.Only)
		}
		encoded, err := json.Marshal(provider)
		if err != nil {
			return nil, model, selected, toolCalls, reasoningEffort, path, namedEndpoints, fmt.Errorf("encode provider config: %w", err)
		}
		var value map[string]any
		if err := json.Unmarshal(encoded, &value); err != nil {
			return nil, model, selected, toolCalls, reasoningEffort, path, namedEndpoints, err
		}
		payload["provider"] = value
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		return nil, model, selected, toolCalls, reasoningEffort, path, namedEndpoints, fmt.Errorf("encode request body: %w", err)
	}
	return buf.Bytes(), model, selected, toolCalls, reasoningEffort, path, namedEndpoints, nil
}

type responseSummary struct {
	model, provider                string
	providerReported               bool
	promptTokens, completionTokens int
	reasoningTokens                int
	cachedTokens, cacheWriteTokens int
	cost                           float64
	hasCost                        bool
}

func extractResponseSummary(data []byte) responseSummary {
	var summary responseSummary
	consume := func(raw []byte) {
		var value map[string]any
		if json.Unmarshal(bytes.TrimSpace(raw), &value) != nil {
			return
		}
		if model, ok := value["model"].(string); ok && model != "" {
			summary.model = model
		}
		if provider, ok := value["provider"].(string); ok && provider != "" {
			summary.provider = provider
			summary.providerReported = true
		}
		usage, _ := value["usage"].(map[string]any)
		if usage == nil {
			return
		}
		if n, ok := number(usage["prompt_tokens"]); ok {
			summary.promptTokens = int(n)
		}
		if n, ok := number(usage["input_tokens"]); ok && summary.promptTokens == 0 {
			summary.promptTokens = int(n)
		}
		if n, ok := number(usage["completion_tokens"]); ok {
			summary.completionTokens = int(n)
		}
		if n, ok := number(usage["output_tokens"]); ok && summary.completionTokens == 0 {
			summary.completionTokens = int(n)
		}
		// Reasoning (thinking) tokens are a breakdown of the output tokens:
		// OpenAI chat shape uses completion_tokens_details, the Responses API
		// shape uses output_tokens_details.
		if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
			if n, ok := number(details["reasoning_tokens"]); ok {
				summary.reasoningTokens = int(n)
			}
		}
		if details, ok := usage["output_tokens_details"].(map[string]any); ok {
			if summary.reasoningTokens == 0 {
				if n, ok := number(details["reasoning_tokens"]); ok {
					summary.reasoningTokens = int(n)
				}
			}
		}
		if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
			if n, ok := number(details["cached_tokens"]); ok {
				summary.cachedTokens = int(n)
			}
			if n, ok := number(details["cache_write_tokens"]); ok {
				summary.cacheWriteTokens = int(n)
			}
		}
		if details, ok := usage["input_tokens_details"].(map[string]any); ok {
			if summary.cachedTokens == 0 {
				if n, ok := number(details["cached_tokens"]); ok {
					summary.cachedTokens = int(n)
				}
			}
			if summary.cacheWriteTokens == 0 {
				if n, ok := number(details["cache_write_tokens"]); ok {
					summary.cacheWriteTokens = int(n)
				}
			}
		}
		// Anthropic-compatible /v1/messages usage shape.
		if summary.cachedTokens == 0 {
			if n, ok := number(usage["cache_read_input_tokens"]); ok {
				summary.cachedTokens = int(n)
			}
		}
		if summary.cacheWriteTokens == 0 {
			if n, ok := number(usage["cache_creation_input_tokens"]); ok {
				summary.cacheWriteTokens = int(n)
			}
		}
		if n, ok := number(usage["cost"]); ok {
			summary.cost, summary.hasCost = n, true
		}
	}
	trimmed := bytes.TrimSpace(data)
	fullJSON := json.Valid(trimmed)
	sawSSE := false
	// Only parse the whole tail as JSON when it is complete; for SSE streams
	// this would be a wasted parse of data that is never valid JSON.
	if fullJSON {
		consume(data)
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			sawSSE = true
			chunk := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if !bytes.Equal(chunk, []byte("[DONE]")) {
				consume(chunk)
			}
		}
	}
	// A large non-streaming JSON response may start before the bounded tail. Its
	// final usage object is still a complete JSON object, so extract it without
	// buffering or parsing the response body itself.
	if !fullJSON && !sawSSE {
		if usage := trailingJSONObject(data, "usage"); len(usage) > 0 && json.Valid(usage) {
			wrapped := append([]byte(`{"usage":`), usage...)
			consume(append(wrapped, '}'))
		}
		if value := trailingJSONString(data, "model"); value != "" {
			summary.model = value
		}
		if value := trailingJSONString(data, "provider"); value != "" {
			summary.provider = value
		}
		// A raw truncated tail cannot prove that provider is top-level metadata.
		// Do not authorize automatic pinning from this fallback.
		summary.providerReported = false
	}
	return summary
}

func trailingJSONObject(data []byte, key string) []byte {
	index := bytes.LastIndex(data, []byte(`"`+key+`"`))
	if index < 0 {
		return nil
	}
	rest := data[index+len(key)+2:]
	colon := bytes.IndexByte(rest, ':')
	if colon < 0 {
		return nil
	}
	rest = rest[colon+1:]
	start := bytes.IndexByte(rest, '{')
	if start < 0 {
		return nil
	}
	depth := 0
	inString, escaped := false, false
	for i, char := range rest[start:] {
		if inString {
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[start : start+i+1]
			}
		}
	}
	return nil
}

func trailingJSONString(data []byte, key string) string {
	index := bytes.LastIndex(data, []byte(`"`+key+`"`))
	if index < 0 {
		return ""
	}
	rest := data[index+len(key)+2:]
	colon := bytes.IndexByte(rest, ':')
	if colon < 0 {
		return ""
	}
	rest = bytes.TrimSpace(rest[colon+1:])
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	escaped := false
	for i := 1; i < len(rest); i++ {
		if escaped {
			escaped = false
			continue
		}
		if rest[i] == '\\' {
			escaped = true
		} else if rest[i] == '"' {
			var value string
			if json.Unmarshal(rest[:i+1], &value) == nil {
				return value
			}
			return ""
		}
	}
	return ""
}

func number(value any) (float64, bool) {
	switch value := value.(type) {
	case float64:
		return value, true
	case json.Number:
		n, err := value.Float64()
		return n, err == nil
	case string:
		n, err := strconv.ParseFloat(value, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func appendTail(tail, chunk []byte, limit int) []byte {
	if len(chunk) >= limit {
		return append(tail[:0], chunk[len(chunk)-limit:]...)
	}
	if overflow := len(tail) + len(chunk) - limit; overflow > 0 {
		copy(tail, tail[overflow:])
		tail = tail[:len(tail)-overflow]
	}
	return append(tail, chunk...)
}

func tokenText(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%dm", n/1_000_000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return strconv.Itoa(n)
}

func formatRecord(record requestRecord) string {
	tokensText := ""
	if record.PromptTokens > 0 || record.CompletionTokens > 0 {
		tokensText = fmt.Sprintf("in %d out %d ", record.PromptTokens, record.CompletionTokens)
	}
	cacheText := "cache 0"
	if record.CacheWriteTokens > 0 {
		cacheText = fmt.Sprintf("cache write %d", record.CacheWriteTokens)
	}
	if record.CachedTokens > 0 {
		if record.CacheWriteTokens > 0 {
			cacheText += fmt.Sprintf(" read %d (%.0f%%)", record.CachedTokens, record.cachePercent())
		} else {
			cacheText = fmt.Sprintf("cache %d (%.0f%%)", record.CachedTokens, record.cachePercent())
		}
	}
	return fmt.Sprintf("%s %s -> %s %d %s %s%.1ft/s $%.6f %s",
		record.Time.Local().Format("15:04:05"), record.Model,
		record.Provider, record.Status, record.Duration.Round(time.Millisecond),
		tokensText, record.tokensPerSecond(), record.Cost, cacheText)
}

func copyHeaders(dst, src http.Header) {
	connectionNamed := src.Values("Connection")
	for key, values := range src {
		if isHopByHop(key) || isConnectionNamed(key, connectionNamed) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func isHopByHop(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}

func isConnectionNamed(key string, connectionHeaders []string) bool {
	for _, name := range connectionHeaders {
		if strings.EqualFold(strings.TrimSpace(name), key) {
			return true
		}
	}
	return false
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"message": message,
		"type":    "orr_error",
	}})
}

// providerTestRounds is how many ping-pong requests a provider test runs before
// averaging the results.
const providerTestRounds = 3

// providerTestMaxTokens is how many tokens each ping generates. Measured
// against a real model's providers, a 16-token reply was not a usable
// measurement: the generation window was so short that jitter dominated it,
// giving a coefficient of variation above 1 and spreads past 50x between
// identical pings. Worse, the error was not merely noisy but differed in
// direction per provider — one read 1.8x its true rate while another read a
// third of its own — which distorts the ranking rather than just scattering
// it. 64 tokens cuts the variation to roughly a quarter; longer replies buy
// almost nothing and cost proportionally more time per round.
const providerTestMaxTokens = 64

// providerTestTimeout bounds each ping-pong round, so a provider that stalls
// mid-response cannot leave the dashboard spinner running forever.
const providerTestTimeout = 30 * time.Second

// minMeasurableRound floors a round's duration before it is divided into a
// token count. A reply can arrive faster than the platform clock resolves —
// notably over loopback on Windows, where the round measures exactly zero — and
// a round too fast to measure is a fast round, not a failed one. Without the
// floor it divides by zero and the fastest provider is the one thrown away.
const minMeasurableRound = 100 * time.Microsecond

// testProvider sends providerTestRounds minimal ping-pong chat completions to
// provider for model and returns the average completion tokens/s and total
// latency. Each round is a real, billed request, so it is recorded into stats
// like any other proxied request. It degrades gracefully when no OpenRouter
// API key is configured.
func (p *proxy) testProvider(model, provider string, rounds int) (providerTestSample, error) {
	return p.testProviderContext(context.Background(), model, provider, rounds)
}

func (p *proxy) testProviderContext(ctx context.Context, model, provider string, rounds int) (providerTestSample, error) {
	if !validModelID(model) {
		return providerTestSample{}, fmt.Errorf("model %q is not a valid author/model", model)
	}
	if provider == "" {
		return providerTestSample{}, fmt.Errorf("no provider selected")
	}
	if p.cfg.openRouterKey() == "" {
		return providerTestSample{}, fmt.Errorf("no OpenRouter API key configured")
	}
	if rounds < 1 {
		rounds = providerTestRounds
	}
	target := *p.upstream
	target.Path = strings.TrimRight(p.upstream.Path, "/") + "/chat/completions"
	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		"max_tokens": providerTestMaxTokens,
		"stream":     true,
		"usage":      map[string]any{"include": true},
		"provider":   map[string]any{"only": []string{provider}, "allow_fallbacks": false},
	})
	if err != nil {
		return providerTestSample{}, err
	}
	throughputs := make([]float64, 0, rounds)
	latencies := make([]float64, 0, rounds)
	ttfts := make([]float64, 0, rounds)
	var lastErr error
	// answered records that the provider replied to at least one round, with
	// an error status. It separates "this provider is failing" from "orr ran
	// out of time", which are treated very differently downstream.
	var answered bool
	for i := 0; i < rounds; i++ {
		roundCtx, cancel := context.WithTimeout(ctx, providerTestTimeout)
		record, err := p.pingPong(roundCtx, target.String(), payload, model, provider)
		cancel()
		record.Benchmark = true
		p.stats.record(record)
		if record.Blocked {
			// A benchmark pins exactly one provider with fallbacks off, so a
			// refusal here names that provider unambiguously. Stop early:
			// further rounds would return the same refusal.
			p.noteBlocked(model, provider, record.BlockedReason)
			return providerTestSample{}, fmt.Errorf("%w: %w", errProviderAnswered, err)
		}
		if err != nil {
			if record.Status != 0 {
				answered = true
			}
			lastErr = err
			continue
		}
		p.noteRoutable(model, provider)
		tps := record.benchmarkThroughput()
		if tps <= 0 || math.IsNaN(tps) || math.IsInf(tps, 0) {
			// A round that produced no measurable throughput cannot be
			// summarized. Keeping it would poison the median of the rounds
			// that did.
			lastErr = fmt.Errorf("provider %s round produced no measurable throughput", provider)
			continue
		}
		throughputs = append(throughputs, tps)
		latencies = append(latencies, float64(record.Duration))
		if record.TTFT > 0 {
			ttfts = append(ttfts, float64(record.TTFT))
		}
	}
	if len(throughputs) < providerTestQuorum(rounds) {
		if lastErr == nil {
			lastErr = fmt.Errorf("provider %s returned no usable benchmark round", provider)
		}
		if answered {
			// The provider replied, with an error. That is a verdict about the
			// provider as it is right now, unlike a deadline orr imposed, and
			// it has to outweigh whatever the provider managed on an earlier
			// day. See mergeMeasuredProviderResults. The pooled successes are
			// forgotten too, or a later cancelled test would bring them back.
			p.stats.clearBenchmarkSamples(model, provider)
			return providerTestSample{}, fmt.Errorf("%w: %w", errProviderAnswered, lastErr)
		}
		return providerTestSample{}, lastErr
	}
	return providerTestSample{
		tps:     medianFloat(throughputs),
		latency: time.Duration(medianFloat(latencies)),
		ttft:    time.Duration(medianFloat(ttfts)),
		samples: len(throughputs),
	}, nil
}

// noteBlocked records a routing refusal against the provider that caused it,
// so the provider stops being ranked, pinned and benchmarked until the block
// expires. A failed persist is logged rather than surfaced: the block still
// applies in memory, and losing it costs one rediscovery.
func (p *proxy) noteBlocked(model, provider, reason string) {
	pinned, manual := p.routing.pinInfo(model)
	wasAutoPinned := pinned == provider && !manual
	discovered, err := p.routing.markBlocked(model, provider, reason)
	if wasAutoPinned {
		// markBlocked promotes the next ranked, routable provider. Mirror that
		// transition into persisted stats so the dashboard and a restart see
		// the same pin requests immediately use.
		if replacement, replacementManual := p.routing.pinInfo(model); replacement != "" && !replacementManual {
			p.stats.setAutoPin(model, replacement)
		} else {
			p.stats.clearAutoPin(model)
		}
		syncPersistedAutomaticPin(p.routing, p.stats, model)
	}
	if discovered {
		log.Printf("provider %s is blocked for %s: %s", provider, model, reason)
	}
	if err != nil {
		// The block applies either way; only its persistence was lost.
		log.Printf("record blocked provider %s for %s: %v", provider, model, err)
	}
}

// noteRoutable forgets a recorded refusal once the provider answers again,
// which is how a loosened guardrail takes effect without a restart.
func (p *proxy) noteRoutable(model, provider string) {
	if !p.routing.isBlocked(model, provider) {
		return
	}
	if err := p.routing.clearBlocked(model, provider); err != nil {
		log.Printf("clear blocked provider %s for %s: %v", provider, model, err)
	}
}

// errProviderAnswered marks a benchmark failure the provider itself produced —
// an error status, or a refusal — as opposed to one orr caused by running out
// of time. The distinction matters because a provider that answers with an
// error is describing itself as it is now, while a cancelled test describes
// nothing at all.
var errProviderAnswered = errors.New("provider answered with an error")

// providerTestQuorum is how many of rounds must succeed. A three-round
// benchmark tolerates one failure, so a single timeout does not throw away two
// good measurements; shorter runs must complete every round.
func providerTestQuorum(rounds int) int {
	if rounds >= 3 {
		return rounds - 1
	}
	return rounds
}

// medianFloat is the P50 of values. Rounds are summarized by median rather
// than mean so one slow round cannot drag a provider's measurement with it.
func medianFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return percentileFloat(sorted, 0.5)
}

// pingPong performs one streaming chat completion and returns the measured
// request record, which the caller records into stats like any other request.
func (p *proxy) pingPong(ctx context.Context, target string, payload []byte, model, provider string) (requestRecord, error) {
	started := p.now()
	record := requestRecord{Time: started, Method: http.MethodPost, Path: "/v1/chat/completions", Model: model, Provider: provider}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		record.Duration = p.now().Sub(started)
		return record, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.cfg.openRouterKey())
	resp, err := p.client.Do(req)
	if err != nil {
		record.Duration = p.now().Sub(started)
		return record, err
	}
	defer resp.Body.Close()
	record.Status = resp.StatusCode
	record.Err = resp.StatusCode < 200 || resp.StatusCode >= 300
	if record.Err {
		// Read the error body rather than reporting the status alone: the
		// status cannot tell a provider that is blocked for this account from
		// one that is merely down, and only the body says which.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		record.Duration = p.now().Sub(started)
		if refusal, blocked := parseBlockedReason(body); blocked {
			record.Blocked, record.BlockedReason = true, refusal.Reason
			return record, fmt.Errorf("provider %s is blocked for this account: %s", provider, refusal.Reason)
		}
		return record, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	var ttft time.Duration
	tail := make([]byte, 0, 64*1024)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if ttft == 0 {
				ttft = p.now().Sub(started)
			}
			tail = appendTail(tail, buf[:n], 64*1024)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			record.Duration = p.now().Sub(started)
			return record, readErr
		}
	}
	record.Duration = p.now().Sub(started)
	record.TTFT = ttft
	summary := extractResponseSummary(tail)
	record.PromptTokens = summary.promptTokens
	record.CompletionTokens = summary.completionTokens
	record.ReasoningTokens = summary.reasoningTokens
	record.CachedTokens = summary.cachedTokens
	record.CacheWriteTokens = summary.cacheWriteTokens
	if summary.hasCost {
		record.Cost = summary.cost
	}
	if refusal, blocked := parseBlockedReason(tail); blocked {
		record.Blocked, record.BlockedReason, record.Err = true, refusal.Reason, true
		return record, fmt.Errorf("provider %s is blocked for this account: %s", provider, refusal.Reason)
	}
	if record.CompletionTokens <= 0 {
		return record, fmt.Errorf("no completion tokens in response")
	}
	return record, nil
}

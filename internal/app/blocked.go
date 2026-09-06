package app

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// A provider can be unroutable for this account without being unhealthy. The
// account's data policy and the workspace guardrails exclude endpoints that
// train on paid prompts or do not offer zero data retention, and OpenRouter
// applies those rules only while routing a request: the endpoints catalog
// reports such an endpoint as status 0 with full uptime and no policy field at
// all. The refusal is therefore the only description of them orr can obtain,
// which is why a blocked provider is discovered by being tried and then
// remembered rather than filtered out of the catalog up front.
//
// blockedProviderTTL is how long a refusal is trusted before the provider is
// probed again. Guardrails are settings rather than weather — they change when
// somebody changes them — so a block is remembered across restarts and retried
// once a day instead of on every benchmark. A manual `r` refresh clears them
// immediately, which is the escape hatch after editing a guardrail.
const blockedProviderTTL = 24 * time.Hour

// blockedProvider is one provider OpenRouter refused to route to, recorded per
// model in the providers file so a restart does not spend a client request
// rediscovering it.
type blockedProvider struct {
	Provider   string    `yaml:"provider"`
	Reason     string    `yaml:"reason,omitempty"`
	DetectedAt time.Time `yaml:"detected_at,omitempty"`
}

// fresh reports whether the refusal is recent enough to still act on. An entry
// without a timestamp is treated as fresh so a hand-written providers file can
// block a provider permanently.
func (b blockedProvider) fresh(now time.Time) bool {
	return b.DetectedAt.IsZero() || now.Sub(b.DetectedAt) < blockedProviderTTL
}

// guardrailRoutingStep is the routing stage OpenRouter names when a request
// fails on policy grounds rather than on provider health.
const guardrailRoutingStep = "Filter by Guardrails"

// openRouterErrorEnvelope is the error body OpenRouter returns when routing
// fails. Ineligibility reasons can describe request-specific incompatibilities
// as well as account policy, so the failed routing step and the reason names
// must be checked before a refusal is persisted as a provider block.
type openRouterErrorEnvelope struct {
	Error struct {
		Code     int    `json:"code"`
		Message  string `json:"message"`
		Metadata struct {
			InputEndpointCount   int    `json:"input_endpoint_count"`
			ProviderName         string `json:"provider_name"`
			IneligibilityReasons []struct {
				Reason string `json:"reason"`
			} `json:"ineligibility_reasons"`
			FailedRoutingStep string `json:"failed_routing_step"`
		} `json:"metadata"`
	} `json:"error"`
}

// parseProviderRateLimit reports a 429 produced by the selected provider, as
// opposed to a request-wide OpenRouter or account rate limit. OpenRouter names
// the upstream provider in metadata for the former. Callers must additionally
// prove that the request named exactly one endpoint before attributing it.
func parseProviderRateLimit(body []byte) bool {
	for _, candidate := range errorCandidates(body) {
		var envelope openRouterErrorEnvelope
		if json.Unmarshal(candidate, &envelope) != nil {
			continue
		}
		if envelope.Error.Code == 429 && strings.TrimSpace(envelope.Error.Metadata.ProviderName) != "" {
			return true
		}
	}
	return false
}

// parseProviderUnavailable reports OpenRouter's response when provider.only
// names an endpoint that no longer serves the selected model. Callers must
// additionally prove that the request named exactly one endpoint; without
// that request-side fact, the error only says that an entire candidate set was
// exhausted and cannot identify which provider should be removed.
func parseProviderUnavailable(body []byte) bool {
	for _, candidate := range errorCandidates(body) {
		var envelope openRouterErrorEnvelope
		if json.Unmarshal(candidate, &envelope) != nil || envelope.Error.Code != 404 {
			continue
		}
		message := strings.ToLower(strings.TrimSpace(envelope.Error.Message))
		if strings.HasPrefix(message, "no allowed providers are available") {
			return true
		}
	}
	return false
}

// blockedRefusal is OpenRouter declining to route a request on policy grounds.
//
// RequestedEndpoints is how many endpoints the refused request named, and it
// is what makes the refusal attributable: a request that pinned one provider
// and came back refused identifies that provider, while a refusal covering a
// whole fallback list says only that none of them worked and cannot single one
// out. Benchmarks always pin exactly one, so they are the reliable source.
type blockedRefusal struct {
	Reason             string
	RequestedEndpoints int
}

// parseBlockedReason reports whether body is OpenRouter refusing to route on
// policy grounds, and summarizes why. Every ineligibility reason is kept,
// comma-separated, because an endpoint can fail several rules at once and each
// one names a different setting to change.
//
// Both a plain JSON error body and an error delivered inside a stream are
// accepted: a pre-flight refusal arrives as the whole response, but reading the
// same shape out of a `data:` frame costs nothing and keeps the caller from
// having to know which it received.
func parseBlockedReason(body []byte) (blockedRefusal, bool) {
	for _, candidate := range errorCandidates(body) {
		var envelope openRouterErrorEnvelope
		if json.Unmarshal(candidate, &envelope) != nil {
			continue
		}
		metadata := envelope.Error.Metadata
		step := strings.TrimSpace(metadata.FailedRoutingStep)
		// A named non-guardrail step describes this request, not a durable
		// account restriction. Persisting it would incorrectly hide the
		// provider from unrelated requests for the next 24 hours.
		if step != "" && step != guardrailRoutingStep {
			continue
		}
		refusal := blockedRefusal{RequestedEndpoints: metadata.InputEndpointCount}
		reasons := make([]string, 0, len(metadata.IneligibilityReasons))
		for _, reason := range metadata.IneligibilityReasons {
			if trimmed := strings.TrimSpace(reason.Reason); trimmed != "" {
				reasons = append(reasons, trimmed)
			}
		}
		if len(reasons) == 0 {
			// The step name alone still identifies a policy refusal, so a
			// response that names it without itemizing counts as blocked.
			if step == guardrailRoutingStep {
				refusal.Reason = "guardrail"
				return refusal, true
			}
			continue
		}
		// Some responses omit the failed step. In that shape, accept only the
		// reason families that explicitly say the decision came from the
		// account or its guardrails; arbitrary endpoint-ineligibility reasons
		// can depend on parameters in this one request.
		if step == "" && !allPersistentPolicyReasons(reasons) {
			continue
		}
		sort.Strings(reasons)
		refusal.Reason = strings.Join(reasons, ", ")
		return refusal, true
	}
	return blockedRefusal{}, false
}

func allPersistentPolicyReasons(reasons []string) bool {
	if len(reasons) == 0 {
		return false
	}
	for _, reason := range reasons {
		normalized := strings.ToLower(strings.TrimSpace(reason))
		if !strings.HasSuffix(normalized, "-by-guardrail") && !strings.HasSuffix(normalized, "-by-account") {
			return false
		}
	}
	return true
}

// errorCandidates yields the JSON documents in body that could hold an error
// envelope: the body itself, and the payload of each server-sent-event frame.
func errorCandidates(body []byte) [][]byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}
	candidates := [][]byte{trimmed}
	for _, line := range bytes.Split(trimmed, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
			candidates = append(candidates, payload)
		}
	}
	return candidates
}

// shortBlockReason compresses a reason list into a label narrow enough for the
// provider table. The distinction that matters there is which setting to go
// change, not how many endpoints tripped it.
func shortBlockReason(reason string) string {
	labels := make([]string, 0, 2)
	if strings.Contains(reason, "zdr") {
		labels = append(labels, "zdr")
	}
	if strings.Contains(reason, "training") {
		labels = append(labels, "training")
	}
	if len(labels) == 0 {
		if reason == "" {
			return "policy"
		}
		return strings.SplitN(reason, ",", 2)[0]
	}
	return strings.Join(labels, "+")
}

// blockedSet indexes a model's currently-effective refusals by provider tag.
// Entries past blockedProviderTTL are left out, so a stale block stops
// excluding its provider and the next benchmark re-probes it.
func blockedSet(cfg providerConfig, now time.Time) map[string]blockedProvider {
	if len(cfg.Blocked) == 0 {
		return nil
	}
	set := make(map[string]blockedProvider, len(cfg.Blocked))
	for _, entry := range cfg.Blocked {
		if entry.Provider != "" && entry.fresh(now) {
			set[entry.Provider] = entry
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// partitionBlocked splits providers into the routable ones and the blocked
// ones, preserving the relative order of each group. Blocked providers are
// kept rather than dropped so the dashboard can still show them, marked
// broken, instead of silently shrinking the list a user is comparing against
// OpenRouter's own.
func partitionBlocked(providers []string, blocked map[string]blockedProvider) (routable, unroutable []string) {
	if len(blocked) == 0 {
		return providers, nil
	}
	routable = make([]string, 0, len(providers))
	for _, provider := range providers {
		if _, isBlocked := blocked[provider]; isBlocked {
			unroutable = append(unroutable, provider)
			continue
		}
		routable = append(routable, provider)
	}
	return routable, unroutable
}

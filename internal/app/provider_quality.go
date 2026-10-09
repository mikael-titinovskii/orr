package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
)

const providerQualityTTL = 15 * time.Minute
const providerQualityRetryDelay = 30 * time.Second

type providerQualityCache struct {
	values     map[string]providerQuality
	fetchedAt  time.Time
	generation uint64
}

// Each metric contributes equally. Unknown metrics contribute the neutral
// midpoint rather than a fabricated zero. Quality adjusts the cost-delay score
// by at most 25% in either direction; it cannot bypass health or latency tiers.
func providerQualityMultiplier(quality providerQuality) float64 {
	total := 0.0
	for _, metric := range []struct {
		value     *float64
		maximum   float64
		errorRate bool
	}{
		{quality.GPQA, 1, false},
		{quality.Tau, 1, false},
		{quality.ToolError, 100, true},
		{quality.StructuredError, 100, true},
	} {
		value := 0.5
		if validQualityValue(metric.value, metric.maximum) {
			value = *metric.value / metric.maximum
			if metric.errorRate {
				value = 1 - value
			}
		}
		total += value
	}
	return 1.25 - 0.5*(total/4)
}

// Dashboard and benchmark fetches share a per-model cache and in-flight work.
// Only completed successful fetches get a freshness timestamp. Invalidation
// advances a generation so superseded fetches cannot repopulate the cache.
func (r *routingState) refreshProviderQuality(ctx context.Context, client *http.Client, origin, apiKey, model string) (providerQualityCache, error) {
	for {
		r.mu.Lock()
		generation := r.qualityGenerations[model]
		if err := ctx.Err(); err != nil {
			r.mu.Unlock()
			return providerQualityCache{generation: generation}, err
		}
		if cached, ok := r.qualityCache[model]; ok && r.now().Sub(cached.fetchedAt) < providerQualityTTL {
			r.mu.Unlock()
			return cached, nil
		}
		if done, ok := r.qualityLoading[model]; ok {
			r.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return providerQualityCache{generation: generation}, ctx.Err()
			}
		}
		done := make(chan struct{})
		r.qualityLoading[model] = done
		r.mu.Unlock()
		values, err := fetchProviderQuality(ctx, client, origin, apiKey, model)
		r.mu.Lock()
		delete(r.qualityLoading, model)
		close(done)
		if generation != r.qualityGenerations[model] {
			r.mu.Unlock()
			continue
		}
		cached := providerQualityCache{generation: generation}
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		if err == nil {
			cached.values, cached.fetchedAt = values, r.now()
			r.qualityCache[model] = cached
		} else {
			delete(r.qualityCache, model)
		}
		r.mu.Unlock()
		return cached, err
	}
}

func (r *routingState) invalidateProviderQuality(model string) {
	r.mu.Lock()
	delete(r.qualityCache, model)
	r.qualityGenerations[model]++
	r.mu.Unlock()
}

// These are OpenRouter's published scores, independent of local request stats.
// Benchmark scores are fractions; chart error rates are already percentages.
type providerQuality struct {
	GPQA            *float64
	Tau             *float64
	ToolError       *float64
	StructuredError *float64
}

type qualityEndpoint struct {
	ID           string `json:"id"`
	Tag          string `json:"provider_slug"`
	ModelVariant string `json:"model_variant_slug"`
}

type qualityBenchmark struct {
	EndpointID string   `json:"endpoint_id"`
	Type       string   `json:"benchmark_type"`
	Score      *float64 `json:"score"`
}

type qualityChartPoint struct {
	Values map[string]*float64 `json:"y"`
}

// The website API is not part of OpenRouter's stable /api/v1 contract. Fetches
// are bounded and optional, shared by the dashboard and provider benchmarks.
func fetchProviderQuality(ctx context.Context, client *http.Client, origin, apiKey, model string) (map[string]providerQuality, error) {
	var catalog struct {
		Data []struct {
			ID        string `json:"id"`
			Canonical string `json:"canonical_slug"`
		} `json:"data"`
	}
	if err := fetchQualityJSON(ctx, client, origin+"/api/v1/models", apiKey, &catalog); err != nil {
		return nil, err
	}
	canonical := ""
	for _, entry := range catalog.Data {
		if entry.ID == model {
			canonical = entry.Canonical
			break
		}
	}
	if canonical == "" {
		return nil, fmt.Errorf("OpenRouter quality metadata unavailable")
	}
	query := url.Values{"permaslug": {canonical}}
	var endpoints struct {
		Data []qualityEndpoint `json:"data"`
	}
	endpointURL := origin + "/api/frontend/v1/stats/endpoint?" + query.Encode()
	if err := fetchQualityJSON(ctx, client, endpointURL, apiKey, &endpoints); err != nil {
		return nil, err
	}
	var benchmarks struct {
		Data struct {
			Scores []qualityBenchmark `json:"scores"`
		} `json:"data"`
	}
	var tools, structured struct {
		Data []qualityChartPoint `json:"data"`
	}
	var wg sync.WaitGroup
	wg.Add(3)
	var benchmarkErr, toolErr, structuredErr error
	benchmarkQuery := query.Encode()
	go func() {
		defer wg.Done()
		benchmarkErr = fetchQualityJSON(ctx, client, origin+"/api/frontend/v1/stats/benchmark-scores?"+benchmarkQuery, apiKey, &benchmarks)
	}()
	query.Set("timeRange", "1w")
	chartQuery := query.Encode()
	go func() {
		defer wg.Done()
		toolErr = fetchQualityJSON(ctx, client, origin+"/api/frontend/v1/stats/tool-call-error-rate?"+chartQuery, apiKey, &tools)
	}()
	go func() {
		defer wg.Done()
		structuredErr = fetchQualityJSON(ctx, client, origin+"/api/frontend/v1/stats/structured-output-error-rate?"+chartQuery, apiKey, &structured)
	}()
	wg.Wait()
	if benchmarkErr != nil && toolErr != nil && structuredErr != nil {
		return nil, fmt.Errorf("OpenRouter quality metrics unavailable")
	}
	if benchmarkErr != nil {
		benchmarks.Data.Scores = nil
	}
	if toolErr != nil {
		tools.Data = nil
	}
	if structuredErr != nil {
		structured.Data = nil
	}
	return mergeProviderQuality(model, endpoints.Data, benchmarks.Data.Scores, tools.Data, structured.Data), nil
}

func fetchQualityJSON(ctx context.Context, client *http.Client, endpoint, apiKey string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", "orr/"+Version)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch OpenRouter quality metrics failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("OpenRouter quality metrics returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil || len(body) > 8<<20 {
		return fmt.Errorf("OpenRouter quality response could not be read")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("OpenRouter quality response could not be decoded")
	}
	return nil
}

func mergeProviderQuality(model string, endpoints []qualityEndpoint, scores []qualityBenchmark, tools, structured []qualityChartPoint) map[string]providerQuality {
	result := make(map[string]providerQuality)
	tagCounts := make(map[string]int)
	for _, ep := range endpoints {
		if ep.ModelVariant == model && ep.Tag != "" {
			tagCounts[ep.Tag]++
		}
	}
	for _, ep := range endpoints {
		// Never copy a provider-wide or automatic-routing baseline to an exact
		// endpoint. Ambiguous deployment tags stay blank rather than guessing.
		if ep.ID == "" || ep.ModelVariant != model || tagCounts[ep.Tag] != 1 {
			continue
		}
		quality := providerQuality{ToolError: averageQualityChart(tools, ep.ID), StructuredError: averageQualityChart(structured, ep.ID)}
		for _, score := range scores {
			if score.EndpointID != ep.ID || !validQualityValue(score.Score, 1) {
				continue
			}
			switch score.Type {
			case "gpqa_diamond":
				quality.GPQA = score.Score
			case "tau_bench_verified_airline":
				quality.Tau = score.Score
			}
		}
		result[ep.Tag] = quality
	}
	return result
}

func validQualityValue(value *float64, maximum float64) bool {
	return value != nil && !math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= 0 && *value <= maximum
}

// Match the website's legend: arithmetic mean of available daily percentages,
// including reported zeroes, without request-volume weighting or filling gaps.
func averageQualityChart(points []qualityChartPoint, id string) *float64 {
	var total float64
	count := 0
	for _, point := range points {
		value := point.Values[id]
		if validQualityValue(value, 100) {
			total += *value
			count++
		}
	}
	if count == 0 {
		return nil
	}
	average := total / float64(count)
	return &average
}

// Normalize each metric independently across the listed providers. Benchmark
// scores use the same higher-is-better direction as throughput; error intensity
// increases with the rate and excludes zeroes, which are displayed blank.
func applyProviderQualityScores(providers []string, qualities map[string]providerQuality, scores map[string]providerScores) {
	gpqa, tau := make(map[string]float64), make(map[string]float64)
	tools, structured := make(map[string]float64), make(map[string]float64)
	for _, provider := range providers {
		quality := qualities[provider]
		if validQualityValue(quality.GPQA, 1) {
			gpqa[provider] = *quality.GPQA
		}
		if validQualityValue(quality.Tau, 1) {
			tau[provider] = *quality.Tau
		}
		if validQualityValue(quality.ToolError, 100) && *quality.ToolError > 0 {
			tools[provider] = *quality.ToolError
		}
		if validQualityValue(quality.StructuredError, 100) && *quality.StructuredError > 0 {
			structured[provider] = *quality.StructuredError
		}
	}
	gpqaLow, gpqaHigh := robustBounds(gpqa)
	tauLow, tauHigh := robustBounds(tau)
	toolLow, toolHigh := robustBounds(tools)
	jsonLow, jsonHigh := robustBounds(structured)
	for _, provider := range providers {
		s := scores[provider]
		s.gpqa, s.tau = noScore, noScore
		s.orToolError, s.orJSONError = noScore, noScore
		if value, ok := gpqa[provider]; ok {
			s.gpqa = scoreFor(value, gpqaHigh, gpqaLow)
		}
		if value, ok := tau[provider]; ok {
			s.tau = scoreFor(value, tauHigh, tauLow)
		}
		if value, ok := tools[provider]; ok {
			s.orToolError = errorRateScore(value, toolLow, toolHigh)
		}
		if value, ok := structured[provider]; ok {
			s.orJSONError = errorRateScore(value, jsonLow, jsonHigh)
		}
		scores[provider] = s
	}
}

func applyMeasuredErrorScores(providers []string, records map[string][]requestRecord, scores map[string]providerScores) {
	api, tools := make(map[string]float64), make(map[string]float64)
	for _, provider := range providers {
		apiRate, toolRate := groupErrorRateValues(records[provider])
		if apiRate > 0 {
			api[provider] = apiRate
		}
		if toolRate > 0 {
			tools[provider] = toolRate
		}
	}
	apiLow, apiHigh := robustBounds(api)
	toolLow, toolHigh := robustBounds(tools)
	for _, provider := range providers {
		s := scores[provider]
		s.apiError, s.toolError = noScore, noScore
		if value, ok := api[provider]; ok {
			s.apiError = errorRateScore(value, apiLow, apiHigh)
		}
		if value, ok := tools[provider]; ok {
			s.toolError = errorRateScore(value, toolLow, toolHigh)
		}
		scores[provider] = s
	}
}

func errorRateScore(value, low, high float64) float64 {
	if low == high {
		return 1
	}
	return scoreFor(value, high, low)
}

// Keep every nonzero error visibly red, from pale red to the existing error
// color. Unlike benchmark colors, error intensity does not use contrast boosts.
func qualityErrorColor(score float64) lipgloss.Color {
	if score >= 1 {
		return lipgloss.Color("203")
	}
	shade := 215 - int(120*max(0, score))
	return lipgloss.Color(fmt.Sprintf("#ff%02x%02x", shade, shade))
}

func styleQualityError(value string, score float64) string {
	if value == "" || score < 0 {
		return value
	}
	return lipgloss.NewStyle().Foreground(qualityErrorColor(score)).Render(value)
}

func providerQualityCells(quality providerQuality) []string {
	benchmark := func(value *float64) string {
		if !validQualityValue(value, 1) {
			return ""
		}
		return fmt.Sprintf("%.1f%%", *value*100)
	}
	errorRate := func(value *float64) string {
		if !validQualityValue(value, 100) || *value == 0 {
			return ""
		}
		if *value < .005 {
			return "<0.01%"
		}
		return fmt.Sprintf("%.2f%%", *value)
	}
	return []string{benchmark(quality.GPQA), benchmark(quality.Tau), errorRate(quality.ToolError), errorRate(quality.StructuredError)}
}

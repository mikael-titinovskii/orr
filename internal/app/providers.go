package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const openRouterAPI = "https://openrouter.ai/api/v1"

// decimal unmarshals JSON numbers that may be encoded as strings (e.g.
// OpenRouter pricing fields such as "0.00000066"). JSON null is treated as
// zero, matching a missing pricing field.
type decimal float64

func (d *decimal) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		*d = 0
		return nil
	}
	var n float64
	if err := json.Unmarshal(trimmed, &n); err == nil {
		*d = decimal(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return fmt.Errorf("decimal must be a JSON number or string: %w", err)
	}
	parsed, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("decimal string %q parse: %w", s, err)
	}
	*d = decimal(parsed)
	return nil
}

func (d decimal) float() float64 { return float64(d) }

type endpointPricing struct {
	Prompt            decimal `json:"prompt"`
	Completion        decimal `json:"completion"`
	Image             decimal `json:"image"`
	Request           decimal `json:"request"`
	InputCacheRead    decimal `json:"input_cache_read"`
	InputCacheWrite   decimal `json:"input_cache_write"`
	InputCacheWrite1h decimal `json:"input_cache_write_1h"`
}

func (ep endpointPricing) supportsCaching() bool {
	return ep.InputCacheRead > 0 || ep.InputCacheWrite > 0 || ep.InputCacheWrite1h > 0
}

func (ep endpointPricing) cacheWrite() decimal {
	if ep.InputCacheWrite > 0 {
		return ep.InputCacheWrite
	}
	return ep.InputCacheWrite1h
}

type endpointList struct {
	Data struct {
		Endpoints []modelEndpoint `json:"endpoints"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type modelEndpoint struct {
	ProviderName            string          `json:"provider_name"`
	Tag                     string          `json:"tag"`
	Status                  int             `json:"status"`
	SupportedParameters     []string        `json:"supported_parameters"`
	SupportsImplicitCaching bool            `json:"supports_implicit_caching"`
	Pricing                 endpointPricing `json:"pricing"`
	Throughput              *percentiles    `json:"throughput_last_30m"`
	Latency                 *percentiles    `json:"latency_last_30m"`
	Uptime                  *float64        `json:"uptime_last_30m"`
	Quantization            string          `json:"quantization"`
}

type percentiles struct {
	P50 float64 `json:"p50"`
}

func runProviders(envPath, rawModel string, output io.Writer) error {
	model := strings.TrimSpace(rawModel)
	parts := strings.SplitN(model, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("model must use the OpenRouter author/model format")
	}

	fileEnv, err := readDotEnv(envPath)
	if err != nil {
		return err
	}
	apiKey := envValue(fileEnv, "OPENROUTER_API_KEY")
	apiBase := envValue(fileEnv, "ORR_UPSTREAM")
	if apiBase == "" {
		apiBase = openRouterAPI
	}

	client := &http.Client{Timeout: 20 * time.Second}
	result, err := fetchEndpoints(context.Background(), client, apiBase, apiKey, model)
	if err != nil {
		return err
	}

	compatible := compatibleEndpoints(result.Data.Endpoints)
	if len(compatible) == 0 {
		return fmt.Errorf("no healthy tool-capable endpoints found for %s", model)
	}
	printEndpoints(output, compatible)
	return nil
}

func fetchEndpoints(ctx context.Context, client *http.Client, apiBase, apiKey, model string) (endpointList, error) {
	parts := strings.SplitN(model, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return endpointList{}, fmt.Errorf("model must use the OpenRouter author/model format")
	}
	endpoint := strings.TrimRight(apiBase, "/") + "/models/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/endpoints"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return endpointList{}, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("User-Agent", "orr/"+Version)

	resp, err := client.Do(req)
	if err != nil {
		return endpointList{}, fmt.Errorf("query OpenRouter: %w", err)
	}
	defer resp.Body.Close()

	var result endpointList
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result); err != nil {
		// Drain the remaining body so the connection can be reused; the decode
		// failure means the response was malformed or over the size cap.
		_, _ = io.Copy(io.Discard, resp.Body)
		return endpointList{}, fmt.Errorf("decode OpenRouter response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if result.Error != nil && result.Error.Message != "" {
			return endpointList{}, fmt.Errorf("OpenRouter returned %d: %s", resp.StatusCode, result.Error.Message)
		}
		return endpointList{}, fmt.Errorf("OpenRouter returned %s", resp.Status)
	}
	return result, nil
}

func compatibleEndpoints(endpoints []modelEndpoint) []modelEndpoint {
	compatible := make([]modelEndpoint, 0, len(endpoints))
	for _, candidate := range endpoints {
		if candidate.Status == 0 && supports(candidate, "tools") && supports(candidate, "tool_choice") {
			compatible = append(compatible, candidate)
		}
	}
	return rankEndpoints(compatible, false)
}

// rankEndpoints orders endpoints by throughput, cache-read price, and latency.
// Missing or non-positive cache prices and latencies sort after reported values.
func rankEndpoints(endpoints []modelEndpoint, cacheOnly bool) []modelEndpoint {
	if cacheOnly {
		filtered := make([]modelEndpoint, 0, len(endpoints))
		for _, endpoint := range endpoints {
			if endpoint.Pricing.supportsCaching() || endpoint.SupportsImplicitCaching {
				filtered = append(filtered, endpoint)
			}
		}
		endpoints = filtered
	}
	sort.SliceStable(endpoints, func(i, j int) bool {
		return rankLess(endpointRankFields(endpoints[i]), endpointRankFields(endpoints[j]))
	})
	return endpoints
}

// rankFields is the subset of endpoint data the shared ranking compares.
type rankFields struct {
	throughput float64
	cachePrice float64
	hasCache   bool
	latency    float64
}

// rankLess orders endpoints by throughput (desc), cache-read price (asc), then
// latency (asc). Missing or non-positive cache prices and latencies sort after
// reported values.
func rankLess(left, right rankFields) bool {
	if left.throughput != right.throughput {
		return left.throughput > right.throughput
	}
	if left.hasCache {
		if !right.hasCache {
			return true
		} else if left.cachePrice != right.cachePrice {
			return left.cachePrice < right.cachePrice
		}
	} else if right.hasCache {
		return false
	}
	leftMissing := left.latency <= 0
	rightMissing := right.latency <= 0
	if leftMissing != rightMissing {
		return !leftMissing
	}
	if !leftMissing {
		return left.latency < right.latency
	}
	return false
}

func endpointRankFields(endpoint modelEndpoint) rankFields {
	cachePrice, hasCache := cacheReadPrice(endpoint)
	return rankFields{
		throughput: p50(endpoint.Throughput),
		cachePrice: cachePrice,
		hasCache:   hasCache,
		latency:    p50(endpoint.Latency),
	}
}

func cacheReadPrice(endpoint modelEndpoint) (float64, bool) {
	price := endpoint.Pricing.InputCacheRead.float()
	return price, price > 0
}

func supports(endpoint modelEndpoint, parameter string) bool {
	for _, supported := range endpoint.SupportedParameters {
		if supported == parameter {
			return true
		}
	}
	return false
}

func p50(value *percentiles) float64 {
	if value == nil {
		return -1
	}
	return value.P50
}

func printEndpoints(output io.Writer, endpoints []modelEndpoint) {
	w := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ENDPOINT\tPROVIDER\tTOKENS/S P50\tLATENCY P50\tUPTIME 30M\tCACHE")
	for _, endpoint := range endpoints {
		throughput := ""
		if endpoint.Throughput != nil {
			throughput = fmt.Sprintf("%.1f", endpoint.Throughput.P50)
		}
		latency := ""
		if endpoint.Latency != nil {
			latency = fmt.Sprintf("%.0f ms", endpoint.Latency.P50)
		}
		uptime := ""
		if endpoint.Uptime != nil {
			uptime = fmt.Sprintf("%.1f%%", *endpoint.Uptime)
		}
		cache := ""
		if endpoint.Pricing.supportsCaching() || endpoint.SupportsImplicitCaching {
			cache = "yes"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", endpoint.Tag, endpoint.ProviderName, throughput, latency, uptime, cache)
	}
	_ = w.Flush()
}

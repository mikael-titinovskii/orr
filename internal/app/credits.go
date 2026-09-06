package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var creditPollInterval = 5 * time.Minute
var statsSaveInterval = 30 * time.Second

func fetchCreditInfo(ctx context.Context, client *http.Client, upstream, apiKey string, s *stats) {
	base, err := url.Parse(strings.TrimRight(upstream, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		log.Printf("credit poll: invalid upstream URL")
		return
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/key"
	base.RawQuery = ""
	base.Fragment = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		log.Printf("credit poll: create request: %v", err)
		return
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("User-Agent", "orr/"+Version)
	response, err := client.Do(request)
	if err != nil {
		log.Printf("credit poll: %v", err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		log.Printf("credit poll: upstream returned %s", response.Status)
		return
	}
	var result struct {
		Data struct {
			UsageMonthly json.RawMessage `json:"usage_monthly"`
			Limit        json.RawMessage `json:"limit"`
			LimitReset   *string         `json:"limit_reset"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		log.Printf("credit poll: decode response: %v", err)
		return
	}
	var usage, limit *float64
	if raw := result.Data.UsageMonthly; len(raw) > 0 && string(raw) != "null" {
		if n, ok := decodeJSONFloat(raw); ok {
			usage = &n
		}
	}
	if raw := result.Data.Limit; len(raw) > 0 {
		if string(raw) == "null" {
			// A null limit means the key has no credit cap.
			zero := 0.0
			limit = &zero
		} else if n, ok := decodeJSONFloat(raw); ok {
			limit = &n
		}
	}
	// Only update state when the response actually carries data, so a 200 with
	// an empty or foreign envelope cannot zero out previously good values.
	if usage != nil || limit != nil || result.Data.LimitReset != nil {
		usageValue, limitValue, resetValue := 0.0, 0.0, ""
		if usage != nil {
			usageValue = *usage
		}
		if limit != nil {
			limitValue = *limit
		}
		if result.Data.LimitReset != nil {
			resetValue = *result.Data.LimitReset
		}
		s.updateKeyInfo(usageValue, limitValue, resetValue)
	}
}

// decodeJSONFloat decodes a JSON number that may be encoded as a string (for
// example OpenRouter quoting usage_monthly as "12.5"). JSON null and other
// shapes are rejected.
func decodeJSONFloat(raw json.RawMessage) (float64, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, false
	}
	var n float64
	if err := json.Unmarshal(trimmed, &n); err == nil {
		return n, true
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func pollCredits(ctx context.Context, client *http.Client, upstream, apiKey string, s *stats) {
	if strings.TrimSpace(apiKey) == "" {
		return
	}
	poll := func() { fetchCreditInfo(ctx, client, upstream, apiKey, s) }
	poll()
	ticker := time.NewTicker(creditPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}

func saveStatsPeriodically(ctx context.Context, s *stats, path string, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(statsSaveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.save(path); err != nil {
				log.Printf("periodic stats save: %v", err)
			}
		}
	}
}

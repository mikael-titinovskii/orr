package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// An operator window, opened by `orr guardrails allow`, lets a request a rail
// would refuse through until it expires. The window lives in the proxy's
// memory: a restart closes it, so an allow never outlives the process that
// granted it, and the database needs no row for it.
//
// It waives verdicts, not failures. A sidecar that cannot answer is still a 503
// in block mode: "let this flagged text through" is a different decision from
// "run without a guardrail".
const (
	guardrailAllowDefault = 5 * time.Minute
	guardrailAllowMin     = time.Minute
	guardrailAllowMax     = time.Hour
)

// parseGuardrailAllowDuration reads the window a caller asked for. An empty
// value is the default, so both `orr guardrails allow` and the API's empty body
// mean five minutes.
func parseGuardrailAllowDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return guardrailAllowDefault, nil
	}
	span, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse duration: %v", err)
	}
	if span < guardrailAllowMin || span > guardrailAllowMax {
		return 0, fmt.Errorf("duration must be between %s and %s", shortDuration(guardrailAllowMin), shortDuration(guardrailAllowMax))
	}
	return span, nil
}

// allowGuardrails opens the window for the given span and returns its end. A
// second call replaces the window rather than extending it, so the expiry is
// always something an operator asked for.
func (p *proxy) allowGuardrails(span time.Duration) time.Time {
	until := p.guardrailNow().Add(span)
	p.guardrailAllowMu.Lock()
	p.guardrailAllowUntil = until
	p.guardrailAllowMu.Unlock()
	return until
}

// guardrailsAllowed reports whether the operator window is open, and so a
// blocked verdict must be recorded rather than enforced.
func (p *proxy) guardrailsAllowed() bool {
	p.guardrailAllowMu.Lock()
	defer p.guardrailAllowMu.Unlock()
	return p.guardrailNow().Before(p.guardrailAllowUntil)
}

// guardrailNow reads the proxy's clock, tolerating the zero value a unit test
// builds when it only needs a check path.
func (p *proxy) guardrailNow() time.Time {
	if p.now == nil {
		return time.Now()
	}
	return p.now()
}

// guardrailsEnforce reports whether a blocked verdict should refuse the
// request. It folds the configured action and the operator window together so
// every surface — input, tool result, and output — answers the same way.
func (p *proxy) guardrailsEnforce(action string) bool {
	return action == guardrailsActionBlock && !p.guardrailsAllowed()
}

// requestGuardrailAllow opens a window on the running proxy through the
// management API, which is the only channel a separate `orr` process has to it.
func requestGuardrailAllow(settings guardrailsConfig, span time.Duration) (time.Time, error) {
	body, err := json.Marshal(map[string]string{"duration": span.String()})
	if err != nil {
		return time.Time{}, err
	}
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodPost, "http://"+managementAddress(settings)+"/api/v1/guardrails/allow", bytes.NewReader(body))
	if err != nil {
		return time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var payload struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&payload); err == nil && payload.Error != "" {
			return time.Time{}, fmt.Errorf("management API returned %s: %s", resp.Status, payload.Error)
		}
		return time.Time{}, fmt.Errorf("management API returned %s", resp.Status)
	}
	var payload struct {
		Until time.Time `json:"until"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&payload); err != nil {
		return time.Time{}, err
	}
	return payload.Until, nil
}

// managementAddress is the loopback address the management API listens on.
func managementAddress(settings guardrailsConfig) string {
	if settings.Listen != "" {
		return settings.Listen
	}
	return "127.0.0.1:8787"
}

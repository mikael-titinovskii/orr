package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseGuardrailAllowDuration(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  time.Duration
		bad   bool
	}{
		{name: "default", value: "", want: guardrailAllowDefault},
		{name: "minute", value: "1m", want: time.Minute},
		{name: "hour", value: "1h", want: time.Hour},
		{name: "trimmed", value: " 30m ", want: 30 * time.Minute},
		{name: "under a minute", value: "59s", bad: true},
		{name: "over an hour", value: "61m", bad: true},
		{name: "unparsable", value: "soon", bad: true},
		{name: "unitless", value: "5", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			span, err := parseGuardrailAllowDuration(tc.value)
			if tc.bad {
				if err == nil {
					t.Fatalf("duration %q was accepted as %s", tc.value, span)
				}
				return
			}
			if err != nil || span != tc.want {
				t.Fatalf("duration %q = %s, %v; want %s", tc.value, span, err, tc.want)
			}
		})
	}
}

// TestGuardrailsAllowWindowWaivesBlock proves the window turns a refusal into a
// recorded verdict and closes again on its own.
func TestGuardrailsAllowWindowWaivesBlock(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"blocked","rail":"regex check input"}`))
	}))
	defer sidecar.Close()

	var clock time.Time
	p := &proxy{
		cfg: config{Guardrails: guardrailsConfig{
			Mode: guardrailsModeOn, Action: guardrailsActionBlock, URL: sidecar.URL,
		}},
		guardrailsClient: &http.Client{},
		now:              func() time.Time { return clock },
	}
	body := []byte(`{"model":"a/b","messages":[{"role":"user","content":"x"}]}`)

	clock = time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	p.allowGuardrails(5 * time.Minute)
	var record requestRecord
	p.checkGuardrailsInput(context.Background(), body, &record)
	if record.GuardBlocked || !record.GuardObserved {
		t.Fatalf("record = %+v, want the block observed inside the window", record)
	}
	if record.GuardRail != "regex check input" || record.GuardPhase != "input" {
		t.Fatalf("record = %+v, want the verdict recorded", record)
	}

	clock = clock.Add(5 * time.Minute)
	record = requestRecord{}
	p.checkGuardrailsInput(context.Background(), body, &record)
	if !record.GuardBlocked || record.GuardObserved {
		t.Fatalf("record = %+v, want the block enforced once the window closed", record)
	}
}

// TestRecordGuardVerdictRespectsWindow covers the output and tool-call surface:
// both decide through recordGuardVerdict, which must fold in the same window the
// input and tool-result checks use.
func TestRecordGuardVerdictRespectsWindow(t *testing.T) {
	verdict := guardrailVerdict{Status: "blocked", Rail: "injection detection"}

	p := &proxy{}
	var record requestRecord
	blocked, rail := p.recordGuardVerdict(&record, verdict, guardrailsActionBlock, "answer")
	if !blocked || !record.GuardBlocked || record.GuardObserved || rail != "injection detection" {
		t.Fatalf("record = %+v, rail = %q; want the answer refused", record, rail)
	}
	if record.GuardPhase != "output" {
		t.Fatalf("phase = %q, want output", record.GuardPhase)
	}

	p = &proxy{}
	p.allowGuardrails(time.Minute)
	record = requestRecord{}
	blocked, rail = p.recordGuardVerdict(&record, verdict, guardrailsActionBlock, "answer")
	if blocked || record.GuardBlocked || !record.GuardObserved {
		t.Fatalf("record = %+v, rail = %q; want the block recorded inside the window", record, rail)
	}
	if record.GuardRail != "injection detection" {
		t.Fatalf("rail = %q, want the verdict still named", record.GuardRail)
	}
}

func TestGuardrailsAllowEndpoint(t *testing.T) {
	a := testManagementAPI(t)
	now := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	a.p.now = func() time.Time { return now }

	r := apiRequest(a, http.MethodPost, "guardrails/allow", `{"duration":"30m"}`)
	if r.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", r.Code, r.Body.String())
	}
	var payload struct {
		Until time.Time `json:"until"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if want := now.Add(30 * time.Minute); !payload.Until.Equal(want) {
		t.Fatalf("until = %s, want %s", payload.Until, want)
	}
	if !a.p.guardrailsAllowed() {
		t.Fatal("the window did not open")
	}

	if r := apiRequest(a, http.MethodPost, "guardrails/allow", `{"duration":"2h"}`); r.Code != http.StatusBadRequest {
		t.Fatalf("status = %d for a two-hour window, want 400", r.Code)
	}
	if r := apiRequest(a, http.MethodPost, "guardrails/allow", `{"duration":5}`); r.Code != http.StatusBadRequest {
		t.Fatalf("status = %d for a malformed body, want 400", r.Code)
	}
	if r := apiRequest(a, http.MethodGet, "guardrails/allow", ""); r.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d for GET, want 405", r.Code)
	}

	// An empty body is the default window, so `orr guardrails allow` and the
	// API agree on what no duration means.
	now = now.Add(45 * time.Minute)
	if r := apiRequest(a, http.MethodPost, "guardrails/allow", ""); r.Code != http.StatusOK {
		t.Fatalf("status = %d for an empty body, want 200", r.Code)
	}
	if !a.p.guardrailsAllowed() {
		t.Fatal("the default window did not open")
	}
}

func TestRequestGuardrailAllow(t *testing.T) {
	until := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)
	var posted struct {
		Duration string `json:"duration"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/guardrails/allow" {
			t.Errorf("%s %s, want POST /api/v1/guardrails/allow", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"until": until.Format(time.RFC3339)})
	}))
	defer server.Close()

	got, err := requestGuardrailAllow(guardrailsConfig{Listen: server.Listener.Addr().String()}, 30*time.Minute)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if !got.Equal(until) {
		t.Fatalf("until = %s, want %s", got, until)
	}
	if posted.Duration != "30m0s" {
		t.Fatalf("duration = %q, want 30m0s", posted.Duration)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusBadRequest, "duration must be between 1m0s and 1h0m0s")
	}))
	defer failing.Close()
	if _, err := requestGuardrailAllow(guardrailsConfig{Listen: failing.Listener.Addr().String()}, time.Minute); err == nil {
		t.Fatal("an API refusal was reported as success")
	}
}

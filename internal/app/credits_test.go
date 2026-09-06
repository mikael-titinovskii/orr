package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFetchCreditInfoReadsKeyLimit(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "upstream.test" || request.URL.Path != "/api/v1/key" {
			t.Fatalf("polled %s, want https://upstream.test/api/v1/key", request.URL)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization = %q", got)
		}
		body := `{"data":{"usage_monthly":12.5,"limit":75,"limit_reset":"monthly"}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	stats := newStats()
	fetchCreditInfo(context.Background(), client, "https://upstream.test/api/v1", "test-key", stats)
	snap := stats.snapshot()
	if snap.Budget != 75 || snap.BudgetReset != "monthly" {
		t.Fatalf("budget = %v / %q", snap.Budget, snap.BudgetReset)
	}
	if snap.CreditSpend != 12.5 {
		t.Fatalf("credit spend = %v, want 12.5", snap.CreditSpend)
	}
	if stats.creditMonth != time.Now().Local().Format("2006-01") {
		t.Fatalf("credit month = %q, want current month", stats.creditMonth)
	}
}

func TestPollCreditsSkipsEmptyKey(t *testing.T) {
	called := false
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	pollCredits(context.Background(), client, "https://upstream.test/api/v1", " ", newStats())
	if called {
		t.Fatal("transport called without an API key")
	}
}

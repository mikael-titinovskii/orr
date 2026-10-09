package app

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func qualityCacheTestResponse(req *http.Request) *http.Response {
	switch req.URL.Path {
	case "/api/v1/models":
		return response(200, `{"data":[{"id":"author/model","canonical_slug":"author/permanent"}]}`)
	case "/api/frontend/v1/stats/endpoint":
		return response(200, `{"data":[{"id":"endpoint","provider_slug":"provider","model_variant_slug":"author/model"}]}`)
	case "/api/frontend/v1/stats/benchmark-scores":
		return response(200, `{"data":{"scores":[{"endpoint_id":"endpoint","benchmark_type":"gpqa_diamond","score":0.9}]}}`)
	default:
		return response(200, `{"data":[{"y":{"endpoint":0}}]}`)
	}
}

func TestDashboardQualityUsesSharedFetchTimestamp(t *testing.T) {
	const model = "author/model"
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	fetchedAt := now.Add(-providerQualityTTL + time.Second)
	routing := newRoutingState(nil, time.Hour)
	routing.now = func() time.Time { return now }
	routing.qualityCache[model] = providerQualityCache{values: map[string]providerQuality{"provider": {GPQA: floatPtr(.8)}}, fetchedAt: fetchedAt}
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return qualityCacheTestResponse(req), nil
	})}
	dashboard := newDashboard(config{}, newStats(), routing)
	dashboard.model = model
	dashboard.fetchQuality = func(model string) (providerQualityCache, error) {
		return routing.refreshProviderQuality(context.Background(), client, "https://openrouter.ai", "test-key", model)
	}
	updated, _ := dashboard.Update(dashboard.refreshQualityCmd()())
	dashboard = updated.(dashboardModel)
	if !dashboard.qualityFetchedAt[model].Equal(fetchedAt) || calls.Load() != 0 {
		t.Fatal("dashboard extended the shared cache timestamp or fetched before expiry")
	}
	now = now.Add(time.Second)
	cmd := dashboard.refreshQualityCmd()
	if cmd == nil {
		t.Fatal("dashboard failed to refresh at shared cache expiry")
	}
	updated, _ = dashboard.Update(cmd())
	dashboard = updated.(dashboardModel)
	if calls.Load() != 5 || *dashboard.quality[model]["provider"].GPQA != .9 || !dashboard.qualityFetchedAt[model].Equal(now) {
		t.Fatal("expired shared quality was not refreshed")
	}
}

func TestQualityInvalidationDiscardsInflightResult(t *testing.T) {
	for _, failFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("superseded_failure_%t", failFirst), func(t *testing.T) {
			routing := newRoutingState(nil, time.Hour)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			var catalogs atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/api/v1/models" && catalogs.Add(1) == 1 {
					close(started)
					select {
					case <-release:
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
					if failFirst {
						return response(503, "unavailable"), nil
					}
					// A superseded successful fetch must not leak stale scores.
					return response(200, `{"data":[{"id":"author/model","canonical_slug":"author/stale"}]}`), nil
				}
				if req.URL.Query().Get("permaslug") == "author/stale" && req.URL.Path == "/api/frontend/v1/stats/benchmark-scores" {
					return response(200, `{"data":{"scores":[{"endpoint_id":"endpoint","benchmark_type":"gpqa_diamond","score":0.1}]}}`), nil
				}
				return qualityCacheTestResponse(req), nil
			})}
			type outcome struct {
				cached providerQualityCache
				err    error
			}
			outcomes := make(chan outcome, 2)
			fetch := func() {
				cached, err := routing.refreshProviderQuality(ctx, client, "https://openrouter.ai", "test-key", "author/model")
				outcomes <- outcome{cached, err}
			}
			go fetch()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("fetch did not start")
			}
			routing.invalidateProviderQuality("author/model")
			go fetch()
			releaseOnce.Do(func() { close(release) })
			for range 2 {
				select {
				case got := <-outcomes:
					if got.err != nil || got.cached.generation != 1 || got.cached.values["provider"].GPQA == nil || *got.cached.values["provider"].GPQA != .9 {
						t.Fatalf("caller received superseded data or failure: %+v, err=%v", got.cached, got.err)
					}
				case <-ctx.Done():
					t.Fatal("quality waiters did not finish")
				}
			}
			if catalogs.Load() != 2 {
				t.Fatalf("catalog fetches=%d, want stale fetch plus one shared replacement", catalogs.Load())
			}
		})
	}
}

func TestFailedQualityFetchCanRetryImmediately(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled_%t", cancelFirst), func(t *testing.T) {
			routing := newRoutingState(nil, time.Hour)
			var calls atomic.Int32
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					if cancelFirst {
						cancel()
						return nil, ctx.Err()
					}
					return response(503, "unavailable"), nil
				}
				return qualityCacheTestResponse(req), nil
			})}
			if _, err := routing.refreshProviderQuality(ctx, client, "https://openrouter.ai", "test-key", "author/model"); err == nil {
				t.Fatal("expected failed first fetch")
			}
			if _, cached := routing.qualityCache["author/model"]; cached {
				t.Fatal("failed attempt became a fresh cache entry")
			}
			cached, err := routing.refreshProviderQuality(context.Background(), client, "https://openrouter.ai", "test-key", "author/model")
			if err != nil || calls.Load() != 6 || cached.values["provider"].GPQA == nil {
				t.Fatalf("retry failed: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestDashboardDiscardsQualityDeliveredAfterInvalidation(t *testing.T) {
	routing := newRoutingState(nil, time.Hour)
	dashboard := newDashboard(config{}, newStats(), routing)
	routing.invalidateProviderQuality("author/model")
	updated, _ := dashboard.Update(providerQualityMsg{model: "author/model", values: map[string]providerQuality{"provider": {GPQA: floatPtr(.1)}}, fetchedAt: routing.now(), generation: 0})
	dashboard = updated.(dashboardModel)
	if len(dashboard.quality["author/model"]) != 0 || !dashboard.qualityFetchedAt["author/model"].IsZero() {
		t.Fatal("superseded UI message restored stale data")
	}
}

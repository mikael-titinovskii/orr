package app

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestProviderQualityMatchesExactEndpointsAndPreservesMissingValues(t *testing.T) {
	const model = "author/model"
	endpoints := []qualityEndpoint{
		{ID: "fp8", Tag: "provider/fp8", ModelVariant: model},
		{ID: "fp4", Tag: "provider/fp4", ModelVariant: model},
		{ID: "other", Tag: "other", ModelVariant: "other/model"},
		{ID: "a", Tag: "ambiguous", ModelVariant: model},
		{ID: "b", Tag: "ambiguous", ModelVariant: model},
	}
	scores := []qualityBenchmark{
		{EndpointID: "fp8", Type: "gpqa_diamond", Score: floatPtr(.901)},
		{EndpointID: "fp4", Type: "gpqa_diamond", Score: floatPtr(.892)},
		{EndpointID: "fp4", Type: "tau_bench_verified_airline", Score: floatPtr(.76)},
		{Type: "gpqa_diamond", Score: floatPtr(.99)}, // Unpinned baseline.
		{EndpointID: "fp8", Type: "unknown", Score: floatPtr(.01)},
	}
	tools := []qualityChartPoint{
		{Values: map[string]*float64{"fp8": floatPtr(0), "fp4": floatPtr(.02)}},
		{Values: map[string]*float64{"fp8": floatPtr(.04), "fp4": nil}},
		{Values: map[string]*float64{"fp8": floatPtr(math.NaN())}},
	}
	quality := mergeProviderQuality(model, endpoints, scores, tools, nil)
	if len(quality) != 2 || quality["provider/fp8"].Tau != nil || quality["provider/fp4"].StructuredError != nil {
		t.Fatalf("wrong scope or missing-data handling: %+v", quality)
	}
	if *quality["provider/fp8"].GPQA != .901 || *quality["provider/fp4"].GPQA != .892 || *quality["provider/fp8"].ToolError != .02 {
		t.Fatalf("endpoint scores or daily average were mixed: %+v", quality)
	}
	if got := providerQualityCells(providerQuality{}); strings.Join(got, "") != "" {
		t.Fatalf("missing values must be blank: %q", got)
	}
	if got := providerQualityCells(providerQuality{GPQA: floatPtr(0), ToolError: floatPtr(0)}); got[0] != "0.0%" || got[2] != "" {
		t.Fatalf("reported zero score or zero error rate mishandled: %q", got)
	}
	if got := providerQualityCells(providerQuality{ToolError: floatPtr(.001)}); got[2] != "<0.01%" {
		t.Fatalf("small positive error rate rounded to zero: %q", got)
	}
}

func TestFetchProviderQualityUsesCanonicalModelAndParallelChartRequests(t *testing.T) {
	var arrivals atomic.Int32
	ready := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("configured API key missing")
		}
		switch r.URL.Path {
		case "/api/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"author/model","canonical_slug":"author/permanent"}]}`)
		case "/api/frontend/v1/stats/endpoint":
			fmt.Fprint(w, `{"data":[{"id":"endpoint","provider_slug":"provider/fp8","model_variant_slug":"author/model"}]}`)
		default:
			if arrivals.Add(1) == 3 {
				close(ready)
			}
			select {
			case <-ready:
			case <-r.Context().Done():
				return
			}
			if r.URL.Query().Get("permaslug") != "author/permanent" {
				t.Error("alias used in place of canonical model")
			}
			switch r.URL.Path {
			case "/api/frontend/v1/stats/benchmark-scores":
				fmt.Fprint(w, `{"data":{"scores":[{"endpoint_id":"endpoint","benchmark_type":"tau_bench_verified_airline","score":0.762}]}}`)
			case "/api/frontend/v1/stats/tool-call-error-rate":
				if r.URL.Query().Get("timeRange") != "1w" {
					t.Error("wrong chart window")
				}
				fmt.Fprint(w, `{"data":[{"y":{"endpoint":0}},{"y":{"endpoint":0.04}},{"y":{"endpoint":null}}]}`)
			case "/api/frontend/v1/stats/structured-output-error-rate":
				w.WriteHeader(http.StatusServiceUnavailable)
			default:
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	quality, err := fetchProviderQuality(ctx, server.Client(), server.URL, "test-key", "author/model")
	if err != nil {
		t.Fatal(err)
	}
	value := quality["provider/fp8"]
	if value.Tau == nil || *value.Tau != .762 || value.ToolError == nil || *value.ToolError != .02 || value.StructuredError != nil {
		t.Fatalf("partial metrics or percentage units wrong: %+v", value)
	}
}

func TestQualityFetchHonorsDeadlineAndDoesNotExposeResponseBodies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, "private-response-body")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var target any
	if err := fetchQualityJSON(ctx, server.Client(), server.URL+"/slow", "test-key", &target); err == nil {
		t.Fatal("deadline did not abort fetch")
	}
	err := fetchQualityJSON(context.Background(), server.Client(), server.URL+"/invalid", "test-key", &target)
	if err == nil || strings.Contains(err.Error(), "private-response-body") || strings.Contains(err.Error(), "test-key") {
		t.Fatalf("decode error leaked private data: %v", err)
	}
}

func TestDashboardQualityCacheScopesLateResultsAndClearsFailedRefresh(t *testing.T) {
	const model = "author/model"
	now := time.Now()
	routing := newRoutingState(nil, time.Hour)
	routing.now = func() time.Time { return now }
	dashboard := newDashboard(config{}, newStats(), routing)
	dashboard.model = model
	dashboard.fetchQuality = func(string) (map[string]providerQuality, error) {
		return map[string]providerQuality{"provider": {GPQA: floatPtr(.9)}}, nil
	}
	cmd := dashboard.refreshQualityCmd()
	if cmd == nil || dashboard.refreshQualityCmd() != nil {
		t.Fatal("fetch missing or not coalesced")
	}
	dashboard.model = "other/model"
	updated, _ := dashboard.Update(cmd())
	dashboard = updated.(dashboardModel)
	if len(dashboard.quality[model]) != 1 || len(dashboard.quality[dashboard.model]) != 0 {
		t.Fatal("late response leaked into selected model")
	}
	dashboard.model = model
	if dashboard.refreshQualityCmd() != nil {
		t.Fatal("cache refreshed before TTL")
	}
	now = now.Add(providerQualityTTL)
	if dashboard.refreshQualityCmd() == nil {
		t.Fatal("expired cache did not refresh")
	}
	updated, _ = dashboard.Update(providerQualityMsg{model: model, err: fmt.Errorf("unavailable")})
	dashboard = updated.(dashboardModel)
	if len(dashboard.quality[model]) != 0 || dashboard.refreshQualityCmd() != nil {
		t.Fatal("failed fetch retained stale data or retried immediately")
	}
}

func TestProviderTableShowsOpenRouterQualityAtDifferentWidths(t *testing.T) {
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"provider/fp8", "empty"}}}, time.Hour)
	routing.endpointCache[model] = []endpointMeta{{Tag: "provider/fp8", Throughput: 119, Latency: 1040,
		Pricing: endpointPricing{Prompt: .00000015, Completion: .00000060, InputCacheRead: .00000002}}}
	stats := newStats()
	stats.record(requestRecord{Time: time.Now(), Model: model, Provider: "provider/fp8", Status: 200,
		ToolCalls: 1, TTFT: 250 * time.Millisecond, Duration: 1250 * time.Millisecond, CompletionTokens: 100})
	stats.record(requestRecord{Time: time.Now(), Model: model, Provider: "provider/fp8", Status: 500, Err: true, ToolCalls: 1})
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.model = model
	dashboard.quality[model] = map[string]providerQuality{"provider/fp8": {
		GPQA: floatPtr(.901), Tau: floatPtr(.76), ToolError: floatPtr(.02), StructuredError: floatPtr(1.73),
	}}
	for _, width := range []int{36, 60, 68, 100, 110, 160} {
		view := ansi.Strip(dashboard.renderProviderTable(dashboard.stats.snapshot(), width, 100))
		for _, want := range []string{"GPQA", "Tau", "Tool", "Json", "Ts", "Lat", "90.1%", "76.0%", "0.02%", "1.73%", "50.0%"} {
			if !strings.Contains(view, want) {
				t.Fatalf("missing %q at width %d:\n%s", want, width, view)
			}
		}
		if strings.Count(view, "50.0%") != 2 {
			t.Fatalf("local API/tool error values clipped at width %d:\n%s", width, view)
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("table overflow at width %d: %q", width, line)
			}
		}
	}
}

func TestProviderTableKeepsSelectedProviderAndQualityVisibleAtLimitedHeight(t *testing.T) {
	const model = "author/model"
	providers := []string{"first", "second", "third", "selected"}
	routing := newRoutingState(map[string]providerConfig{model: {Order: providers}}, time.Hour)
	dashboard := newDashboard(config{}, newStats(), routing)
	dashboard.model, dashboard.selection = model, len(providers)-1
	dashboard.quality[model] = map[string]providerQuality{"selected": {
		GPQA: floatPtr(.901), Tau: floatPtr(.76), ToolError: floatPtr(.02), StructuredError: floatPtr(1.73),
	}}
	for _, size := range []struct{ width, height int }{{36, 8}, {68, 8}, {160, 3}} {
		view := ansi.Strip(dashboard.renderProviderTable(dashboard.stats.snapshot(), size.width, size.height))
		for _, want := range []string{"selected", "90.1%", "76.0%", "0.02%", "1.73%"} {
			if !strings.Contains(view, want) {
				t.Fatalf("selected provider missing %q at width %d:\n%s", want, size.width, view)
			}
		}
		if len(strings.Split(view, "\n")) > size.height {
			t.Fatalf("table exceeds the available height at width %d:\n%s", size.width, view)
		}
	}
}

func TestProviderTableColorsBenchmarksIndependentlyWithHigherScoresBetter(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(previous)
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"first", "middle", "last", "empty"}}}, time.Hour)
	dashboard := newDashboard(config{}, newStats(), routing)
	dashboard.model = model
	dashboard.quality[model] = map[string]providerQuality{
		"first":  {GPQA: floatPtr(.5), Tau: floatPtr(.8)},
		"middle": {GPQA: floatPtr(.7), Tau: floatPtr(.6)},
		"last":   {GPQA: floatPtr(.9), Tau: floatPtr(.4)},
		"absent": {GPQA: floatPtr(1), Tau: floatPtr(0)},
	}
	for _, width := range []int{36, 68, 160} {
		view := dashboard.renderProviderTable(dashboard.stats.snapshot(), width, 100)
		for _, cell := range []struct {
			value string
			score float64
		}{{"50.0%", 0}, {"40.0%", 0}, {"70.0%", .5}, {"60.0%", .5}, {"90.0%", 1}, {"80.0%", 1}} {
			want := lipgloss.NewStyle().Foreground(gradientColor(displayContrastScore(cell.score))).Render(cell.value)
			if !strings.Contains(view, want) {
				t.Fatalf("benchmark %s has wrong color at width %d: %q", cell.value, width, view)
			}
		}
		plain := ansi.Strip(view)
		if strings.Count(plain, "%") != 6 || strings.Contains(plain, "100.0%") {
			t.Fatalf("missing or unlisted provider affected benchmarks: %s", plain)
		}
	}
}

func TestProviderTableColorsOpenRouterErrorsFromPaleToCurrentRed(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(previous)
	const model = "author/model"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"first", "middle", "last", "zero", "empty"}}}, time.Hour)
	dashboard := newDashboard(config{}, newStats(), routing)
	dashboard.model = model
	dashboard.quality[model] = map[string]providerQuality{
		"first":  {ToolError: floatPtr(1), StructuredError: floatPtr(8)},
		"middle": {ToolError: floatPtr(2), StructuredError: floatPtr(6)},
		"last":   {ToolError: floatPtr(3), StructuredError: floatPtr(4)},
		"zero":   {ToolError: floatPtr(0), StructuredError: floatPtr(0)},
		"absent": {ToolError: floatPtr(100), StructuredError: floatPtr(100)},
	}
	for _, width := range []int{36, 68, 160} {
		view := dashboard.renderProviderTable(dashboard.stats.snapshot(), width, 100)
		for _, cell := range []struct {
			value string
			color lipgloss.Color
		}{{"1.00%", "#ffd7d7"}, {"4.00%", "#ffd7d7"}, {"2.00%", "#ff9b9b"}, {"6.00%", "#ff9b9b"}, {"3.00%", "203"}, {"8.00%", "203"}} {
			want := lipgloss.NewStyle().Foreground(cell.color).Render(cell.value)
			if !strings.Contains(view, want) {
				t.Fatalf("error rate %s has wrong shade at width %d: %q", cell.value, width, view)
			}
		}
		if strings.Count(ansi.Strip(view), "%") != 6 {
			t.Fatalf("zero or missing rates must stay blank: %q", view)
		}
	}
	dashboard.quality[model] = map[string]providerQuality{
		"first": {ToolError: floatPtr(.02)}, "middle": {ToolError: floatPtr(.02)},
	}
	view := dashboard.renderProviderTable(dashboard.stats.snapshot(), 160, 100)
	want := errorStyle.Render("0.02%")
	if strings.Count(view, want) != 2 {
		t.Fatalf("equal error rates should use the strongest red: %q", view)
	}
}

func TestProviderTableScalesMeasuredErrorsIndependentlyOfOtherColumns(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(previous)
	const model = "author/model"
	providers := []string{"first", "middle", "last", "zero", "empty"}
	routing := newRoutingState(map[string]providerConfig{model: {Order: providers}}, time.Hour)
	stats := newStats()
	now := time.Now()
	for p, provider := range providers[:3] {
		apiErrors, toolErrors := 10*(p+1), 8-2*p
		for i := 0; i < 100; i++ {
			record := requestRecord{Time: now, Model: model, Provider: provider, Status: 200}
			if i < apiErrors {
				record.Status, record.Err = 500, true
			}
			if i < toolErrors || (i >= apiErrors && i < apiErrors+10-toolErrors) {
				record.ToolCalls = 1
			}
			stats.record(record)
		}
	}
	stats.record(requestRecord{Time: now, Model: model, Provider: "zero", Status: 200, ToolCalls: 1})
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.model = model
	for _, quality := range []map[string]providerQuality{
		{"first": {ToolError: floatPtr(.01), StructuredError: floatPtr(99)}, "last": {ToolError: floatPtr(99), StructuredError: floatPtr(.01)}},
		{"first": {ToolError: floatPtr(99), StructuredError: floatPtr(.01)}, "last": {ToolError: floatPtr(.01), StructuredError: floatPtr(99)}},
	} {
		dashboard.quality[model] = quality
		for _, width := range []int{36, 68, 160} {
			view := dashboard.renderProviderTable(stats.snapshot(), width, 100)
			for _, cell := range []struct {
				value string
				color lipgloss.Color
			}{{"10.0%", "#ffd7d7"}, {"40.0%", "#ffd7d7"}, {"20.0%", "#ff9b9b"}, {"60.0%", "#ff9b9b"}, {"30.0%", "203"}, {"80.0%", "203"}} {
				want := lipgloss.NewStyle().Foreground(cell.color).Render(cell.value)
				if !strings.Contains(view, want) {
					t.Fatalf("measured rate %s affected by another column at width %d: %q", cell.value, width, view)
				}
			}
			for _, field := range strings.Fields(ansi.Strip(view)) {
				if field == "0.0%" {
					t.Fatalf("zero measured rates must stay blank: %q", view)
				}
			}
		}
	}
}

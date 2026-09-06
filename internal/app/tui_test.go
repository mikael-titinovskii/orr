package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestPercentileFloatInterpolatesAndClamps(t *testing.T) {
	values := []float64{0, 10, 20}
	for _, test := range []struct {
		name string
		p    float64
		want float64
	}{
		{name: "below range", p: -1, want: 0},
		{name: "first quartile", p: .25, want: 5},
		{name: "third quartile", p: .75, want: 15},
		{name: "above range", p: 2, want: 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := percentileFloat(values, test.p); got != test.want {
				t.Fatalf("percentileFloat(%v) = %v, want %v", test.p, got, test.want)
			}
		})
	}
	if got := percentileFloat(nil, .5); got != 0 {
		t.Fatalf("empty percentileFloat = %v, want 0", got)
	}
}

func TestRobustBoundsUsesInterpolatedP10AndP90(t *testing.T) {
	values := map[string]float64{
		"a": 0,
		"b": 10,
		"c": 20,
		"d": 30,
		"e": 40,
	}
	low, high := robustBounds(values)
	if low != 4 || high != 36 {
		t.Fatalf("robustBounds = (%v, %v), want (4, 36)", low, high)
	}
}

func TestDisplayContrastScorePreservesBoundsAndOrder(t *testing.T) {
	inputs := []float64{0, .01, .25, .64, 1}
	previous := -1.0
	for _, input := range inputs {
		got := displayContrastScore(input)
		if got < 0 || got > 1 {
			t.Fatalf("displayContrastScore(%v) = %v, outside [0,1]", input, got)
		}
		if got <= previous {
			t.Fatalf("displayContrastScore is not strictly ordered at %v: %v <= %v", input, got, previous)
		}
		previous = got
	}
	if got := displayContrastScore(-1); got != 0 {
		t.Fatalf("negative contrast score = %v, want 0", got)
	}
	if got := displayContrastScore(2); got != 1 {
		t.Fatalf("oversized contrast score = %v, want 1", got)
	}
}

func TestComputeScoresSevenProviderPriceDistribution(t *testing.T) {
	providers := []string{"cheapest", "cluster-1", "cluster-2", "cluster-3", "cluster-4", "cluster-5", "outlier"}
	prices := []float64{1, 2, 2.1, 2.2, 2.3, 2.4, 100}
	endpoints := make(map[string]endpointMeta, len(providers))
	for i, provider := range providers {
		endpoints[provider] = endpointMeta{
			Tag:     provider,
			Pricing: endpointPricing{Prompt: decimal(prices[i])},
		}
	}

	scores := computeScores(providers, endpoints, nil)
	if got := scores["cheapest"].prompt; got != 1 {
		t.Fatalf("cheapest score = %v, want endpoint 1", got)
	}
	if got := scores["outlier"].prompt; got != 0 {
		t.Fatalf("outlier score = %v, want endpoint 0", got)
	}
	for _, provider := range providers[1 : len(providers)-1] {
		if got := scores[provider].prompt; got <= 0 {
			t.Fatalf("clustered provider %q has worst score %v", provider, got)
		}
	}
}

func TestDisplayMonthSpendNeverFallsBelowLocalSpend(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	snap := statsSnapshot{
		DailyCosts:  map[string]float64{"2026-08-29": 1.9},
		CreditSpend: .7,
		CreditMonth: "2026-08",
	}
	if got := displayMonthSpend(snap, now); got != 1.9 {
		t.Fatalf("display month spend = %v, want 1.9", got)
	}
	snap.CreditSpend = 2.1
	if got := displayMonthSpend(snap, now); got != 2.1 {
		t.Fatalf("exact month spend = %v, want 2.1", got)
	}
	snap.CreditMonth = "2026-07"
	if got := displayMonthSpend(snap, now); got != 1.9 {
		t.Fatalf("stale credit spend = %v, want local spend 1.9", got)
	}
}

func TestRenderExpensesOmitsLastTurnAndRecentTitle(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	snap := statsSnapshot{DailyCosts: map[string]float64{"2026-08-29": 1.9}}
	got := renderExpenses(snap, now, 40, 100)
	for _, label := range []string{"Last turn", "Recent", "Today", "This month", "Last month"} {
		if strings.Contains(got, label) {
			t.Fatalf("expenses panel still contains %q:\n%s", label, got)
		}
	}
	if strings.Contains(got, "Expenses\n\n\n") {
		t.Fatalf("extra blank line after expenses header:\n%s", got)
	}
}

func TestRenderHistoryPerDayUsesLocalCosts(t *testing.T) {
	now := time.Date(2026, time.March, 31, 12, 0, 0, 0, time.UTC)
	snap := statsSnapshot{
		DailyCosts:  map[string]float64{"2026-03-31": 3, "2026-02-28": 7},
		CreditSpend: 92.8,
		CreditMonth: "2026-03",
	}
	got := renderHistoryPerDay(snap, now, 60)
	if !strings.Contains(got, "This m  $3.0000") {
		t.Fatalf("history used account-wide credit spend:\n%s", got)
	}
	if !strings.Contains(got, "Last m  $7.0000") {
		t.Fatalf("last month spend crossed month boundary:\n%s", got)
	}
}

func TestRenderHistoryPerDayShowsRecentPersistedDays(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	snap := statsSnapshot{
		DailyCosts: map[string]float64{
			"2026-08-29": 1.9,
			"2026-08-30": 23.0,
			"2026-07-15": 5.0,
		},
		DailyTokens: map[string]int64{
			"2026-08-29": 250_000,
			"2026-08-30": 1_250_000,
			"2026-07-15": 500_000,
		},
	}
	got := renderHistoryPerDay(snap, now, 60)
	for _, want := range []string{
		"History",
		"This m  $24.9000  1.50m",
		"Last m  $5.0000  0.50m",
		"Aug 30   $23.0000",
		"Jul 15   $5.0000",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "History per day") {
		t.Fatalf("history title was not renamed:\n%s", got)
	}
}

func TestRenderHistoryPerDayCapsAtSevenDays(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	days := make(map[string]float64)
	for i := 0; i < 10; i++ {
		day := now.AddDate(0, 0, -i).Format(time.DateOnly)
		days[day] = float64(i)
	}
	snap := statsSnapshot{DailyCosts: days}
	got := renderHistoryPerDay(snap, now, 60)
	// Seven day rows plus the populated current-month summary; the missing
	// previous month stays blank and the 8th-oldest day must be absent.
	if strings.Count(got, "$") != 1+7 {
		t.Fatalf("expected 7 day rows, got:\n%s", got)
	}
	for _, want := range []string{
		now.Format("Jan 02"),
		now.AddDate(0, 0, -6).Format("Jan 02"),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing day %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, now.AddDate(0, 0, -7).Format("Jan 02")) {
		t.Fatalf("8th-oldest day should be capped away:\n%s", got)
	}
}

func TestRenderExpensesOmitsDailyHistory(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	snap := statsSnapshot{DailyCosts: map[string]float64{"2026-08-29": 1.9}}
	got := renderExpenses(snap, now, 40, 100)
	if strings.Contains(got, "Aug 29") {
		t.Fatalf("expenses panel still contains daily history:\n%s", got)
	}
}

func TestRenderExpensesIncludesRecentSpendTable(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	snap := statsSnapshot{
		Records: []requestRecord{
			{Time: now, Model: "moonshotai/kimi-k3", Provider: "fireworks", Cost: 1.1000},
			{Time: now.Add(-5 * time.Minute), Model: "openai/gpt-4o", Provider: "openai", Cost: 1.0500},
			{Time: now.Add(-2 * time.Minute), Model: "aggregate/model", Provider: "many-small", Cost: 0.6000},
			{Time: now.Add(-3 * time.Minute), Model: "aggregate/model", Provider: "many-small", Cost: 0.6000},
			{Time: now.Add(-3 * time.Minute), Model: "cheap/model", Provider: "low-spend", Cost: 0.9999},
			{Time: now.Add(-61 * time.Minute), Model: "moonshotai/kimi-k3", Provider: "fireworks", Cost: 0.9000},
		},
	}
	got := renderExpenses(snap, now, 60, 100)
	if !strings.Contains(got, "Spend windows") || !strings.Contains(got, "8h") {
		t.Fatalf("spend-window title missing:\n%s", got)
	}
	if strings.Contains(got, "Showing 60m spend ≥ $1") {
		t.Fatalf("recent spend filter notice still visible:\n%s", got)
	}
	if strings.Contains(got, "\nBudget") {
		t.Fatalf("separate Budget row is still visible:\n%s", got)
	}
	if strings.Contains(got, "Recent") {
		t.Fatalf("recent section still has a title:\n%s", got)
	}
	if !strings.Contains(got, "moonshotai/kimi-k3") {
		t.Fatalf("missing model line:\n%s", got)
	}
	if !strings.Contains(got, "fireworks") {
		t.Fatalf("missing provider line:\n%s", got)
	}
	if !strings.Contains(got, "openai/gpt-4o") {
		t.Fatalf("missing second model line:\n%s", got)
	}
	if !strings.Contains(got, "$1.1000") {
		t.Fatalf("missing latest spend:\n%s", got)
	}
	if !strings.Contains(got, "$1.0500") {
		t.Fatalf("missing older spend:\n%s", got)
	}
	if !strings.Contains(got, "$2.0000") {
		t.Fatalf("8-hour spend is missing:\n%s", got)
	}
	if strings.Contains(got, "$0.9000") {
		t.Fatalf("older-than-60m cost leaked into table:\n%s", got)
	}
	if strings.Contains(got, "cheap/model") || strings.Contains(got, "low-spend") {
		t.Fatalf("sub-$1 spend row was not filtered:\n%s", got)
	}
	if !strings.Contains(got, "aggregate/model") || !strings.Contains(got, "many-small") || !strings.Contains(got, "$1.2000") {
		t.Fatalf("aggregated spend above $1 was filtered out:\n%s", got)
	}
	moonshot := strings.Index(got, "moonshotai/kimi-k3")
	aggregate := strings.Index(got, "aggregate/model")
	openai := strings.Index(got, "openai/gpt-4o")
	if !(moonshot < aggregate && aggregate < openai) {
		t.Fatalf("rows are not ordered by 8h spend descending:\n%s", got)
	}
}

func TestRenderExpensesCapsHeightKeepingRecentModels(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	var records []requestRecord
	for i := 0; i < 10; i++ {
		records = append(records, requestRecord{
			Time:     now.Add(-time.Duration(i) * time.Minute),
			Model:    fmt.Sprintf("model-%d", i),
			Provider: "p",
			Cost:     1.01,
		})
	}
	got := renderExpenses(statsSnapshot{Records: records}, now, 60, 8)
	if lipgloss.Height(got) != 8 {
		t.Fatalf("expenses height = %d, want 8:\n%s", lipgloss.Height(got), got)
	}
	if !strings.Contains(got, "Used") || strings.Contains(got, "$0") {
		t.Fatalf("missing budget should stay blank:\n%s", got)
	}
	if !strings.Contains(got, "model-0") {
		t.Fatalf("newest model dropped:\n%s", got)
	}
	if strings.Contains(got, "model-9") {
		t.Fatalf("oldest model should be capped away:\n%s", got)
	}
}

func TestRenderRecentExpensesUsesWindowBufferForEightHours(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	recent := requestRecord{Time: now, Model: "author/model", Provider: "provider", Cost: 1}
	older := requestRecord{Time: now.Add(-2 * time.Hour), Model: "author/model", Provider: "provider", Cost: 2}
	snap := statsSnapshot{
		Records:       []requestRecord{recent},
		WindowRecords: []requestRecord{older, recent},
	}
	got := renderRecentExpenses(snap, 80, now, 100)
	if !strings.Contains(got, "$3.0000") {
		t.Fatalf("8h spend did not use the rolling-window buffer:\n%s", got)
	}
}

func TestSortSpendGroupsUsesLongestToShortestPriority(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	records := []requestRecord{
		{Time: now.Add(-2 * time.Hour), Model: "model", Provider: "eight-hour", Cost: 9},
		{Time: now.Add(-2 * time.Hour), Model: "model", Provider: "sixty-minute", Cost: 5},
		{Time: now.Add(-30 * time.Minute), Model: "model", Provider: "sixty-minute", Cost: 3},
		{Time: now.Add(-2 * time.Hour), Model: "model", Provider: "ten-minute", Cost: 5},
		{Time: now.Add(-5 * time.Minute), Model: "model", Provider: "ten-minute", Cost: 3},
		{Time: now.Add(-2 * time.Hour), Model: "model", Provider: "one-minute", Cost: 5},
		{Time: now.Add(-5 * time.Minute), Model: "model", Provider: "one-minute", Cost: 2},
		{Time: now, Model: "model", Provider: "one-minute", Cost: 1},
	}
	groups := groupByModelProvider(records)
	sortSpendGroups(groups, now, []time.Duration{8 * time.Hour, time.Hour, 10 * time.Minute, time.Minute})

	want := []string{"eight-hour", "one-minute", "ten-minute", "sixty-minute"}
	if len(groups) != 1 || len(groups[0].providers) != len(want) {
		t.Fatalf("unexpected grouped providers: %#v", groups)
	}
	for i, provider := range groups[0].providers {
		if provider.provider != want[i] {
			t.Fatalf("provider order[%d] = %q, want %q", i, provider.provider, want[i])
		}
	}
}

func TestRenderUsageBarAlternatesQuarterColors(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(previous)

	for _, width := range []int{20, 40} {
		got := renderUsageBar(0.3, width)
		if lipgloss.Width(got) != width {
			t.Fatalf("usage bar width = %d, want %d: %q", lipgloss.Width(got), width, got)
		}
		quarter := strings.Repeat("░", width/4)
		want := usageQuarterStyle.Render(quarter) + quarter + usageQuarterStyle.Render(quarter) + quarter
		if got != want {
			t.Fatalf("usage bar quarters = %q, want %q", got, want)
		}
	}

	if got := renderUsageBar(0.3, 10); lipgloss.Width(got) != 10 {
		t.Fatalf("narrow usage bar width = %d, want 10: %q", lipgloss.Width(got), got)
	}

	got := renderUsageBar(35, 40)
	if !strings.HasPrefix(got, strings.Repeat("█", 14)) {
		t.Fatalf("usage fill should render at full brightness above quarter shading: %q", got)
	}
}

func TestRenderExpensesCombinesUsageSpendAndBudget(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	snap := statsSnapshot{Budget: 1000, CreditSpend: 3, CreditMonth: "2026-08"}
	got := renderExpenses(snap, now, 40, 8)
	if !strings.Contains(got, "Used $3 / $1000 ") {
		t.Fatalf("combined usage row is missing:\n%s", got)
	}
	if strings.Contains(got, "0.3%") {
		t.Fatalf("numeric usage is still visible:\n%s", got)
	}
	if strings.Contains(got, "\nBudget") {
		t.Fatalf("separate Budget row is still visible:\n%s", got)
	}
	lines := strings.Split(ansi.Strip(got), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "Used ") && (i+1 >= len(lines) || lines[i+1] != "") {
			t.Fatalf("usage row is not followed by a blank line:\n%s", got)
		}
	}
}

func TestRenderHistoryLeavesMissingMonthsBlank(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	got := renderHistoryPerDay(statsSnapshot{}, now, 60)
	if strings.Contains(got, "$0.0000") {
		t.Fatalf("missing month cost rendered as zero:\n%s", got)
	}
}

func TestGroupByModelProviderOrdersByRecency(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	records := []requestRecord{
		{Time: now.Add(-5 * time.Minute), Model: "a/b", Provider: "p2"},
		{Time: now, Model: "a/b", Provider: "p1"},
		{Time: now.Add(-2 * time.Minute), Model: "c/d", Provider: "p3"},
	}
	groups := groupByModelProvider(records)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	if groups[0].model != "a/b" || groups[1].model != "c/d" {
		t.Fatalf("model order = %q, %q", groups[0].model, groups[1].model)
	}
	if len(groups[0].providers) != 2 || groups[0].providers[0].provider != "p1" || groups[0].providers[1].provider != "p2" {
		t.Fatalf("provider order = %#v", groups[0].providers)
	}
}

func TestRenderRecentExpensesGroupsProvidersByModel(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	snap := statsSnapshot{
		Records: []requestRecord{
			{Time: now, Model: "moonshotai/kimi-k3", Provider: "fireworks", Cost: 1.1000},
			{Time: now.Add(-2 * time.Minute), Model: "moonshotai/kimi-k3", Provider: "together", Cost: 1.2000},
			{Time: now.Add(-5 * time.Minute), Model: "openai/gpt-4o", Provider: "openai", Cost: 1.0500},
		},
	}
	got := renderRecentExpenses(snap, 80, now, 100)

	// The model name should appear exactly once even though two providers were used.
	modelCount := strings.Count(got, "moonshotai/kimi-k3")
	if modelCount != 1 {
		t.Fatalf("model name appears %d times, want 1:\n%s", modelCount, got)
	}

	// Both providers should be listed under the shared model.
	if !strings.Contains(got, "fireworks") {
		t.Fatalf("missing fireworks provider:\n%s", got)
	}
	if !strings.Contains(got, "together") {
		t.Fatalf("missing together provider:\n%s", got)
	}

	// Providers should be indented under the model.
	lines := strings.Split(got, "\n")
	var modelIndex int
	for i, line := range lines {
		if strings.TrimSpace(line) == "moonshotai/kimi-k3" {
			modelIndex = i
			break
		}
	}
	if modelIndex == 0 {
		t.Fatal("model line not found")
	}
	if !strings.HasPrefix(lines[modelIndex+1], "  together") {
		t.Fatalf("together provider not indented under model:\n%s", got)
	}
	if !strings.HasPrefix(lines[modelIndex+2], "  fireworks") {
		t.Fatalf("fireworks provider not indented under model:\n%s", got)
	}

	// Providers should be ordered by spend, longest window first.
	if strings.Index(got, "together") > strings.Index(got, "fireworks") {
		t.Fatalf("providers not ordered by spend:\n%s", got)
	}
}

func TestOverlayWatermarkRightAlignsToCorner(t *testing.T) {
	line := strings.Repeat("x", 10)
	content := strings.Join([]string{line, line, line, line, line, line, line}, "\n")
	got := overlayWatermark(content, 80)
	lines := strings.Split(got, "\n")
	if len(lines) != 7 {
		t.Fatalf("line count = %d, want 7", len(lines))
	}
	// Logo sits one row above the bottom (bottom margin), right-aligned.
	if lines[0] != line {
		t.Fatalf("top row was modified:\n%q", lines[0])
	}
	if lines[6] != line {
		t.Fatalf("bottom margin row was modified:\n%q", lines[6])
	}
	for i, logoRow := range watermarkArt {
		plain := ansi.Strip(lines[1+i])
		if !strings.HasSuffix(plain, padRight(logoRow, watermarkWidth())) {
			t.Fatalf("row %d does not end with the watermark row:\n%q", i, plain)
		}
	}
}

func TestOverlayWatermarkSkipsShortContent(t *testing.T) {
	content := "a\nb"
	if got := overlayWatermark(content, 80); got != content {
		t.Fatalf("short content was modified:\n%q", got)
	}
}

func TestOverlayWatermarkTextPassesOver(t *testing.T) {
	line := strings.Repeat("x", 70)
	content := strings.Join([]string{line, line, line, line, line, line, line}, "\n")
	got := overlayWatermark(content, 80)
	plain := ansi.Strip(strings.Split(got, "\n")[1])
	want := strings.Repeat("x", 70) + watermarkArt[0][10:]
	if plain != want {
		t.Fatalf("log text did not draw over the watermark:\n got %q\nwant %q", plain, want)
	}
}

func TestOverlayWatermarkKeepsAnsiStyling(t *testing.T) {
	styled := "\x1b[38;5;203m" + strings.Repeat("x", 70) + "\x1b[0m"
	content := strings.Join([]string{styled, styled, styled, styled, styled, styled, styled}, "\n")
	got := overlayWatermark(content, 80)
	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, "\x1b[") {
			t.Fatalf("ANSI escape codes were lost:\n%q", line)
		}
		if !strings.HasPrefix(ansi.Strip(line), strings.Repeat("x", 70)) {
			t.Fatalf("log text was not preserved over the watermark:\n%q", line)
		}
	}
}

func TestRenderLogTableAlignsColumnsAndTruncatesCells(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	record := requestRecord{
		Time: now, Model: "very/long/model/name/with/a/long/suffix", Provider: "provider", Status: 200,
		Duration: 1250 * time.Millisecond, PromptTokens: 1200, CachedTokens: 600, CompletionTokens: 3456,
		Cost: 0.0123,
	}
	width := logTableMinWidth
	header := ansi.Strip(renderLogTableHeader(width))
	row := ansi.Strip(renderLogRows([]requestRecord{record}, width))
	if lipgloss.Width(header) != lipgloss.Width(row) {
		t.Fatalf("header width = %d, row width = %d:\nheader %q\nrow %q", lipgloss.Width(header), lipgloss.Width(row), header, row)
	}
	if !strings.Contains(row, "very/lon") || !strings.Contains(row, "…") {
		t.Fatalf("long model was not truncated:\n%s", row)
	}
	for _, want := range []string{"Duration", "Tokens in", "Tokens in u", "Tokens out", "Think%", "Tok/s", "Cache read", "Cache write", "Cache%", "Reason"} {
		if !strings.Contains(header, want) {
			t.Fatalf("missing table header %q:\n%s", want, header)
		}
	}
}

func TestRenderLogTableNarrowFallbackAndErrorStyling(t *testing.T) {
	record := requestRecord{Time: time.Now(), Model: "model", Provider: "provider", Status: 500, Err: true}
	got := renderLogTableHeader(40) + "\n" + renderLogRows([]requestRecord{record}, 40)
	plain := ansi.Strip(got)
	if !strings.Contains(plain, "Time  Code  Duration  Cost") || !strings.Contains(plain, "Model / Provider") {
		t.Fatalf("narrow table did not use the compact layout:\n%s", plain)
	}
	if !strings.Contains(plain, "model / provider") || !strings.Contains(plain, "500") {
		t.Fatalf("narrow fallback omitted fields:\n%s", plain)
	}
	if errorStyle.Render("x") == "x" {
		t.Skip("lipgloss color profile disables ANSI styling")
	}
	if !strings.Contains(got, "\x1b[") {
		t.Fatalf("error row is not styled:\n%q", got)
	}
}

func TestLogCellFormatsPlainTokensAndSecondsDuration(t *testing.T) {
	if got := logTokenValue(36000); got != "36000" {
		t.Fatalf("logTokenValue = %q, want 36000", got)
	}
	if got := logTokenValue(0); got != "" {
		t.Fatalf("logTokenValue(0) = %q, want blank", got)
	}
	record := requestRecord{CachedTokens: 36000, CacheWriteTokens: 5000}
	if got := logCacheReadText(record); got != "36000" {
		t.Fatalf("logCacheReadText = %q, want 36000", got)
	}
	if got := logCacheWriteText(record); got != "5000" {
		t.Fatalf("logCacheWriteText = %q, want 5000", got)
	}
	if got := logDurationText(2427 * time.Millisecond); got != "2.4s" {
		t.Fatalf("logDurationText = %q, want 2.4s", got)
	}
	if got := logDurationText(0); got != "" {
		t.Fatalf("logDurationText(0) = %q, want blank", got)
	}
}

func TestLogUncachedText(t *testing.T) {
	if got := logUncachedText(requestRecord{PromptTokens: 1200, CachedTokens: 600}); got != "600" {
		t.Fatalf("logUncachedText = %q, want 600", got)
	}
	if got := logUncachedText(requestRecord{PromptTokens: 1200}); got != "1200" {
		t.Fatalf("logUncachedText without cache = %q, want 1200", got)
	}
	if got := logUncachedText(requestRecord{PromptTokens: 600, CachedTokens: 600}); got != "" {
		t.Fatalf("fully cached input should render as blank, got %q", got)
	}
	if got := logUncachedText(requestRecord{}); got != "" {
		t.Fatalf("no tokens should render as blank, got %q", got)
	}
}

func TestLogReasoningText(t *testing.T) {
	if got := logReasoningText(requestRecord{ReasoningEffort: "xhigh"}); got != "xhigh" {
		t.Fatalf("logReasoningText = %q, want xhigh", got)
	}
	if got := logReasoningText(requestRecord{}); got != "" {
		t.Fatalf("no reasoning effort should render as blank, got %q", got)
	}
}

func TestLogThinkPercentText(t *testing.T) {
	if got := logThinkPercentText(requestRecord{}); got != "" {
		t.Fatalf("no reasoning tokens should render as blank, got %q", got)
	}
	if got := logThinkPercentText(requestRecord{CompletionTokens: 500, ReasoningTokens: 250}); got != "50%" {
		t.Fatalf("logThinkPercentText = %q, want 50%%", got)
	}
	// Some providers report reasoning tokens outside the completion count;
	// the share must never exceed 100%.
	if got := logThinkPercentText(requestRecord{CompletionTokens: 100, ReasoningTokens: 300}); got != "100%" {
		t.Fatalf("logThinkPercentText = %q, want 100%%", got)
	}
}

func TestDashboardLogKeepsHeaderFixedAndWatermark(t *testing.T) {
	stats := newStats()
	now := time.Now()
	for i := 0; i < 20; i++ {
		stats.record(requestRecord{Time: now.Add(time.Duration(i) * time.Second), Model: "model", Provider: "provider", Status: 200})
	}
	routing := newRoutingState(nil, time.Hour)
	dashboard := newDashboard(config{}, stats, routing)
	updated, _ := dashboard.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	result := updated.(dashboardModel)
	view := result.View()
	plain := ansi.Strip(view)
	if !strings.Contains(plain, "Duration") {
		t.Fatalf("dashboard log header missing:\n%s", view)
	}
	if !strings.Contains(plain, "____") {
		t.Fatalf("dashboard log watermark missing:\n%s", view)
	}
	updated, _ = result.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	result = updated.(dashboardModel)
	if result.autoScroll {
		t.Fatal("PgUp re-enabled auto-scroll")
	}
	if !strings.Contains(ansi.Strip(result.View()), "Duration") {
		t.Fatalf("log header scrolled away after PgUp:\n%s", result.View())
	}
}

func TestDashboardShowsReleaseUpdateInLogTitle(t *testing.T) {
	dashboard := newDashboard(config{}, newStats(), newRoutingState(nil, time.Hour))
	updated, _ := dashboard.Update(releaseUpdateResultMsg{version: "v0.3.0"})
	updated, _ = updated.(dashboardModel).Update(tea.WindowSizeMsg{Width: 190, Height: 60})
	result := updated.(dashboardModel)

	if got := ansi.Strip(result.View()); !strings.Contains(got, "• v0.3.0 available run ./orr upgrade") {
		t.Fatalf("dashboard log title missing release notice:\n%s", result.View())
	}
}

func TestDashboardStartsReleaseCheckOnInit(t *testing.T) {
	dashboard := newDashboard(config{}, newStats(), newRoutingState(nil, time.Hour))
	dashboard.checkRelease = func() (string, error) { return "v0.3.0", nil }

	batch, ok := dashboard.Init()().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("dashboard Init message = %#v, want render ticker and release check", batch)
	}
	message, ok := batch[1]().(releaseUpdateResultMsg)
	if !ok || message.version != "v0.3.0" || message.err != nil {
		t.Fatalf("initial release check result = %#v, want update available", message)
	}
}

func TestDashboardRunsReleaseCheckAfterTimer(t *testing.T) {
	dashboard := newDashboard(config{}, newStats(), newRoutingState(nil, time.Hour))
	called := false
	dashboard.checkRelease = func() (string, error) {
		called = true
		return "v0.3.0", nil
	}

	updated, command := dashboard.Update(releaseUpdateTickMsg{})
	if command == nil {
		t.Fatal("release timer did not start an update check")
	}
	message := command()
	if !called {
		t.Fatal("release timer command did not call the update checker")
	}
	updated, next := updated.(dashboardModel).Update(message)
	result := updated.(dashboardModel)
	if result.releaseUpdate != "v0.3.0" {
		t.Fatal("successful release check did not mark the update available")
	}
	if next == nil {
		t.Fatal("successful release check did not schedule the next hourly check")
	}
}

func TestDashboardKeepsReleaseNoticeWhenCheckFails(t *testing.T) {
	dashboard := newDashboard(config{}, newStats(), newRoutingState(nil, time.Hour))
	dashboard.releaseUpdate = "v0.3.0"
	updated, next := dashboard.Update(releaseUpdateResultMsg{err: context.DeadlineExceeded})
	result := updated.(dashboardModel)
	if result.releaseUpdate != "v0.3.0" {
		t.Fatal("a failed release check cleared the existing update notice")
	}
	if next == nil {
		t.Fatal("failed release check did not schedule a retry")
	}
}

func TestDashboardViewFitsTerminalHeight(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	stats := newStats()
	now := time.Now()
	stats.record(requestRecord{
		Time: now, Model: model, Provider: "fireworks", Cost: 0.1,
		PromptTokens: 100, CachedTokens: 50,
	})
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"fireworks"}}}, time.Hour)
	routing.setCurrentModel(model)
	dashboard := newDashboard(config{}, stats, routing)
	updated, _ := dashboard.Update(tea.WindowSizeMsg{Width: 190, Height: 60})
	view := updated.(dashboardModel).View()
	if got := lipgloss.Height(view); got > 60 {
		t.Fatalf("view overflows terminal height 60, got %d:\n%s", got, view)
	}
	for _, title := range []string{"Expenses", "Routing", "Log", "Stopwatch"} {
		if !strings.Contains(view, title) {
			t.Fatalf("missing %q panel title:\n%s", title, view)
		}
	}
	if strings.Contains(view, "Performance") {
		t.Fatalf("dashboard still renders the Performance panel:\n%s", view)
	}
}

func TestDashboardViewFitsTerminalBoundsAcrossWidths(t *testing.T) {
	const model = "deepseek/deepseek-v4-flash-0731"
	now := time.Now()
	stats := newStats()
	stats.record(requestRecord{
		Time: now, Model: model, Provider: "streamlake/fp8", Status: 200,
		Duration: 1500 * time.Millisecond, TTFT: 500 * time.Millisecond,
		PromptTokens: 10_000, CompletionTokens: 200,
	})
	providers := []string{
		"reka/fp4", "streamlake/fp8", "relace/fp4", "coreweave/fp8",
		"inceptron/fp4", "wafer/fast", "siliconflow/fp8", "atlas-cloud/fp4",
	}
	routing := newRoutingState(map[string]providerConfig{
		model: {Order: providers, UpdatedAt: now},
	}, time.Hour)
	routing.setCurrentModel(model)
	for _, provider := range providers {
		routing.endpointCache[model] = append(routing.endpointCache[model], endpointMeta{
			Tag: provider, Throughput: 120, Latency: 1250,
			Pricing: endpointPricing{Prompt: 0.00000011, Completion: 0.00000066, InputCacheRead: 0.000000007},
		})
	}

	for _, width := range []int{120, 190, 240, 320} {
		dashboard := newDashboard(config{}, stats, routing)
		updated, _ := dashboard.Update(tea.WindowSizeMsg{Width: width, Height: 60})
		view := updated.(dashboardModel).View()
		if got := lipgloss.Width(view); got != width {
			t.Fatalf("dashboard width = %d at terminal width %d, want an exact fit:\n%s", got, width, view)
		}
		if got := lipgloss.Height(view); got > 60 {
			t.Fatalf("dashboard height = %d at terminal width %d, want <= 60:\n%s", got, width, view)
		}
	}
}

// TestDashboardViewFitsNarrowTerminalWithHistory guards the History panel
// against wrapping: its lines are clipped to the half-width, so the panel
// must render at the height expensesHeights computed even on narrow
// terminals with a full 7-day history.
func TestDashboardViewFitsNarrowTerminalWithHistory(t *testing.T) {
	stats := newStats()
	now := time.Now()
	for i := 0; i < 7; i++ {
		day := now.AddDate(0, 0, -i).Format(time.DateOnly)
		stats.record(requestRecord{
			Time: now.AddDate(0, 0, -i), Model: "m", Provider: "p",
			Cost: 1234.5678, PromptTokens: 1000, CompletionTokens: 500,
		})
		stats.dailyCosts[day] = 1234.5678
		stats.dailyTokens[day] = 1_200_000
	}
	routing := newRoutingState(map[string]providerConfig{}, time.Hour)
	for _, width := range []int{190, 120} {
		dashboard := newDashboard(config{}, stats, routing)
		updated, _ := dashboard.Update(tea.WindowSizeMsg{Width: width, Height: 60})
		view := updated.(dashboardModel).View()
		if got := lipgloss.Height(view); got > 60 {
			t.Fatalf("view overflows terminal height 60 at width %d, got %d:\n%s", width, got, view)
		}
	}
}

func TestDashboardSplitsHistoryAndStopwatchHalfWidth(t *testing.T) {
	stats := newStats()
	routing := newRoutingState(nil, time.Hour)
	dashboard := newDashboard(config{}, stats, routing)
	updated, _ := dashboard.Update(tea.WindowSizeMsg{Width: 190, Height: 60})
	result := updated.(dashboardModel)
	view := result.View()
	plain := ansi.Strip(view)
	if !strings.Contains(plain, "History") || !strings.Contains(plain, "Stopwatch") {
		t.Fatalf("left column missing History or Stopwatch:\n%s", view)
	}
	// The Log pane is back to full width: its hint no longer mentions the
	// stopwatch key.
	if strings.Contains(plain, "s stopwatch") {
		t.Fatalf("Log hint still mentions stopwatch:\n%s", view)
	}
	// The Stopwatch title must sit in the right half of the History panel:
	// the panel is split 50/50, so it starts after the History half.
	columnWidth := 190/3 - 2
	historyHalfWidth := (columnWidth - 2) / 2
	for _, line := range strings.Split(plain, "\n") {
		if idx := strings.Index(line, "Stopwatch"); idx >= 0 {
			if idx < historyHalfWidth+2 {
				t.Fatalf("Stopwatch starts at column %d, want right half (>= %d):\n%s", idx, historyHalfWidth+2, line)
			}
			return
		}
	}
	t.Fatal("Stopwatch title not found in any line")
}

func TestEnterPinsSelectedProviderAndPersistsIt(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	stats := newStats()
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"fireworks", "together"}}}, time.Hour)
	routing.setCurrentModel(model)
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()
	dashboard.selection = 1
	updated, _ := dashboard.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result := updated.(dashboardModel)
	if got := result.routing.pinnedProvider(model); got != "together" {
		t.Fatalf("pinned provider = %q", got)
	}
	if saved, _ := result.routing.modelConfig(model); saved.ManualPin != "together" {
		t.Fatalf("persisted provider = %q", saved.ManualPin)
	}
	if _, exists := result.stats.providerPinsSnapshot()[model]; exists {
		t.Fatal("manual pin was also persisted in stats")
	}
	updated, _ = result.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result = updated.(dashboardModel)
	if provider, manual := result.routing.pinInfo(model); provider != "fireworks" || manual {
		t.Fatalf("pin after clearing manual override = %q manual=%v, want automatic fireworks", provider, manual)
	}
	if saved, _ := result.routing.modelConfig(model); saved.ManualPin != "" {
		t.Fatalf("cleared provider pin remained in providers config: %q", saved.ManualPin)
	}
	if persisted := result.stats.providerPinsSnapshot()[model]; persisted.Provider != "fireworks" {
		t.Fatalf("automatic fallback pin was not persisted: %#v", persisted)
	}
	if result.selection != 0 {
		t.Fatalf("selection = %d after clearing pin, want default index 0", result.selection)
	}
}

func TestClearingManualPinRestoresAndDisplaysAutomaticPin(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	stats := newStats()
	stats.setAutoPin(model, "automatic")
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"automatic", "manual"},
	}}, time.Hour)
	routing.setCurrentModel(model)
	routing.autoPin(model, "automatic")
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()

	// Override the measured automatic winner with a manual selection.
	dashboard.selection = 1
	updated, _ := dashboard.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result := updated.(dashboardModel)
	if provider, manual := routing.pinInfo(model); provider != "manual" || !manual {
		t.Fatalf("manual override = %q manual=%v", provider, manual)
	}
	if persisted := stats.providerPinsSnapshot()[model]; persisted.Provider != "automatic" {
		t.Fatalf("automatic pin was discarded under manual override: %#v", persisted)
	}

	// Toggling the manual pin off must reveal that automatic winner in both
	// routing behavior and the rendered Pin line.
	updated, _ = result.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result = updated.(dashboardModel)
	if provider, manual := routing.pinInfo(model); provider != "automatic" || manual {
		t.Fatalf("restored pin = %q manual=%v, want automatic", provider, manual)
	}
	view := ansi.Strip(result.renderRouting(stats.snapshot(), 80, 20))
	if !strings.Contains(view, "Pin    automatic (auto") {
		t.Fatalf("restored automatic pin is not visible:\n%s", view)
	}
	body, _, _, _, _, _, _, err := prepareRequest(strings.NewReader(`{"model":"moonshotai/kimi-k3"}`), routing, "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(body, &forwarded); err != nil {
		t.Fatal(err)
	}
	provider := forwarded["provider"].(map[string]any)
	if only := provider["only"].([]any); len(only) != 1 || only[0] != "automatic" || provider["allow_fallbacks"] != false {
		t.Fatalf("routing after clearing manual pin = %#v", provider)
	}
}

func TestClearingManualPinRecoversFreshMeasuredWinner(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	measuredAt := time.Now().Add(-5 * time.Minute)
	stats := newStats()
	stats.mu.Lock()
	stats.addBenchmarkSampleLocked(model, "automatic", benchmarkSample{
		Time: measuredAt, TPS: 100, TTFTms: 200,
	})
	stats.mu.Unlock()
	routing := newRoutingState(map[string]providerConfig{model: {
		Order:     []string{"automatic", "manual"},
		ManualPin: "manual",
	}}, time.Hour)
	routing.setCurrentModel(model)
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()

	updated, _ := dashboard.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result := updated.(dashboardModel)
	if provider, manual := routing.pinInfo(model); provider != "automatic" || manual {
		t.Fatalf("recovered pin = %q manual=%v, want automatic", provider, manual)
	}
	persisted := stats.providerPinsSnapshot()[model]
	if persisted.Provider != "automatic" || !persisted.PinnedAt.Equal(measuredAt) {
		t.Fatalf("recovered persisted pin = %#v", persisted)
	}
	view := ansi.Strip(result.renderRouting(stats.snapshot(), 80, 20))
	if !strings.Contains(view, "Pin    automatic (auto") {
		t.Fatalf("recovered automatic pin is not visible:\n%s", view)
	}
}

func TestClearingManualPinCreatesAndDisplaysRankedHeadAutoPin(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	stats := newStats()
	routing := newRoutingState(map[string]providerConfig{model: {
		Order:     []string{"automatic", "manual"},
		ManualPin: "manual",
	}}, time.Hour)
	routing.setCurrentModel(model)
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()
	dashboard.selection = 1

	updated, _ := dashboard.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result := updated.(dashboardModel)
	if provider, manual := routing.pinInfo(model); provider != "automatic" || manual {
		t.Fatalf("pin after clearing manual override = %q manual=%v, want automatic ranked head", provider, manual)
	}
	if persisted := stats.providerPinsSnapshot()[model]; persisted.Provider != "automatic" {
		t.Fatalf("ranked-head automatic pin was not persisted: %#v", persisted)
	}
	view := ansi.Strip(result.renderRouting(stats.snapshot(), 80, 20))
	if !strings.Contains(view, "Pin    automatic (auto") {
		t.Fatalf("ranked-head automatic pin is not visible:\n%s", view)
	}
	body, _, _, _, _, _, _, err := prepareRequest(strings.NewReader(`{"model":"moonshotai/kimi-k3"}`), routing, "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(body, &forwarded); err != nil {
		t.Fatal(err)
	}
	route := forwarded["provider"].(map[string]any)
	if only := route["only"].([]any); len(only) != 1 || only[0] != "automatic" || route["allow_fallbacks"] != false {
		t.Fatalf("routing after clearing manual override = %#v", route)
	}
}

func TestClearingManualPinRejectsExpiredBlockedAutoPin(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.Local)
	stats := newStats()
	stats.setAutoPinAt(model, "stale", now.Add(-2*time.Hour))
	routing := newRoutingState(map[string]providerConfig{model: {
		Order:     []string{"stale", "clean", "manual"},
		ManualPin: "manual",
		Blocked: []blockedProvider{{
			Provider: "stale", Reason: "guardrail", DetectedAt: now,
		}},
	}}, time.Hour)
	routing.now = func() time.Time { return now }
	stats.now = routing.now
	routing.setCurrentModel(model)
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()

	updated, _ := dashboard.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result := updated.(dashboardModel)
	if provider, manual := routing.pinInfo(model); provider != "clean" || manual {
		t.Fatalf("pin after clearing manual = %q manual=%v, want clean automatic", provider, manual)
	}
	if persisted := stats.providerPinsSnapshot()[model]; persisted.Provider != "clean" || !persisted.PinnedAt.Equal(now) {
		t.Fatalf("replacement automatic pin = %#v, want clean at controlled time", persisted)
	}
	if result.selection != 0 {
		t.Fatalf("selection = %d, want clean ranked head", result.selection)
	}
}

func TestClearingOnlyManualPinReturnsToDiscoveryMode(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	stats := newStats()
	routing := newRoutingState(map[string]providerConfig{model: {
		ManualPin: "manual",
	}}, time.Hour)
	routing.setCurrentModel(model)
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()
	if providers := dashboard.providers(); len(providers) != 1 || providers[0] != "manual" {
		t.Fatalf("providers before clear = %v", providers)
	}

	_, _ = dashboard.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if provider, manual := routing.pinInfo(model); provider != "" || manual {
		t.Fatalf("pin after clear = %q manual=%v, want discovery mode", provider, manual)
	}
	cfg, _ := routing.modelConfig(model)
	if cfg.ManualPin != "" || cfg.hasRouting() {
		t.Fatalf("config after clear = %+v, want empty routing awaiting discovery", cfg)
	}

	body, _, _, _, _, _, _, err := prepareRequest(strings.NewReader(
		`{"model":"moonshotai/kimi-k3","provider":{"only":["client-choice"]}}`,
	), routing, "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(body, &forwarded); err != nil {
		t.Fatal(err)
	}
	provider := forwarded["provider"].(map[string]any)
	if only := provider["only"].([]any); len(only) != 1 || only[0] != "client-choice" {
		t.Fatalf("first discovery-mode routing = %#v, want client routing preserved", provider)
	}
}

func TestEnterOnAutomaticPinClearsItExplicitly(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	stats := newStats()
	stats.setAutoPin(model, "automatic")
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"automatic", "fallback"},
	}}, time.Hour)
	routing.setCurrentModel(model)
	routing.autoPin(model, "automatic")
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()

	updated, _ := dashboard.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result := updated.(dashboardModel)
	if provider, _ := routing.pinInfo(model); provider != "" {
		t.Fatalf("automatic pin remained after explicit clear: %q", provider)
	}
	if _, ok := stats.providerPinsSnapshot()[model]; ok {
		t.Fatal("cleared automatic pin remained persisted")
	}
	view := ansi.Strip(result.renderRouting(stats.snapshot(), 80, 20))
	if !strings.Contains(view, "Pin    \n") {
		t.Fatalf("cleared automatic pin is still visible:\n%s", view)
	}
}

func TestKeyRRequestsProviderRefresh(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"fireworks"}}}, time.Hour)
	routing.setCurrentModel(model)
	dashboard := newDashboard(config{}, newStats(), routing)
	dashboard.syncModel()

	var refreshed string
	dashboard.refresh = func(model string) { refreshed = model }
	updated, _ := dashboard.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if got := updated.(dashboardModel).model; got != model {
		t.Fatalf("dashboard model = %q, want %q", got, model)
	}
	if refreshed != model {
		t.Fatalf("r key refreshed model %q, want %q", refreshed, model)
	}
}

func TestKeyTTestsSelectedProviderAndShowsResult(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"fireworks", "together"}}}, time.Hour)
	routing.setCurrentModel(model)
	stats := newStats()
	// A measured record for the tested provider so the result gets a color score.
	stats.record(requestRecord{
		Time: time.Now(), Model: model, Provider: "together", Status: 200,
		TTFT: 100 * time.Millisecond, Duration: 500 * time.Millisecond, CompletionTokens: 50,
	})
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()
	dashboard.selection = 1 // together

	var testedModel, testedProvider string
	dashboard.testProvider = func(model, provider string) (providerTestSample, error) {
		testedModel, testedProvider = model, provider
		return pingSample(120, 800*time.Millisecond), nil
	}
	updated, cmd := dashboard.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	result := updated.(dashboardModel)
	if !result.testing[testResultKey(model, "together")] {
		t.Fatal("t key did not start a provider test")
	}
	if cmd == nil {
		t.Fatal("t key returned no command")
	}
	// While testing, the spinner shows next to the provider name.
	testing := ansi.Strip(result.renderRouting(result.stats.snapshot(), 60, 20))
	if !strings.Contains(testing, "together "+spinnerFrames[0]) {
		t.Fatalf("spinner missing next to provider while testing:\n%s", testing)
	}
	msg := cmd()
	updated, _ = result.Update(msg)
	result = updated.(dashboardModel)
	if result.testing[testResultKey(model, "together")] {
		t.Fatal("test still running after result")
	}
	if testedModel != model || testedProvider != "together" {
		t.Fatalf("tested %q/%q, want %q/%q", testedModel, testedProvider, model, "together")
	}
	if result.testResults[testResultKey(model, "together")].tps != 120 || result.testResults[testResultKey(model, "together")].latency != 800*time.Millisecond || result.testResults[testResultKey(model, "together")].runs != providerTestRounds {
		t.Fatalf("test result = %#v", result.testResults[testResultKey(model, "together")])
	}
	view := ansi.Strip(result.renderRouting(result.stats.snapshot(), 60, 20))
	if strings.Contains(view, "Test    ") {
		t.Fatalf("routing view still has a Test line:\n%s", view)
	}
	// The averaged result lands in the provider table's measured columns.
	if !strings.Contains(view, "120") || !strings.Contains(view, "800") {
		t.Fatalf("routing view missing test result in table:\n%s", view)
	}
	if strings.Contains(view, "~120") || strings.Contains(view, "~800") {
		t.Fatalf("test result still has a ~ prefix:\n%s", view)
	}
	// The result is color-coded against the measured metric scores.
	if errorStyle.Render("x") != "x" {
		var found bool
		for _, line := range strings.Split(result.renderRouting(result.stats.snapshot(), 60, 20), "\n") {
			if strings.Contains(line, "mTs") && strings.Contains(line, "120") {
				found = true
				if !strings.Contains(line, "\x1b[") {
					t.Fatalf("test result cell is not color-coded:\n%q", line)
				}
			}
		}
		if !found {
			t.Fatal("test result row not found")
		}
	}
}

func TestKeyTShowsTestError(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"fireworks"}}}, time.Hour)
	routing.setCurrentModel(model)
	dashboard := newDashboard(config{}, newStats(), routing)
	dashboard.syncModel()
	dashboard.testProvider = func(model, provider string) (providerTestSample, error) {
		return providerTestSample{}, fmt.Errorf("boom")
	}
	updated, cmd := dashboard.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	result := updated.(dashboardModel)
	updated, _ = result.Update(cmd())
	result = updated.(dashboardModel)
	if result.testing[testResultKey(model, "fireworks")] {
		t.Fatal("test still running after error")
	}
	if result.testResults[testResultKey(model, "fireworks")].err == nil {
		t.Fatal("test error not recorded")
	}
	view := ansi.Strip(result.renderRouting(result.stats.snapshot(), 60, 16))
	if !strings.Contains(view, "fireworks ✗") {
		t.Fatalf("routing view missing test error marker:\n%s", view)
	}
	if strings.Contains(view, "boom") {
		t.Fatalf("routing view leaks the error message:\n%s", view)
	}
}

func TestKeyTWithoutSelectionIsNoOp(t *testing.T) {
	dashboard := newDashboard(config{}, newStats(), newRoutingState(nil, time.Hour))
	updated, cmd := dashboard.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	result := updated.(dashboardModel)
	if cmd != nil {
		t.Fatal("t key returned a command without a selection")
	}
	if len(result.testing) != 0 || len(result.testResults) != 0 {
		t.Fatalf("t key without selection changed test state: testing=%v results=%v", result.testing, result.testResults)
	}
}

func TestKeyTAllowsConcurrentTestsOnOtherProviders(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"fireworks", "together"}}}, time.Hour)
	routing.setCurrentModel(model)
	dashboard := newDashboard(config{}, newStats(), routing)
	dashboard.syncModel()
	dashboard.testProvider = func(model, provider string) (providerTestSample, error) {
		return pingSample(100, time.Second), nil
	}
	// Start a test on the first provider, then move and start another while it runs.
	updated, cmd1 := dashboard.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	result := updated.(dashboardModel)
	updated, _ = result.Update(tea.KeyMsg{Type: tea.KeyDown})
	result = updated.(dashboardModel)
	updated, cmd2 := result.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	result = updated.(dashboardModel)
	if !result.testing[testResultKey(model, "fireworks")] || !result.testing[testResultKey(model, "together")] {
		t.Fatalf("expected both providers in flight, got %v", result.testing)
	}
	if cmd1 == nil || cmd2 == nil {
		t.Fatal("expected commands for both tests")
	}
	// Both results arrive independently.
	updated, _ = result.Update(cmd1())
	result = updated.(dashboardModel)
	updated, _ = result.Update(cmd2())
	result = updated.(dashboardModel)
	if len(result.testing) != 0 {
		t.Fatalf("tests still in flight: %v", result.testing)
	}
	if result.testResults[testResultKey(model, "fireworks")].tps != 100 || result.testResults[testResultKey(model, "together")].tps != 100 {
		t.Fatalf("results missing: %#v", result.testResults)
	}
}

func TestKeyAStartsTestsForAllProviders(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"fireworks", "together"}}}, time.Hour)
	routing.setCurrentModel(model)
	dashboard := newDashboard(config{}, newStats(), routing)
	dashboard.syncModel()
	tested := make(map[string]bool)
	dashboard.testProvider = func(model, provider string) (providerTestSample, error) {
		tested[provider] = true
		return pingSample(90, 900*time.Millisecond), nil
	}
	updated, cmd := dashboard.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	result := updated.(dashboardModel)
	if cmd == nil {
		t.Fatal("a key returned no command")
	}
	if !result.testing[testResultKey(model, "fireworks")] || !result.testing[testResultKey(model, "together")] {
		t.Fatalf("a key did not start all providers, got %v", result.testing)
	}
	// Batch wraps the per-provider commands; the runtime executes each one.
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("a key returned %T, want tea.BatchMsg", msg)
	}
	for _, sub := range batch {
		if sub == nil {
			continue
		}
		if m := sub(); m != nil {
			updated, _ = result.Update(m)
			result = updated.(dashboardModel)
		}
	}
	if len(tested) != 2 {
		t.Fatalf("testProvider called for %d providers, want 2", len(tested))
	}
	if len(result.testing) != 0 {
		t.Fatalf("tests still in flight: %v", result.testing)
	}
	if result.testResults[testResultKey(model, "fireworks")].tps != 90 || result.testResults[testResultKey(model, "together")].tps != 90 {
		t.Fatalf("results missing: %#v", result.testResults)
	}
}

func TestKeyASortsProvidersAndSelectsBestAutoProvider(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"slow", "fast"}}}, time.Hour)
	routing.setCurrentModel(model)
	dashboard := newDashboard(config{}, newStats(), routing)
	dashboard.syncModel()
	dashboard.testProvider = func(model, provider string) (providerTestSample, error) {
		if provider == "fast" {
			return pingSample(200, 200*time.Millisecond), nil
		}
		return pingSample(20, time.Second), nil
	}

	updated, cmd := dashboard.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	result := updated.(dashboardModel)
	batch := cmd().(tea.BatchMsg)
	for _, sub := range batch {
		if msg := sub(); msg != nil {
			updated, _ = result.Update(msg)
			result = updated.(dashboardModel)
		}
	}
	cfg, _ := routing.modelConfig(model)
	if want := []string{"fast", "slow"}; !slices.Equal(cfg.Order, want) {
		t.Fatalf("order after a = %v, want %v", cfg.Order, want)
	}
	if provider, manual := routing.pinInfo(model); provider != "fast" || manual {
		t.Fatalf("pin after a = %q manual=%v, want fast auto", provider, manual)
	}
	if persisted := result.stats.providerPinsSnapshot()[model]; persisted.Provider != "fast" || persisted.PinnedAt.IsZero() {
		t.Fatalf("a did not persist fast auto pin: %#v", persisted)
	}
	if result.selection != 0 {
		t.Fatalf("selection after a = %d, want best provider at 0", result.selection)
	}
	body, _, _, _, _, _, _, err := prepareRequest(strings.NewReader(`{"model":"moonshotai/kimi-k3"}`), routing, "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(body, &forwarded); err != nil {
		t.Fatal(err)
	}
	provider := forwarded["provider"].(map[string]any)
	if only := provider["only"].([]any); len(only) != 1 || only[0] != "fast" || provider["allow_fallbacks"] != false {
		t.Fatalf("routing after a = %#v, want strict fast pin", provider)
	}
}

func TestFailedProviderBatchKeepsVisibleAutomaticFallback(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	stats := newStats()
	routing := newRoutingState(map[string]providerConfig{model: {
		Order: []string{"first", "second"},
	}}, time.Hour)
	routing.setCurrentModel(model)
	routing.autoPin(model, "first")
	stats.setAutoPin(model, "first")
	stats.record(requestRecord{
		Time: time.Now(), Model: model, Provider: "first",
		Status: http.StatusBadGateway, Err: true,
	})
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()

	updated, _ := dashboard.Update(providerTestBatchMsg{
		model: model,
		results: []providerTestResult{
			{provider: "first", err: context.DeadlineExceeded},
			{provider: "second", err: context.DeadlineExceeded},
		},
		testedAt: time.Now(),
		revision: routing.modelRevision(model),
	})
	result := updated.(dashboardModel)
	if provider, manual := routing.pinInfo(model); provider != "second" || manual {
		t.Fatalf("pin after failed batch = %q manual=%v, want automatic second", provider, manual)
	}
	if persisted := stats.providerPinsSnapshot()[model]; persisted.Provider != "second" {
		t.Fatalf("failed batch did not persist automatic fallback: %#v", persisted)
	}
	view := ansi.Strip(result.renderRouting(stats.snapshot(), 80, 20))
	if !strings.Contains(view, "Pin    second (auto") {
		t.Fatalf("automatic fallback is not visible after failed batch:\n%s", view)
	}
}

func TestKeyAReordersButCannotReplaceManualPin(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order:     []string{"manual", "fast"},
		ManualPin: "manual",
	}}, time.Hour)
	routing.setCurrentModel(model)
	dashboard := newDashboard(config{}, newStats(), routing)
	dashboard.syncModel()
	dashboard.testAllProviders = func(string, []string) []providerTestResult {
		return []providerTestResult{
			{provider: "manual", tps: 20, latency: time.Second},
			{provider: "fast", tps: 200, latency: 100 * time.Millisecond},
		}
	}

	updated, cmd := dashboard.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	result := updated.(dashboardModel)
	updated, _ = result.Update(cmd())
	result = updated.(dashboardModel)
	if provider, manual := routing.pinInfo(model); provider != "manual" || !manual {
		t.Fatalf("pin after a = %q manual=%v, want unchanged manual pin", provider, manual)
	}
	if persisted := result.stats.providerPinsSnapshot()[model]; persisted.Provider != "fast" || persisted.PinnedAt.IsZero() {
		t.Fatalf("measured auto winner was not retained under manual override: %#v", persisted)
	}
	cfg, _ := routing.modelConfig(model)
	if want := []string{"fast", "manual"}; !slices.Equal(cfg.Order, want) {
		t.Fatalf("order after a = %v, want %v", cfg.Order, want)
	}
	body, _, _, _, _, _, _, err := prepareRequest(strings.NewReader(`{"model":"moonshotai/kimi-k3"}`), routing, "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(body, &forwarded); err != nil {
		t.Fatal(err)
	}
	provider := forwarded["provider"].(map[string]any)
	if only := provider["only"].([]any); len(only) != 1 || only[0] != "manual" || provider["allow_fallbacks"] != false {
		t.Fatalf("routing after a = %#v, want strict manual pin", provider)
	}
}

func TestKeyAUsesBoundedBatchRunnerAndClearsCanceledProviders(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"slow", "fast", "canceled"}}}, time.Hour)
	routing.setCurrentModel(model)
	dashboard := newDashboard(config{}, newStats(), routing)
	dashboard.syncModel()
	dashboard.testAllProviders = func(gotModel string, providers []string) []providerTestResult {
		if gotModel != model || !slices.Equal(providers, []string{"slow", "fast", "canceled"}) {
			t.Fatalf("batch input = %q %v", gotModel, providers)
		}
		return []providerTestResult{
			{provider: "slow", tps: 20, latency: time.Second},
			{provider: "fast", tps: 200, latency: 200 * time.Millisecond},
		}
	}

	updated, cmd := dashboard.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	result := updated.(dashboardModel)
	if cmd == nil || len(result.testing) != 3 {
		t.Fatalf("bounded a did not mark all providers: cmd=%v testing=%v", cmd != nil, result.testing)
	}
	updated, _ = result.Update(cmd())
	result = updated.(dashboardModel)
	if len(result.testing) != 0 {
		t.Fatalf("canceled providers still marked testing: %v", result.testing)
	}
	cfg, _ := routing.modelConfig(model)
	if cfg.Order[0] != "fast" {
		t.Fatalf("bounded a winner = %q, want fast", cfg.Order[0])
	}
}

func TestTestResultsScopedToModel(t *testing.T) {
	const modelA = "moonshotai/kimi-k3"
	const modelB = "moonshotai/kimi-k4"
	routing := newRoutingState(map[string]providerConfig{
		modelA: {Order: []string{"fireworks"}},
		modelB: {Order: []string{"fireworks"}},
	}, time.Hour)
	routing.setCurrentModel(modelA)
	dashboard := newDashboard(config{}, newStats(), routing)
	dashboard.syncModel()
	dashboard.testProvider = func(model, provider string) (providerTestSample, error) {
		return pingSample(100, time.Second), nil
	}
	// Run a test under model A.
	updated, cmd := dashboard.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	result := updated.(dashboardModel)
	updated, _ = result.Update(cmd())
	result = updated.(dashboardModel)
	if _, ok := result.testResultFor(modelA, "fireworks"); !ok {
		t.Fatal("test result missing under model A")
	}
	// Switch to model B: the result must not leak into its table.
	routing.setCurrentModel(modelB)
	result.syncModel()
	view := ansi.Strip(result.renderRouting(result.stats.snapshot(), 60, 14))
	if strings.Contains(view, "100") {
		t.Fatalf("model A test result leaked into model B's table:\n%s", view)
	}
	if _, ok := result.testResultFor(modelB, "fireworks"); ok {
		t.Fatal("test result reported for model B")
	}
}

func TestExpireTestResultsClearsAfterLiveRequest(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	stats := newStats()
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"fireworks"}}}, time.Hour)
	dashboard := newDashboard(config{}, stats, routing)

	// Simulate a `t` test result that landed at t0.
	pingResult := providerTestMsg{
		model: model, provider: "fireworks",
		tps: 100, latency: 200 * time.Millisecond,
		testedAt: time.Now(),
	}
	dashboard.testResults[testResultKey(model, "fireworks")] = pingResult

	// Tick once with no live request: the test result must stick so the
	// panel shows the explicit ping-pong value.
	dashboard.syncModel()
	if _, ok := dashboard.testResultFor(model, "fireworks"); !ok {
		t.Fatal("test result dropped before any live request")
	}

	// A real proxied request for the same provider arrives after the test.
	stats.record(requestRecord{
		Time: time.Now().Add(time.Second), Model: model, Provider: "fireworks",
		Status: 200, Duration: time.Second, CompletionTokens: 80,
	})
	dashboard.syncModel()
	if _, ok := dashboard.testResultFor(model, "fireworks"); ok {
		t.Fatal("test result not cleared after live request superseded it")
	}
}

func TestExpireTestResultsScopeIsPerProvider(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	stats := newStats()
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"fireworks", "together"}}}, time.Hour)
	dashboard := newDashboard(config{}, stats, routing)

	pingedAt := time.Now()
	dashboard.testResults[testResultKey(model, "fireworks")] = providerTestMsg{model: model, provider: "fireworks", tps: 100, testedAt: pingedAt}
	dashboard.testResults[testResultKey(model, "together")] = providerTestMsg{model: model, provider: "together", tps: 90, testedAt: pingedAt}

	// Only fireworks sees a new live request; together's pin must stay.
	stats.record(requestRecord{
		Time: time.Now().Add(time.Second), Model: model, Provider: "fireworks",
		Status: 200, Duration: time.Second, CompletionTokens: 80,
	})
	dashboard.syncModel()

	if _, ok := dashboard.testResultFor(model, "fireworks"); ok {
		t.Fatal("fireworks result not cleared")
	}
	if _, ok := dashboard.testResultFor(model, "together"); !ok {
		t.Fatal("together result dropped despite no live request")
	}
}

func TestRoutingViewDistinguishesDefaultFromPin(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"fireworks"}}}, time.Hour)
	routing.setCurrentModel(model)
	dashboard := newDashboard(config{}, newStats(), routing)
	dashboard.syncModel()
	unpinned := dashboard.renderRouting(dashboard.stats.snapshot(), 40, 14)
	if !strings.Contains(unpinned, "Pin    \n") || strings.Contains(unpinned, "◆") {
		t.Fatalf("default routing appears pinned:\n%s", unpinned)
	}
	routing.pin(model, "fireworks")
	pinned := dashboard.renderRouting(dashboard.stats.snapshot(), 40, 14)
	if !strings.Contains(pinned, "Pin    fireworks (manual)") || !strings.Contains(pinned, "◆") {
		t.Fatalf("manual pin is not visible:\n%s", pinned)
	}
	routing.unpin(model)
	routing.autoPin(model, "fireworks")
	auto := dashboard.renderRouting(dashboard.stats.snapshot(), 40, 14)
	if !strings.Contains(auto, "Pin    fireworks (auto") || !strings.Contains(auto, "◈") {
		t.Fatalf("auto pin is not visible:\n%s", auto)
	}
}

func TestRoutingViewCombinesSelectionAndProviderMetrics(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"fast", "cheap"}}}, time.Hour)
	routing.setCurrentModel(model)
	routing.endpointCache[model] = []endpointMeta{
		{
			Tag:          "fast",
			Pricing:      endpointPricing{Prompt: 0.000001, Completion: 0.000004, InputCacheRead: 0.0000002},
			Throughput:   120,
			Latency:      350,
			Quantization: "fp8",
		},
		{
			Tag:          "cheap",
			Pricing:      endpointPricing{Prompt: 0.0000005, Completion: 0.000003, InputCacheRead: 0.0000001},
			Throughput:   80,
			Latency:      500,
			Quantization: "int4",
		},
	}
	stats := newStats()
	now := time.Now()
	stats.record(requestRecord{
		Time:             now.Add(-5 * time.Minute),
		Model:            model,
		Provider:         "fast",
		Status:           200,
		TTFT:             250 * time.Millisecond,
		Duration:         1250 * time.Millisecond,
		PromptTokens:     200,
		CachedTokens:     100,
		CompletionTokens: 100, // 100 tokens in 1s = 100 tok/s
	})
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()
	dashboard.selection = 1
	routing.pin(model, "cheap")

	got := dashboard.renderRouting(dashboard.stats.snapshot(), 100, 20)
	for _, want := range []string{"Routing", "Provider", "Tok/s", "Lat", "mTs", "mLat", "TTFT", "Cache%", "API Err", "Tool Err", "◆ cheap", "100", "250", "50%", "measured tok/s and lat - ttl 30m", "API P50: last 30m • website P50: 1 week"} {
		if !strings.Contains(got, want) {
			t.Fatalf("combined routing table missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"Qtz", "fp8", "int4"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("combined routing table unexpectedly contains %q:\n%s", unwanted, got)
		}
	}
	if strings.Contains(got, "Pricing") {
		t.Fatalf("routing still renders a separate Pricing section:\n%s", got)
	}

	var header string
	for _, line := range strings.Split(ansi.Strip(got), "\n") {
		if strings.HasPrefix(line, "Provider") {
			header = line
			break
		}
	}
	if want := padRight("Provider", reservedLabelWidth) + " "; !strings.HasPrefix(header, want) {
		t.Fatalf("routing provider column is not reserved like Recent:\n got %q\nwant prefix %q", header, want)
	}

	medium := ansi.Strip(dashboard.renderRouting(dashboard.stats.snapshot(), 68, 20))
	if !strings.Contains(medium, "Provider metrics") {
		t.Fatalf("68-cell routing view did not use the compact layout:\n%s", medium)
	}
	for _, want := range []string{"Provider", "Cache", "Tok/s", "mTs", "Lat", "mLat", "TTFT", "Cache%", "API Err", "Tool Err"} {
		if !strings.Contains(medium, want) {
			t.Fatalf("68-cell routing table is missing %q:\n%s", want, medium)
		}
	}

	narrow := dashboard.renderRouting(dashboard.stats.snapshot(), 36, 20)
	for _, want := range []string{"mTs", "mLat", "Tok/s", "Lat", "TTFT", "Cache%", "API", "Tool", "100", "1250"} {
		if !strings.Contains(ansi.Strip(narrow), want) {
			t.Fatalf("narrow routing view missing %q:\n%s", want, narrow)
		}
	}
}

func TestProviderTableScopesObservedMetricsToProvider(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(previous)
	const model = "author/model"
	now := time.Now()
	stats := newStats()
	for _, record := range []requestRecord{
		{Time: now, Model: model, Provider: "healthy", Status: 200, ToolCalls: 1, TTFT: 200 * time.Millisecond, PromptTokens: 100, CachedTokens: 75},
		{Time: now.Add(-31 * time.Minute), Model: model, Provider: "healthy", Status: 500, Err: true, ToolCalls: 1},
		{Time: now, Model: model, Provider: "failing", Status: 500, Err: true, TTFT: 800 * time.Millisecond, PromptTokens: 100},
		{Time: now, Model: model, Provider: "failing", Status: 500, Err: true, ToolCalls: 1, TTFT: 1200 * time.Millisecond, PromptTokens: 100},
		{Time: now, Model: "other/model", Provider: "healthy", Status: 500, Err: true, ToolCalls: 1},
	} {
		stats.record(record)
	}
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"healthy", "failing"}}}, time.Hour)
	routing.setCurrentModel(model)
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()
	dashboard.model = model
	dashboard.selection = -1
	for _, width := range []int{59, 84, 160} {
		colored := dashboard.renderProviderTable(stats.snapshot(), width, 100)
		for _, want := range []string{
			errorStyle.Render("100.0%"),
			lipgloss.NewStyle().Foreground(gradientColor(1)).Render("200"),
			lipgloss.NewStyle().Foreground(gradientColor(0)).Render("1000"),
		} {
			if !strings.Contains(colored, want) {
				t.Fatalf("missing colored metric %q at width %d: %q", want, width, colored)
			}
		}
		for _, field := range strings.Fields(ansi.Strip(colored)) {
			if field == "0.0%" {
				t.Fatalf("zero error rate still visible at width %d: %q", width, colored)
			}
		}
	}

	view := ansi.Strip(dashboard.renderRouting(stats.snapshot(), 160, 24))
	rows := make(map[string]string)
	for _, line := range strings.Split(view, "\n") {
		for _, provider := range []string{"healthy", "failing"} {
			if strings.Contains(line, provider) {
				rows[provider] = line
			}
		}
	}
	if strings.Contains(rows["healthy"], "0.0%") {
		t.Fatalf("healthy API/tool rates should be blank: %q", rows["healthy"])
	}
	if strings.Contains(rows["healthy"], "50.0%") {
		t.Fatalf("expired API/tool error leaked into healthy row: %q", rows["healthy"])
	}
	if !strings.Contains(rows["healthy"], "200") || !strings.Contains(rows["healthy"], "75%") {
		t.Fatalf("healthy TTFT/cache metrics missing from its row: %q", rows["healthy"])
	}
	if got := strings.Count(rows["failing"], "100.0%"); got != 2 {
		t.Fatalf("failing API/tool rates = %q, want two 100.0%% values:\n%s", rows["failing"], view)
	}
	if !strings.Contains(rows["failing"], "1000") {
		t.Fatalf("failing TTFT missing from its row: %q", rows["failing"])
	}
	for _, field := range strings.Fields(rows["failing"]) {
		if field == "0%" {
			t.Fatalf("zero cache-hit rate should be blank: %q", rows["failing"])
		}
	}
}

func TestProviderTableUsesMetricSpecificWidths(t *testing.T) {
	rows := [][]string{
		{"Provider", "In", "Out", "Cache", "Tok/s", "mTs", "Lat", "mLat"},
		{"  siliconflow/fp8", "0.15", "0.60", "0.020", "119", "138", "1040", "4623"},
	}
	const width = 68
	columnWidths := providerTableColumnWidths(rows, width)
	header := renderProviderTableRow(rows[0], columnWidths, width)
	values := renderProviderTableRow(rows[1], columnWidths, width)

	for _, want := range rows[0] {
		if !strings.Contains(header, want) {
			t.Fatalf("provider header truncates %q at width %d:\n%s", want, width, header)
		}
	}
	for _, want := range rows[1] {
		if !strings.Contains(values, want) {
			t.Fatalf("provider row truncates %q at width %d:\n%s", want, width, values)
		}
	}
	for _, line := range []string{header, values} {
		if got := lipgloss.Width(line); got > width {
			t.Fatalf("provider row width = %d, want <= %d:\n%s", got, width, line)
		}
		if strings.Contains(line, "…") {
			t.Fatalf("provider row is still ellipsized at width %d:\n%s", width, line)
		}
	}
}

func TestExpandedProviderTableFitsAllConsolidatedMetrics(t *testing.T) {
	rows := [][]string{
		{"Provider", "In", "Out", "Cache", "Tok/s", "mTs", "Lat", "mLat", "TTFT", "Cache%", "API Err", "Tool Err"},
		{"  provider", "0.15", "0.60", "0.020", "119", "138", "1040", "4623", "250", "100%", "100.0%", "100.0%"},
	}
	const width = 85
	columnWidths := providerTableColumnWidths(rows, width)
	header := renderProviderTableRow(rows[0], columnWidths, width)
	values := renderProviderTableRow(rows[1], columnWidths, width)

	for _, want := range rows[0] {
		if !strings.Contains(header, want) {
			t.Fatalf("consolidated header truncates %q at width %d:\n%s", want, width, header)
		}
	}
	for _, want := range rows[1] {
		if !strings.Contains(values, want) {
			t.Fatalf("consolidated row truncates %q at width %d:\n%s", want, width, values)
		}
	}
	for _, line := range []string{header, values} {
		if got := lipgloss.Width(line); got > width {
			t.Fatalf("consolidated row width = %d, want <= %d:\n%s", got, width, line)
		}
		if strings.Contains(line, "…") {
			t.Fatalf("consolidated row is ellipsized at width %d:\n%s", width, line)
		}
	}
}

func TestRoutingSelectionHighlightsRow(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"fast", "cheap"}}}, time.Hour)
	routing.setCurrentModel(model)
	routing.endpointCache[model] = []endpointMeta{
		{Tag: "fast", Pricing: endpointPricing{Prompt: 0.000001, Completion: 0.000004, InputCacheRead: 0.0000002}, Throughput: 120, Latency: 350},
		{Tag: "cheap", Pricing: endpointPricing{Prompt: 0.0000005, Completion: 0.000003, InputCacheRead: 0.0000001}, Throughput: 80, Latency: 500},
	}
	dashboard := newDashboard(config{}, newStats(), routing)
	dashboard.syncModel()
	dashboard.selection = 1

	// Tests run without a TTY, so lipgloss would otherwise render no ANSI at
	// all; pin a color profile so the highlight is actually emitted.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(prev)

	const (
		selBg   = "\x1b[48;5;237m"
		ansiRst = "\x1b[0m"
	)
	assertHighlighted := func(line string, width int) {
		t.Helper()
		if !strings.HasPrefix(line, selBg) {
			t.Fatalf("selected row does not start with the highlight:\n%q", line)
		}
		if !strings.HasSuffix(line, ansiRst) {
			t.Fatalf("selected row does not close the highlight:\n%q", line)
		}
		// The background must span the whole row: every reset the styled cells
		// embed has to re-assert it, not just the trailling one.
		if strings.Contains(strings.ReplaceAll(strings.TrimSuffix(line, ansiRst), ansiRst+selBg, ""), ansiRst) {
			t.Fatalf("highlight breaks at a cell reset instead of covering the row:\n%q", line)
		}
		if got := lipgloss.Width(line); got != width {
			t.Fatalf("selected row width = %d, want %d:\n%q", got, width, line)
		}
	}

	check := func(view string, width int) {
		t.Helper()
		for _, line := range strings.Split(view, "\n") {
			switch {
			case strings.Contains(line, "cheap"):
				assertHighlighted(line, width)
			case strings.Contains(line, "fast"):
				if strings.Contains(line, selBg) {
					t.Fatalf("unselected row is highlighted:\n%q", line)
				}
			}
		}
	}

	check(dashboard.renderRouting(dashboard.stats.snapshot(), 100, 20), 100)
	check(dashboard.renderRouting(dashboard.stats.snapshot(), 36, 20), 36)
}

func TestProviderMeasuredMetricsRespectsTTL(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	records := []requestRecord{
		{
			Time:             now.Add(-10 * time.Minute),
			Model:            model,
			Provider:         "fast",
			Status:           200,
			TTFT:             200 * time.Millisecond,
			Duration:         1200 * time.Millisecond,
			CompletionTokens: 100, // 100 TPS
		},
		{
			Time:             now.Add(-20 * time.Minute),
			Model:            model,
			Provider:         "fast",
			Status:           200,
			TTFT:             400 * time.Millisecond,
			Duration:         1400 * time.Millisecond,
			CompletionTokens: 50, // 50 TPS
		},
		{
			Time:             now.Add(-31 * time.Minute), // expired (TTL 30m)
			Model:            model,
			Provider:         "fast",
			Status:           200,
			TTFT:             1000 * time.Millisecond,
			Duration:         2000 * time.Millisecond,
			CompletionTokens: 10,
		},
		{
			Time:             now.Add(-5 * time.Minute),
			Model:            "other/model",
			Provider:         "fast",
			Status:           200,
			TTFT:             100 * time.Millisecond,
			Duration:         600 * time.Millisecond,
			CompletionTokens: 100,
		},
		{
			Time:             now.Add(-5 * time.Minute),
			Model:            model,
			Provider:         "fast",
			Status:           500,
			Err:              true,
			TTFT:             50 * time.Millisecond,
			Duration:         100 * time.Millisecond,
			CompletionTokens: 500,
		},
	}

	tps, lat := providerMeasuredMetrics(records, model, "fast", now)
	if tps != 75 { // Interpolated P50 of 50 and 100.
		t.Fatalf("measured TPS = %v, want 75", tps)
	}
	if lat != 1300 { // Interpolated P50 of 1200ms and 1400ms.
		t.Fatalf("measured Lat = %v, want 1300", lat)
	}

	tpsCheap, latCheap := providerMeasuredMetrics(records, model, "cheap", now)
	if tpsCheap != 0 || latCheap != 0 {
		t.Fatalf("expected 0 for unrequested provider, got tps=%v, lat=%v", tpsCheap, latCheap)
	}
}

func TestActiveModelsOrdersByRecencyWithinWindow(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	snap := statsSnapshot{Records: []requestRecord{
		{Time: now.Add(-2 * time.Minute), Model: "a/b"},
		{Time: now.Add(-30 * time.Second), Model: "c/d"},
		{Time: now.Add(-11 * time.Minute), Model: "e/f"},
		{Time: now.Add(-time.Minute), Model: "a/b"},
	}}
	got := activeModels(snap, now)
	want := []string{"c/d", "a/b"}
	if !slices.Equal(got, want) {
		t.Fatalf("active models = %v, want %v", got, want)
	}
}

func TestSyncModelKeepsSelectedModelWhileOthersActive(t *testing.T) {
	const modelA, modelB = "a/b", "c/d"
	stats := newStats()
	now := time.Now()
	stats.record(requestRecord{Time: now, Model: modelA, Provider: "p1"})
	routing := newRoutingState(map[string]providerConfig{
		modelA: {Order: []string{"p1"}},
		modelB: {Order: []string{"p2"}},
	}, time.Hour)
	routing.setCurrentModel(modelA)
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()
	if dashboard.model != modelA {
		t.Fatalf("initial model = %q, want %q", dashboard.model, modelA)
	}

	// A concurrent request for another model must not yank the panel away.
	stats.record(requestRecord{Time: now.Add(time.Second), Model: modelB, Provider: "p2"})
	dashboard.syncModel()
	if dashboard.model != modelA {
		t.Fatalf("panel switched to %q while %q still active", dashboard.model, modelA)
	}
	if len(dashboard.activeModels) != 2 {
		t.Fatalf("active models = %v, want 2 entries", dashboard.activeModels)
	}

	// Tab cycles to the other model.
	updated, _ := dashboard.Update(tea.KeyMsg{Type: tea.KeyTab})
	tabbed := updated.(dashboardModel)
	if tabbed.model != modelB {
		t.Fatalf("Tab switched to %q, want %q", tabbed.model, modelB)
	}
}

func TestSyncModelFallsBackWhenDisplayedModelGoesStale(t *testing.T) {
	const modelA, modelB = "a/b", "c/d"
	stats := newStats()
	// modelA is already outside the active window; syncModel falls back to the
	// last requested model for the initial display.
	stats.record(requestRecord{Time: time.Now().Add(-activeModelWindow - time.Minute), Model: modelA, Provider: "p1"})
	routing := newRoutingState(map[string]providerConfig{
		modelA: {Order: []string{"p1"}},
		modelB: {Order: []string{"p2"}},
	}, time.Hour)
	routing.setCurrentModel(modelA)
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()
	if dashboard.model != modelA {
		t.Fatalf("initial model = %q, want %q", dashboard.model, modelA)
	}

	// modelA has no recent request; the panel falls back to modelB.
	stats.record(requestRecord{Time: time.Now(), Model: modelB, Provider: "p2"})
	dashboard.syncModel()
	if dashboard.model != modelB {
		t.Fatalf("stale model kept: %q, want %q", dashboard.model, modelB)
	}
}

func TestSyncModelPreservesSelectionWhenModelStaysStale(t *testing.T) {
	const model = "a/b"
	stats := newStats()
	// model is outside the active window; routing.current() keeps it displayed.
	stats.record(requestRecord{Time: time.Now().Add(-activeModelWindow - time.Minute), Model: model, Provider: "p1"})
	routing := newRoutingState(map[string]providerConfig{model: {Order: []string{"p1", "p2"}}}, time.Hour)
	routing.setCurrentModel(model)
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()
	if dashboard.model != model {
		t.Fatalf("initial model = %q, want %q", dashboard.model, model)
	}
	// The user moves the selection away from the pinned provider.
	dashboard.selection = 1
	// Ticks keep coming while the model stays stale; the selection must survive.
	dashboard.syncModel()
	if dashboard.model != model {
		t.Fatalf("stale model was switched away: %q, want %q", dashboard.model, model)
	}
	if dashboard.selection != 1 {
		t.Fatalf("selection reset to %d, want 1", dashboard.selection)
	}
}

func TestRoutingShowsActiveModelCounter(t *testing.T) {
	const modelA, modelB = "a/b", "c/d"
	stats := newStats()
	now := time.Now()
	stats.record(requestRecord{Time: now.Add(-time.Minute), Model: modelB, Provider: "p2"})
	stats.record(requestRecord{Time: now, Model: modelA, Provider: "p1"})
	routing := newRoutingState(map[string]providerConfig{
		modelA: {Order: []string{"p1"}},
		modelB: {Order: []string{"p2"}},
	}, time.Hour)
	routing.setCurrentModel(modelA)
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()
	got := dashboard.renderRouting(dashboard.stats.snapshot(), 60, 12)
	if !strings.Contains(got, "[1/2 Tab]") {
		t.Fatalf("routing panel missing active model counter:\n%s", got)
	}
	if !strings.Contains(got, "Tab switch") {
		t.Fatalf("routing panel missing Tab hint:\n%s", got)
	}
}

// Pooled benchmark results carry no latency, so they rank providers but must
// never land in the display cache: a pooled entry stamped as just-tested
// blanked mLat behind a fresh timestamp, and it re-armed the freshness window
// of providers the run never tested.
func TestKeyAStoresFreshResultsNotPooledOnes(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order:     []string{"tested", "pooled"},
		UpdatedAt: time.Now(),
	}}, time.Hour)
	routing.setCurrentModel(model)
	stats := newStats()
	// "tested" already has pooled history and "pooled" exists only there, the
	// way a provider cancelled by the run's deadline does.
	stats.addBenchmarkSampleLocked(model, "tested", benchmarkSample{Time: time.Now(), TPS: 150, TTFTms: 300})
	stats.addBenchmarkSampleLocked(model, "pooled", benchmarkSample{Time: time.Now(), TPS: 90, TTFTms: 200})
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()
	dashboard.testAllProviders = func(string, []string) []providerTestResult {
		return []providerTestResult{
			{provider: "tested", tps: 100, ttft: 250 * time.Millisecond, latency: 2 * time.Second, samples: 3},
		}
	}

	updated, cmd := dashboard.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	updated, _ = updated.(dashboardModel).Update(cmd())
	result := updated.(dashboardModel)

	r, fresh := result.testResultFor(model, "tested")
	if !fresh || r.latency != 2*time.Second || r.tps != 100 {
		t.Fatalf("testResults for tested = %+v fresh=%v, want the run's own measurement", r, fresh)
	}
	if _, ok := result.testResults[testResultKey(model, "pooled")]; ok {
		t.Fatal("a pool-only provider was stored as freshly tested, re-arming its freshness window")
	}
}

// The measured columns describe successful requests from the last 30 minutes.
// Intermittent errors are a separate reliability signal: they must not erase
// valid measurements from later successful requests.
func TestRoutingTableKeepsMeasuredColumnsAlongsideRecentErrors(t *testing.T) {
	const model = "moonshotai/kimi-k3"
	routing := newRoutingState(map[string]providerConfig{model: {
		Order:     []string{"failing", "steady"},
		UpdatedAt: time.Now(),
	}}, time.Hour)
	routing.setCurrentModel(model)
	routing.endpointCache[model] = []endpointMeta{{Tag: "failing"}, {Tag: "steady"}}
	stats := newStats()
	now := time.Now()
	for _, record := range []requestRecord{
		// The provider answered, was rate-limited, and then recovered.
		{Time: now.Add(-10 * time.Minute), Model: model, Provider: "failing", Status: 200, Duration: 2 * time.Second, CompletionTokens: 100},
		{Time: now.Add(-2 * time.Minute), Model: model, Provider: "failing", Status: 429, Err: true, Duration: 100 * time.Millisecond},
		{Time: now.Add(-time.Minute), Model: model, Provider: "failing", Status: 200, Duration: 2 * time.Second, CompletionTokens: 100},
		{Time: now.Add(-time.Minute), Model: model, Provider: "steady", Status: 200, Duration: 3 * time.Second, CompletionTokens: 150},
	} {
		stats.record(record)
	}
	dashboard := newDashboard(config{}, stats, routing)
	dashboard.syncModel()

	stripped := ansi.Strip(dashboard.renderRouting(stats.snapshot(), 100, 20))
	lines := map[string]string{}
	for _, line := range strings.Split(stripped, "\n") {
		if strings.Contains(line, "failing") {
			lines["failing"] = line
		}
		if strings.Contains(line, "steady") {
			lines["steady"] = line
		}
	}
	if !strings.Contains(lines["failing"], "50") || !strings.Contains(lines["failing"], "2000") {
		t.Fatalf("a recent 429 erased successful measurements:\n%s", lines["failing"])
	}
	if !strings.Contains(lines["steady"], "50") {
		t.Fatalf("healthy provider lost its measured throughput:\n%s", lines["steady"])
	}

	compact := ansi.Strip(dashboard.renderRouting(stats.snapshot(), 36, 30))
	failingStart := strings.Index(compact, "failing")
	steadyStart := strings.Index(compact, "steady")
	if failingStart < 0 || steadyStart <= failingStart {
		t.Fatalf("compact provider rows are missing:\n%s", compact)
	}
	failingBlock := compact[failingStart:steadyStart]
	if !strings.Contains(failingBlock, "mTs 50") || !strings.Contains(failingBlock, "mLat 2000") {
		t.Fatalf("compact view erased successful measurements after a 429:\n%s", failingBlock)
	}
}

// A benchmark round cancelled by orr's own gate has no response status, and
// aborting it says nothing about the provider. Such records must not count as
// API errors — neither in the share that drives automatic selection nor in
// the rate the performance table shows.
func TestCancelledRoundsAreNotAPIErrors(t *testing.T) {
	const model = "author/model"
	now := time.Now()
	records := []requestRecord{
		{Time: now.Add(-time.Minute), Model: model, Provider: "slow", Status: 200, Duration: 2 * time.Second},
		{Time: now.Add(-time.Minute), Model: model, Provider: "slow", Status: 200, Duration: 2 * time.Second},
		// The gate closed while the third round was in flight.
		{Time: now, Model: model, Provider: "slow", Duration: 3 * time.Second},
		{Time: now, Model: model, Provider: "answering", Status: 500, Err: true, Duration: time.Second},
	}
	rates := providerAPIErrorRates(records, model, now)
	if rates["slow"] != 0 {
		t.Fatalf("slow error share = %v, want 0: orr's cancelled round is not the provider failing", rates["slow"])
	}
	if rates["answering"] != 1 {
		t.Fatalf("answering error share = %v, want 1 for a provider that answered with an error", rates["answering"])
	}
	api, _ := groupErrorRates(records)
	if api != "33.3%" {
		t.Fatalf("group API error rate = %q, want 33.3%% over the three answered requests, with the cancelled one ignored", api)
	}
}

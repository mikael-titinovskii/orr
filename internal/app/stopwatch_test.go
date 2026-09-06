package app

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func TestStopwatchToggleCreatesRowsWithIncrementingIDs(t *testing.T) {
	s := newStopwatch()
	now := time.Now()
	s.toggle(now, statsSnapshot{})
	s.toggle(now.Add(time.Second), statsSnapshot{})
	s.toggle(now.Add(2*time.Second), statsSnapshot{})
	if len(s.rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(s.rows))
	}
	if s.rows[0].id != 1 || s.rows[1].id != 2 {
		t.Fatalf("ids = %d, %d; want 1, 2", s.rows[0].id, s.rows[1].id)
	}
	if !s.running {
		t.Fatal("stopwatch should be running after the third toggle")
	}
	if !s.rows[0].stoppedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("row 1 stoppedAt = %v, want %v", s.rows[0].stoppedAt, now.Add(time.Second))
	}
	if !s.rows[1].stoppedAt.IsZero() {
		t.Fatalf("row 2 should still be running, stoppedAt = %v", s.rows[1].stoppedAt)
	}
}

func TestStopwatchAccumulatesMetricsFromRecords(t *testing.T) {
	s := newStopwatch()
	now := time.Now()
	s.toggle(now, statsSnapshot{SessionRequestCount: 0})
	snap := statsSnapshot{
		SessionRequestCount: 2,
		Records: []requestRecord{
			{Time: now.Add(time.Second), Model: "m", Provider: "p", Status: 200, Duration: 1000 * time.Millisecond, TTFT: 100 * time.Millisecond, PromptTokens: 400, CachedTokens: 200, CompletionTokens: 900, ReasoningTokens: 450, Cost: 0.001},
			{Time: now.Add(2 * time.Second), Model: "m", Provider: "p", Status: 200, Duration: 2000 * time.Millisecond, TTFT: 200 * time.Millisecond, PromptTokens: 600, CachedTokens: 300, CompletionTokens: 1800, ReasoningTokens: 900, Cost: 0.002},
		},
	}
	s.tick(now.Add(3*time.Second), snap)
	row := s.rows[0]
	if row.cost != 0.003 {
		t.Fatalf("cost = %v, want 0.003", row.cost)
	}
	if row.tokTotal != 3700 {
		t.Fatalf("tokTotal = %d, want 3700", row.tokTotal)
	}
	if got := stopwatchTok(row); got != "3k" {
		t.Fatalf("tok = %q, want 3k", got)
	}
	if got := stopwatchThinkPercent(row); got != "50%" {
		t.Fatalf("think = %q, want 50%%", got)
	}
	// Some providers report reasoning tokens outside the completion count;
	// the share must never exceed 100%.
	if got := stopwatchThinkPercent(stopwatchRow{completionTotal: 100, thinkTotal: 300}); got != "100%" {
		t.Fatalf("think = %q, want 100%%", got)
	}
	// A later tick with the same snapshot must not double-count.
	s.tick(now.Add(4*time.Second), snap)
	if s.rows[0].cost != 0.003 {
		t.Fatalf("cost double-counted: %v", s.rows[0].cost)
	}
}

func TestStopwatchIgnoresRecordsWhileStopped(t *testing.T) {
	s := newStopwatch()
	now := time.Now()
	s.toggle(now, statsSnapshot{SessionRequestCount: 0})
	s.tick(now.Add(time.Second), statsSnapshot{
		SessionRequestCount: 1,
		Records:             []requestRecord{{Time: now, Model: "m", Provider: "p", Status: 200, Duration: time.Second, Cost: 0.01}},
	})
	// Stop, then more requests complete while stopped.
	s.toggle(now.Add(2*time.Second), statsSnapshot{SessionRequestCount: 1})
	s.tick(now.Add(3*time.Second), statsSnapshot{
		SessionRequestCount: 3,
		Records: []requestRecord{
			{Time: now.Add(2*time.Second + 1), Model: "m", Provider: "p", Status: 200, Duration: time.Second, Cost: 0.02},
			{Time: now.Add(2*time.Second + 2), Model: "m", Provider: "p", Status: 200, Duration: time.Second, Cost: 0.03},
		},
	})
	if len(s.rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(s.rows))
	}
	if s.rows[0].cost != 0.01 {
		t.Fatalf("stopped row cost = %v, want 0.01", s.rows[0].cost)
	}
}

func TestStopwatchRenderShowsTitleAndColumns(t *testing.T) {
	s := newStopwatch()
	now := time.Now()
	s.toggle(now, statsSnapshot{})
	out := s.render(60, 100, now.Add(30*time.Second))
	for _, want := range []string{"Stopwatch", "Time", "Spent", "Tok", "Think%"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in running render:\n%s", want, out)
		}
	}
	for _, gone := range []string{"tok/s", "Lat", "Cache%"} {
		if strings.Contains(out, gone) {
			t.Fatalf("%q should be removed from stopwatch:\n%s", gone, out)
		}
	}
	if strings.Contains(out, "#") {
		t.Fatalf("id column should be removed:\n%s", out)
	}
	if !strings.Contains(out, "30.0s") {
		t.Fatalf("missing elapsed time in running row:\n%s", out)
	}
	s.toggle(now.Add(30*time.Second), statsSnapshot{})
	out = s.render(60, 100, now.Add(60*time.Second))
	if !strings.Contains(out, "30.0s") {
		t.Fatalf("missing elapsed time in stopped row:\n%s", out)
	}
}

func TestStopwatchRenderClipsRowsToMaxLines(t *testing.T) {
	s := newStopwatch()
	now := time.Now()
	// Five toggles → three rows (start/stop/start/stop/start), the newest
	// still running.
	for i := 0; i < 5; i++ {
		s.toggle(now.Add(time.Duration(i)*time.Second), statsSnapshot{})
	}
	// Wide format: 4 fixed lines + header + 3 rows = 8 lines. Clipping to 7
	// keeps the header and the two newest rows; the oldest row disappears.
	out := s.render(60, 7, now.Add(10*time.Second))
	if got := lipgloss.Height(out); got != 7 {
		t.Fatalf("height = %d, want 7 (clipped):\n%s", got, out)
	}
	if !strings.Contains(out, "6.0s") {
		t.Fatalf("newest running row missing:\n%s", out)
	}
	if strings.Count(out, "1.0s") != 1 {
		t.Fatalf("oldest row should be clipped (only one 1.0s row left):\n%s", out)
	}
}

func TestStopwatchRenderCapsAtSevenRows(t *testing.T) {
	s := newStopwatch()
	now := time.Now()
	// 15 toggles → 8 rows; the panel must show at most 7.
	for i := 0; i < 15; i++ {
		s.toggle(now.Add(time.Duration(i)*time.Second), statsSnapshot{})
	}
	out := s.render(60, 100, now.Add(20*time.Second))
	// 4 fixed lines + header + 7 rows = 12 lines; the 8th row is dropped.
	if got := lipgloss.Height(out); got != 12 {
		t.Fatalf("height = %d, want 12 (7 rows capped):\n%s", got, out)
	}
	// Newest row started at 14s and is running at 20s → 6.0s.
	if !strings.Contains(out, "6.0s") {
		t.Fatalf("newest row missing:\n%s", out)
	}
	// Rows 2-7 all show 1.0s; the dropped 8th row (row 1) is the only other
	// 1.0s row, so exactly 6 must remain.
	if got := strings.Count(out, "1.0s"); got != 6 {
		t.Fatalf("expected 6 one-second rows, got %d:\n%s", got, out)
	}
}

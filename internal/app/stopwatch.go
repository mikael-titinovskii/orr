package app

import (
	"fmt"
	"strings"
	"time"
)

// stopwatchRow is one completed or running stopwatch session. Metrics are
// accumulated from request records that completed while the row was running.
type stopwatchRow struct {
	id              int
	startedAt       time.Time
	stoppedAt       time.Time // zero while the row is still running
	cost            float64
	tokTotal        int // prompt + completion tokens
	completionTotal int // completion tokens alone, for the thinking share
	thinkTotal      int // reasoning (thinking) tokens within the completion tokens
}

// stopwatchState holds the in-memory stopwatch rows for the dashboard. Rows
// are not persisted: they live while the dashboard is open and are lost on
// quit.
type stopwatchState struct {
	rows      []stopwatchRow
	running   bool
	seenCount uint64 // session request count already accumulated
}

func newStopwatch() *stopwatchState {
	return &stopwatchState{}
}

// toggle stops the running row (freezing it) or starts a new one. snap is
// used to reset the accumulation cursor so a new row only counts requests
// that complete after it starts, and to catch up the final records before a
// row is frozen.
func (s *stopwatchState) toggle(now time.Time, snap statsSnapshot) {
	if s.running && len(s.rows) > 0 {
		s.accumulate(snap)
		s.rows[len(s.rows)-1].stoppedAt = now
		s.running = false
		return
	}
	s.rows = append(s.rows, stopwatchRow{id: len(s.rows) + 1, startedAt: now})
	s.running = true
	s.seenCount = snap.SessionRequestCount
}

// tick accumulates any request records completed since the last tick into the
// running row. It is a no-op while the stopwatch is stopped.
func (s *stopwatchState) tick(now time.Time, snap statsSnapshot) {
	if s.running {
		s.accumulate(snap)
	}
}

// accumulate folds the records completed since the last accumulation into the
// running row. The diff against the session request count bounds the scan to
// the records that actually arrived, so this stays cheap on every tick.
func (s *stopwatchState) accumulate(snap statsSnapshot) {
	if len(s.rows) == 0 {
		return
	}
	total := snap.SessionRequestCount
	if total <= s.seenCount {
		return
	}
	newCount := int(total - s.seenCount)
	records := snap.Records
	if newCount > len(records) {
		newCount = len(records)
	}
	row := &s.rows[len(s.rows)-1]
	for _, r := range records[len(records)-newCount:] {
		row.cost += r.Cost
		row.tokTotal += r.PromptTokens + r.CompletionTokens
		row.completionTotal += r.CompletionTokens
		row.thinkTotal += r.ReasoningTokens
	}
	s.seenCount = total
}

// stopwatchMaxRows is the most stopwatch rows the panel shows; older rows are
// dropped. History shows the same number of days, so both halves of the
// History panel stay balanced.
const stopwatchMaxRows = 7

// contentHeight returns the content height the panel needs to show its rows
// (capped at stopwatchMaxRows). The History panel sizes itself to match, so
// the column only grows as rows accumulate, up to the cap.
func (s *stopwatchState) contentHeight() int {
	lines := 4 // title, blank, hint, blank
	rows := min(len(s.rows), stopwatchMaxRows)
	return lines + 1 + rows // header + rows
}

// render returns the Stopwatch panel content, clipped to maxLines so the
// panel never grows taller than its allotted space: rows that do not fit at
// the bottom are dropped. Rows render newest-first, so the dropped rows are
// the oldest.
func (s *stopwatchState) render(width, maxLines int, now time.Time) string {
	lines := []string{
		titleStyle.Render("Stopwatch"),
		"",
		dimStyle.Render(shorten("s start/stop", width)),
		"",
		renderStopwatchHeader(width),
	}
	rowsShown := 0
	for i := len(s.rows) - 1; i >= 0 && rowsShown < stopwatchMaxRows; i-- {
		rowLine := renderStopwatchRow(s.rows[i], width, now)
		if len(lines)+1 > maxLines {
			break
		}
		lines = append(lines, rowLine)
		rowsShown++
	}
	return strings.Join(lines, "\n")
}

func renderStopwatchHeader(width int) string {
	return dimStyle.Render(shorten(fmt.Sprintf("%-6s %-8s %-4s %-6s", "Time", "Spent", "Tok", "Think%"), width))
}

func renderStopwatchRow(row stopwatchRow, width int, now time.Time) string {
	return shorten(fmt.Sprintf("%-6s %-8s %-4s %-6s",
		formatElapsed(rowElapsed(row, now)),
		fmt.Sprintf("$%.4f", row.cost),
		stopwatchTok(row),
		stopwatchThinkPercent(row),
	), width)
}

// rowElapsed is the wall-clock time the row ran: from start to stop, or to now
// while it is still running.
func rowElapsed(row stopwatchRow, now time.Time) time.Duration {
	end := row.stoppedAt
	if end.IsZero() {
		end = now
	}
	return end.Sub(row.startedAt)
}

// formatElapsed renders a duration compactly: 12.3s under a minute, then
// mmmss (1m02s, 12m05s) and hmm for longer runs.
func formatElapsed(d time.Duration) string {
	d = d.Round(100 * time.Millisecond)
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60
	if hours > 0 {
		return fmt.Sprintf("%dh%02dm", hours, mins)
	}
	return fmt.Sprintf("%dm%02ds", mins, secs)
}

func stopwatchTok(row stopwatchRow) string {
	if row.tokTotal == 0 {
		return ""
	}
	return tokenText(row.tokTotal)
}

// stopwatchThinkPercent is the share of the row's output tokens spent on
// reasoning (thinking), or "" when no reasoning tokens were reported. Some
// providers report reasoning tokens outside the completion count, so the share
// is clamped at 100%.
func stopwatchThinkPercent(row stopwatchRow) string {
	if row.completionTotal <= 0 || row.thinkTotal <= 0 {
		return ""
	}
	pct := float64(row.thinkTotal) / float64(row.completionTotal) * 100
	if pct > 100 {
		pct = 100
	}
	return fmt.Sprintf("%.0f%%", pct)
}

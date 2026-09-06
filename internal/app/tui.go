package app

import (
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type tickMsg time.Time
type serverErrorMsg struct{ err error }
type releaseUpdateTickMsg struct{}
type releaseUpdateResultMsg struct {
	version string
	err     error
}

// providerTestMsg carries the outcome of a 't' ping-pong test against one
// provider. err is set when the test could not be completed. model is the
// model the test ran against, so results stay scoped to one model.
type providerTestMsg struct {
	model    string
	provider string
	tps      float64
	ttft     time.Duration
	latency  time.Duration
	samples  int
	runs     int
	err      error
	testedAt time.Time
}

// testResult converts a displayed test outcome back into a ranking input, so
// the measurements the fresh test collected reach selection intact.
func (m providerTestMsg) testResult(provider string) providerTestResult {
	return providerTestResult{
		provider: provider,
		tps:      m.tps,
		ttft:     m.ttft,
		latency:  m.latency,
		samples:  m.samples,
		err:      m.err,
	}
}

type providerTestBatchMsg struct {
	model    string
	results  []providerTestResult
	testedAt time.Time
	revision uint64
}

type dashboardModel struct {
	stats            *stats
	routing          *routingState
	cfg              config
	viewport         viewport.Model
	width            int
	height           int
	selection        int
	model            string
	activeModels     []string
	autoScroll       bool
	serverErr        error
	spinnerFrame     int
	refresh          func(model string)
	testProvider     func(model, provider string) (providerTestSample, error)
	testAllProviders func(model string, providers []string) []providerTestResult
	testResults      map[string]providerTestMsg
	testing          map[string]bool
	allTesting       map[string]map[string]bool
	allTestRevisions map[string]uint64
	snap             statsSnapshot
	snapVersion      uint64
	haveSnap         bool
	stopwatch        *stopwatchState
	releaseUpdate    string
	checkRelease     func() (string, error)
}

var (
	panelStyle         = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("62")).Padding(0, 1)
	titleStyle         = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	dimStyle           = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	errorStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	watermarkStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("234"))
	usageQuarterStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("249"))
	releaseUpdateStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("136"))
	spinnerFrames      = []string{"─", "\\", "|", "/"}
	// selectionStyle lifts the selected provider row with a subtle background
	// so the '>' cursor's target line is easy to spot.
	selectionStyle = lipgloss.NewStyle().Background(lipgloss.Color("237"))
)

// watermarkArt is the orr logo, drawn faintly in the bottom-right corner of
// the log pane like a background watermark.
var watermarkArt = strings.Split(`   ____  ____  ____ 
  / __ \/ __ \/ __ \
 / / / / /_/ / /_/ /
/ /_/ / _, _/ _, _/ 
\____/_/ |_/_/ |_|`, "\n")

func watermarkWidth() int {
	width := 0
	for _, row := range watermarkArt {
		if w := lipgloss.Width(row); w > width {
			width = w
		}
	}
	return width
}

// watermarkBottomMargin is the empty rows left below the logo so it sits a
// little off the bottom border, matching the pane's ~1-cell right padding.
const watermarkBottomMargin = 1

// overlayWatermark draws the watermark in the bottom-right corner of the log
// pane content, behind the log text: cells already occupied by log text keep
// the text, and the faint logo shows through the empty cells. width is the
// pane's content width.
func overlayWatermark(content string, width int) string {
	rows := len(watermarkArt)
	lines := strings.Split(content, "\n")
	if len(lines) < rows+watermarkBottomMargin || width <= 0 {
		return content
	}
	left := max(0, width-watermarkWidth())
	start := len(lines) - rows - watermarkBottomMargin
	for i := 0; i < rows; i++ {
		lines[start+i] = overlayWatermarkRow(lines[start+i], watermarkArt[i], left, width)
	}
	return strings.Join(lines, "\n")
}

// overlayWatermarkRow lets the log line's text draw over the watermark row:
// the watermark glyphs appear only in cells past the end of the log text.
func overlayWatermarkRow(line, row string, left, width int) string {
	textWidth := ansi.StringWidth(strings.TrimRight(ansi.Strip(line), " "))
	start := max(textWidth, left)
	if start >= width {
		return line
	}
	var wm strings.Builder
	for c := start; c < width; c++ {
		glyph := ' '
		if c-left < len(row) {
			glyph = rune(row[c-left])
		}
		wm.WriteRune(glyph)
	}
	return ansi.Truncate(line, textWidth, "") + strings.Repeat(" ", start-textWidth) + watermarkStyle.Render(wm.String())
}

func newDashboard(cfg config, stats *stats, routing *routingState) dashboardModel {
	return dashboardModel{
		cfg: cfg, stats: stats, routing: routing,
		viewport:         viewport.New(0, 0),
		autoScroll:       true,
		testResults:      make(map[string]providerTestMsg),
		testing:          make(map[string]bool),
		allTesting:       make(map[string]map[string]bool),
		allTestRevisions: make(map[string]uint64),
		stopwatch:        newStopwatch(),
		checkRelease:     checkForReleaseUpdate,
	}
}

func (m dashboardModel) Init() tea.Cmd {
	return tea.Batch(tickDashboard(), checkReleaseUpdateCmd(m.checkRelease))
}

func tickDashboard() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(now time.Time) tea.Msg { return tickMsg(now) })
}

func checkReleaseUpdateCmd(check func() (string, error)) tea.Cmd {
	return func() tea.Msg {
		version, err := check()
		return releaseUpdateResultMsg{version: version, err: err}
	}
}

func scheduleReleaseUpdateCheck() tea.Cmd {
	return tea.Tick(releaseUpdateCheckInterval, func(time.Time) tea.Msg { return releaseUpdateTickMsg{} })
}

// refreshSnap recomputes the stats snapshot the dashboard renders from. It is
// called once per tick when new data arrived, so a frame only pays for one
// snapshot instead of one per panel.
func (m *dashboardModel) refreshSnap() {
	m.snapVersion = m.stats.mutationsCount()
	m.snap = m.stats.snapshot()
	m.haveSnap = true
}

// currentSnap returns the cached snapshot, computing it on first use (for
// example the initial frame before the first tick).
func (m *dashboardModel) currentSnap() statsSnapshot {
	if !m.haveSnap {
		m.refreshSnap()
	}
	return m.snap
}

func (m dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.refreshLog()
	case tickMsg:
		m.spinnerFrame = (m.spinnerFrame + 1) % len(spinnerFrames)
		m.syncModel()
		m.refreshLog()
		m.stopwatch.tick(m.routing.now(), m.currentSnap())
		return m, tickDashboard()
	case serverErrorMsg:
		m.serverErr = msg.err
		return m, tea.Quit
	case releaseUpdateTickMsg:
		return m, checkReleaseUpdateCmd(m.checkRelease)
	case releaseUpdateResultMsg:
		if msg.err == nil {
			m.releaseUpdate = msg.version
		}
		return m, scheduleReleaseUpdateCheck()
	case providerTestMsg:
		key := testResultKey(msg.model, msg.provider)
		delete(m.testing, key)
		m.testResults[key] = msg
		if pending := m.allTesting[msg.model]; pending != nil {
			delete(pending, msg.provider)
			if len(pending) == 0 {
				delete(m.allTesting, msg.model)
				revision := m.allTestRevisions[msg.model]
				delete(m.allTestRevisions, msg.model)
				results := make([]providerTestResult, 0)
				for _, provider := range m.routing.providers(msg.model, m.currentSnap().Observed[msg.model]) {
					if result, ok := m.testResults[testResultKey(msg.model, provider)]; ok {
						results = append(results, result.testResult(provider))
					}
				}
				snap := m.currentSnap()
				providers := m.routing.providers(msg.model, snap.Observed[msg.model])
				now := m.routing.now()
				results = mergeMeasuredProviderResults(results, m.stats.benchmarkPool(msg.model), providers, now)
				profile := newRequestProfile(snap.MetricsRecords, msg.model, now)
				apiErrs := providerAPIErrorRates(snap.Records, msg.model, now)
				best, autoUpdated, err := m.routing.applyProviderTestResultsAtRevision(msg.model, results, profile, revision, apiErrs)
				if err != nil {
					m.serverErr = err
					return m, tea.Quit
				}
				if best != "" {
					// Keep the measured automatic winner while a manual pin is
					// overriding it, so clearing the manual pin can reveal it.
					m.stats.setAutoPin(msg.model, best)
				} else if autoUpdated {
					m.stats.clearAutoPin(msg.model)
					fallback := m.routing.active(msg.model, snap.Observed[msg.model])
					if m.routing.ensureAutoPin(msg.model, fallback) {
						m.stats.setAutoPin(msg.model, fallback)
					}
				}
				if best != "" || autoUpdated {
					syncPersistedAutomaticPin(m.routing, m.stats, msg.model)
				}
				if msg.model == m.model {
					m.selectActiveProvider(m.currentSnap())
				}
			}
		}
	case providerTestBatchMsg:
		for _, provider := range m.routing.providers(msg.model, m.currentSnap().Observed[msg.model]) {
			delete(m.testing, testResultKey(msg.model, provider))
		}
		delete(m.allTesting, msg.model)
		delete(m.allTestRevisions, msg.model)
		snap := m.currentSnap()
		providers := m.routing.providers(msg.model, snap.Observed[msg.model])
		// Only freshly measured results go into the display cache. The merged
		// list summarizes the benchmark pool for ranking, and pooled entries
		// carry no latency: storing them here blanked mLat behind a fresh
		// timestamp and re-armed the freshness window of providers this run
		// never tested.
		for _, result := range msg.results {
			m.testResults[testResultKey(msg.model, result.provider)] = providerTestMsg{
				model: msg.model, provider: result.provider, tps: result.tps,
				ttft: result.ttft, latency: result.latency, samples: result.samples,
				runs: providerTestRounds, err: result.err,
				testedAt: msg.testedAt,
			}
		}
		msg.results = mergeMeasuredProviderResults(msg.results, m.stats.benchmarkPool(msg.model), providers, msg.testedAt)
		profile := newRequestProfile(snap.MetricsRecords, msg.model, msg.testedAt)
		apiErrs := providerAPIErrorRates(m.currentSnap().Records, msg.model, msg.testedAt)
		best, autoUpdated, err := m.routing.applyProviderTestResultsAtRevision(msg.model, msg.results, profile, msg.revision, apiErrs)
		if err != nil {
			m.serverErr = err
			return m, tea.Quit
		}
		if best != "" {
			m.stats.setAutoPin(msg.model, best)
		} else if autoUpdated {
			m.stats.clearAutoPin(msg.model)
			fallback := m.routing.active(msg.model, snap.Observed[msg.model])
			if m.routing.ensureAutoPin(msg.model, fallback) {
				m.stats.setAutoPin(msg.model, fallback)
			}
		}
		if best != "" || autoUpdated {
			syncPersistedAutomaticPin(m.routing, m.stats, msg.model)
		}
		if msg.model == m.model {
			m.selectActiveProvider(m.currentSnap())
		}
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up":
			providers := m.providers()
			if len(providers) > 0 {
				m.selection = (m.selection - 1 + len(providers)) % len(providers)
			}
		case "down":
			providers := m.providers()
			if len(providers) > 0 {
				m.selection = (m.selection + 1) % len(providers)
			}
		case "tab", "shift+tab":
			if len(m.activeModels) > 1 {
				idx := indexOfString(m.activeModels, m.model)
				if idx < 0 {
					idx = 0
				}
				delta := 1
				if msg.String() == "shift+tab" {
					delta = -1
				}
				m.model = m.activeModels[(idx+delta+len(m.activeModels))%len(m.activeModels)]
				m.selectActiveProvider(m.stats.snapshot())
			}
		case "enter":
			providers := m.providers()
			if m.selection >= 0 && m.selection < len(providers) {
				provider := providers[m.selection]
				// Compare against the displayed pin, which is also the pin
				// requests use: a blocked provider is still pinnable, so it
				// still has to be togglable off.
				currentPin, currentManual := m.routing.pinInfo(m.model)
				if currentPin == provider {
					if err := m.routing.setManualPin(m.model, ""); err != nil {
						m.serverErr = err
						return m, tea.Quit
					}
					if currentManual {
						// A manual pin is an override. Reveal the measured automatic
						// winner that was active underneath it, preserving its TTL.
						m.restoreAutomaticPin(m.model)
					} else {
						// Enter on the automatic pin explicitly clears automatic
						// routing and returns to the configured fallback order.
						m.stats.clearAutoPin(m.model)
					}
					m.selectActiveProvider(m.currentSnap())
				} else {
					if err := m.routing.setManualPin(m.model, provider); err != nil {
						return m, nil
					}
				}
			}
		case "r":
			if m.refresh != nil && m.model != "" {
				// Re-reading the catalog is when a user who just changed a
				// guardrail expects orr to find out, so forget the recorded
				// refusals and let the next benchmark re-probe them.
				if err := m.routing.clearAllBlocked(m.model); err != nil {
					log.Printf("clear blocked providers for %s: %v", m.model, err)
				}
				m.refresh(m.model)
			}
		case "t":
			providers := m.providers()
			if m.model == "" || len(providers) == 0 || m.selection < 0 || m.selection >= len(providers) || m.testProvider == nil {
				break
			}
			provider := providers[m.selection]
			key := testResultKey(m.model, provider)
			if m.testing[key] {
				break
			}
			m.testing[key] = true
			return m, m.testCmd(provider)
		case "a":
			revision := m.routing.modelRevision(m.model)
			providers := m.providers()
			if m.model == "" || len(providers) == 0 || (m.testProvider == nil && m.testAllProviders == nil) || len(m.allTesting[m.model]) > 0 {
				break
			}
			pending := make(map[string]bool, len(providers))
			alreadyTesting := make(map[string]bool, len(providers))
			for _, provider := range providers {
				pending[provider] = true
				key := testResultKey(m.model, provider)
				alreadyTesting[provider] = m.testing[key]
				m.testing[key] = true
			}
			m.allTesting[m.model] = pending
			m.allTestRevisions[m.model] = revision
			if m.testAllProviders != nil {
				model := m.model
				revision := m.allTestRevisions[model]
				allProviders := append([]string(nil), providers...)
				testAll := m.testAllProviders
				return m, func() tea.Msg {
					return providerTestBatchMsg{model: model, results: testAll(model, allProviders), testedAt: m.routing.now(), revision: revision}
				}
			}
			cmds := make([]tea.Cmd, 0, len(providers))
			for _, provider := range providers {
				if alreadyTesting[provider] {
					continue
				}
				cmds = append(cmds, m.testCmd(provider))
			}
			if len(cmds) > 0 {
				return m, tea.Batch(cmds...)
			}
		case "s":
			m.stopwatch.toggle(m.routing.now(), m.currentSnap())
		case "pgup":
			m.autoScroll = false
			m.viewport.ViewUp()
		case "pgdown":
			m.viewport.ViewDown()
			m.autoScroll = m.viewport.AtBottom()
		}
	}
	return m, nil
}

func (m *dashboardModel) resize() {
	m.viewport.Width = max(1, m.width-4)
	// -4: the title, blank line, and two border rows; the table header adds its own height.
	m.viewport.Height = max(1, m.height-m.topRowHeight()-4-logTableHeaderHeight(m.viewport.Width))
}

// stopwatchContentWidth returns the content width of the Stopwatch half of
// the History panel for a terminal of the given width.
func stopwatchContentWidth(termWidth int) int {
	columnWidth, _ := dashboardPanelWidths(termWidth)
	historyHalf := max(1, (columnWidth-2)/2)
	stopwatchHalf := max(1, columnWidth-2-historyHalf)
	return max(1, stopwatchHalf-2)
}

// historyContentWidth returns the content width of the History half of the
// History/Stopwatch panel for a terminal of the given width.
func historyContentWidth(termWidth int) int {
	columnWidth, _ := dashboardPanelWidths(termWidth)
	historyHalf := max(1, (columnWidth-2)/2)
	return max(1, historyHalf-2)
}

// dashboardPanelWidths returns the widths passed to lipgloss.Style.Width for
// the two top-row columns. A bordered panel renders two cells wider than this
// value. Routing absorbs the space freed by the old Performance panel so its
// provider table can keep all comparison metrics together.
func dashboardPanelWidths(termWidth int) (expenses, routing int) {
	outerThird := max(3, termWidth/3)
	expenses = max(1, outerThird-2)
	routing = max(1, termWidth-(expenses+2)-2)
	return expenses, routing
}

// expensesTopFloor is the smallest Expenses panel content height: the four
// usage lines, the table header, and two table rows.
const expensesTopFloor = 7

// expensesHeights returns the content heights of the two stacked expenses
// panels. resize and View must agree on these so the viewport never
// overflows the log pane (an overflowing frame scrolls off screen).
func (m dashboardModel) expensesHeights() (top, bottom int) {
	now := m.routing.now()
	snap := m.currentSnap()
	innerHeight := max(1, max(10, m.height/2)-2)
	// The bottom panel must fit the stopwatch's rows (capped at 7); History
	// fills the same height, capped at 7 days.
	bottomMin := max(lipgloss.Height(renderHistoryPerDay(snap, now, historyContentWidth(m.width))), m.stopwatch.contentHeight())
	// The Expenses panel is capped at its 2/3 share of the column so a long
	// model list cannot push the History/Stopwatch row off screen.
	top = max(expensesTopFloor, (innerHeight*2)/3)
	bottom = max(bottomMin, innerHeight-top)
	return top, bottom
}

// topRowHeight is the rendered height (borders included) of the panels above
// the log pane. Routing is explicitly sized to match the expenses column.
func (m dashboardModel) topRowHeight() int {
	top, bottom := m.expensesHeights()
	return top + bottom + 8
}

// activeModelWindow is how long a model with no completed requests stays in
// the Routing panel's model list.
const activeModelWindow = 10 * time.Minute

// activeModels returns models with a completed request within the window,
// ordered by most recent activity first.
func activeModels(snap statsSnapshot, now time.Time) []string {
	seen := make(map[string]time.Time)
	for _, record := range snap.Records {
		if record.Model == "" || record.Time.Before(now.Add(-activeModelWindow)) {
			continue
		}
		if last, ok := seen[record.Model]; !ok || record.Time.After(last) {
			seen[record.Model] = record.Time
		}
	}
	models := make([]string, 0, len(seen))
	for model := range seen {
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool { return seen[models[i]].After(seen[models[j]]) })
	return models
}

// expireTestResults drops explicit ping-pong results that a real proxied
// request for the same (model, provider) has since superseded. Without this,
// pressing `t` or `a` would pin the measured column to its value for the
// full 30-minute TTL and ignore live P50 from incoming requests.
func (m *dashboardModel) expireTestResults(snap statsSnapshot) {
	if len(m.testResults) == 0 {
		return
	}
	latest := make(map[string]time.Time, len(m.testResults))
	for _, record := range snap.MetricsRecords {
		key := testResultKey(record.Model, record.Provider)
		if last, ok := latest[key]; !ok || record.Time.After(last) {
			latest[key] = record.Time
		}
	}
	for key, result := range m.testResults {
		if t, ok := latest[key]; ok && t.After(result.testedAt) {
			delete(m.testResults, key)
		}
	}
}

func (m *dashboardModel) syncModel() {
	if !m.haveSnap || m.stats.mutationsCount() != m.snapVersion {
		m.refreshSnap()
	}
	snap := m.currentSnap()
	m.expireTestResults(snap)
	m.activeModels = activeModels(snap, m.routing.now())
	// Keep the displayed model once chosen: concurrent requests for other
	// models must not yank the Routing panel away from it.
	if m.model != "" && containsString(m.activeModels, m.model) {
		return
	}
	next := m.model
	switch {
	case len(m.activeModels) > 0:
		next = m.activeModels[0]
	case m.routing.current() != "":
		next = m.routing.current()
	default:
		next = ""
	}
	// Re-anchor the selection only when the displayed model actually changes.
	// A stale model that stays on screen must not reset the user's navigation
	// on every tick.
	if next != m.model {
		m.model = next
		m.selectActiveProvider(snap)
	}
}

// selectActiveProvider moves the selection to the model's active provider.
func (m *dashboardModel) selectActiveProvider(snap statsSnapshot) {
	m.selection = 0
	active := m.routing.active(m.model, snap.Observed[m.model])
	for i, provider := range m.routing.providers(m.model, snap.Observed[m.model]) {
		if provider == active {
			m.selection = i
			break
		}
	}
}

// restoreAutomaticPin reveals the automatic route hidden by a manual
// override. Prefer its preserved measured winner; if none is available, pin
// the first routable provider in the current ranked order. Removing a manual
// override must never leave automatic routing active but visually unpinned.
func (m *dashboardModel) restoreAutomaticPin(model string) {
	if pin, ok := m.stats.providerPinsSnapshot()[model]; ok {
		m.routing.restorePins(map[string]persistedPin{model: pin})
		if provider, manual := m.routing.pinInfo(model); provider != "" && !manual {
			return
		}
		m.stats.clearAutoPin(model)
		// Do not let an unresolved saved pin replace the fallback selected below
		// if endpoint discovery completes later.
		m.routing.unpin(model)
	}

	cfg, ok := m.routing.modelConfig(model)
	if !ok {
		return
	}
	providers := cfg.Order
	if len(providers) == 0 {
		providers = cfg.Only
	}
	pool := m.stats.benchmarkPool(model)
	now := m.routing.now()
	cutoff := now.Add(-m.routing.pinTTL)
	blocked := m.routing.blockedProviders(model)
	apiErrors := providerAPIErrorRates(m.currentSnap().Records, model, now)
	for _, provider := range providers {
		if _, refused := blocked[provider]; refused || apiErrors[provider] > 0 {
			continue
		}
		var measuredAt time.Time
		for _, sample := range pool[provider] {
			if sample.TPS > 0 && !sample.Time.Before(cutoff) && sample.Time.After(measuredAt) {
				measuredAt = sample.Time
			}
		}
		if measuredAt.IsZero() {
			continue
		}
		pin := persistedPin{Provider: provider, PinnedAt: measuredAt}
		m.stats.setAutoPinAt(model, provider, measuredAt)
		m.routing.restorePins(map[string]persistedPin{model: pin})
		return
	}

	provider := m.routing.active(model, m.currentSnap().Observed[model])
	if m.routing.ensureAutoPin(model, provider) {
		m.stats.setAutoPin(model, provider)
	}
}

func indexOfString(values []string, target string) int {
	for i, value := range values {
		if value == target {
			return i
		}
	}
	return -1
}

func (m dashboardModel) providers() []string {
	return m.routing.providers(m.model, m.currentSnap().Observed[m.model])
}

func (m *dashboardModel) refreshLog() {
	m.resize()
	snap := m.currentSnap()
	m.viewport.SetContent(renderLogRows(snap.Records, m.viewport.Width))
	if m.autoScroll {
		m.viewport.GotoBottom()
	}
}

const logTableMinWidth = 144

// renderLogRows renders only the scrollable body. The fixed header is rendered
// beside the viewport so PgUp/PgDn never moves it out of view.
func renderLogRows(records []requestRecord, width int) string {
	tableWidth := logTableWidth(width)
	if tableWidth < logTableMinWidth {
		return renderCompactLogRows(records, width)
	}
	modelWidth, providerWidth, _, _, _, _, _, _, _, _, _, _, _, _ := logTableWidths(tableWidth)
	lines := make([]string, 0, len(records))
	for _, record := range records {
		cells := []string{
			record.Time.Local().Format("15:04:05"),
			shorten(record.Model, modelWidth),
			shorten(record.Provider, providerWidth),
			logReasoningText(record),
			fmt.Sprintf("%d", record.Status),
			logDurationText(record.Duration),
			logTokenValue(record.PromptTokens),
			logUncachedText(record),
			logTokenValue(record.CompletionTokens),
			logThinkPercentText(record),
			fmt.Sprintf("%.0f", record.tokensPerSecond()),
			fmt.Sprintf("$%.4f", record.Cost),
			logCacheReadText(record),
			logCacheWriteText(record),
			logCachePercentText(record),
		}
		line := renderLogTableRow(cells, logTableColumns(tableWidth))
		if record.apiError() {
			line = errorStyle.Render(line)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func logTableWidth(width int) int {
	return max(1, width)
}

func logTableColumns(width int) []int {
	modelWidth, providerWidth, reasonWidth, statusWidth, durationWidth, tokenInWidth, tokenInUncachedWidth, tokenOutWidth, thinkPercentWidth, speedWidth, costWidth, cacheReadWidth, cacheWriteWidth, cachePercentWidth := logTableWidths(width)
	return []int{12, modelWidth, providerWidth, reasonWidth, statusWidth, durationWidth, tokenInWidth, tokenInUncachedWidth, tokenOutWidth, thinkPercentWidth, speedWidth, costWidth, cacheReadWidth, cacheWriteWidth, cachePercentWidth}
}

func logTableWidths(width int) (model, provider, reason, status, duration, tokenIn, tokenInUncached, tokenOut, thinkPercent, speed, cost, cacheRead, cacheWrite, cachePercent int) {
	const (
		fixed           = 12 + 6 + 4 + 8 + 9 + 11 + 10 + 7 + 6 + 8 + 10 + 11 + 7 + 14 // fixed columns plus separators
		minimumModel    = 9
		minimumProvider = 10
		maximumModel    = 36
		maximumProvider = 18
	)
	variable := max(0, width-fixed-minimumModel-minimumProvider)
	model = min(maximumModel, minimumModel+variable/2)
	provider = min(maximumProvider, minimumProvider+variable-variable/2)
	return model, provider, 6, 4, 8, 9, 11, 10, 7, 6, 8, 10, 11, 7
}

func logTableHeaderHeight(width int) int {
	if logTableWidth(width) < logTableMinWidth {
		return 2
	}
	return 1
}

func renderLogTableHeader(width int) string {
	tableWidth := logTableWidth(width)
	if tableWidth < logTableMinWidth {
		return dimStyle.Render(strings.Join([]string{
			shorten("Time  Code  Duration  Cost", width),
			shorten("Model / Provider  Reason  Tokens in  Tokens in u  Tokens out  Think%  Tok/s  Cache read  Cache write  Cache%", width),
		}, "\n"))
	}
	header := renderLogTableRow([]string{"Time", "Model", "Provider", "Reason", "Code", "Duration", "Tokens in", "Tokens in u", "Tokens out", "Think%", "Tok/s", "Cost", "Cache read", "Cache write", "Cache%"}, logTableColumns(tableWidth))
	return dimStyle.Render(header)
}

func renderLogTableRow(cells []string, widths []int) string {
	parts := make([]string, len(cells))
	for i, cell := range cells {
		parts[i] = padRight(shorten(cell, widths[i]), widths[i])
	}
	return strings.Join(parts, " ")
}

func renderCompactLogRows(records []requestRecord, width int) string {
	lines := make([]string, 0, len(records)*2)
	for _, record := range records {
		first := shorten(fmt.Sprintf("%s  %d  %s  $%.4f", record.Time.Local().Format("15:04:05"), record.Status, logDurationText(record.Duration), record.Cost), width)
		second := shorten(fmt.Sprintf("%s / %s  %s  %s  %s  %s  %.0f  %s  %s  %s  %s", record.Model, record.Provider, logReasoningText(record), logTokenValue(record.PromptTokens), logUncachedText(record), logTokenValue(record.CompletionTokens), record.tokensPerSecond(), logThinkPercentText(record), logCacheReadText(record), logCacheWriteText(record), logCachePercentText(record)), width)
		if record.apiError() {
			first = errorStyle.Render(first)
			second = errorStyle.Render(second)
		}
		lines = append(lines, first, second)
	}
	return strings.Join(lines, "\n")
}

func logTokenValue(tokens int) string {
	if tokens <= 0 {
		return ""
	}
	return strconv.Itoa(tokens)
}

// logUncachedText renders the input tokens not served from cache (Tokens in
// minus Cache read). A blank means nothing was billed as uncached input.
func logUncachedText(record requestRecord) string {
	uncached := record.PromptTokens - record.CachedTokens
	if uncached <= 0 {
		return ""
	}
	return strconv.Itoa(uncached)
}

// logReasoningText renders the reasoning effort level requested for the call
// (e.g. "low", "medium", "high", "xhigh"). A blank means none was requested.
func logReasoningText(record requestRecord) string {
	if record.ReasoningEffort == "" {
		return ""
	}
	return record.ReasoningEffort
}

func logCacheReadText(record requestRecord) string {
	if record.CachedTokens <= 0 {
		return ""
	}
	return strconv.Itoa(record.CachedTokens)
}

func logCacheWriteText(record requestRecord) string {
	if record.CacheWriteTokens <= 0 {
		return ""
	}
	return strconv.Itoa(record.CacheWriteTokens)
}

func logCachePercentText(record requestRecord) string {
	if record.PromptTokens <= 0 {
		return ""
	}
	return cachePercentText(record.cachePercent())
}

// cachePercentText suppresses cache-hit values that display as zero.
func cachePercentText(percent float64) string {
	text := fmt.Sprintf("%.0f%%", percent)
	if text == "0%" {
		return ""
	}
	return text
}

// logThinkPercentText renders the share of output tokens spent on reasoning
// (thinking). A blank means the provider did not report reasoning tokens.
func logThinkPercentText(record requestRecord) string {
	if record.CompletionTokens <= 0 || record.ReasoningTokens <= 0 {
		return ""
	}
	return fmt.Sprintf("%.0f%%", record.thinkingPercent())
}

func logDurationText(duration time.Duration) string {
	if duration == 0 {
		return ""
	}
	return formatElapsed(duration)
}

func (m dashboardModel) View() string {
	if m.width == 0 || m.height == 0 {
		return "starting orr…"
	}
	snap := m.currentSnap()
	now := m.routing.now()
	expensesWidth, routingWidth := dashboardPanelWidths(m.width)
	routingContentWidth := max(1, routingWidth-2)

	expensesTopHeight, expensesBottomHeight := m.expensesHeights()
	// Match the left column's rendered height (top+bottom+8 including
	// borders) so all three top panels end at the same row.
	expensesColumnHeight := expensesTopHeight + expensesBottomHeight + 6

	expensesContent := renderExpenses(snap, now, max(1, expensesWidth-2), expensesTopHeight)

	expensesTop := panelStyle.Width(expensesWidth).Height(expensesTopHeight + 2).Render(expensesContent)

	// The History panel is split 50/50: History keeps the left half, the
	// Stopwatch panel fills the right half. panelStyle renders Width()+2
	// cells, so the two halves together must match the column width; the
	// extra cell from an odd column width goes to the Stopwatch side.
	historyHalfWidth := max(1, (expensesWidth-2)/2)
	stopwatchHalfWidth := max(1, expensesWidth-2-historyHalfWidth)
	historyPane := panelStyle.Width(historyHalfWidth).Height(expensesBottomHeight + 2).Render(renderHistoryPerDay(snap, now, historyContentWidth(m.width)))
	stopwatchPane := panelStyle.Width(stopwatchHalfWidth).Height(expensesBottomHeight + 2).Render(m.stopwatch.render(stopwatchContentWidth(m.width), expensesBottomHeight, now))
	expensesBottom := lipgloss.JoinHorizontal(lipgloss.Top, historyPane, stopwatchPane)
	expenses := lipgloss.JoinVertical(lipgloss.Left, expensesTop, expensesBottom)

	routing := panelStyle.Width(routingWidth).Height(expensesColumnHeight).Render(m.renderRouting(snap, routingContentWidth, max(2, expensesColumnHeight-2)))

	top := lipgloss.JoinHorizontal(lipgloss.Top, expenses, routing)
	logTitle := titleStyle.Render("Log")
	if snap.InFlight > 0 {
		logTitle += " " + spinnerFrames[m.spinnerFrame]
	} else {
		logTitle += " " + spinnerFrames[0]
	}
	logTitle += dimStyle.Render("  PgUp/PgDn scroll • q quit")
	if m.releaseUpdate != "" {
		logTitle += releaseUpdateStyle.Render(" • " + m.releaseUpdate + " available run ./orr upgrade")
	}
	logWidth := max(1, m.width-4)
	logView := overlayWatermark(m.viewport.View(), logWidth)
	logBody := renderLogTableHeader(logWidth) + "\n" + logView
	logPane := panelStyle.Width(max(1, m.width-2)).Height(max(1, m.height-lipgloss.Height(top)-2)).Render(logTitle + "\n\n" + logBody)
	return lipgloss.JoinVertical(lipgloss.Left, top, logPane)
}

// renderExpenses renders the Expenses panel capped at maxHeight lines: the
// four usage lines are always kept and the Recent spend table gets the
// remainder, so a long model list cannot grow the panel beyond its share.
func renderExpenses(snap statsSnapshot, now time.Time, width, maxHeight int) string {
	month := displayMonthSpend(snap, now)
	used := 0.0
	if snap.Budget > 0 {
		used = month / snap.Budget * 100
	}
	usedLine := "Used"
	if snap.Budget > 0 {
		usedLabel := fmt.Sprintf("Used $%.0f / $%.0f ", month, snap.Budget)
		barWidth := max(0, width-lipgloss.Width(usedLabel))
		usedLine = usedLabel + renderUsageBar(used, barWidth)
	} else if month > 0 {
		usedLine = fmt.Sprintf("Used $%.0f", month)
	}
	lines := []string{
		titleStyle.Render("Expenses"),
		"",
		shorten(usedLine, width),
		"",
	}
	// The four usage lines always stay; the table gets the remainder.
	return strings.Join(lines, "\n") + "\n" + renderRecentExpenses(snap, width, now, max(1, maxHeight-4))
}

func renderUsageBar(used float64, width int) string {
	filled := min(width, max(0, int(used/100*float64(width)+.5)))
	bar := []rune(strings.Repeat("█", filled) + strings.Repeat("░", width-filled))
	var rendered strings.Builder
	for quarter := 0; quarter < 4; quarter++ {
		start := width * quarter / 4
		end := width * (quarter + 1) / 4
		if start == end {
			continue
		}
		// Keep the usage fill at full brightness. Quarter shading is only a
		// background guide for the unfilled portion of the bar.
		unfilledStart := min(end, max(start, filled))
		rendered.WriteString(string(bar[start:unfilledStart]))
		segment := string(bar[unfilledStart:end])
		if quarter%2 == 0 && segment != "" {
			segment = usageQuarterStyle.Render(segment)
		}
		rendered.WriteString(segment)
	}
	return rendered.String()
}

// renderHistoryPerDay renders the per-day cost history, clipped to width so
// the panel never wraps (a wrapped line would render taller than the height
// computed by expensesHeights and push the log pane off screen).
func renderHistoryPerDay(snap statsSnapshot, now time.Time, width int) string {
	currentMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	month := monthCostText(snap.DailyCosts, currentMonth)
	monthTokens := monthTokenTotal(snap.DailyTokens, currentMonth)
	previousMonth := currentMonth.AddDate(0, -1, 0)
	lastMonth := monthCostText(snap.DailyCosts, previousMonth)
	lastMonthTokens := monthTokenTotal(snap.DailyTokens, previousMonth)
	days := make([]string, 0, len(snap.DailyCosts))
	for day := range snap.DailyCosts {
		days = append(days, day)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	lines := []string{
		titleStyle.Render("History"),
		"",
		shorten(fmt.Sprintf("This m  %s  %s", month, formatDailyTokens(monthTokens)), width),
		shorten(fmt.Sprintf("Last m  %s  %s", lastMonth, formatDailyTokens(lastMonthTokens)), width),
		"",
	}
	for _, day := range days[:min(7, len(days))] {
		parsed, _ := time.ParseInLocation(time.DateOnly, day, now.Location())
		lines = append(lines, shorten(fmt.Sprintf("%-8s $%.4f  %s", parsed.Format("Jan 02"), snap.DailyCosts[day], formatDailyTokens(snap.DailyTokens[day])), width))
	}
	return strings.Join(lines, "\n")
}

func monthCostText(daily map[string]float64, month time.Time) string {
	prefix := month.Local().Format("2006-01-")
	var total float64
	found := false
	for day, cost := range daily {
		if strings.HasPrefix(day, prefix) {
			total += cost
			found = true
		}
	}
	if !found {
		return ""
	}
	return fmt.Sprintf("$%.4f", total)
}

func monthTokenTotal(daily map[string]int64, month time.Time) int64 {
	prefix := month.Local().Format("2006-01-")
	var total int64
	for day, tokens := range daily {
		if strings.HasPrefix(day, prefix) {
			total += tokens
		}
	}
	return total
}

func formatDailyTokens(tokens int64) string {
	if tokens <= 0 {
		return ""
	}
	return fmt.Sprintf("%.2fm", float64(tokens)/1_000_000)
}

func (m dashboardModel) renderRouting(snap statsSnapshot, width, height int) string {
	pinned, pinnedManual := m.routing.pinInfo(m.model)
	blocked := m.routing.blockedProviders(m.model)
	pinSuffix := ""
	if pinned != "" {
		if pinnedManual {
			pinSuffix = " (manual)"
		} else {
			pinSuffix = fmt.Sprintf(" (auto %s t/o)", formatPinTTL(m.routing.pinTTL))
		}
	}
	// A pinned provider the account cannot route to is still routed to; the
	// mark says the requests will come back refused, not that orr went
	// somewhere else.
	pinBroken := false
	if entry, ok := blocked[pinned]; ok {
		pinBroken = true
		pinSuffix += " ⊘ " + shortBlockReason(entry.Reason)
	}
	modelLine := "Model  " + m.model
	if len(m.activeModels) > 1 {
		idx := indexOfString(m.activeModels, m.model)
		if idx < 0 {
			idx = 0
		}
		modelLine += fmt.Sprintf("  [%d/%d Tab]", idx+1, len(m.activeModels))
	}
	hint := "↑/↓ select • Enter toggle • r refresh • t test • a all"
	if len(m.activeModels) > 1 {
		hint = "↑/↓ select • Enter toggle • Tab switch • r refresh • t test • a all"
	}
	pinLine := "Pin    " + shorten(pinned+pinSuffix, max(1, width-7))
	if pinBroken {
		pinLine = errorStyle.Render(pinLine)
	}
	lines := []string{
		titleStyle.Render("Routing"),
		"",
		dimStyle.Render(shorten(hint, width)),
		dimStyle.Render(shorten("measured tok/s and lat - ttl 30m", width)),
		dimStyle.Render(shorten("API P50: last 30m • website P50: 1 week", width)),
		"",
		shorten(modelLine, max(7, width)),
		pinLine,
		"",
	}
	tableHeight := max(1, height-len(lines)-2)
	lines = append(lines, m.renderProviderTable(snap, width, tableHeight))
	content := strings.Join(lines, "\n")
	if lipgloss.Height(content) > height {
		content = shortenLines(content, height)
	}
	return content
}

// testResultKey scopes test state to a single (model, provider) pair, so
// results measured under one model never leak into another model's table.
func testResultKey(model, provider string) string {
	return model + "\x00" + provider
}

// testCmd launches one ping-pong test for provider and returns the command
// that delivers its result. Tests run concurrently, one per provider.
func (m dashboardModel) testCmd(provider string) tea.Cmd {
	return func() tea.Msg {
		sample, err := m.testProvider(m.model, provider)
		return providerTestMsg{
			model: m.model, provider: provider, tps: sample.tps, ttft: sample.ttft,
			latency: sample.latency, samples: sample.samples, runs: providerTestRounds,
			err: err, testedAt: m.routing.now(),
		}
	}
}

// testResultFor returns the fresh 't' ping-pong result for provider, if any.
// Results expire on the same 30-minute window as the measured P50 metrics, so
// the two stay consistent.
func (m dashboardModel) testResultFor(model, provider string) (providerTestMsg, bool) {
	r, ok := m.testResults[testResultKey(model, provider)]
	if !ok || m.routing.now().Sub(r.testedAt) > measuredMetricsTTL {
		return providerTestMsg{}, false
	}
	return r, true
}

// noScore marks a metric with no data for a provider; such cells render unstyled.
const noScore = -1

type measuredMetrics struct {
	throughput float64
	latency    float64
}

type providerScores struct {
	prompt         float64
	completion     float64
	cacheRead      float64
	throughput     float64
	latency        float64
	measThroughput float64
	measLatency    float64
}

func computeScores(providers []string, endpoints map[string]endpointMeta, measured map[string]measuredMetrics) map[string]providerScores {
	result := make(map[string]providerScores, len(providers))
	prompts := make(map[string]float64, len(providers))
	completions := make(map[string]float64, len(providers))
	cacheReads := make(map[string]float64, len(providers))
	throughputs := make(map[string]float64, len(providers)*2)
	latencies := make(map[string]float64, len(providers)*2)
	for _, provider := range providers {
		if ep, ok := endpoints[provider]; ok {
			if ep.Pricing.Prompt > 0 {
				prompts[provider] = ep.Pricing.Prompt.float()
			}
			if ep.Pricing.Completion > 0 {
				completions[provider] = ep.Pricing.Completion.float()
			}
			if ep.Pricing.InputCacheRead > 0 {
				cacheReads[provider] = ep.Pricing.InputCacheRead.float()
			}
			if ep.Throughput > 0 {
				throughputs[provider+"_catalog"] = ep.Throughput
			}
			if ep.Latency > 0 {
				latencies[provider+"_catalog"] = ep.Latency
			}
		}
		if m, ok := measured[provider]; ok {
			if m.throughput > 0 {
				throughputs[provider+"_meas"] = m.throughput
			}
			if m.latency > 0 {
				latencies[provider+"_meas"] = m.latency
			}
		}
	}
	// Use 10th/90th percentile bounds so a single outlier doesn't flatten
	// the gradient; values beyond the bounds clamp to red/green.
	promptLow, promptHigh := robustBounds(prompts)
	completionLow, completionHigh := robustBounds(completions)
	cacheLow, cacheHigh := robustBounds(cacheReads)
	throughputLow, throughputHigh := robustBounds(throughputs)
	latencyLow, latencyHigh := robustBounds(latencies)
	for _, provider := range providers {
		scores := providerScores{
			prompt:         noScore,
			completion:     noScore,
			cacheRead:      noScore,
			throughput:     noScore,
			latency:        noScore,
			measThroughput: noScore,
			measLatency:    noScore,
		}
		if _, ok := endpoints[provider]; ok {
			if v, ok := prompts[provider]; ok {
				scores.prompt = scoreFor(v, promptLow, promptHigh)
			}
			if v, ok := completions[provider]; ok {
				scores.completion = scoreFor(v, completionLow, completionHigh)
			}
			if v, ok := cacheReads[provider]; ok {
				scores.cacheRead = scoreFor(v, cacheLow, cacheHigh)
			}
			if v, ok := throughputs[provider+"_catalog"]; ok {
				scores.throughput = scoreFor(v, throughputHigh, throughputLow)
			}
			if v, ok := latencies[provider+"_catalog"]; ok {
				scores.latency = scoreFor(v, latencyLow, latencyHigh)
			}
		}
		if m, ok := measured[provider]; ok {
			if m.throughput > 0 {
				scores.measThroughput = scoreFor(m.throughput, throughputHigh, throughputLow)
			}
			if m.latency > 0 {
				scores.measLatency = scoreFor(m.latency, latencyLow, latencyHigh)
			}
		}
		result[provider] = scores
	}
	return result
}

// scoreFor normalizes value into [0,1]: best maps to 1, worst to 0.
// Values beyond the bounds clamp to the nearest endpoint.
func scoreFor(value, best, worst float64) float64 {
	if best == worst {
		return 1
	}
	score := (value - worst) / (best - worst)
	return max(0, min(1, score))
}

// displayContrastExponent controls the display-only contrast adjustment. An
// exponent below one spreads scores away from the worst endpoint without
// changing their ordering or the raw scores used for routing.
const displayContrastExponent = 0.35

// displayContrastScore applies the display-only contrast adjustment, preserving
// the [0,1] bounds and both endpoint values.
func displayContrastScore(score float64) float64 {
	if score <= 0 {
		return 0
	}
	if score >= 1 {
		return 1
	}
	return math.Pow(score, displayContrastExponent)
}

// gradientColor maps a score in [0,1] onto a red→white→green gradient:
// 0 (worst) is red, 0.5 is white, and scores in the best 5% use the header
// color. Endpoint shades match the previous ANSI 256 colors 203 (#ff5f5f)
// and 42 (#00d700).
func gradientColor(score float64) lipgloss.Color {
	if score >= 0.95 {
		return lipgloss.Color("81")
	}

	var r, g, b int
	switch {
	case score <= 0.5:
		t := score / 0.5
		r = 255
		g = 95 + int(160*t)
		b = 95 + int(160*t)
	default:
		t := (score - 0.5) / 0.5
		r = int(255 * (1 - t))
		g = 255 - int(40*t)
		b = int(255 * (1 - t))
	}
	return lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", r, g, b))
}

// robustBounds returns the 10th and 90th percentiles of m, used as gradient
// endpoints so a single outlier doesn't flatten the color scale.
func robustBounds(m map[string]float64) (float64, float64) {
	values := make([]float64, 0, len(m))
	for _, v := range m {
		values = append(values, v)
	}
	sort.Float64s(values)
	return percentileFloat(values, 0.1), percentileFloat(values, 0.9)
}

// percentileFloat returns the p-th percentile (0..1) of sorted values using
// linear interpolation between adjacent order statistics. Values of p outside
// [0,1] are clamped to the nearest endpoint; NaN is treated as 0.
func percentileFloat(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	if math.IsNaN(p) {
		p = 0
	} else {
		p = max(0, min(1, p))
	}
	position := p * float64(len(values)-1)
	lower := int(position)
	upper := min(len(values)-1, lower+1)
	fraction := position - float64(lower)
	return values[lower] + (values[upper]-values[lower])*fraction
}

// providerMeasuredMetrics reports the rolling 30-minute P50 throughput and
// total request latency shown in the routing table's mTs and mLat columns.
//
// This is the lived-experience view: every successful request counts, including
// the client's own traffic. Ranking deliberately does not use it — see
// mergeMeasuredProviderResults, which compares providers only on the identical
// benchmark ping, because client traffic reaches whichever provider is pinned
// and so cannot be compared against another provider's ping.
func providerMeasuredMetrics(records []requestRecord, model, provider string, now time.Time) (tps, latency float64) {
	cutoff := now.Add(-measuredMetricsTTL)
	throughputs := make([]float64, 0)
	latencies := make([]float64, 0)
	for _, r := range records {
		if r.Model != model || r.Provider != provider || r.Time.Before(cutoff) || r.apiError() {
			continue
		}
		if value := r.tokensPerSecond(); value > 0 {
			throughputs = append(throughputs, value)
		}
		if value := float64(r.Duration.Milliseconds()); value > 0 {
			latencies = append(latencies, value)
		}
	}
	sort.Float64s(throughputs)
	sort.Float64s(latencies)
	if len(throughputs) > 0 {
		tps = percentileFloat(throughputs, 0.5)
	}
	if len(latencies) > 0 {
		latency = percentileFloat(latencies, 0.5)
	}
	return tps, latency
}

func (m dashboardModel) renderProviderTable(snap statsSnapshot, width, height int) string {
	pinned, pinnedManual := m.routing.pinInfo(m.model)
	providers := m.routing.providers(m.model, snap.Observed[m.model])

	endpoints := m.routing.endpoints(m.model)
	endpointByTag := make(map[string]endpointMeta, len(endpoints))
	for _, ep := range endpoints {
		endpointByTag[ep.Tag] = ep
	}

	if len(providers) == 0 && len(endpoints) == 0 {
		return dimStyle.Render(shorten("waiting for model data", width))
	}

	header := []string{"Provider", "In", "Out", "Cache", "Tok/s", "mTs", "Lat", "mLat", "TTFT", "Cache%", "API Err", "Tool Err"}

	now := m.routing.now()
	measured := make(map[string]measuredMetrics, len(providers))
	records := make(map[string][]requestRecord, len(providers))
	for _, provider := range providers {
		tps, lat := providerMeasuredMetrics(snap.MetricsRecords, m.model, provider, now)
		measured[provider] = measuredMetrics{throughput: tps, latency: lat}
	}
	cutoff := now.Add(-measuredMetricsTTL)
	for _, record := range snapshotWindowRecords(snap) {
		if record.Model == m.model && (record.Time.IsZero() || !record.Time.Before(cutoff)) {
			records[record.Provider] = append(records[record.Provider], record)
		}
	}

	blocked := m.routing.blockedProviders(m.model)
	scores := computeScores(providers, endpointByTag, measured)
	ttfts := make(map[string]float64, len(providers))
	for _, provider := range providers {
		if value := groupTTFTDuration(records[provider]); value > 0 {
			ttfts[provider] = float64(value)
		}
	}
	ttftLow, ttftHigh := robustBounds(ttfts)
	ttftCells := make(map[string]string, len(providers))
	for _, provider := range providers {
		cell := groupTTFT(records[provider])
		if value, ok := ttfts[provider]; ok {
			cell = lipgloss.NewStyle().Foreground(gradientColor(displayContrastScore(scoreFor(value, ttftLow, ttftHigh)))).Render(cell)
		}
		ttftCells[provider] = cell
	}
	styleCell := func(value string, score float64) string {
		if score < 0 {
			return value
		}
		return lipgloss.NewStyle().Foreground(gradientColor(displayContrastScore(score))).Render(value)
	}
	// Below 85 cells the single-row table no longer has enough room for both
	// readable provider labels and its metric headers. Wider panes use the
	// elastic table. Narrow panes retain every metric in a multi-line layout.
	if width < 85 {
		return m.renderCompactProviderTable(providers, endpointByTag, measured, records, ttftCells, scores, blocked, pinned, pinnedManual, m.selection, width, height, styleCell)
	}

	type tableRow struct {
		cells    []string
		selected bool
		blocked  bool
	}
	rows := []tableRow{{cells: header}}
	for i, provider := range providers {
		ep, ok := endpointByTag[provider]
		marker := " "
		if provider == pinned {
			if pinnedManual {
				marker = "◆"
			} else {
				marker = "◈"
			}
		}
		label := marker + " " + provider
		block, isBlocked := blocked[provider]
		if isBlocked {
			// A blocked provider is broken for this account no matter how
			// healthy the catalog claims it is, and it stays broken until a
			// setting changes — so unlike a failed test this mark does not
			// expire, and it names the reason instead of just failing.
			label += " ⊘ " + shortBlockReason(block.Reason)
		} else if m.testing[testResultKey(m.model, provider)] {
			label += " " + spinnerFrames[m.spinnerFrame]
		} else if r, fresh := m.testResultFor(m.model, provider); fresh && r.err != nil {
			label = errorStyle.Render(label + " ✗")
		}
		cells := []string{label}
		s := scores[provider]
		meas := measured[provider]
		mToks := meas.throughput
		mLat := meas.latency
		if r, fresh := m.testResultFor(m.model, provider); fresh && r.err == nil {
			mToks = r.tps
			mLat = float64(r.latency.Milliseconds())
		}
		if isBlocked {
			// An account-policy refusal makes the endpoint unroutable regardless
			// of older successes, so those measurements no longer describe an
			// available route. Ordinary API errors remain a separate reliability
			// signal and must not erase successful measurements.
			mToks, mLat = 0, 0
			s.measThroughput, s.measLatency = noScore, noScore
		}
		apiErr, toolErr := coloredGroupErrorRates(records[provider])
		observed := []string{ttftCells[provider], groupCache(records[provider]), apiErr, toolErr}
		if ok {
			cells = append(cells,
				styleCell(pricePerM(ep.Pricing.Prompt), s.prompt),
				styleCell(pricePerM(ep.Pricing.Completion), s.completion),
				styleCell(pricePerM(ep.Pricing.InputCacheRead), s.cacheRead),
				styleCell(formatThroughput(ep.Throughput), s.throughput),
				styleCell(formatThroughput(mToks), s.measThroughput),
				styleCell(formatLatency(ep.Latency), s.latency),
				styleCell(formatLatency(mLat), s.measLatency),
				observed[0], observed[1], observed[2], observed[3],
			)
		} else {
			cells = append(cells,
				"", "", "", "",
				styleCell(formatThroughput(mToks), s.measThroughput),
				"",
				styleCell(formatLatency(mLat), s.measLatency),
				observed[0], observed[1], observed[2], observed[3],
			)
		}
		rows = append(rows, tableRow{cells: cells, selected: i == m.selection, blocked: isBlocked})
	}

	allCells := make([][]string, len(rows))
	for i := range rows {
		allCells[i] = rows[i].cells
	}
	columnWidths := providerTableColumnWidths(allCells, width)
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		line := renderProviderTableRow(row.cells, columnWidths, width)
		if row.blocked {
			// Tint the whole row, discarding the price gradient: those prices
			// are not on offer to this account, so colouring them as a good
			// deal would be the misleading part.
			line = applyRowStyle(line, errorStyle)
		}
		if row.selected {
			line = applyRowStyle(padRight(line, width), selectionStyle)
		}
		lines = append(lines, line)
	}
	content := strings.Join(lines, "\n")
	if lipgloss.Height(content) > height {
		content = shortenLines(content, height)
	}
	return content
}

func (m dashboardModel) renderCompactProviderTable(providers []string, endpoints map[string]endpointMeta, measured map[string]measuredMetrics, records map[string][]requestRecord, ttftCells map[string]string, scores map[string]providerScores, blocked map[string]blockedProvider, pinned string, pinnedManual bool, selection, width, height int, styleCell func(string, float64) string) string {
	lines := []string{titleStyle.Render("Provider metrics")}
	for i, provider := range providers {
		marker := " "
		if provider == pinned {
			if pinnedManual {
				marker = "◆"
			} else {
				marker = "◈"
			}
		}
		label := marker + " " + provider
		block, isBlocked := blocked[provider]
		if isBlocked {
			label = errorStyle.Render(label + " ⊘ " + shortBlockReason(block.Reason))
		} else if m.testing[testResultKey(m.model, provider)] {
			label += " " + spinnerFrames[m.spinnerFrame]
		} else if r, fresh := m.testResultFor(m.model, provider); fresh && r.err != nil {
			label = errorStyle.Render(label + " ✗")
		}
		if i == selection {
			lines = append(lines, applyRowStyle(padRight(shorten(label, width), width), selectionStyle))
		} else {
			lines = append(lines, shorten(label, width))
		}
		ep := endpoints[provider]
		s := scores[provider]
		meas := measured[provider]
		mToks := meas.throughput
		mLat := meas.latency
		if r, fresh := m.testResultFor(m.model, provider); fresh && r.err == nil {
			mToks = r.tps
			mLat = float64(r.latency.Milliseconds())
		}
		if isBlocked {
			mToks, mLat = 0, 0
			s.measThroughput, s.measLatency = noScore, noScore
		}
		apiErr, toolErr := coloredGroupErrorRates(records[provider])
		pricing := []string{
			"In " + styleCell(pricePerM(ep.Pricing.Prompt), s.prompt),
			"Out " + styleCell(pricePerM(ep.Pricing.Completion), s.completion),
			"Cache " + styleCell(pricePerM(ep.Pricing.InputCacheRead), s.cacheRead),
		}
		performance := []string{
			"Tok/s " + styleCell(formatThroughput(ep.Throughput), s.throughput),
			"mTs " + styleCell(formatThroughput(mToks), s.measThroughput),
			"Lat " + styleCell(formatLatency(ep.Latency), s.latency),
			"mLat " + styleCell(formatLatency(mLat), s.measLatency),
			"TTFT " + ttftCells[provider],
			"Cache% " + groupCache(records[provider]),
		}
		if width < 60 {
			observed := []string{"TTFT " + ttftCells[provider], "Cache% " + groupCache(records[provider]), "API " + apiErr, "Tool " + toolErr}
			lines = append(lines, shorten(strings.Join(pricing, " "), width))
			lines = append(lines, shorten(strings.Join(performance[:4], " "), width))
			lines = append(lines, shorten(strings.Join(observed, " "), width))
		} else {
			pricing = append(pricing, "API Err "+apiErr, "Tool Err "+toolErr)
			lines = append(lines, shorten(strings.Join(pricing, " "), width))
			lines = append(lines, shorten(strings.Join(performance, " "), width))
		}
	}
	if lipgloss.Height(strings.Join(lines, "\n")) > height {
		return shortenLines(strings.Join(lines, "\n"), height)
	}
	return strings.Join(lines, "\n")
}

// providerTableColumnWidths gives metric columns enough room for their headers
// and displayed values before expanding the provider label into any remaining
// space. This avoids wasting half of a narrow routing pane on short provider
// names while every metric is ellipsized to the same small width.
func providerTableColumnWidths(rows [][]string, maxWidth int) []int {
	if len(rows) == 0 || len(rows[0]) == 0 {
		return nil
	}
	columns := len(rows[0])
	desired := make([]int, columns)
	for _, row := range rows {
		for i := 0; i < min(columns, len(row)); i++ {
			desired[i] = max(desired[i], lipgloss.Width(row[i]))
		}
	}

	widths := make([]int, columns)
	for i, cell := range rows[0] {
		widths[i] = lipgloss.Width(cell)
	}
	widths[0] = max(20, widths[0])
	desired[0] = min(36, max(widths[0], desired[0]))
	for i := 1; i < columns; i++ {
		desired[i] = min(10, max(widths[i], desired[i]))
	}

	available := max(columns, maxWidth-(columns-1))
	used := 0
	for _, cellWidth := range widths {
		used += cellWidth
	}
	// Metric columns take priority because a truncated number is ambiguous.
	for i := 1; i < columns && used < available; i++ {
		grow := min(desired[i]-widths[i], available-used)
		widths[i] += grow
		used += grow
	}
	// Provider consumes the true surplus, up to the established 36-cell cap.
	if used < available {
		grow := min(desired[0]-widths[0], available-used)
		widths[0] += grow
		used += grow
	}
	if used < available {
		widths[0] += min(36-widths[0], available-used)
	}
	return widths
}

func renderProviderTableRow(cells []string, columnWidths []int, maxWidth int) string {
	if len(cells) == 0 {
		return ""
	}
	parts := make([]string, len(cells))
	for i := range cells {
		cellWidth := 1
		if i < len(columnWidths) {
			cellWidth = columnWidths[i]
		}
		parts[i] = padRight(shorten(cells[i], cellWidth), cellWidth)
	}
	return shorten(strings.Join(parts, " "), maxWidth)
}

// renderProviderTableRowStyled renders a provider-table row with style applied
// across the whole line. The cells already carry their own ANSI resets, so the
// style wraps the completed row instead of tinting each cell separately —
// per-cell tinting stops at each cell's reset, leaving only the glyphs lit.
func renderProviderTableRowStyled(cells []string, columnWidths []int, maxWidth int, style lipgloss.Style) string {
	if len(cells) == 0 {
		return ""
	}
	return applyRowStyle(renderProviderTableRow(cells, columnWidths, maxWidth), style)
}

// applyRowStyle spans line with style's background. A plain line gets a simple
// wrap; a line containing styled cells gets the background re-asserted after
// every embedded ANSI reset, so the tint covers the whole row — including the
// padding and column separators — instead of breaking at each reset.
func applyRowStyle(line string, style lipgloss.Style) string {
	if !strings.Contains(line, "\x1b") {
		return style.Render(line)
	}
	prefix, suffix := stylePrefixSuffix(style)
	if prefix == "" || suffix == "" {
		return line
	}
	return prefix + strings.ReplaceAll(strings.TrimSuffix(line, suffix), suffix, suffix+prefix) + suffix
}

// stylePrefixSuffix extracts the ANSI prefix and suffix a style wraps around
// its content, from a probe render.
func stylePrefixSuffix(style lipgloss.Style) (string, string) {
	const probe = "X"
	rendered := style.Render(probe)
	i := strings.Index(rendered, probe)
	if i < 0 {
		return "", ""
	}
	return rendered[:i], rendered[i+1:]
}

func padRight(value string, width int) string {
	w := lipgloss.Width(value)
	if w >= width {
		return value
	}
	return value + strings.Repeat(" ", width-w)
}

func formatThroughput(value float64) string {
	if value <= 0 {
		return ""
	}
	return fmt.Sprintf("%.0f", value)
}

func formatLatency(value float64) string {
	if value <= 0 {
		return ""
	}
	return fmt.Sprintf("%.0f", value)
}

func pricePerM(value decimal) string {
	if value <= 0 {
		return ""
	}
	f := value.float()
	if f < 0.001 {
		return fmt.Sprintf("%.3f", f*1_000_000)
	}
	return fmt.Sprintf("%.2f", f*1_000_000)
}

func shortenLines(content string, height int) string {
	lines := strings.Split(content, "\n")
	if len(lines) <= height {
		return content
	}
	return strings.Join(lines[:height], "\n")
}

func displayMonthSpend(snap statsSnapshot, now time.Time) float64 {
	creditSpend := snap.CreditSpend
	if snap.CreditMonth != now.Local().Format("2006-01") {
		creditSpend = 0
	}
	return max(monthSpend(snap.DailyCosts, now), creditSpend)
}

func shorten(value string, width int) string {
	if lipgloss.Width(value) <= width {
		return value
	}
	if width <= 1 {
		return "…"
	}
	runes := []rune(value)
	for len(runes) > 0 && lipgloss.Width(string(runes))+1 > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

// shortenTail keeps the end of value, truncating the front with an ellipsis.
func shortenTail(value string, width int) string {
	if lipgloss.Width(value) <= width {
		return value
	}
	if width <= 1 {
		return "…"
	}
	runes := []rune(value)
	for len(runes) > 0 && lipgloss.Width(string(runes))+1 > width {
		runes = runes[1:]
	}
	return "…" + string(runes)
}

// providerGroup is one (model, provider) pair with its request records.
type providerGroup struct {
	model    string
	provider string
	records  []requestRecord
	latest   time.Time
}

// modelGroup is a model with its providers, ordered by most recent activity.
type modelGroup struct {
	model     string
	providers []providerGroup
	latest    time.Time
}

// groupByModelProvider groups records by (model, provider), ordered by most
// recent activity: models first, then providers within each model.
// providerGroupKey identifies one (model, provider) pair. A struct key avoids
// collisions from slashes inside model or provider names.
type providerGroupKey struct {
	model    string
	provider string
}

func groupByModelProvider(records []requestRecord) []modelGroup {
	byKey := make(map[providerGroupKey]*providerGroup)
	for _, r := range records {
		if r.Model == "" || r.Provider == "" {
			continue
		}
		key := providerGroupKey{model: r.Model, provider: r.Provider}
		pg, ok := byKey[key]
		if !ok {
			pg = &providerGroup{model: r.Model, provider: r.Provider}
			byKey[key] = pg
		}
		pg.records = append(pg.records, r)
		if r.Time.After(pg.latest) {
			pg.latest = r.Time
		}
	}
	byModel := make(map[string]*modelGroup)
	for _, pg := range byKey {
		mg, ok := byModel[pg.model]
		if !ok {
			mg = &modelGroup{model: pg.model}
			byModel[pg.model] = mg
		}
		mg.providers = append(mg.providers, *pg)
		if pg.latest.After(mg.latest) {
			mg.latest = pg.latest
		}
	}
	groups := make([]modelGroup, 0, len(byModel))
	for _, mg := range byModel {
		sort.Slice(mg.providers, func(i, j int) bool {
			pi, pj := mg.providers[i], mg.providers[j]
			if !pi.latest.Equal(pj.latest) {
				return pi.latest.After(pj.latest)
			}
			return pi.provider < pj.provider
		})
		groups = append(groups, *mg)
	}
	sort.Slice(groups, func(i, j int) bool {
		gi, gj := groups[i], groups[j]
		if !gi.latest.Equal(gj.latest) {
			return gi.latest.After(gj.latest)
		}
		return gi.model < gj.model
	})
	return groups
}

// reservedLabelWidth is the minimum width reserved for the model/provider
// label column in grouped tables such as Recent spend.
const reservedLabelWidth = 33

// groupLabelWidth sizes the label column for the model/provider groups, capped
// so the data columns and the spaces between them remain inside width.
func groupLabelWidth(groups []modelGroup, width, dataWidth, dataCols int) int {
	labelWidth := reservedLabelWidth
	for _, g := range groups {
		if w := lipgloss.Width(g.model); w > labelWidth {
			labelWidth = w
		}
		for _, p := range g.providers {
			if w := lipgloss.Width("  " + p.provider); w > labelWidth {
				labelWidth = w
			}
		}
	}
	maxLabelWidth := min(36, width-dataWidth-dataCols)
	// The reserved width is a preference, not a hard minimum. Forcing all 33
	// label cells in a narrow pane can make rows wrap even though Lip Gloss
	// still counts each as one line, causing panel borders to drift.
	return max(1, min(labelWidth, maxLabelWidth))
}

// groupHeaderLine renders the title and column headers of a grouped table.
func groupHeaderLine(title string, labelWidth int, dataWidths []int, headers []string) string {
	line := padRight(shorten(title, labelWidth), labelWidth)
	for i, h := range headers {
		line += " " + padRight(shorten(h, dataWidths[i]), dataWidths[i])
	}
	return line
}

// groupEmptyRow renders a blank row for a grouped table with no data.
func groupEmptyRow(labelWidth int, dataWidths []int) string {
	row := padRight("", labelWidth)
	for _, dataWidth := range dataWidths {
		row += " " + padRight("", dataWidth)
	}
	return row
}

// renderGroupedTable renders the Recent spend table grouped by model and
// provider: a header row, then each model with its providers underneath. cells
// returns the data values for one provider row. maxHeight caps the number of
// rendered lines; callers provide groups in their desired display order.
func renderGroupedTable(groups []modelGroup, width int, headers []string, minDataWidth, maxHeight int, cells func(providerGroup) []string) string {
	dataWidths := make([]int, len(headers))
	dataWidth := 0
	for i, header := range headers {
		dataWidths[i] = max(minDataWidth, lipgloss.Width(header))
		dataWidth += dataWidths[i]
	}
	// Extremely narrow terminals cannot fit the preferred metric widths. Trim
	// the widest columns first, always leaving one cell for every metric and
	// one for the model/provider label.
	maxDataWidth := max(len(headers), width-len(headers)-1)
	for dataWidth > maxDataWidth {
		widest := -1
		for i, cellWidth := range dataWidths {
			if cellWidth > 1 && (widest < 0 || cellWidth > dataWidths[widest]) {
				widest = i
			}
		}
		if widest < 0 {
			break
		}
		dataWidths[widest]--
		dataWidth--
	}
	labelWidth := groupLabelWidth(groups, width, dataWidth, len(headers))

	lines := []string{groupHeaderLine("Spend windows", labelWidth, dataWidths, headers)}
	if len(groups) == 0 {
		lines = append(lines, groupEmptyRow(labelWidth, dataWidths))
	}
	for _, g := range groups {
		lines = append(lines, shorten(padRight(shortenTail(g.model, labelWidth), labelWidth), width))
		for _, p := range g.providers {
			row := padRight(shortenTail("  "+p.provider, labelWidth), labelWidth)
			for i, value := range cells(p) {
				if i >= len(dataWidths) {
					break
				}
				row += " " + padRight(shorten(value, dataWidths[i]), dataWidths[i])
			}
			lines = append(lines, shorten(row, width))
		}
	}
	if maxHeight > 0 && len(lines) > maxHeight {
		lines = lines[:maxHeight]
	}
	return strings.Join(lines, "\n")
}

// groupTTFT returns the average time to first token across records, or "".
func groupTTFT(records []requestRecord) string {
	return durationText(groupTTFTDuration(records))
}

func groupTTFTDuration(records []requestRecord) time.Duration {
	var total time.Duration
	var count int
	for _, r := range records {
		if r.TTFT > 0 {
			total += r.TTFT
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return total / time.Duration(count)
}

// Hide zero and missing rates; color the remaining measured error rates red.
func coloredGroupErrorRates(records []requestRecord) (api, tools string) {
	api, tools = groupErrorRates(records)
	if api == "0.0%" || api == "" {
		api = ""
	} else {
		api = errorStyle.Render(api)
	}
	if tools == "0.0%" || tools == "" {
		tools = ""
	} else {
		tools = errorStyle.Render(tools)
	}
	return api, tools
}

// groupCache returns the average cache-hit percentage across records, or "".
func groupCache(records []requestRecord) string {
	var totalCached, totalPrompt int
	for _, r := range records {
		if r.PromptTokens > 0 {
			totalCached += r.CachedTokens
			totalPrompt += r.PromptTokens
		}
	}
	if totalPrompt == 0 {
		return ""
	}
	return cachePercentText(float64(totalCached) / float64(totalPrompt) * 100)
}

// groupErrorRates returns the API and tool error rates across records.
func groupErrorRates(records []requestRecord) (api, tools string) {
	var total, apiErrs, toolReqs, toolErrs int
	for _, r := range records {
		// A record with no response status was cancelled by orr — the
		// benchmark gate, a client disconnect — and is not the provider
		// failing, so it belongs to neither side of the ratio.
		if r.Status == 0 {
			continue
		}
		total++
		if r.apiError() {
			apiErrs++
		}
		if r.ToolCalls > 0 {
			toolReqs++
			if r.apiError() {
				toolErrs++
			}
		}
	}
	if total == 0 {
		return "", ""
	}
	api = formatRate(float64(apiErrs) / float64(total) * 100)
	tools = ""
	if toolReqs > 0 {
		tools = formatRate(float64(toolErrs) / float64(toolReqs) * 100)
	}
	return api, tools
}

func formatRate(rate float64) string {
	if rate < 0 {
		return ""
	}
	return fmt.Sprintf("%.1f%%", rate)
}

func formatCost(cost float64) string {
	if cost < 0 {
		return ""
	}
	return fmt.Sprintf("$%.4f", cost)
}

func renderRecentExpenses(snap statsSnapshot, width int, now time.Time, maxHeight int) string {
	const minimumRecentSpend = 1.0
	spendWindows := []time.Duration{time.Minute, 10 * time.Minute, time.Hour, spendWindowRetention}
	sortWindows := []time.Duration{spendWindowRetention, time.Hour, 10 * time.Minute, time.Minute}
	records := snapshotWindowRecords(snap)

	totals := make(map[providerGroupKey]float64)
	cutoff := now.Add(-diagnosticWindows[len(diagnosticWindows)-1])
	for _, record := range records {
		if !record.Time.Before(cutoff) {
			totals[providerGroupKey{model: record.Model, provider: record.Provider}] += record.Cost
		}
	}
	filteredRecords := make([]requestRecord, 0, len(records))
	for _, record := range records {
		key := providerGroupKey{model: record.Model, provider: record.Provider}
		if totals[key] >= minimumRecentSpend {
			filteredRecords = append(filteredRecords, record)
		}
	}
	groups := groupByModelProvider(filteredRecords)
	sortSpendGroups(groups, now, sortWindows)

	return renderGroupedTable(groups, width, []string{"1m", "10m", "60m", "8h"}, 7, maxHeight, func(p providerGroup) []string {
		costs, counts := spendWindowTotals(p.records, now, spendWindows)
		values := make([]string, len(spendWindows))
		for i := range spendWindows {
			values[i] = formatWindowCost(costs[i], counts[i])
		}
		return values
	})
}

// snapshotWindowRecords returns the larger rolling-window buffer in production.
// The fallback keeps direct statsSnapshot fixtures useful in focused render tests.
func snapshotWindowRecords(snap statsSnapshot) []requestRecord {
	if len(snap.WindowRecords) > 0 {
		return snap.WindowRecords
	}
	return snap.Records
}

func spendWindowTotals(records []requestRecord, now time.Time, windows []time.Duration) ([]float64, []int) {
	costs := make([]float64, len(windows))
	counts := make([]int, len(windows))
	for _, record := range records {
		for i, window := range windows {
			if !record.Time.Before(now.Add(-window)) {
				costs[i] += record.Cost
				counts[i]++
			}
		}
	}
	return costs, counts
}

// sortSpendGroups orders both model groups and their provider rows by the
// supplied spend-window priority, descending at each window.
func sortSpendGroups(groups []modelGroup, now time.Time, windows []time.Duration) {
	providerTotals := make(map[providerGroupKey][]float64)
	modelTotals := make(map[string][]float64)
	for i := range groups {
		modelTotals[groups[i].model] = make([]float64, len(windows))
		for _, provider := range groups[i].providers {
			costs, _ := spendWindowTotals(provider.records, now, windows)
			providerTotals[providerGroupKey{model: provider.model, provider: provider.provider}] = costs
			for j, cost := range costs {
				modelTotals[groups[i].model][j] += cost
			}
		}
		sort.SliceStable(groups[i].providers, func(a, b int) bool {
			left := groups[i].providers[a]
			right := groups[i].providers[b]
			return spendTotalsGreater(
				providerTotals[providerGroupKey{model: left.model, provider: left.provider}],
				providerTotals[providerGroupKey{model: right.model, provider: right.provider}],
			)
		})
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return spendTotalsGreater(modelTotals[groups[i].model], modelTotals[groups[j].model])
	})
}

func spendTotalsGreater(left, right []float64) bool {
	for i := 0; i < min(len(left), len(right)); i++ {
		if left[i] != right[i] {
			return left[i] > right[i]
		}
	}
	return false
}

func formatWindowCost(cost float64, count int) string {
	if count == 0 {
		return ""
	}
	return formatCost(cost)
}

func durationText(duration time.Duration) string {
	if duration == 0 {
		return ""
	}
	return fmt.Sprintf("%d", duration.Round(time.Millisecond)/time.Millisecond)
}

func formatPinTTL(d time.Duration) string {
	d = d.Round(time.Minute)
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	if hours > 0 && mins == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh%dm", hours, mins)
	}
	if mins > 0 {
		return fmt.Sprintf("%dm", mins)
	}
	return d.String()
}

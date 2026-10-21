package app

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
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

type providerQualityMsg struct {
	model      string
	values     map[string]providerQuality
	err        error
	fetchedAt  time.Time
	generation uint64
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

// modelCatalogMsg carries the upstream model catalog the `k` picker searches.
type modelCatalogMsg struct {
	models []catalogModel
	err    error
}

type dashboardModel struct {
	stats            *stats
	routing          *routingState
	cfg              config
	viewport         viewport.Model
	width            int
	height           int
	selection        int
	providerSort     providerSortColumn
	predictionStep   int
	model            string
	activeModels     []string
	autoScroll       bool
	serverErr        error
	spinnerFrame     int
	refresh          func(model string)
	checkDegradation func(model string)
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
	fetchQuality     func(model string) (providerQualityCache, error)
	quality          map[string]map[string]providerQuality
	qualityFetchedAt map[string]time.Time
	qualityRetryAt   map[string]time.Time
	qualityLoading   map[string]bool
	// watchedModels are the models added with `k`. They stay on the tab list
	// without traffic, so a model nothing has requested yet can be inspected.
	watchedModels []string
	// closedModels holds the tabs closed with `x`, with the time of the close.
	// A closed model returns to the list only on a request that completes
	// afterwards, which is what brings back a model with traffic in flight.
	closedModels map[string]time.Time
	picker       modelPicker
	fetchCatalog func() ([]catalogModel, error)
}

var (
	// panelBorderColor frames the panes and colors the parenthetical keys of the
	// controls inside them, so a helper reads as part of the frame rather than as
	// another dim label.
	panelBorderColor   = lipgloss.Color("62")
	panelStyle         = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(panelBorderColor).Padding(0, 1)
	titleStyle         = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	dimStyle           = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	errorStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	watermarkStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("234"))
	usageQuarterStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("249"))
	releaseUpdateStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("136"))
	keyHintStyle       = lipgloss.NewStyle().Foreground(panelBorderColor)
	spinnerFrames      = []string{"─", "\\", "|", "/"}
	// selectionStyle lifts the selected provider row with a subtle background
	// so the '>' cursor's target line is easy to spot.
	selectionStyle = lipgloss.NewStyle().Background(lipgloss.Color("237"))
	// selectedStyle marks the current speed/price stop and the degradation
	// switch's state. It sets no colour of its own, so those words keep the
	// terminal's foreground the way the values on the header line do: the state
	// reads by contrast with the dim label beside it, not as a second accent
	// competing with the panel border.
	selectedStyle = lipgloss.NewStyle()
	// logStripeColor shades every other log column so adjacent values stay
	// visually distinguishable across the table's full width. It sits close to
	// a black background on purpose: the stripe should read as texture, not as
	// a highlight competing with the values.
	logStripeColor = lipgloss.Color("234")
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
	m := dashboardModel{
		cfg: cfg, stats: stats, routing: routing,
		viewport:         viewport.New(0, 0),
		autoScroll:       true,
		predictionStep:   defaultPredictionWorkloadStep,
		testResults:      make(map[string]providerTestMsg),
		testing:          make(map[string]bool),
		allTesting:       make(map[string]map[string]bool),
		allTestRevisions: make(map[string]uint64),
		stopwatch:        newStopwatch(),
		checkRelease:     checkForReleaseUpdate,
		quality:          make(map[string]map[string]providerQuality),
		qualityFetchedAt: make(map[string]time.Time),
		qualityRetryAt:   make(map[string]time.Time),
		qualityLoading:   make(map[string]bool),
		closedModels:     make(map[string]time.Time),
	}
	if upstream, err := url.Parse(cfg.Upstream); err == nil && upstream.Scheme == "https" && upstream.Host == "openrouter.ai" {
		client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		m.fetchQuality = func(model string) (providerQualityCache, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return routing.refreshProviderQuality(ctx, client, "https://openrouter.ai", cfg.openRouterKey(), model)
		}
		m.fetchCatalog = func() ([]catalogModel, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return fetchCatalogModels(ctx, client, cfg.Upstream, cfg.openRouterKey())
		}
	}
	return m
}

func (m *dashboardModel) refreshQualityCmd() tea.Cmd {
	if m.fetchQuality == nil || m.model == "" || m.qualityLoading[m.model] {
		return nil
	}
	if last, ok := m.qualityFetchedAt[m.model]; ok && m.routing.now().Sub(last) < providerQualityTTL {
		return nil
	}
	if m.routing.now().Before(m.qualityRetryAt[m.model]) {
		return nil
	}
	model, fetch := m.model, m.fetchQuality
	m.qualityLoading[model] = true
	return func() tea.Msg {
		cached, err := fetch(model)
		return providerQualityMsg{model: model, values: cached.values, fetchedAt: cached.fetchedAt, generation: cached.generation, err: err}
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
		if qualityCmd := m.refreshQualityCmd(); qualityCmd != nil {
			return m, tea.Batch(tickDashboard(), qualityCmd)
		}
		return m, tickDashboard()
	case providerQualityMsg:
		delete(m.qualityLoading, msg.model)
		m.routing.mu.RLock()
		generation := m.routing.qualityGenerations[msg.model]
		m.routing.mu.RUnlock()
		if msg.generation != generation {
			break
		}
		delete(m.qualityFetchedAt, msg.model)
		delete(m.qualityRetryAt, msg.model)
		// Expired data must not survive a failed refresh as if it were current.
		delete(m.quality, msg.model)
		if msg.err == nil {
			m.quality[msg.model] = msg.values
			m.qualityFetchedAt[msg.model] = msg.fetchedAt
		} else {
			m.qualityRetryAt[msg.model] = m.routing.now().Add(providerQualityRetryDelay)
		}
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
				now := m.routing.now()
				results := m.selectionResults(msg.model, m.freshTestResults(msg.model), now)
				profile, apiErrs := m.selectionProfile(msg.model, now)
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
					fallback := m.routing.active(msg.model, m.currentSnap().Observed[msg.model])
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
		msg.results = m.selectionResults(msg.model, msg.results, msg.testedAt)
		profile, apiErrs := m.selectionProfile(msg.model, msg.testedAt)
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
	case modelCatalogMsg:
		m.picker.loading = false
		m.picker.err = msg.err
		// A failed or empty fetch leaves the models orr already knows in the
		// list, so the picker stays usable without the catalog.
		if msg.err == nil && len(msg.models) > 0 {
			m.picker.catalog = msg.models
			m.picker.fetchedAt = m.routing.now()
		}
	case tea.KeyMsg:
		if m.picker.open {
			next, cmd := m.updatePicker(msg)
			return next, cmd
		}
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
		case "left", "right":
			step := benchmarkTimeWeightStep
			if msg.String() == "left" {
				step = -step
			}
			m.adjustTimeWeight(step)
		case "d":
			m.toggleDegradation()
		case "p":
			// Cycle the day the projection prices. It is a view setting, so it
			// stays in memory and resets with the process.
			m.predictionStep = (m.predictionStep + 1) % len(predictionWorkloadSteps)
		case "k":
			m.openPicker()
			return m, m.pickerCmd()
		case "x":
			m.closeTab()
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
			delete(m.qualityFetchedAt, m.model)
			delete(m.qualityRetryAt, m.model)
			delete(m.quality, m.model)
			m.routing.invalidateProviderQuality(m.model)
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
		case "o":
			// Cycle the table's display order. Only the rows move: the routing
			// order requests follow, the pin, and the stored order stay as they
			// are. The cursor stays on the same provider.
			selected := ""
			providers := m.providers()
			if m.selection >= 0 && m.selection < len(providers) {
				selected = providers[m.selection]
			}
			m.providerSort = m.providerSort.next()
			m.selectProvider(selected)
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

// requestFinishedAt is when record's request completed. Time is the start and
// Duration carries the round trip, so a stream that outlived the active window
// still dates its activity by the moment it finished — the same moment a closed
// tab counts as reopened, so the tab list and the close cannot disagree about
// whether the model is back.
func requestFinishedAt(record requestRecord) time.Time {
	return record.Time.Add(record.Duration)
}

// activeModels returns models with a request that finished within the window,
// ordered by most recent activity first. Benchmark pings are orr's own probes,
// not requests the user sent, so a cold start that measures every configured
// model must not fill the tab list with models nothing asked for.
func activeModels(snap statsSnapshot, now time.Time) []string {
	seen := make(map[string]time.Time)
	cutoff := now.Add(-activeModelWindow)
	for _, record := range snap.Records {
		finished := requestFinishedAt(record)
		if record.Model == "" || record.Benchmark || finished.Before(cutoff) {
			continue
		}
		if last, ok := seen[record.Model]; !ok || finished.After(last) {
			seen[record.Model] = finished
		}
	}
	models := make([]string, 0, len(seen))
	for model := range seen {
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool { return seen[models[i]].After(seen[models[j]]) })
	return models
}

// tabModels returns the models the Routing panel switches between: models with
// recent traffic, then the models added with `k` that have none yet. A model
// closed with `x` drops out until a request completes for it later, so closing
// a tab whose traffic is still in flight is temporary. Closed entries are
// cleared by expireClosedTabs before this runs.
func (m *dashboardModel) tabModels(snap statsSnapshot, now time.Time) []string {
	tabs := make([]string, 0, len(m.watchedModels)+4)
	for _, model := range activeModels(snap, now) {
		if m.modelClosed(model) {
			continue
		}
		tabs = append(tabs, model)
	}
	for _, model := range m.watchedModels {
		if !containsString(tabs, model) {
			tabs = append(tabs, model)
		}
	}
	return tabs
}

// hasRequestAfter reports whether a request for model finished after t and is
// still inside the active window. A completion older than the window can no
// longer put the model on the tab list, so it must not release the close either:
// the two decisions share requestFinishedAt and activeModelWindow.
func hasRequestAfter(snap statsSnapshot, model string, t, cutoff time.Time) bool {
	for _, record := range snap.Records {
		if record.Model != model || record.Benchmark {
			continue
		}
		finished := requestFinishedAt(record)
		if finished.After(t) && !finished.Before(cutoff) {
			return true
		}
	}
	return false
}

// expireClosedTabs forgets a close once a request has finished for the model
// since and that completion is still fresh enough to restore the tab. From then
// on the tab is open again, so the idle fallback must treat the model as
// displayable even after its traffic leaves the active window.
func (m *dashboardModel) expireClosedTabs(snap statsSnapshot, now time.Time) {
	cutoff := now.Add(-activeModelWindow)
	for model, closed := range m.closedModels {
		if hasRequestAfter(snap, model, closed, cutoff) {
			delete(m.closedModels, model)
		}
	}
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
	m.expireClosedTabs(snap, m.routing.now())
	m.activeModels = m.tabModels(snap, m.routing.now())
	// Keep the displayed model once chosen: concurrent requests for other
	// models must not yank the Routing panel away from it.
	if m.model != "" && containsString(m.activeModels, m.model) {
		return
	}
	next := m.model
	switch {
	case len(m.activeModels) > 0:
		next = m.activeModels[0]
	case m.routing.current() != "" && !m.modelClosed(m.routing.current()):
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

// modelClosed reports whether model was closed with `x` and no request has
// finished for it since; expireClosedTabs forgets the close on that traffic.
func (m dashboardModel) modelClosed(model string) bool {
	_, closed := m.closedModels[model]
	return closed
}

// selectActiveProvider moves the selection to the model's active provider, in
// the order the table currently displays.
func (m *dashboardModel) selectActiveProvider(snap statsSnapshot) {
	m.selection = 0
	active := m.routing.active(m.model, snap.Observed[m.model])
	for i, provider := range m.providerTableData(snap).ordered(m.providerSort) {
		if provider == active {
			m.selection = i
			break
		}
	}
}

// selectProvider keeps the cursor on a provider by name after the table is
// re-ordered under it.
func (m *dashboardModel) selectProvider(provider string) {
	if provider == "" {
		return
	}
	if index := indexOfString(m.providers(), provider); index >= 0 {
		m.selection = index
	}
}

// restoreAutomaticPin reveals the automatic route hidden by a manual
// override. Prefer its preserved measured winner; if none is available, pin
// the first routable provider in the current ranked order. Removing a manual
// override must never leave automatic routing active but visually unpinned.
func (m *dashboardModel) restoreAutomaticPin(model string) {
	restoreAutomaticPin(m.routing, m.stats, model)
}

func indexOfString(values []string, target string) int {
	for i, value := range values {
		if value == target {
			return i
		}
	}
	return -1
}

// providers lists the current model's providers in the order the table shows
// them, so navigation and the selection-bound keys follow the displayed rows.
func (m dashboardModel) providers() []string {
	return m.providerTableData(m.currentSnap()).ordered(m.providerSort)
}

func (m *dashboardModel) refreshLog() {
	m.resize()
	snap := m.currentSnap()
	m.viewport.SetContent(renderLogRows(snap.Records, m.viewport.Width))
	if m.autoScroll {
		m.viewport.GotoBottom()
	}
}

const logTableMinWidth = 151

// logTableTimeWidth is the width of the leading Time column, which every row
// renders as 15:04:05. It is named so that logTableWidths and logTableColumns,
// which both need it, cannot drift apart.
const logTableTimeWidth = 8

// renderLogRows renders only the scrollable body. The fixed header is rendered
// beside the viewport so PgUp/PgDn never moves it out of view.
func renderLogRows(records []requestRecord, width int) string {
	tableWidth := logTableWidth(width)
	if tableWidth < logTableMinWidth {
		return renderCompactLogRows(records, width)
	}
	modelWidth, providerWidth, _, _, _, _, _, _, _, _, _, _, _, _, _ := logTableWidths(tableWidth)
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
			logThroughputText(record),
			logLatencyText(record),
			fmt.Sprintf("$%.4f", record.Cost),
			logCacheReadText(record),
			logCacheWriteText(record),
			logCachePercentText(record),
		}
		line := renderLogTableRow(cells, logTableColumns(tableWidth), logRowForeground(record))
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func logTableWidth(width int) int {
	return max(1, width)
}

func logTableColumns(width int) []int {
	modelWidth, providerWidth, reasonWidth, statusWidth, durationWidth, tokenInWidth, tokenInUncachedWidth, tokenOutWidth, thinkPercentWidth, speedWidth, latencyWidth, costWidth, cacheReadWidth, cacheWriteWidth, cachePercentWidth := logTableWidths(width)
	return []int{logTableTimeWidth, modelWidth, providerWidth, reasonWidth, statusWidth, durationWidth, tokenInWidth, tokenInUncachedWidth, tokenOutWidth, thinkPercentWidth, speedWidth, latencyWidth, costWidth, cacheReadWidth, cacheWriteWidth, cachePercentWidth}
}

func logTableWidths(width int) (model, provider, reason, status, duration, tokenIn, tokenInUncached, tokenOut, thinkPercent, speed, latency, cost, cacheRead, cacheWrite, cachePercent int) {
	const (
		// Every metric column shares one width: a grouped token count reaches
		// nine cells, and holding the narrower columns to it too keeps the
		// table a regular grid rather than a ragged one.
		fixed           = logTableTimeWidth + 6 + 4 + 9 + 9 + 9 + 9 + 9 + 9 + 9 + 9 + 9 + 9 + 9 + 15 // fixed columns plus separators
		minimumModel    = 9
		minimumProvider = 10
		maximumModel    = 36
		maximumProvider = 18
	)
	variable := max(0, width-fixed-minimumModel-minimumProvider)
	model = min(maximumModel, minimumModel+variable/2)
	provider = min(maximumProvider, minimumProvider+variable-variable/2)
	return model, provider, 6, 4, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9
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
			shorten("Model / Provider  Reason  In  Uncached  Out  Think%  Tp  Lat  CR  CW  C%", width),
		}, "\n"))
	}
	return renderLogTableRow(logTableHeaderCells, logTableColumns(tableWidth), dimStyle.GetForeground())
}

// logTableHeaderCells are the column labels of the wide log table, in the same
// order as logTableColumns. They are abbreviated so the table stays compact.
var logTableHeaderCells = []string{"Time", "Model", "Provider", "Reason", "Code", "Duration", "In", "Uncached", "Out", "Think%", "Tp", "Lat", "Cost", "CR", "CW", "C%"}

// logTableFirstMetricColumn is the index of Duration, the first column holding
// a measured value rather than an identifier. It and everything after it is
// centered; the columns before it are left-aligned.
const logTableFirstMetricColumn = 5

// renderLogTableRow lays out one log table row. The first column and every
// other one after it carries the stripe background so adjacent values stay
// distinguishable. Text columns are left-aligned; the metric columns are
// centered, which is what a fixed-width numeric grid reads best as. fg applies
// to the whole row — dim for the header, red for failed requests — and is set
// per cell because a cell's own reset would otherwise clear a row-wide style.
func renderLogTableRow(cells []string, widths []int, fg lipgloss.TerminalColor) string {
	plain, shaded := logCellStyles(fg)
	parts := make([]string, len(cells))
	for i, cell := range cells {
		style := plain
		if i%2 == 0 {
			style = shaded
		}
		text := shorten(cell, widths[i])
		if i >= logTableFirstMetricColumn {
			text = padCenter(text, widths[i])
		} else {
			text = padRight(text, widths[i])
		}
		parts[i] = style.Render(text)
	}
	return strings.Join(parts, " ")
}

// logCellStyles returns the pair of cell styles used down a log table row: the
// plain style and the shaded style for every other column.
func logCellStyles(fg lipgloss.TerminalColor) (plain, shaded lipgloss.Style) {
	plain = lipgloss.NewStyle()
	if fg != nil {
		plain = plain.Foreground(fg)
	}
	return plain, plain.Background(logStripeColor)
}

// logRowForeground returns the foreground shared by every cell of a log row,
// or nil when the row takes the terminal's default foreground.
func logRowForeground(record requestRecord) lipgloss.TerminalColor {
	if record.apiError() {
		return errorStyle.GetForeground()
	}
	return nil
}

func renderCompactLogRows(records []requestRecord, width int) string {
	lines := make([]string, 0, len(records)*2)
	for _, record := range records {
		first := shorten(fmt.Sprintf("%s  %d  %s  $%.4f", record.Time.Local().Format("15:04:05"), record.Status, logDurationText(record.Duration), record.Cost), width)
		second := shorten(fmt.Sprintf("%s / %s  %s  %s  %s  %s  %s  %s  %s  %s  %s  %s", record.Model, record.Provider, logReasoningText(record), logTokenValue(record.PromptTokens), logUncachedText(record), logTokenValue(record.CompletionTokens), logThroughputText(record), logThinkPercentText(record), logLatencyText(record), logCacheReadText(record), logCacheWriteText(record), logCachePercentText(record)), width)
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
	return groupDigits(tokens)
}

// logUncachedText renders the input tokens not served from cache (Tokens in
// minus Cache read). A blank means nothing was billed as uncached input.
func logUncachedText(record requestRecord) string {
	uncached := record.PromptTokens - record.CachedTokens
	if uncached <= 0 {
		return ""
	}
	return groupDigits(uncached)
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
	return groupDigits(record.CachedTokens)
}

func logCacheWriteText(record requestRecord) string {
	if record.CacheWriteTokens <= 0 {
		return ""
	}
	return groupDigits(record.CacheWriteTokens)
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

// logThroughputText renders the request's output tokens per second, matching
// the provider table's Tp column.
func logThroughputText(record requestRecord) string {
	return groupDigits(int(math.Round(record.tokensPerSecond())))
}

// logLatencyText renders the time to first token in milliseconds, matching
// OpenRouter's latency definition and the provider table's mLat. A blank
// means the proxy never saw a first streamed token for the request.
func logLatencyText(record requestRecord) string {
	if record.TTFT <= 0 {
		return ""
	}
	return groupDigits(int(record.TTFT.Round(time.Millisecond) / time.Millisecond))
}

// groupDigits separates an integer into groups of three digits with spaces so
// long numbers stay readable: 6591173 renders as "6 591 173".
func groupDigits(n int) string {
	digits := strconv.Itoa(n)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	if len(digits) <= 3 {
		return sign + digits
	}
	head := len(digits) % 3
	if head == 0 {
		head = 3
	}
	var b strings.Builder
	b.WriteString(sign)
	b.WriteString(digits[:head])
	for i := head; i < len(digits); i += 3 {
		b.WriteByte(' ')
		b.WriteString(digits[i : i+3])
	}
	return b.String()
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
	logTitle += "  " + renderKeyHints([]keyHint{{"PgUp/PgDn", "scroll"}, {"q", "quit"}}, max(1, m.width-8))
	if m.releaseUpdate != "" {
		logTitle += releaseUpdateStyle.Render(" • " + m.releaseUpdate + " available run ./orr upgrade")
	}
	logWidth := max(1, m.width-4)
	logView := overlayWatermark(m.viewport.View(), logWidth)
	logBody := renderLogTableHeader(logWidth) + "\n" + logView
	logPane := panelStyle.Width(max(1, m.width-2)).Height(max(1, m.height-lipgloss.Height(top)-2)).Render(logTitle + "\n\n" + logBody)
	view := lipgloss.JoinVertical(lipgloss.Left, top, logPane)
	if m.picker.open {
		return overlayCenter(view, m.renderPicker(m.width, m.height), m.width, m.height)
	}
	return view
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
	// The label recedes so the figures and the bar read as the values.
	const usedLabel = "Used"
	usedValue := ""
	if snap.Budget > 0 {
		usedValue = fmt.Sprintf(" $%.0f / $%.0f ", month, snap.Budget)
		usedValue += renderUsageBar(used, max(0, width-lipgloss.Width(usedLabel+usedValue)))
	} else if month > 0 {
		usedValue = fmt.Sprintf(" $%.0f", month)
	}
	lines := []string{
		titleStyle.Render("Expenses"),
		"",
		dimLabelLine(usedLabel, usedValue, width),
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
		historyMonthLine("This m", month, monthTokens, width),
		historyMonthLine("Last m", lastMonth, lastMonthTokens, width),
		"",
	}
	for _, day := range days[:min(7, len(days))] {
		parsed, _ := time.ParseInLocation(time.DateOnly, day, now.Location())
		lines = append(lines, shorten(fmt.Sprintf("%-8s $%.4f  %s", parsed.Format("Jan 02"), snap.DailyCosts[day], formatDailyTokens(snap.DailyTokens[day])), width))
	}
	return strings.Join(lines, "\n")
}

// dimLabelLine renders label in the dim label colour and the rest of the line at
// the terminal's foreground. A line too narrow to hold the label keeps its plain
// text, so truncation never lands inside a style.
func dimLabelLine(label, rest string, width int) string {
	plain := shorten(label+rest, width)
	if !strings.HasPrefix(plain, label) {
		return plain
	}
	return dimStyle.Render(label) + strings.TrimPrefix(plain, label)
}

// historyMonthLine renders one month's cost and tokens. The month label recedes
// so the figures beside it read as the values.
func historyMonthLine(label, cost string, tokens int64, width int) string {
	return dimLabelLine(label, fmt.Sprintf("  %s  %s", cost, formatDailyTokens(tokens)), width)
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
	// A count that runs past a thousand million tokens is written in billions:
	// "1478.16m" is a number nobody reads as a size.
	millions := float64(tokens) / 1_000_000
	if millions >= 999.995 {
		return fmt.Sprintf("%.2fb", millions/1_000)
	}
	return fmt.Sprintf("%.2fm", millions)
}

// timeWeightStopNames name the control's positions from the price end to the
// speed end. The control shows these instead of the exponent it sets: the
// names are also the explanation of what each end buys, which a bar with a
// number beside it never was.
var timeWeightStopNames = []string{"price", "balanced", "performance"}

// timeWeightStop is the position the live exponent sits on.
func timeWeightStop(weight float64) int {
	return int(math.Round((clampTimeWeight(weight) - benchmarkTimeWeightMin) / benchmarkTimeWeightStep))
}

// keyHint is one shortcut on a hint line: the key itself and what it does. Only
// the key and its own parentheses take the panel border colour, so the hint
// points at what to press without tinting the explanation beside it.
type keyHint struct {
	key         string
	description string
}

func (h keyHint) parenthetical() string {
	return "(" + h.key + ")"
}

func (h keyHint) plain() string {
	if h.description == "" {
		return h.parenthetical()
	}
	return h.parenthetical() + " " + h.description
}

func (h keyHint) render() string {
	if h.description == "" {
		return keyHintStyle.Render(h.parenthetical())
	}
	return keyHintStyle.Render(h.parenthetical()) + dimStyle.Render(" "+h.description)
}

// renderKeyHints lays the hints out on one line within width. A hint whose
// description no longer fits keeps its key; once not even that fits, the rest
// are dropped, because a styled line cannot be cut at an arbitrary column.
func renderKeyHints(hints []keyHint, width int) string {
	if len(hints) == 0 {
		return ""
	}
	parts := make([]string, 0, len(hints)*2)
	used := 0
	truncated := false
	for _, hint := range hints {
		separator := ""
		if len(parts) > 0 {
			separator = " • "
		}
		if used+lipgloss.Width(separator)+lipgloss.Width(hint.plain()) <= width {
			if separator != "" {
				parts = append(parts, dimStyle.Render(separator))
			}
			parts = append(parts, hint.render())
			used += lipgloss.Width(separator) + lipgloss.Width(hint.plain())
			continue
		}
		keyOnly := separator + hint.parenthetical()
		if hint.description != "" && used+lipgloss.Width(keyOnly) < width {
			if separator != "" {
				parts = append(parts, dimStyle.Render(separator))
			}
			parts = append(parts, keyHintStyle.Render(hint.parenthetical()))
			used += lipgloss.Width(keyOnly)
		}
		truncated = true
		break
	}
	if len(parts) == 0 {
		return keyHintStyle.Render(shorten(hints[0].parenthetical(), width))
	}
	if truncated && used+1 <= width {
		parts = append(parts, dimStyle.Render("…"))
	}
	return strings.Join(parts, "")
}

// renderKeyHint fits one hint into width, shedding its description before the
// key itself and shortening the key only when the pane cannot hold even that.
func renderKeyHint(hint keyHint, width int) string {
	if lipgloss.Width(hint.plain()) <= width {
		return hint.render()
	}
	if lipgloss.Width(hint.parenthetical()) <= width {
		return keyHintStyle.Render(hint.parenthetical())
	}
	return keyHintStyle.Render(shorten(hint.parenthetical(), width))
}

// renderTimeWeightSlider draws the speed/price control as its named stops, with
// the current one lit. The line carries its own keys, the same convention as the
// degradation line below it and the model line's `[n/m Tab]`, so the control
// still says how to move once the key hint above it has truncated. Narrow panes
// give up detail in the order that costs least: the other stop names first, then
// the "Speed/price" label, then the lit stop itself, and never the keys, because
// a styled line cannot be clipped without cutting an escape sequence in half.
func renderTimeWeightSlider(weight float64, width int) string {
	stop := timeWeightStop(weight)
	const prefix = "Speed/price  "
	const key = "(←/→)"
	keys := "  " + key
	full := prefix + strings.Join(timeWeightStopNames, " · ") + keys
	if lipgloss.Width(full) <= width {
		styled := make([]string, len(timeWeightStopNames))
		for i, name := range timeWeightStopNames {
			styled[i] = dimStyle.Render(name)
		}
		styled[stop] = selectedStyle.Render(timeWeightStopNames[stop])
		return dimStyle.Render(prefix) + strings.Join(styled, dimStyle.Render(" · ")) + keyHintStyle.Render(keys)
	}
	current := prefix + timeWeightStopNames[stop] + keys
	if lipgloss.Width(current) <= width {
		return dimStyle.Render(prefix) + selectedStyle.Render(timeWeightStopNames[stop]) + keyHintStyle.Render(keys)
	}
	if lipgloss.Width(timeWeightStopNames[stop]+keys) <= width {
		return selectedStyle.Render(timeWeightStopNames[stop]) + keyHintStyle.Render(keys)
	}
	if lipgloss.Width(key) <= width {
		return keyHintStyle.Render(key)
	}
	return dimStyle.Render(shorten(full, width))
}

// renderDegradationLine draws the degradation recovery switch under the
// speed/price control. On, it also names the rule the switch is running: the
// share of baseline that counts as degraded, and how long the drop must hold.
// The line carries the `d` key itself: the hint above it lists the pane's other
// controls, and the same convention puts `[n/m Tab]` on the model line. A pane
// that runs out of room drops the rule first, then the label, and never the key:
// a styled line cannot be clipped safely at an arbitrary column.
func renderDegradationLine(enabled bool, cfg config, width int) string {
	const prefix = "Degradation recovery  "
	const key = "  (d)"
	state := "off"
	stateStyle := dimStyle
	if enabled {
		state = "on"
		stateStyle = selectedStyle
	}
	// Missing settings stay off the line rather than printing a zero: a pane is
	// no place to invent a rule the watch is not running.
	rule := ""
	if enabled {
		if cfg.DegradationThreshold > 0 {
			rule = fmt.Sprintf("%.0f%%", cfg.DegradationThreshold*100)
		}
		if window := shortDuration(cfg.DegradationWindow); window != "" {
			if rule != "" {
				rule += " / "
			}
			rule += window
		}
	}
	plain := prefix + state
	styled := dimStyle.Render(prefix) + stateStyle.Render(state)
	if rule != "" {
		plain += "  " + rule
		styled += dimStyle.Render("  " + rule)
	}
	plain += key
	styled += keyHintStyle.Render(key)
	if lipgloss.Width(plain) <= width {
		return styled
	}
	plain = prefix + state + key
	if lipgloss.Width(plain) <= width {
		return dimStyle.Render(prefix) + stateStyle.Render(state) + keyHintStyle.Render(key)
	}
	plain = state + key
	if lipgloss.Width(plain) <= width {
		return stateStyle.Render(state) + keyHintStyle.Render(key)
	}
	if lipgloss.Width("(d)") <= width {
		return keyHintStyle.Render("(d)")
	}
	return dimStyle.Render(shorten(plain, width))
}

// shortDuration renders a configured duration for the pane. Hours and minutes
// are what this feature is tuned in; sub-minute windows keep their seconds, and
// a sub-second one keeps its milliseconds rather than truncating to zero.
func shortDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	if d < time.Second {
		return d.String()
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

func (m dashboardModel) renderRouting(snap statsSnapshot, width, height int) string {
	data := m.providerTableData(snap)
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
	modelCounter := ""
	if len(m.activeModels) > 1 {
		idx := indexOfString(m.activeModels, m.model)
		if idx < 0 {
			idx = 0
		}
		modelCounter = fmt.Sprintf("[%d/%d Tab]", idx+1, len(m.activeModels))
	}
	// The hint names each key in its own parentheses, so the panel's colour can
	// point at what to press without tinting the explanation beside it. The
	// degradation switch below carries its own `d`, so this line does not. The
	// picker and the tab close sit next to the Tab switch they belong with, and
	// the provider-table keys take their own line so neither has to shed its
	// tail on a narrow pane.
	hints := []keyHint{{"↑/↓", "select"}, {"Enter", "toggle"}}
	if len(m.activeModels) > 1 {
		hints = append(hints, keyHint{"Tab", "switch"})
	}
	hints = append(hints,
		keyHint{"k", "models"},
		keyHint{"x", "close tab"},
	)
	providerHints := []keyHint{
		{"r", "reload provider list"},
		{"t", "test provider"},
		{"a", "test all"},
	}
	// Each name on the line follows the same split: a dim label, then the value
	// it names at the terminal's full brightness, the way the prediction line
	// labels its figures. The pin's value is the exception when the account
	// cannot route to it.
	const (
		modelLabel = "Model  "
		pinLabel   = "Pin    "
	)
	pinValue := shorten(pinned+pinSuffix, max(1, width-lipgloss.Width(pinLabel)))
	pinText := pinLabel + pinValue
	pinStyled := dimStyle.Render(pinLabel) + pinValue
	if pinBroken {
		pinStyled = errorStyle.Render(pinText)
	}
	// The model counter, the table's display order, the model name, and the pin
	// share a line, in that order. The pin keeps its place — it is what requests
	// actually follow — and the model name is the only label shortened; the
	// counter and the order indicator are dropped whole once space runs out.
	const gap = "    "
	orderText := "Order " + m.providerSort.label() + " (o)"
	// The order's value stays at full brightness while the routing order is the
	// default; once a column orders the table it is a state, so it takes the
	// accent the panel's keys use.
	orderName := m.providerSort.label()
	if m.providerSort != providerSortRoute {
		orderName = keyHintStyle.Render(m.providerSort.label())
	}
	orderStyled := dimStyle.Render("Order ") + orderName + keyHintStyle.Render(" (o)")
	modelRoom := func(segments ...string) int {
		used := 0
		for _, segment := range segments {
			if segment == "" {
				continue
			}
			used += lipgloss.Width(segment) + lipgloss.Width(gap)
		}
		return width - used
	}
	joinSegments := func(segments ...string) string {
		parts := make([]string, 0, len(segments))
		for _, segment := range segments {
			if segment != "" {
				parts = append(parts, segment)
			}
		}
		return strings.Join(parts, gap)
	}
	// A model name cut below this is no longer worth the cells it takes from
	// the other labels.
	const minModelWidth = 12
	// The label is a fixed seven cells, so the name takes the cuts alone.
	modelSegment := func(room int) string {
		return dimStyle.Render(modelLabel) + shorten(m.model, max(1, room-lipgloss.Width(modelLabel)))
	}
	header := ""
	switch {
	case modelRoom(modelCounter, orderText, pinText) >= minModelWidth:
		header = joinSegments(modelCounter, orderStyled,
			modelSegment(modelRoom(modelCounter, orderText, pinText)), pinStyled)
	case modelRoom(orderText, pinText) >= minModelWidth:
		header = joinSegments(orderStyled, modelSegment(modelRoom(orderText, pinText)), pinStyled)
	case modelRoom(pinText) >= minModelWidth:
		header = joinSegments(modelSegment(modelRoom(pinText)), pinStyled)
	case modelRoom(orderText, pinText) >= 0:
		// Too narrow for the model: the order indicator keeps the pin company.
		header = joinSegments(orderStyled, pinStyled)
	default:
		// The pin wins the line on its own.
		header = pinStyled
	}
	lines := []string{
		titleStyle.Render("Routing"),
		"",
		renderKeyHints(hints, width),
		renderKeyHints(providerHints, width),
		"",
		renderTimeWeightSlider(m.routing.currentTimeWeight(), width),
		renderDegradationLine(m.routing.degradationEnabled(), m.cfg, width),
	}
	// The projection follows the controls it qualifies, above the pin line: it
	// describes the selected route rather than the current pin, and three
	// horizons do not fit beside the pin without shortening the model name. A
	// line too narrow for all three drops the figure whole rather than a horizon.
	if predictText, predictStyled := m.predictionText(data); predictText != "" && lipgloss.Width(predictText) <= width {
		lines = append(lines, predictStyled)
	}
	lines = append(lines, "", header, "")
	tableHeight := max(1, height-lipgloss.Height(strings.Join(lines, "\n"))-2)
	lines = append(lines, m.renderProviderTableData(data, width, tableHeight))
	content := strings.Join(lines, "\n")
	if lipgloss.Height(content) > height {
		content = shortenLines(content, height)
	}
	return content
}

// predictionText projects the selected provider's spend over the pane's
// horizons. The selection is the row the user is on, so the figures answer
// "what would this route cost me" before it is pinned, and they follow ↑/↓ the
// way the highlight does. The label carries the workload being priced, and the
// workload is fixed, so the figures move only when the selection does or the
// provider reprices; nothing is projected when its catalog entry is missing a
// price the workload needs.
func (m dashboardModel) predictionText(data providerTableData) (plain, styled string) {
	provider, ok := data.selected(m.providerSort, m.selection)
	if !ok {
		return "", ""
	}
	endpoint, ok := data.endpoints[provider]
	if !ok {
		return "", ""
	}
	if _, blocked := data.blocked[provider]; blocked {
		// The account cannot route to this provider, so its prices are not on
		// offer and a projection would price a route that does not exist.
		return "", ""
	}
	daily := m.predictionStepValue()
	hourly, ok := newPredictionRate(float64(daily)).hourlyCost(endpoint.Pricing)
	if !ok {
		return "", ""
	}
	workload := formatPredictionWorkload(daily)
	values := strings.Join(predictionSpend(hourly), "  ")
	plain = "Prediction (p) " + workload + "  " + values
	styled = dimStyle.Render("Prediction ") + keyHintStyle.Render("(p)") + " " + workload + "  " + values
	return plain, styled
}

// freshTestResults collects the 't' ping-pong results still held for a model's
// current providers, in routing order. Selection ignores the table's display
// ordering: only which providers were measured matters here.
func (m dashboardModel) freshTestResults(model string) []providerTestResult {
	providers := m.routing.providers(model, m.currentSnap().Observed[model])
	results := make([]providerTestResult, 0, len(providers))
	for _, provider := range providers {
		if result, ok := m.testResults[testResultKey(model, provider)]; ok {
			results = append(results, result.testResult(provider))
		}
	}
	return results
}

// selectionResults completes fresh measurements with the persisted benchmark
// pool, which is what lets a selection be re-decided without re-measuring.
func (m dashboardModel) selectionResults(model string, fresh []providerTestResult, now time.Time) []providerTestResult {
	providers := m.routing.providers(model, m.currentSnap().Observed[model])
	return mergeMeasuredProviderResults(fresh, m.stats.benchmarkPool(model), providers, now)
}

// selectionProfile is the reference request a selection is priced and timed
// against, plus each provider's recent API error share.
func (m dashboardModel) selectionProfile(model string, now time.Time) (requestProfile, map[string]float64) {
	snap := m.currentSnap()
	return newRequestProfile(snap.MetricsRecords, model, now), providerAPIErrorRates(snap.Records, model, now)
}

// adjustTimeWeight moves the speed/price slider by step and re-selects the
// automatic pin from measurements already held, so the routing pane answers the
// keystroke immediately instead of waiting for the next benchmark. The value is
// mirrored into stats, which the periodic and shutdown saves persist; the
// provider order is deliberately left alone (see rerankAutomaticPin).
func (m *dashboardModel) adjustTimeWeight(step float64) {
	weight := clampTimeWeight(m.routing.currentTimeWeight() + step)
	m.routing.setTimeWeight(weight)
	m.stats.setTimeWeight(weight)
	if m.model == "" {
		return
	}
	now := m.routing.now()
	results := m.selectionResults(m.model, m.freshTestResults(m.model), now)
	profile, apiErrors := m.selectionProfile(m.model, now)
	best, updated := m.routing.rerankAutomaticPin(m.model, results, profile, apiErrors)
	if best == "" || !updated {
		return
	}
	syncPersistedAutomaticPin(m.routing, m.stats, m.model)
	m.selectActiveProvider(m.currentSnap())
}

// toggleDegradation flips degradation recovery. Turning it on checks straight
// away — the pane is where the user just made the decision, so the answer
// should not wait for the next request to be measured. Turning it off needs no
// action: the request path stops scanning on its own. The state is mirrored
// into stats, which the periodic and shutdown saves persist.
func (m *dashboardModel) toggleDegradation() {
	enabled := !m.routing.degradationEnabled()
	m.routing.setDegradation(enabled)
	m.stats.setDegradation(enabled)
	if enabled && m.checkDegradation != nil && m.model != "" {
		m.checkDegradation(m.model)
	}
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

// measuredMetrics holds the rolling 30-minute P50 throughput and P50 time to
// first token measured by this proxy. latency is the median time to first
// token, matching OpenRouter's latency definition.
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
	gpqa           float64
	tau            float64
	orToolError    float64
	orJSONError    float64
	apiError       float64
	toolError      float64
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
			gpqa:           noScore,
			tau:            noScore,
			orToolError:    noScore,
			orJSONError:    noScore,
			apiError:       noScore,
			toolError:      noScore,
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

// providerMeasuredMetrics reports the rolling 30-minute P50 throughput and P50
// time to first token shown in the routing table's mTp and mLat
// columns.
//
// Lat is the median time to first token, matching OpenRouter's latency
// definition (their "latency" is time to first token).
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
		if value := float64(r.TTFT.Milliseconds()); r.TTFT > 0 {
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

// providerSortColumn is the provider table's display ordering. Routing order is
// the default and stays the order requests follow; a metric column reorders the
// rows on screen only, leaving the pin and the routing order untouched.
type providerSortColumn int

const (
	providerSortRoute providerSortColumn = iota
	providerSortInput
	providerSortOutput
	providerSortCache
	providerSortThroughput
	providerSortMeasuredThroughput
	providerSortLatency
	providerSortMeasuredLatency
	providerSortGPQA
	providerSortTau
	providerSortToolError
	providerSortJSONError
	providerSortAPIError
	providerSortMeasuredToolError
	providerSortColumns
)

// providerSortLabels names each column as the table heads it, with Default
// standing for the routing order requests follow.
var providerSortLabels = [...]string{
	providerSortRoute:              "Default",
	providerSortInput:              "In",
	providerSortOutput:             "Out",
	providerSortCache:              "Cache",
	providerSortThroughput:         "Tp",
	providerSortMeasuredThroughput: "mTp",
	providerSortLatency:            "Lat",
	providerSortMeasuredLatency:    "mLat",
	providerSortGPQA:               "GPQA",
	providerSortTau:                "Tau",
	providerSortToolError:          "Tool",
	providerSortJSONError:          "Json",
	providerSortAPIError:           "mApi",
	providerSortMeasuredToolError:  "mTool",
}

// providerTableHeader heads the table: the provider label, then one column per
// sortable metric, in providerSortColumn order so headerIndex indexes it.
func providerTableHeader() []string {
	return append([]string{"Provider"}, providerSortLabels[providerSortInput:]...)
}

func (c providerSortColumn) label() string {
	if c < providerSortRoute || c >= providerSortColumns {
		return providerSortLabels[providerSortRoute]
	}
	return providerSortLabels[c]
}

// headerIndex is the column's cell in the provider table header, or -1 when the
// routing order is showing and no column is sorted.
func (c providerSortColumn) headerIndex() int {
	if c < providerSortInput || c >= providerSortColumns {
		return -1
	}
	return int(c)
}

// next advances the 'o' cycle, wrapping back to the routing order.
func (c providerSortColumn) next() providerSortColumn {
	if c < providerSortRoute || c >= providerSortColumns-1 {
		return providerSortRoute
	}
	return c + 1
}

// sortValueMissing marks a cell the table leaves blank. It sorts after every
// reported value whatever the column's direction.
var sortValueMissing = math.Inf(1)

// providerSortValues holds one row's ordering values. Blanks stay at
// sortValueMissing except for the error columns, where a reported zero is a
// clean record and sorts first.
type providerSortValues struct {
	in, out, cache              float64
	throughput, latency         float64
	measThroughput, measLatency float64
	gpqa, tau                   float64
	toolError, jsonError        float64
	apiError, measToolError     float64
}

// sortKey returns an ascending-is-best key for a column: prices, latencies and
// error rates as reported, throughput and benchmark scores descending, and
// blanks last.
func (v providerSortValues) sortKey(column providerSortColumn) float64 {
	switch column {
	case providerSortInput:
		return v.in
	case providerSortOutput:
		return v.out
	case providerSortCache:
		return v.cache
	case providerSortThroughput:
		return descendingKey(v.throughput)
	case providerSortMeasuredThroughput:
		return descendingKey(v.measThroughput)
	case providerSortLatency:
		return v.latency
	case providerSortMeasuredLatency:
		return v.measLatency
	case providerSortGPQA:
		return descendingKey(v.gpqa)
	case providerSortTau:
		return descendingKey(v.tau)
	case providerSortToolError:
		return v.toolError
	case providerSortJSONError:
		return v.jsonError
	case providerSortAPIError:
		return v.apiError
	case providerSortMeasuredToolError:
		return v.measToolError
	}
	return 0
}

// descendingKey flips a higher-is-better value for an ascending comparison,
// leaving the missing marker at positive infinity so blanks stay last.
func descendingKey(value float64) float64 {
	if math.IsInf(value, 1) {
		return value
	}
	return -value
}

// reportedValue keeps a measurement the table would show and blanks the rest.
func reportedValue(value float64) float64 {
	if value > 0 {
		return value
	}
	return sortValueMissing
}

// providerTableData is one model's provider table: the inputs it renders and
// the values its display ordering compares. Rows are held in routing order.
type providerTableData struct {
	providers    []string
	endpoints    map[string]endpointMeta
	measured     map[string]measuredMetrics
	records      map[string][]requestRecord
	scores       map[string]providerScores
	blocked      map[string]blockedProvider
	pinned       string
	pinnedManual bool
	sortValues   map[string]providerSortValues
}

// providerTableData gathers the table's inputs once, so the rows drawn and the
// rows navigation and the p/t keys walk are built from the same values.
func (m dashboardModel) providerTableData(snap statsSnapshot) providerTableData {
	providers := m.routing.providers(m.model, snap.Observed[m.model])
	endpointByTag := make(map[string]endpointMeta, len(providers))
	for _, endpoint := range m.routing.endpoints(m.model) {
		endpointByTag[endpoint.Tag] = endpoint
	}

	now := m.routing.now()
	measured := make(map[string]measuredMetrics, len(providers))
	for _, provider := range providers {
		tps, latency := providerMeasuredMetrics(snap.MetricsRecords, m.model, provider, now)
		measured[provider] = measuredMetrics{throughput: tps, latency: latency}
	}
	cutoff := now.Add(-measuredMetricsTTL)
	records := make(map[string][]requestRecord, len(providers))
	for _, record := range snapshotWindowRecords(snap) {
		if record.Model == m.model && (record.Time.IsZero() || !record.Time.Before(cutoff)) {
			records[record.Provider] = append(records[record.Provider], record)
		}
	}

	blocked := m.routing.blockedProviders(m.model)
	scores := computeScores(providers, endpointByTag, measured)
	applyProviderQualityScores(providers, m.quality[m.model], scores)
	applyMeasuredErrorScores(providers, records, scores)
	pinned, pinnedManual := m.routing.pinInfo(m.model)

	values := make(map[string]providerSortValues, len(providers))
	for _, provider := range providers {
		_, isBlocked := blocked[provider]
		values[provider] = m.providerSortRow(provider, endpointByTag, measured, records, isBlocked)
	}
	return providerTableData{
		providers:    providers,
		endpoints:    endpointByTag,
		measured:     measured,
		records:      records,
		scores:       scores,
		blocked:      blocked,
		pinned:       pinned,
		pinnedManual: pinnedManual,
		sortValues:   values,
	}
}

// ordered returns the rows in the dashboard's display order, keeping the
// routing order between rows whose values tie, blanks included.
func (d providerTableData) ordered(column providerSortColumn) []string {
	if column == providerSortRoute {
		return d.providers
	}
	ordered := append([]string(nil), d.providers...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return d.sortValues[ordered[i]].sortKey(column) < d.sortValues[ordered[j]].sortKey(column)
	})
	return ordered
}

// selected returns the provider the highlight is on in the dashboard's display
// order, which is the row a keystroke or a projection is about.
func (d providerTableData) selected(column providerSortColumn, selection int) (string, bool) {
	ordered := d.ordered(column)
	if selection < 0 || selection >= len(ordered) {
		return "", false
	}
	return ordered[selection], true
}

// providerSortRow reads one row's ordering values from the data the table
// renders, including the fresh 't' measurement that replaces mTp and mLat.
func (m dashboardModel) providerSortRow(provider string, endpoints map[string]endpointMeta, measured map[string]measuredMetrics, records map[string][]requestRecord, blocked bool) providerSortValues {
	values := providerSortValues{
		in:             sortValueMissing,
		out:            sortValueMissing,
		cache:          sortValueMissing,
		throughput:     sortValueMissing,
		latency:        sortValueMissing,
		measThroughput: sortValueMissing,
		measLatency:    sortValueMissing,
		gpqa:           sortValueMissing,
		tau:            sortValueMissing,
		toolError:      sortValueMissing,
		jsonError:      sortValueMissing,
		apiError:       sortValueMissing,
		measToolError:  sortValueMissing,
	}
	if endpoint, ok := endpoints[provider]; ok {
		values.in = reportedValue(endpoint.Pricing.Prompt.float())
		values.out = reportedValue(endpoint.Pricing.Completion.float())
		values.cache = reportedValue(endpoint.Pricing.InputCacheRead.float())
		values.throughput = reportedValue(endpoint.Throughput)
		values.latency = reportedValue(endpoint.Latency)
	}
	if metrics, ok := measured[provider]; ok {
		values.measThroughput = reportedValue(metrics.throughput)
		values.measLatency = reportedValue(metrics.latency)
	}
	if result, fresh := m.testResultFor(m.model, provider); fresh && result.err == nil {
		if result.tps > 0 {
			values.measThroughput = result.tps
		}
		values.measLatency = reportedValue(float64(result.ttft.Milliseconds()))
	}
	if blocked {
		// The table blanks these cells: measurements of an endpoint the account
		// cannot route to no longer describe an available route.
		values.measThroughput, values.measLatency = sortValueMissing, sortValueMissing
	}
	quality := m.quality[m.model][provider]
	if validQualityValue(quality.GPQA, 1) {
		values.gpqa = *quality.GPQA
	}
	if validQualityValue(quality.Tau, 1) {
		values.tau = *quality.Tau
	}
	if validQualityValue(quality.ToolError, 100) {
		values.toolError = *quality.ToolError
	}
	if validQualityValue(quality.StructuredError, 100) {
		values.jsonError = *quality.StructuredError
	}
	apiRate, toolRate := groupErrorRateValues(records[provider])
	if apiRate >= 0 {
		values.apiError = apiRate
	}
	if toolRate >= 0 {
		values.measToolError = toolRate
	}
	return values
}

// metricLabel heads a metric line in the compact layout, in the dim label
// colour the wide layout heads its columns with. The column ordering the rows is
// pointed at in the accent the panel's keys use, so the marked name reads as a
// state of the table rather than a second heading.
func (m dashboardModel) metricLabel(label string) string {
	if m.providerSort != providerSortRoute && m.providerSort.label() == label {
		return keyHintStyle.Render(label)
	}
	return dimStyle.Render(label)
}

func (m dashboardModel) renderProviderTable(snap statsSnapshot, width, height int) string {
	return m.renderProviderTableData(m.providerTableData(snap), width, height)
}

func (m dashboardModel) renderProviderTableData(data providerTableData, width, height int) string {
	if len(data.providers) == 0 && len(data.endpoints) == 0 {
		return dimStyle.Render(shorten("waiting for model data", width))
	}

	header := providerTableHeader()

	providers := data.ordered(m.providerSort)
	endpointByTag := data.endpoints
	measured := data.measured
	records := data.records
	scores := data.scores
	blocked := data.blocked
	pinned, pinnedManual := data.pinned, data.pinnedManual
	styleCell := func(value string, score float64) string {
		if score < 0 {
			return value
		}
		return lipgloss.NewStyle().Foreground(gradientColor(displayContrastScore(score))).Render(value)
	}
	// Keep room for a readable provider label, every metric header, and common
	// numeric values. Narrow panes retain every metric in a multi-line layout.
	minimumWidth := 20 + len(header) - 1
	for _, label := range header[1:] {
		minimumWidth += max(6, lipgloss.Width(label))
	}
	if width < max(110, minimumWidth) {
		return m.renderCompactProviderTable(providers, endpointByTag, measured, records, scores, blocked, pinned, pinnedManual, m.selection, width, height, styleCell)
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
			mLat = float64(r.ttft.Milliseconds())
		}
		if isBlocked {
			// An account-policy refusal makes the endpoint unroutable regardless
			// of older successes, so those measurements no longer describe an
			// available route. Ordinary API errors remain a separate reliability
			// signal and must not erase successful measurements.
			mToks, mLat = 0, 0
			s.measThroughput, s.measLatency = noScore, noScore
		}
		apiErr, toolErr := coloredGroupErrorRates(records[provider], s.apiError, s.toolError)
		quality := providerQualityCells(m.quality[m.model][provider])
		quality[0] = styleCell(quality[0], s.gpqa)
		quality[1] = styleCell(quality[1], s.tau)
		quality[2] = styleQualityError(quality[2], s.orToolError)
		quality[3] = styleQualityError(quality[3], s.orJSONError)
		observed := []string{quality[0], quality[1], quality[2], quality[3], apiErr, toolErr}
		if ok {
			cells = append(cells,
				styleCell(pricePerM(ep.Pricing.Prompt), s.prompt),
				styleCell(pricePerM(ep.Pricing.Completion), s.completion),
				styleCell(pricePerM(ep.Pricing.InputCacheRead), s.cacheRead),
				styleCell(formatThroughput(ep.Throughput), s.throughput),
				styleCell(formatThroughput(mToks), s.measThroughput),
				styleCell(formatLatency(ep.Latency), s.latency),
				styleCell(formatLatency(mLat), s.measLatency),
			)
		} else {
			cells = append(cells,
				"", "", "", "",
				styleCell(formatThroughput(mToks), s.measThroughput),
				"",
				styleCell(formatLatency(mLat), s.measLatency),
			)
		}
		cells = append(cells, observed...)
		rows = append(rows, tableRow{cells: cells, selected: i == m.selection, blocked: isBlocked})
	}

	allCells := make([][]string, len(rows))
	for i := range rows {
		allCells[i] = rows[i].cells
	}
	columnWidths := providerTableColumnWidths(allCells, width)
	lines := make([]string, 0, len(rows))
	for i, row := range rows {
		line := renderProviderTableRow(row.cells, columnWidths, width)
		if i == 0 {
			line = m.renderProviderHeaderRow(row.cells, columnWidths, width)
		}
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
	return renderProviderTableWindow(lines[0], lines[1:], m.selection, height)
}

// renderProviderHeaderRow heads the table in the dim label colour, with the
// column ordering the rows pointed at in the accent the panel's keys use.
func (m dashboardModel) renderProviderHeaderRow(names []string, columnWidths []int, maxWidth int) string {
	sorted := m.providerSort.headerIndex()
	line := ""
	for i, name := range names {
		cellWidth := 1
		if i < len(columnWidths) {
			cellWidth = columnWidths[i]
		}
		cell := padRight(shorten(name, cellWidth), cellWidth)
		if i == sorted {
			cell = keyHintStyle.Render(cell)
		} else {
			cell = dimStyle.Render(cell)
		}
		if i > 0 {
			line += " "
		}
		line += cell
	}
	return shorten(line, maxWidth)
}

func (m dashboardModel) renderCompactProviderTable(providers []string, endpoints map[string]endpointMeta, measured map[string]measuredMetrics, records map[string][]requestRecord, scores map[string]providerScores, blocked map[string]blockedProvider, pinned string, pinnedManual bool, selection, width, height int, styleCell func(string, float64) string) string {
	rows := make([]string, 0, len(providers))
	for i, provider := range providers {
		var lines []string
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
			mLat = float64(r.ttft.Milliseconds())
		}
		if isBlocked {
			mToks, mLat = 0, 0
			s.measThroughput, s.measLatency = noScore, noScore
		}
		apiErr, toolErr := coloredGroupErrorRates(records[provider], s.apiError, s.toolError)
		quality := providerQualityCells(m.quality[m.model][provider])
		quality[0] = styleCell(quality[0], s.gpqa)
		quality[1] = styleCell(quality[1], s.tau)
		quality[2] = styleQualityError(quality[2], s.orToolError)
		quality[3] = styleQualityError(quality[3], s.orJSONError)
		pricing := []string{
			m.metricLabel("In") + " " + styleCell(pricePerM(ep.Pricing.Prompt), s.prompt),
			m.metricLabel("Out") + " " + styleCell(pricePerM(ep.Pricing.Completion), s.completion),
			m.metricLabel("Cache") + " " + styleCell(pricePerM(ep.Pricing.InputCacheRead), s.cacheRead),
		}
		performance := []string{
			m.metricLabel("Tp") + " " + styleCell(formatThroughput(ep.Throughput), s.throughput),
			m.metricLabel("mTp") + " " + styleCell(formatThroughput(mToks), s.measThroughput),
			m.metricLabel("Lat") + " " + styleCell(formatLatency(ep.Latency), s.latency),
			m.metricLabel("mLat") + " " + styleCell(formatLatency(mLat), s.measLatency),
			m.metricLabel("GPQA") + " " + quality[0],
			m.metricLabel("Tau") + " " + quality[1],
		}
		if width < 60 {
			observed := []string{m.metricLabel("GPQA") + " " + quality[0], m.metricLabel("Tau") + " " + quality[1]}
			lines = append(lines, ansi.Wrap(strings.Join(pricing, " "), max(1, width), ""))
			lines = append(lines, ansi.Wrap(strings.Join(performance[:4], " "), max(1, width), ""))
			lines = append(lines, ansi.Wrap(strings.Join(observed, " "), max(1, width), ""))
		} else {
			lines = append(lines, ansi.Wrap(strings.Join(pricing, " "), max(1, width), ""))
			lines = append(lines, ansi.Wrap(strings.Join(performance, " "), max(1, width), ""))
		}
		lines = append(lines, ansi.Wrap(m.metricLabel("Tool")+" "+quality[2]+" "+m.metricLabel("Json")+" "+quality[3], max(1, width), ""))
		lines = append(lines, ansi.Wrap(m.metricLabel("mApi")+" "+apiErr+" "+m.metricLabel("mTool")+" "+toolErr, max(1, width), ""))
		rows = append(rows, strings.Join(lines, "\n"))
	}
	return renderProviderTableWindow(titleStyle.Render("Provider metrics"), rows, selection, height)
}

// Keep the header and the selected provider's metrics in view as navigation
// moves beyond the pane. Compact rows can have different heights after wrapping.
func renderProviderTableWindow(header string, rows []string, selection, height int) string {
	if height <= 0 {
		return ""
	}
	first := 0
	if selection >= 0 && selection < len(rows) {
		budget := max(0, height-lipgloss.Height(header))
		used := lipgloss.Height(rows[selection])
		for i := 0; i < selection; i++ {
			used += lipgloss.Height(rows[i])
		}
		if used > budget {
			first = selection
			used = lipgloss.Height(rows[selection])
			for first > 0 && used+lipgloss.Height(rows[first-1]) <= budget {
				first--
				used += lipgloss.Height(rows[first])
			}
		}
	}
	content := header
	if len(rows) > 0 {
		content += "\n" + strings.Join(rows[first:], "\n")
	}
	return shortenLines(content, height)
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
		desired[i] = min(max(10, widths[i]), max(widths[i], desired[i]))
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

// padCenter centers value in width. An odd cell of slack goes to the right.
func padCenter(value string, width int) string {
	w := lipgloss.Width(value)
	if w >= width {
		return value
	}
	left := (width - w) / 2
	return strings.Repeat(" ", left) + value + strings.Repeat(" ", width-w-left)
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

// displayMonthSpend reports the figure the budget row compares with the key's
// cap. A polled usage for the current billing month is what that cap actually
// counted, so it wins outright: local accounting also sums traffic the proxy
// carried under other keys, which the cap never saw. Local traffic is only the
// fallback for a key that has not answered a poll. The month field is the flag
// for that answer — updateKeyInfo sets it on every poll that carries data — so
// a key that has spent nothing this cycle reports the polled zero instead of
// the proxy's own traffic from before the reset.
func displayMonthSpend(snap statsSnapshot, now time.Time) float64 {
	if snap.CreditMonth == now.Local().Format("2006-01") {
		return snap.CreditSpend
	}
	return monthSpend(snap.DailyCosts, now)
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

// groupHeaderLine renders the title and column headers of a grouped table in
// the dim label colour: the names head the table's columns, so they recede the
// way every other heading does and leave the full brightness to the values.
func groupHeaderLine(title string, labelWidth int, dataWidths []int, headers []string) string {
	line := padRight(shorten(title, labelWidth), labelWidth)
	for i, h := range headers {
		line += " " + padRight(shorten(h, dataWidths[i]), dataWidths[i])
	}
	return dimStyle.Render(line)
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

// Hide zero and missing rates; shade each measured error column independently.
func coloredGroupErrorRates(records []requestRecord, apiScore, toolScore float64) (api, tools string) {
	api, tools = groupErrorRates(records)
	if api == "0.0%" || api == "" {
		api = ""
	} else {
		api = styleQualityError(api, apiScore)
	}
	if tools == "0.0%" || tools == "" {
		tools = ""
	} else {
		tools = styleQualityError(tools, toolScore)
	}
	return api, tools
}

// groupErrorRates returns the API and tool error rates across records.
func groupErrorRates(records []requestRecord) (api, tools string) {
	apiRate, toolRate := groupErrorRateValues(records)
	return formatRate(apiRate), formatRate(toolRate)
}

func groupErrorRateValues(records []requestRecord) (api, tools float64) {
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
		return noScore, noScore
	}
	api = float64(apiErrs) / float64(total) * 100
	tools = noScore
	if toolReqs > 0 {
		tools = float64(toolErrs) / float64(toolReqs) * 100
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

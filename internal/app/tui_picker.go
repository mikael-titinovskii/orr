package app

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// pickerCatalogTTL bounds how long the picker reuses the catalog it fetched
// before opening again refetches it. The catalog changes rarely, and every
// open must stay instant once it is in memory.
const pickerCatalogTTL = 5 * time.Minute

// pickerMaxRows caps the visible list so the modal leaves the dashboard
// readable around it on a tall terminal.
const pickerMaxRows = 14

// pickerChromeHeight is the number of modal rows outside the model list: the
// title, a blank line, the search field, its status line, the footer, one more
// blank line, and the two border rows.
const pickerChromeHeight = 8

// modelPicker is the `k` modal's state: whether it is open, the query typed
// into it, the cursor within the matching models, and the catalog it searches.
// The catalog is fetched on the first open and reused until it goes stale, so
// a model without any request is still findable by id or name.
type modelPicker struct {
	open      bool
	query     string
	cursor    int
	catalog   []catalogModel
	fetchedAt time.Time
	loading   bool
	err       error
}

func (m *dashboardModel) openPicker() {
	m.picker.open = true
	m.picker.query = ""
	m.picker.cursor = 0
}

// pickerCmd fetches the upstream catalog when it is missing or stale. The
// modal opens on the models orr already knows and fills the catalog in when it
// arrives, so `k` never waits on the network.
func (m *dashboardModel) pickerCmd() tea.Cmd {
	if m.fetchCatalog == nil || m.picker.loading {
		return nil
	}
	if len(m.picker.catalog) > 0 && m.routing.now().Sub(m.picker.fetchedAt) < pickerCatalogTTL {
		return nil
	}
	m.picker.loading = true
	m.picker.err = nil
	fetch := m.fetchCatalog
	return func() tea.Msg {
		models, err := fetch()
		return modelCatalogMsg{models: models, err: err}
	}
}

// updatePicker drives the modal: typing narrows the list, ↑/↓ move the cursor,
// Enter opens the highlighted model, and Esc leaves without choosing. Every
// other key belongs to the query, so `q` types instead of quitting.
func (m *dashboardModel) updatePicker(msg tea.KeyMsg) (dashboardModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return *m, tea.Quit
	case "esc":
		m.picker.open = false
	case "up":
		m.movePicker(-1)
	case "down":
		m.movePicker(1)
	case "pgup":
		m.movePicker(-pickerMaxRows)
	case "pgdown":
		m.movePicker(pickerMaxRows)
	case "backspace":
		if runes := []rune(m.picker.query); len(runes) > 0 {
			m.picker.query = string(runes[:len(runes)-1])
			m.picker.cursor = 0
		}
	case "enter":
		rows := m.pickerRows()
		if m.picker.cursor < 0 || m.picker.cursor >= len(rows) {
			break
		}
		model := rows[m.picker.cursor].ID
		m.picker.open = false
		if err := m.pickModel(model); err != nil {
			m.serverErr = err
			return *m, tea.Quit
		}
	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			m.picker.query += string(msg.Runes)
			m.picker.cursor = 0
		}
	}
	return *m, nil
}

func (m *dashboardModel) movePicker(delta int) {
	rows := len(m.pickerRows())
	if rows == 0 {
		m.picker.cursor = 0
		return
	}
	// Normalize after the addition, not before: a page jump longer than the
	// list would otherwise leave a negative cursor that Enter would index with.
	m.picker.cursor = ((m.picker.cursor+delta)%rows + rows) % rows
}

// knownModels lists the models orr already knows, in the order the tab list
// prefers them: watched models, then models seen in the request log, then the
// rest of the stored routing configuration. They head the picker even before a
// catalog arrives.
func (m dashboardModel) knownModels() []string {
	models := make([]string, 0, len(m.watchedModels)+len(m.activeModels)+8)
	seen := make(map[string]bool)
	add := func(model string) {
		if model == "" || seen[model] {
			return
		}
		seen[model] = true
		models = append(models, model)
	}
	for _, model := range m.watchedModels {
		add(model)
	}
	for _, model := range m.activeModels {
		add(model)
	}
	configured := make([]string, 0, 8)
	for model := range m.routing.modelsSnapshot() {
		configured = append(configured, model)
	}
	sort.Strings(configured)
	for _, model := range configured {
		add(model)
	}
	add(m.routing.current())
	return models
}

// pickerMatchRank scores model against a lowercased query: ids that start with
// it first, then ids that mention it, then display names, so the canonical id
// decides the order whenever it matches. A negative rank means the model does
// not match.
func pickerMatchRank(model catalogModel, query string) int {
	if query == "" {
		return 0
	}
	id := strings.ToLower(model.ID)
	name := strings.ToLower(model.Name)
	switch {
	case strings.HasPrefix(id, query):
		return 0
	case strings.Contains(id, query):
		return 1
	case strings.HasPrefix(name, query):
		return 2
	case strings.Contains(name, query):
		return 3
	}
	return -1
}

// pickerRows returns the models the modal offers for the current query. The
// candidates are the models orr knows plus everything in the fetched catalog,
// matched case-insensitively by id and display name. Known models keep their
// own order ahead of catalog-only ones at equal rank, so an empty query lists
// what the dashboard is already using before the rest of OpenRouter.
func (m dashboardModel) pickerRows() []catalogModel {
	query := strings.ToLower(strings.TrimSpace(m.picker.query))
	names := make(map[string]string, len(m.picker.catalog))
	for _, model := range m.picker.catalog {
		names[model.ID] = model.Name
	}
	known := m.knownModels()
	knownIndex := make(map[string]int, len(known))
	for i, model := range known {
		knownIndex[model] = i
	}
	rows := make([]catalogModel, 0, len(known)+len(m.picker.catalog))
	rank := make(map[string]int, cap(rows))
	seen := make(map[string]bool, cap(rows))
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		model := catalogModel{ID: id, Name: names[id]}
		matched := pickerMatchRank(model, query)
		if matched < 0 {
			return
		}
		seen[id] = true
		rank[id] = matched
		rows = append(rows, model)
	}
	for _, id := range known {
		add(id)
	}
	for _, model := range m.picker.catalog {
		add(model.ID)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rank[rows[i].ID] != rank[rows[j].ID] {
			return rank[rows[i].ID] < rank[rows[j].ID]
		}
		knownI, iKnown := knownIndex[rows[i].ID]
		knownJ, jKnown := knownIndex[rows[j].ID]
		switch {
		case iKnown && jKnown:
			return knownI < knownJ
		case iKnown != jKnown:
			return iKnown
		}
		return rows[i].ID < rows[j].ID
	})
	return rows
}

// pickModel switches the panel to model and keeps it on the tab list from now
// on. The model becomes a stored routing entry the way a first request for it
// would, and its compatible providers are fetched when it has none yet, so a
// model nothing has requested still shows a table to pin.
func (m *dashboardModel) pickModel(model string) error {
	added, err := m.routing.ensureModel(model)
	if err != nil {
		return err
	}
	delete(m.closedModels, model)
	if !containsString(m.watchedModels, model) {
		m.watchedModels = append(m.watchedModels, model)
	}
	m.model = model
	m.selectActiveProvider(m.currentSnap())
	if m.refresh != nil && (added || len(m.providers()) == 0) {
		m.refresh(model)
	}
	m.syncModel()
	return nil
}

// closeTab removes the displayed model from the tab list and moves the panel to
// the next one. A model with traffic still in flight comes back when that
// request completes.
func (m *dashboardModel) closeTab() {
	if m.model == "" {
		return
	}
	closed := m.model
	for i, watched := range m.watchedModels {
		if watched == closed {
			m.watchedModels = append(m.watchedModels[:i], m.watchedModels[i+1:]...)
			break
		}
	}
	m.closedModels[closed] = m.routing.now()
	m.model = ""
	m.syncModel()
}

// renderPicker draws the `k` modal: the search field, the models matching it,
// and the keys that drive them.
func (m dashboardModel) renderPicker(width, height int) string {
	rows := m.pickerRows()
	contentWidth := max(16, min(72, width-10))
	// The list takes only the rows it has, so a narrow query does not leave the
	// modal padded with blank space, and it is capped so a long result set and a
	// short terminal both stay inside the screen.
	listHeight := min(max(1, min(len(rows), pickerMaxRows)), max(0, height-pickerChromeHeight))
	cursor := min(max(0, m.picker.cursor), max(0, len(rows)-1))

	lines := []string{
		titleStyle.Render("Find model"),
		"",
		dimStyle.Render("Search  ") + shorten(m.picker.query, max(1, contentWidth-8)) + keyHintStyle.Render("▏"),
		m.pickerStatus(),
	}
	switch {
	case listHeight == 0:
	case len(rows) == 0:
		lines = append(lines, dimStyle.Render("no model matches"))
	default:
		first := 0
		if cursor >= listHeight {
			first = cursor - listHeight + 1
		}
		for i := first; i < len(rows) && i-first < listHeight; i++ {
			lines = append(lines, renderPickerRow(rows[i], i == cursor, contentWidth))
		}
	}
	lines = append(lines, "", renderPickerFooter(cursor, len(rows), contentWidth))
	return panelStyle.Width(contentWidth).Render(strings.Join(lines, "\n"))
}

// pickerStatus explains where the list came from while it is still arriving or
// when the catalog could not be fetched.
func (m dashboardModel) pickerStatus() string {
	switch {
	case m.picker.loading:
		return dimStyle.Render("loading the OpenRouter catalog…")
	case m.picker.err != nil:
		return errorStyle.Render("catalog unavailable — showing models orr knows")
	}
	return ""
}

func renderPickerRow(model catalogModel, selected bool, width int) string {
	mark := "  "
	if selected {
		mark = "> "
	}
	line := padRight(mark+shorten(model.ID, max(1, width-lipgloss.Width(mark))), width)
	if selected {
		return applyRowStyle(line, selectionStyle)
	}
	return line
}

// renderPickerFooter puts the cursor's position and the modal's keys on one dim
// line, the way the panes label their own controls.
func renderPickerFooter(cursor, total, width int) string {
	position := ""
	if total > 0 {
		position = fmt.Sprintf("%d/%d  ", min(cursor, total-1)+1, total)
	}
	hints := []keyHint{{"↑/↓", "move"}, {"Enter", "select"}, {"Esc", "cancel"}}
	return dimStyle.Render(position) + renderKeyHints(hints, max(1, width-lipgloss.Width(position)))
}

// overlayCenter centers box over base. Both are ANSI-styled multi-line
// strings: each box row replaces the base cells it covers, and the base keeps
// its own content and styling on both sides of the box. A box wider than the
// terminal is clipped, so a tiny window cannot wrap it.
func overlayCenter(base, box string, width, height int) string {
	lines := strings.Split(base, "\n")
	boxLines := strings.Split(box, "\n")
	boxWidth := 0
	for _, line := range boxLines {
		boxWidth = max(boxWidth, lipgloss.Width(line))
	}
	boxWidth = min(boxWidth, max(0, width))
	left := max(0, (width-boxWidth)/2)
	top := max(0, (height-len(boxLines))/2)
	for len(lines) < top+len(boxLines) {
		lines = append(lines, "")
	}
	for i, line := range boxLines {
		row := top + i
		baseLine := lines[row]
		prefix := padRight(ansi.Cut(baseLine, 0, left), left)
		tail := ansi.Cut(baseLine, left+boxWidth, max(left+boxWidth, width))
		right := max(0, width-left-boxWidth-lipgloss.Width(tail))
		lines[row] = prefix + padRight(ansi.Truncate(line, boxWidth, ""), boxWidth) + tail + strings.Repeat(" ", right)
	}
	return strings.Join(lines, "\n")
}

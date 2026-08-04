package chat

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/samsaffron/term-llm/internal/mentions"
)

const (
	mentionQueryDebounce = 30 * time.Millisecond
	mentionMatchLimit    = 50
	mentionDisplayLimit  = 8
)

type mentionQueryMode uint8

const (
	mentionQueryGeneral mentionQueryMode = iota
	mentionQueryAgentsOnly
	mentionQueryFilesOnly
)

type agentMentionMatch struct {
	name      string
	positions []int
}

type mentionPopupModel struct {
	visible        bool
	token          mentions.ActiveToken
	matches        []mentions.Match
	agentMatches   []agentMentionMatch
	mode           mentionQueryMode
	agentErr       error
	matchesRoot    string
	matchesToken   mentions.ActiveToken
	matchesCursor  int
	matchesGen     uint64
	matchesRequest uint64
	selected       int
	indexing       bool
	refreshing     bool
	searching      bool
	truncated      bool
	err            error
}

func (p *mentionPopupModel) selectableCount() int {
	if p == nil {
		return 0
	}
	return len(p.agentMatches) + len(p.matches)
}

func routeMentionQuery(token mentions.ActiveToken) (mentionQueryMode, string, string) {
	if token.Agent {
		return mentionQueryAgentsOnly, "", token.Query
	}
	if token.Quoted || strings.HasPrefix(token.Query, "./") || strings.HasPrefix(token.Query, "../") || strings.ContainsAny(token.Query, `/\\`) {
		return mentionQueryFilesOnly, token.Query, ""
	}
	return mentionQueryGeneral, token.Query, token.Query
}

type rankedAgentMention struct {
	agentMentionMatch
	rank int
}

func rankAgentMentionMatches(capability AgentMentionCapability, query string, limit int) ([]agentMentionMatch, error) {
	if capability == nil {
		return nil, errors.New("agent mentions are unavailable because spawn_agent is not enabled in this session")
	}
	names, err := capability.PermittedAgentNames()
	if err != nil {
		return nil, err
	}
	lowerQuery := strings.ToLower(query)
	ranked := make([]rankedAgentMention, 0, len(names))
	for _, name := range names {
		lowerName := strings.ToLower(name)
		rank := 4
		var positions []int
		switch {
		case lowerQuery == "":
		case lowerName == lowerQuery:
			rank = 0
			positions = contiguousBytePositions(name, 0, len(name))
		case strings.HasPrefix(lowerName, lowerQuery):
			rank = 1
			positions = contiguousBytePositions(name, 0, len(lowerQuery))
		case strings.Contains(lowerName, lowerQuery):
			rank = 2
			start := strings.Index(lowerName, lowerQuery)
			positions = contiguousBytePositions(name, start, start+len(lowerQuery))
		default:
			var ok bool
			positions, ok = agentSubsequencePositions(name, lowerQuery)
			if !ok {
				continue
			}
			rank = 3
		}
		ranked = append(ranked, rankedAgentMention{agentMentionMatch: agentMentionMatch{name: name, positions: positions}, rank: rank})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].rank != ranked[j].rank {
			return ranked[i].rank < ranked[j].rank
		}
		leftLen := utf8.RuneCountInString(ranked[i].name)
		rightLen := utf8.RuneCountInString(ranked[j].name)
		if leftLen != rightLen {
			return leftLen < rightLen
		}
		return ranked[i].name < ranked[j].name
	})
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	matches := make([]agentMentionMatch, len(ranked))
	for i := range ranked {
		matches[i] = ranked[i].agentMentionMatch
	}
	return matches, nil
}

func contiguousBytePositions(value string, start, end int) []int {
	if start < 0 || end <= start || start >= len(value) {
		return nil
	}
	end = min(end, len(value))
	positions := make([]int, 0, end-start)
	for i := start; i < end; i++ {
		positions = append(positions, i)
	}
	return positions
}

func agentSubsequencePositions(name, lowerQuery string) ([]int, bool) {
	if lowerQuery == "" {
		return nil, true
	}
	queryRunes := []rune(lowerQuery)
	queryIndex := 0
	positions := make([]int, 0, len(queryRunes))
	for offset, r := range name {
		if queryIndex < len(queryRunes) && unicode.ToLower(r) == queryRunes[queryIndex] {
			positions = append(positions, offset)
			queryIndex++
		}
	}
	return positions, queryIndex == len(queryRunes)
}

func (p *mentionPopupModel) IsVisible() bool { return p != nil && p.visible }
func (p *mentionPopupModel) invalidateMatchContext() {
	if p == nil {
		return
	}
	p.matchesRoot = ""
	p.matchesToken = mentions.ActiveToken{}
	p.matchesCursor = 0
	p.matchesGen = 0
	p.matchesRequest = 0
}

func (p *mentionPopupModel) clearMatches() {
	if p == nil {
		return
	}
	p.matches = nil
	p.agentMatches = nil
	p.agentErr = nil
	p.invalidateMatchContext()
	p.selected = 0
}
func (p *mentionPopupModel) Hide() {
	if p == nil {
		return
	}
	p.visible = false
	p.clearMatches()
	p.err = nil
}

type mentionIndexReadyMsg struct {
	root       string
	generation uint64
	snapshot   *mentions.Snapshot
	err        error
}
type mentionDebounceMsg struct {
	root                string
	generation, request uint64
	token               mentions.ActiveToken
	cursor              int
}
type mentionMatchesMsg struct {
	root                string
	generation, request uint64
	token               mentions.ActiveToken
	cursor              int
	mode                mentionQueryMode
	matches             []mentions.Match
	agentMatches        []agentMentionMatch
	agentErr            error
}

func (m *Model) initializeMentions() {
	m.agentMentionEnabled = mentions.EnabledFromEnv()
	m.mentionEnabled = m.agentMentionEnabled
	if !m.mentionEnabled {
		return
	}
	root, err := mentionAbsoluteRoot(m.effectiveWorkingDir())
	if err != nil {
		m.mentionEnabled = false
		m.mentionPopup.err = err
		return
	}
	m.mentionRoot = root
}

func mentionAbsoluteRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", errors.New("project directory is unavailable")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve project directory: %w", err)
	}
	return filepath.Clean(abs), nil
}

func (m *Model) startMentionIndex() tea.Cmd {
	if m == nil || !m.mentionEnabled {
		return nil
	}
	if m.mentionBuildCancel != nil {
		m.mentionBuildCancel()
	}
	root, err := mentionAbsoluteRoot(m.effectiveWorkingDir())
	if err != nil {
		m.mentionIndexGeneration++
		generation := m.mentionIndexGeneration
		m.mentionRoot = ""
		return func() tea.Msg { return mentionIndexReadyMsg{generation: generation, err: err} }
	}
	ctx, cancel := context.WithCancel(m.rootContext())
	m.mentionBuildCancel = cancel
	m.mentionIndexGeneration++
	generation := m.mentionIndexGeneration
	m.mentionRoot = root
	m.mentionPopup.indexing = m.mentionIndex == nil
	m.mentionPopup.refreshing = m.mentionIndex != nil
	return func() tea.Msg {
		snapshot, err := mentions.Build(ctx, root, mentions.DefaultBuildOptions())
		return mentionIndexReadyMsg{root: root, generation: generation, snapshot: snapshot, err: err}
	}
}

func (m *Model) handleMentionMessage(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case mentionIndexReadyMsg:
		if msg.root != m.mentionRoot || msg.generation != m.mentionIndexGeneration {
			return true, nil
		}
		if m.mentionBuildCancel != nil {
			m.mentionBuildCancel()
			m.mentionBuildCancel = nil
		}
		m.mentionPopup.indexing = false
		m.mentionPopup.refreshing = false
		if msg.err != nil {
			if m.mentionIndex == nil {
				m.mentionPopup.err = msg.err
			}
			return true, nil
		}
		m.mentionIndex = msg.snapshot
		m.mentionPopup.clearMatches()
		m.mentionPopup.searching = m.mentionPopup.visible
		m.mentionPopup.truncated = msg.snapshot.Truncated
		m.mentionPopup.err = nil
		if m.mentionPopup.visible {
			return true, m.scheduleMentionQuery(m.mentionPopup.token)
		}
		return true, nil
	case mentionDebounceMsg:
		if !m.mentionQueryCurrent(msg.root, msg.generation, msg.request, msg.token, msg.cursor) {
			return true, nil
		}
		mode, fileQuery, agentQuery := routeMentionQuery(msg.token)
		snapshot := m.mentionIndex
		capability := m.agentMentionCapability
		ctx := m.mentionQueryCtx
		return true, func() tea.Msg {
			var matches []mentions.Match
			if mode != mentionQueryAgentsOnly && snapshot != nil {
				matches = snapshot.Search(ctx, fileQuery, mentionMatchLimit)
			}
			var agentMatches []agentMentionMatch
			var agentErr error
			if mode != mentionQueryFilesOnly {
				agentMatches, agentErr = rankAgentMentionMatches(capability, agentQuery, mentionMatchLimit)
			}
			return mentionMatchesMsg{
				root: msg.root, generation: msg.generation, request: msg.request,
				token: msg.token, cursor: msg.cursor, mode: mode, matches: matches,
				agentMatches: agentMatches, agentErr: agentErr,
			}
		}
	case mentionMatchesMsg:
		if !m.mentionQueryCurrent(msg.root, msg.generation, msg.request, msg.token, msg.cursor) {
			return true, nil
		}
		if m.mentionQueryCancel != nil {
			m.mentionQueryCancel()
			m.mentionQueryCancel = nil
		}
		m.mentionPopup.searching = false
		m.mentionPopup.matches = msg.matches
		m.mentionPopup.agentMatches = msg.agentMatches
		m.mentionPopup.mode = msg.mode
		m.mentionPopup.agentErr = msg.agentErr
		m.mentionPopup.matchesRoot = msg.root
		m.mentionPopup.matchesToken = msg.token
		m.mentionPopup.matchesCursor = msg.cursor
		m.mentionPopup.matchesGen = msg.generation
		m.mentionPopup.matchesRequest = msg.request
		// Result replacement can change both row identity and category offsets.
		// Never carry an index from the old result set into the new one.
		m.mentionPopup.selected = 0
		return true, nil
	default:
		return false, nil
	}
}

func (m *Model) mentionQueryCurrent(root string, generation, request uint64, token mentions.ActiveToken, cursor int) bool {
	if !m.mentionPopup.visible || root != m.mentionRoot || generation != m.mentionIndexGeneration || request != m.mentionQueryRequest {
		return false
	}
	currentCursor := textareaCursorByteOffset(m.textarea.Value(), m.textarea.Line(), m.textarea.Column())
	current, ok := mentions.ActiveTokenAt(m.textarea.Value(), currentCursor)
	return ok && current == token && currentCursor == cursor
}

func (m *Model) updateMentionQuery() tea.Cmd {
	if m == nil || !m.mentionEnabled || strings.HasPrefix(m.textarea.Value(), "/") || m.dialog.IsOpen() {
		m.hideMentionPopup()
		return nil
	}
	cursor := textareaCursorByteOffset(m.textarea.Value(), m.textarea.Line(), m.textarea.Column())
	token, ok := mentions.ActiveTokenAt(m.textarea.Value(), cursor)
	if !ok {
		m.hideMentionPopup()
		return nil
	}
	m.completions.Hide()
	m.mentionPopup.visible = true
	m.mentionPopup.token = token
	mode, _, _ := routeMentionQuery(token)
	m.mentionPopup.mode = mode
	// Keep the previous rows in place while the debounced query runs. Clearing
	// them here collapses the popup to its one-line searching state on every
	// keystroke, then expands it again when results arrive, which visibly flashes
	// in both inline and alt-screen rendering. The context is invalidated so the
	// retained rows cannot be accepted for the new token.
	m.mentionPopup.invalidateMatchContext()
	m.mentionPopup.searching = true
	needsFiles := mode != mentionQueryAgentsOnly
	m.mentionPopup.indexing = needsFiles && m.mentionIndex == nil
	var refresh tea.Cmd
	if needsFiles && m.mentionIndex == nil && m.mentionBuildCancel == nil {
		refresh = m.startMentionIndex()
	} else if needsFiles && m.mentionIndex != nil && m.mentionBuildCancel == nil && time.Since(m.mentionIndex.BuiltAt) >= 10*time.Second {
		refresh = m.startMentionIndex()
	}
	return tea.Batch(refresh, m.scheduleMentionQuery(token))
}

func (m *Model) scheduleMentionQuery(token mentions.ActiveToken) tea.Cmd {
	if m.mentionQueryCancel != nil {
		m.mentionQueryCancel()
	}
	m.mentionQueryRequest++
	request := m.mentionQueryRequest
	ctx, cancel := context.WithCancel(m.rootContext())
	m.mentionQueryCtx, m.mentionQueryCancel = ctx, cancel
	root, generation := m.mentionRoot, m.mentionIndexGeneration
	cursor := textareaCursorByteOffset(m.textarea.Value(), m.textarea.Line(), m.textarea.Column())
	return func() tea.Msg {
		timer := time.NewTimer(mentionQueryDebounce)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return mentionDebounceMsg{root: root, generation: generation, request: request, token: token, cursor: cursor}
		}
	}
}

func (m *Model) hideMentionPopup() {
	m.mentionPopup.Hide()
	if m.mentionQueryCancel != nil {
		m.mentionQueryCancel()
		m.mentionQueryCancel = nil
	}
}

func (m *Model) handleMentionPopupKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	if !m.mentionPopup.IsVisible() {
		return false, nil
	}
	switch msg.String() {
	case "esc":
		m.hideMentionPopup()
		return true, nil
	case "up", "ctrl+p":
		if count := m.mentionPopup.selectableCount(); count > 0 {
			m.mentionPopup.selected = (m.mentionPopup.selected - 1 + count) % count
		}
		return true, nil
	case "down", "ctrl+n":
		if count := m.mentionPopup.selectableCount(); count > 0 {
			m.mentionPopup.selected = (m.mentionPopup.selected + 1) % count
		}
		return true, nil
	case "enter":
		if m.mentionPopup.token.Query == "" {
			return false, nil
		}
		accepted, cmd := m.acceptMentionSelection()
		if !accepted {
			m.hideMentionPopup()
			return false, cmd
		}
		return true, cmd
	case "tab":
		accepted, cmd := m.acceptMentionSelection()
		if !accepted {
			m.hideMentionPopup()
		}
		return true, cmd
	}
	return false, nil
}

func (m *Model) acceptMentionSelection() (bool, tea.Cmd) {
	count := m.mentionPopup.selectableCount()
	if count == 0 || m.mentionPopup.selected < 0 || m.mentionPopup.selected >= count {
		return false, nil
	}
	cursor := textareaCursorByteOffset(m.textarea.Value(), m.textarea.Line(), m.textarea.Column())
	current, ok := mentions.ActiveTokenAt(m.textarea.Value(), cursor)
	if !ok || current != m.mentionPopup.token || current != m.mentionPopup.matchesToken ||
		cursor != m.mentionPopup.matchesCursor || m.mentionPopup.matchesRoot != m.mentionRoot ||
		m.mentionPopup.matchesGen != m.mentionIndexGeneration || m.mentionPopup.matchesRequest != m.mentionQueryRequest {
		return false, nil
	}

	inserted := ""
	if m.mentionPopup.selected < len(m.mentionPopup.agentMatches) {
		target := m.mentionPopup.agentMatches[m.mentionPopup.selected]
		if m.agentMentionCapability == nil {
			_, cmd := m.showFooterError("agent mention selection is stale because spawn_agent is no longer available")
			return false, cmd
		}
		if err := m.agentMentionCapability.ValidateAgentMention(target.name); err != nil {
			_, cmd := m.showFooterError(fmt.Sprintf("cannot select %s: %v", mentions.InsertAgentText(target.name), err))
			return false, cmd
		}
		inserted = mentions.InsertAgentText(target.name)
	} else {
		fileSelection := m.mentionPopup.selected - len(m.mentionPopup.agentMatches)
		if m.mentionIndex == nil || fileSelection < 0 || fileSelection >= len(m.mentionPopup.matches) {
			return false, nil
		}
		match := m.mentionPopup.matches[fileSelection]
		if match.Candidate < 0 || match.Candidate >= len(m.mentionIndex.Candidates) {
			return false, nil
		}
		target := m.mentionIndex.Candidates[match.Candidate]
		inserted = mentions.InsertText(target.Path, target.Kind == mentions.KindDirectory)
	}

	token := current
	old := m.textarea.Value()
	if token.Start < 0 || token.End < token.Start || token.End > len(old) || old[token.Start:token.End] == "" {
		return false, nil
	}
	suffix := old[token.End:]
	separator := " "
	if suffix != "" {
		if r, _ := utf8.DecodeRuneInString(suffix); unicode.IsSpace(r) {
			separator = ""
		}
	}
	newValue := old[:token.Start] + inserted + separator + suffix
	m.textarea.SetValue(newValue)
	m.moveTextareaCursorToByteOffset(token.Start + len(inserted) + len(separator))
	m.updateTextareaHeight()
	m.hideMentionPopup()
	return true, nil
}

type mentionDisplayEntry struct {
	header      string
	agent       *agentMentionMatch
	file        *mentions.Match
	selectIndex int
}

func (m *Model) mentionDisplayEntries() []mentionDisplayEntry {
	entries := make([]mentionDisplayEntry, 0, m.mentionPopup.selectableCount()+2)
	if len(m.mentionPopup.agentMatches) > 0 {
		entries = append(entries, mentionDisplayEntry{header: "AGENTS", selectIndex: -1})
		for i := range m.mentionPopup.agentMatches {
			entries = append(entries, mentionDisplayEntry{agent: &m.mentionPopup.agentMatches[i], selectIndex: i})
		}
	}
	if len(m.mentionPopup.matches) > 0 {
		entries = append(entries, mentionDisplayEntry{header: "FILES & DIRECTORIES", selectIndex: -1})
		for i := range m.mentionPopup.matches {
			selection := len(m.mentionPopup.agentMatches) + i
			entries = append(entries, mentionDisplayEntry{file: &m.mentionPopup.matches[i], selectIndex: selection})
		}
	}
	return entries
}

func (m *Model) renderMentionPopup() string {
	if !m.mentionPopup.IsVisible() {
		return ""
	}
	theme := m.styles.Theme()
	muted := lipgloss.NewStyle().Foreground(theme.Muted)
	selected := lipgloss.NewStyle().Foreground(theme.Primary).Bold(true)
	width := max(24, min(m.width-2, 88))
	popupStyle := lipgloss.NewStyle().Width(width).MaxWidth(width).Border(lipgloss.RoundedBorder()).BorderForeground(theme.Muted)
	contentWidth := max(1, width-popupStyle.GetHorizontalFrameSize())
	var rows []string
	entries := m.mentionDisplayEntries()
	if len(entries) > 0 {
		selectedEntry := 0
		for i := range entries {
			if entries[i].selectIndex == m.mentionPopup.selected {
				selectedEntry = i
				break
			}
		}
		start := max(0, selectedEntry-mentionDisplayLimit+1)
		end := min(len(entries), start+mentionDisplayLimit)
		for _, entry := range entries[start:end] {
			if entry.header != "" {
				headerStyle := muted.Bold(true)
				rows = append(rows, headerStyle.Render("  "+entry.header))
				continue
			}
			prefix := "  "
			baseStyle := muted
			if entry.selectIndex == m.mentionPopup.selected {
				prefix = "› "
				baseStyle = selected
			}
			if entry.agent != nil {
				display := mentions.InsertAgentText(entry.agent.name) + "  agent"
				rows = append(rows, baseStyle.Render(prefix+truncateMentionRow(display, max(4, contentWidth-lipgloss.Width(prefix)))))
				continue
			}
			if entry.file == nil || m.mentionIndex == nil || entry.file.Candidate < 0 || entry.file.Candidate >= len(m.mentionIndex.Candidates) {
				continue
			}
			candidate := m.mentionIndex.Candidates[entry.file.Candidate]
			displayPath := candidate.Path
			if candidate.Kind == mentions.KindDirectory && !strings.HasSuffix(displayPath, "/") {
				displayPath += "/"
			}
			available := max(4, contentWidth-lipgloss.Width(prefix))
			path, positions := truncateMentionPath(displayPath, entry.file.Positions, available)
			row := baseStyle.Render(prefix) + highlightMentionPath(path, positions, baseStyle, selected.Underline(true))
			rows = append(rows, row)
		}
	} else {
		switch {
		case m.mentionPopup.mode == mentionQueryAgentsOnly && m.mentionPopup.agentErr != nil:
			rows = append(rows, muted.Render("  agent completion unavailable: "+m.mentionPopup.agentErr.Error()))
		case m.mentionPopup.mode == mentionQueryAgentsOnly && m.mentionPopup.searching:
			rows = append(rows, muted.Render("  searching permitted agents…"))
		case m.mentionPopup.mode == mentionQueryAgentsOnly:
			rows = append(rows, muted.Render("  no matching permitted agents"))
		case m.mentionPopup.indexing:
			rows = append(rows, muted.Render("  indexing project files…"))
		case m.mentionPopup.err != nil:
			rows = append(rows, muted.Render("  file index unavailable: "+m.mentionPopup.err.Error()))
		case m.mentionPopup.searching:
			rows = append(rows, muted.Render("  searching agents and project files…"))
		default:
			rows = append(rows, muted.Render("  no matching agents or project files"))
		}
	}
	// Reserve the full result area from the first visible frame. Indexing,
	// searching, and short result sets otherwise make the bottom-anchored popup
	// grow upward as rows arrive.
	for len(rows) < mentionDisplayLimit {
		rows = append(rows, "")
	}
	status := "↑↓ navigate  enter/tab select  esc close"
	rows = append(rows, muted.Render(truncateMentionRow("  "+status, contentWidth)))
	return popupStyle.Render(strings.Join(rows, "\n"))
}

func highlightMentionPath(path string, positions []int, normal, highlight lipgloss.Style) string {
	hits := make(map[int]bool, len(positions))
	for _, position := range positions {
		hits[position] = true
	}
	var output, run strings.Builder
	highlighted := false
	flush := func() {
		if run.Len() == 0 {
			return
		}
		style := normal
		if highlighted {
			style = highlight
		}
		output.WriteString(style.Render(run.String()))
		run.Reset()
	}
	first := true
	for offset, r := range path {
		hit := false
		for i := 0; i < utf8.RuneLen(r); i++ {
			if hits[offset+i] {
				hit = true
				break
			}
		}
		if !first && hit != highlighted {
			flush()
		}
		first = false
		highlighted = hit
		run.WriteRune(r)
	}
	flush()
	return output.String()
}

func truncateMentionPath(value string, positions []int, width int) (string, []int) {
	if lipgloss.Width(value) <= width {
		return value, positions
	}
	runes := []rune(value)
	startRune := len(runes)
	for startRune > 0 && lipgloss.Width("…"+string(runes[startRune-1:])) <= width {
		startRune--
	}
	if startRune < len(runes) && lipgloss.Width("…"+string(runes[startRune:])) > width {
		startRune++
	}
	if startRune >= len(runes) {
		return "…", nil
	}
	byteStart := len(string(runes[:startRune]))
	display := "…" + string(runes[startRune:])
	adjusted := make([]int, 0, len(positions))
	for _, position := range positions {
		if position >= byteStart {
			adjusted = append(adjusted, len("…")+position-byteStart)
		}
	}
	return display, adjusted
}

func truncateMentionRow(value string, width int) string {
	if lipgloss.Width(value) <= width {
		return value
	}
	runes := []rune(value)
	for len(runes) > 1 && lipgloss.Width(string(runes))+1 > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

func (m *Model) resetMentionsForRoot(dir string) {
	if !m.mentionEnabled {
		return
	}
	if m.mentionBuildCancel != nil {
		m.mentionBuildCancel()
		m.mentionBuildCancel = nil
	}
	if m.mentionQueryCancel != nil {
		m.mentionQueryCancel()
		m.mentionQueryCancel = nil
	}
	m.hideMentionPopup()
	m.mentionIndex = nil
	m.mentionRoot = ""
	m.mentionPopup.indexing = false
	m.mentionPopup.refreshing = false
	root, err := mentionAbsoluteRoot(dir)
	if err != nil {
		m.mentionEnabled = false
		m.mentionPopup.err = err
		return
	}
	m.mentionRoot = root
}

func (m *Model) eagerMentionContext(content string) (string, []string) {
	if m == nil || !m.mentionEnabled {
		return "", nil
	}
	root := m.mentionRoot
	if root == "" {
		root = m.effectiveWorkingDir()
	}
	var allowed func(string) bool
	if m.toolMgr != nil {
		allowed = m.toolMgr.IsReadPathApproved
	}
	ctx, cancel := context.WithTimeout(m.rootContext(), time.Second)
	defer cancel()
	attachments := mentions.LoadEagerAttachments(ctx, root, content, allowed)
	labels := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		labels = append(labels, attachment.Path)
	}
	return mentions.FormatEagerAttachments(attachments), labels
}

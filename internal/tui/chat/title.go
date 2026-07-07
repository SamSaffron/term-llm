package chat

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/sessiontitle"
)

// TerminalTitleMode controls whether and how chat updates terminal/window titles.
type TerminalTitleMode string

const (
	TerminalTitleSmart TerminalTitleMode = "smart"
	TerminalTitleBasic TerminalTitleMode = "basic"
	TerminalTitleOff   TerminalTitleMode = "off"
)

const (
	terminalTitleMaxRunes       = 120
	liveTitleGenerationMaxTries = 2
	liveTitleFallbackDelay      = 30 * time.Second
)

type titleFallbackTickMsg struct {
	sessionID string
}

// ParseTerminalTitleMode normalizes a chat.terminal_title config value.
// Empty values default to smart. The bool is false only for unknown non-empty values.
func ParseTerminalTitleMode(raw string) (TerminalTitleMode, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(TerminalTitleSmart):
		return TerminalTitleSmart, true
	case string(TerminalTitleBasic):
		return TerminalTitleBasic, true
	case string(TerminalTitleOff):
		return TerminalTitleOff, true
	default:
		return TerminalTitleSmart, false
	}
}

type titleState struct {
	Attention bool
	Agent     string
	Task      string
	Model     string
	Phase     string
	Streaming bool
	Elapsed   time.Duration
}

func buildTitle(st titleState) string {
	agent := titleSegment(st.Agent)
	task := titleSegment(st.Task)
	model := titleSegment(st.Model)

	if task == "" {
		task = "term-llm"
	}

	metaSegments := make([]string, 0, 2)
	if agent != "" {
		metaSegments = append(metaSegments, agent)
	}
	if model != "" {
		metaSegments = append(metaSegments, model)
	}

	prefix := ""
	if st.Attention {
		prefix = "‼ "
	}
	suffix := ""
	if len(metaSegments) > 0 {
		suffix = " · " + strings.Join(metaSegments, " · ")
	}
	return buildTitleWithElidedTask(prefix, task, suffix)
}

func buildTitleWithElidedTask(prefix, task, suffix string) string {
	full := strings.TrimSpace(prefix + task + suffix)
	if utf8.RuneCountInString(full) <= terminalTitleMaxRunes {
		return full
	}
	if suffix == "" {
		return limitTitleRunes(full, terminalTitleMaxRunes)
	}
	availableTaskRunes := terminalTitleMaxRunes - utf8.RuneCountInString(prefix) - utf8.RuneCountInString(suffix)
	if availableTaskRunes < 1 {
		return limitTitleRunes(full, terminalTitleMaxRunes)
	}
	task = limitTitleRunes(task, availableTaskRunes)
	return strings.TrimSpace(prefix + task + suffix)
}

func titleSegment(s string) string {
	return strings.Join(strings.Fields(sanitizeTerminalTitle(s)), " ")
}

func streamingTitleActivity(st titleState) string {
	if st.Elapsed > 0 {
		seconds := int(st.Elapsed.Round(time.Second) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		return fmt.Sprintf("%ds…", seconds)
	}
	phase := strings.TrimSuffix(titleSegment(st.Phase), "...")
	phase = strings.TrimSuffix(phase, "…")
	if phase != "" && !strings.EqualFold(phase, "thinking") {
		return strings.ToLower(phase) + "…"
	}
	return "…"
}

func limitTitleRunes(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	if maxRunes == 1 {
		return "…"
	}
	runes := []rune(s)
	truncated := strings.TrimSpace(string(runes[:maxRunes-1]))
	return truncated + "…"
}

func sanitizeTerminalTitle(title string) string {
	if title == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(title))
	for _, r := range title {
		if unicode.IsControl(r) {
			switch r {
			case '\n', '\r', '\t':
				b.WriteRune(' ')
			}
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(strings.Join(strings.Fields(b.String()), " "))
}

var (
	terminalTitleSimpleVarPattern = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	terminalTitleEnvDotPattern    = regexp.MustCompile(`\{\{\s*env\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
)

type terminalTitleFormatter struct {
	raw  string
	tmpl *template.Template
	env  TerminalTitleEnvironment
}

type terminalTitleFormatData struct {
	Title           string
	StableTitle     string
	Agent           string
	Task            string
	Model           string
	Phase           string
	State           string
	Activity        string
	Elapsed         string
	Attention       bool
	AttentionMarker string
	Streaming       bool
	Env             map[string]string
}

func newTerminalTitleFormatter(format string, env TerminalTitleEnvironment) *terminalTitleFormatter {
	format = strings.TrimSpace(format)
	formatter := &terminalTitleFormatter{raw: format, env: env}
	if format == "" {
		return formatter
	}
	tmpl, err := parseTerminalTitleFormat(format, env)
	if err != nil {
		return formatter
	}
	formatter.tmpl = tmpl
	return formatter
}

func ValidateTerminalTitleFormat(format string) error {
	format = strings.TrimSpace(format)
	if format == "" {
		return nil
	}
	tmpl, err := parseTerminalTitleFormat(format, TerminalTitleEnvironment{})
	if err != nil {
		return err
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, terminalTitleFormatDataForState(titleState{Agent: "agent", Task: "task", Model: "model"}, TerminalTitleEnvironment{})); err != nil {
		return fmt.Errorf("execute terminal title format: %w", err)
	}
	return nil
}

func parseTerminalTitleFormat(format string, env TerminalTitleEnvironment) (*template.Template, error) {
	funcs := template.FuncMap{
		"env": func(name string) string {
			return env.Get(name)
		},
		"default": func(fallback, value string) string {
			if strings.TrimSpace(value) == "" {
				return fallback
			}
			return value
		},
	}
	return template.New("terminal-title").Funcs(funcs).Option("missingkey=zero").Parse(normalizeTerminalTitleFormat(format))
}

func normalizeTerminalTitleFormat(format string) string {
	format = terminalTitleEnvDotPattern.ReplaceAllString(format, `{{env "$1"}}`)
	return terminalTitleSimpleVarPattern.ReplaceAllStringFunc(format, func(action string) string {
		matches := terminalTitleSimpleVarPattern.FindStringSubmatch(action)
		if len(matches) != 2 {
			return action
		}
		if field, ok := terminalTitleFormatField(matches[1]); ok {
			return "{{." + field + "}}"
		}
		return action
	})
}

func terminalTitleFormatField(name string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "title":
		return "Title", true
	case "stable_title", "stabletitle":
		return "StableTitle", true
	case "agent":
		return "Agent", true
	case "task":
		return "Task", true
	case "model":
		return "Model", true
	case "phase":
		return "Phase", true
	case "state":
		return "State", true
	case "activity":
		return "Activity", true
	case "elapsed":
		return "Elapsed", true
	case "attention":
		return "AttentionMarker", true
	case "attention_marker", "attentionmarker":
		return "AttentionMarker", true
	default:
		return "", false
	}
}

func (f *terminalTitleFormatter) Format(st titleState) string {
	defaultTitle := buildTitle(st)
	if f == nil || strings.TrimSpace(f.raw) == "" || f.tmpl == nil {
		return defaultTitle
	}
	data := terminalTitleFormatDataForState(st, f.env)
	var b bytes.Buffer
	if err := f.tmpl.Execute(&b, data); err != nil {
		return defaultTitle
	}
	formatted := sanitizeTerminalTitle(b.String())
	if formatted == "" {
		return defaultTitle
	}
	return limitTitleRunes(formatted, terminalTitleMaxRunes)
}

func terminalTitleFormatDataForState(st titleState, env TerminalTitleEnvironment) terminalTitleFormatData {
	agent := titleSegment(st.Agent)
	task := titleSegment(st.Task)
	if task == "" {
		task = "term-llm"
	}
	model := titleSegment(st.Model)
	phase := titleSegment(st.Phase)
	state := "idle"
	activity := model
	attentionMarker := ""
	if st.Attention {
		state = "attention"
		activity = "attention"
		attentionMarker = "‼"
	} else if st.Streaming {
		state = "streaming"
		activity = streamingTitleActivity(st)
	}
	elapsed := ""
	if st.Elapsed > 0 {
		seconds := int(st.Elapsed.Round(time.Second) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		elapsed = fmt.Sprintf("%ds", seconds)
	}
	stable := st
	stable.Elapsed = 0
	return terminalTitleFormatData{
		Title:           buildTitle(st),
		StableTitle:     buildTitle(stable),
		Agent:           agent,
		Task:            task,
		Model:           model,
		Phase:           phase,
		State:           state,
		Activity:        activity,
		Elapsed:         elapsed,
		Attention:       st.Attention,
		AttentionMarker: attentionMarker,
		Streaming:       st.Streaming,
		Env:             env.Values(),
	}
}

type terminalTitleSnapshot struct {
	Title       string
	StableTitle string
	InProgress  bool
}

type terminalTitleProvider interface {
	UpdateCmd(snapshot terminalTitleSnapshot) tea.Cmd
	HandleMsg(msg tea.Msg, snapshot terminalTitleSnapshot) (bool, tea.Cmd)
	Restore()
}

type terminalTitleProviderFactory func(mode TerminalTitleMode, env TerminalTitleEnvironment, progress bool) []terminalTitleProvider

var terminalTitleProviderFactories []terminalTitleProviderFactory

func registerTerminalTitleProviderFactory(factory terminalTitleProviderFactory) {
	if factory == nil {
		return
	}
	terminalTitleProviderFactories = append(terminalTitleProviderFactories, factory)
}

type terminalTitleManager struct {
	providers []terminalTitleProvider
}

func newTerminalTitleManager(mode TerminalTitleMode, env TerminalTitleEnvironment, progress bool) *terminalTitleManager {
	if mode == "" {
		mode = TerminalTitleSmart
	}
	if mode == TerminalTitleOff {
		return &terminalTitleManager{}
	}

	providers := []terminalTitleProvider{&oscTitleProvider{}}
	if mode == TerminalTitleSmart {
		for _, factory := range terminalTitleProviderFactories {
			providers = append(providers, factory(mode, env, progress)...)
		}
	}
	return &terminalTitleManager{providers: providers}
}

func (mgr *terminalTitleManager) UpdateCmd(snapshot terminalTitleSnapshot) tea.Cmd {
	if mgr == nil || len(mgr.providers) == 0 {
		return nil
	}
	var cmds []tea.Cmd
	for _, provider := range mgr.providers {
		if provider == nil {
			continue
		}
		if cmd := provider.UpdateCmd(snapshot); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return batchTerminalTitleCmds(cmds)
}

func (mgr *terminalTitleManager) HandleMsg(msg tea.Msg, snapshot terminalTitleSnapshot) (bool, tea.Cmd) {
	if mgr == nil || len(mgr.providers) == 0 {
		return false, nil
	}
	for _, provider := range mgr.providers {
		if provider == nil {
			continue
		}
		if handled, cmd := provider.HandleMsg(msg, snapshot); handled {
			return true, cmd
		}
	}
	return false, nil
}

func (mgr *terminalTitleManager) Restore() {
	if mgr == nil {
		return
	}
	for _, provider := range mgr.providers {
		if provider != nil {
			provider.Restore()
		}
	}
}

func batchTerminalTitleCmds(cmds []tea.Cmd) tea.Cmd {
	switch len(cmds) {
	case 0:
		return nil
	case 1:
		return cmds[0]
	default:
		return tea.Batch(cmds...)
	}
}

type oscTitleProvider struct {
	lastSent string
}

func (p *oscTitleProvider) UpdateCmd(snapshot terminalTitleSnapshot) tea.Cmd {
	if p == nil {
		return nil
	}
	title := sanitizeTerminalTitle(snapshot.Title)
	if title == "" || title == p.lastSent {
		return nil
	}
	p.lastSent = title
	return tea.Raw(oscTitleSequence(title))
}

func (p *oscTitleProvider) HandleMsg(tea.Msg, terminalTitleSnapshot) (bool, tea.Cmd) {
	return false, nil
}

func (p *oscTitleProvider) Restore() {}

func oscTitleSequence(title string) string {
	title = sanitizeTerminalTitle(title)
	if title == "" {
		return ""
	}
	// OSC 2 sets the window/surface title. Ghostty's own shell integration uses
	// this form, and xterm-compatible terminals commonly support it.
	return "\x1b]2;" + title + "\x07"
}

func (m *Model) terminalTitleCmd() tea.Cmd {
	if m == nil || m.titleManager == nil {
		return nil
	}
	return m.titleManager.UpdateCmd(m.currentTerminalTitleSnapshot())
}

func (m *Model) handleTerminalTitleProviderMsg(msg tea.Msg) (bool, tea.Cmd) {
	if m == nil || m.titleManager == nil {
		return false, nil
	}
	return m.titleManager.HandleMsg(msg, m.currentTerminalTitleSnapshot())
}

func (m *Model) withTerminalTitleCmd(cmd tea.Cmd) tea.Cmd {
	titleCmd := m.terminalTitleCmd()
	if cmd == nil {
		return titleCmd
	}
	if titleCmd == nil {
		return cmd
	}
	return tea.Batch(cmd, titleCmd)
}

func (m *Model) appendTerminalTitleCmd(cmds *[]tea.Cmd) {
	if cmd := m.terminalTitleCmd(); cmd != nil {
		*cmds = append(*cmds, cmd)
	}
}

func (m *Model) ConfigureTerminalTitleEnvironment(env TerminalTitleEnvironment) {
	if m == nil {
		return
	}
	m.titleFormatter = newTerminalTitleFormatter(m.titleFormat, env)
	m.titleManager = newTerminalTitleManager(m.titleMode, env, m.titleProgress)
}

func (m *Model) RestoreTerminalTitle() {
	if m == nil || m.titleManager == nil {
		return
	}
	m.titleManager.Restore()
}

func (m *Model) currentTerminalTitleSnapshot() terminalTitleSnapshot {
	if m == nil {
		return terminalTitleSnapshot{Title: "term-llm", StableTitle: "term-llm"}
	}
	return terminalTitleSnapshot{
		Title:       m.getTerminalTitle(),
		StableTitle: m.getStableTerminalTitle(),
		InProgress:  m.streaming,
	}
}

func (m *Model) getTerminalTitle() string {
	if m == nil {
		return "term-llm"
	}
	st := m.currentTitleState(true)
	if m.titleFormatter != nil {
		return m.titleFormatter.Format(st)
	}
	return buildTitle(st)
}

func (m *Model) getStableTerminalTitle() string {
	if m == nil {
		return "term-llm"
	}
	st := m.currentTitleState(false)
	if m.titleFormatter != nil {
		return m.titleFormatter.Format(st)
	}
	return buildTitle(st)
}

func (m *Model) currentTitleState(includeElapsed bool) titleState {
	st := titleState{
		Attention: m.titleNeedsAttention(),
		Agent:     m.agentName,
		Model:     shortenModelName(m.displayModelName()),
		Phase:     m.phase,
		Streaming: m.streaming,
	}
	if st.Model == "" {
		st.Model = shortenModelName(m.modelName)
	}
	if m.sess != nil {
		st.Task = m.sess.PreferredShortTitle()
	}
	if includeElapsed && m.streaming && !m.streamStartTime.IsZero() {
		st.Elapsed = time.Since(m.streamStartTime)
	}
	return st
}

func (m *Model) titleNeedsAttention() bool {
	return m != nil && (m.approvalModel != nil || m.askUserModel != nil || m.handoverPreview != nil)
}

func (m *Model) resetTitleGenerationStateForSession() {
	if m == nil {
		return
	}
	sessionID := ""
	if m.sess != nil {
		sessionID = m.sess.ID
	}
	m.titleGenerationSessionID = sessionID
	m.titleGenerationAttempts = 0
	m.titleGenerationLastMessageCount = 0
	m.titleGenerationInFlight = false
}

func (m *Model) scheduleTitleFallbackCmd() tea.Cmd {
	if m == nil || m.sess == nil || m.fastProvider == nil || m.store == nil {
		return nil
	}
	if strings.TrimSpace(m.sess.GeneratedShortTitle) != "" {
		return nil
	}
	sessionID := strings.TrimSpace(m.sess.ID)
	if sessionID == "" {
		return nil
	}
	return tea.Tick(liveTitleFallbackDelay, func(time.Time) tea.Msg {
		return titleFallbackTickMsg{sessionID: sessionID}
	})
}

func (m *Model) maybeGenerateSessionTitleCmd() tea.Cmd {
	return m.generateSessionTitleCmd(false, false, 0)
}

func (m *Model) generateSessionTitleCmd(force bool, clearManualName bool, manualEditVersion uint64) tea.Cmd {
	if m == nil || m.sess == nil || m.fastProvider == nil || m.store == nil {
		return nil
	}
	sessionID := strings.TrimSpace(m.sess.ID)
	if sessionID == "" {
		return nil
	}
	if m.titleGenerationSessionID != sessionID {
		m.resetTitleGenerationStateForSession()
	}
	if m.titleGenerationInFlight {
		return nil
	}
	if !force && strings.TrimSpace(m.sess.GeneratedShortTitle) != "" {
		return nil
	}
	if !force && m.titleGenerationAttempts >= liveTitleGenerationMaxTries {
		return nil
	}

	m.messagesMu.Lock()
	messages := append([]session.Message(nil), m.messages...)
	m.messagesMu.Unlock()
	if len(messages) == 0 {
		return nil
	}
	if !force && m.titleGenerationAttempts > 0 && len(messages) <= m.titleGenerationLastMessageCount {
		return nil
	}

	provider := m.fastProvider
	store := m.store
	sessCopy := *m.sess
	rootCtx := m.rootContext()
	basisSeq := messages[len(messages)-1].Sequence
	messageCount := len(messages)

	m.titleGenerationInFlight = true
	m.titleGenerationAttempts++
	m.titleGenerationLastMessageCount = messageCount

	return func() tea.Msg {
		cand, err := sessiontitle.Generate(rootCtx, provider, &sessCopy, messages)
		if err != nil {
			return titleGeneratedMsg{sessionID: sessionID, err: err, force: force, clearManualName: clearManualName, manualEditVersion: manualEditVersion}
		}
		generatedAt := time.Now().UTC()
		if !force {
			if err := session.UpdateGeneratedTitle(rootCtx, store, &sessCopy, cand.ShortTitle, cand.LongTitle, generatedAt, basisSeq); err != nil {
				return titleGeneratedMsg{sessionID: sessionID, err: err}
			}
		}
		return titleGeneratedMsg{
			sessionID:         sessionID,
			candidate:         cand,
			generatedAt:       generatedAt,
			basisMsgSeq:       basisSeq,
			force:             force,
			clearManualName:   clearManualName,
			manualEditVersion: manualEditVersion,
		}
	}
}

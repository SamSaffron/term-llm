package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/agents/gist"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/samsaffron/term-llm/internal/worktree"
)

func TestAllCommandsIncludesCompact(t *testing.T) {
	commands := AllCommands()
	found := false
	for _, cmd := range commands {
		if cmd.Name == "compact" {
			found = true
			if cmd.Usage != "/compact [hard]" {
				t.Errorf("compact usage = %q, want /compact [hard]", cmd.Usage)
			}
			if len(cmd.Subcommands) == 0 {
				t.Errorf("compact should expose subcommands for completion")
			}
			break
		}
	}
	if !found {
		t.Error("AllCommands() should include 'compact' command")
	}
}

func TestFilterCommandsMatchesCompact(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{"compact", true},
		{"comp", true},
		{"compa", true},
		{"xyz", false},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			results := FilterCommands(tt.query)
			found := false
			for _, cmd := range results {
				if cmd.Name == "compact" {
					found = true
					break
				}
			}
			if found != tt.want {
				t.Errorf("FilterCommands(%q) found compact = %v, want %v", tt.query, found, tt.want)
			}
		})
	}
}

func TestAllCommandsIncludesTitleCommands(t *testing.T) {
	want := map[string]string{
		"title":     "/title <name>",
		"autotitle": "/autotitle",
	}
	for _, cmd := range AllCommands() {
		if usage, ok := want[cmd.Name]; ok {
			if cmd.Usage != usage {
				t.Fatalf("%s usage = %q, want %q", cmd.Name, cmd.Usage, usage)
			}
			delete(want, cmd.Name)
		}
	}
	for name := range want {
		t.Fatalf("AllCommands() should include %q command", name)
	}
}

func TestAllCommandsIncludesStats(t *testing.T) {
	for _, cmd := range AllCommands() {
		if cmd.Name == "stats" {
			if cmd.Usage != "/stats" {
				t.Fatalf("stats usage = %q, want /stats", cmd.Usage)
			}
			return
		}
	}
	t.Fatal("AllCommands() should include 'stats' command")
}

func TestFilterCommandsShortAliasAlsoShowsPrefixMatches(t *testing.T) {
	results := FilterCommands("/sh")
	for _, want := range []string{"share", "shell"} {
		if !slices.ContainsFunc(results, func(cmd Command) bool { return cmd.Name == want }) {
			t.Fatalf("FilterCommands(/sh) missing %q: %+v", want, results)
		}
	}
}

func TestFilterCommandsMatchesStats(t *testing.T) {
	for _, query := range []string{"stats", "stat", "st"} {
		t.Run(query, func(t *testing.T) {
			results := FilterCommands(query)
			for _, cmd := range results {
				if cmd.Name == "stats" {
					return
				}
			}
			t.Fatalf("FilterCommands(%q) did not include stats", query)
		})
	}
}

func TestAllCommandsRemovesLoadAndKeepsResume(t *testing.T) {
	commands := AllCommands()

	hasLoad := false
	hasResume := false
	for _, cmd := range commands {
		if cmd.Name == "load" {
			hasLoad = true
		}
		if cmd.Name == "resume" {
			hasResume = true
		}
	}

	if hasLoad {
		t.Error("AllCommands() should not include 'load'")
	}
	if !hasResume {
		t.Error("AllCommands() should include 'resume'")
	}
}

func TestAllCommandsIncludesEffort(t *testing.T) {
	for _, cmd := range AllCommands() {
		if cmd.Name == "effort" {
			if cmd.Usage != "/effort [none|minimal|low|medium|high|xhigh|max|default]" {
				t.Fatalf("effort usage = %q", cmd.Usage)
			}
			return
		}
	}
	t.Fatal("AllCommands() should include 'effort' command")
}

func TestFilterCommandsMatchesEffort(t *testing.T) {
	for _, query := range []string{"effort", "eff"} {
		t.Run(query, func(t *testing.T) {
			results := FilterCommands(query)
			for _, cmd := range results {
				if cmd.Name == "effort" {
					return
				}
			}
			t.Fatalf("FilterCommands(%q) did not include effort", query)
		})
	}
}

func TestStreamingLocalSlashCommandIncludesEffort(t *testing.T) {
	if !isStreamingLocalSlashCommand("/effort high") {
		t.Fatal("expected /effort to be handled locally while streaming")
	}
}

// mockStore implements session.Store for testing resume behavior.
type mockStore struct {
	session.NoopStore
	sessions         map[string]*session.Session
	getErr           error
	messages         map[string][]session.Message
	summaries        []session.SessionSummary
	msgErr           error
	updated          *session.Session
	updateErr        error
	created          []*session.Session
	createErr        error
	added            []session.Message
	addErr           error
	messageUpdates   []session.Message
	updateMessageErr error
	currentID        string
	setCurrentErr    error
	deleted          []string
	deleteErr        error
	statusUpdates    []statusUpdate
	updateStatusErr  error
	compacted        []session.Message
	compactSession   string
	compactErr       error
}

type statusUpdate struct {
	id     string
	status session.SessionStatus
}

func (s *mockStore) Get(_ context.Context, id string) (*session.Session, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if sess, ok := s.sessions[id]; ok {
		return sess, nil
	}
	return nil, nil
}

func (s *mockStore) GetByPrefix(_ context.Context, prefix string) (*session.Session, error) {
	if sess, ok := s.sessions[prefix]; ok {
		return sess, nil
	}
	return nil, nil
}

func (s *mockStore) GetMessages(_ context.Context, sessionID string, _, _ int) ([]session.Message, error) {
	if s.msgErr != nil {
		return nil, s.msgErr
	}
	return s.messages[sessionID], nil
}

func (s *mockStore) GetMessagesFrom(_ context.Context, sessionID string, fromSeq, limit int) ([]session.Message, error) {
	if s.msgErr != nil {
		return nil, s.msgErr
	}
	var filtered []session.Message
	for _, msg := range s.messages[sessionID] {
		if msg.Sequence >= fromSeq {
			filtered = append(filtered, msg)
		}
	}
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered, nil
}

func (s *mockStore) List(_ context.Context, _ session.ListOptions) ([]session.SessionSummary, error) {
	return s.summaries, nil
}

func (s *mockStore) Update(_ context.Context, sess *session.Session) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.updated = sess
	return nil
}

func (s *mockStore) Create(_ context.Context, sess *session.Session) error {
	if s.createErr != nil {
		return s.createErr
	}
	if sess.ID == "" {
		sess.ID = session.NewID()
	}
	if s.sessions == nil {
		s.sessions = make(map[string]*session.Session)
	}
	cp := *sess
	s.created = append(s.created, &cp)
	s.sessions[sess.ID] = sess
	return nil
}

func (s *mockStore) ensureMessages() {
	if s.messages == nil {
		s.messages = make(map[string][]session.Message)
	}
}

func (s *mockStore) AddMessage(_ context.Context, sessionID string, msg *session.Message) error {
	if s.addErr != nil {
		return s.addErr
	}
	s.ensureMessages()
	msg.SessionID = sessionID
	if msg.Sequence < 0 {
		msg.Sequence = len(s.messages[sessionID])
	}
	cp := *msg
	s.added = append(s.added, cp)
	s.messages[sessionID] = append(s.messages[sessionID], cp)
	return nil
}

func (s *mockStore) UpdateMessage(_ context.Context, sessionID string, msg *session.Message) error {
	if s.updateMessageErr != nil {
		return s.updateMessageErr
	}
	s.ensureMessages()
	s.messageUpdates = append(s.messageUpdates, *msg)
	for i := range s.messages[sessionID] {
		if s.messages[sessionID][i].ID == msg.ID {
			s.messages[sessionID][i] = *msg
			return nil
		}
	}
	return session.ErrNotFound
}

func (s *mockStore) SetCurrent(_ context.Context, sessionID string) error {
	if s.setCurrentErr != nil {
		return s.setCurrentErr
	}
	s.currentID = sessionID
	return nil
}

func (s *mockStore) Delete(_ context.Context, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, id)
	if s.sessions != nil {
		delete(s.sessions, id)
	}
	delete(s.messages, id)
	return nil
}

func (s *mockStore) UpdateStatus(_ context.Context, id string, status session.SessionStatus) error {
	if s.updateStatusErr != nil {
		return s.updateStatusErr
	}
	s.statusUpdates = append(s.statusUpdates, statusUpdate{id: id, status: status})
	return nil
}

func (s *mockStore) CompactMessages(_ context.Context, sessionID string, messages []session.Message) error {
	if s.compactErr != nil {
		return s.compactErr
	}
	s.ensureMessages()
	startSeq := len(s.messages[sessionID])
	for i := range messages {
		messages[i].SessionID = sessionID
		messages[i].Sequence = startSeq + i
	}
	if sess := s.sessions[sessionID]; sess != nil {
		sess.CompactionSeq = startSeq
		sess.CompactionCount++
	}
	s.compactSession = sessionID
	s.compacted = append([]session.Message(nil), messages...)
	s.messages[sessionID] = append(s.messages[sessionID], messages...)
	return nil
}

func TestCmdHelpOpensModal(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	result, cmd := m.cmdHelp()
	if cmd != nil {
		t.Fatalf("cmdHelp returned unexpected command")
	}
	rm := result.(*Model)
	if !rm.dialog.IsOpen() || rm.dialog.Type() != DialogContent {
		t.Fatalf("help should open content dialog, got open=%v type=%v", rm.dialog.IsOpen(), rm.dialog.Type())
	}
	if !strings.Contains(rm.dialog.Content(), "Slash commands") || !strings.Contains(rm.dialog.Content(), "/stats") {
		t.Fatalf("help content missing expected commands: %q", rm.dialog.Content())
	}
	for _, want := range []string{
		"Ctrl+/",
		"Ctrl+H",
		"Show help",
		"Ctrl+R",
		"Cycle reasoning effort",
		"Ctrl+E",
		"Expand/collapse tool and reasoning details",
		"Ctrl+O",
		"Inspect conversation context",
		"Ctrl+Y",
		"Copy selected conversation text",
		"PageUp / PageDown",
		"Ctrl+J / Alt+Enter / Shift+Enter",
		"Pickers and completions",
		"Ctrl+T",
	} {
		if !strings.Contains(rm.dialog.Content(), want) {
			t.Fatalf("help content missing %q: %q", want, rm.dialog.Content())
		}
	}
}

func TestHelpShortcutCtrlShiftSlashOpensModalPreservesDraft(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.keyMap = DefaultKeyMap()
	m.completions = NewCompletionsModel(m.styles)
	m.setTextareaValue("draft prompt")

	result, cmd := m.handleKeyMsg(tea.KeyPressMsg{
		Code:        '/',
		ShiftedCode: '?',
		BaseCode:    '/',
		Mod:         tea.ModCtrl | tea.ModShift,
	})
	if cmd != nil {
		t.Fatalf("help shortcut returned unexpected command")
	}
	rm := result.(*Model)
	if !rm.dialog.IsOpen() || rm.dialog.Type() != DialogContent {
		t.Fatalf("help shortcut should open content dialog, got open=%v type=%v", rm.dialog.IsOpen(), rm.dialog.Type())
	}
	if !strings.Contains(rm.dialog.Content(), "Slash commands") || !strings.Contains(rm.dialog.Content(), "Ctrl+/") {
		t.Fatalf("help shortcut opened unexpected content: %q", rm.dialog.Content())
	}
	if got := rm.textarea.Value(); got != "draft prompt" {
		t.Fatalf("help shortcut should preserve composer draft, got %q", got)
	}
}

func TestHelpShortcutCtrlQuestionTextOpensModal(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.keyMap = DefaultKeyMap()
	m.completions = NewCompletionsModel(m.styles)

	result, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: '?', Text: "?", Mod: tea.ModCtrl | tea.ModShift})
	rm := result.(*Model)
	if !rm.dialog.IsOpen() || rm.dialog.Type() != DialogContent {
		t.Fatalf("ctrl+? should open help dialog, got open=%v type=%v", rm.dialog.IsOpen(), rm.dialog.Type())
	}
}

func TestHelpShortcutCtrlHOpensModal(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.keyMap = DefaultKeyMap()
	m.completions = NewCompletionsModel(m.styles)

	result, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
	rm := result.(*Model)
	if !rm.dialog.IsOpen() || rm.dialog.Type() != DialogContent {
		t.Fatalf("ctrl+h should open help dialog, got open=%v type=%v", rm.dialog.IsOpen(), rm.dialog.Type())
	}
}

func TestPlainQuestionDoesNotOpenHelp(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.keyMap = DefaultKeyMap()
	m.completions = NewCompletionsModel(m.styles)

	result, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: '?', Text: "?"})
	rm := result.(*Model)
	if rm.dialog.IsOpen() {
		t.Fatalf("plain ? should not open help dialog")
	}
}

func TestCmdStatsOpensModalWithTotalsAndCompactions(t *testing.T) {
	oldEstimator := statsCostEstimator
	statsCostEstimator = func(model string, stats *ui.SessionStats) (float64, error) { return 0.0123, nil }
	defer func() { statsCostEstimator = oldEstimator }()

	m := newCmdTestModel(&mockStore{})
	m.providerKey = "openai"
	m.providerName = "openai"
	m.modelName = "gpt-4o"
	m.sess = &session.Session{ID: "s1", UserTurns: 2, LLMTurns: 3, CompactionCount: 3, CompactionSeq: 25}
	m.stats = ui.NewSessionStats()
	m.stats.SeedTotals(1000, 500, 100, 50, 4, 3)
	m.stats.AddCompactionUsage(484, 1200, 200000, 0)
	m.messages = []session.Message{
		{Role: llm.RoleUser, TextContent: "hello"},
		{Role: llm.RoleAssistant, TextContent: "hi"},
	}

	result, cmd := m.cmdStats()
	if cmd != nil {
		t.Fatalf("cmdStats returned unexpected command")
	}
	rm := result.(*Model)
	if !rm.dialog.IsOpen() || rm.dialog.Type() != DialogContent {
		t.Fatalf("stats should open content dialog, got open=%v type=%v", rm.dialog.IsOpen(), rm.dialog.Type())
	}
	content := rm.dialog.Content()
	for _, want := range []string{"Stats:", "Current Context / Window Pressure", "Current context vs cumulative history", "Current context", "Cumulative history", "Cumulative Session Token Usage", "Fresh input tokens", "Cache write tokens", "Cache hit rate:", "Total tokens", "Cumulative Session Activity", "Tool calls", "Compactions:        3", "LLM cost:           200k cache, 484 in, 1.2k out", "Last boundary:"} {
		if !strings.Contains(content, want) {
			t.Fatalf("stats content missing %q:\n%s", want, content)
		}
	}
	if content := rm.dialog.Content(); strings.Contains(content, "billed-ish") || strings.Contains(content, "build-ish") {
		t.Fatalf("stats content should not use vague billed/build-ish label:\n%s", content)
	}
}

func TestCmdStatsUsesActiveContextAfterCompaction(t *testing.T) {
	oldEstimator := statsCostEstimator
	statsCostEstimator = func(model string, stats *ui.SessionStats) (float64, error) { return 0, fmt.Errorf("not tested") }
	defer func() { statsCostEstimator = oldEstimator }()

	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "s1", CompactionCount: 2, CompactionSeq: 10}
	m.stats = ui.NewSessionStats()
	m.messages = []session.Message{
		{Role: llm.RoleUser, TextContent: strings.Repeat("old ", 200)},
		{Role: llm.RoleAssistant, TextContent: strings.Repeat("old reply ", 200)},
		{Role: llm.RoleUser, TextContent: "summary"},
		{Role: llm.RoleAssistant, TextContent: "ack"},
	}
	m.compactionIdx = 2

	content := m.renderStatsModal()
	for _, want := range []string{
		"Compactions:        2",
		"Last boundary:      seq 10 (2 messages hidden from active context)",
		"Active messages:    2 (user 1, assistant 1, tool 0)",
		"Outside context:",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("stats content missing %q:\n%s", want, content)
		}
	}
}

func TestStatsSeedsCacheWriteTokensFromSession(t *testing.T) {
	oldEstimator := statsCostEstimator
	statsCostEstimator = func(model string, stats *ui.SessionStats) (float64, error) { return 0, fmt.Errorf("not tested") }
	defer func() { statsCostEstimator = oldEstimator }()

	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "s1", InputTokens: 100, CachedInputTokens: 20, CacheWriteTokens: 30, OutputTokens: 40}
	m.seedStatsFromSession()
	result, _ := m.cmdStats()
	content := result.(*Model).dialog.Content()
	if !strings.Contains(content, "Cache write tokens:") || !strings.Contains(content, "30") {
		t.Fatalf("stats did not include cache write tokens restored from session DB metrics:\n%s", content)
	}
	if !strings.Contains(content, "Total tokens:       190") {
		t.Fatalf("stats total did not include cache-write tokens:\n%s", content)
	}
}

func TestStatsModalUsesEngineThresholdsAndClearCacheDenominator(t *testing.T) {
	oldEstimator := statsCostEstimator
	estimatorCalls := 0
	statsCostEstimator = func(string, *ui.SessionStats) (float64, error) {
		estimatorCalls++
		if estimatorCalls == 1 {
			return 0.1234, nil
		}
		return 0, errors.New("raw pricing lookup detail")
	}
	t.Cleanup(func() { statsCostEstimator = oldEstimator })

	m := newCmdTestModel(&mockStore{})
	m.engine = llm.NewEngine(nil, nil)
	m.engine.SetCompaction(1000, llm.CompactionConfig{
		SoftThresholdRatio: 0.70,
		HardThresholdRatio: 0.85,
	})
	m.engine.SetContextEstimateBaseline(10, 0)
	m.stats = ui.NewSessionStats()
	m.stats.AddUsage(400, 100, 200, 200)

	content := m.renderStatsModal()
	for _, want := range []string{
		"Stats:",
		"$0.1234",
		"× Soft compact at:  700       70.0% (300 window buffer)",
		"! Hard compact at:  850       85.0% (150 window buffer)",
		"Cache hit rate:     25.0% (cache read / (fresh + read + write input))",
		"Estimated cost:     unavailable",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("stats content missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "raw pricing lookup detail") {
		t.Fatalf("stats exposed raw pricing error:\n%s", content)
	}
	// The modal may price its summary copy, but must not attach that cost to the
	// live stats object.
	if live := m.stats.Render(); strings.Contains(live, "$") {
		t.Fatalf("rendering /stats mutated live estimated cost: %s", live)
	}
}

func TestStatsModalCollapsesEqualCompactionThresholds(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.engine = llm.NewEngine(nil, nil)
	m.engine.SetCompaction(1000, llm.CompactionConfig{ThresholdRatio: 0.8})
	m.engine.SetContextEstimateBaseline(1, 0)
	content := m.renderStatsModal()
	if strings.Count(content, "compact at:") != 1 || !strings.Contains(content, "Soft compact at:") {
		t.Fatalf("equal thresholds should display once:\n%s", content)
	}
}

func TestStatsModalDoesNotFinalizeLiveTiming(t *testing.T) {
	oldEstimator := statsCostEstimator
	statsCostEstimator = func(string, *ui.SessionStats) (float64, error) {
		return 0, errors.New("unavailable")
	}
	t.Cleanup(func() { statsCostEstimator = oldEstimator })

	m := newCmdTestModel(&mockStore{})
	m.stats = ui.NewSessionStats()
	before := m.stats.LLMTime
	_ = m.renderStatsModal()
	if m.stats.LLMTime != before {
		t.Fatalf("rendering /stats finalized live timing: before=%v after=%v", before, m.stats.LLMTime)
	}
}

func TestStatsPricingModelStripsProviderAndEffort(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.modelName = "chatgpt:gpt-5.5-medium"
	if got := m.statsPricingModel(); got != "gpt-5.5" {
		t.Fatalf("statsPricingModel() = %q, want gpt-5.5", got)
	}
}

func TestCmdStatsHandlesNoUsage(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "s1"}
	result, _ := m.cmdStats()
	content := result.(*Model).dialog.Content()
	if !strings.Contains(content, "No token usage recorded yet") {
		t.Fatalf("stats no-usage content missing empty state:\n%s", content)
	}
}

func newCmdTestModel(store session.Store) *Model {
	styles := ui.DefaultStyles()
	ta := textarea.New()
	return &Model{
		width:    80,
		height:   24,
		textarea: ta,
		styles:   styles,
		dialog:   NewDialogModel(styles),
		store:    store,
		handoverSystemPromptResolver: func(agent *agents.Agent, providerKey, modelName string) (string, error) {
			if agent == nil {
				return "", nil
			}
			return agent.SystemPrompt, nil
		},
	}
}

func TestCmdTitleSetsSessionNameVerbatim(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.sess = &session.Session{ID: "manual-title", GeneratedShortTitle: "Old Auto"}
	m.setTextareaValue("/title Research  plan   v2")

	result, cmd := m.ExecuteCommand("/title   Research  plan   v2")
	m = result.(*Model)

	if got := m.sess.Name; got != "Research  plan   v2" {
		t.Fatalf("session name = %q, want spaces preserved", got)
	}
	if m.sess.TitleSource != session.TitleSourceUser {
		t.Fatalf("TitleSource = %q, want user", m.sess.TitleSource)
	}
	if store.updated == nil || store.updated.Name != "Research  plan   v2" {
		t.Fatalf("store updated name = %#v", store.updated)
	}
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("textarea = %q, want cleared", got)
	}
	if got := m.footerMessage; got != "Session title set to 'Research  plan   v2'." {
		t.Fatalf("footer = %q", got)
	}
	if cmd == nil {
		t.Fatal("expected footer clear command")
	}
}

func TestCmdTitleWithoutArgsDoesNotClobberName(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.sess = &session.Session{ID: "manual-title-empty", Name: "Existing Name"}

	result, cmd := m.ExecuteCommand("/title")
	m = result.(*Model)

	if got := m.sess.Name; got != "Existing Name" {
		t.Fatalf("session name = %q, want unchanged", got)
	}
	if store.updated != nil {
		t.Fatalf("store should not be updated, got %#v", store.updated)
	}
	if got := m.footerMessage; got != "Usage: /title <name>" {
		t.Fatalf("footer = %q, want usage", got)
	}
	if got := m.footerMessageTone; got != "error" {
		t.Fatalf("footer tone = %q, want error", got)
	}
	if cmd == nil {
		t.Fatal("expected footer clear command")
	}
}

func TestCmdAutotitleForcesGenerationDespiteExistingTitleAndAttempts(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{
		ID:                  "force-autotitle",
		Provider:            "mock",
		Model:               "mock-model",
		Mode:                session.ModeChat,
		GeneratedShortTitle: "Old Title",
		GeneratedLongTitle:  "Old long title",
		TitleSource:         session.TitleSourceGenerated,
	}
	m.fastProvider = llm.NewMockProvider("fast").AddTextResponse(`{"short_title":"Fresh Forced Title","long_title":"Fresh forced title from command","confidence":0.95}`)
	m.messages = []session.Message{
		{SessionID: m.sess.ID, Role: llm.RoleUser, TextContent: "Please investigate the flaky websocket reconnection tests.", Sequence: 0},
		{SessionID: m.sess.ID, Role: llm.RoleAssistant, TextContent: "I'll trace the reconnect loop and add coverage.", Sequence: 1},
	}
	m.titleGenerationSessionID = m.sess.ID
	m.titleGenerationAttempts = liveTitleGenerationMaxTries
	m.titleGenerationLastMessageCount = len(m.messages)

	result, cmd := m.ExecuteCommand("/autotitle")
	m = result.(*Model)
	if cmd == nil {
		t.Fatal("expected forced title generation command")
	}
	if !m.titleGenerationInFlight {
		t.Fatal("expected title generation to be in flight")
	}

	updated, _ := m.Update(cmd())
	m = updated.(*Model)
	if got := m.sess.GeneratedShortTitle; got != "Fresh Forced Title" {
		t.Fatalf("GeneratedShortTitle = %q, want fresh title", got)
	}
	if got := m.footerMessage; got != "Updated title: Fresh Forced Title" {
		t.Fatalf("footer = %q", got)
	}
	if got := m.footerMessageTone; got != "success" {
		t.Fatalf("footer tone = %q, want success", got)
	}
}

func TestCmdAutotitleWhileInFlightShowsFooterError(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "autotitle-inflight", Provider: "mock", Model: "mock-model", Mode: session.ModeChat}
	m.fastProvider = llm.NewMockProvider("fast")
	m.messages = []session.Message{{SessionID: m.sess.ID, Role: llm.RoleUser, TextContent: "Name this later", Sequence: 0}}
	m.titleGenerationSessionID = m.sess.ID
	m.titleGenerationInFlight = true

	result, cmd := m.ExecuteCommand("/autotitle")
	m = result.(*Model)
	if got := m.footerMessage; got != "Title generation is already running." {
		t.Fatalf("footer = %q", got)
	}
	if got := m.footerMessageTone; got != "error" {
		t.Fatalf("footer tone = %q, want error", got)
	}
	if cmd == nil {
		t.Fatal("expected footer clear command")
	}
}

func TestCmdAutotitleWithoutFastProviderShowsFooterError(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "autotitle-no-fast", Provider: "mock", Model: "mock-model", Mode: session.ModeChat}
	m.messages = []session.Message{{SessionID: m.sess.ID, Role: llm.RoleUser, TextContent: "Name this later", Sequence: 0}}

	result, cmd := m.ExecuteCommand("/autotitle")
	m = result.(*Model)
	if got := m.footerMessage; got != "Fast title generation is unavailable." {
		t.Fatalf("footer = %q", got)
	}
	if got := m.footerMessageTone; got != "error" {
		t.Fatalf("footer tone = %q, want error", got)
	}
	if cmd == nil {
		t.Fatal("expected footer clear command")
	}
}

func TestCmdAutotitleClearsManualNameOnSuccess(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.sess = &session.Session{
		ID:                  "autotitle-clears-name",
		Provider:            "mock",
		Model:               "mock-model",
		Mode:                session.ModeChat,
		Name:                "Custom Manual Name",
		GeneratedShortTitle: "Old Auto Title",
		GeneratedLongTitle:  "Old automatic title",
		TitleSource:         session.TitleSourceUser,
	}
	m.fastProvider = llm.NewMockProvider("fast").AddTextResponse(`{"short_title":"Regenerated Auto Title","long_title":"Regenerated automatic title after manual name","confidence":0.94}`)
	m.messages = []session.Message{{SessionID: m.sess.ID, Role: llm.RoleUser, TextContent: "Retitle this session from the actual task.", Sequence: 0}}

	result, cmd := m.ExecuteCommand("/autotitle")
	m = result.(*Model)
	if cmd == nil {
		t.Fatal("expected title generation command")
	}
	if got := m.sess.Name; got != "Custom Manual Name" {
		t.Fatalf("session name = %q, want preserved until generation succeeds", got)
	}

	updated, _ := m.Update(cmd())
	m = updated.(*Model)
	if got := m.sess.Name; got != "" {
		t.Fatalf("session name = %q, want cleared after success", got)
	}
	if store.updated == nil || store.updated.Name != "" {
		t.Fatalf("store should persist cleared name, got %#v", store.updated)
	}
	if got := m.sess.PreferredShortTitle(); got != "Regenerated Auto Title" {
		t.Fatalf("PreferredShortTitle = %q, want regenerated title", got)
	}
}

func TestCmdAutotitlePreservesManualNameOnFailure(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.sess = &session.Session{
		ID:                  "autotitle-fails-name",
		Provider:            "mock",
		Model:               "mock-model",
		Mode:                session.ModeChat,
		Name:                "Custom Manual Name",
		GeneratedShortTitle: "Old Auto Title",
		TitleSource:         session.TitleSourceUser,
	}
	m.fastProvider = llm.NewMockProvider("fast").AddError(errors.New("provider down"))
	m.messages = []session.Message{{SessionID: m.sess.ID, Role: llm.RoleUser, TextContent: "Retitle this session from the actual task.", Sequence: 0}}

	result, cmd := m.ExecuteCommand("/autotitle")
	m = result.(*Model)
	if cmd == nil {
		t.Fatal("expected title generation command")
	}

	updated, _ := m.Update(cmd())
	m = updated.(*Model)
	if got := m.sess.Name; got != "Custom Manual Name" {
		t.Fatalf("session name = %q, want preserved after generation failure", got)
	}
	if got := m.sess.TitleSource; got != session.TitleSourceUser {
		t.Fatalf("TitleSource = %q, want user", got)
	}
	if got := m.sess.GeneratedShortTitle; got != "Old Auto Title" {
		t.Fatalf("GeneratedShortTitle = %q, want unchanged", got)
	}
	if got := m.footerMessage; !strings.Contains(got, "Title generation failed") {
		t.Fatalf("footer = %q, want failure message", got)
	}
}

func TestCmdAutotitleResultDoesNotClobberLaterManualTitle(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.sess = &session.Session{
		ID:                  "autotitle-title-race",
		Provider:            "mock",
		Model:               "mock-model",
		Mode:                session.ModeChat,
		GeneratedShortTitle: "Old Auto Title",
		TitleSource:         session.TitleSourceGenerated,
	}
	m.fastProvider = llm.NewMockProvider("fast").AddTextResponse(`{"short_title":"Late Auto Title","long_title":"Late automatic title result","confidence":0.94}`)
	m.messages = []session.Message{{SessionID: m.sess.ID, Role: llm.RoleUser, TextContent: "Retitle this session from the actual task.", Sequence: 0}}

	result, cmd := m.ExecuteCommand("/autotitle")
	m = result.(*Model)
	if cmd == nil {
		t.Fatal("expected title generation command")
	}
	result, _ = m.ExecuteCommand("/title Manual Override")
	m = result.(*Model)
	if got := m.titleManualEditVersion; got != 1 {
		t.Fatalf("titleManualEditVersion = %d, want 1 after /title", got)
	}

	updated, _ := m.Update(cmd())
	m = updated.(*Model)
	if got := m.sess.Name; got != "Manual Override" {
		t.Fatalf("session name = %q, want manual override", got)
	}
	if got := m.sess.TitleSource; got != session.TitleSourceUser {
		t.Fatalf("TitleSource = %q, want user", got)
	}
	if got := m.sess.GeneratedShortTitle; got != "Old Auto Title" {
		t.Fatalf("GeneratedShortTitle = %q, want unchanged", got)
	}
}

func newEffortCmdTestModel(provider, model string) (*Model, *mockStore) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.config = &config.Config{Providers: map[string]config.ProviderConfig{}}
	if provider == "openai" {
		m.config.Providers["openai"] = config.ProviderConfig{Type: config.ProviderTypeOpenAI, Model: model}
	}
	m.sess = &session.Session{ID: "sess-effort", Provider: provider, ProviderKey: provider, Model: model}
	m.providerName = provider
	m.providerKey = provider
	m.modelName = model
	m.engine = llm.NewEngine(llm.NewMockProvider("old"), nil)
	return m, store
}

func prepareEffortShortcutTestModel(m *Model) {
	m.keyMap = DefaultKeyMap()
	m.completions = NewCompletionsModel(m.styles)
}

func TestCycleEffortShortcutPreservesDraft(t *testing.T) {
	m, store := newEffortCmdTestModel("custom", "alias-model")
	m.config.Providers["custom"] = config.ProviderConfig{
		Type:  config.ProviderTypeOpenAICompat,
		Model: "alias-model",
		ModelConfigs: []config.ProviderModelConfig{{
			ID:               "upstream/model-id",
			Alias:            "alias-model",
			ReasoningEfforts: []string{"high", "max"},
		}},
	}
	prepareEffortShortcutTestModel(m)
	m.messages = []session.Message{*session.NewMessage(m.sess.ID, llm.UserText("previous"), 0)}
	m.setTextareaValue("half-written prompt")
	activeEngine := m.engine

	result, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	rm := result.(*Model)
	if cmd == nil {
		t.Fatal("expected footer command")
	}
	if rm.modelName != "alias-model-high" {
		t.Fatalf("modelName = %q, want alias-model-high; footer=%q", rm.modelName, rm.footerMessage)
	}
	if got := rm.textarea.Value(); got != "half-written prompt" {
		t.Fatalf("draft = %q, want preserved draft", got)
	}
	if rm.sess.Model != "alias-model-high" || store.updated == nil {
		t.Fatalf("session/store not updated: sess=%#v updated=%#v", rm.sess, store.updated)
	}
	if rm.engine != activeEngine {
		t.Fatal("ctrl+r should update effort state without rebuilding the provider/engine")
	}
	if len(rm.messages) != 1 {
		t.Fatalf("ctrl+r appended visible messages immediately: len=%d", len(rm.messages))
	}
	if len(store.added) != 0 {
		t.Fatalf("ctrl+r persisted marker immediately: %#v", store.added)
	}
	if rm.pendingModelSwitch == nil {
		t.Fatal("expected deferred model-switch marker")
	}

	result, _ = rm.sendMessage("half-written prompt")
	rm = result.(*Model)
	if len(rm.messages) < 3 {
		t.Fatalf("messages len = %d, want previous + marker + user", len(rm.messages))
	}
	markerMsg := rm.messages[1]
	if markerMsg.Role != llm.RoleEvent {
		t.Fatalf("deferred marker role = %q, want event", markerMsg.Role)
	}
	marker, ok := llm.ParseModelSwapMarker(markerMsg.ToLLMMessage())
	if !ok {
		t.Fatalf("failed to parse deferred marker: %#v", markerMsg)
	}
	if marker.FromModel != "alias-model" || marker.ToModel != "alias-model-high" {
		t.Fatalf("unexpected deferred marker: %#v", marker)
	}
	if rm.pendingModelSwitch != nil {
		t.Fatalf("pending marker was not cleared: %#v", rm.pendingModelSwitch)
	}
}

func TestCycleEffortShortcutWrapsToDefault(t *testing.T) {
	m, _ := newEffortCmdTestModel("claude-bin", "sonnet-high")
	prepareEffortShortcutTestModel(m)
	m.setTextareaValue("draft")

	// Sonnet efforts are low, medium, high, so high wraps to default.
	result, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	rm := result.(*Model)
	if rm.modelName != "sonnet" {
		t.Fatalf("modelName = %q, want sonnet; footer=%q", rm.modelName, rm.footerMessage)
	}
	if got := rm.textarea.Value(); got != "draft" {
		t.Fatalf("draft = %q, want preserved draft", got)
	}
}

func TestCycleEffortShortcutUnsupportedPreservesDraft(t *testing.T) {
	m, store := newEffortCmdTestModel("mock", "mock-model")
	prepareEffortShortcutTestModel(m)
	m.setTextareaValue("keep me")

	result, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	rm := result.(*Model)
	if rm.modelName != "mock-model" {
		t.Fatalf("modelName changed to %q", rm.modelName)
	}
	if got := rm.textarea.Value(); got != "keep me" {
		t.Fatalf("draft = %q, want preserved draft", got)
	}
	if store.updated != nil {
		t.Fatalf("store updated on unsupported effort: %#v", store.updated)
	}
	if !strings.Contains(rm.footerMessage, "does not expose switchable reasoning efforts") {
		t.Fatalf("unexpected footer: %q", rm.footerMessage)
	}
}

func TestCmdEffortSwitchesOpenAIEffort(t *testing.T) {
	m, store := newEffortCmdTestModel("openai", "gpt-5.4-high")

	result, cmd := m.cmdEffort([]string{"medium"})
	rm := result.(*Model)
	if cmd == nil {
		t.Fatal("expected footer command")
	}
	if rm.modelName != "gpt-5.4-medium" {
		t.Fatalf("modelName = %q, want gpt-5.4-medium", rm.modelName)
	}
	if rm.sess.Model != "gpt-5.4-medium" || store.updated == nil {
		t.Fatalf("session/store not updated: sess=%#v updated=%#v", rm.sess, store.updated)
	}
}

func TestStreamingCmdEffortQueuesWithoutSwitchingActiveEngine(t *testing.T) {
	m, store := newEffortCmdTestModel("openai", "gpt-5.4-high")
	prepareEffortShortcutTestModel(m)
	m.streaming = true
	m.setTextareaValue("/effort medium")
	activeEngine := m.engine

	result, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	rm := result.(*Model)
	if cmd == nil {
		t.Fatal("expected queued-effort footer command")
	}
	if !rm.streaming {
		t.Fatal("expected active stream to keep running")
	}
	if rm.engine != activeEngine {
		t.Fatal("streaming /effort replaced the active engine")
	}
	if rm.modelName != "gpt-5.4-high" || rm.sess.Model != "gpt-5.4-high" {
		t.Fatalf("model switched during stream: modelName=%q sess.Model=%q", rm.modelName, rm.sess.Model)
	}
	if store.updated != nil {
		t.Fatalf("store updated before stream completed: %#v", store.updated)
	}
	if rm.pendingStreamModelSwitch == nil {
		t.Fatal("expected queued stream model switch")
	}
	if rm.pendingStreamModelSwitch.provider != "openai" || rm.pendingStreamModelSwitch.model != "gpt-5.4-medium" {
		t.Fatalf("unexpected queued switch: %#v", rm.pendingStreamModelSwitch)
	}
	if got := rm.textarea.Value(); got != "" {
		t.Fatalf("textarea = %q, want cleared command", got)
	}
	if !strings.Contains(rm.footerMessage, "Effort medium queued") || !strings.Contains(rm.footerMessage, "next model turn") {
		t.Fatalf("unexpected queued footer: %q", rm.footerMessage)
	}
}

func TestStreamingCmdEffortCurrentEffortClearsQueuedSwitch(t *testing.T) {
	m, _ := newEffortCmdTestModel("openai", "gpt-5.4-high")
	m.streaming = true
	m.pendingStreamModelSwitch = &pendingStreamModelSwitch{provider: "openai", model: "gpt-5.4-medium"}
	activeEngine := m.engine

	result, _ := m.cmdEffort([]string{"high"})
	rm := result.(*Model)
	if rm.pendingStreamModelSwitch != nil {
		t.Fatalf("expected current-effort command to clear queued switch, got %#v", rm.pendingStreamModelSwitch)
	}
	if rm.engine != activeEngine || rm.modelName != "gpt-5.4-high" || rm.sess.Model != "gpt-5.4-high" {
		t.Fatalf("current-effort command mutated state: engineChanged=%v model=%q sess=%q", rm.engine != activeEngine, rm.modelName, rm.sess.Model)
	}
	if !strings.Contains(rm.footerMessage, "cleared queued effort") {
		t.Fatalf("unexpected footer: %q", rm.footerMessage)
	}
}

func TestStreamingCmdEffortNoArgsShowsQueuedEffort(t *testing.T) {
	m, _ := newEffortCmdTestModel("openai", "gpt-5.4-medium")
	m.streaming = true
	m.queuePendingStreamModelSwitch("openai", "gpt-5.4-xhigh")

	result, _ := m.cmdEffort(nil)
	rm := result.(*Model)
	if !strings.Contains(rm.footerMessage, "Current effort: medium") || !strings.Contains(rm.footerMessage, "Queued: xhigh") {
		t.Fatalf("footer missing current/queued effort: %q", rm.footerMessage)
	}
	if !strings.Contains(rm.footerMessage, "next model turn") {
		t.Fatalf("footer missing timing hint: %q", rm.footerMessage)
	}
}

func TestStreamingCmdEffortNoArgsShowsAppliedEffortAsCurrent(t *testing.T) {
	m, _ := newEffortCmdTestModel("openai", "gpt-5.4-medium")
	m.streaming = true
	m.queuePendingStreamModelSwitch("openai", "gpt-5.4-xhigh")
	m.markPendingStreamModelSwitchApplied("gpt-5.4-xhigh")

	result, _ := m.cmdEffort(nil)
	rm := result.(*Model)
	if !strings.Contains(rm.footerMessage, "Current effort: xhigh") || !strings.Contains(rm.footerMessage, "Active for current run: xhigh") {
		t.Fatalf("footer missing applied current effort: %q", rm.footerMessage)
	}
	if strings.Contains(rm.footerMessage, "Current effort: medium") {
		t.Fatalf("footer shows stale current effort: %q", rm.footerMessage)
	}
}

func TestCycleEffortWhileStreamingQueuesNextEffortAndPreservesDraft(t *testing.T) {
	m, store := newEffortCmdTestModel("openai", "gpt-5.4-low")
	prepareEffortShortcutTestModel(m)
	m.streaming = true
	m.setTextareaValue("draft interjection")
	activeEngine := m.engine

	result, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	rm := result.(*Model)
	if rm.engine != activeEngine || rm.modelName != "gpt-5.4-low" || rm.sess.Model != "gpt-5.4-low" {
		t.Fatalf("streaming Ctrl+R mutated active model: engineChanged=%v model=%q sess=%q", rm.engine != activeEngine, rm.modelName, rm.sess.Model)
	}
	if store.updated != nil {
		t.Fatalf("store updated before stream completed: %#v", store.updated)
	}
	if rm.pendingStreamModelSwitch == nil || rm.pendingStreamModelSwitch.model != "gpt-5.4-medium" {
		t.Fatalf("unexpected queued switch: %#v", rm.pendingStreamModelSwitch)
	}
	if got := rm.textarea.Value(); got != "draft interjection" {
		t.Fatalf("draft = %q, want preserved", got)
	}
	if !strings.Contains(rm.footerMessage, "Effort medium queued") {
		t.Fatalf("unexpected footer: %q", rm.footerMessage)
	}
}

func TestCycleEffortWhileStreamingAdvancesFromQueuedEffort(t *testing.T) {
	m, store := newEffortCmdTestModel("openai", "gpt-5.4-medium")
	prepareEffortShortcutTestModel(m)
	m.streaming = true
	m.setTextareaValue("draft interjection")
	activeEngine := m.engine

	result, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	rm := result.(*Model)
	if rm.pendingStreamModelSwitch == nil || rm.pendingStreamModelSwitch.model != "gpt-5.4-high" {
		t.Fatalf("first queued switch = %#v, want high", rm.pendingStreamModelSwitch)
	}

	result, _ = rm.handleKeyMsg(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	rm = result.(*Model)
	if rm.engine != activeEngine || rm.modelName != "gpt-5.4-medium" || rm.sess.Model != "gpt-5.4-medium" {
		t.Fatalf("second streaming Ctrl+R mutated active model: engineChanged=%v model=%q sess=%q", rm.engine != activeEngine, rm.modelName, rm.sess.Model)
	}
	if store.updated != nil {
		t.Fatalf("store updated before stream completed: %#v", store.updated)
	}
	if rm.pendingStreamModelSwitch == nil || rm.pendingStreamModelSwitch.model != "gpt-5.4-xhigh" {
		t.Fatalf("second queued switch = %#v, want xhigh", rm.pendingStreamModelSwitch)
	}
	if got := rm.textarea.Value(); got != "draft interjection" {
		t.Fatalf("draft = %q, want preserved", got)
	}
	if !strings.Contains(rm.footerMessage, "Effort xhigh queued") {
		t.Fatalf("unexpected footer: %q", rm.footerMessage)
	}
}

func TestPendingStreamingEffortPreservesQueuedInterjectionsOnStreamDone(t *testing.T) {
	m, _ := newEffortCmdTestModel("openai", "gpt-5.4-medium")
	prepareEffortShortcutTestModel(m)
	m.streaming = true
	firstID := m.nextPendingInterjectionID()
	secondID := m.nextPendingInterjectionID()
	m.applyInterruptAction(firstID, "first note", llm.InterruptInterject)
	m.applyInterruptAction(secondID, "second note", llm.InterruptInterject)
	m.pendingStreamModelSwitch = &pendingStreamModelSwitch{provider: "openai", model: "gpt-5.4-high"}
	oldEngine := m.engine

	result, _ := m.Update(streamEventMsg{event: ui.DoneEvent(0)})
	rm := result.(*Model)
	if rm.streaming {
		t.Fatal("stream should be marked complete")
	}
	if rm.engine == oldEngine || rm.modelName != "gpt-5.4-high" {
		t.Fatalf("queued switch did not apply: engineChanged=%v model=%q", rm.engine != oldEngine, rm.modelName)
	}
	if got := rm.textarea.Value(); got != "first note\nsecond note" {
		t.Fatalf("restored draft = %q, want both queued interjections", got)
	}
	if len(rm.pendingInterjections) != 0 || rm.pendingInterjection != "" {
		t.Fatalf("pending UI state not cleared: latest=%q stack=%#v", rm.pendingInterjection, rm.pendingInterjections)
	}
}

func TestPendingStreamingEffortAppliesOnStreamDone(t *testing.T) {
	m, store := newEffortCmdTestModel("openai", "gpt-5.4-high")
	store.messages = map[string][]session.Message{
		m.sess.ID: {*session.NewMessage(m.sess.ID, llm.UserText("previous"), 0)},
	}
	m.streaming = true
	m.pendingStreamModelSwitch = &pendingStreamModelSwitch{provider: "openai", model: "gpt-5.4-medium"}
	activeEngine := m.engine

	result, _ := m.Update(streamEventMsg{event: ui.DoneEvent(0)})
	rm := result.(*Model)
	if rm.streaming {
		t.Fatal("stream should be marked complete")
	}
	if rm.pendingStreamModelSwitch != nil {
		t.Fatalf("pending switch was not cleared: %#v", rm.pendingStreamModelSwitch)
	}
	if rm.engine == activeEngine {
		t.Fatal("expected engine to be replaced after stream completion")
	}
	if rm.modelName != "gpt-5.4-medium" || rm.sess.Model != "gpt-5.4-medium" {
		t.Fatalf("queued switch not applied: modelName=%q sess.Model=%q", rm.modelName, rm.sess.Model)
	}
	if store.updated == nil || store.updated.Model != "gpt-5.4-medium" {
		t.Fatalf("store metadata not updated after queued switch: %#v", store.updated)
	}
	if len(rm.messages) != 1 {
		t.Fatalf("messages len = %d, want previous message only until next send", len(rm.messages))
	}
	if len(store.added) != 0 {
		t.Fatalf("stream-done switch persisted marker immediately: %#v", store.added)
	}
	if rm.pendingModelSwitch == nil {
		t.Fatal("expected model-swap marker to defer until next send")
	}

	result, _ = rm.sendMessage("next prompt")
	rm = result.(*Model)
	if len(rm.messages) < 3 {
		t.Fatalf("messages len = %d, want previous + marker + user", len(rm.messages))
	}
	marker, ok := llm.ParseModelSwapMarker(rm.messages[1].ToLLMMessage())
	if !ok {
		t.Fatalf("expected deferred model-swap marker before next user turn, got %#v", rm.messages[1])
	}
	if marker.FromModel != "gpt-5.4-high" || marker.ToModel != "gpt-5.4-medium" {
		t.Fatalf("unexpected marker: %#v", marker)
	}
	if rm.messages[2].Role != llm.RoleUser || rm.messages[2].TextContent != "next prompt" {
		t.Fatalf("expected user message after marker, got %#v", rm.messages[2])
	}
	if rm.pendingModelSwitch != nil {
		t.Fatalf("pending marker was not cleared: %#v", rm.pendingModelSwitch)
	}
}

func TestPendingStreamingEffortAppliesOnStreamError(t *testing.T) {
	m, store := newEffortCmdTestModel("openai", "gpt-5.4-high")
	m.messages = []session.Message{*session.NewMessage(m.sess.ID, llm.UserText("previous"), 0)}
	m.streaming = true
	m.pendingStreamModelSwitch = &pendingStreamModelSwitch{provider: "openai", model: "gpt-5.4-medium"}
	activeEngine := m.engine

	result, _ := m.Update(streamEventMsg{event: ui.ErrorEvent(errors.New("boom"))})
	rm := result.(*Model)
	if rm.streaming {
		t.Fatal("stream should be marked stopped after error")
	}
	if rm.pendingStreamModelSwitch != nil {
		t.Fatalf("pending switch was not cleared on error: %#v", rm.pendingStreamModelSwitch)
	}
	if rm.engine == activeEngine || rm.modelName != "gpt-5.4-medium" || rm.sess.Model != "gpt-5.4-medium" {
		t.Fatalf("queued switch not applied on error: engineChanged=%v model=%q sess=%q", rm.engine != activeEngine, rm.modelName, rm.sess.Model)
	}
	if store.updated == nil || store.updated.Model != "gpt-5.4-medium" {
		t.Fatalf("store metadata not updated after error switch: %#v", store.updated)
	}
	if rm.pendingModelSwitch == nil {
		t.Fatal("expected error-path model marker to defer until next send")
	}
}

func TestManualModelSwitchClearsStaleQueuedStreamingEffort(t *testing.T) {
	m, _ := newEffortCmdTestModel("openai", "gpt-5.4-high")
	m.pendingStreamModelSwitch = &pendingStreamModelSwitch{provider: "openai", model: "gpt-5.4-medium"}
	m.pendingModelSwitch = &llm.ModelSwapMarker{FromProvider: "openai", FromModel: "gpt-5.4-high", ToProvider: "openai", ToModel: "gpt-5.4-medium", Status: "started"}

	result, _ := m.cmdEffort([]string{"low"})
	rm := result.(*Model)
	if rm.pendingStreamModelSwitch != nil {
		t.Fatalf("manual effort switch left stale queued switch: %#v", rm.pendingStreamModelSwitch)
	}
	if rm.pendingModelSwitch != nil {
		t.Fatalf("manual effort switch left stale deferred marker: %#v", rm.pendingModelSwitch)
	}
	if rm.modelName != "gpt-5.4-low" || rm.sess.Model != "gpt-5.4-low" {
		t.Fatalf("manual effort switch did not apply: model=%q sess=%q", rm.modelName, rm.sess.Model)
	}
}

func TestPendingStreamingEffortAppliesBeforeNextSendIfStillPending(t *testing.T) {
	m, store := newEffortCmdTestModel("openai", "gpt-5.4-high")
	m.messages = []session.Message{*session.NewMessage(m.sess.ID, llm.UserText("previous"), 0)}
	m.pendingStreamModelSwitch = &pendingStreamModelSwitch{provider: "openai", model: "gpt-5.4-medium"}
	activeEngine := m.engine

	result, _ := m.sendMessage("next prompt")
	rm := result.(*Model)
	if rm.pendingStreamModelSwitch != nil {
		t.Fatalf("pending switch was not cleared before send: %#v", rm.pendingStreamModelSwitch)
	}
	if rm.engine == activeEngine || rm.modelName != "gpt-5.4-medium" || rm.sess.Model != "gpt-5.4-medium" {
		t.Fatalf("pending switch not applied before send: engineChanged=%v model=%q sess=%q", rm.engine != activeEngine, rm.modelName, rm.sess.Model)
	}
	if !rm.streaming {
		t.Fatal("sendMessage should still start the next stream")
	}
	if store.updated == nil || store.updated.Model != "gpt-5.4-medium" {
		t.Fatalf("store metadata not updated before send: %#v", store.updated)
	}
	if len(rm.messages) < 3 {
		t.Fatalf("messages len = %d, want previous + marker + user", len(rm.messages))
	}
	if _, ok := llm.ParseModelSwapMarker(rm.messages[1].ToLLMMessage()); !ok {
		t.Fatalf("expected model marker before user message, got %#v", rm.messages[1])
	}
	if rm.messages[2].Role != llm.RoleUser || rm.messages[2].TextContent != "next prompt" {
		t.Fatalf("expected user message after marker, got %#v", rm.messages[2])
	}
}

func TestStreamingCmdEffortRejectsInvalidWithoutQueue(t *testing.T) {
	m, store := newEffortCmdTestModel("openai", "gpt-5.4-high")
	m.streaming = true
	activeEngine := m.engine

	result, _ := m.cmdEffort([]string{"max"})
	rm := result.(*Model)
	if rm.pendingStreamModelSwitch != nil {
		t.Fatalf("invalid effort queued a switch: %#v", rm.pendingStreamModelSwitch)
	}
	if rm.engine != activeEngine || rm.modelName != "gpt-5.4-high" || rm.sess.Model != "gpt-5.4-high" {
		t.Fatalf("invalid streaming effort mutated state: engineChanged=%v model=%q sess=%q", rm.engine != activeEngine, rm.modelName, rm.sess.Model)
	}
	if store.updated != nil {
		t.Fatalf("store updated on invalid streaming effort: %#v", store.updated)
	}
	if !strings.Contains(rm.footerMessage, "Unsupported effort") {
		t.Fatalf("expected unsupported effort footer, got %q", rm.footerMessage)
	}
}

func TestCmdEffortRejectsUnsupportedOpenAIMax(t *testing.T) {
	m, store := newEffortCmdTestModel("openai", "gpt-5.4-high")

	result, _ := m.cmdEffort([]string{"max"})
	rm := result.(*Model)
	if rm.modelName != "gpt-5.4-high" {
		t.Fatalf("modelName changed to %q", rm.modelName)
	}
	if store.updated != nil {
		t.Fatalf("store updated on invalid effort: %#v", store.updated)
	}
	if !strings.Contains(rm.footerMessage, "Unsupported effort") || !strings.Contains(rm.footerMessage, "minimal, low, medium, high, xhigh, default") {
		t.Fatalf("unexpected footer: %q", rm.footerMessage)
	}
}

func TestCmdEffortDefaultStripsSuffix(t *testing.T) {
	m, _ := newEffortCmdTestModel("openai", "gpt-5.4-medium")

	result, _ := m.cmdEffort([]string{"default"})
	rm := result.(*Model)
	if rm.modelName != "gpt-5.4" {
		t.Fatalf("modelName = %q, want gpt-5.4", rm.modelName)
	}
	if rm.sess.Model != "gpt-5.4" {
		t.Fatalf("session model = %q, want gpt-5.4", rm.sess.Model)
	}
}

func TestCmdEffortSwitchesClaudeBinOpusMaxAndXHigh(t *testing.T) {
	for _, effort := range []string{"max", "xhigh"} {
		t.Run(effort, func(t *testing.T) {
			m, _ := newEffortCmdTestModel("claude-bin", "opus-high")
			result, _ := m.cmdEffort([]string{effort})
			rm := result.(*Model)
			want := "opus-" + effort
			if rm.modelName != want {
				t.Fatalf("modelName = %q, want %q; footer=%q", rm.modelName, want, rm.footerMessage)
			}
		})
	}
}

func TestCmdEffortRejectsUnsupportedClaudeBinEfforts(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		effort   string
		wantStay string
	}{
		{"sonnet max", "sonnet-high", "max", "sonnet-high"},
		{"haiku medium", "haiku", "medium", "haiku"},
		{"invalid", "opus-high", "turbo", "opus-high"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, store := newEffortCmdTestModel("claude-bin", tt.model)
			result, _ := m.cmdEffort([]string{tt.effort})
			rm := result.(*Model)
			if rm.modelName != tt.wantStay {
				t.Fatalf("modelName changed to %q, want %q", rm.modelName, tt.wantStay)
			}
			if store.updated != nil {
				t.Fatalf("store updated on invalid effort: %#v", store.updated)
			}
			if !strings.Contains(rm.footerMessage, "Unsupported effort") {
				t.Fatalf("expected unsupported footer, got %q", rm.footerMessage)
			}
		})
	}
}

func TestCmdEffortSwitchesConfigEnabledVLLM(t *testing.T) {
	llm.RegisterConfigReasoningEfforts([]llm.ConfigModelReasoningEfforts{{
		Provider: "cdck_qwen",
		Model:    "Qwen/Qwen3.5-122B-A10B",
		Efforts:  llm.DefaultReasoningEffortsForProviderType("vllm"),
	}})
	defer llm.RegisterConfigReasoningEfforts(nil)

	store := &mockStore{}
	m := newCmdTestModel(store)
	m.config = &config.Config{Providers: map[string]config.ProviderConfig{
		"cdck_qwen": {Type: config.ProviderTypeVLLM, BaseURL: "http://example.invalid/v1", Model: "Qwen/Qwen3.5-122B-A10B"},
	}}
	m.sess = &session.Session{ID: "sess-effort-vllm", Provider: "cdck_qwen", ProviderKey: "cdck_qwen", Model: "Qwen/Qwen3.5-122B-A10B"}
	m.providerName = "cdck_qwen"
	m.providerKey = "cdck_qwen"
	m.modelName = "Qwen/Qwen3.5-122B-A10B"
	m.engine = llm.NewEngine(llm.NewMockProvider("old"), nil)

	result, _ := m.cmdEffort([]string{"max"})
	rm := result.(*Model)
	if rm.modelName != "Qwen/Qwen3.5-122B-A10B-max" {
		t.Fatalf("modelName = %q, want Qwen/Qwen3.5-122B-A10B-max; footer=%q", rm.modelName, rm.footerMessage)
	}
}

func TestCmdEffortNoArgsShowsCurrentAndAvailable(t *testing.T) {
	m, _ := newEffortCmdTestModel("claude-bin", "opus-high")
	result, _ := m.cmdEffort(nil)
	rm := result.(*Model)
	for _, want := range []string{"Current effort: high", "low, medium, high, xhigh, max, default"} {
		if !strings.Contains(rm.footerMessage, want) {
			t.Fatalf("footer missing %q: %q", want, rm.footerMessage)
		}
	}
}

func TestEffortCompletionsAreModelSpecific(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		model       string
		input       string
		wantNames   []string
		rejectNames []string
	}{
		{
			name:        "openai medium no max",
			provider:    "openai",
			model:       "gpt-5.4-high",
			input:       "/effort m",
			wantNames:   []string{"effort medium"},
			rejectNames: []string{"effort max"},
		},
		{
			name:        "sonnet excludes opus-only",
			provider:    "claude-bin",
			model:       "sonnet-high",
			input:       "/effort ",
			wantNames:   []string{"effort low", "effort medium", "effort high", "effort default", "effort auto"},
			rejectNames: []string{"effort max", "effort xhigh"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestChatModel(false)
			m.providerKey = tt.provider
			m.providerName = tt.provider
			m.modelName = tt.model
			m.sess = &session.Session{ID: "sess-complete", ProviderKey: tt.provider, Model: tt.model}
			m.completions.Show()
			m.setTextareaValue(tt.input)
			m.updateCompletions()

			got := completionNames(m.completions.filtered)
			for _, want := range tt.wantNames {
				if !containsString(got, want) {
					t.Fatalf("completions missing %q: %v", want, got)
				}
			}
			for _, reject := range tt.rejectNames {
				if containsString(got, reject) {
					t.Fatalf("completions included %q: %v", reject, got)
				}
			}
		})
	}
}

func completionNames(items []Command) []string {
	names := make([]string, len(items))
	for i, item := range items {
		names[i] = item.Name
	}
	return names
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestCmdResume_DirectResumeRequestsRelaunch(t *testing.T) {
	sessionID := "sess-resume-1"
	sess := &session.Session{ID: sessionID, Number: 1, Name: "my session"}
	msgs := []session.Message{
		{ID: 1, SessionID: sessionID, Role: "user", TextContent: "hello"},
		{ID: 2, SessionID: sessionID, Role: "assistant", TextContent: "hi"},
	}
	store := &mockStore{
		sessions: map[string]*session.Session{sessionID: sess},
		messages: map[string][]session.Message{sessionID: msgs},
	}
	m := newCmdTestModel(store)
	result, _ := m.cmdResume([]string{sessionID})
	rm := result.(*Model)

	if !rm.quitting {
		t.Fatal("expected cmdResume to request chat relaunch via quit")
	}
	if rm.RequestedResumeSessionID() != sessionID {
		t.Fatalf("expected pending resume session ID %q, got %q", sessionID, rm.RequestedResumeSessionID())
	}
	if len(rm.messages) != 0 {
		t.Errorf("expected no in-place message load, got %d messages", len(rm.messages))
	}
	if len(msgs) == 0 {
		t.Fatal("test fixture expected non-empty source messages")
	}
}

func TestCmdResume_DoesNotMutateViewStateInPlace(t *testing.T) {
	sessionID := "sess-cache-bug"
	sess := &session.Session{ID: sessionID, Number: 2}
	msgs := []session.Message{
		{ID: 1, SessionID: sessionID},
		{ID: 2, SessionID: sessionID},
	}
	store := &mockStore{
		sessions: map[string]*session.Session{sessionID: sess},
		messages: map[string][]session.Message{sessionID: msgs},
	}
	m := newCmdTestModel(store)

	// Simulate stale cache with the same message count as the incoming session.
	// Before the fix, historyValid stays true because the count check is a false positive.
	m.viewCache.historyValid = true
	m.viewCache.historyMsgCount = len(msgs) // same count — the bug
	m.viewCache.contentVersion = 5

	result, _ := m.cmdResume([]string{sessionID})
	rm := result.(*Model)

	if !rm.viewCache.historyValid {
		t.Error("expected in-place view cache to be untouched because resume now relaunches chat")
	}
	if rm.viewCache.contentVersion != 5 {
		t.Errorf("expected contentVersion to remain unchanged, got %d", rm.viewCache.contentVersion)
	}
	if rm.RequestedResumeSessionID() != sessionID {
		t.Fatalf("expected pending resume session ID %q, got %q", sessionID, rm.RequestedResumeSessionID())
	}
}

func TestCmdResume_NoArgs_OpensEmbeddedSessionsBrowser(t *testing.T) {
	sessionID := "sess-resume-picker-1"
	store := &mockStore{
		summaries: []session.SessionSummary{
			{
				ID:           sessionID,
				Number:       7,
				Name:         "session seven",
				MessageCount: 3,
				Model:        "claude-sonnet-20250101",
				Summary:      "Discussed release notes and rollout checks",
				UpdatedAt:    time.Now().Add(-5 * time.Minute),
			},
		},
	}
	m := newCmdTestModel(store)
	m.setTextareaValue("draft note")

	result, _ := m.cmdResume([]string{})
	rm := result.(*Model)

	if !rm.resumeBrowserMode {
		t.Fatal("expected resume browser mode to be active")
	}
	if rm.resumeBrowserModel == nil {
		t.Fatal("expected embedded resume browser model to be initialized")
	}
	if rm.dialog.IsOpen() {
		t.Fatal("expected generic dialog to remain closed")
	}
	if got := rm.textarea.Value(); got != "draft note" {
		t.Fatalf("expected draft input to be preserved, got %q", got)
	}
}

func TestUpdateCompletions_ModelCommandShowsModelMatches(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := newTestChatModel(false)
	m.config = &config.Config{DefaultProvider: "anthropic"}
	m.completions.Show()
	m.setTextareaValue("/model claude-sonnet")

	m.updateCompletions()

	if len(m.completions.filtered) == 0 {
		t.Fatal("expected /model completions to include matching models")
	}

	for _, item := range m.completions.filtered {
		if strings.HasPrefix(item.Name, "model anthropic:claude-sonnet") {
			return
		}
	}

	t.Fatalf("expected an anthropic claude-sonnet completion, got %#v", m.completions.filtered)
}

func TestUpdateCompletions_ModelCommandFallbackFuzzyMatchesProviderAndModel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := newTestChatModel(false)
	m.config = &config.Config{DefaultProvider: "chatgpt"}
	m.completions.Show()
	m.setTextareaValue("/model chat5.4")

	m.updateCompletions()

	if len(m.completions.filtered) == 0 {
		t.Fatal("expected fuzzy /model completions to include matching models")
	}

	for _, item := range m.completions.filtered {
		if item.Name == "model chatgpt:gpt-5.4" {
			return
		}
	}

	t.Fatalf("expected fuzzy completion to include chatgpt:gpt-5.4, got %#v", m.completions.filtered)
}

func TestUpdateCompletions_HandoverModelOverrideUsesSameProviderModelMatcher(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := newTestChatModel(false)
	m.config = &config.Config{DefaultProvider: "chatgpt"}
	m.agentLister = func(cfg *config.Config) ([]string, error) {
		return []string{"developer"}, nil
	}
	m.completions.Show()
	m.setTextareaValue("/handover @developer chat5.4")

	m.updateCompletions()

	if len(m.completions.filtered) == 0 {
		t.Fatal("expected /handover completions to include matching provider:model overrides")
	}

	for _, item := range m.completions.filtered {
		if item.Name == "handover @developer chatgpt:gpt-5.4" {
			return
		}
	}

	t.Fatalf("expected handover completion to include chatgpt:gpt-5.4, got %#v", m.completions.filtered)
}

func TestResolveProviderModelArg_FuzzyMatchSharedByCommands(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	resolved, ok := resolveProviderModelArg("chat5.4", &config.Config{DefaultProvider: "chatgpt"}, "")
	if !ok {
		t.Fatal("expected shorthand provider/model query to resolve")
	}
	if resolved != "chatgpt:gpt-5.4" {
		t.Fatalf("resolveProviderModelArg() = %q, want %q", resolved, "chatgpt:gpt-5.4")
	}
}

func TestResolveProviderModelArg_PrefersRecentModels(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	if err := config.RecordModelUse("openai:gpt-5.4"); err != nil {
		t.Fatalf("RecordModelUse: %v", err)
	}

	resolved, ok := resolveProviderModelArg("gpt-5.4", &config.Config{DefaultProvider: "chatgpt"}, "")
	if !ok {
		t.Fatal("expected gpt-5.4 to resolve")
	}
	if resolved != "openai:gpt-5.4" {
		t.Fatalf("resolveProviderModelArg() = %q, want %q", resolved, "openai:gpt-5.4")
	}
}

func TestProviderModelCompletionItems_PrefersRecentModels(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	if err := config.RecordModelUse("openai:gpt-5.4"); err != nil {
		t.Fatalf("RecordModelUse: %v", err)
	}

	items := providerModelCompletionItems("model ", "gpt-5.4", &config.Config{DefaultProvider: "chatgpt"})
	if len(items) == 0 {
		t.Fatal("expected provider model completions")
	}
	if items[0].Name != "model openai:gpt-5.4" {
		t.Fatalf("first completion = %q, want %q", items[0].Name, "model openai:gpt-5.4")
	}
}

func TestSendMessage_RecordsCurrentModelUse(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	m := newTestChatModel(false)
	m.providerKey = "claude-bin"
	m.modelName = "opus"

	_, _ = m.sendMessage("hello")
	config.FlushModelHistoryAsync()

	history, err := config.LoadModelHistory()
	if err != nil {
		t.Fatalf("LoadModelHistory: %v", err)
	}
	if len(history) == 0 || history[0].Model != "claude-bin:opus" {
		t.Fatalf("expected claude-bin:opus in model history, got %v", history)
	}
}

func TestCmdReload_QuitsWithoutPrintingStatus(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "sess-reload-1"}

	result, cmd := m.cmdReload()
	rm := result.(*Model)

	if !rm.quitting {
		t.Fatal("expected /reload to request quit")
	}
	if !rm.WantsReload() {
		t.Fatal("expected /reload to mark reload requested")
	}
	if got := rm.ReloadSessionID(); got != "sess-reload-1" {
		t.Fatalf("ReloadSessionID() = %q, want %q", got, "sess-reload-1")
	}
	if cmd == nil {
		t.Fatal("expected quit command")
	}
	if _, isBatch := cmd().(tea.BatchMsg); isBatch {
		t.Fatal("expected /reload to quit without printing a status line")
	}
}

func TestCmdCompress_StartDoesNotPrintStatusLine(t *testing.T) {
	m := newTestChatModel(false)
	m.messages = []session.Message{
		{
			SessionID:   m.sess.ID,
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "hello"}},
			TextContent: "hello",
			CreatedAt:   time.Now(),
		},
		{
			SessionID:   m.sess.ID,
			Role:        llm.RoleAssistant,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "hi"}},
			TextContent: "hi",
			CreatedAt:   time.Now(),
		},
	}

	result, cmd := m.cmdCompress()
	rm := result.(*Model)

	if !rm.streaming {
		t.Fatal("expected compaction to enter streaming mode")
	}
	if got := rm.phase; got != llm.PhaseCompacting {
		t.Fatalf("phase = %q, want %q", got, llm.PhaseCompacting)
	}
	if cmd == nil {
		t.Fatal("expected compaction start command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected compaction to return a batch command, got %T", cmd())
	}
	if len(batch) != 3 {
		t.Fatalf("expected compaction start batch to avoid print command, got %d entries", len(batch))
	}
}

func TestCmdCompressSoftDefaultUsesBriefPrompt(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTextResponse("soft brief")
	m := newTestChatModel(false)
	m.provider = provider
	m.engine = llm.NewEngine(provider, nil)
	m.messages = []session.Message{
		*session.NewMessage(m.sess.ID, llm.UserText("please continue"), 0),
		*session.NewMessage(m.sess.ID, llm.AssistantText("working"), 1),
	}

	result, cmd := m.ExecuteCommand("/compact")
	rm := result.(*Model)
	if !rm.streaming {
		t.Fatal("expected /compact to start streaming soft compaction")
	}
	if got := rm.phase; got != llm.PhaseCompacting {
		t.Fatalf("phase = %q, want compacting phase", got)
	}
	batch := commandBatch(t, cmd)
	msg := batch[0]()
	done, ok := msg.(compactDoneMsg)
	if !ok {
		t.Fatalf("first command returned %T, want compactDoneMsg", msg)
	}
	if done.err != nil {
		t.Fatalf("soft compact command error = %v", done.err)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(provider.Requests))
	}
	last := llm.MessageText(provider.Requests[0].Messages[len(provider.Requests[0].Messages)-1])
	if !strings.Contains(last, "compact continuation brief") {
		t.Fatalf("/compact default did not use soft brief prompt: %q", last)
	}
	if strings.Contains(last, "Create a detailed summary") {
		t.Fatalf("/compact default unexpectedly used hard compaction prompt: %q", last)
	}
	if done.result == nil || !strings.Contains(done.result.Summary, "<PREVIOUS_TURNS>") || !strings.Contains(done.result.Summary, "<SUMMARY_AND_NEXT_ACTIONS>") || !strings.Contains(done.result.Summary, "soft brief") {
		t.Fatalf("soft result summary = %#v", done.result)
	}
}

func TestCmdCompressHardUsesFullCompactionPrompt(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTextResponse("hard summary")
	m := newTestChatModel(false)
	m.provider = provider
	m.engine = llm.NewEngine(provider, nil)
	m.messages = []session.Message{
		*session.NewMessage(m.sess.ID, llm.UserText("please continue"), 0),
		*session.NewMessage(m.sess.ID, llm.AssistantText("working"), 1),
	}

	result, cmd := m.ExecuteCommand("/compact hard")
	rm := result.(*Model)
	if !rm.streaming {
		t.Fatal("expected /compact hard to start streaming hard compaction")
	}
	if got := rm.phase; got != llm.PhaseCompactingSummarizeHistory {
		t.Fatalf("phase = %q, want hard summary phase", got)
	}
	batch := commandBatch(t, cmd)
	msg := batch[0]()
	done, ok := msg.(compactDoneMsg)
	if !ok {
		t.Fatalf("first command returned %T, want compactDoneMsg", msg)
	}
	if done.err != nil {
		t.Fatalf("hard compact command error = %v", done.err)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(provider.Requests))
	}
	last := llm.MessageText(provider.Requests[0].Messages[len(provider.Requests[0].Messages)-1])
	if !strings.Contains(last, "Create a compact continuation brief") || !strings.Contains(last, "## Objective") {
		t.Fatalf("/compact hard did not use structured hard compaction prompt: %q", last)
	}
	if done.result == nil || !strings.Contains(done.result.Summary, "hard summary") || !strings.Contains(done.result.Summary, "<SUMMARY_AND_NEXT_ACTIONS>") {
		t.Fatalf("hard result summary = %#v", done.result)
	}
}

func TestCmdCompressInvalidArgShowsUsage(t *testing.T) {
	m := newTestChatModel(false)
	m.messages = []session.Message{
		*session.NewMessage(m.sess.ID, llm.UserText("please continue"), 0),
		*session.NewMessage(m.sess.ID, llm.AssistantText("working"), 1),
	}

	result, cmd := m.ExecuteCommand("/compact banana")
	rm := result.(*Model)
	if rm.streaming {
		t.Fatal("invalid /compact arg should not start streaming")
	}
	if got := rm.footerMessage; got != "Usage: /compact [hard]" {
		t.Fatalf("footer = %q, want usage", got)
	}
	if cmd == nil {
		t.Fatal("expected footer clear command")
	}
}

func commandBatch(t *testing.T, cmd tea.Cmd) tea.BatchMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected command")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected batch command, got %T", msg)
	}
	if len(batch) == 0 {
		t.Fatalf("expected non-empty batch")
	}
	return batch
}

func TestUpdate_CompactDone_CancelUsesMutedFooterMessage(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true

	result, cmd := m.Update(compactDoneMsg{err: context.Canceled})
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Compaction cancelled." {
		t.Fatalf("expected compact cancel footer message, got %q", got)
	}
	if got := rm.footerMessageTone; got != "muted" {
		t.Fatalf("expected compact cancel footer tone muted, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected compact cancel footer clear command")
	}
}

func TestUpdate_CompactDone_SuccessUsesFooterMessage(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true

	result, cmd := m.Update(compactDoneMsg{result: &llm.CompactionResult{NewMessages: []llm.Message{llm.UserText("summary")}}})
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Conversation compacted." {
		t.Fatalf("expected compact success footer message, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected compact success footer clear command")
	}
}

func TestUpdate_CompactDone_PersistErrorDoesNotMutateMemory(t *testing.T) {
	store := &mockStore{compactErr: errors.New("disk full")}
	m := newTestChatModel(false)
	m.store = store
	m.sess = &session.Session{ID: "sess-compact-fail"}
	m.streaming = true
	m.messages = []session.Message{
		*session.NewMessage(m.sess.ID, llm.UserText("old user"), 0),
		*session.NewMessage(m.sess.ID, llm.AssistantText("old assistant"), 1),
	}
	oldMessages := append([]session.Message(nil), m.messages...)

	result, cmd := m.Update(compactDoneMsg{result: &llm.CompactionResult{NewMessages: []llm.Message{llm.UserText("summary")}}})
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Compaction finished, but saving failed: disk full" {
		t.Fatalf("expected compact persist error footer message, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected compact persist error footer clear command")
	}
	if rm.compactionIdx != 0 {
		t.Fatalf("compactionIdx changed on persist failure: got %d want 0", rm.compactionIdx)
	}
	if len(rm.messages) != len(oldMessages) {
		t.Fatalf("messages changed on persist failure: got %d want %d", len(rm.messages), len(oldMessages))
	}
	for i := range oldMessages {
		if rm.messages[i].TextContent != oldMessages[i].TextContent || rm.messages[i].SessionID != oldMessages[i].SessionID {
			t.Fatalf("message %d changed on persist failure: got %#v want %#v", i, rm.messages[i], oldMessages[i])
		}
	}
}

func TestCmdHandover_StartDoesNotPrintStatusLine(t *testing.T) {
	m := newTestChatModel(false)
	m.store = &mockStore{}
	m.sess = &session.Session{ID: "sess-handover-start", CreatedAt: time.Now()}
	m.messages = []session.Message{
		{
			SessionID:   m.sess.ID,
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "please continue"}},
			TextContent: "please continue",
			CreatedAt:   time.Now(),
		},
		{
			SessionID:   m.sess.ID,
			Role:        llm.RoleAssistant,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "Working on it."}},
			TextContent: "Working on it.",
			CreatedAt:   time.Now(),
		},
	}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return &agents.Agent{Name: name, SystemPrompt: "You are target."}, nil
	}

	result, cmd := m.cmdHandover([]string{"@target"})
	rm := result.(*Model)

	if !rm.streaming {
		t.Fatal("expected handover generation to enter streaming mode")
	}
	if got := rm.phase; got != "Handover" {
		t.Fatalf("phase = %q, want %q", got, "Handover")
	}
	if cmd == nil {
		t.Fatal("expected handover start command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected handover to return a batch command, got %T", cmd())
	}
	if len(batch) != 3 {
		t.Fatalf("expected handover start batch to avoid print command, got %d entries", len(batch))
	}
}

func TestCmdHandover_LightModeResultCarriesContextOnly(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "sess-handover-light", CreatedAt: time.Now()}
	m.agentName = "source"
	m.currentAgent = &agents.Agent{Name: "source", HandoverMode: "light"}
	m.messages = []session.Message{
		*session.NewMessage(m.sess.ID, llm.UserText("what next?"), 0),
		*session.NewMessage(m.sess.ID, llm.AssistantText("handover this update"), 1),
	}
	targetAgent := &agents.Agent{Name: "target", SystemPrompt: "You are target."}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return targetAgent, nil
	}

	_, cmd := m.cmdHandover([]string{"@target"})
	if cmd == nil {
		t.Fatal("expected light handover command")
	}
	msg, ok := cmd().(handoverDoneMsg)
	if !ok {
		t.Fatalf("handover command returned %T, want handoverDoneMsg", cmd())
	}
	if msg.result == nil || len(msg.result.NewMessages) == 0 {
		t.Fatalf("expected handover result messages, got %#v", msg.result)
	}
	if msg.result.NewMessages[0].Role == llm.RoleSystem {
		t.Fatalf("intermediate handover result should not carry target system prompt: %#v", msg.result.NewMessages[0])
	}
	if msg.result.NewMessages[0].Role != llm.RoleUser || !strings.Contains(llm.MessageText(msg.result.NewMessages[0]), "handover this update") {
		t.Fatalf("first handover result message should be user context, got %#v", msg.result.NewMessages[0])
	}
}

func TestStartHandoverScriptHandover_DoesNotPrintStatusLine(t *testing.T) {
	m := newTestChatModel(false)
	targetAgent := &agents.Agent{Name: "target", SystemPrompt: "You are target.", HandoverScript: "./handover.sh"}

	result, cmd := m.startHandoverScriptHandover(targetAgent, "source", targetAgent, "", false, "")
	rm := result.(*Model)

	if !rm.streaming {
		t.Fatal("expected script-backed handover to enter streaming mode")
	}
	if got := rm.phase; got != "Handover" {
		t.Fatalf("phase = %q, want %q", got, "Handover")
	}
	if cmd == nil {
		t.Fatal("expected script-backed handover command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected script-backed handover to return a batch command, got %T", cmd())
	}
	if len(batch) != 3 {
		t.Fatalf("expected script-backed handover batch to avoid print command, got %d entries", len(batch))
	}
}

func TestUpdate_HandoverDone_CancelUsesMutedFooterMessage(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true

	result, cmd := m.Update(handoverDoneMsg{err: context.Canceled, agentName: "target"})
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Handover cancelled." {
		t.Fatalf("expected handover cancel footer message, got %q", got)
	}
	if got := rm.footerMessageTone; got != "muted" {
		t.Fatalf("expected handover cancel footer tone muted, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected handover cancel footer clear command")
	}
}

func TestUpdate_HandoverDone_ErrorUsesFooterMessage(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true

	result, cmd := m.Update(handoverDoneMsg{err: errors.New("boom"), agentName: "target"})
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Handover failed: boom" {
		t.Fatalf("expected handover error footer message, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected handover error footer clear command")
	}
}

func TestExecuteHandover_ResolveErrorUsesFooterMessage(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.config = &config.Config{}
	m.pendingHandover = &handoverDoneMsg{agentName: "target"}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return nil, errors.New("missing target")
	}

	result, cmd := m.executeHandover()
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Handover failed to resolve target agent: missing target" {
		t.Fatalf("expected executeHandover resolve error in footer, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected executeHandover error footer clear command")
	}
}

func TestExecuteHandover_CreateErrorUsesFooterMessage(t *testing.T) {
	store := &mockStore{createErr: errors.New("disk full")}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "sess-handover-persist"}
	targetAgent := &agents.Agent{Name: "target", SystemPrompt: "You are target."}
	expectedResult := llm.HandoverFromFile("handover doc", targetAgent.SystemPrompt, "source", targetAgent.Name)
	m.pendingHandover = &handoverDoneMsg{
		agentName: "target",
		result:    expectedResult,
	}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return targetAgent, nil
	}

	result, cmd := m.executeHandover()
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Handover failed to persist: disk full" {
		t.Fatalf("expected executeHandover persist error in footer, got %q", got)
	}
	if got := rm.footerMessageTone; got != "error" {
		t.Fatalf("expected executeHandover persist error tone error, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected executeHandover persist error footer clear command")
	}
	if len(store.compacted) != 0 {
		t.Fatalf("handover must not compact source session, compacted %d messages", len(store.compacted))
	}
	if len(store.statusUpdates) != 0 {
		t.Fatalf("source session should not be marked complete on create failure, got %#v", store.statusUpdates)
	}
	if rm.pendingHandover == nil {
		t.Fatal("expected pending handover to remain available for retry")
	}
}

func TestExecuteHandover_CreatesNewIsolatedSessionAndRequestsResume(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	worktreeDir := filepath.Join(t.TempDir(), "bound-worktree")
	oldSess := &session.Session{
		ID:          "old-session",
		Provider:    "Old Provider",
		ProviderKey: "old-provider",
		Model:       "old-model",
		Agent:       "source",
		CWD:         worktreeDir,
		WorktreeDir: worktreeDir,
		Search:      false,
		Tools:       "read_file",
		MCP:         "old-mcp",
		CreatedAt:   time.Now().Add(-time.Hour),
	}
	m.sess = oldSess
	m.providerName = "Old Provider"
	m.providerKey = "old-provider"
	m.modelName = "old-model"
	m.messages = []session.Message{
		*session.NewMessage(oldSess.ID, llm.UserText("old user content"), 0),
		*session.NewMessage(oldSess.ID, llm.AssistantText("old assistant content"), 1),
	}
	oldMessages := append([]session.Message(nil), m.messages...)

	targetAgent := &agents.Agent{
		Name:          "target",
		SystemPrompt:  "You are target.",
		Provider:      "gemini",
		Model:         "gemini-2.5-pro",
		Search:        true,
		DefaultPrompt: "Continue from handover.",
		AgentsMd:      "false",
		Tools:         agents.ToolsConfig{Enabled: []string{"read_file", "edit_file"}},
		MCP:           []agents.MCPConfig{{Name: "server-a"}, {Name: "server-b"}},
	}
	expectedResult := llm.HandoverFromFile("handover doc", targetAgent.SystemPrompt, "source", targetAgent.Name)
	m.pendingHandover = &handoverDoneMsg{
		agentName: "target",
		result:    expectedResult,
	}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		if name != "target" {
			t.Fatalf("resolver called with %q, want target", name)
		}
		return targetAgent, nil
	}

	result, cmd := m.executeHandover()
	rm := result.(*Model)

	if cmd == nil {
		t.Fatal("expected executeHandover to quit for relaunch")
	}
	if !rm.quitting {
		t.Fatal("expected model to be quitting for handover relaunch")
	}
	if len(store.created) != 1 {
		t.Fatalf("expected exactly one new session, got %d", len(store.created))
	}
	newSess := store.created[0]
	if newSess.ID == "" || newSess.ID == oldSess.ID {
		t.Fatalf("expected a fresh session ID distinct from %q, got %q", oldSess.ID, newSess.ID)
	}
	if got := rm.RequestedResumeSessionID(); got != newSess.ID {
		t.Fatalf("resume session ID = %q, want new session %q", got, newSess.ID)
	}
	if store.currentID != newSess.ID {
		t.Fatalf("current session = %q, want %q", store.currentID, newSess.ID)
	}
	if newSess.Agent != "target" {
		t.Fatalf("new session agent = %q, want target", newSess.Agent)
	}
	if !newSess.Search {
		t.Fatal("expected target search setting on new session")
	}
	if newSess.Tools != "read_file,edit_file" {
		t.Fatalf("new session tools = %q, want read_file,edit_file", newSess.Tools)
	}
	if newSess.MCP != "server-a,server-b" {
		t.Fatalf("new session MCP = %q, want server-a,server-b", newSess.MCP)
	}
	if newSess.ProviderKey != "gemini" || newSess.Model != "gemini-2.5-pro" {
		t.Fatalf("new provider/model = %q/%q, want gemini/gemini-2.5-pro", newSess.ProviderKey, newSess.Model)
	}
	if newSess.Provider == "Old Provider" || newSess.Provider == "" {
		t.Fatalf("new provider label = %q, want target provider label", newSess.Provider)
	}
	if newSess.WorktreeDir != worktreeDir || newSess.CWD != worktreeDir {
		t.Fatalf("handover worktree/cwd = %q/%q, want %q", newSess.WorktreeDir, newSess.CWD, worktreeDir)
	}
	if newSess.Status != session.StatusActive {
		t.Fatalf("new session status = %q, want active", newSess.Status)
	}
	if newSess.CompactionSeq != -1 {
		t.Fatalf("new session compaction seq = %d, want -1", newSess.CompactionSeq)
	}
	if len(store.statusUpdates) != 1 || store.statusUpdates[0].id != oldSess.ID || store.statusUpdates[0].status != session.StatusComplete {
		t.Fatalf("expected source session to be marked complete, got %#v", store.statusUpdates)
	}
	if len(store.compacted) != 0 || store.compactSession != "" {
		t.Fatalf("handover must not compact any session, compactSession=%q compacted=%d", store.compactSession, len(store.compacted))
	}
	if len(store.added) != len(expectedResult.NewMessages) {
		t.Fatalf("expected %d reconstructed messages to be added, got %d", len(expectedResult.NewMessages), len(store.added))
	}
	for i, msg := range store.added {
		if msg.SessionID != newSess.ID {
			t.Fatalf("added message %d session ID = %q, want %q", i, msg.SessionID, newSess.ID)
		}
	}
	if store.added[0].Role != llm.RoleSystem || store.added[0].TextContent != targetAgent.SystemPrompt {
		t.Fatalf("first added message = role %q text %q, want target system prompt", store.added[0].Role, store.added[0].TextContent)
	}
	if !strings.Contains(store.added[1].TextContent, "handover doc") || !strings.Contains(store.added[1].TextContent, "@source -> @target") {
		t.Fatalf("handover message missing document/source-target prefix: %q", store.added[1].TextContent)
	}
	if len(rm.messages) != len(oldMessages) {
		t.Fatalf("old in-memory messages length changed: got %d want %d", len(rm.messages), len(oldMessages))
	}
	for i := range oldMessages {
		if rm.messages[i].SessionID != oldMessages[i].SessionID || rm.messages[i].TextContent != oldMessages[i].TextContent {
			t.Fatalf("old in-memory message %d changed: got %#v want %#v", i, rm.messages[i], oldMessages[i])
		}
	}
	if oldSess.Agent != "source" || oldSess.ProviderKey != "old-provider" || oldSess.Model != "old-model" || oldSess.Tools != "read_file" || oldSess.MCP != "old-mcp" || oldSess.WorktreeDir != worktreeDir || oldSess.CWD != worktreeDir {
		t.Fatalf("source session metadata was mutated: %#v", oldSess)
	}
	if rm.pendingHandover != nil {
		t.Fatal("expected successful handover to clear pending handover")
	}
}

func TestExecuteHandover_PersistsResolvedTargetSystemPromptWithAgentsMdFirst(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	oldSess := &session.Session{
		ID:          "old-session",
		ProviderKey: "old-provider",
		Model:       "old-model",
		Agent:       "source",
		CreatedAt:   time.Now().Add(-time.Hour),
	}
	m.sess = oldSess
	m.providerKey = "old-provider"
	m.modelName = "old-model"

	targetAgent := &agents.Agent{
		Name:         "target",
		SystemPrompt: "raw target prompt without project instructions",
		AgentsMd:     "true",
	}
	if !targetAgent.ShouldLoadProjectInstructions() {
		t.Fatal("test target agent should request AGENTS.md/project instruction loading")
	}

	expectedResult := llm.HandoverFromFile("handover doc", "stale raw prompt from handover result", "source", targetAgent.Name)
	m.pendingHandover = &handoverDoneMsg{
		agentName: "target",
		result:    expectedResult,
	}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		if name != "target" {
			t.Fatalf("resolver called with %q, want target", name)
		}
		return targetAgent, nil
	}
	wantPrompt := targetAgent.SystemPrompt + "\n\n---\n\nAGENTS.md instructions"
	m.handoverSystemPromptResolver = func(agent *agents.Agent, providerKey, modelName string) (string, error) {
		if agent != targetAgent {
			t.Fatalf("system prompt resolver agent = %#v, want target agent", agent)
		}
		if providerKey != "old-provider" || modelName != "old-model" {
			t.Fatalf("system prompt resolver provider/model = %q/%q, want old-provider/old-model", providerKey, modelName)
		}
		return wantPrompt, nil
	}

	result, cmd := m.executeHandover()
	rm := result.(*Model)

	if cmd == nil {
		t.Fatal("expected executeHandover to quit for relaunch")
	}
	if !rm.quitting {
		t.Fatal("expected model to be quitting for handover relaunch")
	}
	if len(store.created) != 1 {
		t.Fatalf("expected exactly one new session, got %d", len(store.created))
	}
	if len(store.added) != len(expectedResult.NewMessages) {
		t.Fatalf("expected %d handover context messages to be persisted, got %d", len(expectedResult.NewMessages), len(store.added))
	}

	if store.added[0].Role != llm.RoleSystem {
		t.Fatalf("first persisted message role = %q, want system", store.added[0].Role)
	}
	if store.added[0].TextContent != wantPrompt {
		t.Fatalf("first persisted system prompt = %q, want resolved target prompt %q", store.added[0].TextContent, wantPrompt)
	}
	if strings.Contains(store.added[0].TextContent, "stale raw prompt") {
		t.Fatalf("persisted system prompt used stale handover result prompt: %q", store.added[0].TextContent)
	}
	if store.added[0].Sequence != 0 {
		t.Fatalf("first persisted system sequence = %d, want 0", store.added[0].Sequence)
	}
	if len(store.added) < 2 || store.added[1].Role != llm.RoleUser || !strings.Contains(store.added[1].TextContent, "handover doc") {
		t.Fatalf("expected handover document immediately after system prompt, got %#v", store.added)
	}

	active := session.LLMActiveMessages(store.added, 0, wantPrompt)
	if len(active) != len(store.added) {
		t.Fatalf("active message count = %d, want %d", len(active), len(store.added))
	}
	if active[0].Role != llm.RoleSystem || llm.MessageText(active[0]) != wantPrompt {
		t.Fatalf("active first message = role %q text %q, want resolved target system prompt", active[0].Role, llm.MessageText(active[0]))
	}
	systemCount := 0
	for _, msg := range active {
		if msg.Role == llm.RoleSystem {
			systemCount++
		}
	}
	if systemCount != 1 {
		t.Fatalf("active messages contain %d system prompts, want exactly 1: %#v", systemCount, active)
	}
}

func TestExecuteHandover_PersistsAgentsMdSystemPromptWhenTargetPromptEmpty(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	m.sess = &session.Session{
		ID:          "old-session",
		ProviderKey: "old-provider",
		Model:       "old-model",
		Agent:       "source",
		CreatedAt:   time.Now().Add(-time.Hour),
	}
	m.providerKey = "old-provider"
	m.modelName = "old-model"

	targetAgent := &agents.Agent{
		Name:     "target",
		AgentsMd: "true",
	}
	m.pendingHandover = &handoverDoneMsg{
		agentName: "target",
		result:    llm.HandoverFromFile("handover doc", "", "source", targetAgent.Name),
	}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		if name != "target" {
			t.Fatalf("resolver called with %q, want target", name)
		}
		return targetAgent, nil
	}
	m.handoverSystemPromptResolver = func(agent *agents.Agent, providerKey, modelName string) (string, error) {
		if agent != targetAgent {
			t.Fatalf("system prompt resolver agent = %#v, want target agent", agent)
		}
		return "AGENTS-only instructions", nil
	}

	result, cmd := m.executeHandover()
	rm := result.(*Model)
	if cmd == nil || !rm.quitting {
		t.Fatal("expected executeHandover to request relaunch")
	}
	if len(store.added) != 3 {
		t.Fatalf("expected system + handover user + ack to be persisted, got %d messages: %#v", len(store.added), store.added)
	}
	if store.added[0].Role != llm.RoleSystem || store.added[0].TextContent != "AGENTS-only instructions" {
		t.Fatalf("first persisted message = role %q text %q, want AGENTS.md-only system prompt", store.added[0].Role, store.added[0].TextContent)
	}
	if store.added[0].Sequence != 0 {
		t.Fatalf("first persisted system sequence = %d, want 0", store.added[0].Sequence)
	}
	if store.added[1].Role != llm.RoleUser || !strings.Contains(store.added[1].TextContent, "handover doc") {
		t.Fatalf("expected handover document immediately after system prompt, got %#v", store.added[1])
	}
}

func TestExecuteHandover_ProviderOverrideStoredOnNewSession(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "old-session", ProviderKey: "old-provider", Model: "old-model"}
	m.providerName = "Old Provider"
	m.providerKey = "old-provider"
	m.modelName = "old-model"
	targetAgent := &agents.Agent{Name: "target", SystemPrompt: "You are target.", Provider: "gemini", Model: "gemini-2.5-pro"}
	m.pendingHandover = &handoverDoneMsg{
		agentName:   "target",
		providerStr: "openai:gpt-5",
		result:      llm.HandoverFromFile("handover doc", targetAgent.SystemPrompt, "source", targetAgent.Name),
	}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return targetAgent, nil
	}

	result, _ := m.executeHandover()
	rm := result.(*Model)

	if rm.footerMessage != "" {
		t.Fatalf("unexpected footer error: %s", rm.footerMessage)
	}
	if len(store.created) != 1 {
		t.Fatalf("expected one created session, got %d", len(store.created))
	}
	newSess := store.created[0]
	if newSess.ProviderKey != "openai" || newSess.Model != "gpt-5" {
		t.Fatalf("new provider/model = %q/%q, want openai/gpt-5", newSess.ProviderKey, newSess.Model)
	}
	if newSess.Provider == "Old Provider" || !strings.Contains(newSess.Provider, "openai") {
		t.Fatalf("new provider label = %q, want openai label", newSess.Provider)
	}
}

func TestExecuteHandover_AddMessageErrorUsesFooterMessage(t *testing.T) {
	store := &mockStore{addErr: errors.New("write failed")}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "old-session"}
	targetAgent := &agents.Agent{Name: "target", SystemPrompt: "You are target."}
	m.pendingHandover = &handoverDoneMsg{
		agentName: "target",
		result:    llm.HandoverFromFile("handover doc", targetAgent.SystemPrompt, "source", targetAgent.Name),
	}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return targetAgent, nil
	}

	result, cmd := m.executeHandover()
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Handover failed to persist: write failed" {
		t.Fatalf("expected add-message error in footer, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected footer clear command")
	}
	if rm.quitting {
		t.Fatal("should not quit when message persistence fails")
	}
	if store.currentID != "" {
		t.Fatalf("should not set current session on failed handover, got %q", store.currentID)
	}
	if len(store.statusUpdates) != 0 {
		t.Fatalf("source session should not be marked complete on failed handover, got %#v", store.statusUpdates)
	}
	if len(store.created) != 1 {
		t.Fatalf("expected target session to have been created before add failure, got %d", len(store.created))
	}
	if len(store.deleted) != 1 || store.deleted[0] != store.created[0].ID {
		t.Fatalf("expected failed target session cleanup, deleted=%#v created=%q", store.deleted, store.created[0].ID)
	}
	if rm.pendingHandover == nil {
		t.Fatal("expected pending handover to remain available for retry")
	}
}

func TestShowSystemMessage_SingleLineUsesTransientFooter(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.setTextareaValue("/search")

	result, cmd := m.showSystemMessage("Web search enabled.")
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Web search enabled." {
		t.Fatalf("expected footer message %q, got %q", "Web search enabled.", got)
	}
	if got := rm.textarea.Value(); got != "" {
		t.Fatalf("expected showSystemMessage to clear the composer, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected transient footer clear command")
	}

	updated, _ := rm.Update(footerMessageClearMsg{Seq: rm.footerMessageSeq})
	cleared := updated.(*Model)
	if cleared.footerMessage != "" {
		t.Fatalf("expected footer message to clear after timer, got %q", cleared.footerMessage)
	}
}

func TestShowSystemMessage_SingleLineStripsSimpleMarkdownForFooter(t *testing.T) {
	m := newCmdTestModel(&mockStore{})

	result, _ := m.showSystemMessage("Approved directory: `/tmp/demo`")
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Approved directory: /tmp/demo" {
		t.Fatalf("expected footer markdown to be stripped, got %q", got)
	}
}

func TestShowSystemMessage_MultilineFallsBackToScrollback(t *testing.T) {
	m := newCmdTestModel(&mockStore{})

	result, cmd := m.showSystemMessage("## Help\n\nUse `/help` for commands.")
	rm := result.(*Model)

	if got := rm.footerMessage; got != "" {
		t.Fatalf("expected multiline system message to avoid footer, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected multiline system message to return a print command")
	}
}

func TestShowSystemMessage_NewFooterMessageReplacesOlderTimer(t *testing.T) {
	m := newCmdTestModel(&mockStore{})

	result, _ := m.showSystemMessage("First message")
	rm := result.(*Model)
	firstSeq := rm.footerMessageSeq

	result, _ = rm.showSystemMessage("Second message")
	rm = result.(*Model)
	secondSeq := rm.footerMessageSeq
	if secondSeq <= firstSeq {
		t.Fatalf("expected footer message sequence to advance, got first=%d second=%d", firstSeq, secondSeq)
	}

	updated, _ := rm.Update(footerMessageClearMsg{Seq: firstSeq})
	stillVisible := updated.(*Model)
	if got := stillVisible.footerMessage; got != "Second message" {
		t.Fatalf("expected stale timer to preserve latest footer message, got %q", got)
	}
}

func TestSwitchModel_UpdatesSessionMetadata(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	m.sess = &session.Session{
		ID:       "sess-model-switch-1",
		Provider: "OpenAI (old-model)",
		Model:    "old-model",
	}
	m.providerName = "OpenAI (old-model)"
	m.modelName = "old-model"
	m.engine = llm.NewEngine(llm.NewMockProvider("old"), nil)
	m.setTextareaValue("/model debug:fast")

	result, cmd := m.switchModel("debug:fast")
	rm := result.(*Model)

	if rm.sess.Provider != "debug:fast" {
		t.Fatalf("expected session provider to be updated to %q, got %q", "debug:fast", rm.sess.Provider)
	}
	if rm.sess.Model != "fast" {
		t.Fatalf("expected session model to be updated to %q, got %q", "fast", rm.sess.Model)
	}
	if store.updated == nil {
		t.Fatal("expected switchModel to persist session changes")
	}
	if got := rm.textarea.Value(); got != "" {
		t.Fatalf("expected switchModel to clear the composer, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected switchModel success to show a model-switch notice")
	}
	if !strings.Contains(rm.footerMessage, "Switched model to debug:fast") {
		t.Fatalf("expected footer model-switch notice, got %q", rm.footerMessage)
	}
}

func TestSwitchModel_WithExistingHistoryPersistsModelSwapEventMarker(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "sess-model-switch-marker", Provider: "old", ProviderKey: "old", Model: "old-model"}
	m.providerName = "old"
	m.providerKey = "old"
	m.modelName = "old-model"
	m.engine = llm.NewEngine(llm.NewMockProvider("old"), nil)
	m.messages = []session.Message{*session.NewMessage(m.sess.ID, llm.UserText("hello"), 0)}
	m.altScreen = true
	m.viewCache.completedStream = "cached previous assistant"
	m.viewCache.historyValid = true

	result, _ := m.switchModel("debug:fast")
	rm := result.(*Model)
	if len(rm.messages) != 2 {
		t.Fatalf("messages len = %d, want user + event marker", len(rm.messages))
	}
	markerMsg := rm.messages[1]
	if markerMsg.Role != llm.RoleEvent {
		t.Fatalf("marker role = %q, want event", markerMsg.Role)
	}
	marker, ok := llm.ParseModelSwapMarker(markerMsg.ToLLMMessage())
	if !ok {
		t.Fatalf("failed to parse model-swap marker: %#v", markerMsg)
	}
	if marker.FromProvider != "old" || marker.FromModel != "old-model" || marker.ToProvider != "debug" || marker.ToModel != "fast" || marker.Status != "started" {
		t.Fatalf("unexpected marker: %#v", marker)
	}
	if len(store.added) != 1 || store.added[0].Role != llm.RoleEvent {
		t.Fatalf("expected store AddMessage event marker, got %#v", store.added)
	}
	if rm.viewCache.completedStream != "" {
		t.Fatalf("expected model switch to clear completed stream cache, got %q", rm.viewCache.completedStream)
	}
	if rm.viewCache.historyValid {
		t.Fatal("expected model switch to invalidate history cache")
	}
	built := rm.buildMessages()
	for _, msg := range built {
		if msg.Role == llm.RoleEvent {
			t.Fatalf("buildMessages included event marker in provider context: %#v", built)
		}
	}
}

func TestResumeFormatAge(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"just now", now.Add(-30 * time.Second), "just now"},
		{"minutes", now.Add(-5 * time.Minute), "5m ago"},
		{"hours", now.Add(-2 * time.Hour), "2h ago"},
		{"days", now.Add(-2 * 24 * time.Hour), "2d ago"},
		{"old date", time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), "Jan 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resumeFormatAge(tt.t)
			if got != tt.want {
				t.Errorf("resumeFormatAge() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCmdHandover_TargetScriptIsDeferredUntilConfirmation(t *testing.T) {
	agentDir := t.TempDir()
	scriptPath := filepath.Join(agentDir, "handover.sh")
	markerPath := filepath.Join(agentDir, "marker")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nprintf 'generated handover'\nprintf 'script warning' >&2\ntouch marker\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	targetAgent := &agents.Agent{
		Name:           "target",
		SystemPrompt:   "You are target.",
		HandoverScript: "./handover.sh",
		Source:         agents.SourceLocal,
		SourcePath:     agentDir,
	}

	m := newCmdTestModel(&mockStore{})
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "sess-handover-preview", CreatedAt: time.Now()}
	m.agentName = "source"
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return targetAgent, nil
	}
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	mgr.SetYoloMode(true)
	m.SetHandoverApprovalManager(mgr)

	result, cmd := m.cmdHandover([]string{"@target"})
	if cmd == nil {
		t.Fatal("expected preview command")
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("script should not run before preview, stat err = %v", err)
	}

	updated, _ := result.(*Model).Update(cmd())
	rm := updated.(*Model)
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("script should remain deferred until confirmation, stat err = %v", err)
	}
	if rm.pendingHandover == nil || rm.handoverPreview == nil {
		t.Fatal("expected pending handover preview to be shown")
	}
	if !strings.Contains(rm.pendingHandover.result.Document, "will run only after you confirm") {
		t.Fatalf("expected deferred-script preview, got %q", rm.pendingHandover.result.Document)
	}
}

func TestHandoverScriptCmd_UsesAgentSourcePathAndPersistsResult(t *testing.T) {
	agentDir := t.TempDir()

	targetAgent := &agents.Agent{
		Name:           "target",
		SystemPrompt:   "You are target.",
		DefaultPrompt:  "  review changes  ",
		HandoverScript: "./handover.sh",
		Source:         agents.SourceLocal,
		SourcePath:     agentDir,
	}
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "sess-handover-confirm", CreatedAt: time.Now()}
	m.agentName = "source"
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return targetAgent, nil
	}
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	mgr.SetYoloMode(true)
	m.yolo = true
	m.SetHandoverApprovalManager(mgr)
	m.pendingHandover = &handoverDoneMsg{
		result:    llm.HandoverFromFile("placeholder", targetAgent.SystemPrompt, "source", targetAgent.Name),
		agentName: targetAgent.Name,
	}

	runnerCalled := false
	origRunner := runHandoverScriptForCmd
	runHandoverScriptForCmd = func(ctx context.Context, approvalMgr *tools.ApprovalManager, agent *agents.Agent, script string, transcript []tools.TranscriptEntry) (string, error) {
		runnerCalled = true
		if agent.SourcePath != agentDir {
			t.Fatalf("runner agent SourcePath = %q, want %q", agent.SourcePath, agentDir)
		}
		if script != "./handover.sh" {
			t.Fatalf("runner script = %q", script)
		}
		return "generated handover", nil
	}
	t.Cleanup(func() { runHandoverScriptForCmd = origRunner })

	msg := handoverScriptCmd(context.Background(), mgr, targetAgent, "source", targetAgent, "", true, "please focus on tests")()
	updated, quitCmd := m.Update(msg)
	rm := updated.(*Model)

	if quitCmd == nil {
		t.Fatal("expected handover to request quit after confirmation")
	}
	if !runnerCalled {
		t.Fatal("expected handover script runner to be called")
	}
	if !rm.quitting {
		t.Fatal("expected model to request restart after confirmed handover")
	}
	if got := rm.RequestedResumeSessionID(); got == "" || got == "sess-handover-confirm" {
		t.Fatalf("RequestedResumeSessionID() = %q, want a fresh target session", got)
	}
	if store.currentID != rm.RequestedResumeSessionID() {
		t.Fatalf("current session = %q, want requested resume session %q", store.currentID, rm.RequestedResumeSessionID())
	}
	if got := rm.RequestedHandoverAutoSend(); got != "review changes" {
		t.Fatalf("RequestedHandoverAutoSend() = %q, want %q", got, "review changes")
	}
	if !rm.YoloModeActive() {
		t.Fatal("expected yolo mode to remain active after confirmed handover")
	}
	if len(store.created) != 1 {
		t.Fatalf("expected one fresh handover session to be created, got %d", len(store.created))
	}
	if store.created[0].ID != rm.RequestedResumeSessionID() {
		t.Fatalf("created session ID = %q, want resume session %q", store.created[0].ID, rm.RequestedResumeSessionID())
	}
	if len(store.compacted) != 0 {
		t.Fatalf("handover must not compact the source session, compacted %d messages", len(store.compacted))
	}
	if len(store.added) == 0 {
		t.Fatal("expected handover messages to be added to fresh session")
	}
	var combined strings.Builder
	for _, msg := range store.added {
		if msg.SessionID != rm.RequestedResumeSessionID() {
			t.Fatalf("persisted message session ID = %q, want %q", msg.SessionID, rm.RequestedResumeSessionID())
		}
		if msg.TextContent == "" {
			continue
		}
		combined.WriteString(msg.TextContent)
		combined.WriteString("\n")
	}
	text := combined.String()
	if !strings.Contains(text, "generated handover") {
		t.Fatalf("expected script output in persisted messages, got %q", text)
	}
	if strings.Contains(text, "script warning") {
		t.Fatalf("expected stderr to be excluded from persisted messages, got %q", text)
	}
	if !strings.Contains(text, "Additional Instructions") || !strings.Contains(text, "please focus on tests") {
		t.Fatalf("expected additional instructions in persisted messages, got %q", text)
	}
}

func TestRunHandoverScript_CancelKillsProcessGroup(t *testing.T) {
	if os.Getenv("TERM_LLM_SLOW_TESTS") == "" {
		t.Skip("set TERM_LLM_SLOW_TESTS=1 to run process-group cancellation integration test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("process-group cancellation test is unix-specific")
	}

	agentDir := t.TempDir()
	scriptPath := filepath.Join(agentDir, "handover.sh")
	childPIDPath := filepath.Join(agentDir, "child.pid")
	script := "#!/bin/sh\nchild_pid_file=\"$1\"\n(while :; do sleep 1; done) &\necho $! > \"$child_pid_file\"\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	agent := &agents.Agent{
		Name:       "target",
		Source:     agents.SourceLocal,
		SourcePath: agentDir,
	}
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	mgr.SetYoloMode(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := runHandoverScript(ctx, mgr, agent, fmt.Sprintf("./handover.sh %q", childPIDPath), nil)
		errCh <- err
	}()

	childPID := waitForHandoverChildPID(t, childPIDPath)
	cancel()

	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runHandoverScript error = %v, want context.Canceled", err)
	}
	waitForProcessExit(t, childPID)
}

func waitForHandoverChildPID(t *testing.T, path string) int {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pidText := strings.TrimSpace(string(data))
			if pidText != "" {
				pid, convErr := strconv.Atoi(pidText)
				if convErr != nil {
					t.Fatalf("parse child pid %q: %v", pidText, convErr)
				}
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for child pid file %s", path)
	return 0
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if processHasExited(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for process %d to exit", pid)
}

func processHasExited(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err != nil {
		return errors.Is(err, syscall.ESRCH)
	}
	if runtime.GOOS == "linux" {
		state, ok := linuxProcState(pid)
		return ok && state == 'Z'
	}
	return false
}

func linuxProcState(pid int) (byte, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	return procStatState(data)
}

func procStatState(data []byte) (byte, bool) {
	stat := string(data)
	end := strings.LastIndex(stat, ")")
	if end == -1 {
		return 0, false
	}
	rest := strings.TrimSpace(stat[end+1:])
	if rest == "" {
		return 0, false
	}
	return rest[0], true
}

func TestProcStatState(t *testing.T) {
	tests := []struct {
		name string
		stat string
		want byte
		ok   bool
	}{
		{name: "running", stat: "123 (sleep) S 1 2 3", want: 'S', ok: true},
		{name: "zombie", stat: "123 (sleep) Z 1 2 3", want: 'Z', ok: true},
		{name: "command with paren", stat: "123 (odd)name) R 1 2 3", want: 'R', ok: true},
		{name: "missing close paren", stat: "123 sleep S 1 2 3", ok: false},
		{name: "missing state", stat: "123 (sleep)", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := procStatState([]byte(tt.stat))
			if ok != tt.ok || got != tt.want {
				t.Fatalf("procStatState(%q) = %q, %v; want %q, %v", tt.stat, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestStreamDoneReloadRespectsCompactionSeq(t *testing.T) {
	sessionID := "sess-compacted-reload"
	store := &mockStore{
		sessions: map[string]*session.Session{
			sessionID: {ID: sessionID, CompactionSeq: 2},
		},
		messages: map[string][]session.Message{
			sessionID: {
				*session.NewMessage(sessionID, llm.UserText("old user"), 0),
				*session.NewMessage(sessionID, llm.AssistantText("old assistant"), 1),
				*session.NewMessage(sessionID, llm.SystemText("post compact system"), 2),
				*session.NewMessage(sessionID, llm.UserText("post compact summary"), 3),
			},
		},
	}
	m := newTestChatModel(false)
	m.store = store
	m.sess = store.sessions[sessionID]
	m.streaming = true
	m.messages = append([]session.Message(nil), store.messages[sessionID]...)
	m.compactionIdx = 2

	result, _ := m.Update(streamEventMsg{event: ui.DoneEvent(42)})
	rm := result.(*Model)

	if len(rm.messages) != 4 {
		t.Fatalf("expected full scrollback after reload, got %d", len(rm.messages))
	}
	if rm.messages[0].Sequence != 0 || rm.messages[3].Sequence != 3 {
		t.Fatalf("unexpected reloaded scrollback sequences: got first=%d last=%d want 0,3", rm.messages[0].Sequence, rm.messages[3].Sequence)
	}
	if rm.compactionIdx != 2 {
		t.Fatalf("compactionIdx after scrollback reload = %d, want 2", rm.compactionIdx)
	}
	built := rm.buildMessagesForStream()
	if len(built) != 2 {
		t.Fatalf("expected next request to include 2 compacted messages, got %d", len(built))
	}
}

func TestStreamDoneRefreshFailureDoesNotReloadFullCompactedHistory(t *testing.T) {
	sessionID := "sess-compacted-refresh-fail"
	store := &mockStore{
		getErr: errors.New("db temporarily unavailable"),
		sessions: map[string]*session.Session{
			sessionID: {ID: sessionID, CompactionSeq: -1},
		},
		messages: map[string][]session.Message{
			sessionID: {
				*session.NewMessage(sessionID, llm.UserText("old user"), 0),
				*session.NewMessage(sessionID, llm.AssistantText("old assistant"), 1),
				*session.NewMessage(sessionID, llm.SystemText("post compact system"), 2),
				*session.NewMessage(sessionID, llm.UserText("post compact summary"), 3),
			},
		},
	}
	m := newTestChatModel(false)
	m.store = store
	m.sess = store.sessions[sessionID]
	m.streaming = true
	m.messages = append([]session.Message(nil), store.messages[sessionID]...)
	m.compactionIdx = 2

	result, _ := m.Update(streamEventMsg{event: ui.DoneEvent(42)})
	rm := result.(*Model)

	if len(rm.messages) != 4 {
		t.Fatalf("expected in-memory history to be preserved on refresh failure, got %d", len(rm.messages))
	}
	if rm.compactionIdx != 2 {
		t.Fatalf("compactionIdx after refresh failure = %d, want 2", rm.compactionIdx)
	}
	built := rm.buildMessagesForStream()
	if len(built) != 2 {
		t.Fatalf("expected next request to keep using compacted in-memory window, got %d messages", len(built))
	}
	if got := rm.footerMessage; !strings.Contains(got, "Session refresh failed after compaction") {
		t.Fatalf("expected refresh failure footer, got %q", got)
	}
}

func TestFastCommandTogglesWhenMetadataSupportsModel(t *testing.T) {
	m := newTestChatModel(true)
	m.providerKey = "chatgpt"
	m.modelName = "gpt-5.5-medium"
	m.fastMetadataLoaded = true
	m.modelMetadata = []llm.ModelInfo{{
		ID:           "gpt-5.5",
		ServiceTiers: []llm.ModelServiceTier{{ID: llm.ServiceTierFast, Name: "fast"}},
	}}
	m.setTextareaValue("/fast")

	_, cmd := m.ExecuteCommand("/fast")
	if cmd == nil {
		t.Fatal("expected footer clear command")
	}
	if !m.fastMode {
		t.Fatal("expected fast mode enabled")
	}
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("textarea = %q, want empty", got)
	}
	if !strings.Contains(m.footerMessage, "enabled") {
		t.Fatalf("footer = %q, want enabled message", m.footerMessage)
	}

	_, _ = m.ExecuteCommand("/fast")
	if m.fastMode {
		t.Fatal("expected fast mode disabled")
	}
}

func TestFastCommandRejectsUnsupportedModel(t *testing.T) {
	m := newTestChatModel(true)
	m.providerKey = "chatgpt"
	m.modelName = "gpt-5.5-medium"
	m.fastMetadataLoaded = true
	m.modelMetadata = []llm.ModelInfo{{ID: "gpt-5.5"}}

	_, _ = m.ExecuteCommand("/fast")
	if m.fastMode {
		t.Fatal("expected fast mode to remain disabled")
	}
	if !strings.Contains(m.footerMessage, "not supported") {
		t.Fatalf("footer = %q, want unsupported message", m.footerMessage)
	}
}

func TestFastCommandWhileMetadataLoadingKeepsPendingToggle(t *testing.T) {
	m := newTestChatModel(true)
	m.providerKey = "chatgpt"
	m.modelName = "gpt-5.5-medium"
	m.fastMetadataLoading = true

	_, _ = m.ExecuteCommand("/fast")
	if !m.pendingFastToggle {
		t.Fatal("expected pending fast toggle while metadata is loading")
	}
	if !strings.Contains(m.footerMessage, "Loading model metadata") {
		t.Fatalf("footer = %q, want loading message", m.footerMessage)
	}
}

func TestFastCommandSupportsChatGPTProviderAlias(t *testing.T) {
	m := newTestChatModel(true)
	m.config = &config.Config{Providers: map[string]config.ProviderConfig{
		"work": {Type: config.ProviderTypeChatGPT},
	}}
	m.providerKey = "work"
	m.modelName = "gpt-5.5-medium"
	m.fastMetadataLoaded = true
	m.modelMetadata = []llm.ModelInfo{{
		ID:           "gpt-5.5",
		ServiceTiers: []llm.ModelServiceTier{{ID: llm.ServiceTierFast, Name: "fast"}},
	}}

	_, _ = m.ExecuteCommand("/fast")
	if !m.fastMode {
		t.Fatal("expected fast mode enabled for chatgpt provider alias")
	}
}

func TestFastCommandClearsProviderDefaultWithoutMetadata(t *testing.T) {
	m := newTestChatModel(true)
	m.config = &config.Config{Providers: map[string]config.ProviderConfig{
		"chatgpt": {Type: config.ProviderTypeChatGPT, ServiceTier: "fast"},
	}}
	m.providerKey = "chatgpt"
	m.modelName = "gpt-unknown"
	m.fastProviderDefault = true
	m.fastMode = true

	_, _ = m.ExecuteCommand("/fast")
	if m.fastMode {
		t.Fatal("expected fast mode disabled")
	}
	serviceTier, set := m.currentServiceTier()
	if !set || serviceTier != "" {
		t.Fatalf("currentServiceTier() = (%q, %v), want explicit clear", serviceTier, set)
	}
}

func TestFastCommandTogglesOpenAIWithoutMetadata(t *testing.T) {
	m := newTestChatModel(true)
	m.config = &config.Config{Providers: map[string]config.ProviderConfig{
		"openai": {Type: config.ProviderTypeOpenAI},
	}}
	m.providerKey = "openai"
	m.modelName = "gpt-5.4"

	_, _ = m.ExecuteCommand("/fast")
	if !m.fastMode {
		t.Fatal("expected fast mode enabled for OpenAI")
	}
	serviceTier, set := m.currentServiceTier()
	if !set || serviceTier != llm.ServiceTierFast {
		t.Fatalf("currentServiceTier() = (%q, %v), want fast override", serviceTier, set)
	}
}

func TestUpdateCompletions_WorktreeTargetCommandsUseManagedNames(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForChatWorktreeTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "alpha-feature"})
	if err != nil {
		t.Fatalf("Create worktree: %v", err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), wt.Dir, worktree.RemoveOptions{Force: true}) })

	tests := []struct {
		input string
		want  string
	}{
		{input: "/worktree rm ", want: "worktree rm alpha-feature"},
		{input: "/worktree rm --force ", want: "worktree rm --force alpha-feature"},
		{input: "/wt switch alp", want: "wt switch alpha-feature"},
		{input: "/worktree diff ", want: "worktree diff alpha-feature"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			m := newTestChatModel(false)
			m.sess = &session.Session{ID: "sess-worktree-complete", CWD: repo}
			m.completions.Show()
			m.setTextareaValue(tt.input)
			m.updateCompletions()

			got := completionNames(m.completions.filtered)
			if !containsString(got, tt.want) {
				t.Fatalf("completions = %v, want %q", got, tt.want)
			}
		})
	}
}

func runWorktreeOperationTestCmd(t *testing.T, cmd tea.Cmd) worktreeOperationDoneMsg {
	t.Helper()
	raw := cmd()
	if msg, ok := raw.(worktreeOperationDoneMsg); ok {
		return msg
	}
	if batch, ok := raw.(tea.BatchMsg); ok {
		for _, child := range batch {
			if child == nil {
				continue
			}
			childRaw := child()
			if msg, ok := childRaw.(worktreeOperationDoneMsg); ok {
				return msg
			}
		}
	}
	t.Fatalf("promote command returned %T", raw)
	return worktreeOperationDoneMsg{}
}

func TestCmdWorktreePromoteBranchUsesBoundWorktreeName(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForChatWorktreeTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "promote-selected"})
	if err != nil {
		t.Fatalf("Create worktree: %v", err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), wt.Dir, worktree.RemoveOptions{Force: true}) })

	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "sess-promote-selected", WorktreeDir: wt.Dir, CWD: wt.Dir}
	m.setTextareaValue("/worktree promote --branch")

	result, cmd := m.ExecuteCommand("/worktree promote --branch")
	m = result.(*Model)
	if cmd == nil {
		t.Fatalf("expected promote command, footer=%q", m.footerMessage)
	}
	msg := runWorktreeOperationTestCmd(t, cmd)
	if msg.err != nil {
		t.Fatalf("promote command error: %v", msg.err)
	}
	if msg.branch != "promote-selected" || msg.promote.Branch != "promote-selected" {
		t.Fatalf("promote branch = msg:%q result:%q, want promote-selected", msg.branch, msg.promote.Branch)
	}
}

func TestCmdWorktreePromoteRejectsArgumentsOtherThanBranchMode(t *testing.T) {
	for _, command := range []string{
		"/worktree promote another-worktree",
		"/worktree promote --branch feature",
		"/worktree promote --branch=feature",
	} {
		t.Run(command, func(t *testing.T) {
			m := newTestChatModel(false)
			m.sess = &session.Session{ID: "sess-promote-invalid", CWD: t.TempDir()}

			result, _ := m.ExecuteCommand(command)
			m = result.(*Model)
			if m.worktreeOperation != "" {
				t.Fatalf("invalid promote command started operation %q", m.worktreeOperation)
			}
			if !strings.Contains(m.footerMessage, "Usage: /worktree promote [--branch]") {
				t.Fatalf("footer = %q, want promote usage", m.footerMessage)
			}
		})
	}
}

func TestCmdWorktreeMergeIsUnknown(t *testing.T) {
	m := newTestChatModel(false)
	result, _ := m.ExecuteCommand("/worktree merge")
	m = result.(*Model)
	if m.worktreeOperation != "" || !strings.Contains(m.footerMessage, "Unknown /worktree subcommand: merge") {
		t.Fatalf("removed merge command state = operation:%q footer:%q", m.worktreeOperation, m.footerMessage)
	}
}

func TestCmdWorktreeNewClearsComposerImmediately(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForChatWorktreeTest(t)
	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "sess-worktree-new-clear", CWD: repo}
	m.setTextareaValue("/worktree new feature-clean")

	result, cmd := m.ExecuteCommand("/worktree new feature-clean")
	m = result.(*Model)

	if got := m.textarea.Value(); got != "" {
		t.Fatalf("textarea = %q, want cleared", got)
	}
	if cmd == nil {
		t.Fatal("expected async worktree create command")
	}
	if got := m.worktreeOperation; got != "new" {
		t.Fatalf("worktreeOperation = %q, want new", got)
	}
}

func TestCmdWorktreeClearsComposerForImmediateSubcommands(t *testing.T) {
	m := newTestChatModel(false)
	m.setTextareaValue("/worktree pwd")

	result, _ := m.ExecuteCommand("/worktree pwd")
	m = result.(*Model)

	if got := m.textarea.Value(); got != "" {
		t.Fatalf("textarea = %q, want cleared", got)
	}
}

func TestCmdWorktreeKeepsComposerOnCommandError(t *testing.T) {
	m := newTestChatModel(false)
	m.setTextareaValue("/worktree switch")

	result, _ := m.ExecuteCommand("/worktree switch")
	m = result.(*Model)

	if got := m.textarea.Value(); got != "/worktree switch" {
		t.Fatalf("textarea = %q, want failed command preserved", got)
	}
}

func TestUpdateCompletions_WorktreeOptionCommands(t *testing.T) {
	m := newTestChatModel(false)
	m.completions.Show()
	m.setTextareaValue("/worktree new --b")
	m.updateCompletions()
	got := completionNames(m.completions.filtered)
	if !containsString(got, "worktree new --base") || !containsString(got, "worktree new --branch") {
		t.Fatalf("new option completions = %v, want --base and --branch", got)
	}

	m.completions.Show()
	m.setTextareaValue("/worktree promote --b")
	m.updateCompletions()
	got = completionNames(m.completions.filtered)
	if !containsString(got, "worktree promote --branch") {
		t.Fatalf("promote option completions = %v, want --branch", got)
	}
}

func TestUpdateCompletions_WorktreePromotePinsCurrentWorktree(t *testing.T) {
	repo := newGitRepoForChatWorktreeTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "current-feature"})
	if err != nil {
		t.Fatalf("Create worktree: %v", err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), wt.Dir, worktree.RemoveOptions{Force: true}) })

	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "sess-promote-complete", CWD: wt.Dir, WorktreeDir: wt.Dir}
	m.completions.Show()
	m.setTextareaValue("/worktree promote ")
	m.updateCompletions()

	if len(m.completions.filtered) < 2 {
		t.Fatalf("promote completions = %#v, want current action and --branch", m.completions.filtered)
	}
	if got := m.completions.filtered[0]; got.Name != "worktree promote" || !strings.Contains(got.Description, "current-feature") {
		t.Fatalf("first promote completion = %#v, want current worktree action", got)
	}
	if !containsString(completionNames(m.completions.filtered), "worktree promote --branch") {
		t.Fatalf("promote completions = %#v, want --branch", m.completions.filtered)
	}
}

func TestResolveWorktreeTargetRejectsUnknownManagedName(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForChatWorktreeTest(t)
	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "sess-worktree-target", CWD: repo}
	if _, err := m.resolveWorktreeTarget("does-not-exist"); err == nil {
		t.Fatal("resolveWorktreeTarget accepted unknown managed worktree name")
	}
	if got, err := m.resolveWorktreeTarget("./relative-dir"); err != nil || got != "./relative-dir" {
		t.Fatalf("relative path target = %q, %v; want passthrough", got, err)
	}
}

func newGitRepoForChatWorktreeTest(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("MkdirAll repo: %v", err)
	}
	runGitForChatWorktreeTest(t, repo, "init", "-q")
	runGitForChatWorktreeTest(t, repo, "config", "user.name", "Test User")
	runGitForChatWorktreeTest(t, repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	runGitForChatWorktreeTest(t, repo, "add", "README.md")
	runGitForChatWorktreeTest(t, repo, "commit", "-q", "-m", "init")
	return repo
}

func runGitForChatWorktreeTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test User",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test User",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("git %v failed: %v\n%s", args, err, strings.TrimSpace(string(out)))
	}
}

func TestAllCommandsIncludesShell(t *testing.T) {
	commands := AllCommands()
	for _, cmd := range commands {
		if cmd.Name == "shell" {
			if cmd.Usage != "/shell [--no-rc]" {
				t.Fatalf("shell usage = %q, want /shell [--no-rc]", cmd.Usage)
			}
			if !slices.Contains(cmd.Aliases, "sh") {
				t.Fatalf("shell aliases = %v, want sh", cmd.Aliases)
			}
			return
		}
	}
	t.Fatal("AllCommands() missing /shell")
}

func TestInteractiveShellCommandUsesBoundWorktreeDir(t *testing.T) {
	dir := t.TempDir()
	otherDir := t.TempDir()
	t.Setenv("SHELL", "custom-shell-for-test")

	m := newTestChatModel(false)
	m.sess = &session.Session{WorktreeDir: dir, CWD: otherDir}

	cmd, gotDir, err := m.interactiveShellCommand(false)
	if err != nil {
		t.Fatalf("interactiveShellCommand: %v", err)
	}
	if gotDir != dir || cmd.Dir != dir {
		t.Fatalf("shell dir = %q cmd.Dir = %q, want %q", gotDir, cmd.Dir, dir)
	}
	if cmd.Path != "custom-shell-for-test" {
		t.Fatalf("shell path = %q, want custom shell", cmd.Path)
	}
	if cmd.Stdout != os.Stdout || cmd.Stderr != os.Stderr {
		t.Fatalf("shell output = (%T, %T), want direct terminal stdout/stderr", cmd.Stdout, cmd.Stderr)
	}
	if cmd.Stdin != nil {
		t.Fatalf("shell stdin = %T, want nil for tea.ExecProcess TTY attachment", cmd.Stdin)
	}
	env := strings.Join(cmd.Env, "\n")
	if !strings.Contains(env, "PWD="+dir) || !strings.Contains(env, "TERM_LLM_BASE_DIR="+dir) || !strings.Contains(env, "TERM_LLM_WORKTREE_DIR="+dir) {
		t.Fatalf("shell env missing bound dir entries:\n%s", env)
	}
}

func TestViewReleasesTerminalModesWhileShellRuns(t *testing.T) {
	for _, altScreen := range []bool{false, true} {
		t.Run(fmt.Sprintf("altScreen=%t", altScreen), func(t *testing.T) {
			m := newTestChatModel(altScreen)
			m.setShellTerminalHandoff(true)

			view := m.View()
			if view.Content != "" {
				t.Fatalf("shell handoff view content = %q, want empty", view.Content)
			}
			if view.AltScreen {
				t.Fatal("shell handoff view kept alternate screen enabled")
			}
			if view.MouseMode != tea.MouseModeNone {
				t.Fatalf("shell handoff mouse mode = %v, want none", view.MouseMode)
			}
			if view.Cursor != nil {
				t.Fatal("shell handoff view kept cursor configuration")
			}

			updated, _ := m.Update(shellExitedMsg{})
			restored := updated.(*Model)
			if restored.externalProcessActive {
				t.Fatal("shell exit did not restore normal rendering")
			}
			if restored.pausedForExternalUI {
				t.Fatal("shell exit left external UI paused")
			}
			restoredView := restored.View()
			if restoredView.AltScreen != altScreen {
				t.Fatalf("restored view AltScreen = %t, want %t", restoredView.AltScreen, altScreen)
			}
			if restored.mouseMode && restoredView.MouseMode != tea.MouseModeCellMotion {
				t.Fatalf("restored view mouse mode = %v, want cell motion", restoredView.MouseMode)
			}
		})
	}
}

func TestShellExitErrorRestoresRendering(t *testing.T) {
	m := newTestChatModel(true)
	m.setShellTerminalHandoff(true)

	updated, _ := m.Update(shellExitedMsg{err: errors.New("restore failed")})
	restored := updated.(*Model)
	if restored.externalProcessActive || restored.pausedForExternalUI {
		t.Fatal("shell error left external process rendering state active")
	}
	restored.postFrameImageMu.Lock()
	imageSuppressed := restored.postFrameImageSuppressed
	restored.postFrameImageMu.Unlock()
	if imageSuppressed {
		t.Fatal("shell error left post-frame images suppressed")
	}
	if !restored.View().AltScreen {
		t.Fatal("shell error did not restore alternate-screen rendering")
	}
}

func TestInteractiveShellEnvReplacesPWD(t *testing.T) {
	env := interactiveShellEnv([]string{"PWD=/old", "TERM_LLM_BASE_DIR=/old", "KEEP=1"}, "/new", "", false)
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "PWD=/old") || strings.Contains(joined, "TERM_LLM_BASE_DIR=/old") {
		t.Fatalf("old directory env leaked:\n%s", joined)
	}
	if !strings.Contains(joined, "KEEP=1") || !strings.Contains(joined, "PWD=/new") || !strings.Contains(joined, "TERM_LLM_BASE_DIR=/new") {
		t.Fatalf("replacement env missing entries:\n%s", joined)
	}
	if strings.Contains(joined, "TERM_LLM_WORKTREE_DIR=") {
		t.Fatalf("unexpected worktree env for root shell:\n%s", joined)
	}
}

func TestInteractiveShellCommandNoRCUsesZshFastFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("ZDOTDIR", "/custom-zdotdir")

	m := newTestChatModel(false)
	m.sess = &session.Session{CWD: dir}

	cmd, gotDir, err := m.interactiveShellCommand(true)
	if err != nil {
		t.Fatalf("interactiveShellCommand(no-rc): %v", err)
	}
	if gotDir != dir || cmd.Dir != dir {
		t.Fatalf("shell dir = %q cmd.Dir = %q, want %q", gotDir, cmd.Dir, dir)
	}
	if !slices.Equal(cmd.Args, []string{"/bin/zsh", "-f"}) {
		t.Fatalf("zsh no-rc args = %v, want [/bin/zsh -f]", cmd.Args)
	}
	if env := strings.Join(cmd.Env, "\n"); strings.Contains(env, "ZDOTDIR=") {
		t.Fatalf("ZDOTDIR should be removed with --no-rc:\n%s", env)
	}
}

func TestInteractiveShellArgsNoRCCommonShells(t *testing.T) {
	tests := []struct {
		shell string
		want  []string
	}{
		{shell: "/bin/zsh", want: []string{"-f"}},
		{shell: "/opt/homebrew/bin/bash", want: []string{"--noprofile", "--norc"}},
		{shell: "/usr/local/bin/fish", want: []string{"--no-config"}},
		{shell: "/bin/tcsh", want: []string{"-f"}},
		{shell: "/usr/local/bin/nu", want: []string{"--no-config-file"}},
		{shell: "/bin/sh", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.shell, func(t *testing.T) {
			got, err := interactiveShellArgs(tt.shell, true)
			if err != nil {
				t.Fatalf("interactiveShellArgs(%q, true): %v", tt.shell, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("interactiveShellArgs(%q, true) = %v, want %v", tt.shell, got, tt.want)
			}
		})
	}
}

func TestInteractiveShellEnvNoRCRemovesStartupEnv(t *testing.T) {
	env := interactiveShellEnv([]string{"ENV=/tmp/shrc", "BASH_ENV=/tmp/bashrc", "ZDOTDIR=/tmp/zsh", "KEEP=1"}, "/new", "", true)
	joined := strings.Join(env, "\n")
	for _, unwanted := range []string{"ENV=", "BASH_ENV=", "ZDOTDIR="} {
		if strings.Contains(joined, unwanted) {
			t.Fatalf("%s leaked with --no-rc:\n%s", unwanted, joined)
		}
	}
	if !strings.Contains(joined, "KEEP=1") {
		t.Fatalf("expected unrelated env to be kept:\n%s", joined)
	}
}

func TestUpdateCompletions_ShellOptionCommands(t *testing.T) {
	m := newTestChatModel(false)
	m.completions.Show()
	m.setTextareaValue("/shell --")
	m.updateCompletions()
	got := completionNames(m.completions.filtered)
	if !containsString(got, "shell --no-rc") {
		t.Fatalf("shell option completions = %v, want --no-rc", got)
	}

	m.completions.Show()
	m.setTextareaValue("/sh ")
	m.updateCompletions()
	got = completionNames(m.completions.filtered)
	if !containsString(got, "sh --no-rc") {
		t.Fatalf("sh option completions = %v, want --no-rc", got)
	}
}

func TestBindWorktreeDirRejectsNonGitDirectory(t *testing.T) {
	m := newTestChatModel(false)
	if err := m.bindWorktreeDir(t.TempDir()); err == nil {
		t.Fatal("bindWorktreeDir accepted a non-git directory")
	}
}

func TestCmdWorktreePromoteBlockedWhileStreaming(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.sess = &session.Session{WorktreeDir: t.TempDir()}
	model, _ := m.cmdWorktreePromote([]string{"--branch"})
	got := model.(*Model)
	if !strings.Contains(got.footerMessage, "streaming") {
		t.Fatalf("footerMessage = %q, want streaming warning", got.footerMessage)
	}
}

func TestWorktreeMergeConflictMessageGuidesRecovery(t *testing.T) {
	msg := formatWorktreeMergeConflictMessage(worktree.MergeResult{
		WorktreeName:   "goal",
		WorktreeDir:    "/tmp/wt/goal",
		RootDir:        "/repo/root",
		Base:           "1111111111111111111111111111111111111111",
		RootHead:       "2222222222222222222222222222222222222222",
		WorktreeHead:   "3333333333333333333333333333333333333333",
		SnapshotCommit: "4444444444444444444444444444444444444444",
		Conflicts:      []string{"file.txt"},
		ChangedFiles:   []string{"M\tfile.txt"},
		ConflictReset:  true,
	})
	for _, want := range []string{"root checkout was reset cleanly", "/tmp/wt/goal", "/repo/root", "file.txt", "Yes/No prompt", "Select Yes", "/worktree promote --branch", "LLM-assisted recovery prompt", "git cherry-pick -n"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("merge conflict message missing %q:\n%s", want, msg)
		}
	}
}

func TestWorktreeMergeConflictMessageWarnsWhenCleanupFailed(t *testing.T) {
	msg := formatWorktreeMergeConflictMessage(worktree.MergeResult{
		WorktreeName:         "goal",
		WorktreeDir:          "/tmp/wt/goal",
		RootDir:              "/repo/root",
		Conflicts:            []string{"file.txt"},
		ConflictCleanupError: "git reset --merge: failed",
		RootStatus:           "UU file.txt",
	})
	for _, want := range []string{"may still need cleanup", "Cleanup error", "git status", "UU file.txt"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("merge conflict cleanup-failed message missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "was reset cleanly") {
		t.Fatalf("cleanup-failed message should not claim clean reset:\n%s", msg)
	}
}

func TestWorktreeRichOutputUsesDialogsWithoutPrintCommands(t *testing.T) {
	tests := []struct {
		name string
		msg  worktreeOperationDoneMsg
		want string
	}{
		{name: "diff", msg: worktreeOperationDoneMsg{op: "diff", diff: "diff --git a/file b/file\n+changed"}, want: "diff --git"},
		{name: "promote current branch", msg: worktreeOperationDoneMsg{op: "merge", merge: worktree.MergeResult{WorktreeName: "goal", WorktreeDir: "/tmp/goal", RootDir: "/repo"}}, want: "Promoted worktree"},
		{name: "promote dirty root", msg: worktreeOperationDoneMsg{op: "promote", promote: worktree.PromoteResult{WorktreeName: "goal", WorktreeDir: "/tmp/goal", RootDir: "/repo", Branch: "goal", RootStatus: " M file"}, err: worktree.ErrRootDirty}, want: "root checkout has uncommitted changes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestChatModel(false)
			model, cmd := m.handleWorktreeOperationDone(tt.msg)
			got := model.(*Model)
			if cmd != nil {
				t.Fatal("rich worktree output returned a command; expected managed dialog only")
			}
			if !got.dialog.IsOpen() || got.dialog.Type() != DialogContent {
				t.Fatalf("dialog open/type = %v/%v, want content", got.dialog.IsOpen(), got.dialog.Type())
			}
			if !strings.Contains(got.dialog.Content(), tt.want) {
				t.Fatalf("dialog content missing %q:\n%s", tt.want, got.dialog.Content())
			}
		})
	}
}

func TestWorktreeUsageUsesContentDialog(t *testing.T) {
	m := newTestChatModel(false)
	model, cmd := m.ExecuteCommand("/worktree")
	got := model.(*Model)
	if cmd != nil {
		t.Fatal("worktree usage returned unmanaged output command")
	}
	if !got.dialog.IsOpen() || got.dialog.Type() != DialogContent || !strings.Contains(got.dialog.Content(), "Usage: /worktree") {
		t.Fatalf("worktree usage dialog = open:%v type:%v content:%q", got.dialog.IsOpen(), got.dialog.Type(), got.dialog.Content())
	}
}

func TestBoundWorktreeDirPrefersActiveSessionCWD(t *testing.T) {
	m := newTestChatModel(false)
	m.sess = &session.Session{CWD: "/worktrees/active", WorktreeDir: "/worktrees/stale"}
	if got := m.boundWorktreeDir(); got != m.sess.CWD {
		t.Fatalf("boundWorktreeDir = %q, want active session CWD %q", got, m.sess.CWD)
	}
}

func TestCmdWorktreePromoteDefaultsToActiveSessionCWD(t *testing.T) {
	repo := newGitRepoForChatWorktreeTest(t)
	wtA, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "merge-active-a"})
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	wtB, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "merge-stale-b"})
	if err != nil {
		t.Fatalf("Create B: %v", err)
	}
	t.Cleanup(func() {
		_ = worktree.Remove(context.Background(), wtA.Dir, worktree.RemoveOptions{Force: true})
		_ = worktree.Remove(context.Background(), wtB.Dir, worktree.RemoveOptions{Force: true})
	})
	if err := os.WriteFile(filepath.Join(wtA.Dir, "active.txt"), []byte("active\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "merge-active", CWD: wtA.Dir, WorktreeDir: wtB.Dir}
	_, cmd := m.ExecuteCommand("/worktree promote")
	msg := runWorktreeOperationTestCmd(t, cmd)
	if msg.err != nil {
		t.Fatalf("merge: %v", msg.err)
	}
	if !sameWorktreePath(msg.dir, wtA.Dir) {
		t.Fatalf("merged worktree = %q, want active session worktree %q", msg.dir, wtA.Dir)
	}
}

func TestCmdWorktreePromoteDefaultAppliesRemovesAndRebinds(t *testing.T) {
	repo := newGitRepoForChatWorktreeTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "merge-cleanup-tui"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt.Dir, "merged.txt"), []byte("merged\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "merge-cleanup", WorktreeDir: wt.Dir, CWD: wt.Dir}
	model, cmd := m.ExecuteCommand("/worktree promote")
	m = model.(*Model)
	msg := runWorktreeOperationTestCmd(t, cmd)
	if msg.err != nil || !msg.cleanup.Removed {
		t.Fatalf("merge message = %+v, want successful cleanup", msg)
	}
	model, _ = m.handleWorktreeOperationDone(msg)
	m = model.(*Model)
	rootInfo, rootErr := os.Stat(repo)
	cwdInfo, cwdErr := os.Stat(m.sess.CWD)
	if m.sess.WorktreeDir != "" || rootErr != nil || cwdErr != nil || !os.SameFile(rootInfo, cwdInfo) {
		t.Fatalf("session cwd/worktree = %q/%q, want root/%q", m.sess.CWD, m.sess.WorktreeDir, repo)
	}
	if _, err := os.Stat(wt.Dir); !os.IsNotExist(err) {
		t.Fatalf("worktree stat = %v, want removed", err)
	}
}

func TestCmdWorktreePromoteConflictOpensAssistedRecovery(t *testing.T) {
	repo := newGitRepoForChatWorktreeTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "promote-conflict"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), wt.Dir, worktree.RemoveOptions{Force: true}) })

	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("root change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForChatWorktreeTest(t, repo, "add", "file.txt")
	runGitForChatWorktreeTest(t, repo, "commit", "-m", "root change")
	if err := os.WriteFile(filepath.Join(wt.Dir, "file.txt"), []byte("worktree change\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "promote-conflict", WorktreeDir: wt.Dir, CWD: wt.Dir}
	model, cmd := m.ExecuteCommand("/worktree promote")
	m = model.(*Model)
	msg := runWorktreeOperationTestCmd(t, cmd)
	if !errors.Is(msg.err, worktree.ErrConflict) {
		t.Fatalf("promote error = %v, want conflict", msg.err)
	}
	model, _ = m.handleWorktreeOperationDone(msg)
	m = model.(*Model)
	if m.pendingWorktreeRecovery == nil || !m.dialog.IsOpen() || m.dialog.Type() != DialogWorktreeRecovery {
		t.Fatal("promote conflict did not open assisted recovery")
	}
	if _, err := os.Stat(wt.Dir); err != nil {
		t.Fatalf("source worktree was not preserved: %v", err)
	}
}

func TestWorktreePromoteInUsePromptNoKeepsWorktree(t *testing.T) {
	m := newTestChatModel(false)
	msg := worktreeOperationDoneMsg{
		op:      "merge",
		bound:   true,
		merge:   worktree.MergeResult{WorktreeName: "shared", WorktreeDir: "/tmp/shared", RootDir: "/repo"},
		cleanup: worktree.CleanupResult{InUse: []worktree.InUseSession{{ID: "other"}}},
	}
	model, _ := m.handleWorktreeOperationDone(msg)
	m = model.(*Model)
	if m.pendingWorktreeRecovery == nil || !m.dialog.IsOpen() || m.dialog.Type() != DialogWorktreeRecovery {
		t.Fatal("expected remove-in-use confirmation prompt")
	}
	view := ui.StripANSI(m.dialog.View())
	if !strings.Contains(view, "promotion succeeded") || !strings.Contains(view, "remove it anyway") {
		t.Fatalf("prompt missing in-use warning:\n%s", view)
	}
	model, _ = m.resolveWorktreeRecoveryPrompt(false)
	m = model.(*Model)
	if m.pendingWorktreeRecovery != nil || m.worktreeOperation != "" {
		t.Fatalf("decline state: pending=%v op=%q", m.pendingWorktreeRecovery, m.worktreeOperation)
	}
}

func TestPendingWorktreePromoteInUseYesRemoves(t *testing.T) {
	repo := newGitRepoForChatWorktreeTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "merge-in-use-yes"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "current", WorktreeDir: wt.Dir, CWD: wt.Dir}
	pending := pendingWorktreeRecovery{kind: "remove-in-use", bound: true, merge: worktree.MergeResult{WorktreeName: wt.Name, WorktreeDir: wt.Dir, RootDir: repo}}
	m.pendingWorktreeRecovery = &pending
	m.openWorktreeRecoveryPrompt(pending)

	model, cmd := m.resolveWorktreeRecoveryPrompt(true)
	m = model.(*Model)
	if cmd == nil || m.worktreeOperation != "remove" {
		t.Fatalf("yes did not start removal: cmd=%v op=%q", cmd != nil, m.worktreeOperation)
	}
	msg := runWorktreeOperationTestCmd(t, cmd)
	if msg.err != nil {
		t.Fatalf("remove: %v", msg.err)
	}
	model, _ = m.handleWorktreeOperationDone(msg)
	m = model.(*Model)
	rootInfo, rootErr := os.Stat(repo)
	cwdInfo, cwdErr := os.Stat(m.sess.CWD)
	if m.sess.WorktreeDir != "" || rootErr != nil || cwdErr != nil || !os.SameFile(rootInfo, cwdInfo) {
		t.Fatalf("session cwd/worktree = %q/%q, want root", m.sess.CWD, m.sess.WorktreeDir)
	}
	if _, err := os.Stat(wt.Dir); !os.IsNotExist(err) {
		t.Fatalf("worktree stat = %v, want removed", err)
	}
}

func TestCmdWorktreePromoteValidationRunsAsynchronously(t *testing.T) {
	m := newTestChatModel(false)
	invalid := filepath.Join(t.TempDir(), "missing")
	m.sess = &session.Session{ID: "async-merge", CWD: invalid, WorktreeDir: invalid}
	m.setTextareaValue("/worktree promote")

	model, cmd := m.ExecuteCommand("/worktree promote")
	got := model.(*Model)
	if cmd == nil || got.worktreeOperation != "merge" {
		t.Fatalf("merge did not start asynchronously: cmd=%v operation=%q footer=%q", cmd != nil, got.worktreeOperation, got.footerMessage)
	}
	if got.textarea.Value() != "" {
		t.Fatalf("textarea = %q, want cleared", got.textarea.Value())
	}
	msg := runWorktreeOperationTestCmd(t, cmd)
	if msg.err == nil {
		t.Fatal("async merge validation unexpectedly succeeded")
	}
	model, _ = got.handleWorktreeOperationDone(msg)
	if model.(*Model).worktreeOperation != "" {
		t.Fatal("async validation failure did not clear operation state")
	}
}

func TestWorktreeOperationDoneStaleMessageDoesNotClearCurrentOperation(t *testing.T) {
	m := newTestChatModel(false)
	m.worktreeOperation = "assist-merge"
	model, cmd := m.handleWorktreeOperationDone(worktreeOperationDoneMsg{op: "diff", diff: "stale"})
	got := model.(*Model)
	if got.worktreeOperation != "assist-merge" {
		t.Fatalf("worktreeOperation = %q, want assist-merge", got.worktreeOperation)
	}
	if cmd != nil {
		t.Fatal("stale worktree result should not schedule commands")
	}
}

func TestAssistedMergeNothingToApplyMessageSaysRootUnchanged(t *testing.T) {
	msg := formatAssistedMergeNothingToApplyMessage(worktree.AssistedMergeResult{
		RootDir:        "/repo/root",
		WorktreeDir:    "/tmp/wt/goal",
		WorktreeName:   "goal",
		SnapshotCommit: "4444444444444444444444444444444444444444",
	})
	for _, want := range []string{"no worktree changes", "root checkout was not changed", "/tmp/wt/goal"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("nothing-to-apply message missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "Prepared assisted") || strings.Contains(msg, "assist-was-not-created") || strings.Contains(msg, "git branch -D") {
		t.Fatalf("nothing-to-apply message mentioned nonexistent branch:\n%s", msg)
	}
}

func TestWorktreeRecoveryPromptOpensOnMergeConflict(t *testing.T) {
	m := newTestChatModel(false)
	model, cmd := m.handleWorktreeOperationDone(worktreeOperationDoneMsg{
		op: "merge",
		merge: worktree.MergeResult{
			WorktreeName: "goal",
			WorktreeDir:  "/tmp/wt/goal",
			RootDir:      "/repo/root",
			Conflicts:    []string{"first.txt", "second.txt"},
		},
		err: worktree.ErrConflict,
	})
	if cmd != nil {
		t.Fatal("merge conflict scheduled unmanaged output alongside recovery dialog")
	}
	got := model.(*Model)
	if got.pendingWorktreeRecovery == nil {
		t.Fatal("expected pending recovery after merge conflict")
	}
	if !got.dialog.IsOpen() || got.dialog.Type() != DialogWorktreeRecovery {
		t.Fatalf("expected worktree recovery dialog, got open=%v type=%v", got.dialog.IsOpen(), got.dialog.Type())
	}
	view := ui.StripANSI(got.dialog.View())
	for _, want := range []string{"does not promote cleanly", "/tmp/wt/goal", "/repo/root", "first.txt", "Yes", "No"} {
		if !strings.Contains(view, want) {
			t.Fatalf("recovery dialog missing %q:\n%s", want, view)
		}
	}
}

func TestPendingWorktreeRecoveryYesStartsAssistedMergeFromRoot(t *testing.T) {
	m := newTestChatModel(false)
	root := t.TempDir()
	worktreeDir := filepath.Join(t.TempDir(), "goal")
	m.sess = &session.Session{ID: "assisted-recovery", CWD: worktreeDir, WorktreeDir: worktreeDir}
	pending := pendingWorktreeRecovery{kind: "conflict", merge: worktree.MergeResult{WorktreeName: "goal", WorktreeDir: worktreeDir, RootDir: root}}
	m.pendingWorktreeRecovery = &pending
	m.openWorktreeRecoveryPrompt(pending)

	model, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := model.(*Model)
	if got.pendingWorktreeRecovery != nil {
		t.Fatal("pending recovery should be cleared after yes selection")
	}
	if got.dialog.IsOpen() {
		t.Fatal("recovery dialog should close after selection")
	}
	if got.worktreeOperation != "assist-merge" {
		t.Fatalf("worktreeOperation after yes = %q, want assist-merge", got.worktreeOperation)
	}
	if got.sess.CWD != root || got.sess.WorktreeDir != "" {
		t.Fatalf("session binding = %q/%q, want root %q before assisted merge", got.sess.CWD, got.sess.WorktreeDir, root)
	}
	if got.streaming {
		t.Fatal("affirmative conflict recovery should prepare root before starting LLM stream")
	}
	if cmd == nil {
		t.Fatal("expected assisted merge preparation command")
	}
}

func TestAssistedMergeStartsLLMWithoutOpeningContentDialog(t *testing.T) {
	m := newTestChatModel(false)
	root := t.TempDir()
	m.sess = &session.Session{ID: "assisted-ready", CWD: root}
	m.worktreeOperation = "assist-merge"

	model, cmd := m.handleWorktreeOperationDone(worktreeOperationDoneMsg{
		op: "assist-merge",
		assist: worktree.AssistedMergeResult{
			RootDir:         root,
			WorktreeDir:     filepath.Join(t.TempDir(), "goal"),
			WorktreeName:    "goal",
			ChangedFiles:    []string{"file.txt"},
			Conflicts:       []string{"file.txt"},
			NeedsResolution: true,
		},
	})
	got := model.(*Model)
	if got.dialog.IsOpen() {
		t.Fatalf("assisted recovery opened distracting dialog type %v", got.dialog.Type())
	}
	if !got.streaming || cmd == nil {
		t.Fatalf("assisted recovery did not start LLM: streaming=%v cmd=%v", got.streaming, cmd != nil)
	}
	if got.sess.CWD != root || got.sess.WorktreeDir != "" {
		t.Fatalf("session binding = %q/%q, want root %q", got.sess.CWD, got.sess.WorktreeDir, root)
	}
}
func TestPendingWorktreeRecoveryBindFailureKeepsPromptRetryable(t *testing.T) {
	m := newTestChatModel(false)
	root := t.TempDir()
	worktreeDir := filepath.Join(t.TempDir(), "goal")
	m.sess = &session.Session{ID: "assisted-bind-failure", CWD: worktreeDir, WorktreeDir: worktreeDir}
	m.runtimeSystemContextResolver = func(_ *agents.Agent, _, _, dir string) (RuntimeSystemContext, error) {
		return RuntimeSystemContext{}, fmt.Errorf("cannot load root context for %s", dir)
	}
	pending := pendingWorktreeRecovery{kind: "conflict", merge: worktree.MergeResult{WorktreeName: "goal", WorktreeDir: worktreeDir, RootDir: root}}
	m.pendingWorktreeRecovery = &pending
	m.openWorktreeRecoveryPrompt(pending)

	model, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := model.(*Model)
	if got.worktreeOperation != "" {
		t.Fatalf("bind failure started recovery operation %q", got.worktreeOperation)
	}
	if got.pendingWorktreeRecovery == nil {
		t.Fatal("bind failure consumed pending recovery")
	}
	if !got.dialog.IsOpen() || got.dialog.Type() != DialogWorktreeRecovery {
		t.Fatalf("bind failure closed recovery prompt: open=%v type=%v", got.dialog.IsOpen(), got.dialog.Type())
	}
	if got.sess.CWD != worktreeDir || got.sess.WorktreeDir != worktreeDir {
		t.Fatalf("bind failure changed session binding = %q/%q", got.sess.CWD, got.sess.WorktreeDir)
	}
	if !strings.Contains(got.footerMessage, "cannot load root context") {
		t.Fatalf("footerMessage = %q, want binding error", got.footerMessage)
	}
}

func TestAssistedRecoverySnapshotsChangesMadeAfterConflictPrompt(t *testing.T) {
	repo := newGitRepoForChatWorktreeTest(t)
	wt, err := worktree.Create(context.Background(), repo, worktree.CreateOptions{Name: "assist-fresh-snapshot"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		runGitForChatWorktreeTest(t, repo, "reset", "--merge")
		_ = worktree.Remove(context.Background(), wt.Dir, worktree.RemoveOptions{Force: true})
	})

	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("root change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForChatWorktreeTest(t, repo, "add", "file.txt")
	runGitForChatWorktreeTest(t, repo, "commit", "-m", "root change")
	if err := os.WriteFile(filepath.Join(wt.Dir, "file.txt"), []byte("worktree change\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "assist-fresh-snapshot", CWD: wt.Dir, WorktreeDir: wt.Dir}
	model, promoteCmd := m.ExecuteCommand("/worktree promote")
	m = model.(*Model)
	promoteMsg := runWorktreeOperationTestCmd(t, promoteCmd)
	if !errors.Is(promoteMsg.err, worktree.ErrConflict) {
		t.Fatalf("promote error = %v, want conflict", promoteMsg.err)
	}
	model, _ = m.handleWorktreeOperationDone(promoteMsg)
	m = model.(*Model)

	if err := os.WriteFile(filepath.Join(wt.Dir, "after-prompt.txt"), []byte("newer change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	model, assistCmd := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = model.(*Model)
	if assistCmd == nil {
		t.Fatal("expected assisted merge command")
	}
	assistMsg := runWorktreeOperationTestCmd(t, assistCmd)
	if assistMsg.err != nil {
		t.Fatalf("assisted merge: %v", assistMsg.err)
	}
	if !assistMsg.assist.NeedsResolution {
		t.Fatalf("assisted result = %+v, want conflict", assistMsg.assist)
	}
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = repo
	statusOut, err := statusCmd.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	status := string(statusOut)
	if !strings.Contains(status, "A  after-prompt.txt") {
		t.Fatalf("root status = %q, want newer worktree change included", status)
	}
}

func TestAssistedMergeErrRootDirtyClearsOperationAndShowsDetails(t *testing.T) {
	m := newTestChatModel(false)
	m.worktreeOperation = "assist-merge"
	model, cmd := m.handleWorktreeOperationDone(worktreeOperationDoneMsg{
		op: "assist-merge",
		assist: worktree.AssistedMergeResult{
			RootDir:      "/repo/root",
			WorktreeDir:  "/tmp/wt/goal",
			WorktreeName: "goal",
			RootStatus:   " M root.txt",
		},
		err: worktree.ErrRootDirty,
	})
	got := model.(*Model)
	if cmd != nil || got.worktreeOperation != "" {
		t.Fatalf("dirty-root result left operation active: cmd=%v op=%q", cmd != nil, got.worktreeOperation)
	}
	if !got.dialog.IsOpen() || got.dialog.Type() != DialogContent {
		t.Fatalf("dirty-root result dialog = open:%v type:%v", got.dialog.IsOpen(), got.dialog.Type())
	}
	if !strings.Contains(got.dialog.Content(), "root checkout became dirty") || !strings.Contains(got.dialog.Content(), "root.txt") {
		t.Fatalf("dirty-root content missing details:\n%s", got.dialog.Content())
	}
}

func TestPendingWorktreeRecoveryNoClearsPrompt(t *testing.T) {
	m := newTestChatModel(false)
	pending := pendingWorktreeRecovery{kind: "conflict", merge: worktree.MergeResult{WorktreeName: "goal", WorktreeDir: "/tmp/wt/goal"}}
	m.pendingWorktreeRecovery = &pending
	m.openWorktreeRecoveryPrompt(pending)

	model, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	m = model.(*Model)
	model, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := model.(*Model)
	if got.pendingWorktreeRecovery != nil {
		t.Fatal("pending recovery should be cleared after no selection")
	}
	if got.dialog.IsOpen() {
		t.Fatal("recovery dialog should close after no selection")
	}
	if got.worktreeOperation == "assist-merge" || got.streaming {
		t.Fatalf("negative recovery selection should not start recovery: op=%q streaming=%v", got.worktreeOperation, got.streaming)
	}
}

func TestPendingWorktreeDirtyRootYesSendsLLMPrompt(t *testing.T) {
	m := newTestChatModel(false)
	pending := pendingWorktreeRecovery{kind: "dirty-root", merge: worktree.MergeResult{WorktreeName: "goal", WorktreeDir: "/tmp/wt/goal", RootDir: "", RootStatus: " M file.txt"}}
	m.pendingWorktreeRecovery = &pending
	m.openWorktreeRecoveryPrompt(pending)

	model, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := model.(*Model)
	if got.pendingWorktreeRecovery != nil {
		t.Fatal("pending recovery should be cleared after affirmative selection")
	}
	if got.dialog.IsOpen() {
		t.Fatal("recovery dialog should close after affirmative selection")
	}
	if !got.streaming {
		t.Fatal("dirty-root affirmative recovery should send an LLM prompt")
	}
	if cmd == nil {
		t.Fatal("expected stream command")
	}
	if len(got.messages) == 0 || !strings.Contains(got.messages[len(got.messages)-1].TextContent, "blocked because the root checkout is dirty") {
		t.Fatalf("last message = %#v, want dirty-root recovery prompt", got.messages)
	}
}

func TestPendingWorktreeRecoveryEscCancelsPrompt(t *testing.T) {
	m := newTestChatModel(false)
	pending := pendingWorktreeRecovery{kind: "conflict", merge: worktree.MergeResult{WorktreeName: "goal", WorktreeDir: "/tmp/wt/goal"}}
	m.pendingWorktreeRecovery = &pending
	m.openWorktreeRecoveryPrompt(pending)

	model, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := model.(*Model)
	if got.pendingWorktreeRecovery != nil {
		t.Fatal("pending recovery should be cleared after esc")
	}
	if got.dialog.IsOpen() {
		t.Fatal("recovery dialog should close after esc")
	}
	if got.worktreeOperation == "assist-merge" || got.streaming {
		t.Fatalf("esc should not start recovery: op=%q streaming=%v", got.worktreeOperation, got.streaming)
	}
}

func TestWorktreePromoteDoneRebindsSessionToRoot(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	root := t.TempDir()
	wtDir := filepath.Join(t.TempDir(), "goal")
	m.sess = &session.Session{ID: "s1", CWD: wtDir, WorktreeDir: wtDir}
	m.worktreeOperation = "promote"

	model, _ := m.handleWorktreeOperationDone(worktreeOperationDoneMsg{
		op: "promote",
		promote: worktree.PromoteResult{
			RootDir:                     root,
			WorktreeDir:                 wtDir,
			WorktreeName:                "goal",
			Branch:                      "feature/goal",
			PreviousRootBranch:          "main",
			PreviousRootRef:             "1111111111111111111111111111111111111111",
			WorktreeHead:                "2222222222222222222222222222222222222222",
			Applied:                     true,
			OriginalWorktreeStillExists: true,
			RootStatus:                  "M  file.txt",
		},
	})
	got := model.(*Model)
	if got.sess.WorktreeDir != "" || got.sess.CWD != root {
		t.Fatalf("session worktree/cwd = %q/%q, want empty/%q", got.sess.WorktreeDir, got.sess.CWD, root)
	}
	if store.updated == nil || store.updated.CWD != root || store.updated.WorktreeDir != "" {
		t.Fatalf("store updated session = %#v, want root binding", store.updated)
	}
}

func TestWorktreeOperationBlocksSend(t *testing.T) {
	m := newTestChatModel(false)
	m.worktreeOperation = "merge"
	model, _ := m.sendMessage("hello")
	got := model.(*Model)
	if got.streaming {
		t.Fatal("sendMessage started streaming while worktree operation was active")
	}
	if !strings.Contains(got.footerMessage, "worktree operation") {
		t.Fatalf("footerMessage = %q, want worktree operation warning", got.footerMessage)
	}
}

func TestApplyRuntimeDirectoryRefreshesExistingSystemMessage(t *testing.T) {
	store := &mockStore{messages: map[string][]session.Message{}}
	m := newCmdTestModel(store)
	m.sess = &session.Session{ID: "runtime-dir", CWD: "/old"}
	m.messages = []session.Message{{ID: 42, SessionID: m.sess.ID, Role: llm.RoleSystem, Parts: []llm.Part{{Type: llm.PartText, Text: "old"}}, TextContent: "old", Sequence: 0}}
	store.messages[m.sess.ID] = append([]session.Message(nil), m.messages...)
	m.viewCache.historyValid = true
	m.contextEstimateCachedValid = true
	var resolvedDir string
	m.runtimeSystemContextResolver = func(_ *agents.Agent, _, _, dir string) (RuntimeSystemContext, error) {
		resolvedDir = dir
		return RuntimeSystemContext{SystemPrompt: "cwd=" + dir}, nil
	}

	if err := m.applyRuntimeDirectory("/new", "/new"); err != nil {
		t.Fatalf("applyRuntimeDirectory: %v", err)
	}
	if resolvedDir != "/new" {
		t.Fatalf("resolver dir = %q, want /new", resolvedDir)
	}
	if len(m.messages) != 1 || m.messages[0].ID != 42 || m.messages[0].TextContent != "cwd=/new" {
		t.Fatalf("messages = %#v, want same refreshed system row", m.messages)
	}
	if len(store.messageUpdates) != 1 || store.messageUpdates[0].ID != 42 {
		t.Fatalf("message updates = %#v, want one update for ID 42", store.messageUpdates)
	}
	if m.sess.CWD != "/new" || m.sess.WorktreeDir != "/new" {
		t.Fatalf("session binding = %q/%q", m.sess.CWD, m.sess.WorktreeDir)
	}
	if m.viewCache.historyValid || m.contextEstimateCachedValid {
		t.Fatal("runtime refresh left history/context estimate caches valid")
	}
}

func TestApplyRuntimeDirectoryClearsStaleSystemMessageWhenPromptResolvesEmpty(t *testing.T) {
	store := &mockStore{messages: map[string][]session.Message{}}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "empty-prompt", CWD: "/old"}
	m.messages = []session.Message{{ID: 6, SessionID: m.sess.ID, Role: llm.RoleSystem, Parts: []llm.Part{{Type: llm.PartText, Text: "old cwd"}}, TextContent: "old cwd", Sequence: 0}}
	store.messages[m.sess.ID] = append([]session.Message(nil), m.messages...)
	m.runtimeSystemContextResolver = func(_ *agents.Agent, _, _, _ string) (RuntimeSystemContext, error) {
		return RuntimeSystemContext{}, nil
	}

	if err := m.applyRuntimeDirectory("/new", "/new"); err != nil {
		t.Fatalf("applyRuntimeDirectory: %v", err)
	}
	if len(m.messages) != 1 || m.messages[0].ID != 6 || m.messages[0].TextContent != "" {
		t.Fatalf("messages = %#v, want existing system row cleared", m.messages)
	}
	if len(store.messageUpdates) != 1 || store.messageUpdates[0].TextContent != "" {
		t.Fatalf("message updates = %#v, want persisted prompt cleared", store.messageUpdates)
	}
	if m.config.Chat.Instructions != "" {
		t.Fatalf("config instructions = %q, want empty", m.config.Chat.Instructions)
	}
}

func TestRuntimeRollbackBaseUsesEffectivePriorDirectory(t *testing.T) {
	if got := runtimeRollbackBase("/configured", "/session"); got != "/configured" {
		t.Fatalf("configured rollback base = %q", got)
	}
	if got := runtimeRollbackBase("", "/session"); got != "/session" {
		t.Fatalf("session rollback base = %q", got)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got := runtimeRollbackBase("", ""); got != cwd {
		t.Fatalf("process rollback base = %q, want %q", got, cwd)
	}
}

func TestApplyRuntimeDirectoryPreservesExplicitSystemOverride(t *testing.T) {
	store := &mockStore{messages: map[string][]session.Message{}}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "override", CWD: "/old"}
	m.messages = []session.Message{{ID: 7, SessionID: m.sess.ID, Role: llm.RoleSystem, Parts: []llm.Part{{Type: llm.PartText, Text: "old default"}}, TextContent: "old default", Sequence: 0}}
	store.messages[m.sess.ID] = append([]session.Message(nil), m.messages...)
	_, _ = m.cmdSystem([]string{"custom"})
	m.runtimeSystemContextResolver = func(_ *agents.Agent, _, _, _ string) (RuntimeSystemContext, error) {
		return RuntimeSystemContext{SystemPrompt: "agent default"}, nil
	}

	if err := m.applyRuntimeDirectory("/new", "/new"); err != nil {
		t.Fatalf("applyRuntimeDirectory: %v", err)
	}
	if got := m.messages[0].TextContent; got != "custom" {
		t.Fatalf("system prompt = %q, want explicit override", got)
	}
	if len(store.messageUpdates) != 1 || store.messageUpdates[0].TextContent != "custom" {
		t.Fatalf("persisted system prompt updates = %#v, want explicit override", store.messageUpdates)
	}
	if m.config.Chat.Instructions != "custom" {
		t.Fatalf("config instructions = %q, want explicit override", m.config.Chat.Instructions)
	}
}

func TestApplyRuntimeDirectoryResolutionFailureLeavesStateUntouched(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "failure", CWD: "/old"}
	m.messages = []session.Message{{ID: 1, Role: llm.RoleSystem, TextContent: "old"}}
	m.runtimeSystemContextResolver = func(_ *agents.Agent, _, _, _ string) (RuntimeSystemContext, error) {
		return RuntimeSystemContext{}, errors.New("boom")
	}
	if err := m.applyRuntimeDirectory("/new", "/new"); err == nil {
		t.Fatal("applyRuntimeDirectory succeeded, want resolver error")
	}
	if m.sess.CWD != "/old" || m.sess.WorktreeDir != "" || m.messages[0].TextContent != "old" {
		t.Fatalf("state changed after resolution failure: session=%#v messages=%#v", m.sess, m.messages)
	}
}

func TestApplyRuntimeDirectoryMessageFailureRollsBackBindingAndPrompt(t *testing.T) {
	store := &mockStore{messages: map[string][]session.Message{}, updateMessageErr: errors.New("write failed")}
	m := newCmdTestModel(store)
	m.sess = &session.Session{ID: "rollback", CWD: "/old"}
	m.messages = []session.Message{{ID: 9, SessionID: m.sess.ID, Role: llm.RoleSystem, Parts: []llm.Part{{Type: llm.PartText, Text: "old"}}, TextContent: "old", Sequence: 0}}
	store.messages[m.sess.ID] = append([]session.Message(nil), m.messages...)
	m.runtimeSystemContextResolver = func(_ *agents.Agent, _, _, _ string) (RuntimeSystemContext, error) {
		return RuntimeSystemContext{SystemPrompt: "new"}, nil
	}

	if err := m.applyRuntimeDirectory("/new", "/new"); err == nil {
		t.Fatal("applyRuntimeDirectory succeeded, want message persistence error")
	}
	if m.sess.CWD != "/old" || m.sess.WorktreeDir != "" || m.messages[0].TextContent != "old" {
		t.Fatalf("rollback state = session %#v messages %#v", m.sess, m.messages)
	}
}

func TestSwitchEffortRefreshesGuardianAndFailsClosed(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.config = &config.Config{}
	m.providerKey = "openai"
	m.modelName = "old"
	m.approvalMgr = tools.NewApprovalManager(tools.NewToolPermissions())
	m.approvalMgr.SetApprovalMode(tools.ModeAuto)
	m.approvalMgr.PolicyReviewFunc = func(context.Context, tools.PolicyReviewRequest) (tools.PolicyDecision, error) {
		return tools.PolicyDecision{Allowed: true}, nil
	}
	var gotProvider, gotModel string
	m.guardianReviewerRefresh = func(provider, model string) error {
		gotProvider, gotModel = provider, model
		return errors.New("unavailable")
	}

	result, _ := m.switchEffortStateOnly(effortSwitchResolution{provider: "openai", targetModel: "new", label: "high", ok: true}, false)
	got := result.(*Model)
	if gotProvider != "openai" || gotModel != "new" {
		t.Fatalf("guardian refresh = %q/%q, want openai/new", gotProvider, gotModel)
	}
	if got.approvalMgr.PolicyReviewFunc != nil || got.approvalMgr.ApprovalMode() != tools.ModePrompt {
		t.Fatal("guardian refresh failure retained stale reviewer or auto mode")
	}
}

func TestCmdShareRejectsUnknownArgument(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "share-test"}
	result, cmd := m.ExecuteCommand("/share banana")
	m = result.(*Model)
	if cmd == nil || !strings.Contains(m.footerMessage, "Usage: /share [new] [public]") {
		t.Fatalf("footer = %q, cmd nil = %v", m.footerMessage, cmd == nil)
	}
}

func TestCmdShareExistingGistOpensChoice(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "share-test", Share: &session.ShareState{GistID: "abc123"}}
	result, cmd := m.ExecuteCommand("/share")
	m = result.(*Model)
	if cmd != nil || m.dialog.Type() != DialogShareChoice || m.pendingShare == nil {
		t.Fatalf("dialog=%v pending=%v cmd nil=%v", m.dialog.Type(), m.pendingShare, cmd == nil)
	}
}

func TestShareCommandRegistered(t *testing.T) {
	for _, command := range AllCommands() {
		if command.Name == "share" {
			if command.Usage != "/share [new] [public]" || len(command.Subcommands) != 2 {
				t.Fatalf("share command = %+v", command)
			}
			return
		}
	}
	t.Fatal("share command not registered")
}

func TestCmdShareNewSkipsExistingChoice(t *testing.T) {
	store := &mockStore{sessions: map[string]*session.Session{}, messages: map[string][]session.Message{}}
	m := newCmdTestModel(store)
	m.sess = &session.Session{ID: "share-new", Share: &session.ShareState{GistID: "abc123"}}
	store.sessions[m.sess.ID] = m.sess
	result, cmd := m.ExecuteCommand("/share new")
	m = result.(*Model)
	if cmd == nil || m.dialog.Type() == DialogShareChoice || !m.shareInFlight {
		t.Fatalf("dialog=%v inFlight=%v cmd nil=%v", m.dialog.Type(), m.shareInFlight, cmd == nil)
	}
}

func TestCmdShareRejectsWhileStreamingOrInFlight(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "share-busy"}
	m.streaming = true
	result, _ := m.ExecuteCommand("/share")
	m = result.(*Model)
	if !strings.Contains(m.footerMessage, "Cannot share while streaming") {
		t.Fatalf("streaming footer = %q", m.footerMessage)
	}
	m.streaming = false
	m.shareInFlight = true
	result, _ = m.ExecuteCommand("/share")
	m = result.(*Model)
	if !strings.Contains(m.footerMessage, "already in progress") {
		t.Fatalf("in-flight footer = %q", m.footerMessage)
	}
}

func TestHandleShareDonePersistsOriginatingSession(t *testing.T) {
	origin := &session.Session{ID: "origin"}
	current := &session.Session{ID: "current"}
	store := &mockStore{sessions: map[string]*session.Session{"origin": origin, "current": current}}
	m := newCmdTestModel(store)
	m.sess = current
	m.shareInFlight = true
	beforeMessages := len(m.messages)

	result, _ := m.handleShareDone(shareDoneMsg{
		store:     store,
		sessionID: origin.ID,
		gist:      &gist.Gist{ID: "abc123", URL: "https://gist.github.com/u/abc123"},
		preview:   session.GistPreviewURL("abc123"),
	})
	m = result.(*Model)
	if m.shareInFlight {
		t.Fatal("share remained in flight")
	}
	if store.updated == nil || store.updated.ID != origin.ID || store.updated.Share == nil {
		t.Fatalf("updated session = %+v", store.updated)
	}
	if current.Share != nil {
		t.Fatalf("current session was poisoned: %+v", current.Share)
	}
	if m.dialog.Type() != DialogContent || !strings.Contains(m.dialog.Content(), "abc123") {
		t.Fatalf("result dialog type=%v content=%q", m.dialog.Type(), m.dialog.Content())
	}
	if len(m.messages) != beforeMessages {
		t.Fatalf("share result added scrollback: before=%d after=%d", beforeMessages, len(m.messages))
	}
}

func TestHandleShareDonePublicUpdateExplainsVisibility(t *testing.T) {
	sess := &session.Session{ID: "origin"}
	store := &mockStore{sessions: map[string]*session.Session{"origin": sess}}
	m := newCmdTestModel(store)
	m.sess = sess
	result, _ := m.handleShareDone(shareDoneMsg{
		store:           store,
		sessionID:       sess.ID,
		gist:            &gist.Gist{ID: "abc123", URL: "https://gist.github.com/u/abc123"},
		preview:         session.GistPreviewURL("abc123"),
		updated:         true,
		requestedPublic: true,
	})
	m = result.(*Model)
	if !strings.Contains(m.dialog.Content(), "cannot make it public") {
		t.Fatalf("dialog did not explain visibility: %q", m.dialog.Content())
	}
}

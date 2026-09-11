package chat

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func (m *Model) cmdNew() (tea.Model, tea.Cmd) {
	if m.branchContextInFlight() {
		return m.showFooterWarning("Wait for path notes to finish, or press Esc to cancel them before starting a new session.")
	}
	m.clearSideQuestionHistory()
	m.pauseGoalForLocalAction("paused because a new session was started")
	m.clearPendingStreamModelSwitch()
	m.resetMainRunSessionBinding()
	// Mark the old session as complete before creating a new one
	if m.store != nil && m.sess != nil {
		_ = m.store.UpdateStatus(context.Background(), m.sess.ID, session.StatusComplete)
	}

	// Create new session with current settings
	m.sess = &session.Session{
		ID:           session.NewID(),
		Provider:     m.providerName,
		ProviderKey:  m.providerKey,
		Model:        m.modelName,
		Mode:         session.ModeChat,
		Agent:        m.agentName,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
		Search:       m.searchEnabled,
		Tools:        m.toolsStr,
		MCP:          m.mcpStr,
		ApprovalMode: sessionApprovalModeFromTools(m.requestedApprovalMode),
	}
	if cwd, err := os.Getwd(); err == nil {
		m.sess.CWD = cwd
		m.pendingTerminalDirectory = cwd
	}

	// Persist new session and infer its registered project from the CWD.
	persistNewTUISession(context.Background(), m.store, m.sess)
	m.notifySessionInputs()

	// Clear conversation messages and input
	m.messages = nil
	m.compactionIdx = 0
	m.scrollOffset = 0
	m.setTextareaValue("")
	m.clearFiles()
	m.pasteChunks = nil

	// Reset engine state (compaction tracking, provider conversation IDs)
	if m.engine != nil {
		m.engine.ResetConversation()
	}

	// Reset streaming and rendering state
	m.currentResponse.Reset()
	m.currentTokens = 0
	m.webSearchUsed = false
	m.retryStatus = ""
	if m.tracker != nil {
		m.resetTracker()
	}
	if m.smoothBuffer != nil {
		m.smoothBuffer.Reset()
	}
	m.smoothTickPending = false
	m.streamRenderTickPending = false

	// Reset stats for new session
	if m.stats != nil {
		m.stats = ui.NewSessionStats()
	}

	// Reset image renderer caches for this terminal session.
	ui.ClearRenderedImages()
	m.resetImageUploadState()

	// Invalidate view cache so stale content doesn't bleed through
	m.viewCache.historyValid = false
	m.viewCache.completedStream = ""
	m.viewCache.lastSetContentAt = time.Time{}
	m.resetAltScreenStreamingAppendCache()
	m.bumpContentVersion()
	m.resetTitleGenerationStateForSession()
	m.attachVisibleMainRunUISink()

	updated, footerCmd := m.showFooterSuccess("Started a new session.")
	return updated, tea.Batch(footerCmd, m.terminalTitleCmd())
}

func (m *Model) cmdTitleRaw(rawName string) (tea.Model, tea.Cmd) {
	m.setTextareaValue("")
	name := strings.TrimSpace(rawName)
	if name == "" {
		return m.showFooterError("Usage: /title <name>")
	}
	if m.sess == nil {
		return m.showFooterError("No active session to title.")
	}
	if m.store == nil {
		return m.showFooterError("Session storage is disabled. Enable it in config with `sessions.enabled: true`.")
	}

	m.sess.Name = name
	m.sess.TitleSource = session.TitleSourceUser
	m.titleManualEditVersion++
	if err := m.store.Update(context.Background(), m.sess); err != nil {
		return m.showFooterError(fmt.Sprintf("Failed to set session title: %v", err))
	}

	updated, footerCmd := m.showFooterSuccess(fmt.Sprintf("Session title set to '%s'.", name))
	return updated, tea.Batch(footerCmd, m.terminalTitleCmd())
}

func (m *Model) cmdAutotitle() (tea.Model, tea.Cmd) {
	m.setTextareaValue("")
	if m.sess == nil {
		return m.showFooterError("No active session to title.")
	}
	sessionID := strings.TrimSpace(m.sess.ID)
	if sessionID == "" {
		return m.showFooterError("No active session to title.")
	}
	if m.titleGenerationSessionID != sessionID {
		m.resetTitleGenerationStateForSession()
	}
	if m.titleGenerationInFlight {
		return m.showFooterError("Title generation is already running.")
	}
	if m.fastProvider == nil {
		return m.showFooterError("Fast title generation is unavailable.")
	}
	if m.store == nil {
		return m.showFooterError("Session storage is disabled. Enable it in config with `sessions.enabled: true`.")
	}

	m.messagesMu.Lock()
	hasMessages := len(m.messages) > 0
	m.messagesMu.Unlock()
	if !hasMessages {
		return m.showFooterError("No conversation messages to title yet.")
	}

	clearManualName := strings.TrimSpace(m.sess.Name) != ""
	cmd := m.generateSessionTitleCmd(true, clearManualName, m.titleManualEditVersion)
	if cmd == nil {
		return m.showFooterError("No conversation messages to title yet.")
	}
	return m, cmd
}

func (m *Model) cmdSave(args []string) (tea.Model, tea.Cmd) {
	name := ""
	if len(args) > 0 {
		name = strings.Join(args, "-")
	} else {
		// Generate name from first message or timestamp
		if len(m.messages) > 0 {
			// Use first few words of first user message
			for _, msg := range m.messages {
				if msg.Role == llm.RoleUser {
					words := strings.Fields(msg.TextContent)
					if len(words) > 5 {
						words = words[:5]
					}
					name = strings.Join(words, "-")
					name = strings.ToLower(name)
					// Remove special characters
					name = strings.Map(func(r rune) rune {
						if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
							return r
						}
						return -1
					}, name)
					break
				}
			}
		}
		if name == "" {
			name = fmt.Sprintf("session-%d", time.Now().Unix())
		}
	}

	if m.store == nil {
		m.setTextareaValue("")
		return m.showSystemMessage("Session storage is disabled. Enable it in config with `sessions.enabled: true`.")
	}

	m.sess.Name = name
	m.titleManualEditVersion++
	if err := m.store.Update(context.Background(), m.sess); err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to save session: %v", err))
	}

	m.setTextareaValue("")
	updated, footerCmd := m.showFooterSuccess(fmt.Sprintf("Saved session as '%s'.", name))
	return updated, tea.Batch(footerCmd, m.terminalTitleCmd())
}

func (m *Model) cmdResume(args []string) (tea.Model, tea.Cmd) {
	if m.branchContextInFlight() {
		return m.showFooterWarning("Wait for path notes to finish, or press Esc to cancel them before resuming another session.")
	}
	if m.store == nil {
		return m.showSystemMessage("Session storage is disabled.")
	}

	ctx := context.Background()

	// /resume <number|id> — direct resume without the browser.
	if len(args) > 0 {
		sess, err := m.store.GetByPrefix(ctx, args[0])
		if err != nil {
			return m.showSystemMessage(fmt.Sprintf("Failed to find session: %v", err))
		}
		if sess == nil {
			return m.showSystemMessage(fmt.Sprintf("Session '%s' not found.", args[0]))
		}

		m.setTextareaValue("")
		return m.requestResumeSession(sess.ID)
	}

	// /resume with no args — open the dedicated embedded sessions browser.
	summaries, err := m.store.List(ctx, session.ListOptions{Limit: 1})
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to list sessions: %v", err))
	}
	if len(summaries) == 0 {
		return m.showSystemMessage("No saved sessions found.")
	}

	return m.openResumeBrowser()
}

// resumeFormatAge returns a compact human-readable age string.
func resumeFormatAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

// resumeShortenModel returns a compact model name for display in the session picker.
// It strips the "claude-" prefix and any trailing 8-digit date suffix, then truncates.
func resumeShortenModel(model string) string {
	s := strings.TrimPrefix(model, "claude-")
	// Strip trailing date suffix of the form -YYYYMMDD
	if len(s) > 9 {
		suffix := s[len(s)-9:]
		if suffix[0] == '-' {
			allDigits := true
			for _, c := range suffix[1:] {
				if c < '0' || c > '9' {
					allDigits = false
					break
				}
			}
			if allDigits {
				s = s[:len(s)-9]
			}
		}
	}
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

func (m *Model) cmdThinking(args []string) (tea.Model, tea.Cmd) {
	valid := map[string]bool{
		config.ReasoningDisplayOff:       true,
		config.ReasoningDisplayStatus:    true,
		config.ReasoningDisplayCollapsed: true,
		config.ReasoningDisplayExpanded:  true,
		config.ReasoningDisplayRaw:       true,
	}

	mode := ""
	if len(args) > 0 {
		mode = strings.ToLower(strings.TrimSpace(args[0]))
		if mode == config.ReasoningDisplayAuto {
			mode = config.ReasoningDisplayCollapsed
		}
		if !valid[mode] {
			return m.showFooterWarning("Usage: /thinking [off|status|collapsed|expanded|raw]")
		}
	} else {
		current := m.reasoningModeOverride
		if current == "" {
			current = internalreasoning.EffectiveDisplay(m.effectiveReasoningConfig())
		}
		switch current {
		case config.ReasoningDisplayExpanded:
			mode = config.ReasoningDisplayOff
		case config.ReasoningDisplayOff:
			mode = config.ReasoningDisplayCollapsed
		default:
			mode = config.ReasoningDisplayExpanded
		}
	}

	cfg := m.effectiveReasoningConfig()
	cfg.Display = mode
	if mode == config.ReasoningDisplayRaw && !cfg.Raw {
		return m.showFooterWarning("Raw reasoning is disabled. Set reasoning.raw=true or TERM_LLM_SHOW_RAW_REASONING=1 to allow it.")
	}
	m.reasoningModeOverride = mode
	m.reasoningConfig.Display = mode
	if m.chatRenderer != nil {
		m.chatRenderer.SetReasoningConfig(m.effectiveReasoningConfig())
	}
	m.clearReasoningSegmentExpansionOverrides()
	m.forceHistoryRerender()
	m.applyReasoningPhase()
	m.setTextareaValue("")

	label := mode
	if mode == config.ReasoningDisplayRaw {
		label = "raw visible"
	}
	return m.showFooterSuccess(fmt.Sprintf("Reasoning display: %s", label))
}

func (m *Model) cmdExport(args []string) (tea.Model, tea.Cmd) {
	if len(m.messages) == 0 {
		return m.showSystemMessage("No messages to export.")
	}

	// Determine output path
	var outputPath string
	if len(args) > 0 {
		outputPath = strings.Join(args, " ")
	} else {
		// Generate default filename
		timestamp := time.Now().Format("2006-01-02_15-04-05")
		outputPath = fmt.Sprintf("chat-export-%s.md", timestamp)
	}

	// Build markdown content
	var b strings.Builder

	// Header
	b.WriteString("# Chat Export\n\n")
	b.WriteString(fmt.Sprintf("**Model:** %s (%s)\n", m.modelName, m.providerName))
	b.WriteString(fmt.Sprintf("**Exported:** %s\n", time.Now().Format("2006-01-02 15:04:05")))
	if m.sess.Name != "" {
		b.WriteString(fmt.Sprintf("**Session:** %s\n", m.sess.Name))
	}
	b.WriteString("\n---\n\n")

	// Messages
	exportReasoningCfg := m.effectiveReasoningConfig()
	includeReasoningSummaries := internalreasoning.ExportSummaries(exportReasoningCfg)
	includeRawReasoning := internalreasoning.ExportRaw(exportReasoningCfg)
	rawReasoningOmitted := strings.EqualFold(strings.TrimSpace(exportReasoningCfg.Export), config.ReasoningExportRaw) && !exportReasoningCfg.Raw
	for _, msg := range m.messages {
		// Role header
		if msg.Role == llm.RoleUser {
			b.WriteString("## ❯\n\n")
		} else {
			b.WriteString("## 🤖 Assistant")
			if msg.DurationMs > 0 {
				b.WriteString(fmt.Sprintf(" *(%.1fs)*", float64(msg.DurationMs)/1000))
			}
			b.WriteString("\n\n")
		}

		// Content - for user messages, extract just the text (not file contents).
		if msg.Role == llm.RoleUser {
			content := llm.StripEmbeddedFileText(msg.TextContent)
			b.WriteString(content)
			b.WriteString("\n---\n\n")
			continue
		}
		if msg.Role == llm.RoleAssistant {
			for _, part := range msg.Parts {
				if part.Type != llm.PartText {
					continue
				}
				if rendered := session.RenderExportReasoning(part, session.ExportOptions{
					IncludeReasoningSummaries: includeReasoningSummaries,
					IncludeRawReasoning:       includeRawReasoning,
				}); rendered != "" {
					b.WriteString(rendered)
				}
				if part.Text != "" {
					b.WriteString(part.Text)
					b.WriteString("\n\n")
				}
			}
			if len(msg.Parts) == 0 && msg.TextContent != "" {
				b.WriteString(msg.TextContent)
				b.WriteString("\n\n")
			}
		}
		b.WriteString("---\n\n")
	}

	// Write to file
	if err := os.WriteFile(outputPath, []byte(b.String()), 0644); err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to export: %v", err))
	}

	m.setTextareaValue("")
	message := fmt.Sprintf("Exported %d messages to %s.", len(m.messages), outputPath)
	if rawReasoningOmitted {
		return m.showFooterWarning(message + " Raw reasoning omitted; set reasoning.raw=true or TERM_LLM_SHOW_RAW_REASONING=1 to allow it.")
	}
	return m.showFooterSuccess(message)
}

func (m *Model) cmdSystem(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		if m.config.Chat.Instructions != "" {
			return m.showSystemMessage(fmt.Sprintf("Current system prompt:\n\n%s", m.config.Chat.Instructions))
		}
		return m.showSystemMessage("No system prompt set.\nUsage: `/system <prompt>`")
	}

	// Set custom system prompt (session-only, doesn't persist to config)
	prompt := strings.Join(args, " ")
	m.config.Chat.Instructions = prompt
	m.systemPromptOverridden = true
	m.systemPromptOverride = prompt
	m.setTextareaValue("")
	return m.showSystemMessage(fmt.Sprintf("System prompt set for this session:\n\n%s", prompt))
}

func (m *Model) cmdFile(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		if len(m.files) == 0 {
			return m.showSystemMessage("No files attached.\nUsage: `/file <path>` or `/file clear`")
		}
		// Show attached files
		var b strings.Builder
		b.WriteString("## Attached Files\n\n")
		var totalSize int64
		for _, f := range m.files {
			b.WriteString(fmt.Sprintf("- `%s` (%s)\n", f.Name, FormatFileSize(f.Size)))
			totalSize += f.Size
		}
		b.WriteString(fmt.Sprintf("\nTotal: %d file(s), %s", len(m.files), FormatFileSize(totalSize)))
		b.WriteString("\n\nUse `/file clear` to remove all attachments.")
		return m.showSystemMessage(b.String())
	}

	// Handle clear command
	if args[0] == "clear" {
		count := len(m.files)
		m.clearFiles()
		m.setTextareaValue("")
		if count == 0 {
			return m.showSystemMessage("No files were attached.")
		}
		return m.showFooterSuccess(fmt.Sprintf("Cleared %d attached file(s).", count))
	}

	// Join all args in case path has spaces
	path := strings.Join(args, " ")
	m.setTextareaValue("")

	// Check if it's a glob pattern
	if strings.ContainsAny(path, "*?[") {
		return m.attachFiles(path)
	}

	// Single file attachment
	return m.attachFile(path)
}

func (m *Model) cmdDirs(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		// List approved directories
		if len(m.approvedDirs.Directories) == 0 {
			return m.showSystemMessage("No approved directories.\n\nUse `/dirs add <path>` to approve a directory,\nor attach a file to be prompted for approval.")
		}

		var b strings.Builder
		b.WriteString("## Approved Directories\n\n")
		for _, dir := range m.approvedDirs.Directories {
			b.WriteString(fmt.Sprintf("- `%s`\n", dir))
		}
		b.WriteString("\n**Commands:**\n")
		b.WriteString("- `/dirs add <path>` - Approve a directory\n")
		b.WriteString("- `/dirs remove <path>` - Revoke approval")
		return m.showSystemMessage(b.String())
	}

	subCmd := strings.ToLower(args[0])
	subArgs := args[1:]

	switch subCmd {
	case "add":
		if len(subArgs) == 0 {
			return m.showSystemMessage("Usage: `/dirs add <path>`")
		}
		path := strings.Join(subArgs, " ")
		if err := m.approvedDirs.AddDirectory(path); err != nil {
			return m.showSystemMessage(fmt.Sprintf("Failed to add directory: %v", err))
		}
		m.setTextareaValue("")
		return m.showFooterSuccess(fmt.Sprintf("Approved directory: %s", path))

	case "remove", "rm", "delete":
		if len(subArgs) == 0 {
			return m.showSystemMessage("Usage: `/dirs remove <path>`")
		}
		path := strings.Join(subArgs, " ")
		if err := m.approvedDirs.RemoveDirectory(path); err != nil {
			return m.showSystemMessage(fmt.Sprintf("Failed to remove directory: %v", err))
		}
		m.setTextareaValue("")
		return m.showFooterSuccess(fmt.Sprintf("Removed approved directory: %s", path))

	default:
		return m.showSystemMessage(fmt.Sprintf("Unknown subcommand: %s\n\nUsage:\n- `/dirs` - List approved directories\n- `/dirs add <path>` - Approve a directory\n- `/dirs remove <path>` - Revoke approval", subCmd))
	}
}

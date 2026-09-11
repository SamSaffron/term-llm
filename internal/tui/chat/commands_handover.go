package chat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/llm"
	projectpkg "github.com/samsaffron/term-llm/internal/project"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func (m *Model) cmdHandover(args []string) (tea.Model, tea.Cmd) {
	m.pauseGoalForLocalAction("paused for handover")
	m.setTextareaValue("")

	if m.streaming {
		return m.showSystemMessage("Cannot handover while streaming. Wait for the response to finish.")
	}

	if len(args) == 0 {
		return m.showSystemMessage("Usage: /handover @agent [provider:model]\nExample: /handover @developer anthropic:claude-sonnet-4-5")
	}

	if m.agentResolver == nil {
		return m.showSystemMessage("Agent resolver not configured.")
	}

	if m.store == nil {
		return m.showSystemMessage("Handover requires session storage. Enable sessions to use /handover.")
	}

	// Parse agent name (strip @ prefix if present)
	agentName := strings.TrimPrefix(args[0], "@")

	// Optional provider:model override
	var providerStr string
	if len(args) > 1 {
		resolved, ok := resolveProviderModelArg(args[1], m.config, "")
		if !ok {
			return m.showSystemMessage(fmt.Sprintf("Invalid provider format: %s (expected provider:model)", args[1]))
		}
		providerStr = resolved
	}

	// Resolve target agent
	targetAgent, err := m.agentResolver(agentName, m.config)
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to load agent @%s: %v", agentName, err))
	}
	if targetAgent == nil {
		return m.showSystemMessage(fmt.Sprintf("Agent @%s not found.", agentName))
	}

	// Handover results are intermediate context documents. The target agent's
	// actual system prompt is resolved during executeHandover via the same pipeline
	// used to start a new chat session.

	// Determine handover mode from the current (source) agent
	mode := ""
	if m.currentAgent != nil {
		mode = m.currentAgent.HandoverMode
	}

	sourceAgent := m.agentName
	if sourceAgent == "" {
		sourceAgent = "default"
	}

	// Target-declared script takes precedence over source's handover mode, but it
	// only runs after the user explicitly confirms the handover.
	if strings.TrimSpace(targetAgent.HandoverScript) != "" {
		preview := pendingTargetScriptPreview(targetAgent)
		result := llm.HandoverFromFile(preview, transientHandoverSystemPrompt, sourceAgent, targetAgent.Name)
		return m, func() tea.Msg {
			return handoverDoneMsg{result: result, agentName: agentName, providerStr: providerStr}
		}
	}

	// Light mode: use last assistant message as handover context
	if mode == "light" {
		doc := m.lastAssistantMessage()
		if doc == "" {
			doc = "(No assistant response to hand over.)"
		}
		result := llm.HandoverFromFile(doc, transientHandoverSystemPrompt, sourceAgent, targetAgent.Name)
		return m, func() tea.Msg {
			return handoverDoneMsg{
				result:      result,
				agentName:   agentName,
				providerStr: providerStr,
			}
		}
	}

	// Script mode (source-side): run source agent's handover_script
	if mode == "script" {
		script := ""
		if m.currentAgent != nil {
			script = m.currentAgent.HandoverScript
		}
		if script == "" {
			return m.showSystemMessage("Agent has handover_mode: script but no handover_script configured.")
		}
		return m.startHandoverScriptHandover(m.currentAgent, sourceAgent, targetAgent, providerStr, false, "")
	}

	// File mode: use this session's pinned handover document. Legacy prompts
	// without a pinned assignment retain the latest-file fallback.
	if mode == "file" || (mode == "" && m.currentAgent != nil && m.currentAgent.EnableHandover) {
		handoverDir := ""
		pinnedPath := ""
		pathPinned := false
		if m.currentAgent != nil && m.currentAgent.EnableHandover {
			if path, dir, pinned, err := m.resolveHandoverPath(m.currentSystemPromptText()); err == nil {
				pinnedPath, handoverDir, pathPinned = path, dir, pinned
			}
		}
		if handoverDir != "" {
			// When the prompt assigns a path, never fall back to scanning — a
			// newer .md from a concurrent session must not shadow this session's
			// plan, even while ours is empty, missing, or ambiguous.
			var latestFile string
			var latestInfo os.FileInfo
			if pathPinned {
				if pinnedPath != "" {
					if info, err := os.Stat(pinnedPath); err == nil {
						latestFile, latestInfo = pinnedPath, info
					}
				}
			} else {
				latestFile, latestInfo = findLatestHandoverFile(handoverDir)
			}
			if latestFile != "" && latestInfo.Size() > 0 {
				// Check freshness: file must have been modified after the session started
				sessionStart := time.Time{}
				if m.sess != nil {
					sessionStart = m.sess.CreatedAt
				}
				stale := !sessionStart.IsZero() && latestInfo.ModTime().Before(sessionStart)
				if stale && mode == "file" {
					// Explicit file mode — hard fail on stale file
					return m.showSystemMessage(fmt.Sprintf(
						"Handover file %s exists but predates this session (last modified %s).\n"+
							"Ask the agent to update it, or remove it to use LLM compression.",
						latestFile, latestInfo.ModTime().Format("2006-01-02 15:04")))
				}
				if !stale {
					content, readErr := os.ReadFile(latestFile)
					if readErr == nil && len(content) > 0 {
						result := llm.HandoverFromFile(string(content), transientHandoverSystemPrompt, sourceAgent, targetAgent.Name)
						return m, func() tea.Msg {
							return handoverDoneMsg{
								result:      result,
								agentName:   agentName,
								providerStr: providerStr,
							}
						}
					}
				}
				// Auto mode with stale file: fall through to LLM compression
			}
			// File mode was explicitly set but no handover file found — warn
			if mode == "file" {
				return m.showSystemMessage(fmt.Sprintf(
					"Agent @%s has handover_mode: file but no .md files found in %s.\n"+
						"Ask the agent to write the handover document first.",
					sourceAgent, handoverDir))
			}
		}
	}

	// Compress mode (or auto fallback): generate from the active conversation
	// boundary shared with /compact, excluding history hidden by prior compaction.
	currentSystemPrompt, llmMessages := m.snapshotActiveHelperConversation()
	if len(llmMessages) < 2 {
		result := llm.HandoverFromFile("(No prior conversation to hand over.)", transientHandoverSystemPrompt, sourceAgent, targetAgent.Name)
		return m, func() tea.Msg {
			return handoverDoneMsg{
				result:      result,
				agentName:   agentName,
				providerStr: providerStr,
			}
		}
	}

	compactConfig := m.helperCompactionConfig()
	allowProviderFork := m.handoverToolDoneCh == nil
	model := m.modelName
	provider := m.provider

	// A tool-initiated handover continues the current engine stream, so leave
	// its tracker intact; only a manual /handover starts fresh.
	ctx := m.beginHelperStream("Handover", m.handoverToolDoneCh == nil)

	return m, tea.Batch(
		func() tea.Msg {
			result, err := llm.Handover(ctx, provider, model, currentSystemPrompt, transientHandoverSystemPrompt, llmMessages, sourceAgent, targetAgent.Name, compactConfig, llm.HandoverOptions{AllowProviderFork: allowProviderFork})
			return handoverDoneMsg{result: result, err: err, agentName: agentName, providerStr: providerStr}
		},
		m.spinner.Tick,
		m.tickEvery(),
	)
}

// resolveAgentTools derives the comma-separated tool list from an agent's config.
func resolveAgentTools(agent *agents.Agent) string {
	if agent.HasEnabledList() {
		return strings.Join(agent.Tools.Enabled, ",")
	}
	if agent.HasDisabledList() {
		allTools := tools.StandardToolNames()
		enabled := agent.GetEnabledTools(allTools)
		return strings.Join(enabled, ",")
	}
	return "" // No tool restrictions — use defaults
}

// lastAssistantMessage returns the text of the most recent assistant message.
func (m *Model) lastAssistantMessage() string {
	m.messagesMu.Lock()
	defer m.messagesMu.Unlock()

	for i := len(m.messages) - 1; i >= 0; i-- {
		if m.messages[i].Role == llm.RoleAssistant {
			var text strings.Builder
			for _, p := range m.messages[i].Parts {
				if p.Text != "" {
					text.WriteString(p.Text)
				}
			}
			return text.String()
		}
	}
	return ""
}

// pendingTargetScriptPreview describes a deferred target-side handover script.
func pendingTargetScriptPreview(agent *agents.Agent) string {
	var b strings.Builder
	b.WriteString("## Pending Target Handover Script\n\n")
	b.WriteString(fmt.Sprintf("@%s declares a handover script. It will run only after you confirm this handover and approve the command if required.\n", agent.Name))
	if script := strings.TrimSpace(agent.HandoverScript); script != "" {
		b.WriteString("\n```sh\n")
		b.WriteString(script)
		b.WriteString("\n```\n")
	}
	return b.String()
}

// currentSystemPromptText returns the text of this session's persisted system
// message, or "" if none exists.
func (m *Model) currentSystemPromptText() string {
	m.messagesMu.Lock()
	defer m.messagesMu.Unlock()
	for _, msg := range m.messages {
		if msg.Role != llm.RoleSystem {
			continue
		}
		for _, p := range msg.Parts {
			if p.Text != "" {
				return p.Text
			}
		}
	}
	return ""
}

// findLatestHandoverFile scans dir for .md files and returns the path and
// os.FileInfo of the most recently modified one. Returns ("", nil) if none found.
func findLatestHandoverFile(dir string) (string, os.FileInfo) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil
	}
	var latestPath string
	var latestInfo os.FileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if latestInfo == nil || info.ModTime().After(latestInfo.ModTime()) {
			latestPath = filepath.Join(dir, e.Name())
			latestInfo = info
		}
	}
	return latestPath, latestInfo
}

func handoverSourceAgent(pending *handoverDoneMsg, fallback string) string {
	if pending != nil && pending.result != nil && strings.TrimSpace(pending.result.SourceAgent) != "" {
		return strings.TrimSpace(pending.result.SourceAgent)
	}
	fallback = strings.TrimSpace(fallback)
	if fallback == "" {
		return "default"
	}
	return fallback
}

var runHandoverScriptForCmd = runHandoverScript

func handoverScriptCmd(ctx context.Context, approvalMgr *tools.ApprovalManager, scriptAgent *agents.Agent, sourceAgent string, targetAgent *agents.Agent, provider string, confirmed bool, instructions string) tea.Cmd {
	agentName := ""
	if targetAgent != nil {
		agentName = targetAgent.Name
	}
	return func() tea.Msg {
		result, err := buildScriptBackedHandover(ctx, approvalMgr, scriptAgent, sourceAgent, targetAgent, provider, instructions)
		return handoverDoneMsg{
			result:       result,
			err:          err,
			agentName:    agentName,
			providerStr:  provider,
			confirmed:    confirmed,
			instructions: instructions,
		}
	}
}

func buildScriptBackedHandover(ctx context.Context, approvalMgr *tools.ApprovalManager, scriptAgent *agents.Agent, sourceAgent string, targetAgent *agents.Agent, provider string, instructions string) (*llm.HandoverResult, error) {
	if scriptAgent == nil {
		return nil, fmt.Errorf("handover script agent is not configured")
	}
	if targetAgent == nil {
		return nil, fmt.Errorf("target agent is not configured")
	}
	doc, err := runHandoverScriptForCmd(ctx, approvalMgr, scriptAgent, scriptAgent.HandoverScript, handoverApprovalTranscript(sourceAgent, provider, instructions))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(doc) == "" {
		return nil, fmt.Errorf("handover script produced no output")
	}
	return llm.HandoverFromFile(doc, transientHandoverSystemPrompt, sourceAgent, targetAgent.Name), nil
}

func (m *Model) startHandoverScriptHandover(scriptAgent *agents.Agent, sourceAgent string, targetAgent *agents.Agent, providerStr string, confirmed bool, instructions string) (tea.Model, tea.Cmd) {
	// As with the LLM handover path, only drop the retained tracker for a
	// manual handover; a tool-initiated handover continues the engine stream.
	ctx := m.beginHelperStream("Handover", m.handoverToolDoneCh == nil)

	return m, tea.Batch(
		handoverScriptCmd(ctx, m.handoverApprovalMgr, scriptAgent, sourceAgent, targetAgent, providerStr, confirmed, instructions),
		m.spinner.Tick,
		m.tickEvery(),
	)
}

// runHandoverScript executes a handover command without invoking a shell and returns its stdout.
func runHandoverScript(ctx context.Context, approvalMgr *tools.ApprovalManager, agent *agents.Agent, script string, transcript []tools.TranscriptEntry) (string, error) {
	script = strings.TrimSpace(script)
	if script == "" {
		return "", fmt.Errorf("handover_script is empty")
	}
	if tools.HasUnsafeShellSyntax(script) {
		return "", fmt.Errorf("handover_script must be a single executable plus arguments; shell operators like |, &&, or redirection are not supported")
	}

	argv, err := tools.SplitShellWords(script)
	if err != nil {
		return "", fmt.Errorf("invalid handover_script: %w", err)
	}
	if len(argv) == 0 {
		return "", fmt.Errorf("handover_script is empty")
	}

	execPath, workDir, err := resolveHandoverScriptCommand(agent, argv)
	if err != nil {
		return "", err
	}

	if approvalMgr == nil {
		return "", fmt.Errorf("handover script approval is not configured")
	}
	outcome, err := approvalMgr.CheckShellApprovalWithContext(ctx, script, workDir, transcript)
	if err != nil {
		if rationale, ok := tools.GuardianDenialReason(err); ok {
			return "", fmt.Errorf("handover script denied by guardian: %s", rationale)
		}
		return "", err
	}
	if outcome == tools.Cancel {
		return "", fmt.Errorf("handover script not approved")
	}

	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(execCtx, execPath, argv[1:]...)
	cmd.Dir = workDir
	cmd.Env = os.Environ()

	cleanup, prepErr := tools.PrepareCommand(cmd)
	if prepErr != nil {
		return "", fmt.Errorf("handover script setup failed: %w", prepErr)
	}
	defer cleanup()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("handover script timed out after 30s")
	}
	if errors.Is(execCtx.Err(), context.Canceled) {
		return "", execCtx.Err()
	}
	if err != nil {
		if output := strings.TrimSpace(stderr.String()); output != "" {
			return "", fmt.Errorf("handover script failed: %w: %s", err, output)
		}
		return "", fmt.Errorf("handover script failed: %w", err)
	}
	return stdout.String(), nil
}

func resolveHandoverScriptCommand(agent *agents.Agent, argv []string) (string, string, error) {
	workDir, err := os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("resolve working directory: %w", err)
	}
	if gitInfo := tools.DetectGitRepo(workDir); gitInfo.IsRepo {
		workDir = gitInfo.Root
	}

	execPath := argv[0]
	if isRelativeCommandPath(execPath) {
		if agent == nil || !filepath.IsAbs(agent.SourcePath) || agent.Source == agents.SourceBuiltin {
			return "", "", fmt.Errorf("relative handover_script path %q requires a filesystem-backed agent source", execPath)
		}
		workDir = agent.SourcePath
		execPath = filepath.Join(agent.SourcePath, execPath)
	}

	return execPath, workDir, nil
}

func isRelativeCommandPath(path string) bool {
	if filepath.IsAbs(path) {
		return false
	}
	return strings.Contains(path, "/") || strings.Contains(path, "\\") || strings.HasPrefix(path, ".")
}

func applyHandoverInstructions(result *llm.HandoverResult, instructions string) *llm.HandoverResult {
	instructions = strings.TrimSpace(instructions)
	if result == nil || instructions == "" {
		return result
	}
	document := "## Additional Instructions\n\n" + instructions + "\n\n" + result.Document
	return &llm.HandoverResult{
		Document:    document,
		NewMessages: llm.ReconstructHandoverHistory(handoverSystemPrompt(result.NewMessages), document, result.SourceAgent, result.TargetAgent),
		SourceAgent: result.SourceAgent,
		TargetAgent: result.TargetAgent,
		Model:       result.Model,
		Usage:       result.Usage,
	}
}

func handoverSystemPrompt(messages []llm.Message) string {
	if len(messages) == 0 || messages[0].Role != llm.RoleSystem {
		return ""
	}
	for _, p := range messages[0].Parts {
		if p.Text != "" {
			return p.Text
		}
	}
	return ""
}

func handoverMessagesToPersist(messages []llm.Message, systemPrompt string) []llm.Message {
	start := 0
	for start < len(messages) && messages[start].Role == llm.RoleSystem {
		start++
	}

	if strings.TrimSpace(systemPrompt) == "" {
		return messages[start:]
	}

	out := make([]llm.Message, 0, len(messages)-start+1)
	out = append(out, llm.SystemText(systemPrompt))
	out = append(out, messages[start:]...)
	return out
}

// executeHandover performs the actual agent switch after user confirmation.
// It creates a brand-new session containing only the reconstructed handover
// context, then triggers a TUI restart so the target agent's full runtime
// configuration (tools, permissions, MCP, shell allowlists) is applied via the
// normal startup path. The source session is left intact to avoid cross-talk.
func (m *Model) executeHandover() (tea.Model, tea.Cmd) {
	if m.pendingHandover == nil {
		return m, nil
	}

	pending := m.pendingHandover

	// Step 1: Resolve target agent before any mutations.
	targetAgent, resolveErr := m.agentResolver(pending.agentName, m.config)
	if resolveErr != nil || targetAgent == nil {
		m.cancelHandoverTool()
		msg := "unknown error"
		if resolveErr != nil {
			msg = resolveErr.Error()
		}
		return m.showFooterError(fmt.Sprintf("Handover failed to resolve target agent: %s", msg))
	}

	result := applyHandoverInstructions(pending.result, pending.instructions)
	if result == nil {
		m.cancelHandoverTool()
		return m.showFooterError("Handover failed: no result returned.")
	}
	if m.store == nil {
		m.cancelHandoverTool()
		return m.showFooterError("Handover failed to persist: session storage is not configured")
	}

	ctx := context.Background()

	// Step 2: Build a fresh target session. Handover must never compact or mutate
	// the current session: the new agent gets a clean DB session whose history is
	// only the reconstructed handover context.
	newSess := m.buildHandoverSession(pending, targetAgent)

	var targetSystemPrompt string
	var err error
	targetDir := strings.TrimSpace(newSess.WorktreeDir)
	if targetDir == "" {
		targetDir = strings.TrimSpace(newSess.CWD)
	}
	if m.runtimeSystemContextResolver != nil {
		resolved, resolveErr := m.runtimeSystemContextResolver(targetAgent, newSess.ProviderKey, newSess.Model, targetDir)
		err = resolveErr
		targetSystemPrompt = resolved.SystemPrompt
	} else if m.handoverSystemPromptResolver != nil {
		targetSystemPrompt, err = m.handoverSystemPromptResolver(targetAgent, newSess.ProviderKey, newSess.Model)
	} else {
		m.cancelHandoverTool()
		return m.showFooterError("Handover failed to resolve target system prompt: resolver is not configured")
	}
	if err != nil {
		m.cancelHandoverTool()
		return m.showFooterError(fmt.Sprintf("Handover failed to resolve target system prompt: %v", err))
	}

	lookupCtx, cancelLookup := context.WithTimeout(ctx, 2*time.Second)
	assignedProject, _ := projectpkg.AssignSessionForDir(lookupCtx, m.store, newSess, newSess.CWD)
	cancelLookup()
	if err := m.store.Create(ctx, newSess); err != nil {
		m.cancelHandoverTool()
		return m.showFooterError(fmt.Sprintf("Handover failed to persist: %v", err))
	}
	if assignedProject != nil {
		reconcileTUIProjectInBackground(m.store, *assignedProject)
	}
	cleanupNewSession := func() {
		_ = m.store.Delete(context.Background(), newSess.ID)
	}

	// Carry the source conversation's immutable time anchor through reconstructed
	// handover history. Legacy source sessions fall back to their persisted
	// creation time rather than being mislabeled with handover or reload time.
	sourceMessages := make([]llm.Message, 0, len(m.messages))
	for i := range m.messages {
		sourceMessages = append(sourceMessages, m.messages[i].ToLLMMessage())
	}
	if platformText := targetAgent.PlatformMessages.For("chat"); platformText != "" {
		result.NewMessages = llm.InsertPlatformContext(result.NewMessages, []llm.Message{llm.PlatformContextMessage(platformText)})
	}
	if targetAgent.TimeGroundingEnabled() {
		result.NewMessages = llm.InsertConversationStart(result.NewMessages, sourceMessages)
		if _, ok := llm.ConversationStartFrom(result.NewMessages); !ok {
			start := newSess.CreatedAt
			if m.sess != nil && !m.sess.CreatedAt.IsZero() {
				start = m.sess.CreatedAt
			}
			result.NewMessages = llm.InsertConversationStart(result.NewMessages, []llm.Message{llm.ConversationStartMessage(start)})
		}
	} else {
		result.NewMessages = llm.WithoutConversationStart(result.NewMessages)
	}

	// Persist the target agent's resolved system prompt first, followed by the
	// conversational handover context. Keeping the system prompt at sequence 0 is
	// required so every resume/reload sees the target agent's prompt before the
	// handover document and does not inject duplicates later.
	for _, msg := range handoverMessagesToPersist(result.NewMessages, targetSystemPrompt) {
		newMsg := session.NewMessage(newSess.ID, msg, -1)
		if err := m.store.AddMessage(ctx, newSess.ID, newMsg); err != nil {
			cleanupNewSession()
			m.cancelHandoverTool()
			return m.showFooterError(fmt.Sprintf("Handover failed to persist: %v", err))
		}
	}

	if err := m.store.SetCurrent(ctx, newSess.ID); err != nil {
		cleanupNewSession()
		m.cancelHandoverTool()
		return m.showFooterError(fmt.Sprintf("Handover failed to set current session: %v", err))
	}

	if m.sessionInputsObserver != nil {
		m.sessionInputsObserver(newSess, targetSystemPrompt, newSess.Tools)
	}
	// Mark the source session complete only after the target session is fully
	// committed and current. Failure here is best-effort and should not undo the
	// successful handover.
	if m.sess != nil && m.sess.ID != "" {
		_ = m.store.UpdateStatus(ctx, m.sess.ID, session.StatusComplete)
	}
	m.pendingHandover = nil

	// Step 3: Trigger TUI restart via the same resume mechanism used by /resume.
	// This causes runChatOnce to exit and the outer loop to re-enter with the new
	// session, running the full agent setup path. Do not append handover messages
	// into the source session's in-memory scrollback; the dying model should not
	// show or later persist target-agent context under the source session.
	m.pendingResumeSessionID = newSess.ID
	if prompt := strings.TrimSpace(targetAgent.DefaultPrompt); prompt != "" {
		m.pendingHandoverAutoSend = prompt
	} else if pending != nil && pending.result != nil {
		m.pendingHandoverAutoSend = "Execute the pending tasks from the handover."
	} else {
		m.pendingHandoverAutoSend = ""
	}
	// Signal tool-initiated handover (if any) now that the handover is committed.
	// The session is about to restart so the tool result is moot,
	// but we unblock the goroutine to avoid a leak.
	if m.handoverToolDoneCh != nil {
		m.handoverToolDoneCh <- true
		m.handoverToolDoneCh = nil
	}
	// Cancel the engine stream now that the tool is unblocked.
	if m.streamCancelFunc != nil {
		m.streamCancelFunc()
		m.streamCancelFunc = nil
	}
	m.detachMainRun()
	m.quitting = true
	return m, m.quitCmd()
}

func (m *Model) buildHandoverSession(pending *handoverDoneMsg, targetAgent *agents.Agent) *session.Session {
	agentName := strings.TrimSpace(pending.agentName)
	if agentName == "" && targetAgent != nil {
		agentName = targetAgent.Name
	}

	providerKey := m.providerKey
	modelName := m.modelName
	if pending.providerStr != "" {
		parts := strings.SplitN(pending.providerStr, ":", 2)
		if len(parts) == 2 {
			providerKey = parts[0]
			modelName = parts[1]
		}
	} else if targetAgent != nil {
		// Apply agent provider and model independently (matching ResolveSettings behavior).
		if targetAgent.Provider != "" {
			providerKey = targetAgent.Provider
		}
		if targetAgent.Model != "" {
			modelName = targetAgent.Model
		}
	}

	providerLabel := m.providerName
	if providerKey != m.providerKey || modelName != m.modelName {
		providerLabel = providerKey
		if modelName != "" {
			providerLabel = fmt.Sprintf("%s (%s)", providerKey, modelName)
		}
	}
	if providerLabel == "" {
		providerLabel = providerKey
	}

	toolsStr := ""
	searchEnabled := false
	mcpStr := ""
	if targetAgent != nil {
		searchEnabled = targetAgent.Search
		toolsStr = resolveAgentTools(targetAgent)
		mcpNames := targetAgent.GetMCPServerNames()
		if len(mcpNames) > 0 {
			mcpStr = strings.Join(mcpNames, ",")
		}
	}

	now := time.Now()
	newSess := &session.Session{
		ID:            session.NewID(),
		Provider:      providerLabel,
		ProviderKey:   providerKey,
		Model:         modelName,
		Mode:          session.ModeChat,
		Origin:        session.OriginTUI,
		Agent:         agentName,
		CreatedAt:     now,
		UpdatedAt:     now,
		Search:        searchEnabled,
		Tools:         toolsStr,
		MCP:           mcpStr,
		ApprovalMode:  sessionApprovalModeFromTools(m.requestedApprovalMode),
		Status:        session.StatusActive,
		CompactionSeq: -1,
	}
	if m.sess != nil {
		newSess.ProjectID = m.sess.ProjectID
		newSess.ProjectName = m.sess.ProjectName
		if worktreeDir := strings.TrimSpace(m.sess.WorktreeDir); worktreeDir != "" {
			newSess.WorktreeDir = worktreeDir
			newSess.CWD = worktreeDir
			return newSess
		}
		if cwd := strings.TrimSpace(m.sess.CWD); cwd != "" {
			newSess.CWD = cwd
			return newSess
		}
	}
	if m.toolMgr != nil {
		if baseDir := strings.TrimSpace(m.toolMgr.BaseDir()); baseDir != "" {
			newSess.CWD = baseDir
			return newSess
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		newSess.CWD = cwd
	}
	return newSess
}

func handoverApprovalTranscript(sourceAgent, targetAgent, instructions string) []tools.TranscriptEntry {
	var b strings.Builder
	b.WriteString("The operator initiated a handover")
	if strings.TrimSpace(sourceAgent) != "" || strings.TrimSpace(targetAgent) != "" {
		fmt.Fprintf(&b, " from agent %q to agent %q", strings.TrimSpace(sourceAgent), strings.TrimSpace(targetAgent))
	}
	b.WriteString(". Running the configured handover_script is the narrow handover preparation step.")
	if strings.TrimSpace(instructions) != "" {
		fmt.Fprintf(&b, " Operator handover instructions: %s", strings.TrimSpace(instructions))
	}
	return []tools.TranscriptEntry{{Role: "user", Text: b.String()}}
}

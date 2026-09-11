package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/inspector"
)

func (m *Model) cmdSkills(args []string, rawArgs string) (tea.Model, tea.Cmd) {
	return m.executeSkillsCommand(args, rawArgs)
}

func (m *Model) newInspectorConfig() *inspector.Config {
	var toolSpecs []llm.ToolSpec
	if m.engine != nil {
		sessionID := ""
		if m.sess != nil {
			sessionID = m.sess.ID
		}
		toolSpecs = append(toolSpecs, m.engine.ToolDiscoveryActiveSpecs(sessionID)...)
	}
	if len(m.localTools) > 0 && m.engine != nil {
		for _, specName := range m.localTools {
			if tool, ok := m.engine.Tools().Get(specName); ok {
				toolSpecs = append(toolSpecs, tool.Spec())
			}
		}
	}
	toolSpecs = tools.FilterToolSpecsForApprovalMode(toolSpecs, m.approvalMgr)

	cfg := &inspector.Config{
		ProviderName:            m.providerName,
		ModelName:               m.modelName,
		ToolSpecs:               toolSpecs,
		ReasoningConfig:         m.effectiveReasoningConfig(),
		HasCompactionBoundary:   m.compactionIdx > 0 || session.HasCompactionBoundary(m.sess),
		CompactionBoundaryIndex: -1,
		CompactionBoundarySeq:   -1,
		CompactionCount:         0,
	}
	if m.compactionIdx > 0 {
		cfg.CompactionBoundaryIndex = m.compactionIdx
	}
	if m.sess != nil {
		cfg.CompactionBoundarySeq = m.sess.CompactionSeq
		cfg.CompactionCount = m.sess.CompactionCount
	}
	if m.engine != nil {
		sessionID := ""
		if m.sess != nil {
			sessionID = m.sess.ID
		}
		if diagnostics, ok := m.engine.ToolDiscoveryDiagnostics(sessionID); ok {
			cfg.ToolDiscovery = &diagnostics
		}
	}
	return cfg
}

func (m *Model) cmdInspect() (tea.Model, tea.Cmd) {
	m.setTextareaValue("")

	if len(m.messages) == 0 {
		return m.showSystemMessage("No messages to inspect. Send a message first.")
	}

	m.inspectorMode = true
	m.inspectorModel = inspector.NewWithConfig(m.messages, m.width, m.height, m.styles, m.store, m.newInspectorConfig())
	return m, nil
}

func (m *Model) snapshotActiveHelperConversation() (string, []llm.Message) {
	m.messagesMu.Lock()
	snapshot := append([]session.Message(nil), m.messages...)
	compactionIdx := m.compactionIdx
	m.messagesMu.Unlock()

	systemPrompt := ""
	for _, msg := range snapshot {
		if msg.Role == llm.RoleSystem {
			if text := strings.TrimSpace(llm.MessageText(msg.ToLLMMessage())); text != "" {
				systemPrompt = text
				break
			}
		}
	}
	if compactionIdx > 0 {
		if compactionIdx >= len(snapshot) {
			snapshot = nil
		} else {
			snapshot = snapshot[compactionIdx:]
		}
	}

	messages := make([]llm.Message, 0, len(snapshot))
	for _, msg := range snapshot {
		if msg.Role != llm.RoleSystem {
			messages = append(messages, msg.ToLLMMessage())
		}
	}
	return systemPrompt, messages
}

func (m *Model) helperCompactionConfig() llm.CompactionConfig {
	cfg := llm.DefaultCompactionConfig()
	if m.engine != nil && m.engine.InputLimit() > 0 {
		cfg.InputLimit = m.engine.InputLimit()
	} else if limit := llm.InputLimitForProviderModel(m.providerKey, m.modelName); limit > 0 {
		cfg.InputLimit = limit
	}
	return cfg
}

func (m *Model) beginHelperStream(phase string, resetRetainedTracker bool) context.Context {
	m.clearFooterMessage()
	m.streaming = true
	if resetRetainedTracker {
		m.resetRetainedStreamTracker()
	}
	m.phase = phase
	m.streamStartTime = time.Now()
	m.streamElapsedOffset = 0
	if m.altScreen {
		m.scrollToBottom = true
	}
	ctx, cancel := context.WithCancel(m.rootContext())
	m.streamCancelFunc = cancel
	m.streamCleanupFunc = cancel
	return ctx
}

func (m *Model) cmdCompress(args ...string) (tea.Model, tea.Cmd) {
	m.pauseGoalForLocalAction("paused for context compaction")
	m.setTextareaValue("")

	mode := "soft"
	if len(args) > 1 {
		return m.showSystemMessage("Usage: /compact [hard]")
	}
	if len(args) == 1 {
		switch strings.ToLower(strings.TrimSpace(args[0])) {
		case "", "soft":
			mode = "soft"
		case "hard":
			mode = "hard"
		default:
			return m.showSystemMessage("Usage: /compact [hard]")
		}
	}

	if m.streaming {
		return m.showSystemMessage("Cannot compress while streaming. Wait for the response to finish.")
	}

	systemPrompt, llmMessages := m.snapshotActiveHelperConversation()
	if len(llmMessages) < 2 {
		return m.showSystemMessage("Not enough conversation history to compress.")
	}

	compactConfig := m.helperCompactionConfig()
	model := m.modelName
	provider := m.provider
	phase := llm.PhaseCompacting
	if mode == "hard" {
		phase = llm.PhaseCompactingSummarizeHistory
	}

	ctx := m.beginHelperStream(phase, true)

	return m, tea.Batch(
		func() tea.Msg {
			var (
				result *llm.CompactionResult
				err    error
			)
			if mode == "hard" {
				result, err = llm.Compact(ctx, provider, model, systemPrompt, llmMessages, compactConfig)
			} else {
				result, err = llm.SoftCompact(ctx, provider, model, systemPrompt, llmMessages, compactConfig)
			}
			return compactDoneMsg{result: result, err: err}
		},
		m.spinner.Tick,
		m.tickEvery(),
	)
}

type switchModelOptions struct {
	deferMarker bool
}

// switchModel switches to a new provider:model
func (m *Model) switchModel(providerModel string) (tea.Model, tea.Cmd) {
	return m.switchModelWithOptions(providerModel, switchModelOptions{})
}

func (m *Model) switchModelWithOptions(providerModel string, opts switchModelOptions) (tea.Model, tea.Cmd) {
	parts := strings.SplitN(providerModel, ":", 2)
	if len(parts) != 2 {
		return m.showSystemMessage(fmt.Sprintf("Invalid model format: %s", providerModel))
	}

	providerName := parts[0]
	modelName := parts[1]

	oldProvider := strings.TrimSpace(m.providerKey)
	if oldProvider == "" {
		oldProvider = strings.TrimSpace(m.providerName)
	}
	oldModel := strings.TrimSpace(m.modelName)

	// Create new provider using the centralized factory
	provider, err := llm.NewProviderByName(m.config, providerName, modelName)
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to switch model: %v", err))
	}
	m.clearPendingStreamModelSwitch()
	if !opts.deferMarker {
		m.pendingModelSwitch = nil
	}

	// Update model state
	m.provider = provider
	// Preserve existing tool registry when creating new engine.
	m.replaceEngine(llm.NewEngine(provider, m.engine.Tools()))
	m.providerName = provider.Name()
	m.providerKey = providerName
	m.modelName = modelName

	// Cached model capabilities are provider-scoped. Never carry one provider's
	// metadata into another provider's effort parser.
	if !strings.EqualFold(oldProvider, providerName) {
		m.modelMetadata = nil
		m.fastMetadataLoaded = false
		m.fastMetadataStale = false
	}

	// Recompute fast/service-tier state for the new provider. /fast overrides are
	// per-provider-session state, so switching models/providers returns to config.
	var fastMetadataCmd tea.Cmd
	m.fastOverride = serviceTierInherit
	m.fastProviderDefault = m.configuredFastDefault()
	m.pendingFastToggle = false
	m.refreshEffectiveFastMode()
	if m.canLoadModelMetadata() && (!m.fastMetadataLoaded || m.fastMetadataStale) {
		fastMetadataCmd = m.loadModelMetadataCmd()
	} else if !m.supportsServiceTierToggle() {
		m.fastMode = false
	}

	// Keep session metadata aligned so future resume restores correct runtime.
	if m.sess != nil {
		m.sess.Provider = m.providerName
		m.sess.ProviderKey = m.providerKey
		m.sess.Model = modelName
		if m.store != nil {
			_ = m.store.Update(context.Background(), m.sess)
		}
	}

	// Record model usage for MRU ordering in the picker
	m.recordCurrentModelUse()
	m.setTextareaValue("")
	guardianErr := m.refreshGuardianReviewer(providerName, modelName)

	if m.sess != nil && len(m.messages) > 0 {
		marker := llm.ModelSwapMarker{
			FromProvider: oldProvider,
			FromModel:    oldModel,
			ToProvider:   providerName,
			ToModel:      modelName,
			Status:       "started",
		}
		if opts.deferMarker {
			m.deferModelSwitchMarker(marker)
		} else {
			m.appendModelSwitchMarker(marker)
		}
	}

	msg := fmt.Sprintf("Switched model to %s:%s. Next response will try the existing context; if incompatible, use /handover to prepare a compact handoff.", providerName, modelName)
	if guardianErr != nil {
		msg += fmt.Sprintf(" Guardian auto-approval was disabled: %v", guardianErr)
		if fastMetadataCmd != nil {
			_, footerCmd := m.showFooterWarning(msg)
			return m, tea.Batch(footerCmd, fastMetadataCmd)
		}
		return m.showFooterWarning(msg)
	}
	if fastMetadataCmd != nil {
		_, footerCmd := m.showFooterMuted(msg)
		return m, tea.Batch(footerCmd, fastMetadataCmd)
	}
	return m.showFooterMuted(msg)
}

func (m *Model) applyPendingStreamModelSwitch() tea.Cmd {
	if m.pendingStreamModelSwitch == nil {
		return nil
	}
	pending := *m.pendingStreamModelSwitch
	m.clearPendingStreamModelSwitch()
	if strings.TrimSpace(pending.provider) == "" || strings.TrimSpace(pending.model) == "" {
		return nil
	}
	provider, model := m.currentProviderAndModel()
	if provider == pending.provider && model == pending.model {
		return nil
	}

	// Preserve any still-queued steering across the engine replacement. A
	// text-only stream can finish with queued steering that were never
	// committed; applying a deferred effort switch must not strand them on the old
	// engine before restorePendingSteeringDraft has a chance to recover them.
	queuedSteering := m.listPendingSteering()

	// switchModelWithOptions clears the composer because most model switches are
	// explicit slash commands. A queued in-stream effort switch is applied
	// asynchronously, so preserve any draft the user typed while waiting for the
	// current response to finish.
	draft := m.captureComposerSnapshot()
	if m.config == nil {
		m.config = &config.Config{}
	}
	_, cmd := m.switchModelWithOptions(pending.provider+":"+pending.model, switchModelOptions{deferMarker: true})
	if m.engine != nil && m.providerKey == pending.provider && m.modelName == pending.model {
		for _, entry := range queuedSteering {
			m.engine.QueueSteering(entry)
		}
	}
	m.restoreComposerSnapshot(draft)
	return cmd
}

func (m *Model) deferModelSwitchMarker(marker llm.ModelSwapMarker) {
	if m.pendingModelSwitch != nil {
		m.pendingModelSwitch.ToProvider = marker.ToProvider
		m.pendingModelSwitch.ToModel = marker.ToModel
		m.pendingModelSwitch.Status = marker.Status
		return
	}
	pending := marker
	m.pendingModelSwitch = &pending
}

func (m *Model) appendPendingModelSwitchMarker() {
	if m.pendingModelSwitch == nil {
		return
	}
	marker := *m.pendingModelSwitch
	m.pendingModelSwitch = nil
	m.appendModelSwitchMarker(marker)
}

func (m *Model) appendModelSwitchMarker(marker llm.ModelSwapMarker) {
	if m.sess == nil {
		return
	}
	msg := llm.ModelSwapEventMessage(marker)
	sm := *session.NewMessage(m.sess.ID, msg, -1)
	m.messagesMu.Lock()
	m.messages = append(m.messages, sm)
	m.messagesMu.Unlock()
	if m.store != nil {
		_ = m.store.AddMessage(context.Background(), m.sess.ID, &sm)
	}
	// completedStream is an alt-screen-only cache of the response that was just
	// streamed. Once we append a model-switch marker after that assistant turn,
	// the cache is no longer a tail replacement; leaving it in place renders the
	// persisted assistant from history plus the cached stream again.
	m.invalidateViewCache()
}

// transientHandoverSystemPrompt intentionally keeps generated handover results
// free of target-agent system prompts. executeHandover resolves the target prompt
// through the normal chat startup pipeline and prepends it when persisting the
// new session, so these intermediate results should carry handover context only.
const transientHandoverSystemPrompt = ""

// cmdHandover handles /handover @agent [provider:model]

package cmd

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/chat"
	"github.com/spf13/cobra"
)

// buildConcurrentSideChatModel constructs a complete ownership graph for a side
// conversation. Nothing mutable is borrowed from the parent runtime: provider,
// engine, registry, tool/approval manager, MCP manager and model state are new.
func buildConcurrentSideChatModel(rootCtx context.Context, cmd *cobra.Command, store session.Store, sessionID, autoSend string, useAltScreen bool, send func(tea.Msg)) (*chat.Model, error) {
	sess, err := store.Get(context.Background(), sessionID)
	if err != nil {
		return nil, fmt.Errorf("load side conversation: %w", err)
	}
	if sess == nil || sess.Kind != session.KindSide || sess.SideState != session.SideOpen {
		return nil, session.ErrSideClosed
	}

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return nil, err
	}
	agent, err := LoadAgent(strings.TrimSpace(sess.Agent), cfg)
	if err != nil {
		return nil, err
	}
	settings, err := ResolveSettingsInDir(cfg, agent, CLIFlags{
		Provider:      chatProvider,
		Tools:         sess.Tools,
		MCP:           "",
		SystemMessage: chatSystemMessage,
		MaxTurns:      chatMaxTurns,
		MaxTurnsSet:   cmd.Flags().Changed("max-turns"),
		Search:        sess.Search,
		Platform:      "chat",
	}, cfg.Chat.Provider, cfg.Chat.Model, cfg.Chat.Instructions, cfg.Chat.MaxTurns, 200, effectiveSessionDirectory(sess))
	if err != nil {
		return nil, err
	}
	settings.Search = sess.Search
	settings.Tools = sess.Tools
	settings.MCP = ""
	settings.SessionID = sess.ID
	if dir := effectiveSessionDirectory(sess); dir != "" {
		settings.BaseDir = dir
		settings.ReadDirs = append(settings.ReadDirs, dir)
		settings.WriteDirs = append(settings.WriteDirs, dir)
		settings.ShellWorkingDir = dir
	}

	providerKey := resolveSessionProviderKey(cfg, sess)
	if providerKey == "" {
		providerKey = cfg.DefaultProvider
	}
	providerOverride := providerKey
	if model := strings.TrimSpace(sess.Model); model != "" {
		providerOverride += ":" + model
	}
	if err := applyProviderOverridesWithAgent(cfg, cfg.Chat.Provider, cfg.Chat.Model, providerOverride, "", ""); err != nil {
		return nil, err
	}
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return nil, err
	}
	fastProvider, _ := llm.NewFastProvider(cfg, cfg.DefaultProvider)
	engine := newEngine(provider, cfg)
	alignSettingsToActiveProvider(&settings, cfg, provider)
	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		if cleaner, ok := provider.(llm.ProviderCleaner); ok {
			cleaner.CleanupMCP()
		}
		return nil, err
	}
	if toolMgr != nil {
		_ = RestoreWorktreeBinding(context.Background(), store, sess, toolMgr)
		toolMgr.ApprovalMgr.SetApprovalMode(tools.ModePrompt)
		toolMgr.ApprovalMgr.SetRequireExplicitMutations(true)
		toolMgr.ApprovalMgr.IgnoreProjectApprovals = true
	}

	blocked := map[string]bool{
		tools.SpawnAgentToolName: true, tools.QueueAgentToolName: true, tools.WaitForJobsToolName: true,
		tools.InitiateHandoverToolName: true, tools.RunAgentScriptToolName: true,
		tools.HubDelegateToolName: true, tools.HubCheckDelegationToolName: true,
		"activate_skill": true,
	}
	for _, spec := range engine.Tools().AllSpecs() {
		if blocked[spec.Name] || !tools.ValidToolName(spec.Name) {
			engine.UnregisterTool(spec.Name)
		}
	}
	localTools := tools.ParseToolsFlag(settings.Tools)
	filtered := localTools[:0]
	for _, name := range localTools {
		if !blocked[name] && tools.ValidToolName(name) {
			filtered = append(filtered, name)
		}
	}
	localTools = filtered

	modelName := getModelName(cfg)
	if modelName == "" {
		modelName = extractModelFromProviderName(provider.Name())
	}
	mcpManager := mcp.NewManager() // deliberately independent and empty for side policy
	mcpManager.SetSamplingProvider(provider, modelName, false)
	agentName, platformMessage := "", ""
	if agent != nil {
		agentName = agent.Name
		platformMessage = agent.PlatformMessages.For("chat")
	}
	model := chat.NewWithFastProvider(cfg, provider, fastProvider, engine, providerKey, modelName, mcpManager, settings.MaxTurns, resolveForceExternalSearch(cfg, chatNativeSearch, chatNoNativeSearch), chatNoWebFetch, settings.Search, localTools, settings.Tools, "", false, "", store, sess, useAltScreen, nil, false, chatTextMode, agentName, platformMessage, false, toolMgr)
	model.SetRunner(newCmdRunner(cfg, cmdRunnerOptions{
		Provider: chatProvider, ConfigSet: true, ConfigProvider: cfg.Chat.Provider, ConfigModel: cfg.Chat.Model,
		ConfigInstructions: cfg.Chat.Instructions, ConfigMaxTurns: cfg.Chat.MaxTurns,
		Tools: settings.Tools, MCP: "", MaxTurns: settings.MaxTurns, DefaultMaxTurns: 200,
		Search: settings.Search, NoSearch: chatNoSearch, NativeSearch: chatNativeSearch,
		NoNativeSearch: chatNoNativeSearch, Yolo: false, Auto: false, ErrWriter: cmd.ErrOrStderr(), Store: store,
	}))
	model.SetHandoverAutoSend(autoSend)

	runtimeCtx, cancel := context.WithCancel(rootCtx)
	addressed := func(msg tea.Msg) {
		send(chat.RoutedConversationMsg{ConversationID: sess.ID, Generation: model.StreamGeneration(), Msg: msg})
	}
	if toolMgr != nil {
		approvalMgr := toolMgr.ApprovalMgr
		approvalMgr.GuardianEventFunc = func(event tools.GuardianEvent) {
			addressed(chat.GuardianReviewMsg{Event: event})
		}
		approvalMgr.PromptUIFunc = func(path string, isWrite, isShell bool, workDir string) (tools.ApprovalResult, error) {
			done := make(chan tools.ApprovalResult, 1)
			addressed(chat.ApprovalRequestMsg{Path: path, IsWrite: isWrite, IsShell: isShell, WorkDir: workDir, DoneCh: done})
			select {
			case result := <-done:
				return result, nil
			case <-runtimeCtx.Done():
				return tools.ApprovalResult{Choice: tools.ApprovalChoiceDeny}, runtimeCtx.Err()
			}
		}
		model.SetHandoverApprovalManager(approvalMgr)
	}
	runtimeCtx = tools.ContextWithAskUserUIFunc(runtimeCtx, func(ctx context.Context, questions []tools.AskUserQuestion) ([]tools.AskUserAnswer, error) {
		done := make(chan []tools.AskUserAnswer, 1)
		addressed(chat.AskUserRequestMsg{Questions: questions, DoneCh: done})
		select {
		case answers := <-done:
			if answers == nil {
				return nil, fmt.Errorf("cancelled by user")
			}
			return answers, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	runtimeCtx = tools.ContextWithHandoverFunc(runtimeCtx, func(ctx context.Context, agent string) (bool, error) {
		done := make(chan bool, 1)
		addressed(chat.HandoverRequestMsg{Agent: agent, DoneCh: done})
		select {
		case confirmed := <-done:
			return confirmed, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	})
	model.SetRootContext(runtimeCtx)
	model.SetRuntimeCancel(cancel)
	model.SetHandoverApprovalManager(func() *tools.ApprovalManager {
		if toolMgr != nil {
			return toolMgr.ApprovalMgr
		}
		return nil
	}())
	model.PersistApprovalModeActive()
	return model, nil
}

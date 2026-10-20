package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/chat"
)

func applyChatSearchDefault(settings *appserve.SessionSettings, noSearch bool, sess *session.Session, agentActive bool) {
	if settings == nil || noSearch || sess != nil || agentActive {
		return
	}
	// Bare interactive chat historically exposed web_search/read_url by default.
	// Keep that default visible in the footer and reversible with /search; agents
	// keep their explicit search setting, and --no-search is the explicit opt-out.
	settings.Search = true
}

type chatProgramInput struct {
	reader       io.Reader
	disableInput bool
	cleanup      func()
}

type chatRelaunchHandoff struct {
	reloadState     *chat.ReloadState
	branchPrefill   string
	branchPathNotes *chat.BranchPathNotesRequest
	branchAutoSend  string
}

func sessionIsConversationBranch(ctx context.Context, store session.Store, sessionID string) bool {
	branchStore, ok := store.(session.ConversationBranchStore)
	if !ok || strings.TrimSpace(sessionID) == "" {
		return false
	}
	tree, err := branchStore.GetBranchTree(ctx, sessionID)
	if err != nil {
		return false
	}
	for _, node := range tree.Nodes {
		if node.SessionID == sessionID {
			return strings.TrimSpace(node.ParentSessionID) != ""
		}
	}
	return false
}

func (r *chatInvocation) buildChatProgramInput(autoSendMode bool) (chatProgramInput, error) {
	if autoSendMode {
		return chatProgramInput{
			disableInput: true,
			cleanup:      func() {},
		}, nil
	}

	// Keep interactive chat bound to the terminal TTY so redirected stdin can
	// still provide initial content without stealing live keyboard input.
	ttyIn, ttyOut, err := r.deps.OpenTTY()
	if err != nil {
		return chatProgramInput{}, fmt.Errorf("open chat TTY: %w", err)
	}

	return chatProgramInput{
		reader: ttyIn,
		cleanup: func() {
			_ = ttyIn.Close()
			if ttyOut != nil && ttyOut != ttyIn {
				_ = ttyOut.Close()
			}
		},
	}, nil
}

func buildChatHandoverApprovalManager(cfg *config.Config, settings appserve.SessionSettings) (*tools.ApprovalManager, error) {
	toolConfig := buildToolConfig("", nil, nil, settings.ShellAllow, cfg)
	perms := tools.NewToolPermissions()
	for _, pattern := range toolConfig.ShellAllow {
		if err := perms.AddShellPattern(pattern); err != nil {
			return nil, err
		}
	}
	for _, script := range settings.Scripts {
		perms.AddScriptCommand(script)
	}
	return tools.NewApprovalManager(perms), nil
}

func toolManagerHasPathCapableTools(manager *tools.ToolManager) bool {
	if manager == nil {
		return false
	}
	for _, spec := range manager.GetSpecs() {
		if tools.IsPathCapableTool(spec.Name) {
			return true
		}
	}
	return false
}

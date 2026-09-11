package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/prompt"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

type preparedAskConversation struct {
	session          *session.Session
	sessionID        string
	settings         SessionSettings
	messages         []llm.Message
	instructions     string
	historyHasSystem bool
	startedAt        time.Time
}

func readAskPrompt(question string, paths []string) (string, error) {
	var files []input.FileContent
	if len(paths) > 0 {
		var err error
		files, err = input.ReadFiles(paths)
		if err != nil {
			return "", fmt.Errorf("failed to read files: %w", err)
		}
	}
	stdin, err := input.ReadStdin()
	if err != nil {
		return "", fmt.Errorf("failed to read stdin: %w", err)
	}
	return prompt.AskUserPrompt(question, files, stdin), nil
}

// prepareAskConversation establishes/refreshes durable session inputs and then
// projects system, active history, grounding, and the new user message in order.
func prepareAskConversation(ctx context.Context, cfg *config.Config, provider llm.Provider, agent *agents.Agent, store session.Store, sess *session.Session, sessionID string, resuming bool, settings SessionSettings, ticket *sessionInputTicket, basePrompt, userPrompt string, approval tools.ApprovalMode) (preparedAskConversation, error) {
	if !resuming && store != nil {
		if sessionID == "" {
			sessionID = session.NewID()
		}
		model := ""
		if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
			model = providerCfg.Model
		}
		if model == "" {
			model = extractModelFromProviderName(provider.Name())
		}
		agentName := ""
		if agent != nil {
			agentName = agent.Name
		}
		now := time.Now()
		sess = &session.Session{ID: sessionID, Provider: provider.Name(), Model: model, Mode: session.ModeAsk, Agent: agentName, CreatedAt: now, UpdatedAt: now, Search: settings.Search, Tools: settings.Tools, MCP: settings.MCP, ApprovalMode: approvalModeForColdPersistence(approval), Status: session.StatusActive}
		sess.CWD = settings.PrimaryWorkspace
		_ = store.Create(ctx, sess)
	}
	if ticket != nil {
		if ticket.owner {
			refresher, _ := session.AsSessionInputRefresher(store)
			refreshed, err := refresher.RefreshSessionInputs(ctx, sess.ID, settings.SystemPrompt, settings.Tools, equalSessionTools)
			if err != nil {
				return preparedAskConversation{}, err
			}
			settings.Tools = refreshed.Tools
		}
		sess.Tools = settings.Tools
	}
	if !resuming && sess != nil {
		registerOwnedSessionInputs(store, sess, sessionInputSelection{BasePrompt: basePrompt, Prompt: settings.SystemPrompt, Tools: settings.Tools})
	}
	var history []llm.Message
	if resuming {
		rows, err := session.LoadActiveMessages(ctx, store, sess)
		if err != nil && ticket != nil {
			return preparedAskConversation{}, fmt.Errorf("load refreshed session history: %w", err)
		} else if err == nil {
			for _, row := range rows {
				history = append(history, row.ToLLMMessage())
			}
		}
	}
	if ticket != nil {
		if ticket.owner {
			ticket.finish(sessionInputSelection{BasePrompt: basePrompt, Prompt: settings.SystemPrompt, Tools: settings.Tools})
		}
		history = session.ProjectSelectedSessionPrompt(history, settings.SystemPrompt)
	}
	instructions := settings.SystemPrompt
	historyHasSystem := len(history) > 0 && history[0].Role == llm.RoleSystem
	messages := make([]llm.Message, 0, len(history)+3)
	if instructions != "" && !historyHasSystem {
		messages = append(messages, llm.SystemText(instructions))
	}
	messages = append(messages, history...)
	var started time.Time
	if !resuming && settings.TimeGrounding {
		started = time.Now()
		messages = llm.InsertConversationStart(messages, []llm.Message{llm.ConversationStartMessage(started)})
	}
	messages = append(messages, llm.UserText(userPrompt))
	if sess != nil {
		sessionID = sess.ID
	}
	return preparedAskConversation{session: sess, sessionID: sessionID, settings: settings, messages: messages, instructions: instructions, historyHasSystem: historyHasSystem, startedAt: started}, nil
}

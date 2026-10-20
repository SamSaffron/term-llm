package cmd

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/chat"
)

func (r *chatInvocation) resolveChatHandoverSystemPrompt(targetAgent *agents.Agent, providerKey, modelName string, paths appmemory.Paths) (string, error) {
	cfg, err := r.deps.LoadConfig()
	if err != nil {
		return "", err
	}
	return r.resolveChatHandoverSystemPromptWithConfig(cfg, targetAgent, providerKey, modelName, paths)
}

func (r *chatInvocation) resolveChatHandoverSystemPromptWithConfig(cfg *config.Config, targetAgent *agents.Agent, providerKey, modelName string, paths appmemory.Paths) (string, error) {
	if targetAgent == nil {
		return "", nil
	}
	resolved, err := r.resolveChatRuntimeSystemContextWithConfig(cfg, targetAgent, providerKey, modelName, "", cfg.Chat.Instructions, "", paths)
	return resolved.SystemPrompt, err
}

func (r *chatInvocation) resolveChatRuntimeSystemContextWithConfig(cfg *config.Config, targetAgent *agents.Agent, providerKey, modelName, runtimeDir, rawConfigInstructions, systemMessage string, paths appmemory.Paths) (chat.RuntimeSystemContext, error) {
	if cfg == nil {
		cfg = &config.Config{}
	}

	var promptAgent *agents.Agent
	agentSkills := ""
	if targetAgent != nil {
		copyAgent := *targetAgent
		copyAgent.Provider = strings.TrimSpace(providerKey)
		copyAgent.Model = strings.TrimSpace(modelName)
		promptAgent = &copyAgent
		agentSkills = copyAgent.Skills
	}

	maxTurnsSet := r.opts.Changed.MaxTurns
	errWriter := r.deps.Stderr

	settings, err := appserve.ResolveSettingsInDir(cfg, promptAgent, appserve.CLIFlags{
		Provider:        "",
		Tools:           r.opts.Tools,
		ReadDirs:        r.opts.ReadDirs,
		WriteDirs:       r.opts.WriteDirs,
		ShellAllow:      r.opts.ShellAllow,
		MCP:             r.opts.MCP,
		SystemMessage:   systemMessage,
		MaxTurns:        r.opts.MaxTurns,
		MaxTurnsSet:     maxTurnsSet,
		Search:          r.opts.Search,
		NoSearch:        r.opts.NoSearch,
		MaxOutputTokens: 0,
		Platform:        "chat",
		MemoryPaths:     paths,
	}, providerKey, modelName, rawConfigInstructions, cfg.Chat.MaxTurns, 200, runtimeDir)
	if err != nil {
		return chat.RuntimeSystemContext{}, err
	}

	skillsSetup := appserve.SetupSkillsInDir(&cfg.Skills, r.opts.Skills, agentSkills, errWriter, runtimeDir)
	return chat.RuntimeSystemContext{
		SystemPrompt: appserve.InjectSkillsMetadata(settings.SystemPrompt, skillsSetup),
		ApplySkills:  skillContextApplier(skillsSetup),
		Skills:       skillsSetup,
	}, nil
}

func skillContextApplier(setup *skills.Setup) func(*engine.Engine, *tools.ToolManager) {
	return func(engine *engine.Engine, toolMgr *tools.ToolManager) {
		if engine == nil {
			return
		}
		engine.UnregisterTool(tools.ActivateSkillToolName)
		engine.UnregisterTool(tools.SearchSkillsToolName)
		appserve.RegisterSkillToolWithEngine(engine, toolMgr, setup)
	}
}

func printChatResumeHint(w io.Writer, store session.Store, sessionID string) {
	if store == nil || sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	refreshed, err := store.Get(ctx, sessionID)
	if err == nil && refreshed != nil && (refreshed.UserTurns > 0 || refreshed.LLMTurns > 0) {
		fmt.Fprintf(w, "\n💬 Resume: %s\n", chatResumeCommand(refreshed))
	}
}

func chatResumeCommand(sess *session.Session) string {
	resumeID := ""
	if sess != nil {
		if sess.Number > 0 {
			resumeID = strconv.FormatInt(sess.Number, 10)
		} else {
			id := strings.TrimSpace(sess.ID)
			resumeID = id
			if !session.ParseIDTime(id).IsZero() {
				resumeID = session.ShortID(id)
			}
		}
	}
	return "term-llm chat --resume=" + resumeID
}

func resolveChatResumeSession(ctx context.Context, store session.Store, resumeID string) (*session.Session, error) {
	resumeID = strings.TrimSpace(resumeID)
	if resumeID == "" {
		sess, err := store.GetCurrent(ctx)
		if err == nil && sess != nil {
			return sess, nil
		}
		summaries, listErr := store.List(ctx, session.ListOptions{Limit: 1})
		if listErr != nil {
			return nil, fmt.Errorf("failed to list sessions: %w", listErr)
		}
		if len(summaries) == 0 {
			return nil, fmt.Errorf("no session to resume")
		}
		sess, err = store.Get(ctx, summaries[0].ID)
		if err != nil {
			return nil, fmt.Errorf("failed to load session: %w", err)
		}
		if sess == nil {
			return nil, fmt.Errorf("no session to resume")
		}
		return sess, nil
	}

	sess, err := store.GetByPrefix(ctx, resumeID)
	if err != nil {
		return nil, fmt.Errorf("failed to load session: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("session '%s' not found", resumeID)
	}
	return sess, nil
}

// getModelName returns the configured model; "" means caller should fall back to extractModelFromProviderName(provider.Name()).
func getModelName(cfg *config.Config) string {
	if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
		return providerCfg.Model
	}
	return ""
}

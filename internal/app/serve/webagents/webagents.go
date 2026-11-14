package webagents

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/term-llm/term-llm/internal/agents"
	"github.com/term-llm/term-llm/internal/app/runtime/mcpstart"
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/mcp"
	"github.com/term-llm/term-llm/internal/tools"
)

// Settings is the Web UI-only agent policy resolved once at serve startup.
// ApprovalActions maps an agent name ("" is the no-agent Chat mode) to whether
// its resolved capabilities can produce approval-governed actions. An absent
// entry means unknown, and the UI must not hide approval warnings for it.
type Settings struct {
	ProjectDefaultAgent string
	PickerCommon        []string
	ApprovalActions     map[string]bool
}

// Resolve validates web.project_default_agent and resolves draft approval
// capabilities. A pinned server agent owns every new conversation, so the
// project default is not applicable and is not validated against the
// unavailable catalog.
func Resolve(cfg *config.Config, pinnedAgent string, available []string, projectsEnabled, hasWeb bool, toolsFlag string, toolsSet bool, mcpFlag string) (Settings, error) {
	settings := Settings{}
	if cfg == nil || !hasWeb {
		return settings, nil
	}
	settings.PickerCommon = append([]string{}, cfg.Web.AgentPicker.CommonAgents()...)
	if strings.TrimSpace(pinnedAgent) != "" {
		return settings, nil
	}
	projectDefault := strings.TrimSpace(cfg.Web.ProjectDefaultAgent)
	if projectsEnabled {
		if projectDefault == "" {
			return settings, fmt.Errorf("invalid web.project_default_agent %q: projects require an available default agent (set web.project_default_agent or start serve with --no-projects)", cfg.Web.ProjectDefaultAgent)
		}
		if !contains(available, projectDefault) {
			return settings, fmt.Errorf("invalid web.project_default_agent %q: not an available agent (available: %s)", projectDefault, strings.Join(available, ", "))
		}
	}
	settings.ProjectDefaultAgent = projectDefault
	settings.ApprovalActions = DraftApprovalActions(cfg, toolsFlag, toolsSet, mcpFlag)
	return settings, nil
}

// AppendGlobals injects the agent identity and draft capability map. The map is
// omitted entirely when it could not be resolved so the UI stays conservative.
func AppendGlobals(headSnippet, agentName string, agentNames []string, projectDefaultAgent string, pickerCommon []string, actions map[string]bool) string {
	inject := func(name string, value any) {
		encoded, _ := json.Marshal(value)
		headSnippet += `<script>window.` + name + `=` + string(encoded) + `;</script>`
	}
	inject("TERM_LLM_AGENT_NAME", agentName)
	inject("TERM_LLM_AGENT_NAMES", agentNames)
	inject("TERM_LLM_PROJECT_DEFAULT_AGENT", projectDefaultAgent)
	inject("TERM_LLM_AGENT_PICKER_COMMON", pickerCommon)
	if actions != nil {
		inject("TERM_LLM_AGENT_APPROVAL_ACTIONS", actions)
	}
	return headSnippet
}

// ApprovalPolicy is the session approval payload. ApprovalActions is omitted
// when the runtime cannot be inspected so the UI keeps its warnings.
func ApprovalPolicy(defaultMode string, mgr *tools.ApprovalManager, eng *engine.Engine, mcpManager *mcp.Manager) map[string]any {
	policy := map[string]any{
		"default_mode":            tools.ModePrompt.String(),
		"requested_mode":          tools.ModePrompt.String(),
		"effective_mode":          tools.ModePrompt.String(),
		"controls_available":      false,
		"guardian_available":      false,
		"guardian_auto_suspended": false,
	}
	if defaultMode == "" {
		return policy
	}
	policy["default_mode"] = defaultMode
	policy["requested_mode"] = defaultMode
	policy["effective_mode"] = defaultMode
	actions, actionsKnown := RuntimeApprovalActions(eng, mcpManager)
	if actionsKnown {
		policy["approval_actions"] = actions
	}
	if mgr == nil {
		return policy
	}
	policy["controls_available"] = !mgr.HasParent() && (!actionsKnown || actions)
	policy["requested_mode"] = mgr.RequestedApprovalMode().String()
	policy["effective_mode"] = mgr.ApprovalMode().String()
	policy["guardian_available"] = mgr.GuardianReviewerAvailable()
	policy["guardian_auto_suspended"] = mgr.GuardianAutoSuspended()
	return policy
}

// RuntimeApprovalActions reports whether a prepared runtime owns any executable
// tool that can ask for approval. The engine registry is authoritative because
// MCP tools attach independently of a tool manager. Known=false means the
// runtime could not be inspected; callers must not hide warnings then.
func RuntimeApprovalActions(eng *engine.Engine, manager *mcp.Manager) (actions bool, known bool) {
	if eng == nil || eng.Tools() == nil {
		return false, false
	}
	for _, spec := range eng.FilterAllowedToolSpecs(eng.Tools().AllSpecsIncludingDeferred()) {
		if tools.ApprovalGoverned(spec.Name) {
			return true, true
		}
	}
	if manager != nil && len(manager.EnabledServers()) > 0 {
		// A running server has not necessarily published any tool yet. Treat the
		// unresolved selection as unknown, not as an approval-governed action.
		return false, false
	}
	return false, true
}

func contains(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

// DraftApprovalActions classifies every registered agent plus the no-agent Chat
// mode under "". Registry or MCP metadata failures leave entries out rather
// than guessing.
func DraftApprovalActions(cfg *config.Config, toolsFlag string, toolsSet bool, mcpFlag string) map[string]bool {
	actions := map[string]bool{}
	registryCfg := agents.RegistryConfig{UseBuiltin: cfg.Agents.UseBuiltin, SearchPaths: cfg.Agents.SearchPaths}
	registry, err := agents.NewRegistry(registryCfg)
	if err != nil {
		return nil
	}
	list, err := registry.List()
	if err != nil {
		return nil
	}
	mcpConfig, mcpKnown := loadMCPConfig()
	vision := visionRoutingConfigured(cfg)
	for _, agent := range list {
		governed, known := draftAgentApprovalActions(cfg, agent, toolsFlag, toolsSet, mcpFlag, mcpConfig, mcpKnown)
		if known {
			actions[agent.Name] = governed || vision
		}
	}
	governed, known := draftAgentApprovalActions(cfg, nil, toolsFlag, toolsSet, mcpFlag, mcpConfig, mcpKnown)
	if known {
		actions[""] = governed || vision
	}
	return actions
}

func draftAgentApprovalActions(cfg *config.Config, agent *agents.Agent, toolsFlag string, toolsSet bool, mcpFlag string, mcpConfig *mcp.Config, mcpKnown bool) (bool, bool) {
	if tools.AnyApprovalGoverned(effectiveDraftToolNames(cfg, agent, toolsFlag, toolsSet)) {
		return true, true
	}
	if agent != nil && len(agent.Tools.Custom) > 0 {
		return true, true
	}
	selection := strings.TrimSpace(mcpFlag)
	if selection == "" && agent != nil {
		selection = strings.Join(agent.GetMCPServerNames(), ",")
	}
	if selection == "" {
		return false, true
	}
	if !mcpKnown {
		return false, false
	}
	active, known := draftMCPSelectionActive(mcpConfig, agent, selection)
	return active, known
}

// draftMCPSelectionActive applies the configured access policy to an agent's
// MCP selection. It never starts a server; a server that is configured but not
// permitted for this agent does not count as an action source.
func draftMCPSelectionActive(cfg *mcp.Config, agent *agents.Agent, selection string) (bool, bool) {
	if cfg == nil {
		return false, true
	}
	manager := mcp.NewManagerWithConfig(cfg, mcp.WithPolicyIdentity(agents.PolicyIdentity(agent)))
	if err := manager.RefreshPolicy(); err != nil {
		return false, false
	}
	cleaned, err := mcpstart.CleanSelection(manager, selection, false, io.Discard)
	if err != nil {
		return false, false
	}
	if strings.TrimSpace(cleaned) != "" {
		// Selection alone cannot establish whether the server publishes any
		// tools. Discovery may require launching a process; leave the draft
		// unknown until the prepared runtime can inspect its effective tools.
		return false, false
	}
	return false, true
}

func loadMCPConfig() (*mcp.Config, bool) {
	cfg, err := mcp.LoadConfig()
	if err != nil {
		return nil, false
	}
	return cfg, true
}

// effectiveDraftToolNames mirrors the tool precedence used when a run starts:
// an explicit serve/CLI selection, then the agent's allow/deny list, then the
// global config selection. It intentionally does not resolve prompts.
func effectiveDraftToolNames(cfg *config.Config, agent *agents.Agent, toolsFlag string, toolsSet bool) []string {
	if toolsSet || strings.TrimSpace(toolsFlag) != "" {
		return tools.ParseToolsFlag(toolsFlag)
	}
	if agent != nil {
		if agent.HasEnabledList() {
			return expandToolSelection(agent.Tools.Enabled)
		}
		if agent.HasDisabledList() {
			return agent.GetEnabledTools(tools.StandardToolNames(), tools.LegacyDisabledName)
		}
	}
	if cfg == nil {
		return nil
	}
	return expandToolSelection(cfg.Tools.Enabled)
}

func expandToolSelection(names []string) []string {
	for _, name := range names {
		if name == "all" || name == "*" {
			return tools.StandardToolNames()
		}
	}
	return append([]string(nil), names...)
}

// visionRoutingConfigured reports whether any provider or model routes image
// understanding through another model. That path registers view_image even for
// a tool-less agent, and view_image can request path approval. The draft UI
// cannot know the eventual provider/model, so any configured route is treated
// as a potential approval source.
func visionRoutingConfigured(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	for _, provider := range cfg.Providers {
		if strings.TrimSpace(provider.VisionVia) != "" {
			return true
		}
		for _, model := range provider.ModelConfigs {
			if strings.TrimSpace(model.VisionVia) != "" {
				return true
			}
		}
	}
	return false
}

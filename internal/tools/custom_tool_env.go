package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

// customToolEnvironment builds a script's environment: the inherited process
// environment and per-tool env with host-reserved session context removed,
// then that context set only from the host-admitted run.
func customToolEnvironment(ctx context.Context, inherited []string, agentDir string, def agents.CustomToolDef) []string {
	env := make([]string, 0, len(inherited)+len(def.Env)+4)
	for _, entry := range inherited {
		name, _, _ := strings.Cut(entry, "=")
		if !agents.IsReservedCustomToolEnv(name) {
			env = append(env, entry)
		}
	}
	env = append(env, fmt.Sprintf("TERM_LLM_AGENT_DIR=%s", agentDir))
	env = append(env, fmt.Sprintf("TERM_LLM_TOOL_NAME=%s", def.Name))
	for k, v := range def.Env {
		// Validation rejects reserved names; this also covers definitions
		// constructed without validation.
		if !agents.IsReservedCustomToolEnv(k) {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
	}
	if id := core.LockedSessionFromContext(ctx); id != "" {
		env = append(env, agents.EnvSessionID+"="+id, agents.EnvPrivateSession+"=1")
	}
	return env
}

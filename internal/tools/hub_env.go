package tools

import (
	"context"
	"os"
	"strings"
	"sync"
)

// Hub delegation credentials live in process memory, NOT in the process
// environment: every external subprocess this process spawns (shell tool,
// custom tools, widgets, MCP servers, program jobs) inherits os.Environ(), so
// a hub token in env would leak to model-controlled commands. serve hands the
// config over in-process with ConfigureHubDelegation; standalone processes
// may export TERM_LLM_HUB_* instead, which init() captures and then scrubs
// the token from the environment before any tool can spawn a subprocess.

const (
	hubEnvURL    = "TERM_LLM_HUB_URL"
	hubEnvNodeID = "TERM_LLM_HUB_NODE_ID"
	hubEnvToken  = "TERM_LLM_HUB_TOKEN"
)

var hubDelegationCfg struct {
	sync.Mutex
	url, nodeID, token string
	// tokenFromEnv records that the token arrived via the environment, so a
	// reload re-exec of this same binary can hand it to the next generation
	// (HubDelegationEnviron); a flag-provided token is re-derived from flags.
	tokenFromEnv bool
}

func init() { captureHubDelegationEnv() }

// captureHubDelegationEnv fills hub delegation config gaps from TERM_LLM_HUB_*
// and removes the token from the process environment so subprocesses never
// inherit it. Runs at process start; safe to call again (tests).
func captureHubDelegationEnv() {
	hubDelegationCfg.Lock()
	defer hubDelegationCfg.Unlock()
	if v := strings.TrimSpace(os.Getenv(hubEnvURL)); v != "" && hubDelegationCfg.url == "" {
		hubDelegationCfg.url = v
	}
	if v := strings.TrimSpace(os.Getenv(hubEnvNodeID)); v != "" && hubDelegationCfg.nodeID == "" {
		hubDelegationCfg.nodeID = v
	}
	if v := strings.TrimSpace(os.Getenv(hubEnvToken)); v != "" {
		if hubDelegationCfg.token == "" {
			hubDelegationCfg.token = v
			hubDelegationCfg.tokenFromEnv = true
		}
		_ = os.Unsetenv(hubEnvToken)
	}
}

// ConfigureHubDelegation fills hub delegation config gaps in-process (explicit
// TERM_LLM_HUB_* values captured at startup win, matching the old env
// precedence) without ever writing the token into the process environment.
func ConfigureHubDelegation(url, nodeID, token string) {
	hubDelegationCfg.Lock()
	defer hubDelegationCfg.Unlock()
	if v := strings.TrimSpace(url); v != "" && hubDelegationCfg.url == "" {
		hubDelegationCfg.url = v
	}
	if v := strings.TrimSpace(nodeID); v != "" && hubDelegationCfg.nodeID == "" {
		hubDelegationCfg.nodeID = v
	}
	if v := strings.TrimSpace(token); v != "" && hubDelegationCfg.token == "" {
		hubDelegationCfg.token = v
	}
}

// hubDelegationConfig returns the current in-process hub delegation config.
func hubDelegationConfig() (url, nodeID, token string) {
	hubDelegationCfg.Lock()
	defer hubDelegationCfg.Unlock()
	return hubDelegationCfg.url, hubDelegationCfg.nodeID, hubDelegationCfg.token
}

// HubDelegationEnviron returns the TERM_LLM_HUB_* entries a reload re-exec of
// this SAME binary needs to keep hub delegation working when the token
// originally arrived via the environment (init scrubbed it, so os.Environ()
// no longer carries it). It returns nil when the token came from flags or
// in-process config. Never pass the result to any other subprocess.
func HubDelegationEnviron() []string {
	hubDelegationCfg.Lock()
	defer hubDelegationCfg.Unlock()
	if !hubDelegationCfg.tokenFromEnv || hubDelegationCfg.token == "" {
		return nil
	}
	env := []string{hubEnvToken + "=" + hubDelegationCfg.token}
	if hubDelegationCfg.url != "" {
		env = append(env, hubEnvURL+"="+hubDelegationCfg.url)
	}
	if hubDelegationCfg.nodeID != "" {
		env = append(env, hubEnvNodeID+"="+hubDelegationCfg.nodeID)
	}
	return env
}

// resetHubDelegationForTest clears the in-process config and returns a
// restore function.
func resetHubDelegationForTest() func() {
	hubDelegationCfg.Lock()
	prevURL, prevNode, prevToken, prevFromEnv := hubDelegationCfg.url, hubDelegationCfg.nodeID, hubDelegationCfg.token, hubDelegationCfg.tokenFromEnv
	hubDelegationCfg.url, hubDelegationCfg.nodeID, hubDelegationCfg.token, hubDelegationCfg.tokenFromEnv = "", "", "", false
	hubDelegationCfg.Unlock()
	return func() {
		hubDelegationCfg.Lock()
		hubDelegationCfg.url, hubDelegationCfg.nodeID, hubDelegationCfg.token, hubDelegationCfg.tokenFromEnv = prevURL, prevNode, prevToken, prevFromEnv
		hubDelegationCfg.Unlock()
	}
}

// hubDelegationCtxKey carries the id of the hub delegation a tool execution
// belongs to. The jobs-v2 runner sets it from the job's hub_delegation label,
// so chaining is anchored in hub-written provenance rather than in a
// model-controlled argument.
type hubDelegationCtxKey struct{}

// WithHubDelegationID marks ctx as executing inside the given hub delegation.
func WithHubDelegationID(ctx context.Context, id string) context.Context {
	id = strings.TrimSpace(id)
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, hubDelegationCtxKey{}, id)
}

// HubDelegationIDFromContext returns the delegation id ctx executes under, or
// "" when this execution is not part of a hub delegation.
func HubDelegationIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(hubDelegationCtxKey{}).(string)
	return id
}

package hub

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// DefaultDelegationMaxInFlight bounds concurrently active delegations per
// target node when the node's policy does not set max_in_flight.
const DefaultDelegationMaxInFlight = 4

// DefaultDelegationMaxDepth bounds delegation chains (A -> B -> C is depth 3)
// so two mutually-delegating nodes cannot recurse forever.
const DefaultDelegationMaxDepth = 3

// DefaultDelegationAgent is the agent delegated jobs run as when the request
// names none; with no allowed_agents policy it is also the ONLY agent a
// target accepts.
const DefaultDelegationAgent = "developer"

// DelegationPolicy is a node's cross-node delegation policy, configured per
// node in the hub config:
//
//	nodes:
//	  - name: jarvis
//	    url: http://127.0.0.1:8081/chat
//	    token: secret
//	    delegation:
//	      to: ["*"]           # node ids this node may delegate to
//	      accept_from: ["*"]  # node ids this node accepts delegations from
//	      workdir: /work      # REQUIRED to accept; cwd for delegated jobs
//	      max_in_flight: 4    # concurrently active delegations targeting this node
//	      allowed_agents: []  # agents origins may request (default: developer only)
//	      allowed_models: []  # model overrides origins may request (default: none)
//
// Defaults are off: a node must set delegation.enabled to true before it can
// originate or accept delegated work. Accepting still also requires a workdir.
type DelegationPolicy struct {
	Enabled     bool     `json:"enabled,omitempty" yaml:"enabled"`
	To          []string `json:"to,omitempty" yaml:"to"`
	AcceptFrom  []string `json:"accept_from,omitempty" yaml:"accept_from"`
	Workdir     string   `json:"workdir,omitempty" yaml:"workdir"`
	MaxInFlight int      `json:"max_in_flight,omitempty" yaml:"max_in_flight"`
	// AllowedAgents lists agent names origins may request on this node.
	// Empty means only DefaultDelegationAgent; "*" allows any plain agent
	// name, but path-like names (containing a separator or "..") always
	// require an exact entry.
	AllowedAgents []string `json:"allowed_agents,omitempty" yaml:"allowed_agents"`
	// AllowedModels lists model overrides origins may request. Empty means no
	// override is accepted (the target's own default is used); "*" allows any.
	AllowedModels []string `json:"allowed_models,omitempty" yaml:"allowed_models"`
}

// matchNodeList reports whether id matches the pattern list: "*" matches any
// node, anything else is an exact node id match.
func matchNodeList(patterns []string, id string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "*" || p == id {
			return true
		}
	}
	return false
}

// CanDelegateTo reports whether this node's policy allows originating a
// delegation to the target node. Delegation is default-off; once enabled, an
// empty "to" list means any target (the target still has to accept).
func (n Node) CanDelegateTo(targetID string) bool {
	if n.Delegation == nil || !n.Delegation.Enabled {
		return false
	}
	if len(n.Delegation.To) == 0 {
		return true
	}
	return matchNodeList(n.Delegation.To, targetID)
}

// AcceptsDelegationFrom returns nil when this node accepts delegated work
// from the origin node, or an error describing why not. Delegation is
// default-off; accepting additionally requires an explicit delegation workdir.
func (n Node) AcceptsDelegationFrom(originID string) error {
	if n.Delegation == nil || !n.Delegation.Enabled {
		return fmt.Errorf("node %q does not accept delegations (delegation is not enabled)", n.ID)
	}
	if strings.TrimSpace(n.Delegation.Workdir) == "" {
		return fmt.Errorf("node %q does not accept delegations (no delegation workdir configured)", n.ID)
	}
	acceptFrom := n.Delegation.AcceptFrom
	if len(acceptFrom) == 0 {
		acceptFrom = []string{"*"}
	}
	if !matchNodeList(acceptFrom, originID) {
		return fmt.Errorf("node %q does not accept delegations from node %q", n.ID, originID)
	}
	return nil
}

// DelegationWorkdir returns the node's configured delegation workdir ("" when
// delegation is disabled or the node does not accept delegations).
func (n Node) DelegationWorkdir() string {
	if n.Delegation == nil || !n.Delegation.Enabled {
		return ""
	}
	return strings.TrimSpace(n.Delegation.Workdir)
}

// DelegationMaxInFlight returns the per-target concurrency cap for this node.
func (n Node) DelegationMaxInFlight() int {
	if n.Delegation == nil || n.Delegation.MaxInFlight <= 0 {
		return DefaultDelegationMaxInFlight
	}
	return n.Delegation.MaxInFlight
}

// pathLikeAgentName reports whether an agent name looks like a filesystem
// path. Agent names can resolve to files on the target node, so a remote
// origin must never smuggle one in under a "*" wildcard.
func pathLikeAgentName(name string) bool {
	return strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~")
}

// AcceptsDelegationAgent returns nil when this node's policy allows delegated
// work to run as the given agent. With no allowed_agents configured only
// DefaultDelegationAgent is accepted; "*" accepts any plain name; path-like
// names always need an exact entry.
func (n Node) AcceptsDelegationAgent(agent string) error {
	var allowed []string
	if n.Delegation != nil {
		allowed = n.Delegation.AllowedAgents
	}
	if len(allowed) == 0 {
		if agent == DefaultDelegationAgent {
			return nil
		}
		return fmt.Errorf("node %q only accepts the default delegation agent %q (set delegation.allowed_agents to permit others)", n.ID, DefaultDelegationAgent)
	}
	for _, p := range allowed {
		p = strings.TrimSpace(p)
		if p == agent {
			return nil
		}
		if p == "*" && !pathLikeAgentName(agent) {
			return nil
		}
	}
	return fmt.Errorf("node %q does not accept delegations running agent %q", n.ID, agent)
}

// AcceptsDelegationModel returns nil when this node's policy allows the given
// model override. An empty model (use the target's default) is always
// allowed; with no allowed_models configured every override is refused.
func (n Node) AcceptsDelegationModel(model string) error {
	if model == "" {
		return nil
	}
	if n.Delegation == nil || len(n.Delegation.AllowedModels) == 0 {
		return fmt.Errorf("node %q does not accept a model override (set delegation.allowed_models to permit one)", n.ID)
	}
	if !matchNodeList(n.Delegation.AllowedModels, model) {
		return fmt.Errorf("node %q does not accept delegations using model %q", n.ID, model)
	}
	return nil
}

// Delegation statuses. Pending/running/cancel_requested are active; the rest
// are terminal. They deliberately mirror jobs-v2 run statuses since a
// delegation is fronted by one jobs-v2 run on the target node, with "error"
// added for delegations that broke outside the target run itself.
const (
	DelegationStatusPending         = "pending"
	DelegationStatusRunning         = "running"
	DelegationStatusCancelRequested = "cancel_requested"
	DelegationStatusSucceeded       = "succeeded"
	DelegationStatusFailed          = "failed"
	DelegationStatusCancelled       = "cancelled"
	DelegationStatusTimedOut        = "timed_out"
	DelegationStatusError           = "error"
)

// DelegationStatusTerminal reports whether a delegation status is final.
func DelegationStatusTerminal(status string) bool {
	switch status {
	case DelegationStatusSucceeded, DelegationStatusFailed, DelegationStatusCancelled,
		DelegationStatusTimedOut, DelegationStatusError:
		return true
	default:
		return false
	}
}

// Delegation is one hub-mediated cross-node delegation: origin node asked the
// hub to run a prompt as a jobs-v2 LLM job on the target node. The record
// deliberately holds NO tokens — node credentials live only in node sources —
// so the ledger and every API view built from it are safe to serve.
type Delegation struct {
	ID         string `json:"id"`
	OriginNode string `json:"origin_node"`
	TargetNode string `json:"target_node"`
	AgentName  string `json:"agent_name,omitempty"`
	// Prompt is stored truncated (see delegationPromptLimit) for audit; the
	// full prompt only travels to the target job's instructions.
	Prompt string `json:"prompt,omitempty"`
	Model  string `json:"model,omitempty"`
	Cwd    string `json:"cwd,omitempty"`
	// JobID/RunID identify the jobs-v2 job and run on the TARGET node.
	JobID  string `json:"job_id,omitempty"`
	RunID  string `json:"run_id,omitempty"`
	Status string `json:"status"`
	// Depth is 1 for a direct delegation and parent depth+1 for delegations
	// created with parent_delegation_id. Chain lists the node ids involved
	// from the root origin through this delegation's target, used to refuse
	// delegation loops.
	Depth    int      `json:"depth"`
	Chain    []string `json:"chain,omitempty"`
	ParentID string   `json:"parent_delegation_id,omitempty"`
	// Response holds the target run's final response (truncated) once the
	// delegation reaches a terminal status.
	Response  string    `json:"response,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NewDelegationID returns a fresh delegation id ("dlg_" + 16 hex chars).
func NewDelegationID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate delegation id: %w", err)
	}
	return "dlg_" + hex.EncodeToString(buf[:]), nil
}

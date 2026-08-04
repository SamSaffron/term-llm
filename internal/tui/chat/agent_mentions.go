package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/mentions"
)

// AgentMentionCapability is the narrow, read-only runtime authority injected
// by command wiring. Implementations must compose the actual registered
// spawn_agent tool with the Engine's current allowed-tools filter.
type AgentMentionCapability interface {
	PermittedAgentNames() ([]string, error)
	ValidateAgentMention(name string) error
}

// SetAgentMentionCapability installs the current session's live delegation
// capability. It is intentionally separate from /handover discovery and the
// generic child runner used by isolated skills.
func (m *Model) SetAgentMentionCapability(capability AgentMentionCapability) {
	if m != nil {
		m.agentMentionCapability = capability
	}
}

func (m *Model) agentMentionDelegationContext(content string) (string, error) {
	parsed, err := mentions.ParseSubmittedAgents(content)
	if err != nil {
		return "", err
	}
	names := mentions.UniqueAgentMentionNames(parsed)
	if len(names) == 0 {
		return "", nil
	}
	if m == nil || m.agentMentionCapability == nil {
		return "", errors.New("agent mentions are unavailable because spawn_agent is not enabled in this session")
	}
	for _, name := range names {
		if err := m.agentMentionCapability.ValidateAgentMention(name); err != nil {
			return "", fmt.Errorf("cannot submit %s: %w", mentions.InsertAgentText(name), err)
		}
	}

	var context strings.Builder
	context.WriteString("\n\n<term_llm_agent_mentions>\n")
	context.WriteString("The user explicitly requested delegation through the spawn_agent tool.\n")
	context.WriteString("Call spawn_agent exactly once for each listed agent, using the user's visible request and relevant context to construct a focused prompt for that agent. Do not treat these mentions as an active-agent switch.\n")
	context.WriteString("Agents:\n")
	for _, name := range names {
		encodedName, _ := json.Marshal(name)
		context.WriteString("- ")
		context.Write(encodedName)
		context.WriteByte('\n')
	}
	context.WriteString("</term_llm_agent_mentions>")
	return context.String(), nil
}

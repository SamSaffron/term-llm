package core

import "strings"

// TurnMetrics contains metrics collected during a turn.
type TurnMetrics struct {
	InputTokens       int // Non-cached, non-cache-write input tokens this turn
	OutputTokens      int // Tokens generated as output this turn
	CachedInputTokens int // Input tokens served from cache (cache read) this turn
	CacheWriteTokens  int // Input tokens written to cache (cache creation) this turn
	ToolCalls         int // Number of tools executed this turn
}

type SteeringStatus string

const (
	SteeringQueued    SteeringStatus = "queued"
	SteeringCommitted SteeringStatus = "committed"
)

// Continuation is an engine execution boundary, not a new user request. Messages
// contain completed history; Pending contains only calls not yet dispatched.
// Tool results in Messages are never fed back through the tool executor.
type Continuation struct {
	PendingMetrics   TurnMetrics
	BaseMessageCount int
	DiscardPartial   bool
	Request          Request
	Turn             int
	Pending          []ToolCall
	ProviderState    []byte
	Interrupted      bool
}

// CommittedText is the completed assistant prefix of this invocation, excluding
// history from earlier user turns and any discarded partial provider attempt.
func (c *Continuation) CommittedText() string {
	if c == nil {
		return ""
	}
	var text strings.Builder
	for _, message := range c.Request.Messages[min(max(c.BaseMessageCount, 0), len(c.Request.Messages)):] {
		if message.Role == RoleAssistant {
			text.WriteString(MessageText(message))
		}
	}
	return text.String()
}

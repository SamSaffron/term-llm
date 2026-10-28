package serve

import (
	"bytes"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// serveRunResult is returned and retained by value. Its text must not use a
// strings.Builder, whose hidden self-pointer would dangle after a value copy.
// Consumers treat published text buffers as read-only.
type serveRunResult struct {
	Text         bytes.Buffer
	ToolCalls    []core.ToolCall
	Usage        core.Usage
	SessionUsage core.Usage
	ContextUsage *serveContextUsage
	// clientCalls tracks passthrough calls the final provider turn left for
	// the caller to answer; see PendingClientCalls.
	clientCalls pendingClientCallTracker
}

// PendingClientCalls returns the client tool calls this run stopped on.
func (r *serveRunResult) PendingClientCalls() []core.ToolCall {
	return r.clientCalls.pending()
}

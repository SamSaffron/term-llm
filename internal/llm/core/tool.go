package core

import (
	"context"
	"encoding/json"
)

// Tool describes a callable external tool.
type Tool interface {
	Spec() ToolSpec
	Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error)
	// Preview returns a human-readable description of what the tool will do,
	// shown to the user before execution starts (e.g., "Generating image: a cat").
	// Returns empty string if no preview is available.
	Preview(args json.RawMessage) string
}

// FinishingTool is an optional interface for tools that signal agent completion.
// When a finishing tool is executed, the agentic loop should stop after this turn.
// Example: output capture tools like set_commit_message.
type FinishingTool interface {
	IsFinishingTool() bool
}

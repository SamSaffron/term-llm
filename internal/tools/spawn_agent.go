package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// SpawnAgentArgs are the arguments for the spawn_agent tool.
type SpawnAgentArgs struct {
	AgentName string `json:"agent_name"`        // Required: name of the agent to spawn
	Prompt    string `json:"prompt"`            // Required: task/prompt for the sub-agent
	Timeout   int    `json:"timeout,omitempty"` // Optional: timeout in seconds (default 300)
}

// SpawnAgentResult is the result returned by spawn_agent.
type SpawnAgentResult struct {
	AgentName string `json:"agent_name"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	Type      string `json:"type,omitempty"` // Error type for structured handling
	Duration  int64  `json:"duration_ms,omitempty"`
}

// SpawnAgentRunner is the interface for running sub-agents.
// This is set by the cmd package to avoid circular imports.
type SpawnAgentRunner interface {
	// RunAgent runs a sub-agent and returns its text output.
	// ctx is used for cancellation, agentName is the agent to load,
	// prompt is the task, and depth is the current nesting level.
	RunAgent(ctx context.Context, agentName string, prompt string, depth int) (string, error)
}

// SpawnConfig configures spawn_agent behavior.
type SpawnConfig struct {
	MaxParallel    int      // Max concurrent sub-agents (default 3)
	MaxDepth       int      // Max nesting level (default 2)
	DefaultTimeout int      // Default timeout in seconds (default 300)
	AllowedAgents  []string // Optional whitelist of allowed agents
}

// DefaultSpawnConfig returns the default spawn configuration.
func DefaultSpawnConfig() SpawnConfig {
	return SpawnConfig{
		MaxParallel:    3,
		MaxDepth:       2,
		DefaultTimeout: 300,
	}
}

// SpawnAgentTool implements the spawn_agent tool.
type SpawnAgentTool struct {
	runner    SpawnAgentRunner
	config    SpawnConfig
	semaphore chan struct{} // Limits concurrent agents
	depth     int           // Current nesting depth
	mu        sync.Mutex    // Protects runner updates
}

// NewSpawnAgentTool creates a new spawn_agent tool.
func NewSpawnAgentTool(config SpawnConfig, depth int) *SpawnAgentTool {
	if config.MaxParallel <= 0 {
		config.MaxParallel = 3
	}
	if config.MaxDepth <= 0 {
		config.MaxDepth = 2
	}
	if config.DefaultTimeout <= 0 {
		config.DefaultTimeout = 300
	}

	return &SpawnAgentTool{
		config:    config,
		semaphore: make(chan struct{}, config.MaxParallel),
		depth:     depth,
	}
}

// SetRunner sets the runner for executing sub-agents.
// This must be called before Execute can succeed.
func (t *SpawnAgentTool) SetRunner(runner SpawnAgentRunner) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.runner = runner
}

// SetDepth sets the current nesting depth for this tool.
// Used when creating tools for sub-agents to track depth.
func (t *SpawnAgentTool) SetDepth(depth int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.depth = depth
}

// Spec returns the tool specification.
func (t *SpawnAgentTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: SpawnAgentToolName,
		Description: `Spawn a sub-agent to handle a specific task autonomously. Use this to delegate work to specialized agents that can run in parallel.

Guidelines:
- Spawn multiple agents concurrently for independent analysis tasks
- Each agent runs with its own context and tools
- Results are returned when the agent completes
- Use descriptive prompts that give the agent clear objectives`,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_name": map[string]any{
					"type":        "string",
					"description": "Name of the agent to spawn (e.g., 'reviewer', 'researcher')",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "The task or prompt for the sub-agent to execute",
				},
				"timeout": map[string]any{
					"type":        "integer",
					"description": "Optional timeout in seconds (default 300, max 600)",
					"minimum":     10,
					"maximum":     600,
				},
			},
			"required":             []string{"agent_name", "prompt"},
			"additionalProperties": false,
		},
	}
}

// Execute runs the spawn_agent tool.
func (t *SpawnAgentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a SpawnAgentArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return t.formatError(ErrInvalidParams, fmt.Sprintf("failed to parse arguments: %v", err)), nil
	}

	// Validate arguments
	if a.AgentName == "" {
		return t.formatError(ErrInvalidParams, "agent_name is required"), nil
	}
	if a.Prompt == "" {
		return t.formatError(ErrInvalidParams, "prompt is required"), nil
	}

	// Check depth limit
	if t.depth >= t.config.MaxDepth {
		return t.formatError(ErrPermissionDenied, fmt.Sprintf("max agent depth exceeded (current: %d, max: %d)", t.depth, t.config.MaxDepth)), nil
	}

	// Check allowed agents whitelist
	if len(t.config.AllowedAgents) > 0 {
		allowed := false
		for _, name := range t.config.AllowedAgents {
			if name == a.AgentName {
				allowed = true
				break
			}
		}
		if !allowed {
			return t.formatError(ErrPermissionDenied, fmt.Sprintf("agent '%s' is not in the allowed list", a.AgentName)), nil
		}
	}

	// Get runner under lock
	t.mu.Lock()
	runner := t.runner
	t.mu.Unlock()

	if runner == nil {
		return t.formatError(ErrExecutionFailed, "spawn_agent runner not configured"), nil
	}

	// Determine timeout
	timeout := t.config.DefaultTimeout
	if a.Timeout > 0 {
		timeout = a.Timeout
		if timeout > 600 {
			timeout = 600 // Cap at 10 minutes
		}
	}

	// Acquire semaphore (blocks if at max concurrency)
	select {
	case t.semaphore <- struct{}{}:
		defer func() { <-t.semaphore }()
	case <-ctx.Done():
		return t.formatError(ErrTimeout, "context cancelled while waiting for agent slot"), nil
	}

	// Create child context with timeout
	childCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	// Run the sub-agent
	start := time.Now()
	output, err := runner.RunAgent(childCtx, a.AgentName, a.Prompt, t.depth+1)
	duration := time.Since(start).Milliseconds()

	if err != nil {
		// Check for specific error types
		if ctx.Err() == context.DeadlineExceeded || childCtx.Err() == context.DeadlineExceeded {
			return t.formatErrorWithDuration(ErrTimeout, fmt.Sprintf("agent '%s' timed out after %d seconds", a.AgentName, timeout), duration), nil
		}
		if ctx.Err() == context.Canceled || childCtx.Err() == context.Canceled {
			return t.formatErrorWithDuration(ErrExecutionFailed, "agent execution cancelled", duration), nil
		}
		return t.formatErrorWithDuration(ErrExecutionFailed, fmt.Sprintf("agent execution failed: %v", err), duration), nil
	}

	// Return success result
	result := SpawnAgentResult{
		AgentName: a.AgentName,
		Output:    output,
		Duration:  duration,
	}
	data, _ := json.Marshal(result)
	return string(data), nil
}

// Preview returns a short description of the tool call.
func (t *SpawnAgentTool) Preview(args json.RawMessage) string {
	var a SpawnAgentArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return ""
	}
	if a.AgentName == "" {
		return ""
	}
	// Truncate prompt for preview
	prompt := a.Prompt
	if len(prompt) > 50 {
		prompt = prompt[:47] + "..."
	}
	return fmt.Sprintf("@%s: %s", a.AgentName, prompt)
}

// formatError formats an error result.
func (t *SpawnAgentTool) formatError(errType ToolErrorType, message string) string {
	result := SpawnAgentResult{
		Error: message,
		Type:  string(errType),
	}
	data, _ := json.Marshal(result)
	return string(data)
}

// formatErrorWithDuration formats an error result with duration.
func (t *SpawnAgentTool) formatErrorWithDuration(errType ToolErrorType, message string, durationMs int64) string {
	result := SpawnAgentResult{
		Error:    message,
		Type:     string(errType),
		Duration: durationMs,
	}
	data, _ := json.Marshal(result)
	return string(data)
}

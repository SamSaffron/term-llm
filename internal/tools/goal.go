package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

const (
	CreateGoalToolName = "create_goal"
	UpdateGoalToolName = "update_goal"
	GetGoalToolName    = "get_goal"
)

type CreateGoalArgs struct {
	Objective   string `json:"objective"`
	TokenBudget int    `json:"token_budget,omitempty"`
}

type UpdateGoalArgs struct {
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Message  string `json:"message,omitempty"`
}

type goalTool struct {
	name   string
	getter func() *session.Goal
}

// NewCreateGoalTool creates the model-facing tool for starting a persisted goal.
func NewCreateGoalTool() llm.Tool {
	return &goalTool{name: CreateGoalToolName}
}

// NewUpdateGoalTool creates the model-facing tool for completing or blocking a goal.
func NewUpdateGoalTool() llm.Tool {
	return &goalTool{name: UpdateGoalToolName}
}

// NewGetGoalTool creates the model-facing tool for reading the active goal.
func NewGetGoalTool(getter func() *session.Goal) llm.Tool {
	return &goalTool{name: GetGoalToolName, getter: getter}
}

func (t *goalTool) Spec() llm.ToolSpec {
	switch t.name {
	case CreateGoalToolName:
		return llm.ToolSpec{
			Name:        CreateGoalToolName,
			Description: "Create or replace the persistent objective for this session. Use only when the user asks to start a goal.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"objective": map[string]any{
						"type":        "string",
						"description": "The full user-requested objective to pursue across turns.",
					},
					"token_budget": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"description": "Optional total token budget for pursuing the goal.",
					},
				},
				"required": []string{"objective"},
			},
		}
	case UpdateGoalToolName:
		return llm.ToolSpec{
			Name:        UpdateGoalToolName,
			Description: "Mark the persistent session goal complete or blocked after satisfying the required audit. Do not call for ordinary progress updates.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status": map[string]any{
						"type":        "string",
						"enum":        []string{string(session.GoalStatusComplete), string(session.GoalStatusBlocked)},
						"description": "The terminal disposition for the active goal.",
					},
					"reason": map[string]any{
						"type":        "string",
						"description": "Short explanation for why the goal is complete or blocked.",
					},
					"evidence": map[string]any{
						"type":        "string",
						"description": "Concrete evidence supporting the completion or blocked audit.",
					},
					"message": map[string]any{
						"type":        "string",
						"description": "Optional user-facing final note.",
					},
				},
				"required": []string{"status"},
			},
		}
	default:
		return llm.ToolSpec{
			Name:        GetGoalToolName,
			Description: "Get the persistent objective and budget state for this session.",
			Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		}
	}
}

func (t *goalTool) Execute(_ context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	switch t.name {
	case CreateGoalToolName:
		parsed, err := ParseCreateGoalArgs(args)
		if err != nil {
			return llm.ToolOutput{}, err
		}
		msg := "goal created"
		if parsed.TokenBudget > 0 {
			msg = fmt.Sprintf("goal created with token budget %d", parsed.TokenBudget)
		}
		return llm.TextOutput(msg), nil
	case UpdateGoalToolName:
		parsed, err := ParseUpdateGoalArgs(args)
		if err != nil {
			return llm.ToolOutput{}, err
		}
		return llm.TextOutput(fmt.Sprintf("goal marked %s", parsed.Status)), nil
	default:
		goal := (*session.Goal)(nil)
		if t.getter != nil {
			goal = t.getter()
		}
		if goal == nil || !goal.Exists() {
			return llm.TextOutput(`{"status":"none"}`), nil
		}
		data, err := json.Marshal(goal)
		if err != nil {
			return llm.ToolOutput{}, NewToolErrorf(ErrExecutionFailed, "serialize goal: %v", err)
		}
		return llm.TextOutput(string(data)), nil
	}
}

func (t *goalTool) Preview(args json.RawMessage) string {
	switch t.name {
	case CreateGoalToolName:
		parsed, err := ParseCreateGoalArgs(args)
		if err != nil || strings.TrimSpace(parsed.Objective) == "" {
			return "(create goal)"
		}
		return "(create goal: " + previewGoalText(parsed.Objective) + ")"
	case UpdateGoalToolName:
		parsed, err := ParseUpdateGoalArgs(args)
		if err != nil || parsed.Status == "" {
			return "(update goal)"
		}
		return "(goal " + parsed.Status + ")"
	default:
		return "(get goal)"
	}
}

// ParseCreateGoalArgs validates create_goal arguments for both tool execution
// and runner commit tracking.
func ParseCreateGoalArgs(args json.RawMessage) (CreateGoalArgs, error) {
	var parsed CreateGoalArgs
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(args, &parsed); err != nil {
		return CreateGoalArgs{}, NewToolErrorf(ErrInvalidParams, "parse goal args: %v", err)
	}
	parsed.Objective = strings.TrimSpace(parsed.Objective)
	if parsed.Objective == "" {
		return CreateGoalArgs{}, NewToolError(ErrInvalidParams, "objective is required")
	}
	if parsed.TokenBudget < 0 {
		return CreateGoalArgs{}, NewToolError(ErrInvalidParams, "token_budget must be non-negative")
	}
	return parsed, nil
}

// ParseUpdateGoalArgs validates update_goal arguments for both tool execution
// and runner commit tracking.
func ParseUpdateGoalArgs(args json.RawMessage) (UpdateGoalArgs, error) {
	var parsed UpdateGoalArgs
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(args, &parsed); err != nil {
		return UpdateGoalArgs{}, NewToolErrorf(ErrInvalidParams, "parse goal update args: %v", err)
	}
	parsed.Status = strings.ToLower(strings.TrimSpace(parsed.Status))
	parsed.Reason = strings.TrimSpace(parsed.Reason)
	parsed.Evidence = strings.TrimSpace(parsed.Evidence)
	parsed.Message = strings.TrimSpace(parsed.Message)
	switch session.GoalStatus(parsed.Status) {
	case session.GoalStatusComplete, session.GoalStatusBlocked:
		return parsed, nil
	default:
		return UpdateGoalArgs{}, NewToolError(ErrInvalidParams, "status must be complete or blocked")
	}
}

func previewGoalText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	const max = 60
	if len(text) <= max {
		return text
	}
	return text[:max-1] + "…"
}

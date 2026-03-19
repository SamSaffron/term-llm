package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

const (
	UpdateProgressToolName   = "update_progress"
	FinalizeProgressToolName = "finalize_progress"
)

type progressToolArgs struct {
	State   map[string]any `json:"state"`
	Reason  string         `json:"reason,omitempty"`
	Message string         `json:"message,omitempty"`
	Final   bool           `json:"final,omitempty"`
}

type progressTool struct {
	name        string
	description string
	final       bool
}

// NewUpdateProgressTool creates the non-finishing progress checkpoint tool.
func NewUpdateProgressTool() llm.Tool {
	return &progressTool{
		name:        UpdateProgressToolName,
		description: "Persist the current best-so-far state as a JSON object without ending the run.",
	}
}

// NewFinalizeProgressTool creates the finishing progress tool used only during finalization.
func NewFinalizeProgressTool() llm.Tool {
	return &progressTool{
		name:        FinalizeProgressToolName,
		description: "Persist the final best-so-far state as a JSON object and finish the run.",
		final:       true,
	}
}

func (t *progressTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        t.name,
		Description: t.description,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"state": map[string]any{
					"type":                 "object",
					"description":          "Best-so-far structured state for the task.",
					"additionalProperties": true,
				},
				"reason": map[string]any{
					"type":        []string{"string", "null"},
					"description": "Why this checkpoint is being saved: voluntary, milestone, or finalize.",
					"enum":        []any{"voluntary", "milestone", "finalize", nil},
				},
				"message": map[string]any{
					"type":        []string{"string", "null"},
					"description": "Optional short summary of what changed.",
				},
				"final": map[string]any{
					"type":        []string{"boolean", "null"},
					"description": "Whether this checkpoint is the final saved state for the run.",
				},
			},
			"required":             []string{"state", "reason", "message", "final"},
			"additionalProperties": false,
		},
	}
}

func (t *progressTool) Execute(_ context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	parsed, err := parseProgressToolArgs(args)
	if err != nil {
		return llm.ToolOutput{}, err
	}

	if t.final {
		parsed.Final = true
		if strings.TrimSpace(parsed.Reason) == "" {
			parsed.Reason = "finalize"
		}
	}

	if parsed.Final {
		return llm.TextOutput("final progress saved"), nil
	}
	return llm.TextOutput("progress saved"), nil
}

func (t *progressTool) Preview(args json.RawMessage) string {
	parsed, err := parseProgressToolArgs(args)
	if err != nil {
		if t.final {
			return "(save final progress)"
		}
		return "(save progress)"
	}

	parts := make([]string, 0, 2)
	if t.final || parsed.Final {
		parts = append(parts, "final")
	} else if reason := strings.TrimSpace(parsed.Reason); reason != "" {
		parts = append(parts, reason)
	}
	if message := strings.TrimSpace(parsed.Message); message != "" {
		parts = append(parts, message)
	}

	if len(parts) == 0 {
		if t.final {
			return "(save final progress)"
		}
		return "(save progress)"
	}
	return "(" + strings.Join(parts, ": ") + ")"
}

func (t *progressTool) IsFinishingTool() bool {
	return t.final
}

func parseProgressToolArgs(args json.RawMessage) (progressToolArgs, error) {
	var parsed progressToolArgs
	if err := json.Unmarshal(args, &parsed); err != nil {
		return progressToolArgs{}, NewToolErrorf(ErrInvalidParams, "parse progress args: %v", err)
	}
	if parsed.State == nil {
		return progressToolArgs{}, NewToolError(ErrInvalidParams, "state must be an object")
	}
	return parsed, nil
}

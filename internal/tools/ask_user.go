package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/samsaffron/term-llm/internal/llm"
)

// AskUserQuestion represents a question to present to the user.
type AskUserQuestion struct {
	Header   string          `json:"header"`
	Question string          `json:"question"`
	Options  []AskUserOption `json:"options"`
}

// AskUserOption represents a choice for a question.
type AskUserOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// AskUserAnswer represents the user's answer to a question.
type AskUserAnswer struct {
	QuestionIndex int    `json:"question_index"`
	Header        string `json:"header"`
	Selected      string `json:"selected"`
	IsCustom      bool   `json:"is_custom"`
}

// AskUserResult is the complete result returned by the tool.
type AskUserResult struct {
	Answers []AskUserAnswer `json:"answers,omitempty"`
	Error   string          `json:"error,omitempty"`
	Type    string          `json:"type,omitempty"`
}

// AskUserArgs are the arguments passed to the ask_user tool.
type AskUserArgs struct {
	Questions []AskUserQuestion `json:"questions"`
}

// AskUserTool implements the ask_user tool.
type AskUserTool struct{}

// NewAskUserTool creates a new ask_user tool.
func NewAskUserTool() *AskUserTool {
	return &AskUserTool{}
}

// Spec returns the tool specification.
func (t *AskUserTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: AskUserToolName,
		Description: `Present questions to the user and gather their responses. Use this when you need clarification, preferences, or decisions from the user. Each question can have 2-4 predefined options plus an automatic 'Other' option for custom input.

Guidelines:
- Use for implementation choices, preferences, or clarifications
- Keep questions focused and actionable
- Provide clear, distinct options with helpful descriptions
- Use descriptive headers (max 12 chars) for tab navigation`,
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"questions": map[string]interface{}{
					"type":        "array",
					"description": "Array of 1-4 questions to ask the user",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"header": map[string]interface{}{
								"type":        "string",
								"description": "Short label for the question (max 12 chars), e.g., 'Database', 'Auth method'",
							},
							"question": map[string]interface{}{
								"type":        "string",
								"description": "The full question text to display",
							},
							"options": map[string]interface{}{
								"type":        "array",
								"description": "2-4 predefined answer options",
								"items": map[string]interface{}{
									"type": "object",
									"properties": map[string]interface{}{
										"label": map[string]interface{}{
											"type":        "string",
											"description": "Short option label (1-5 words)",
										},
										"description": map[string]interface{}{
											"type":        "string",
											"description": "Explanation of what this option means",
										},
									},
									"required": []string{"label", "description"},
								},
								"minItems": 2,
								"maxItems": 4,
							},
						},
						"required": []string{"header", "question", "options"},
					},
					"minItems": 1,
					"maxItems": 4,
				},
			},
			"required":             []string{"questions"},
			"additionalProperties": false,
		},
	}
}

// Execute runs the ask_user tool.
func (t *AskUserTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a AskUserArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return formatAskUserError(ErrInvalidParams, fmt.Sprintf("failed to parse arguments: %v", err)), nil
	}

	// Validate arguments
	if len(a.Questions) == 0 {
		return formatAskUserError(ErrInvalidParams, "at least one question is required"), nil
	}
	if len(a.Questions) > 4 {
		return formatAskUserError(ErrInvalidParams, "maximum 4 questions allowed"), nil
	}

	for i, q := range a.Questions {
		if q.Header == "" {
			return formatAskUserError(ErrInvalidParams, fmt.Sprintf("question %d: header is required", i+1)), nil
		}
		if len(q.Header) > 12 {
			return formatAskUserError(ErrInvalidParams, fmt.Sprintf("question %d: header must be max 12 characters", i+1)), nil
		}
		if q.Question == "" {
			return formatAskUserError(ErrInvalidParams, fmt.Sprintf("question %d: question text is required", i+1)), nil
		}
		if len(q.Options) < 2 {
			return formatAskUserError(ErrInvalidParams, fmt.Sprintf("question %d: at least 2 options required", i+1)), nil
		}
		if len(q.Options) > 4 {
			return formatAskUserError(ErrInvalidParams, fmt.Sprintf("question %d: maximum 4 options allowed", i+1)), nil
		}
		for j, opt := range q.Options {
			if opt.Label == "" {
				return formatAskUserError(ErrInvalidParams, fmt.Sprintf("question %d, option %d: label is required", i+1, j+1)), nil
			}
			if opt.Description == "" {
				return formatAskUserError(ErrInvalidParams, fmt.Sprintf("question %d, option %d: description is required", i+1, j+1)), nil
			}
		}
	}

	// Get hooks under mutex protection
	askUserMu.Lock()
	startHook := OnAskUserStart
	endHook := OnAskUserEnd
	askUserMu.Unlock()

	// Call the hooks to pause spinner/TUI before showing UI
	if startHook != nil {
		startHook()
	}

	// Run the interactive UI
	answers, err := RunAskUser(a.Questions)

	// Resume spinner/TUI after UI completes
	if endHook != nil {
		endHook()
	}

	if err != nil {
		// Check for cancellation
		if err.Error() == "cancelled by user" {
			result := AskUserResult{
				Error: "User dismissed the question dialog",
				Type:  "USER_CANCELLED",
			}
			data, _ := json.Marshal(result)
			return string(data), nil
		}
		// Other errors (e.g., no TTY)
		return formatAskUserError(ErrExecutionFailed, err.Error()), nil
	}

	// Return successful result
	result := AskUserResult{Answers: answers}
	data, err := json.Marshal(result)
	if err != nil {
		return formatAskUserError(ErrExecutionFailed, fmt.Sprintf("failed to marshal result: %v", err)), nil
	}
	return string(data), nil
}

// Preview returns a short description of the tool call.
func (t *AskUserTool) Preview(args json.RawMessage) string {
	var a AskUserArgs
	if err := json.Unmarshal(args, &a); err != nil || len(a.Questions) == 0 {
		return ""
	}
	if len(a.Questions) == 1 {
		return a.Questions[0].Header
	}
	return fmt.Sprintf("%d questions", len(a.Questions))
}

// formatAskUserError formats an error for the LLM.
func formatAskUserError(errType ToolErrorType, message string) string {
	result := AskUserResult{
		Error: message,
		Type:  string(errType),
	}
	data, _ := json.Marshal(result)
	return string(data)
}

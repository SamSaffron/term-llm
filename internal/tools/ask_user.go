package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

// AskUserQuestion represents a question to present to the user.
type AskUserQuestion struct {
	Header      string          `json:"header"`
	Question    string          `json:"question"`
	Options     []AskUserOption `json:"options"`
	MultiSelect bool            `json:"multi_select"`
}

// AskUserOption represents a choice for a question.
type AskUserOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// AskUserAnswer represents the user's answer to a question.
type AskUserAnswer struct {
	QuestionIndex int      `json:"question_index"`
	Header        string   `json:"header"`
	Selected      string   `json:"selected"`
	SelectedList  []string `json:"selected_list,omitempty"`
	IsCustom      bool     `json:"is_custom"`
	IsMultiSelect bool     `json:"is_multi_select,omitempty"`
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

// NormalizeAskUserAnswers validates ask_user answers and rewrites them into the
// canonical shape expected by the tool result.
func NormalizeAskUserAnswers(questions []AskUserQuestion, answers []AskUserAnswer) ([]AskUserAnswer, error) {
	if len(answers) != len(questions) {
		return nil, fmt.Errorf("ask_user UI returned incomplete answers")
	}

	normalized := make([]AskUserAnswer, len(questions))
	for i, q := range questions {
		answer := answers[i]
		if q.MultiSelect {
			if answer.IsCustom {
				return nil, fmt.Errorf("question %d does not support custom answers", i+1)
			}
			if len(answer.SelectedList) == 0 {
				return nil, fmt.Errorf("question %d requires at least one selection", i+1)
			}
			allowed := make(map[string]struct{}, len(q.Options))
			for _, opt := range q.Options {
				allowed[opt.Label] = struct{}{}
			}
			selected := make([]string, 0, len(answer.SelectedList))
			seen := make(map[string]struct{}, len(answer.SelectedList))
			for _, raw := range answer.SelectedList {
				label := strings.TrimSpace(raw)
				if label == "" {
					return nil, fmt.Errorf("question %d has an empty selection", i+1)
				}
				if _, ok := allowed[label]; !ok {
					return nil, fmt.Errorf("question %d has invalid selection %q", i+1, label)
				}
				if _, ok := seen[label]; ok {
					return nil, fmt.Errorf("question %d has duplicate selection %q", i+1, label)
				}
				seen[label] = struct{}{}
				selected = append(selected, label)
			}
			normalized[i] = AskUserAnswer{
				QuestionIndex: i,
				Header:        q.Header,
				Selected:      strings.Join(selected, ", "),
				SelectedList:  selected,
				IsCustom:      false,
				IsMultiSelect: true,
			}
			continue
		}

		selected := strings.TrimSpace(answer.Selected)
		if selected == "" {
			return nil, fmt.Errorf("question %d requires an answer", i+1)
		}
		if !answer.IsCustom {
			valid := false
			for _, opt := range q.Options {
				if opt.Label == selected {
					valid = true
					break
				}
			}
			if !valid {
				return nil, fmt.Errorf("question %d has invalid selection %q", i+1, selected)
			}
		}
		normalized[i] = AskUserAnswer{
			QuestionIndex: i,
			Header:        q.Header,
			Selected:      selected,
			IsCustom:      answer.IsCustom,
			IsMultiSelect: false,
		}
	}

	return normalized, nil
}

// AskUserAnswerSummary renders a compact summary of canonical ask_user answers.
func AskUserAnswerSummary(answers []AskUserAnswer) string {
	parts := make([]string, 0, len(answers))
	for _, answer := range answers {
		header := strings.TrimSpace(answer.Header)
		selected := strings.TrimSpace(answer.Selected)
		if header == "" {
			parts = append(parts, selected)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s", header, selected))
	}
	return strings.Join(parts, " | ")
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
		Description: `Present questions to the user and gather their responses. Use this when you need clarification, preferences, or decisions from the user. Each question can have 2-8 predefined options plus an automatic 'Other' option for custom input.

Guidelines:
- Use for implementation choices, preferences, or clarifications
- Keep questions focused and actionable
- Provide clear, distinct options with helpful descriptions
- Use descriptive headers (max 12 chars) for tab navigation
- Set multi_select: true when users should be able to select multiple options`,
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
								"description": "2-8 predefined answer options",
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
								"maxItems": 8,
							},
							"multi_select": map[string]interface{}{
								"type":        "boolean",
								"description": "If true, user can select multiple options",
								"default":     false,
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
func (t *AskUserTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a AskUserArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(formatAskUserError(ErrInvalidParams, fmt.Sprintf("failed to parse arguments: %v", err))), nil
	}

	// Validate arguments
	if len(a.Questions) == 0 {
		return llm.TextOutput(formatAskUserError(ErrInvalidParams, "at least one question is required")), nil
	}
	if len(a.Questions) > 4 {
		return llm.TextOutput(formatAskUserError(ErrInvalidParams, "maximum 4 questions allowed")), nil
	}

	for i, q := range a.Questions {
		if q.Header == "" {
			return llm.TextOutput(formatAskUserError(ErrInvalidParams, fmt.Sprintf("question %d: header is required", i+1))), nil
		}
		if len(q.Header) > 12 {
			return llm.TextOutput(formatAskUserError(ErrInvalidParams, fmt.Sprintf("question %d: header must be max 12 characters", i+1))), nil
		}
		if q.Question == "" {
			return llm.TextOutput(formatAskUserError(ErrInvalidParams, fmt.Sprintf("question %d: question text is required", i+1))), nil
		}
		if len(q.Options) < 2 {
			return llm.TextOutput(formatAskUserError(ErrInvalidParams, fmt.Sprintf("question %d: at least 2 options required", i+1))), nil
		}
		if len(q.Options) > 8 {
			return llm.TextOutput(formatAskUserError(ErrInvalidParams, fmt.Sprintf("question %d: maximum 8 options allowed", i+1))), nil
		}
		for j, opt := range q.Options {
			if opt.Label == "" {
				return llm.TextOutput(formatAskUserError(ErrInvalidParams, fmt.Sprintf("question %d, option %d: label is required", i+1, j+1))), nil
			}
			if opt.Description == "" {
				return llm.TextOutput(formatAskUserError(ErrInvalidParams, fmt.Sprintf("question %d, option %d: description is required", i+1, j+1))), nil
			}
		}
	}

	ctxHandler := askUserUIFuncFromContext(ctx)

	// Get hooks and UI func under mutex protection
	askUserMu.Lock()
	startHook := OnAskUserStart
	endHook := OnAskUserEnd
	uiFunc := AskUserUIFunc
	askUserMu.Unlock()

	var answers []AskUserAnswer
	var err error

	switch {
	case ctxHandler != nil:
		answers, err = ctxHandler(ctx, a.Questions)
	case uiFunc != nil:
		// Use custom UI function (inline rendering in alt screen mode)
		answers, err = uiFunc(a.Questions)
	default:
		// Use default RunAskUser with hooks
		// Call the hooks to pause spinner/TUI before showing UI
		if startHook != nil {
			startHook()
		}

		// Run the interactive UI
		answers, err = RunAskUser(a.Questions)

		// Resume spinner/TUI after UI completes
		if endHook != nil {
			endHook()
		}
	}

	if err != nil {
		// Check for cancellation
		if err.Error() == "cancelled by user" {
			result := AskUserResult{
				Error: "User dismissed the question dialog",
				Type:  "USER_CANCELLED",
			}
			data, _ := json.Marshal(result)
			return llm.TextOutput(string(data)), nil
		}
		// Other errors (e.g., no TTY)
		return llm.TextOutput(formatAskUserError(ErrExecutionFailed, err.Error())), nil
	}

	answers, err = NormalizeAskUserAnswers(a.Questions, answers)
	if err != nil {
		return llm.TextOutput(formatAskUserError(ErrExecutionFailed, err.Error())), nil
	}

	// Return successful result
	result := AskUserResult{Answers: answers}
	data, err := json.Marshal(result)
	if err != nil {
		return llm.TextOutput(formatAskUserError(ErrExecutionFailed, fmt.Sprintf("failed to marshal result: %v", err))), nil
	}
	return llm.TextOutput(string(data)), nil
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

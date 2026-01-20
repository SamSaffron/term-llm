package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/skills"
)

// ActivateSkillToolName is the tool spec name.
const ActivateSkillToolName = "activate_skill"

// ActivateSkillArgs are the arguments for the activate_skill tool.
type ActivateSkillArgs struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt,omitempty"`
}

// SkillActivatedCallback is called when a skill is activated.
// It receives the skill's allowed tools list (may be empty if skill has no restrictions).
type SkillActivatedCallback func(allowedTools []string)

// ActivateSkillTool implements the activate_skill tool.
type ActivateSkillTool struct {
	registry    *skills.Registry
	approval    *ApprovalManager
	onActivated SkillActivatedCallback
}

// NewActivateSkillTool creates a new activate_skill tool.
func NewActivateSkillTool(registry *skills.Registry, approval *ApprovalManager) *ActivateSkillTool {
	return &ActivateSkillTool{
		registry: registry,
		approval: approval,
	}
}

// SetOnActivated sets a callback that's called when a skill is activated.
// If the skill has allowed-tools, the callback receives the list for enforcement.
func (t *ActivateSkillTool) SetOnActivated(cb SkillActivatedCallback) {
	t.onActivated = cb
}

// Spec returns the tool specification.
func (t *ActivateSkillTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: ActivateSkillToolName,
		Description: `Activate a skill to load its specialized instructions and capabilities.
Skills provide domain-specific knowledge and guidelines for completing tasks.

Usage:
- Use the skill name from the <available_skills> list
- Optionally provide a prompt for task-specific context
- The skill's full instructions will be loaded and returned
- Use bundled resources (references/, scripts/, assets/) as needed`,
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "The skill name to activate (from available_skills list)",
				},
				"prompt": map[string]interface{}{
					"type":        "string",
					"description": "Optional task-specific context to include with the skill",
				},
			},
			"required":             []string{"name"},
			"additionalProperties": false,
		},
	}
}

// Execute runs the activate_skill tool.
func (t *ActivateSkillTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a ActivateSkillArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return t.formatError(ErrInvalidParams, fmt.Sprintf("failed to parse arguments: %v", err)), nil
	}

	// Validate skill name
	if a.Name == "" {
		return t.formatError(ErrInvalidParams, "skill name is required"), nil
	}

	// Get the skill
	skill, err := t.registry.Get(a.Name)
	if err != nil {
		return t.formatError(ErrFileNotFound, fmt.Sprintf("skill not found: %s", a.Name)), nil
	}

	// Check if skill directory is in allowed read paths
	// For now, we'll trust the skill since it was discovered via the registry
	// Future: Add approval flow for skill directory access

	// Notify callback about allowed-tools (if callback is set and skill has restrictions)
	if t.onActivated != nil && len(skill.AllowedTools) > 0 {
		t.onActivated(skill.AllowedTools)
	}

	// Generate the activation response
	response := skills.GenerateActivationResponse(skill, a.Prompt)

	return response, nil
}

// Preview returns a short description of the tool call.
func (t *ActivateSkillTool) Preview(args json.RawMessage) string {
	var a ActivateSkillArgs
	if err := json.Unmarshal(args, &a); err != nil || a.Name == "" {
		return ""
	}
	return fmt.Sprintf("Activating skill: %s", a.Name)
}

// formatError formats an error for the LLM.
func (t *ActivateSkillTool) formatError(errType ToolErrorType, message string) string {
	payload := ToolPayload{
		Output: message,
		Error: &ToolError{
			Type:    errType,
			Message: message,
		},
	}
	return payload.ToJSON()
}

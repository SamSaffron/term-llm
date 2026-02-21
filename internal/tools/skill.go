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

// SkillToolsRegisteredCallback is called when a skill with declared tools is activated.
// It receives the skill's tool definitions and the skill's source directory,
// allowing the caller to register the tools with the engine dynamically.
type SkillToolsRegisteredCallback func(defs []skills.SkillToolDef, skillDir string)

// ActivateSkillTool implements the activate_skill tool.
type ActivateSkillTool struct {
	registry         *skills.Registry
	approval         *ApprovalManager
	onActivated      SkillActivatedCallback
	onToolsActivated SkillToolsRegisteredCallback
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

// SetOnToolsActivated sets a callback that's called when a skill with declared tools is activated.
// The callback receives the tool definitions and the skill's directory so the caller
// can register them with the engine.
func (t *ActivateSkillTool) SetOnToolsActivated(cb SkillToolsRegisteredCallback) {
	t.onToolsActivated = cb
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
func (t *ActivateSkillTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a ActivateSkillArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(t.formatError(ErrInvalidParams, fmt.Sprintf("failed to parse arguments: %v", err))), nil
	}

	// Validate skill name
	if a.Name == "" {
		return llm.TextOutput(t.formatError(ErrInvalidParams, "skill name is required")), nil
	}

	// Get the skill
	skill, err := t.registry.Get(a.Name)
	if err != nil {
		return llm.TextOutput(t.formatError(ErrFileNotFound, fmt.Sprintf("skill not found: %s", a.Name))), nil
	}

	// Check if skill directory is in allowed read paths
	// For now, we'll trust the skill since it was discovered via the registry
	// Future: Add approval flow for skill directory access

	// Notify callback about allowed-tools (if callback is set and skill has restrictions)
	if t.onActivated != nil && len(skill.AllowedTools) > 0 {
		t.onActivated(skill.AllowedTools)
	}

	// Register skill-declared tools if present
	if t.onToolsActivated != nil && skill.HasTools() {
		t.onToolsActivated(skill.Tools, skill.SourcePath)
	}

	// Generate the activation response
	response := skills.GenerateActivationResponse(skill, a.Prompt)

	return llm.TextOutput(response), nil
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

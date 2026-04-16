package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/skills"
)

// SearchSkillsToolName is the tool spec name.
const SearchSkillsToolName = "search_skills"

// SearchSkillsArgs are the arguments for the search_skills tool.
type SearchSkillsArgs struct {
	Query string `json:"query"`
}

// SearchSkillsTool implements the search_skills tool.
type SearchSkillsTool struct {
	registry *skills.Registry
}

// NewSearchSkillsTool creates a new search_skills tool.
func NewSearchSkillsTool(registry *skills.Registry) *SearchSkillsTool {
	return &SearchSkillsTool{
		registry: registry,
	}
}

// Spec returns the tool specification.
func (t *SearchSkillsTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: SearchSkillsToolName,
		Description: `Search for skills by keyword. Returns matching skill names and descriptions.
Use this when the skill you need is not listed in <available_skills>.
After finding a relevant skill, use activate_skill to load it.`,
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Search query — matches against skill names and descriptions",
				},
			},
			"required":             []string{"query"},
			"additionalProperties": false,
		},
	}
}

// Execute runs the search_skills tool.
func (t *SearchSkillsTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a SearchSkillsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(t.formatError(ErrInvalidParams, fmt.Sprintf("failed to parse arguments: %v", err))), nil
	}

	if a.Query == "" {
		return llm.TextOutput(t.formatError(ErrInvalidParams, "query is required")), nil
	}

	results, err := t.registry.Search(a.Query, 10)
	if err != nil {
		return llm.TextOutput(t.formatError(ErrExecutionFailed, fmt.Sprintf("search failed: %v", err))), nil
	}

	if len(results) == 0 {
		return llm.TextOutput("No skills found matching: " + a.Query), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d skill(s) matching \"%s\":\n\n", len(results), a.Query))
	for _, skill := range results {
		sb.WriteString(fmt.Sprintf("- **%s** — %s (source: %s)\n", skill.Name, skill.Description, skill.Source.SourceName()))
	}
	sb.WriteString("\nUse activate_skill to load any of these skills.")

	return llm.TextOutput(sb.String()), nil
}

// Preview returns a short description of the tool call.
func (t *SearchSkillsTool) Preview(args json.RawMessage) string {
	var a SearchSkillsArgs
	if err := json.Unmarshal(args, &a); err != nil || a.Query == "" {
		return ""
	}
	return fmt.Sprintf("Searching skills: %s", a.Query)
}

// formatError formats an error for the LLM.
func (t *SearchSkillsTool) formatError(errType ToolErrorType, message string) string {
	payload := ToolPayload{
		Output: message,
		Error: &ToolError{
			Type:    errType,
			Message: message,
		},
	}
	return payload.ToJSON()
}

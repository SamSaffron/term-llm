package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/samsaffron/term-llm/internal/prompt"
)

const (
	SuggestCommandsToolName = "suggest_commands"
	EditToolName            = "edit"
	UnifiedDiffToolName     = "unified_diff"
	WebSearchToolName       = "web_search"
)

const (
	suggestCommandsDescription = "Suggest shell commands based on user input. Call this after gathering any needed information from web search."
)

// Tool describes a callable external tool.
type Tool interface {
	Spec() ToolSpec
	Execute(ctx context.Context, args json.RawMessage) (string, error)
	// Preview returns a human-readable description of what the tool will do,
	// shown to the user before execution starts (e.g., "Generating image: a cat").
	// Returns empty string if no preview is available.
	Preview(args json.RawMessage) string
}

// ToolRegistry stores tools by name for execution.
type ToolRegistry struct {
	tools map[string]Tool
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: make(map[string]Tool)}
}

func (r *ToolRegistry) Register(tool Tool) {
	r.tools[tool.Spec().Name] = tool
}

func (r *ToolRegistry) Get(name string) (Tool, bool) {
	tool, ok := r.tools[name]
	return tool, ok
}

func (r *ToolRegistry) Unregister(name string) {
	delete(r.tools, name)
}

// AllSpecs returns the specs for all registered tools.
func (r *ToolRegistry) AllSpecs() []ToolSpec {
	specs := make([]ToolSpec, 0, len(r.tools))
	for _, tool := range r.tools {
		specs = append(specs, tool.Spec())
	}
	return specs
}

// SuggestCommandsToolSpec returns the tool spec for command suggestions.
func SuggestCommandsToolSpec(numSuggestions int) ToolSpec {
	return ToolSpec{
		Name:        SuggestCommandsToolName,
		Description: suggestCommandsDescription,
		Schema:      prompt.SuggestSchema(numSuggestions),
	}
}

// EditToolDescription is the description for the edit tool.
const EditToolDescription = "Edit a file by replacing old_string with new_string. You may include the literal token <<<elided>>> in old_string to match any sequence of characters (including newlines). Use multiple tool calls for multiple edits."

// EditToolSpec returns the tool spec for the edit tool.
func EditToolSpec() ToolSpec {
	return ToolSpec{
		Name:        EditToolName,
		Description: EditToolDescription,
		Schema:      EditToolSchema(),
	}
}

// EditToolSchema returns the JSON schema for the edit tool.
func EditToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"file_path": map[string]interface{}{
				"type":        "string",
				"description": "Path to the file to edit",
			},
			"old_string": map[string]interface{}{
				"type":        "string",
				"description": "The exact text to find and replace. Include enough context to be unique. You may include the literal token <<<elided>>> to match any sequence of characters (including newlines).",
			},
			"new_string": map[string]interface{}{
				"type":        "string",
				"description": "The text to replace old_string with",
			},
		},
		"required":             []string{"file_path", "old_string", "new_string"},
		"additionalProperties": false,
	}
}

// UnifiedDiffToolDescription is the description for the unified diff tool.
const UnifiedDiffToolDescription = `Apply file edits using unified diff format. Output a single diff containing all changes.

Format:
--- path/to/file
+++ path/to/file
@@ context to locate (e.g., func Name) @@
 context line (unchanged, space prefix)
-line to remove
+line to add

Elision (-...) for replacing large blocks:
-func Example() {
-...
-}
+func Example() { return nil }

The -... matches everything between the start anchor (-func Example...) and end anchor (-}).
IMPORTANT: After -... you MUST include an end anchor (another - line) so we know where elision stops.

Rules:
1. @@ headers help locate changes - use function/class names, not line numbers
2. Context lines (space prefix) anchor the position - must match file exactly
3. Use -... ONLY when replacing 10+ lines; for small changes list all - lines explicitly
4. After -... always include the closing line (e.g., -}) as the end anchor
5. Multiple files: use separate --- +++ blocks for each file`

// UnifiedDiffToolSpec returns the tool spec for unified diff edits.
func UnifiedDiffToolSpec() ToolSpec {
	return ToolSpec{
		Name:        UnifiedDiffToolName,
		Description: UnifiedDiffToolDescription,
		Schema:      UnifiedDiffToolSchema(),
	}
}

// UnifiedDiffToolSchema returns the JSON schema for the unified diff tool.
func UnifiedDiffToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"diff": map[string]interface{}{
				"type":        "string",
				"description": "Unified diff with all changes. Format: --- and +++ for paths, @@ for context headers, space prefix for context lines, - for removals, + for additions. Use -... to elide large removed blocks (must have end anchor after).",
			},
		},
		"required":             []string{"diff"},
		"additionalProperties": false,
	}
}

// WebSearchToolSpec returns the tool spec for external web search.
func WebSearchToolSpec() ToolSpec {
	return ToolSpec{
		Name:        WebSearchToolName,
		Description: "Search the web for current information. Use max_results=20 for comprehensive results.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Search query",
				},
				"max_results": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum number of results to return (recommended: 20)",
					"default":     20,
				},
			},
			"required":             []string{"query"},
			"additionalProperties": false,
		},
	}
}

// ParseCommandSuggestions parses a suggest_commands tool call.
func ParseCommandSuggestions(call ToolCall) ([]CommandSuggestion, error) {
	var resp struct {
		Suggestions []CommandSuggestion `json:"suggestions"`
	}
	if err := json.Unmarshal(call.Arguments, &resp); err != nil {
		return nil, fmt.Errorf("parse suggestions: %w", err)
	}
	return resp.Suggestions, nil
}

// ParseEditToolCall parses a single edit tool call payload.
func ParseEditToolCall(call ToolCall) (EditToolCall, error) {
	var edit EditToolCall
	if err := json.Unmarshal(call.Arguments, &edit); err != nil {
		return EditToolCall{}, fmt.Errorf("parse edit: %w", err)
	}
	return edit, nil
}

// ParseUnifiedDiff parses a unified_diff tool call payload.
func ParseUnifiedDiff(call ToolCall) (string, error) {
	var payload struct {
		Diff string `json:"diff"`
	}
	if err := json.Unmarshal(call.Arguments, &payload); err != nil {
		return "", fmt.Errorf("parse unified diff: %w", err)
	}
	return payload.Diff, nil
}

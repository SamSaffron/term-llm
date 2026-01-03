package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"
	"github.com/samsaffron/term-llm/internal/prompt"
)

// OpenAIProvider implements Provider using the standard OpenAI API
type OpenAIProvider struct {
	client *openai.Client
	model  string
	effort string // reasoning effort: "low", "medium", "high", "xhigh", or ""
}

// parseModelEffort extracts effort suffix from model name
// "gpt-5.2-high" -> ("gpt-5.2", "high")
// "gpt-5.2-xhigh" -> ("gpt-5.2", "xhigh")
// "gpt-5.2" -> ("gpt-5.2", "")
func parseModelEffort(model string) (string, string) {
	// Check suffixes in order from longest to shortest to avoid "-high" matching "-xhigh"
	suffixes := []string{"xhigh", "medium", "high", "low"}
	for _, effort := range suffixes {
		suffix := "-" + effort
		if strings.HasSuffix(model, suffix) {
			return strings.TrimSuffix(model, suffix), effort
		}
	}
	return model, ""
}

func NewOpenAIProvider(apiKey, model string) *OpenAIProvider {
	actualModel, effort := parseModelEffort(model)
	client := openai.NewClient(option.WithAPIKey(apiKey))
	return &OpenAIProvider{
		client: &client,
		model:  actualModel,
		effort: effort,
	}
}

func (p *OpenAIProvider) Name() string {
	if p.effort != "" {
		return fmt.Sprintf("OpenAI (%s, effort=%s)", p.model, p.effort)
	}
	return fmt.Sprintf("OpenAI (%s)", p.model)
}

func (p *OpenAIProvider) SuggestCommands(ctx context.Context, req SuggestRequest) ([]CommandSuggestion, error) {
	numSuggestions := req.NumSuggestions
	if numSuggestions <= 0 {
		numSuggestions = 3
	}

	// Define the function tool for structured output
	functionTool := responses.ToolParamOfFunction(
		"suggest_commands",
		prompt.SuggestSchema(numSuggestions),
		true,
	)
	functionTool.OfFunction.Description = openai.String("Suggest shell commands based on user input")

	tools := []responses.ToolUnionParam{functionTool}

	// Add web search tool if enabled
	if req.EnableSearch {
		webSearchTool := responses.ToolParamOfWebSearchPreview(responses.WebSearchToolTypeWebSearchPreview)
		tools = []responses.ToolUnionParam{webSearchTool, functionTool}
	}

	systemPrompt := prompt.SuggestSystemPrompt(req.Shell, req.Instructions, numSuggestions, req.EnableSearch)
	userPrompt := prompt.SuggestUserPrompt(req.UserInput, req.Files, req.Stdin)

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: OpenAI Request ===")
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		if req.EnableSearch {
			fmt.Fprintln(os.Stderr, "Tools: web_search_preview, suggest_commands")
		} else {
			fmt.Fprintln(os.Stderr, "Tools: suggest_commands")
		}
		fmt.Fprintf(os.Stderr, "Instructions:\n%s\n", systemPrompt)
		fmt.Fprintf(os.Stderr, "Input:\n%s\n", userPrompt)
		fmt.Fprintln(os.Stderr, "=============================")
	}

	params := responses.ResponseNewParams{
		Model:        shared.ResponsesModel(p.model),
		Instructions: openai.String(systemPrompt),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String(userPrompt),
		},
		Tools: tools,
	}

	// Add reasoning effort if set
	if p.effort != "" {
		params.Reasoning = shared.ReasoningParam{
			Effort: shared.ReasoningEffort(p.effort),
		}
	}

	resp, err := p.client.Responses.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("openai API error: %w", err)
	}

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: OpenAI Response ===")
		fmt.Fprintf(os.Stderr, "Status: %s\n", resp.Status)
		for i, item := range resp.Output {
			fmt.Fprintf(os.Stderr, "Output %d: type=%s\n", i, item.Type)
			switch item.Type {
			case "function_call":
				fmt.Fprintf(os.Stderr, "  Function: %s\n", item.Name)
				fmt.Fprintf(os.Stderr, "  Arguments:\n%s\n", item.Arguments)
			case "message":
				for _, content := range item.Content {
					fmt.Fprintf(os.Stderr, "  Content type: %s\n", content.Type)
					if content.Text != "" {
						fmt.Fprintf(os.Stderr, "  Text: %s\n", content.Text)
					}
				}
			case "web_search_call":
				fmt.Fprintf(os.Stderr, "  Web search invoked (id=%s)\n", item.ID)
			default:
				if rawJSON, err := json.Marshal(item); err == nil {
					fmt.Fprintf(os.Stderr, "  Raw: %s\n", string(rawJSON))
				}
			}
		}
		fmt.Fprintln(os.Stderr, "==============================")
	}

	// Find the function call output
	for _, item := range resp.Output {
		if item.Type == "function_call" && item.Name == "suggest_commands" {
			var result suggestionsResponse
			if err := json.Unmarshal([]byte(item.Arguments), &result); err != nil {
				return nil, fmt.Errorf("failed to parse response: %w", err)
			}
			return result.Suggestions, nil
		}
	}

	return nil, fmt.Errorf("no suggest_commands function call in response")
}

// GetEdits calls the LLM with the edit tool and returns all proposed edits
func (p *OpenAIProvider) GetEdits(ctx context.Context, systemPrompt, userPrompt string, debug bool) ([]EditToolCall, error) {
	// Define the edit tool using centralized schema
	editTool := responses.ToolParamOfFunction("edit", prompt.EditSchema(), true)
	editTool.OfFunction.Description = openai.String(prompt.EditDescription)

	if debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: OpenAI Edit Request ===")
		fmt.Fprintf(os.Stderr, "System: %s\n", systemPrompt)
		fmt.Fprintf(os.Stderr, "User: %s\n", userPrompt)
		fmt.Fprintln(os.Stderr, "==================================")
	}

	params := responses.ResponseNewParams{
		Model:        shared.ResponsesModel(p.model),
		Instructions: openai.String(systemPrompt),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String(userPrompt),
		},
		Tools: []responses.ToolUnionParam{editTool},
	}

	// Add reasoning effort if set
	if p.effort != "" {
		params.Reasoning = shared.ReasoningParam{
			Effort: shared.ReasoningEffort(p.effort),
		}
	}

	resp, err := p.client.Responses.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("openai API error: %w", err)
	}

	if debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: OpenAI Edit Response ===")
		fmt.Fprintf(os.Stderr, "Status: %s\n", resp.Status)
		for i, item := range resp.Output {
			fmt.Fprintf(os.Stderr, "Output %d: type=%s\n", i, item.Type)
			if item.Type == "function_call" {
				fmt.Fprintf(os.Stderr, "  Function: %s\n", item.Name)
				fmt.Fprintf(os.Stderr, "  Arguments: %s\n", item.Arguments)
			}
		}
		fmt.Fprintln(os.Stderr, "===================================")
	}

	// Print any text output first
	for _, item := range resp.Output {
		if item.Type == "message" {
			for _, content := range item.Content {
				if content.Type == "output_text" && content.Text != "" {
					fmt.Println(content.Text)
				}
			}
		}
	}

	// Collect all edits from function calls
	var edits []EditToolCall
	for _, item := range resp.Output {
		if item.Type != "function_call" || item.Name != "edit" {
			continue
		}

		var editCall EditToolCall
		if err := json.Unmarshal([]byte(item.Arguments), &editCall); err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing edit: %v\n", err)
			continue
		}

		edits = append(edits, editCall)
	}

	return edits, nil
}

// GetUnifiedDiff calls the LLM with the unified_diff tool and returns the diff string.
// This is more efficient for Codex models which are fine-tuned for single tool calls.
func (p *OpenAIProvider) GetUnifiedDiff(ctx context.Context, systemPrompt, userPrompt string, debug bool) (string, error) {
	// Define the unified diff tool using centralized schema
	diffTool := responses.ToolParamOfFunction("unified_diff", prompt.UnifiedDiffSchema(), true)
	diffTool.OfFunction.Description = openai.String(prompt.UnifiedDiffDescription)

	if debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: OpenAI UnifiedDiff Request ===")
		fmt.Fprintf(os.Stderr, "System: %s\n", systemPrompt)
		fmt.Fprintf(os.Stderr, "User: %s\n", userPrompt)
		fmt.Fprintln(os.Stderr, "=========================================")
	}

	params := responses.ResponseNewParams{
		Model:        shared.ResponsesModel(p.model),
		Instructions: openai.String(systemPrompt),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String(userPrompt),
		},
		Tools: []responses.ToolUnionParam{diffTool},
	}

	// Add reasoning effort if set
	if p.effort != "" {
		params.Reasoning = shared.ReasoningParam{
			Effort: shared.ReasoningEffort(p.effort),
		}
	}

	resp, err := p.client.Responses.New(ctx, params)
	if err != nil {
		return "", fmt.Errorf("openai API error: %w", err)
	}

	if debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: OpenAI UnifiedDiff Response ===")
		fmt.Fprintf(os.Stderr, "Status: %s\n", resp.Status)
		for i, item := range resp.Output {
			fmt.Fprintf(os.Stderr, "Output %d: type=%s\n", i, item.Type)
			if item.Type == "function_call" {
				fmt.Fprintf(os.Stderr, "  Function: %s\n", item.Name)
			}
		}
		fmt.Fprintln(os.Stderr, "==========================================")
	}

	// Print any text output first
	for _, item := range resp.Output {
		if item.Type == "message" {
			for _, content := range item.Content {
				if content.Type == "output_text" && content.Text != "" {
					fmt.Println(content.Text)
				}
			}
		}
	}

	// Find the unified_diff function call
	for _, item := range resp.Output {
		if item.Type == "function_call" && item.Name == "unified_diff" {
			var diffResult struct {
				Diff string `json:"diff"`
			}
			if err := json.Unmarshal([]byte(item.Arguments), &diffResult); err != nil {
				return "", fmt.Errorf("failed to parse diff result: %w", err)
			}
			return diffResult.Diff, nil
		}
	}

	return "", fmt.Errorf("no unified_diff function call in response")
}

func (p *OpenAIProvider) StreamResponse(ctx context.Context, req AskRequest, output chan<- string) error {
	defer close(output)

	userMessage := prompt.AskUserPrompt(req.Question, req.Files, req.Stdin)

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: OpenAI Stream Request ===")
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "Question: %s\n", req.Question)
		fmt.Fprintf(os.Stderr, "Search: %v\n", req.EnableSearch)
		fmt.Fprintln(os.Stderr, "====================================")
	}

	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(p.model),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String(userMessage),
		},
	}

	if req.EnableSearch {
		webSearchTool := responses.ToolParamOfWebSearchPreview(responses.WebSearchToolTypeWebSearchPreview)
		params.Tools = []responses.ToolUnionParam{webSearchTool}
	}

	// Add reasoning effort if set
	if p.effort != "" {
		params.Reasoning = shared.ReasoningParam{
			Effort: shared.ReasoningEffort(p.effort),
		}
	}

	stream := p.client.Responses.NewStreaming(ctx, params)

	for stream.Next() {
		event := stream.Current()
		if event.Type == "response.output_text.delta" && event.Text != "" {
			output <- event.Text
		}
	}

	if err := stream.Err(); err != nil {
		return fmt.Errorf("openai streaming error: %w", err)
	}

	return nil
}

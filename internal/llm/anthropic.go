package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/samsaffron/term-llm/internal/prompt"
)

// AnthropicProvider implements Provider using the Anthropic API
type AnthropicProvider struct {
	client *anthropic.Client
	model  string
}

func NewAnthropicProvider(apiKey, model string) *AnthropicProvider {
	client := anthropic.NewClient()
	return &AnthropicProvider{
		client: &client,
		model:  model,
	}
}

func (p *AnthropicProvider) Name() string {
	return fmt.Sprintf("Anthropic (%s)", p.model)
}

func (p *AnthropicProvider) SuggestCommands(ctx context.Context, req SuggestRequest) ([]CommandSuggestion, error) {
	if req.EnableSearch {
		return p.suggestWithSearch(ctx, req)
	}
	return p.suggestWithoutSearch(ctx, req)
}

func (p *AnthropicProvider) suggestWithoutSearch(ctx context.Context, req SuggestRequest) ([]CommandSuggestion, error) {
	numSuggestions := req.NumSuggestions
	if numSuggestions <= 0 {
		numSuggestions = 3
	}

	inputSchema := anthropic.ToolInputSchemaParam{
		Type: "object",
		Properties: map[string]interface{}{
			"suggestions": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"command": map[string]interface{}{
							"type":        "string",
							"description": "The shell command to execute",
						},
						"explanation": map[string]interface{}{
							"type":        "string",
							"description": "Brief explanation of what the command does",
						},
						"likelihood": map[string]interface{}{
							"type":        "integer",
							"minimum":     1,
							"maximum":     10,
							"description": "How likely this command matches user intent (1=unlikely, 10=very likely)",
						},
					},
					"required": []string{"command", "explanation", "likelihood"},
				},
				"minItems": numSuggestions,
				"maxItems": numSuggestions,
			},
		},
		Required: []string{"suggestions"},
	}

	tool := anthropic.ToolUnionParamOfTool(inputSchema, "suggest_commands")
	tool.OfTool.Description = anthropic.String("Suggest shell commands based on user input")

	systemPrompt := prompt.SuggestSystemPrompt(req.Shell, req.Instructions, numSuggestions, false)
	userPrompt := prompt.SuggestUserPrompt(req.UserInput)

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Anthropic Request ===")
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "Tools: suggest_commands\n")
		fmt.Fprintf(os.Stderr, "System:\n%s\n", systemPrompt)
		fmt.Fprintf(os.Stderr, "User:\n%s\n", userPrompt)
		fmt.Fprintln(os.Stderr, "================================")
	}

	message, err := p.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: 1024,
		System: []anthropic.TextBlockParam{
			{Text: systemPrompt},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userPrompt)),
		},
		Tools:      []anthropic.ToolUnionParam{tool},
		ToolChoice: anthropic.ToolChoiceParamOfTool("suggest_commands"),
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic API error: %w", err)
	}

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Anthropic Response ===")
		for _, block := range message.Content {
			if block.Type == "tool_use" {
				fmt.Fprintf(os.Stderr, "Tool: %s\n", block.Name)
				fmt.Fprintf(os.Stderr, "Arguments:\n%s\n", block.JSON.Input.Raw())
			} else if block.Type == "text" {
				fmt.Fprintf(os.Stderr, "Text: %s\n", block.Text)
			}
		}
		fmt.Fprintln(os.Stderr, "=================================")
	}

	return p.extractSuggestions(message.Content)
}

func (p *AnthropicProvider) suggestWithSearch(ctx context.Context, req SuggestRequest) ([]CommandSuggestion, error) {
	numSuggestions := req.NumSuggestions
	if numSuggestions <= 0 {
		numSuggestions = 3
	}

	inputSchema := anthropic.BetaToolInputSchemaParam{
		Type: "object",
		Properties: map[string]interface{}{
			"suggestions": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"command": map[string]interface{}{
							"type":        "string",
							"description": "The shell command to execute",
						},
						"explanation": map[string]interface{}{
							"type":        "string",
							"description": "Brief explanation of what the command does",
						},
						"likelihood": map[string]interface{}{
							"type":        "integer",
							"minimum":     1,
							"maximum":     10,
							"description": "How likely this command matches user intent (1=unlikely, 10=very likely)",
						},
					},
					"required": []string{"command", "explanation", "likelihood"},
				},
				"minItems": numSuggestions,
				"maxItems": numSuggestions,
			},
		},
		Required: []string{"suggestions"},
	}

	suggestTool := anthropic.BetaToolUnionParam{
		OfTool: &anthropic.BetaToolParam{
			Name:        "suggest_commands",
			Description: anthropic.String("Suggest shell commands based on user input. Call this after gathering any needed information from web search."),
			InputSchema: inputSchema,
		},
	}

	webSearchTool := anthropic.BetaToolUnionParam{
		OfWebSearchTool20250305: &anthropic.BetaWebSearchTool20250305Param{
			MaxUses: anthropic.Int(3),
		},
	}

	systemPrompt := prompt.SuggestSystemPrompt(req.Shell, req.Instructions, numSuggestions, true)
	userPrompt := prompt.SuggestUserPrompt(req.UserInput)

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Anthropic Request (with search) ===")
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintln(os.Stderr, "Tools: web_search, suggest_commands")
		fmt.Fprintf(os.Stderr, "System:\n%s\n", systemPrompt)
		fmt.Fprintf(os.Stderr, "User:\n%s\n", userPrompt)
		fmt.Fprintln(os.Stderr, "===============================================")
	}

	message, err := p.client.Beta.Messages.New(ctx, anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: 4096,
		Betas:     []anthropic.AnthropicBeta{"web-search-2025-03-05"},
		System: []anthropic.BetaTextBlockParam{
			{Text: systemPrompt},
		},
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(userPrompt)),
		},
		Tools: []anthropic.BetaToolUnionParam{webSearchTool, suggestTool},
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic API error: %w", err)
	}

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Anthropic Response (with search) ===")
		fmt.Fprintf(os.Stderr, "Stop reason: %s\n", message.StopReason)
		for i, block := range message.Content {
			fmt.Fprintf(os.Stderr, "Block %d: type=%s\n", i, block.Type)
			switch block.Type {
			case "tool_use":
				fmt.Fprintf(os.Stderr, "  Tool: %s\n", block.Name)
				fmt.Fprintf(os.Stderr, "  Arguments:\n%s\n", block.JSON.Input.Raw())
			case "text":
				fmt.Fprintf(os.Stderr, "  Text: %s\n", block.Text)
			case "web_search_tool_result":
				fmt.Fprintf(os.Stderr, "  Search ID: %s\n", block.ToolUseID)
			case "server_tool_use":
				fmt.Fprintf(os.Stderr, "  Server Tool: %s (id=%s)\n", block.Name, block.ID)
			default:
				if rawJSON, err := json.Marshal(block); err == nil {
					fmt.Fprintf(os.Stderr, "  Raw: %s\n", string(rawJSON))
				}
			}
		}
		fmt.Fprintln(os.Stderr, "================================================")
	}

	return p.extractBetaSuggestions(message.Content)
}

func (p *AnthropicProvider) extractSuggestions(content []anthropic.ContentBlockUnion) ([]CommandSuggestion, error) {
	for _, block := range content {
		if block.Type == "tool_use" {
			var resp suggestionsResponse
			if err := json.Unmarshal([]byte(block.JSON.Input.Raw()), &resp); err != nil {
				return nil, fmt.Errorf("failed to parse response: %w", err)
			}
			return resp.Suggestions, nil
		}
	}
	return nil, fmt.Errorf("no tool use in response")
}

func (p *AnthropicProvider) extractBetaSuggestions(content []anthropic.BetaContentBlockUnion) ([]CommandSuggestion, error) {
	for _, block := range content {
		if block.Type == "tool_use" && block.Name == "suggest_commands" {
			var resp suggestionsResponse
			if err := json.Unmarshal([]byte(block.JSON.Input.Raw()), &resp); err != nil {
				return nil, fmt.Errorf("failed to parse response: %w", err)
			}
			return resp.Suggestions, nil
		}
	}
	return nil, fmt.Errorf("no suggest_commands tool use in response")
}

func (p *AnthropicProvider) StreamResponse(ctx context.Context, req AskRequest, output chan<- string) error {
	defer close(output)

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Anthropic Stream Request ===")
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "Question: %s\n", req.Question)
		fmt.Fprintf(os.Stderr, "Search: %v\n", req.EnableSearch)
		fmt.Fprintln(os.Stderr, "=======================================")
	}

	if req.EnableSearch {
		return p.streamWithSearch(ctx, req, output)
	}
	return p.streamWithoutSearch(ctx, req, output)
}

func (p *AnthropicProvider) streamWithoutSearch(ctx context.Context, req AskRequest, output chan<- string) error {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: 4096,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.Question)),
		},
	}

	// Add system prompt if instructions provided
	if req.Instructions != "" {
		params.System = []anthropic.TextBlockParam{
			{Text: req.Instructions},
		}
	}

	stream := p.client.Messages.NewStreaming(ctx, params)

	for stream.Next() {
		event := stream.Current()
		if event.Type == "content_block_delta" && event.Delta.Text != "" {
			output <- event.Delta.Text
		}
	}

	if err := stream.Err(); err != nil {
		return fmt.Errorf("anthropic streaming error: %w", err)
	}

	return nil
}

func (p *AnthropicProvider) streamWithSearch(ctx context.Context, req AskRequest, output chan<- string) error {
	webSearchTool := anthropic.BetaToolUnionParam{
		OfWebSearchTool20250305: &anthropic.BetaWebSearchTool20250305Param{
			MaxUses: anthropic.Int(5),
		},
	}

	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: 4096,
		Betas:     []anthropic.AnthropicBeta{"web-search-2025-03-05"},
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(req.Question)),
		},
		Tools: []anthropic.BetaToolUnionParam{webSearchTool},
	}

	// Add system prompt if instructions provided
	if req.Instructions != "" {
		params.System = []anthropic.BetaTextBlockParam{
			{Text: req.Instructions},
		}
	}

	stream := p.client.Beta.Messages.NewStreaming(ctx, params)

	for stream.Next() {
		event := stream.Current()
		if event.Type == "content_block_delta" && event.Delta.Text != "" {
			output <- event.Delta.Text
		}
	}

	if err := stream.Err(); err != nil {
		return fmt.Errorf("anthropic streaming error: %w", err)
	}

	return nil
}

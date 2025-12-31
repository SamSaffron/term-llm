package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/samsaffron/term-llm/internal/prompt"
)

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

type suggestionsResponse struct {
	Suggestions []CommandSuggestion `json:"suggestions"`
}

func (p *AnthropicProvider) SuggestCommands(ctx context.Context, userInput string, shell string, systemContext string) ([]CommandSuggestion, error) {
	// Define the tool schema for structured output
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
				"minItems": 3,
				"maxItems": 3,
			},
		},
		Required: []string{"suggestions"},
	}

	// Create tool using the helper function
	tool := anthropic.ToolUnionParamOfTool(inputSchema, "suggest_commands")
	tool.OfTool.Description = anthropic.String("Suggest shell commands based on user input")

	message, err := p.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: 1024,
		System: []anthropic.TextBlockParam{
			{Text: prompt.SystemPrompt(shell, systemContext)},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt.UserPrompt(userInput))),
		},
		Tools:      []anthropic.ToolUnionParam{tool},
		ToolChoice: anthropic.ToolChoiceParamOfTool("suggest_commands"),
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic API error: %w", err)
	}

	// Extract tool use from response
	for _, block := range message.Content {
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

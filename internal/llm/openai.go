package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/samsaffron/term-llm/internal/prompt"
	openai "github.com/sashabaranov/go-openai"
)

type OpenAIProvider struct {
	client *openai.Client
	model  string
}

func NewOpenAIProvider(apiKey, model string) *OpenAIProvider {
	return &OpenAIProvider{
		client: openai.NewClient(apiKey),
		model:  model,
	}
}

func (p *OpenAIProvider) SuggestCommands(ctx context.Context, userInput string, shell string, systemContext string) ([]CommandSuggestion, error) {
	// Define the function for structured output
	functions := []openai.FunctionDefinition{
		{
			Name:        "suggest_commands",
			Description: "Suggest shell commands based on user input",
			Parameters:  prompt.JSONSchema(),
		},
	}

	resp, err := p.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: p.model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: prompt.SystemPrompt(shell, systemContext),
			},
			{
				Role:    openai.ChatMessageRoleUser,
				Content: prompt.UserPrompt(userInput),
			},
		},
		Functions: functions,
		FunctionCall: openai.FunctionCall{
			Name: "suggest_commands",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("openai API error: %w", err)
	}

	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("no response from OpenAI")
	}

	choice := resp.Choices[0]
	if choice.Message.FunctionCall == nil {
		return nil, fmt.Errorf("no function call in response")
	}

	var result suggestionsResponse
	if err := json.Unmarshal([]byte(choice.Message.FunctionCall.Arguments), &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return result.Suggestions, nil
}

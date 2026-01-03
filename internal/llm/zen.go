package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/samsaffron/term-llm/internal/prompt"
)

const zenBaseURL = "https://opencode.ai/zen/v1/chat/completions"

// ZenProvider implements Provider using the OpenCode Zen API
// Zen provides free access to models like GLM 4.7 via opencode.ai
// API key is optional: empty for free tier, or set for paid models
type ZenProvider struct {
	apiKey string // Optional: empty for free tier, or set for paid models
	model  string
}

func NewZenProvider(apiKey, model string) *ZenProvider {
	return &ZenProvider{
		apiKey: apiKey,
		model:  model,
	}
}

func (p *ZenProvider) Name() string {
	return fmt.Sprintf("OpenCode Zen (%s)", p.model)
}

// OpenAI-compatible request/response structures
type zenChatRequest struct {
	Model    string       `json:"model"`
	Messages []zenMessage `json:"messages"`
	Tools    []zenTool    `json:"tools,omitempty"`
	Stream   bool         `json:"stream,omitempty"`
}

type zenMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCalls  []zenToolCall `json:"tool_calls,omitempty"`
}

type zenTool struct {
	Type     string      `json:"type"`
	Function zenFunction `json:"function"`
}

type zenFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type zenToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type zenChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Model   string       `json:"model"`
	Choices []zenChoice  `json:"choices"`
	Usage   *zenUsage    `json:"usage,omitempty"`
	Error   *zenAPIError `json:"error,omitempty"`
}

type zenChoice struct {
	Index        int         `json:"index"`
	Message      *zenMessage `json:"message,omitempty"`
	Delta        *zenMessage `json:"delta,omitempty"`
	FinishReason string      `json:"finish_reason"`
}

type zenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type zenAPIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (p *ZenProvider) makeRequest(ctx context.Context, req zenChatRequest) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", zenBaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	return http.DefaultClient.Do(httpReq)
}

func (p *ZenProvider) SuggestCommands(ctx context.Context, req SuggestRequest) ([]CommandSuggestion, error) {
	numSuggestions := req.NumSuggestions
	if numSuggestions <= 0 {
		numSuggestions = 3
	}

	// Define the function schema for structured output
	schemaMap := prompt.SuggestSchema(numSuggestions)
	schema, err := json.Marshal(schemaMap)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal schema: %w", err)
	}

	systemPrompt := prompt.SuggestSystemPrompt(req.Shell, req.Instructions, numSuggestions, req.EnableSearch)
	userPrompt := prompt.SuggestUserPrompt(req.UserInput, req.Files, req.Stdin)

	chatReq := zenChatRequest{
		Model: p.model,
		Messages: []zenMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Tools: []zenTool{
			{
				Type: "function",
				Function: zenFunction{
					Name:        "suggest_commands",
					Description: "Suggest shell commands based on user input",
					Parameters:  schema,
				},
			},
		},
	}

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: OpenCode Zen Request ===")
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "Tools: suggest_commands\n")
		fmt.Fprintf(os.Stderr, "System:\n%s\n", systemPrompt)
		fmt.Fprintf(os.Stderr, "User:\n%s\n", userPrompt)
		fmt.Fprintln(os.Stderr, "====================================")
	}

	resp, err := p.makeRequest(ctx, chatReq)
	if err != nil {
		return nil, fmt.Errorf("zen API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("zen API error (status %d): %s", resp.StatusCode, string(body))
	}

	var chatResp zenChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w\nBody: %s", err, string(body))
	}

	if chatResp.Error != nil {
		return nil, fmt.Errorf("zen API error: %s", chatResp.Error.Message)
	}

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: OpenCode Zen Response ===")
		fmt.Fprintf(os.Stderr, "Model: %s\n", chatResp.Model)
		if len(chatResp.Choices) > 0 && chatResp.Choices[0].Message != nil {
			msg := chatResp.Choices[0].Message
			if len(msg.ToolCalls) > 0 {
				for _, tc := range msg.ToolCalls {
					fmt.Fprintf(os.Stderr, "Function: %s\n", tc.Function.Name)
					fmt.Fprintf(os.Stderr, "Arguments: %s\n", tc.Function.Arguments)
				}
			} else if msg.Content != "" {
				fmt.Fprintf(os.Stderr, "Content: %s\n", msg.Content)
			}
		}
		fmt.Fprintln(os.Stderr, "=====================================")
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	msg := chatResp.Choices[0].Message
	if msg == nil {
		return nil, fmt.Errorf("no message in response")
	}

	// Extract suggestions from tool call
	for _, tc := range msg.ToolCalls {
		if tc.Function.Name == "suggest_commands" {
			var result suggestionsResponse
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &result); err != nil {
				return nil, fmt.Errorf("failed to parse suggestions: %w", err)
			}
			return result.Suggestions, nil
		}
	}

	return nil, fmt.Errorf("no suggest_commands function call in response")
}

// GetEdits calls the LLM with the edit tool and returns all proposed edits
func (p *ZenProvider) GetEdits(ctx context.Context, systemPrompt, userPrompt string, debug bool) ([]EditToolCall, error) {
	// Define the edit tool schema
	editSchema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"file_path": map[string]interface{}{
				"type":        "string",
				"description": "Path to the file to edit",
			},
			"old_string": map[string]interface{}{
				"type":        "string",
				"description": "The exact text to find and replace. Include enough context to be unique.",
			},
			"new_string": map[string]interface{}{
				"type":        "string",
				"description": "The text to replace old_string with",
			},
		},
		"required":             []string{"file_path", "old_string", "new_string"},
		"additionalProperties": false,
	}

	schema, err := json.Marshal(editSchema)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal schema: %w", err)
	}

	if debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: OpenCode Zen Edit Request ===")
		fmt.Fprintf(os.Stderr, "System: %s\n", systemPrompt)
		fmt.Fprintf(os.Stderr, "User: %s\n", userPrompt)
		fmt.Fprintln(os.Stderr, "=========================================")
	}

	chatReq := zenChatRequest{
		Model: p.model,
		Messages: []zenMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Tools: []zenTool{
			{
				Type: "function",
				Function: zenFunction{
					Name:        "edit",
					Description: "Edit a file by replacing old_string with new_string. Use multiple tool calls for multiple edits.",
					Parameters:  schema,
				},
			},
		},
	}

	resp, err := p.makeRequest(ctx, chatReq)
	if err != nil {
		return nil, fmt.Errorf("zen API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("zen API error (status %d): %s", resp.StatusCode, string(body))
	}

	var chatResp zenChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w\nBody: %s", err, string(body))
	}

	if chatResp.Error != nil {
		return nil, fmt.Errorf("zen API error: %s", chatResp.Error.Message)
	}

	if debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: OpenCode Zen Edit Response ===")
		fmt.Fprintf(os.Stderr, "Model: %s\n", chatResp.Model)
		if len(chatResp.Choices) > 0 && chatResp.Choices[0].Message != nil {
			msg := chatResp.Choices[0].Message
			if len(msg.ToolCalls) > 0 {
				for _, tc := range msg.ToolCalls {
					fmt.Fprintf(os.Stderr, "Function: %s\n", tc.Function.Name)
					fmt.Fprintf(os.Stderr, "Arguments: %s\n", tc.Function.Arguments)
				}
			} else if msg.Content != "" {
				fmt.Fprintf(os.Stderr, "Content: %s\n", msg.Content)
			}
		}
		fmt.Fprintln(os.Stderr, "==========================================")
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	msg := chatResp.Choices[0].Message
	if msg == nil {
		return nil, fmt.Errorf("no message in response")
	}

	// Print any text content first
	if msg.Content != "" {
		fmt.Println(msg.Content)
	}

	// Collect all edits from tool calls
	var edits []EditToolCall
	for _, tc := range msg.ToolCalls {
		if tc.Function.Name != "edit" {
			continue
		}

		var editCall EditToolCall
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &editCall); err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing edit: %v\n", err)
			continue
		}

		edits = append(edits, editCall)
	}

	return edits, nil
}

func (p *ZenProvider) StreamResponse(ctx context.Context, req AskRequest, output chan<- string) error {
	defer close(output)

	userMessage := prompt.AskUserPrompt(req.Question, req.Files, req.Stdin)

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: OpenCode Zen Stream Request ===")
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "Question: %s\n", req.Question)
		fmt.Fprintln(os.Stderr, "===========================================")
	}

	messages := []zenMessage{
		{Role: "user", Content: userMessage},
	}

	if req.Instructions != "" {
		messages = []zenMessage{
			{Role: "system", Content: req.Instructions},
			{Role: "user", Content: userMessage},
		}
	}

	chatReq := zenChatRequest{
		Model:    p.model,
		Messages: messages,
		Stream:   true,
	}

	resp, err := p.makeRequest(ctx, chatReq)
	if err != nil {
		return fmt.Errorf("zen API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("zen API error (status %d): %s", resp.StatusCode, string(body))
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chatResp zenChatResponse
		if err := json.Unmarshal([]byte(data), &chatResp); err != nil {
			continue
		}

		if len(chatResp.Choices) > 0 && chatResp.Choices[0].Delta != nil {
			content := chatResp.Choices[0].Delta.Content
			if content != "" {
				output <- content
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("zen streaming error: %w", err)
	}

	return nil
}

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

// OpenAICompatProvider implements Provider for OpenAI-compatible APIs
// Used by Ollama, LM Studio, and other compatible servers
type OpenAICompatProvider struct {
	baseURL string
	apiKey  string // Optional, most servers ignore it
	model   string
	name    string // Display name: "Ollama", "LM Studio", etc.
}

// truncate shortens a string for debug output
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func NewOpenAICompatProvider(baseURL, apiKey, model, name string) *OpenAICompatProvider {
	// Ensure baseURL doesn't have trailing slash
	baseURL = strings.TrimSuffix(baseURL, "/")
	return &OpenAICompatProvider{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		name:    name,
	}
}

func (p *OpenAICompatProvider) Name() string {
	return fmt.Sprintf("%s (%s)", p.name, p.model)
}

// OpenAI-compatible request/response structures
type oaiChatRequest struct {
	Model    string       `json:"model"`
	Messages []oaiMessage `json:"messages"`
	Tools    []oaiTool    `json:"tools,omitempty"`
	Stream   bool         `json:"stream,omitempty"`
}

type oaiMessage struct {
	Role      string        `json:"role"`
	Content   string        `json:"content,omitempty"`
	ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`
}

type oaiTool struct {
	Type     string      `json:"type"`
	Function oaiFunction `json:"function"`
}

type oaiFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Model   string       `json:"model"`
	Choices []oaiChoice  `json:"choices"`
	Usage   *oaiUsage    `json:"usage,omitempty"`
	Error   *oaiAPIError `json:"error,omitempty"`
}

type oaiChoice struct {
	Index        int         `json:"index"`
	Message      *oaiMessage `json:"message,omitempty"`
	Delta        *oaiMessage `json:"delta,omitempty"`
	FinishReason string      `json:"finish_reason"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type oaiAPIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Model listing structures
type oaiModelsResponse struct {
	Data []oaiModel `json:"data"`
}

type oaiModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelInfo represents a model available from a provider
type ModelInfo struct {
	ID          string
	DisplayName string // Human-readable name (Anthropic)
	Created     int64
	OwnedBy     string
}

func (p *OpenAICompatProvider) makeRequest(ctx context.Context, method, endpoint string, body []byte) (*http.Response, error) {
	url := p.baseURL + endpoint

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	return http.DefaultClient.Do(httpReq)
}

func (p *OpenAICompatProvider) makeChatRequest(ctx context.Context, req oaiChatRequest) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return p.makeRequest(ctx, "POST", "/chat/completions", body)
}

// ListModels returns available models from the server
func (p *OpenAICompatProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	resp, err := p.makeRequest(ctx, "GET", "/models", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	var modelsResp oaiModelsResponse
	if err := json.Unmarshal(body, &modelsResp); err != nil {
		return nil, fmt.Errorf("failed to parse models response: %w", err)
	}

	models := make([]ModelInfo, len(modelsResp.Data))
	for i, m := range modelsResp.Data {
		models[i] = ModelInfo{
			ID:      m.ID,
			Created: m.Created,
			OwnedBy: m.OwnedBy,
		}
	}

	return models, nil
}

func (p *OpenAICompatProvider) SuggestCommands(ctx context.Context, req SuggestRequest) ([]CommandSuggestion, error) {
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

	chatReq := oaiChatRequest{
		Model: p.model,
		Messages: []oaiMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Tools: []oaiTool{
			{
				Type: "function",
				Function: oaiFunction{
					Name:        "suggest_commands",
					Description: "Suggest shell commands based on user input",
					Parameters:  schema,
				},
			},
		},
	}

	if req.Debug {
		fmt.Fprintf(os.Stderr, "=== DEBUG: %s Request ===\n", p.name)
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "URL: %s/chat/completions\n", p.baseURL)
		fmt.Fprintf(os.Stderr, "Tools: suggest_commands\n")
		fmt.Fprintf(os.Stderr, "System:\n%s\n", systemPrompt)
		fmt.Fprintf(os.Stderr, "User:\n%s\n", userPrompt)
		fmt.Fprintln(os.Stderr, "====================================")
	}

	resp, err := p.makeChatRequest(ctx, chatReq)
	if err != nil {
		return nil, fmt.Errorf("%s API request failed: %w", p.name, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s API error (status %d): %s", p.name, resp.StatusCode, string(body))
	}

	var chatResp oaiChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w\nBody: %s", err, string(body))
	}

	if chatResp.Error != nil {
		return nil, fmt.Errorf("%s API error: %s", p.name, chatResp.Error.Message)
	}

	if req.Debug {
		fmt.Fprintf(os.Stderr, "=== DEBUG: %s Response ===\n", p.name)
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
func (p *OpenAICompatProvider) GetEdits(ctx context.Context, systemPrompt, userPrompt string, debug bool) ([]EditToolCall, error) {
	// Define the edit tool using centralized schema
	schema, err := json.Marshal(prompt.EditSchema())
	if err != nil {
		return nil, fmt.Errorf("failed to marshal schema: %w", err)
	}

	if debug {
		fmt.Fprintf(os.Stderr, "=== DEBUG: %s Edit Request ===\n", p.name)
		fmt.Fprintf(os.Stderr, "System: %s\n", systemPrompt)
		fmt.Fprintf(os.Stderr, "User: %s\n", userPrompt)
		fmt.Fprintln(os.Stderr, "=========================================")
	}

	chatReq := oaiChatRequest{
		Model: p.model,
		Messages: []oaiMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Tools: []oaiTool{
			{
				Type: "function",
				Function: oaiFunction{
					Name:        "edit",
					Description: prompt.EditDescription,
					Parameters:  schema,
				},
			},
		},
	}

	resp, err := p.makeChatRequest(ctx, chatReq)
	if err != nil {
		return nil, fmt.Errorf("%s API request failed: %w", p.name, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s API error (status %d): %s", p.name, resp.StatusCode, string(body))
	}

	var chatResp oaiChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w\nBody: %s", err, string(body))
	}

	if chatResp.Error != nil {
		return nil, fmt.Errorf("%s API error: %s", p.name, chatResp.Error.Message)
	}

	if debug {
		fmt.Fprintf(os.Stderr, "=== DEBUG: %s Edit Response ===\n", p.name)
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

// GetUnifiedDiff calls the LLM with the unified_diff tool and returns the diff string.
func (p *OpenAICompatProvider) GetUnifiedDiff(ctx context.Context, systemPrompt, userPrompt string, debug bool) (string, error) {
	// Define the unified diff tool using centralized schema
	schema, err := json.Marshal(prompt.UnifiedDiffSchema())
	if err != nil {
		return "", fmt.Errorf("failed to marshal schema: %w", err)
	}

	if debug {
		fmt.Fprintf(os.Stderr, "=== DEBUG: %s UnifiedDiff Request ===\n", p.name)
		fmt.Fprintf(os.Stderr, "System: %s\n", systemPrompt)
		fmt.Fprintf(os.Stderr, "User: %s\n", userPrompt)
		fmt.Fprintln(os.Stderr, "=========================================")
	}

	chatReq := oaiChatRequest{
		Model: p.model,
		Messages: []oaiMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Tools: []oaiTool{
			{
				Type: "function",
				Function: oaiFunction{
					Name:        "unified_diff",
					Description: prompt.UnifiedDiffDescription,
					Parameters:  schema,
				},
			},
		},
	}

	resp, err := p.makeChatRequest(ctx, chatReq)
	if err != nil {
		return "", fmt.Errorf("%s API request failed: %w", p.name, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%s API error (status %d): %s", p.name, resp.StatusCode, string(body))
	}

	var chatResp oaiChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w\nBody: %s", err, string(body))
	}

	if chatResp.Error != nil {
		return "", fmt.Errorf("%s API error: %s", p.name, chatResp.Error.Message)
	}

	if debug {
		fmt.Fprintf(os.Stderr, "=== DEBUG: %s UnifiedDiff Response ===\n", p.name)
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
		return "", fmt.Errorf("no choices in response")
	}

	msg := chatResp.Choices[0].Message
	if msg == nil {
		return "", fmt.Errorf("no message in response")
	}

	// Print any text content first
	if msg.Content != "" {
		fmt.Println(msg.Content)
	}

	// Find the unified_diff function call
	for _, tc := range msg.ToolCalls {
		if tc.Function.Name == "unified_diff" {
			var result struct {
				Diff string `json:"diff"`
			}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &result); err != nil {
				return "", fmt.Errorf("failed to parse unified_diff response: %w", err)
			}
			return result.Diff, nil
		}
	}

	return "", fmt.Errorf("no unified_diff function call in response")
}

func (p *OpenAICompatProvider) StreamResponse(ctx context.Context, req AskRequest, output chan<- string) error {
	defer close(output)

	userMessage := prompt.AskUserPrompt(req.Question, req.Files, req.Stdin)

	if req.Debug {
		fmt.Fprintf(os.Stderr, "=== DEBUG: %s Stream Request ===\n", p.name)
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "URL: %s/chat/completions\n", p.baseURL)
		fmt.Fprintf(os.Stderr, "Question: %s\n", req.Question)
		fmt.Fprintf(os.Stderr, "Files: %d\n", len(req.Files))
		fmt.Fprintf(os.Stderr, "User message length: %d chars\n", len(userMessage))
		fmt.Fprintln(os.Stderr, "===========================================")
	}

	messages := []oaiMessage{
		{Role: "user", Content: userMessage},
	}

	if req.Instructions != "" {
		messages = []oaiMessage{
			{Role: "system", Content: req.Instructions},
			{Role: "user", Content: userMessage},
		}
	}

	chatReq := oaiChatRequest{
		Model:    p.model,
		Messages: messages,
		Stream:   true,
	}

	resp, err := p.makeChatRequest(ctx, chatReq)
	if err != nil {
		return fmt.Errorf("%s API request failed: %w", p.name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s API error (status %d): %s", p.name, resp.StatusCode, string(body))
	}

	scanner := bufio.NewScanner(resp.Body)
	// Increase buffer for large responses
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	var lastEventType string
	for scanner.Scan() {
		line := scanner.Text()

		// Track SSE event type (some servers send "event: error" before error data)
		if strings.HasPrefix(line, "event: ") {
			lastEventType = strings.TrimPrefix(line, "event: ")
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chatResp oaiChatResponse
		if err := json.Unmarshal([]byte(data), &chatResp); err != nil {
			continue
		}

		// Check for error in response (either from event type or error field)
		if lastEventType == "error" || chatResp.Error != nil {
			errMsg := "unknown error"
			if chatResp.Error != nil {
				errMsg = chatResp.Error.Message
			}
			return fmt.Errorf("%s API error: %s", p.name, errMsg)
		}

		if len(chatResp.Choices) > 0 && chatResp.Choices[0].Delta != nil {
			content := chatResp.Choices[0].Delta.Content
			if content != "" {
				output <- content
			}
		}

		lastEventType = "" // Reset after processing data
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s streaming error: %w", p.name, err)
	}

	return nil
}

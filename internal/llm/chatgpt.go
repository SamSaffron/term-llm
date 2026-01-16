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
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/oauth"
)

const chatGPTDefaultModel = "gpt-5.2-codex"

// chatGPTHTTPTimeout is the timeout for ChatGPT HTTP requests
const chatGPTHTTPTimeout = 10 * time.Minute

// chatGPTHTTPClient is a shared HTTP client with reasonable timeouts
var chatGPTHTTPClient = &http.Client{
	Timeout: chatGPTHTTPTimeout,
}

// ChatGPTProvider implements Provider using the ChatGPT backend API with native OAuth.
type ChatGPTProvider struct {
	creds  *credentials.ChatGPTCredentials
	model  string
	effort string // reasoning effort: "low", "medium", "high", "xhigh", or ""
}

// NewChatGPTProvider creates a new ChatGPT provider.
// If credentials are not available or expired, it will prompt the user to authenticate.
func NewChatGPTProvider(model string) (*ChatGPTProvider, error) {
	if model == "" {
		model = chatGPTDefaultModel
	}
	actualModel, effort := parseModelEffort(model)

	// Try to load existing credentials
	creds, err := credentials.GetChatGPTCredentials()
	if err != nil {
		// No credentials - prompt user to authenticate
		creds, err = promptForChatGPTAuth()
		if err != nil {
			return nil, err
		}
	}

	// Refresh if expired
	if creds.IsExpired() {
		if err := credentials.RefreshChatGPTCredentials(creds); err != nil {
			// Refresh failed - need to re-authenticate
			fmt.Println("Token refresh failed. Re-authentication required.")
			creds, err = promptForChatGPTAuth()
			if err != nil {
				return nil, err
			}
		}
	}

	return &ChatGPTProvider{
		creds:  creds,
		model:  actualModel,
		effort: effort,
	}, nil
}

// NewChatGPTProviderWithCreds creates a ChatGPT provider with pre-loaded credentials.
// This is used by the factory when credentials are already resolved.
func NewChatGPTProviderWithCreds(creds *credentials.ChatGPTCredentials, model string) *ChatGPTProvider {
	if model == "" {
		model = chatGPTDefaultModel
	}
	actualModel, effort := parseModelEffort(model)
	return &ChatGPTProvider{
		creds:  creds,
		model:  actualModel,
		effort: effort,
	}
}

// promptForChatGPTAuth prompts the user to authenticate with ChatGPT
func promptForChatGPTAuth() (*credentials.ChatGPTCredentials, error) {
	fmt.Println("ChatGPT provider requires authentication.")
	fmt.Print("Press Enter to open browser and sign in with your ChatGPT account...")

	reader := bufio.NewReader(os.Stdin)
	reader.ReadString('\n')

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	oauthCreds, err := oauth.AuthenticateChatGPT(ctx)
	if err != nil {
		return nil, fmt.Errorf("authentication failed: %w", err)
	}

	// Convert oauth credentials to stored credentials format
	creds := &credentials.ChatGPTCredentials{
		AccessToken:  oauthCreds.AccessToken,
		RefreshToken: oauthCreds.RefreshToken,
		ExpiresAt:    oauthCreds.ExpiresAt,
		AccountID:    oauthCreds.AccountID,
	}

	// Save credentials
	if err := credentials.SaveChatGPTCredentials(creds); err != nil {
		return nil, fmt.Errorf("failed to save credentials: %w", err)
	}

	fmt.Println("Authentication successful!")
	return creds, nil
}

func (p *ChatGPTProvider) Name() string {
	if p.effort != "" {
		return fmt.Sprintf("ChatGPT (%s, effort=%s)", p.model, p.effort)
	}
	return fmt.Sprintf("ChatGPT (%s)", p.model)
}

func (p *ChatGPTProvider) Credential() string {
	return "chatgpt"
}

func (p *ChatGPTProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch: true,
		NativeWebFetch:  false,
		ToolCalls:       true,
	}
}

func (p *ChatGPTProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	// Check and refresh token if needed
	if p.creds.IsExpired() {
		if err := credentials.RefreshChatGPTCredentials(p.creds); err != nil {
			return nil, fmt.Errorf("token refresh failed: %w (re-run with --provider chatgpt to re-authenticate)", err)
		}
	}

	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		// Get Codex instructions (same as codex provider)
		codexInstructions, err := (&CodexProvider{model: p.model}).getCodexInstructions()
		if err != nil {
			return fmt.Errorf("failed to get Codex instructions: %w", err)
		}

		system, user := flattenSystemUser(req.Messages)
		combinedPrompt := strings.TrimSpace(strings.Join([]string{system, user}, "\n\n"))
		if combinedPrompt == "" {
			return fmt.Errorf("no prompt content provided")
		}

		tools := []interface{}{}
		if req.Search {
			tools = append(tools, map[string]interface{}{"type": "web_search"})
		}
		for _, spec := range req.Tools {
			tools = append(tools, map[string]interface{}{
				"type":        "function",
				"name":        spec.Name,
				"description": spec.Description,
				"strict":      true,
				"parameters":  normalizeSchemaForOpenAI(spec.Schema),
			})
		}

		// Strip effort suffix from req.Model if present
		reqModel, reqEffort := parseModelEffort(req.Model)
		model := chooseModel(reqModel, p.model)
		effort := p.effort
		if effort == "" && reqEffort != "" {
			effort = reqEffort
		}

		reqBody := map[string]interface{}{
			"model":               model,
			"instructions":        codexInstructions,
			"input":               p.buildInput(combinedPrompt),
			"tools":               tools,
			"tool_choice":         "auto",
			"parallel_tool_calls": req.ParallelToolCalls,
			"stream":              true,
			"store":               false,
			"include":             []string{},
		}

		if effort != "" {
			reqBody["reasoning"] = map[string]interface{}{
				"effort": effort,
			}
		}

		body, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("failed to marshal request: %w", err)
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", chatGPTResponsesURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}

		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+p.creds.AccessToken)
		httpReq.Header.Set("ChatGPT-Account-ID", p.creds.AccountID)
		httpReq.Header.Set("OpenAI-Beta", "responses=experimental")
		httpReq.Header.Set("originator", "term-llm")
		httpReq.Header.Set("Accept", "text/event-stream")

		resp, err := chatGPTHTTPClient.Do(httpReq)
		if err != nil {
			return fmt.Errorf("request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("API error (%d): %s", resp.StatusCode, string(respBody))
		}

		// Stream and handle both text and tool calls (same logic as codex provider)
		acc := newCodexToolAccumulator()
		var lastUsage *Usage
		buf := make([]byte, 4096)
		var pending string
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				pending += string(buf[:n])
				for {
					idx := strings.Index(pending, "\n")
					if idx < 0 {
						break
					}
					line := pending[:idx]
					pending = pending[idx+1:]
					if !strings.HasPrefix(line, "data:") {
						continue
					}
					jsonData := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
					if jsonData == "" || jsonData == "[DONE]" {
						continue
					}
					if req.DebugRaw {
						DebugRawSection(req.DebugRaw, "ChatGPT SSE Line", jsonData)
					}

					var event codexSSEEvent
					if json.Unmarshal([]byte(jsonData), &event) != nil {
						continue
					}

					switch event.Type {
					case "response.output_text.delta":
						if event.Delta != "" {
							events <- Event{Type: EventTextDelta, Text: event.Delta}
						}
					case "response.output_item.added":
						switch event.Item.Type {
						case "web_search_call":
							events <- Event{Type: EventToolExecStart, ToolName: "web_search"}
						case "function_call":
							id := event.Item.ID
							if id == "" {
								id = event.Item.CallID
							}
							call := ToolCall{
								ID:        id,
								Name:      event.Item.Name,
								Arguments: json.RawMessage(event.Item.Arguments),
							}
							acc.setCall(call)
							if event.Item.Arguments != "" {
								acc.setArgs(id, event.Item.Arguments)
							}
						}
					case "response.output_item.done":
						switch event.Item.Type {
						case "web_search_call":
							events <- Event{Type: EventToolExecEnd, ToolName: "web_search", ToolSuccess: true}
						case "function_call":
							id := event.Item.ID
							if id == "" {
								id = event.Item.CallID
							}
							call := ToolCall{
								ID:        id,
								Name:      event.Item.Name,
								Arguments: json.RawMessage(event.Item.Arguments),
							}
							acc.setCall(call)
							if event.Item.Arguments != "" {
								acc.setArgs(id, event.Item.Arguments)
							}
						}
					case "response.function_call_arguments.delta":
						acc.ensureCall(event.ItemID)
						acc.appendArgs(event.ItemID, event.Delta)
					case "response.function_call_arguments.done":
						acc.ensureCall(event.ItemID)
						acc.setArgs(event.ItemID, event.Arguments)
					case "response.completed":
						if event.Response.Usage.OutputTokens > 0 {
							lastUsage = &Usage{
								InputTokens:       event.Response.Usage.InputTokens,
								OutputTokens:      event.Response.Usage.OutputTokens,
								CachedInputTokens: event.Response.Usage.InputTokensDetails.CachedTokens,
							}
						}
					}
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("stream read error: %w", err)
			}
		}

		// Emit any tool calls that were accumulated
		for _, call := range acc.finalize() {
			events <- Event{Type: EventToolCall, Tool: &call}
		}

		if lastUsage != nil {
			events <- Event{Type: EventUsage, Use: lastUsage}
		}
		events <- Event{Type: EventDone}
		return nil
	}), nil
}

func (p *ChatGPTProvider) buildInput(userPrompt string) []map[string]interface{} {
	return []map[string]interface{}{
		{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": userPrompt},
			},
		},
	}
}

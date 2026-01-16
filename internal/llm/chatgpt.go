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

// chatGPTResponsesURL is the ChatGPT backend API endpoint for responses
const chatGPTResponsesURL = "https://chatgpt.com/backend-api/codex/responses"

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
		// Build structured input from conversation history
		system, inputItems := buildChatGPTInput(req.Messages)
		if system == "" && len(inputItems) == 0 {
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
			"instructions":        system,
			"input":               inputItems,
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

		if req.DebugRaw {
			var prettyBody bytes.Buffer
			json.Indent(&prettyBody, body, "", "  ")
			DebugRawSection(req.DebugRaw, "ChatGPT Request", prettyBody.String())
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
			if req.DebugRaw {
				var debugInfo strings.Builder
				debugInfo.WriteString(fmt.Sprintf("Status: %d %s\n", resp.StatusCode, resp.Status))
				debugInfo.WriteString("Headers:\n")
				for key, values := range resp.Header {
					for _, value := range values {
						debugInfo.WriteString(fmt.Sprintf("  %s: %s\n", key, value))
					}
				}
				debugInfo.WriteString("Body:\n")
				// Try to pretty-print JSON body
				var prettyBody bytes.Buffer
				if json.Indent(&prettyBody, respBody, "", "  ") == nil {
					debugInfo.WriteString(prettyBody.String())
				} else {
					debugInfo.WriteString(string(respBody))
				}
				DebugRawSection(req.DebugRaw, "ChatGPT Error Response", debugInfo.String())
			}
			return fmt.Errorf("API error (%d): %s", resp.StatusCode, string(respBody))
		}

		// Stream and handle both text and tool calls
		acc := newChatGPTToolAccumulator()
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

					var event chatGPTSSEEvent
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

// buildChatGPTInput converts Messages to the ChatGPT Responses API input format.
// Returns the system instructions string and the input array.
func buildChatGPTInput(messages []Message) (string, []interface{}) {
	var systemParts []string
	var input []interface{}

	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			// Collect system messages into instructions
			text := collectTextParts(msg.Parts)
			if text != "" {
				systemParts = append(systemParts, text)
			}

		case RoleUser:
			text := collectTextParts(msg.Parts)
			if text != "" {
				input = append(input, map[string]interface{}{
					"type": "message",
					"role": "user",
					"content": []map[string]string{
						{"type": "input_text", "text": text},
					},
				})
			}

		case RoleAssistant:
			// Collect text content from parts
			var textContent string
			var toolCalls []ToolCall

			for _, part := range msg.Parts {
				switch part.Type {
				case PartText:
					if part.Text != "" {
						if textContent != "" {
							textContent += "\n"
						}
						textContent += part.Text
					}
				case PartToolCall:
					if part.ToolCall != nil {
						toolCalls = append(toolCalls, *part.ToolCall)
					}
				}
			}

			// Add assistant message with content if present
			if textContent != "" {
				input = append(input, map[string]interface{}{
					"type": "message",
					"role": "assistant",
					"content": []map[string]string{
						{"type": "output_text", "text": textContent},
					},
				})
			}

			// Add function_call items for tool calls
			for _, tc := range toolCalls {
				input = append(input, map[string]interface{}{
					"type":      "function_call",
					"id":        tc.ID,
					"call_id":   tc.ID,
					"name":      tc.Name,
					"arguments": string(tc.Arguments),
				})
			}

		case RoleTool:
			// Tool results as function_call_output
			for _, part := range msg.Parts {
				if part.Type != PartToolResult || part.ToolResult == nil {
					continue
				}
				callID := strings.TrimSpace(part.ToolResult.ID)
				if callID == "" {
					continue
				}
				input = append(input, map[string]interface{}{
					"type":    "function_call_output",
					"call_id": callID,
					"output":  part.ToolResult.Content,
				})
			}
		}
	}

	return strings.Join(systemParts, "\n\n"), input
}

// chatGPTSSEEvent represents a Server-Sent Event from the ChatGPT API
type chatGPTSSEEvent struct {
	Type string `json:"type"`
	Item struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	ItemID    string `json:"item_id"`
	Delta     string `json:"delta"`
	Arguments string `json:"arguments"`
	Response  struct {
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	} `json:"response"`
}

// chatGPTToolAccumulator accumulates streaming tool calls
type chatGPTToolAccumulator struct {
	order    []string
	calls    map[string]ToolCall
	partials map[string]*strings.Builder
	final    map[string]string
}

func newChatGPTToolAccumulator() *chatGPTToolAccumulator {
	return &chatGPTToolAccumulator{
		calls:    make(map[string]ToolCall),
		partials: make(map[string]*strings.Builder),
		final:    make(map[string]string),
	}
}

func (a *chatGPTToolAccumulator) ensureCall(id string) {
	if id == "" {
		return
	}
	if _, ok := a.calls[id]; ok {
		return
	}
	a.calls[id] = ToolCall{ID: id}
	a.order = append(a.order, id)
}

func (a *chatGPTToolAccumulator) setCall(call ToolCall) {
	if call.ID == "" {
		return
	}
	if _, ok := a.calls[call.ID]; !ok {
		a.order = append(a.order, call.ID)
	}
	a.calls[call.ID] = call
}

func (a *chatGPTToolAccumulator) appendArgs(id, delta string) {
	if id == "" || delta == "" {
		return
	}
	if a.final[id] != "" {
		return
	}
	builder := a.partials[id]
	if builder == nil {
		builder = &strings.Builder{}
		a.partials[id] = builder
	}
	builder.WriteString(delta)
}

func (a *chatGPTToolAccumulator) setArgs(id, args string) {
	if id == "" || args == "" {
		return
	}
	a.final[id] = args
	delete(a.partials, id)
}

func (a *chatGPTToolAccumulator) finalize() []ToolCall {
	out := make([]ToolCall, 0, len(a.order))
	for _, id := range a.order {
		call, ok := a.calls[id]
		if !ok {
			continue
		}
		if args := a.final[id]; args != "" {
			call.Arguments = json.RawMessage(args)
		} else if builder := a.partials[id]; builder != nil && builder.Len() > 0 {
			call.Arguments = json.RawMessage(builder.String())
		}
		out = append(out, call)
	}
	return out
}

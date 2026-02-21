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
			"include":             []string{"reasoning.encrypted_content"},
		}
		if req.SessionID != "" {
			reqBody["prompt_cache_key"] = req.SessionID
		}

		reasoning := map[string]interface{}{
			"summary": "auto",
		}
		if effort != "" {
			reasoning["effort"] = effort
		}
		reqBody["reasoning"] = reasoning

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
		if req.SessionID != "" {
			httpReq.Header.Set("session_id", req.SessionID)
		}

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

			// Handle rate limits with a friendly message
			if resp.StatusCode == http.StatusTooManyRequests {
				return parseChatGPTRateLimitError(respBody, resp.Header)
			}

			return fmt.Errorf("API error (%d): %s", resp.StatusCode, string(respBody))
		}

		// Stream and handle both text and tool calls
		acc := newChatGPTToolAccumulator()
		reasoningAcc := newChatGPTReasoningAccumulator()
		var lastUsage *Usage
		scanner := bufio.NewScanner(resp.Body)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 10*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
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
				case "reasoning":
					id := event.Item.ID
					if id == "" {
						id = event.ItemID
					}
					reasoningAcc.start(id, event.OutputIndex, event.Item.EncryptedContent, event.Item.Summary)
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
				case "reasoning":
					id := event.Item.ID
					if id == "" {
						id = event.ItemID
					}
					reasoningAcc.finish(id, event.OutputIndex, event.Item.EncryptedContent, event.Item.Summary)
					if part := reasoningAcc.part(id, event.OutputIndex); part != nil {
						events <- Event{
							Type:                      EventReasoningDelta,
							Text:                      part.ReasoningContent,
							ReasoningItemID:           part.ReasoningItemID,
							ReasoningEncryptedContent: part.ReasoningEncryptedContent,
						}
					}
				}
			case "response.function_call_arguments.delta":
				acc.ensureCall(event.ItemID)
				acc.appendArgs(event.ItemID, event.Delta)
			case "response.function_call_arguments.done":
				acc.ensureCall(event.ItemID)
				acc.setArgs(event.ItemID, event.Arguments)
			case "response.reasoning_summary_part.added":
				reasoningAcc.ensure(event.ItemID, event.OutputIndex)
			case "response.reasoning_summary_text.delta":
				reasoningAcc.appendSummary(event.ItemID, event.OutputIndex, event.Delta)
			case "response.completed":
				if event.Response.Usage.InputTokens > 0 ||
					event.Response.Usage.OutputTokens > 0 ||
					event.Response.Usage.InputTokensDetails.CachedTokens > 0 {
					lastUsage = &Usage{
						InputTokens:       event.Response.Usage.InputTokens,
						OutputTokens:      event.Response.Usage.OutputTokens,
						CachedInputTokens: event.Response.Usage.InputTokensDetails.CachedTokens,
					}
				}
			}
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("stream read error: %w", err)
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
			var imageParts []map[string]interface{}
			for _, part := range msg.Parts {
				if part.Type == PartImage && part.ImageData != nil {
					dataURL := fmt.Sprintf("data:%s;base64,%s", part.ImageData.MediaType, part.ImageData.Base64)
					imageParts = append(imageParts, map[string]interface{}{
						"type":      "input_image",
						"image_url": dataURL,
					})
				}
			}
			if text == "" && len(imageParts) == 0 {
				continue
			}
			var content []map[string]interface{}
			if text != "" {
				content = append(content, map[string]interface{}{
					"type": "input_text",
					"text": text,
				})
			}
			content = append(content, imageParts...)
			input = append(input, map[string]interface{}{
				"type":    "message",
				"role":    "user",
				"content": content,
			})

		case RoleAssistant:
			var textContent strings.Builder

			flushAssistantText := func() {
				if textContent.Len() == 0 {
					return
				}
				input = append(input, map[string]interface{}{
					"type": "message",
					"role": "assistant",
					"content": []map[string]string{
						{"type": "output_text", "text": textContent.String()},
					},
				})
				textContent.Reset()
			}

			for _, part := range msg.Parts {
				switch part.Type {
				case PartText:
					if hasChatGPTReasoningReplay(part) {
						flushAssistantText()
						input = append(input, buildChatGPTReasoningInputItem(part))
					}
					if part.Text != "" {
						if textContent.Len() > 0 {
							textContent.WriteString("\n")
						}
						textContent.WriteString(part.Text)
					}
				case PartToolCall:
					if part.ToolCall == nil {
						continue
					}
					flushAssistantText()
					args := strings.TrimSpace(string(part.ToolCall.Arguments))
					if args == "" {
						args = "{}"
					}
					input = append(input, map[string]interface{}{
						"type":      "function_call",
						"id":        part.ToolCall.ID,
						"call_id":   part.ToolCall.ID,
						"name":      part.ToolCall.Name,
						"arguments": args,
					})
				}
			}
			flushAssistantText()

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
		Type             string                          `json:"type"`
		ID               string                          `json:"id"`
		CallID           string                          `json:"call_id"`
		Name             string                          `json:"name"`
		Arguments        string                          `json:"arguments"`
		EncryptedContent string                          `json:"encrypted_content"`
		Summary          []responsesReasoningSummaryPart `json:"summary"`
	} `json:"item"`
	ItemID      string `json:"item_id"`
	OutputIndex *int   `json:"output_index,omitempty"`
	Delta       string `json:"delta"`
	Arguments   string `json:"arguments"`
	Response    struct {
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	} `json:"response"`
}

func hasChatGPTReasoningReplay(part Part) bool {
	return strings.TrimSpace(part.ReasoningItemID) != "" || strings.TrimSpace(part.ReasoningEncryptedContent) != ""
}

func buildChatGPTReasoningInputItem(part Part) map[string]interface{} {
	item := map[string]interface{}{
		"type":              "reasoning",
		"id":                strings.TrimSpace(part.ReasoningItemID),
		"encrypted_content": strings.TrimSpace(part.ReasoningEncryptedContent),
		"summary":           []map[string]string{},
	}

	if strings.TrimSpace(part.ReasoningContent) != "" {
		item["summary"] = []map[string]string{
			{
				"type": "summary_text",
				"text": strings.TrimSpace(part.ReasoningContent),
			},
		}
	}

	return item
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

type chatGPTReasoningAccumulator struct {
	items         map[int]*Part
	idToIndex     map[string]int
	nextSynthetic int
}

func newChatGPTReasoningAccumulator() *chatGPTReasoningAccumulator {
	return &chatGPTReasoningAccumulator{
		items:         make(map[int]*Part),
		idToIndex:     make(map[string]int),
		nextSynthetic: -1,
	}
}

func (a *chatGPTReasoningAccumulator) resolveIndex(id string, outputIndex *int) int {
	if outputIndex != nil {
		idx := *outputIndex
		if id != "" {
			a.idToIndex[id] = idx
		}
		return idx
	}

	if id != "" {
		if idx, ok := a.idToIndex[id]; ok {
			return idx
		}
		idx := a.nextSynthetic
		a.nextSynthetic--
		a.idToIndex[id] = idx
		return idx
	}

	idx := a.nextSynthetic
	a.nextSynthetic--
	return idx
}

func (a *chatGPTReasoningAccumulator) ensure(id string, outputIndex *int) int {
	idx := a.resolveIndex(id, outputIndex)
	if _, ok := a.items[idx]; !ok {
		part := &Part{Type: PartText}
		if id != "" {
			part.ReasoningItemID = id
		}
		a.items[idx] = part
	} else if id != "" && a.items[idx].ReasoningItemID == "" {
		a.items[idx].ReasoningItemID = id
	}
	return idx
}

func (a *chatGPTReasoningAccumulator) start(id string, outputIndex *int, encrypted string, summary []responsesReasoningSummaryPart) {
	idx := a.ensure(id, outputIndex)
	item := a.items[idx]
	if encrypted != "" {
		item.ReasoningEncryptedContent = encrypted
	}
	if text := extractReasoningSummaryText(summary); text != "" {
		item.ReasoningContent = text
	}
}

func (a *chatGPTReasoningAccumulator) appendSummary(id string, outputIndex *int, delta string) {
	if delta == "" {
		return
	}
	idx := a.ensure(id, outputIndex)
	a.items[idx].ReasoningContent += delta
}

func (a *chatGPTReasoningAccumulator) finish(id string, outputIndex *int, encrypted string, summary []responsesReasoningSummaryPart) {
	a.start(id, outputIndex, encrypted, summary)
}

func (a *chatGPTReasoningAccumulator) part(id string, outputIndex *int) *Part {
	if id == "" && outputIndex == nil {
		return nil
	}
	idx := a.resolveIndex(id, outputIndex)
	part, ok := a.items[idx]
	if !ok || part == nil {
		return nil
	}
	if part.ReasoningItemID == "" && part.ReasoningEncryptedContent == "" && part.ReasoningContent == "" {
		return nil
	}
	clone := *part
	return &clone
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

// chatGPTRateLimitResponse represents the error response from ChatGPT rate limits
type chatGPTRateLimitResponse struct {
	Error struct {
		Type         string `json:"type"`
		Message      string `json:"message"`
		PlanType     string `json:"plan_type"`
		ResetsAt     int64  `json:"resets_at"`
		ResetsInSecs int    `json:"resets_in_seconds"`
	} `json:"error"`
}

// RateLimitError represents a rate limit error with retry information.
type RateLimitError struct {
	Message       string
	RetryAfter    time.Duration
	PlanType      string
	PrimaryUsed   int
	SecondaryUsed int
}

func (e *RateLimitError) Error() string {
	return e.Message
}

// IsLongWait returns true if the retry wait is too long for automatic retry.
func (e *RateLimitError) IsLongWait() bool {
	return e.RetryAfter > 2*time.Minute
}

// parseChatGPTRateLimitError parses a 429 response and returns a RateLimitError.
// For short waits, the retry logic can use RetryAfter to wait and retry.
// For long waits, it gives a clear message about when to try again.
func parseChatGPTRateLimitError(body []byte, headers http.Header) error {
	var resp chatGPTRateLimitResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		// Couldn't parse, return generic error
		return fmt.Errorf("rate limit exceeded (429): %s", string(body))
	}

	resetsIn := resp.Error.ResetsInSecs

	// Also check headers for reset time (more reliable)
	if headerSecs := headers.Get("X-Codex-Primary-Reset-After-Seconds"); headerSecs != "" {
		if secs, err := parseIntHeader(headerSecs); err == nil && secs > 0 {
			resetsIn = secs
		}
	}

	// Get usage percentages from headers for context
	primaryUsed, _ := parseIntHeader(headers.Get("X-Codex-Primary-Used-Percent"))
	secondaryUsed, _ := parseIntHeader(headers.Get("X-Codex-Secondary-Used-Percent"))

	// Format the reset time in a human-readable way
	resetTime := formatDuration(time.Duration(resetsIn) * time.Second)

	// Build informative error message
	var msg strings.Builder
	msg.WriteString("ChatGPT usage limit reached")

	if resp.Error.PlanType != "" {
		msg.WriteString(fmt.Sprintf(" (%s plan)", resp.Error.PlanType))
	}
	msg.WriteString(". ")

	msg.WriteString(fmt.Sprintf("Resets in %s", resetTime))

	if primaryUsed > 0 || secondaryUsed > 0 {
		msg.WriteString(" (usage: ")
		if primaryUsed > 0 {
			msg.WriteString(fmt.Sprintf("primary %d%%", primaryUsed))
		}
		if secondaryUsed > 0 {
			if primaryUsed > 0 {
				msg.WriteString(", ")
			}
			msg.WriteString(fmt.Sprintf("weekly %d%%", secondaryUsed))
		}
		msg.WriteString(")")
	}

	retryAfter := time.Duration(resetsIn) * time.Second

	// For short waits, include Retry-After hint for the retry logic
	if resetsIn > 0 && resetsIn <= 120 {
		msg.WriteString(fmt.Sprintf(". Retry-After: %d", resetsIn))
	}

	return &RateLimitError{
		Message:       msg.String(),
		RetryAfter:    retryAfter,
		PlanType:      resp.Error.PlanType,
		PrimaryUsed:   primaryUsed,
		SecondaryUsed: secondaryUsed,
	}
}

// parseIntHeader safely parses an integer from a header value
func parseIntHeader(s string) (int, error) {
	s = strings.TrimSpace(s)
	var val int
	_, err := fmt.Sscanf(s, "%d", &val)
	return val, err
}

// formatDuration formats a duration in a human-readable way
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	}
	if d < time.Hour {
		mins := int(d.Minutes())
		secs := int(d.Seconds()) % 60
		if secs > 0 {
			return fmt.Sprintf("%dm %ds", mins, secs)
		}
		return fmt.Sprintf("%d minutes", mins)
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	if mins > 0 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%d hours", hours)
}

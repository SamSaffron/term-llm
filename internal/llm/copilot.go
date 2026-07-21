package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/oauth"
	"github.com/samsaffron/term-llm/internal/signal"
	"golang.org/x/term"
)

var copilotDefaultModel = config.DefaultProviderModel("copilot") // Broadly available Copilot model

// copilotDefaultAPIURL is the default GitHub Copilot API base URL (for individual plans)
// Business/Enterprise accounts use api.business.githubcopilot.com instead
const copilotDefaultAPIURL = "https://api.githubcopilot.com"

// copilotTokenURL is the endpoint to exchange GitHub OAuth token for Copilot session token
const copilotTokenURL = "https://api.github.com/copilot_internal/v2/token"

// Copilot API header constants.
// These values are required to access GitHub's internal Copilot APIs, which check
// for specific client identifiers. We use the VS Code Copilot extension's identifiers
// as this is the standard approach used by third-party Copilot clients.
const (
	copilotUserAgent          = "GitHubCopilotChat/0.26.7"
	copilotEditorVersion      = "vscode/1.96.0"
	copilotPluginVersion      = "copilot-chat/0.26.7"
	copilotIntegrationID      = "vscode-chat"
	copilotAPIVersion         = "2025-04-01"
	copilotOpenAIIntent       = "conversation-panel"
	copilotSessionRefreshSkew = 1 * time.Minute // Refresh shortly before expiry to avoid unnecessary token roundtrips.
)

// copilotHTTPClient is a shared HTTP client with transport-level timeouts.
//
// http.Client.Timeout is intentionally NOT set: it applies to the entire
// request lifetime including reading the streaming response body, and would
// abort legitimate long-running Copilot streams.
var copilotHTTPClient = newStreamingHTTPClient()

// CopilotProvider implements Provider using GitHub Copilot's OpenAI-compatible API.
type CopilotProvider struct {
	creds              *credentials.CopilotCredentials
	model              string
	effort             string            // reasoning effort: "low", "medium", "high", "xhigh", or ""
	apiBaseURL         string            // Set from token exchange (business vs individual)
	sessionToken       string            // Copilot session token (different from OAuth token)
	sessionTokenExpiry time.Time         // When the session token expires
	fileUploadPolicy   *FileUploadPolicy // Provider-specific native file forwarding policy
	responsesClient    *ResponsesClient  // Shared client for Responses API (GPT-5+, codex)
}

// NewCopilotProvider creates a new Copilot provider.
// If credentials are not available or expired, it will prompt the user to authenticate.
func NewCopilotProvider(model string) (*CopilotProvider, error) {
	if model == "" {
		model = copilotDefaultModel
	}
	actualModel, effort := ParseModelEffort(model)

	// Try to load existing credentials
	creds, err := credentials.GetCopilotCredentials()
	if err != nil {
		// No credentials - prompt user to authenticate
		creds, err = PromptForCopilotAuth()
		if err != nil {
			return nil, err
		}
	}

	// Check if token is expired (rare for GitHub tokens, but check anyway)
	if creds.IsExpired() {
		fmt.Println("Copilot token expired. Re-authentication required.")
		creds, err = PromptForCopilotAuth()
		if err != nil {
			return nil, err
		}
	}

	return &CopilotProvider{
		creds:  creds,
		model:  actualModel,
		effort: effort,
	}, nil
}

// NewCopilotProviderWithCreds creates a Copilot provider with pre-loaded credentials.
// This is used by the factory when credentials are already resolved.
func NewCopilotProviderWithCreds(creds *credentials.CopilotCredentials, model string) *CopilotProvider {
	if model == "" {
		model = copilotDefaultModel
	}
	actualModel, effort := ParseModelEffort(model)
	return &CopilotProvider{
		creds:  creds,
		model:  actualModel,
		effort: effort,
	}
}

// PromptForCopilotAuth prompts the user to authenticate with GitHub Copilot.
// Returns an error if running in a non-interactive context (e.g., scripts, CI).
// Exported so `term-llm auth login copilot` can drive the same flow used by
// lazy auth.
func PromptForCopilotAuth() (*credentials.CopilotCredentials, error) {
	// Check if stdin is a terminal - if not, we can't do interactive auth
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, fmt.Errorf("Copilot authentication required but running in non-interactive mode.\n" +
			"Run 'term-llm auth login copilot' interactively first to authenticate")
	}

	fmt.Println("GitHub Copilot provider requires authentication.")
	fmt.Print("Press Enter to start device code authentication...")

	if err := waitForEnterOrInterrupt(); err != nil {
		return nil, err
	}

	// Wire Ctrl-C through to the OAuth wait so the user can cancel
	// while we're polling for the device-code grant.
	sigCtx, stopSig := signal.NotifyContext()
	defer stopSig()
	ctx, cancel := context.WithTimeout(sigCtx, 5*time.Minute)
	defer cancel()

	oauthCreds, err := oauth.AuthenticateCopilot(ctx)
	if err != nil {
		return nil, fmt.Errorf("authentication failed: %w", err)
	}

	// Convert oauth credentials to stored credentials format
	creds := &credentials.CopilotCredentials{
		AccessToken: oauthCreds.AccessToken,
		ExpiresAt:   oauthCreds.ExpiresAt,
	}

	// Save credentials
	if err := credentials.SaveCopilotCredentials(creds); err != nil {
		return nil, fmt.Errorf("failed to save credentials: %w", err)
	}

	fmt.Println("Authentication successful!")
	return creds, nil
}

func (p *CopilotProvider) Name() string {
	if p.effort != "" {
		return fmt.Sprintf("GitHub Copilot (%s, effort=%s)", p.model, p.effort)
	}
	return fmt.Sprintf("GitHub Copilot (%s)", p.model)
}

func (p *CopilotProvider) Credential() string {
	return "copilot"
}

func (p *CopilotProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch: false,
		NativeWebFetch:  false,
		ToolCalls:       true,
	}
}

// useResponsesAPI returns true if the model should use the Responses API.
// GPT-5+ models (including codex variants) require Responses API.
// Older models (gpt-4.1, claude-*, etc.) use Chat Completions.
// Note: gpt-5-mini is excluded as Copilot doesn't support it on Responses API.
func useResponsesAPI(model string) bool {
	model = strings.ToLower(model)
	// GPT-5 and above (gpt-5, gpt-5.2, gpt-5.2-codex, etc.)
	// Exclude gpt-5-mini which isn't supported on Copilot Responses API
	if strings.Contains(model, "gpt-5") && !strings.Contains(model, "gpt-5-mini") {
		return true
	}
	// Codex models explicitly
	if strings.Contains(model, "codex") {
		return true
	}
	// o1, o3, o4 reasoning models
	if strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4") {
		return true
	}
	return false
}

// ResetConversation clears server state for the Responses API client.
// Called on /clear or new conversation.
func (p *CopilotProvider) ResetConversation() {
	if p.responsesClient != nil {
		p.responsesClient.ResetConversation()
	}
}

func (p *CopilotProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	req.MaxOutputTokens = ClampOutputTokens(req.MaxOutputTokens, chooseModel(req.Model, p.model))
	// Check if OAuth token is expired
	if p.creds.IsExpired() {
		return nil, fmt.Errorf("Copilot token expired (re-run with --provider copilot to re-authenticate)")
	}

	// Ensure we have a valid session token (refresh if expired or not initialized)
	if err := p.ensureValidSession(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize Copilot session: %w", err)
	}

	model := chooseModel(req.Model, p.model)

	// GPT-5+, codex, and reasoning models use Responses API
	if useResponsesAPI(model) {
		return p.streamResponses(ctx, req, model)
	}

	// Older models (gpt-4.1, claude-sonnet, etc.) use Chat Completions
	return p.streamChatCompletions(ctx, req, model)
}

// streamChatCompletions streams using the Chat Completions API for older models
func (p *CopilotProvider) streamChatCompletions(ctx context.Context, req Request, model string) (Stream, error) {
	// Build messages using OpenAI-compatible format
	messages := buildCompatMessages(req.Messages)
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages provided")
	}

	tools, err := buildCompatTools(req.Tools)
	if err != nil {
		return nil, err
	}

	chatReq := oaiChatRequest{
		Model:    model, // Use the model passed from Stream()
		Messages: messages,
		Tools:    tools,
		Stream:   true,
		StreamOptions: &oaiStreamOptions{
			IncludeUsage: true,
		},
	}

	if req.ToolChoice.Mode != "" {
		chatReq.ToolChoice = buildCompatToolChoice(req.ToolChoice)
	}
	if req.ParallelToolCalls {
		chatReq.ParallelToolCalls = boolPtr(true)
	}
	if req.Temperature > 0 {
		v := float64(req.Temperature)
		chatReq.Temperature = &v
	}
	if req.TopP > 0 {
		v := float64(req.TopP)
		chatReq.TopP = &v
	}
	if req.MaxOutputTokens > 0 {
		v := req.MaxOutputTokens
		chatReq.MaxTokens = &v
	}

	body, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	if req.DebugRaw {
		var prettyBody bytes.Buffer
		json.Indent(&prettyBody, body, "", "  ")
		DebugRawSection(req.DebugRaw, "Copilot Request", prettyBody.String())
	}

	// Capture values needed by the goroutine
	apiURL := p.apiBaseURL + "/chat/completions"
	sessionToken := p.sessionToken
	debugRaw := req.DebugRaw

	// Create async stream - HTTP request is made inside the goroutine to ensure
	// proper ownership of resp.Body (fixes potential resource leak)
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}

		// Set required Copilot headers
		p.setCopilotAPIHeaders(httpReq, sessionToken)
		httpReq.Header.Set("Copilot-Vision-Request", "true")
		httpReq.Header.Set("Accept", "text/event-stream")

		resp, err := copilotHTTPClient.Do(httpReq)
		if err != nil {
			return fmt.Errorf("Copilot API request failed: %w", err)
		}
		defer resp.Body.Close()

		// Check for error responses
		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)

			if debugRaw {
				var debugInfo strings.Builder
				debugInfo.WriteString(fmt.Sprintf("Status: %d %s\n", resp.StatusCode, resp.Status))
				debugInfo.WriteString("Headers:\n")
				for key, values := range resp.Header {
					for _, value := range values {
						debugInfo.WriteString(fmt.Sprintf("  %s: %s\n", key, value))
					}
				}
				debugInfo.WriteString("Body:\n")
				var prettyBody bytes.Buffer
				if json.Indent(&prettyBody, respBody, "", "  ") == nil {
					debugInfo.WriteString(prettyBody.String())
				} else {
					debugInfo.WriteString(string(respBody))
				}
				DebugRawSection(debugRaw, "Copilot Error Response", debugInfo.String())
			}

			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return fmt.Errorf("Copilot authentication failed (status %d): token may be invalid or expired. Re-run with --provider copilot to re-authenticate", resp.StatusCode)
			}
			return newHTTPStatusError("Copilot", resp, respBody)
		}

		// TODO: Confirm GitHub Copilot's chat/completions stream always
		// terminates with [DONE], then switch RequireDone to true. The previous
		// parser accepted EOF without [DONE], so keep this lenient for now.
		decoder := newSSEDecoder(resp.Body, sseDecoderOptions{RequireDone: false, Transport: "Copilot SSE"})

		toolState := newCompatToolState()
		var lastUsage *Usage
		sawVisibleText := false
		unmarshalErrors := 0

		for {
			eventType, data, err := decoder.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("Copilot streaming error: %w", err)
			}

			if debugRaw {
				DebugRawSection(debugRaw, "Copilot SSE Event", string(data))
			}

			var chatResp oaiChatResponse
			if err := json.Unmarshal(data, &chatResp); err != nil {
				unmarshalErrors++
				if debugRaw {
					DebugRawSection(debugRaw, "Copilot SSE Parse Error", fmt.Sprintf("error: %v, data: %s", err, string(data)))
				}
				// Allow some parse errors (keepalives, partial data) but fail if too many
				if unmarshalErrors > 10 {
					return fmt.Errorf("too many SSE parse errors, last: %w", err)
				}
				continue
			}

			if eventType == "error" || chatResp.Error != nil {
				errMsg := "unknown error"
				if chatResp.Error != nil {
					errMsg = chatResp.Error.Message
				}
				return fmt.Errorf("Copilot API error: %s", errMsg)
			}

			if chatResp.Usage != nil {
				cached := chatResp.Usage.PromptTokensDetails.CachedTokens
				lastUsage = &Usage{
					// OpenAI prompt_tokens includes cached; subtract to get non-cached portion.
					InputTokens:            chatResp.Usage.PromptTokens - cached,
					OutputTokens:           chatResp.Usage.CompletionTokens,
					CachedInputTokens:      cached,
					ProviderRawInputTokens: chatResp.Usage.PromptTokens,
					ProviderTotalTokens:    chatResp.Usage.TotalTokens,
					ReasoningTokens:        chatResp.Usage.CompletionTokensDetails.ReasoningTokens,
				}
			}
			for _, choice := range chatResp.Choices {
				if choice.Delta != nil {
					reasoningDelta := choice.Delta.Reasoning
					if reasoningDelta == "" {
						reasoningDelta = choice.Delta.ReasoningContent
					}
					if content, ok := choice.Delta.Content.(string); ok && content != "" {
						if !isLeadingReasoningWhitespaceArtifact(content, reasoningDelta, sawVisibleText) {
							if hasVisibleTextDelta(content) {
								sawVisibleText = true
							}
							if err := send.Send(Event{Type: EventTextDelta, Text: content}); err != nil {
								return err
							}
						}
					}
					if reasoningDelta != "" {
						if err := send.Send(Event{Type: EventReasoningDelta, Text: reasoningDelta, ReasoningKind: ReasoningKindRaw}); err != nil {
							return err
						}
					}
					if len(choice.Delta.ToolCalls) > 0 {
						toolState.Add(choice.Delta.ToolCalls)
					}
				}
			}
		}

		if err := toolState.Validate(); err != nil {
			return err
		}
		for _, call := range toolState.Calls() {
			if err := send.Send(Event{Type: EventToolCall, Tool: &call}); err != nil {
				return err
			}
		}
		if lastUsage != nil {
			if err := send.Send(Event{Type: EventUsage, Use: lastUsage}); err != nil {
				return err
			}
		}
		return send.Send(Event{Type: EventDone})
	}), nil
}

// streamResponses streams using the Responses API for GPT-5+, codex, and reasoning models
func (p *CopilotProvider) streamResponses(ctx context.Context, req Request, model string) (Stream, error) {
	// Reuse client across requests (but server state is disabled for Copilot)
	if p.responsesClient == nil {
		p.responsesClient = &ResponsesClient{
			BaseURL:       p.apiBaseURL + "/responses",
			GetAuthHeader: func() string { return "Bearer " + p.sessionToken },
			ExtraHeaders: map[string]string{
				"User-Agent":             copilotUserAgent,
				"Copilot-Integration-Id": copilotIntegrationID,
				"Editor-Version":         copilotEditorVersion,
				"Editor-Plugin-Version":  copilotPluginVersion,
				"Openai-Intent":          copilotOpenAIIntent,
				"X-Github-Api-Version":   copilotAPIVersion,
				"Copilot-Vision-Request": "true",
			},
			HTTPClient:         copilotHTTPClient,
			DisableServerState: true, // Copilot doesn't support previous_response_id
			OnAuthRetry: func(retryCtx context.Context) error {
				// Try silent session token refresh using the current request context
				if err := p.refreshSession(retryCtx); err == nil {
					return nil
				}
				// Clear stale credentials so next run triggers interactive auth
				if clearErr := credentials.ClearCopilotCredentials(); clearErr != nil {
					return fmt.Errorf("Copilot session expired and failed to clear credentials: %w", clearErr)
				}
				return fmt.Errorf("Copilot session expired — please re-run your command to re-authenticate")
			},
		}
	}

	// Update auth header in case session token was refreshed
	p.responsesClient.GetAuthHeader = func() string { return "Bearer " + p.sessionToken }

	responsesReq := ResponsesRequest{
		Model:            model,
		Input:            BuildResponsesInputWithFilePolicy(req.Messages, p.effectiveFileUploadPolicy()),
		FileUploadPolicy: p.effectiveFileUploadPolicy(),
		Tools:            BuildResponsesTools(req.Tools),
		Include:          []string{"reasoning.encrypted_content"},
		PromptCacheKey:   req.SessionID,
		Stream:           true,
		SessionID:        req.SessionID,
	}

	if req.ToolChoice.Mode != "" {
		responsesReq.ToolChoice = BuildResponsesToolChoice(req.ToolChoice)
	}
	if req.ParallelToolCalls {
		responsesReq.ParallelToolCalls = boolPtr(true)
	}
	if req.Temperature > 0 {
		v := float64(req.Temperature)
		responsesReq.Temperature = &v
	}
	if req.TopP > 0 {
		v := float64(req.TopP)
		responsesReq.TopP = &v
	}
	if req.MaxOutputTokens > 0 {
		responsesReq.MaxOutputTokens = req.MaxOutputTokens
	}
	// Handle reasoning effort - prefer request-level, fall back to provider-level
	effort := p.effort
	if req.ReasoningEffort != "" {
		effort = req.ReasoningEffort
	}
	responsesReq.Reasoning = &ResponsesReasoning{Summary: "auto"}
	if effort != "" {
		responsesReq.Reasoning.Effort = effort
	}

	return p.responsesClient.Stream(ctx, responsesReq, req.DebugRaw)
}

// copilotModelsResponse represents the response from the Copilot models API
type copilotModelsResponse struct {
	Data []copilotModel `json:"data"`
}

type copilotModel struct {
	ID                  string              `json:"id"`
	Name                string              `json:"name"`
	Version             string              `json:"version"`
	Vendor              string              `json:"vendor"`
	Preview             bool                `json:"preview"`
	ModelPickerCategory string              `json:"model_picker_category"` // "lightweight", "versatile", etc.
	Capabilities        copilotCapabilities `json:"capabilities"`
	// Limit fields are intentionally accepted in several shapes. Current Copilot
	// responses use capabilities.limits, but accepting flat/top-level variants
	// keeps the parser resilient if GitHub moves fields again.
	Limits           copilotModelLimits `json:"limits"`
	MaxPromptTokens  int                `json:"max_prompt_tokens"`
	MaxInputTokens   int                `json:"max_input_tokens"`
	InputTokenLimit  int                `json:"input_token_limit"`
	ContextWindow    int                `json:"context_window"`
	MaxContextWindow int                `json:"max_context_window_tokens"`
	MaxOutputTokens  int                `json:"max_output_tokens"`
}

type copilotCapabilities struct {
	Limits copilotModelLimits `json:"limits"`
}

type copilotModelLimits struct {
	MaxPromptTokens  int `json:"max_prompt_tokens"`
	MaxInputTokens   int `json:"max_input_tokens"`
	InputTokenLimit  int `json:"input_token_limit"`
	ContextWindow    int `json:"context_window"`
	MaxContextWindow int `json:"max_context_window_tokens"`
	MaxOutputTokens  int `json:"max_output_tokens"`
}

const copilotPracticalOutputReserve = 20_000

func (m copilotModel) inputLimit() int {
	// Prefer an explicit prompt/input budget from Copilot. That is the source of
	// truth and avoids reverse-engineering context math.
	if input := firstNonZero(
		m.MaxPromptTokens,
		m.MaxInputTokens,
		m.InputTokenLimit,
		m.Limits.MaxPromptTokens,
		m.Limits.MaxInputTokens,
		m.Limits.InputTokenLimit,
		m.Capabilities.Limits.MaxPromptTokens,
		m.Capabilities.Limits.MaxInputTokens,
		m.Capabilities.Limits.InputTokenLimit,
	); input > 0 {
		return input
	}

	// Some Copilot responses expose only context-window and output metadata.
	// term-llm almost never requests giant outputs from Copilot, so reserve at
	// most 20K tokens instead of subtracting a huge theoretical max output.
	contextWindow := firstNonZero(
		m.MaxContextWindow,
		m.ContextWindow,
		m.Limits.MaxContextWindow,
		m.Limits.ContextWindow,
		m.Capabilities.Limits.MaxContextWindow,
		m.Capabilities.Limits.ContextWindow,
	)
	if contextWindow <= 0 {
		return 0
	}
	maxOutput := firstNonZero(
		m.MaxOutputTokens,
		m.Limits.MaxOutputTokens,
		m.Capabilities.Limits.MaxOutputTokens,
	)
	return copilotInputLimit(contextWindow, maxOutput)
}

func copilotInputLimit(contextWindow, maxOutput int) int {
	if contextWindow <= 0 {
		return 0
	}
	reserve := maxOutput
	if reserve <= 0 || reserve > copilotPracticalOutputReserve {
		reserve = copilotPracticalOutputReserve
	}
	if reserve <= 0 {
		return contextWindow
	}
	if reserve >= contextWindow {
		return 0
	}
	return contextWindow - reserve
}

// ListModels returns available models from the GitHub Copilot API
func (p *CopilotProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	// Ensure we have a valid session token
	if err := p.ensureValidSession(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize Copilot: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "GET", p.apiBaseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set required Copilot headers
	p.setCopilotAPIHeaders(httpReq, p.sessionToken)
	httpReq.Header.Set("Accept", "application/json")

	resp, err := copilotHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Copilot models request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("Copilot authentication failed: token may be invalid or expired")
		}
		return nil, newHTTPStatusError("Copilot", resp, body)
	}

	var modelsResp copilotModelsResponse
	if err := json.Unmarshal(body, &modelsResp); err != nil {
		return nil, fmt.Errorf("failed to decode models response: %w", err)
	}

	models := make([]ModelInfo, 0, len(modelsResp.Data))
	for _, m := range modelsResp.Data {
		displayName := m.Name
		if m.Preview {
			displayName += " (preview)"
		}
		models = append(models, ModelInfo{
			ID:          m.ID,
			DisplayName: displayName,
			OwnedBy:     m.Vendor,
			InputLimit:  m.inputLimit(),
		})
	}
	RefreshCopilotCacheSync(models)

	return models, nil
}

// copilotSessionTokenResponse represents the response from the Copilot token exchange endpoint
type copilotSessionTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int    `json:"expires_at"`
	RefreshIn int    `json:"refresh_in"`
	Endpoints struct {
		API string `json:"api"`
	} `json:"endpoints"`
}

// copilotAPIError represents an error response from the Copilot API with status code.
type copilotAPIError struct {
	*HTTPStatusError
}

func newCopilotAPIError(resp *http.Response, body []byte) *copilotAPIError {
	return &copilotAPIError{HTTPStatusError: newHTTPStatusError("Copilot", resp, body)}
}

func (e *copilotAPIError) Is404() bool {
	return e != nil && e.HTTPStatusCode() == http.StatusNotFound
}

// ensureValidSession ensures we have a valid session token, refreshing if needed.
// Session tokens typically expire after ~25 minutes.
func (p *CopilotProvider) ensureValidSession(ctx context.Context) error {
	// Check if we need to refresh (no token, missing expiry, or close to expiry).
	if p.sessionToken == "" || p.sessionTokenExpiry.IsZero() || time.Until(p.sessionTokenExpiry) <= copilotSessionRefreshSkew {
		return p.refreshSession(ctx)
	}
	return nil
}

// refreshSession fetches a new session token from the Copilot API.
func (p *CopilotProvider) refreshSession(ctx context.Context) error {
	tokenResp, err := p.fetchCopilotTokenInfo(ctx)
	if err != nil {
		return err
	}
	if tokenResp.Endpoints.API != "" {
		p.apiBaseURL = tokenResp.Endpoints.API
	} else {
		p.apiBaseURL = copilotDefaultAPIURL
	}
	p.sessionToken = tokenResp.Token
	// ExpiresAt is a Unix timestamp
	p.sessionTokenExpiry = time.Unix(int64(tokenResp.ExpiresAt), 0)
	return nil
}

// setCopilotAPIHeaders sets the standard headers required for Copilot API requests.
func (p *CopilotProvider) setCopilotAPIHeaders(req *http.Request, token string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", copilotUserAgent)
	req.Header.Set("Copilot-Integration-Id", copilotIntegrationID)
	req.Header.Set("Editor-Version", copilotEditorVersion)
	req.Header.Set("Editor-Plugin-Version", copilotPluginVersion)
	req.Header.Set("Openai-Intent", copilotOpenAIIntent)
	req.Header.Set("X-Github-Api-Version", copilotAPIVersion)
}

// setGitHubAPIHeaders sets the standard headers required for GitHub API requests.
func (p *CopilotProvider) setGitHubAPIHeaders(req *http.Request) {
	req.Header.Set("Authorization", "token "+p.creds.AccessToken)
	req.Header.Set("User-Agent", copilotUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Github-Api-Version", copilotAPIVersion)
	req.Header.Set("Editor-Version", copilotEditorVersion)
	req.Header.Set("Editor-Plugin-Version", copilotPluginVersion)
}

// fetchCopilotTokenInfo gets the token exchange response (used for endpoint info and session token)
func (p *CopilotProvider) fetchCopilotTokenInfo(ctx context.Context) (*copilotSessionTokenResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", copilotTokenURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set required GitHub API headers (using OAuth token)
	p.setGitHubAPIHeaders(httpReq)

	resp, err := copilotHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Copilot token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newCopilotAPIError(resp, body)
	}

	var tokenResp copilotSessionTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}

	return &tokenResp, nil
}

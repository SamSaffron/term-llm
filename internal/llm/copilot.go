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
	"golang.org/x/term"
)

const copilotDefaultModel = "gpt-4.1" // Free tier model

// copilotDefaultAPIURL is the default GitHub Copilot API base URL (for individual plans)
// Business/Enterprise accounts use api.business.githubcopilot.com instead
const copilotDefaultAPIURL = "https://api.githubcopilot.com"

// copilotUsageURL is the GitHub API endpoint for Copilot usage data
const copilotUsageURL = "https://api.github.com/copilot_internal/user"

// copilotTokenURL is the endpoint to exchange GitHub OAuth token for Copilot session token
const copilotTokenURL = "https://api.github.com/copilot_internal/v2/token"

// copilotHTTPTimeout is the timeout for Copilot HTTP requests
const copilotHTTPTimeout = 10 * time.Minute

// Copilot API header constants.
// These values are required to access GitHub's internal Copilot APIs, which check
// for specific client identifiers. We use the VS Code Copilot extension's identifiers
// as this is the standard approach used by third-party Copilot clients.
const (
	copilotUserAgent        = "GitHubCopilotChat/0.26.7"
	copilotEditorVersion    = "vscode/1.96.0"
	copilotPluginVersion    = "copilot-chat/0.26.7"
	copilotIntegrationID    = "vscode-chat"
	copilotAPIVersion       = "2025-04-01"
	copilotOpenAIIntent     = "conversation-panel"
	copilotSessionRefreshIn = 20 * time.Minute // Refresh session token before expiry (tokens last ~25min)
)

// copilotHTTPClient is a shared HTTP client with reasonable timeouts
var copilotHTTPClient = &http.Client{
	Timeout: copilotHTTPTimeout,
}

// CopilotProvider implements Provider using GitHub Copilot's OpenAI-compatible API.
type CopilotProvider struct {
	creds              *credentials.CopilotCredentials
	model              string
	effort             string           // reasoning effort: "low", "medium", "high", "xhigh", or ""
	apiBaseURL         string           // Set from token exchange (business vs individual)
	sessionToken       string           // Copilot session token (different from OAuth token)
	sessionTokenExpiry time.Time        // When the session token expires
	responsesClient    *ResponsesClient // Shared client for Responses API (GPT-5+, codex)
}

// NewCopilotProvider creates a new Copilot provider.
// If credentials are not available or expired, it will prompt the user to authenticate.
func NewCopilotProvider(model string) (*CopilotProvider, error) {
	if model == "" {
		model = copilotDefaultModel
	}
	actualModel, effort := parseModelEffort(model)

	// Try to load existing credentials
	creds, err := credentials.GetCopilotCredentials()
	if err != nil {
		// No credentials - prompt user to authenticate
		creds, err = promptForCopilotAuth()
		if err != nil {
			return nil, err
		}
	}

	// Check if token is expired (rare for GitHub tokens, but check anyway)
	if creds.IsExpired() {
		fmt.Println("Copilot token expired. Re-authentication required.")
		creds, err = promptForCopilotAuth()
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
	actualModel, effort := parseModelEffort(model)
	return &CopilotProvider{
		creds:  creds,
		model:  actualModel,
		effort: effort,
	}
}

// promptForCopilotAuth prompts the user to authenticate with GitHub Copilot.
// Returns an error if running in a non-interactive context (e.g., scripts, CI).
func promptForCopilotAuth() (*credentials.CopilotCredentials, error) {
	// Check if stdin is a terminal - if not, we can't do interactive auth
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, fmt.Errorf("Copilot authentication required but running in non-interactive mode.\n" +
			"Run 'term-llm ask --provider copilot \"test\"' interactively first to authenticate")
	}

	fmt.Println("GitHub Copilot provider requires authentication.")
	fmt.Print("Press Enter to start device code authentication...")

	reader := bufio.NewReader(os.Stdin)
	reader.ReadString('\n')

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
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
			return fmt.Errorf("Copilot API error (status %d): %s", resp.StatusCode, string(respBody))
		}

		scanner := bufio.NewScanner(resp.Body)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		toolState := newCompatToolState()
		var lastUsage *Usage
		var lastEventType string
		unmarshalErrors := 0

		for scanner.Scan() {
			line := scanner.Text()
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

			if debugRaw {
				DebugRawSection(debugRaw, "Copilot SSE Line", data)
			}

			var chatResp oaiChatResponse
			if err := json.Unmarshal([]byte(data), &chatResp); err != nil {
				unmarshalErrors++
				if debugRaw {
					DebugRawSection(debugRaw, "Copilot SSE Parse Error", fmt.Sprintf("error: %v, data: %s", err, data))
				}
				// Allow some parse errors (keepalives, partial data) but fail if too many
				if unmarshalErrors > 10 {
					return fmt.Errorf("too many SSE parse errors, last: %w", err)
				}
				continue
			}

			if lastEventType == "error" || chatResp.Error != nil {
				errMsg := "unknown error"
				if chatResp.Error != nil {
					errMsg = chatResp.Error.Message
				}
				return fmt.Errorf("Copilot API error: %s", errMsg)
			}

			if chatResp.Usage != nil {
				lastUsage = &Usage{
					InputTokens:       chatResp.Usage.PromptTokens,
					OutputTokens:      chatResp.Usage.CompletionTokens,
					CachedInputTokens: chatResp.Usage.PromptTokensDetails.CachedTokens,
				}
			}

			for _, choice := range chatResp.Choices {
				if choice.Delta != nil {
					if content, ok := choice.Delta.Content.(string); ok && content != "" {
						events <- Event{Type: EventTextDelta, Text: content}
					}
					if len(choice.Delta.ToolCalls) > 0 {
						toolState.Add(choice.Delta.ToolCalls)
					}
				}
			}

			lastEventType = ""
		}

		if err := scanner.Err(); err != nil {
			return fmt.Errorf("Copilot streaming error: %w", err)
		}

		for _, call := range toolState.Calls() {
			events <- Event{Type: EventToolCall, Tool: &call}
		}
		if lastUsage != nil {
			events <- Event{Type: EventUsage, Use: lastUsage}
		}
		events <- Event{Type: EventDone}
		return nil
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
		}
	}

	// Update auth header in case session token was refreshed
	p.responsesClient.GetAuthHeader = func() string { return "Bearer " + p.sessionToken }

	responsesReq := ResponsesRequest{
		Model:          model,
		Input:          BuildResponsesInput(req.Messages),
		Tools:          BuildResponsesTools(req.Tools),
		Include:        []string{"reasoning.encrypted_content"},
		PromptCacheKey: req.SessionID,
		Stream:         true,
		SessionID:      req.SessionID,
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
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Version             string `json:"version"`
	Vendor              string `json:"vendor"`
	Preview             bool   `json:"preview"`
	ModelPickerCategory string `json:"model_picker_category"` // "lightweight", "versatile", etc.
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
		return nil, fmt.Errorf("Copilot API error (status %d): %s", resp.StatusCode, string(body))
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
		})
	}

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

// copilotAPIError represents an error response from the Copilot API with status code
type copilotAPIError struct {
	StatusCode int
	Body       string
}

func (e *copilotAPIError) Error() string {
	return fmt.Sprintf("Copilot API error (status %d): %s", e.StatusCode, e.Body)
}

func (e *copilotAPIError) Is404() bool {
	return e.StatusCode == http.StatusNotFound
}

// ensureValidSession ensures we have a valid session token, refreshing if needed.
// Session tokens typically expire after ~25 minutes.
func (p *CopilotProvider) ensureValidSession(ctx context.Context) error {
	// Check if we need to refresh (no token, or approaching expiry)
	if p.sessionToken == "" || time.Now().After(p.sessionTokenExpiry.Add(-copilotSessionRefreshIn)) {
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
		return nil, &copilotAPIError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	var tokenResp copilotSessionTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}

	return &tokenResp, nil
}

// CopilotUsage represents the usage data from GitHub Copilot
type CopilotUsage struct {
	Plan        string        `json:"plan"`
	ResetDate   time.Time     `json:"reset_date"`
	PremiumChat *CopilotQuota `json:"premium_chat,omitempty"`
	Chat        *CopilotQuota `json:"chat,omitempty"`
	Completions *CopilotQuota `json:"completions,omitempty"`
}

// CopilotQuota represents quota information for a specific feature
type CopilotQuota struct {
	Entitlement      int     `json:"entitlement"`
	Used             int     `json:"used"`
	Remaining        int     `json:"remaining"`
	PercentRemaining float64 `json:"percent_remaining"`
	Unlimited        bool    `json:"unlimited"`
}

// copilotUsageResponse represents the raw API response
type copilotUsageResponse struct {
	CopilotPlan    string `json:"copilot_plan"`
	QuotaResetDate string `json:"quota_reset_date"`
	QuotaSnapshots struct {
		PremiumInteractions *struct {
			Entitlement      int     `json:"entitlement"`
			Remaining        int     `json:"remaining"`
			PercentRemaining float64 `json:"percent_remaining"`
			Unlimited        bool    `json:"unlimited"`
		} `json:"premium_interactions"`
		Chat *struct {
			Entitlement      int     `json:"entitlement"`
			Remaining        int     `json:"remaining"`
			PercentRemaining float64 `json:"percent_remaining"`
			Unlimited        bool    `json:"unlimited"`
		} `json:"chat"`
		Completions *struct {
			Entitlement      int     `json:"entitlement"`
			Remaining        int     `json:"remaining"`
			PercentRemaining float64 `json:"percent_remaining"`
			Unlimited        bool    `json:"unlimited"`
		} `json:"completions"`
	} `json:"quota_snapshots"`
}

// GetUsage fetches the current Copilot usage and quota information.
// This uses GitHub's internal API which requires the VS Code OAuth client ID.
func (p *CopilotProvider) GetUsage(ctx context.Context) (*CopilotUsage, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", copilotUsageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set required GitHub API headers (using OAuth token)
	p.setGitHubAPIHeaders(httpReq)

	resp, err := copilotHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Copilot usage request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("Copilot usage API not available.\n" +
				"This may happen if you're using a different OAuth app.\n" +
				"To check your usage, visit: https://github.com/settings/copilot")
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("Copilot authentication failed: token may be invalid or expired")
		}
		return nil, fmt.Errorf("Copilot API error (status %d): %s", resp.StatusCode, string(body))
	}

	var rawResp copilotUsageResponse
	if err := json.Unmarshal(body, &rawResp); err != nil {
		return nil, fmt.Errorf("failed to decode usage response: %w", err)
	}

	// Parse reset date
	resetDate, _ := time.Parse(time.RFC3339, rawResp.QuotaResetDate)

	usage := &CopilotUsage{
		Plan:      rawResp.CopilotPlan,
		ResetDate: resetDate,
	}

	// Map premium_interactions to PremiumChat
	if pi := rawResp.QuotaSnapshots.PremiumInteractions; pi != nil {
		usage.PremiumChat = &CopilotQuota{
			Entitlement:      pi.Entitlement,
			Used:             pi.Entitlement - pi.Remaining,
			Remaining:        pi.Remaining,
			PercentRemaining: pi.PercentRemaining,
			Unlimited:        pi.Unlimited,
		}
	}

	if c := rawResp.QuotaSnapshots.Chat; c != nil {
		usage.Chat = &CopilotQuota{
			Entitlement:      c.Entitlement,
			Used:             c.Entitlement - c.Remaining,
			Remaining:        c.Remaining,
			PercentRemaining: c.PercentRemaining,
			Unlimited:        c.Unlimited,
		}
	}

	if c := rawResp.QuotaSnapshots.Completions; c != nil {
		usage.Completions = &CopilotQuota{
			Entitlement:      c.Entitlement,
			Used:             c.Entitlement - c.Remaining,
			Remaining:        c.Remaining,
			PercentRemaining: c.PercentRemaining,
			Unlimited:        c.Unlimited,
		}
	}

	return usage, nil
}

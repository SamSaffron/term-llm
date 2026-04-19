package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/oauth"
	"github.com/samsaffron/term-llm/internal/signal"
	"golang.org/x/term"
)

const chatGPTDefaultModel = "gpt-5.3-codex"

// chatGPTResponsesURL is the ChatGPT backend API endpoint for responses
const chatGPTResponsesURL = "https://chatgpt.com/backend-api/codex/responses"

// chatGPTHTTPClient intentionally uses transport-level timeouts only.
//
// ChatGPT streams can legitimately run for longer than 10 minutes, and
// http.Client.Timeout would abort an otherwise healthy stream mid-response.
var chatGPTHTTPClient = defaultHTTPClient

// ChatGPTProvider implements Provider using the ChatGPT backend API with native OAuth.
type ChatGPTProvider struct {
	creds           *credentials.ChatGPTCredentials
	model           string
	effort          string // reasoning effort: "low", "medium", "high", "xhigh", or ""
	responsesClient *ResponsesClient
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

// promptForChatGPTAuth prompts the user to authenticate with ChatGPT.
// Prefers the device-code flow so auth works on headless/remote/containerized
// boxes; falls back to the localhost browser flow if the backend doesn't
// advertise device-code support.
func promptForChatGPTAuth() (*credentials.ChatGPTCredentials, error) {
	// Check if stdin is a terminal - if not, we can't do interactive auth
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, fmt.Errorf("ChatGPT authentication required but running in non-interactive mode.\n" +
			"Run 'term-llm ask --provider chatgpt \"test\"' interactively first to authenticate")
	}

	fmt.Println("ChatGPT provider requires authentication.")

	// Wire Ctrl-C through to the full auth wait (device-code poll OR
	// browser callback). The 15-minute cap matches the server-side
	// device-code expiry.
	sigCtx, stopSig := signal.NotifyContext()
	defer stopSig()
	ctx, cancel := context.WithTimeout(sigCtx, 15*time.Minute)
	defer cancel()

	oauthCreds, err := runChatGPTDeviceCodeFlow(ctx)
	if errors.Is(err, oauth.ErrChatGPTDeviceCodeNotEnabled) {
		fmt.Println("(device-code login unavailable — falling back to browser flow)")
		oauthCreds, err = runChatGPTBrowserFlow(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("authentication failed: %w", err)
	}

	creds := &credentials.ChatGPTCredentials{
		AccessToken:  oauthCreds.AccessToken,
		RefreshToken: oauthCreds.RefreshToken,
		ExpiresAt:    oauthCreds.ExpiresAt,
		AccountID:    oauthCreds.AccountID,
	}
	if err := credentials.SaveChatGPTCredentials(creds); err != nil {
		return nil, fmt.Errorf("failed to save credentials: %w", err)
	}

	fmt.Println("Authentication successful!")
	return creds, nil
}

func runChatGPTDeviceCodeFlow(ctx context.Context) (*oauth.ChatGPTCredentials, error) {
	dc, err := oauth.RequestChatGPTDeviceCode(ctx)
	if err != nil {
		return nil, err
	}
	fmt.Printf("\nTo sign in with ChatGPT:\n")
	fmt.Printf("  1. Open this URL in any browser: %s\n", dc.VerificationURL)
	fmt.Printf("  2. Enter this one-time code:     %s\n\n", dc.UserCode)
	fmt.Print("Waiting for approval (Ctrl-C to cancel)...")

	creds, err := oauth.AuthenticateChatGPTDevice(ctx, dc)
	if err != nil {
		fmt.Println()
		return nil, err
	}
	fmt.Println(" done!")
	return creds, nil
}

func runChatGPTBrowserFlow(ctx context.Context) (*oauth.ChatGPTCredentials, error) {
	fmt.Print("Press Enter to open browser and sign in with your ChatGPT account...")
	if err := waitForEnterOrInterrupt(); err != nil {
		return nil, err
	}
	return oauth.AuthenticateChatGPT(ctx)
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

	// Reuse client across requests
	if p.responsesClient == nil {
		p.responsesClient = &ResponsesClient{
			BaseURL: chatGPTResponsesURL,
			GetAuthHeader: func() string {
				return "Bearer " + p.creds.AccessToken
			},
			ExtraHeaders: map[string]string{
				"ChatGPT-Account-ID": p.creds.AccountID,
				"OpenAI-Beta":        "responses=experimental",
				"originator":         "term-llm",
			},
			HTTPClient:         chatGPTHTTPClient,
			DisableServerState: true,
			HandleError: func(statusCode int, body []byte, headers http.Header) error {
				if statusCode == http.StatusTooManyRequests {
					return parseChatGPTRateLimitError(body, headers)
				}
				return nil // fall through to default handling
			},
			OnAuthRetry: func(_ context.Context) error {
				// Try silent token refresh
				if err := credentials.RefreshChatGPTCredentials(p.creds); err == nil {
					return nil
				}
				// Clear stale credentials so next run triggers interactive auth
				if clearErr := credentials.ClearChatGPTCredentials(); clearErr != nil {
					return fmt.Errorf("ChatGPT session expired and failed to clear credentials: %w", clearErr)
				}
				return fmt.Errorf("ChatGPT session expired — please re-run your command to re-authenticate")
			},
		}
	}

	// Effort precedence: req.ReasoningEffort wins over model suffix, which wins over provider-level effort.
	reqModel, reqEffort := parseModelEffort(req.Model)
	model := chooseModel(reqModel, p.model)
	effort := p.effort
	if reqEffort != "" {
		effort = reqEffort
	}
	if v := strings.TrimSpace(req.ReasoningEffort); v != "" {
		effort = v
	}

	// Build tools
	tools := BuildResponsesTools(req.Tools)
	if req.Search {
		tools = append([]any{ResponsesWebSearchTool{Type: "web_search"}}, tools...)
	}

	// Build input with system messages extracted as instructions
	instructions, input := BuildResponsesInputWithInstructions(req.Messages)

	responsesReq := ResponsesRequest{
		Model:          model,
		Instructions:   instructions,
		Input:          input,
		Tools:          tools,
		Include:        []string{"reasoning.encrypted_content"},
		PromptCacheKey: req.SessionID,
		Store:          boolPtr(false),
		Stream:         true,
		SessionID:      req.SessionID,
	}

	if req.ToolChoice.Mode != "" {
		responsesReq.ToolChoice = BuildResponsesToolChoice(req.ToolChoice)
	}
	if req.ParallelToolCalls {
		responsesReq.ParallelToolCalls = boolPtr(true)
	}
	responsesReq.Reasoning = &ResponsesReasoning{Summary: "auto"}
	if effort != "" {
		responsesReq.Reasoning.Effort = effort
	}

	return p.responsesClient.Stream(ctx, responsesReq, req.DebugRaw)
}

// ResetConversation clears server state for the Responses API client.
func (p *ChatGPTProvider) ResetConversation() {
	if p.responsesClient != nil {
		p.responsesClient.ResetConversation()
	}
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

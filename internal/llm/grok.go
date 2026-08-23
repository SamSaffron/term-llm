package llm

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/grokprotocol"
	"github.com/samsaffron/term-llm/internal/oauth"
	"github.com/samsaffron/term-llm/internal/signal"
	"golang.org/x/term"
)

const (
	grokResponsesURL = "https://cli-chat-proxy.grok.com/v1/responses"
	// grokProxyCompatibilityVersion is the wire compatibility version currently
	// required by the Grok CLI subscription proxy. It is not term-llm's version.
	grokProxyCompatibilityVersion = grokprotocol.ClientVersion
	grokUserAgent                 = grokprotocol.ClientIdentifier
	grokDefaultInstructions       = "You are a helpful assistant."
)

var (
	grokDefaultModel                     = config.DefaultProviderModel("grok")
	grokHTTPClient                       = defaultHTTPClient
	grokOAuthClient         grokOAuthAPI = oauth.NewGrokOAuthClient(nil)
	grokInteractiveTerminal              = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	grokAuthOutput          io.Writer    = os.Stderr
	grokAuthContext                      = signal.NotifyContext
)

type grokOAuthAPI interface {
	RequestDeviceCode(context.Context) (*oauth.GrokDeviceCode, error)
	PollDeviceToken(context.Context, *oauth.GrokDeviceCode) (*oauth.GrokTokenResponse, error)
	RefreshToken(context.Context, string) (*oauth.GrokTokenResponse, error)
	UserInfo(context.Context, string) (string, error)
	Revoke(context.Context, string) error
}

type GrokProvider struct {
	creds            *credentials.GrokCredentials
	model            string
	effort           string
	responsesClient  *ResponsesClient
	fileUploadPolicy *FileUploadPolicy
}

type GrokProviderOptions struct {
	FileUploadPolicy *FileUploadPolicy
}

func NewGrokProvider(model string) (*GrokProvider, error) {
	return NewGrokProviderWithOptions(model, GrokProviderOptions{})
}

func NewGrokProviderWithOptions(model string, opts GrokProviderOptions) (*GrokProvider, error) {
	if model == "" {
		model = grokDefaultModel
	}
	actualModel, _ := parseModelEffortForProvider("grok", model)
	if err := validateGrokModel(actualModel); err != nil {
		return nil, err
	}
	creds, err := credentials.GetGrokCredentials()
	if err != nil {
		creds, err = PromptForGrokAuth()
		if err != nil {
			return nil, err
		}
	}
	if creds.IsExpired() {
		if err := refreshGrokSession(context.Background(), creds, false); err != nil {
			if !errors.Is(err, oauth.ErrGrokRefreshTokenInvalid) || !grokInteractiveTerminal() {
				return nil, fmt.Errorf("refresh Grok session: %w", err)
			}
			creds, err = PromptForGrokAuth()
			if err != nil {
				return nil, err
			}
		}
	}
	return NewGrokProviderWithCredsAndOptions(creds, model, opts), nil
}

func NewGrokProviderWithCreds(creds *credentials.GrokCredentials, model string) *GrokProvider {
	return NewGrokProviderWithCredsAndOptions(creds, model, GrokProviderOptions{})
}

func NewGrokProviderWithCredsAndOptions(creds *credentials.GrokCredentials, model string, opts GrokProviderOptions) *GrokProvider {
	if model == "" {
		model = grokDefaultModel
	}
	actualModel, effort := parseModelEffortForProvider("grok", model)
	return &GrokProvider{
		creds:            creds,
		model:            actualModel,
		effort:           effort,
		fileUploadPolicy: cloneFileUploadPolicy(opts.FileUploadPolicy),
	}
}

func PromptForGrokAuth() (*credentials.GrokCredentials, error) {
	if !grokInteractiveTerminal() {
		return nil, errors.New("Grok subscription authentication required in non-interactive mode; run 'term-llm auth login grok' interactively first")
	}
	fmt.Fprintln(grokAuthOutput, "Grok subscription provider requires xAI authentication.")
	sigCtx, stop := grokAuthContext()
	defer stop()
	device, err := grokOAuthClient.RequestDeviceCode(sigCtx)
	if err != nil {
		return nil, fmt.Errorf("start Grok authentication: %w", err)
	}
	if err := oauth.ValidateGrokDeviceCodeForDisplay(device); err != nil {
		return nil, fmt.Errorf("start Grok authentication: %w", err)
	}
	fmt.Fprintln(grokAuthOutput, "\nTo sign in with Grok:")
	if device.VerificationURIComplete != "" {
		fmt.Fprintf(grokAuthOutput, "  Open: %s\n", device.VerificationURIComplete)
	}
	fmt.Fprintf(grokAuthOutput, "  Or open: %s\n  Code:    %s\n\n", device.VerificationURI, device.UserCode)
	fmt.Fprint(grokAuthOutput, "Waiting for approval (Ctrl-C to cancel)...")
	token, err := grokOAuthClient.PollDeviceToken(sigCtx, device)
	if err != nil {
		fmt.Fprintln(grokAuthOutput)
		return nil, fmt.Errorf("Grok authentication failed: %w", err)
	}
	accountID, err := grokOAuthClient.UserInfo(sigCtx, token.AccessToken)
	if err != nil {
		fmt.Fprintln(grokAuthOutput)
		return nil, fmt.Errorf("Grok authentication failed: %w", err)
	}
	expiresAt, err := oauth.GrokExpiryUnix(time.Now().Unix(), token.ExpiresIn)
	if err != nil {
		fmt.Fprintln(grokAuthOutput)
		return nil, fmt.Errorf("Grok authentication failed: %w", err)
	}
	creds := &credentials.GrokCredentials{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		ExpiresAt:    expiresAt,
		AccountID:    accountID,
	}
	if err := credentials.SaveGrokCredentials(creds); err != nil {
		return nil, fmt.Errorf("save Grok credentials: %w", err)
	}
	fmt.Fprintln(grokAuthOutput, " done!\nAuthentication successful!")
	return creds, nil
}

// LogoutGrok best-effort revokes the refresh token and always removes the same
// local credential generation. A concurrent login is preserved.
func LogoutGrok(ctx context.Context) (warning string, err error) {
	creds, loadErr := credentials.GetGrokCredentials()
	if loadErr != nil {
		if !credentials.GrokCredentialsExist() {
			return "", nil
		}
		// A corrupt/insecure local file still must be deleted on explicit logout.
		if clearErr := credentials.ClearGrokCredentials(); clearErr != nil {
			return "", clearErr
		}
		_ = clearGrokModelsCache()
		return "could not revoke the remote Grok session because local credentials were unreadable", nil
	}
	revokeErr := grokOAuthClient.Revoke(ctx, creds.RefreshToken)
	if clearErr := credentials.ClearGrokCredentialsIfRefreshToken(creds.RefreshToken); clearErr != nil {
		return "", clearErr
	}
	_ = clearGrokModelsCache()
	if revokeErr != nil {
		return "remote Grok session revocation failed; local credentials were cleared", nil
	}
	return "", nil
}

func (p *GrokProvider) Name() string {
	if p.effort != "" {
		return fmt.Sprintf("Grok Subscription (%s, effort=%s)", p.model, p.effort)
	}
	return fmt.Sprintf("Grok Subscription (%s)", p.model)
}

func (p *GrokProvider) Credential() string { return "grok" }

func (p *GrokProvider) Capabilities() Capabilities {
	return Capabilities{ToolCalls: true, SupportsToolChoice: true}
}

func (p *GrokProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	if err := validateGrokModel(chooseModel(req.Model, p.model)); err != nil {
		return nil, err
	}
	if p.creds.IsExpired() {
		if err := refreshGrokSession(ctx, p.creds, false); err != nil {
			return nil, fmt.Errorf("refresh Grok session: %w", err)
		}
	}
	if p.responsesClient == nil {
		p.responsesClient = newGrokResponsesClient(p.creds)
	}
	reqModel, reqEffort := parseModelEffortForProvider("grok", req.Model)
	model := chooseModel(reqModel, p.model)
	effort := p.effort
	if reqEffort != "" {
		effort = reqEffort
	}
	if strings.TrimSpace(req.ReasoningEffort) != "" {
		effort = strings.TrimSpace(req.ReasoningEffort)
	}
	tools := BuildResponsesTools(req.Tools)
	instructions := collectRoleText(req.Messages, RoleSystem)
	if strings.TrimSpace(instructions) == "" {
		instructions = grokDefaultInstructions
	}
	requestID, err := newGrokRequestID()
	if err != nil {
		return nil, err
	}
	extraHeaders := map[string]string{
		"x-grok-model-override": model,
		"x-grok-req-id":         requestID,
	}
	promptCacheKey := ""
	if req.SessionID != "" && validGrokConversationID(req.SessionID) {
		extraHeaders["x-grok-conv-id"] = req.SessionID
		extraHeaders["x-grok-session-id"] = req.SessionID
		promptCacheKey = req.SessionID
	}
	responsesReq := ResponsesRequest{
		Model:                           model,
		Instructions:                    instructions,
		Messages:                        req.Messages,
		ExtractInstructionsFromMessages: true,
		Tools:                           tools,
		ToolChoice:                      "auto",
		ParallelToolCalls:               boolPtr(true),
		MaxOutputTokens:                 clampGrokOutputTokens(req.MaxOutputTokens, model),
		Text:                            &ResponsesText{Verbosity: "low"},
		Include:                         []string{"reasoning.encrypted_content"},
		PromptCacheKey:                  promptCacheKey,
		Store:                           boolPtr(false),
		Stream:                          true,
		ExtraHeaders:                    extraHeaders,
		FileUploadPolicy:                p.fileUploadPolicy,
	}
	if req.ToolChoice.Mode != "" {
		responsesReq.ToolChoice = BuildResponsesToolChoice(req.ToolChoice)
	}
	if req.TemperatureSet || req.Temperature != 0 {
		value := float64(req.Temperature)
		responsesReq.Temperature = &value
	}
	if req.TopPSet || req.TopP != 0 {
		value := float64(req.TopP)
		responsesReq.TopP = &value
	}
	if effort != "" {
		responsesReq.Reasoning = &ResponsesReasoning{Effort: effort, Summary: "auto"}
	}
	return p.responsesClient.Stream(ctx, responsesReq, req.DebugRaw)
}

func (p *GrokProvider) ResetConversation() {
	if p.responsesClient != nil {
		p.responsesClient.ResetConversation()
	}
}

func (p *GrokProvider) isolateHelperConversation() Provider {
	clone := *p
	clone.responsesClient = cloneResponsesClientFreshConversation(p.responsesClient)
	return &clone
}

func newGrokResponsesClient(creds *credentials.GrokCredentials) *ResponsesClient {
	return &ResponsesClient{
		BaseURL: grokResponsesURL,
		GetAuthHeader: func() string {
			return "Bearer " + creds.AccessToken
		},
		ExtraHeaders: map[string]string{
			"X-XAI-Token-Auth":       "xai-grok-cli",
			"x-authenticateresponse": "authenticate-response",
			// Canonical uses this for a client_mode metrics label, not auth.
			// The provider has no reliable runtime interactive signal, so term-llm
			// subscription requests are classified as headless.
			"x-grok-client-mode":       grokprotocol.ClientModeHeadless,
			"x-grok-client-version":    grokProxyCompatibilityVersion,
			"x-grok-client-identifier": grokprotocol.ClientIdentifier,
			"x-grok-user-id":           creds.AccountID,
			"User-Agent":               grokUserAgent,
		},
		HTTPClient:         grokHTTPClient,
		DisableServerState: true,
		HandleError: func(statusCode int, body []byte, headers http.Header) error {
			switch statusCode {
			case http.StatusForbidden:
				return errors.New("Grok subscription access was denied (403); verify that this xAI account has an active Grok subscription and CLI entitlement")
			case http.StatusTooManyRequests:
				return &RateLimitError{Message: "Grok subscription rate limit exceeded", RetryAfter: grokRetryAfter(headers.Get("Retry-After"))}
			default:
				return nil
			}
		},
		OnAuthRetry: func(ctx context.Context) error {
			return refreshGrokSession(ctx, creds, true)
		},
	}
}

func refreshGrokSession(ctx context.Context, creds *credentials.GrokCredentials, force bool) error {
	failedRefreshToken := ""
	if creds != nil {
		failedRefreshToken = creds.RefreshToken
	}
	if err := credentials.RefreshGrokCredentialsWithClient(ctx, creds, force, grokOAuthClient); err != nil {
		if errors.Is(err, oauth.ErrGrokRefreshTokenInvalid) {
			if clearErr := credentials.ClearGrokCredentialsIfRefreshToken(failedRefreshToken); clearErr != nil {
				return fmt.Errorf("%w; clear stale Grok credentials: %v; run 'term-llm auth login grok'", err, clearErr)
			}
			_ = clearGrokModelsCache()
			return fmt.Errorf("%w; local credentials were cleared; run 'term-llm auth login grok'", err)
		}
		return err
	}
	return nil
}

func validateGrokModel(model string) error {
	if len(model) == 0 || len(model) > 256 {
		return errors.New("invalid Grok model ID")
	}
	for i := 0; i < len(model); i++ {
		if model[i] <= 0x20 || model[i] == 0x7f {
			return errors.New("invalid Grok model ID")
		}
	}
	return nil
}

func newGrokRequestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate Grok request ID: %w", err)
	}
	// Match canonical UUIDv4 request IDs without adding a UUID dependency.
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), nil
}

func validGrokConversationID(id string) bool {
	if len(id) == 0 || len(id) > 256 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

func grokRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}

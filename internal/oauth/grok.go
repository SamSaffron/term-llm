package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/grokprotocol"
)

const (
	GrokClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	// term-llm only calls the subscription Responses and model-catalog APIs, so
	// conversations/workspaces scopes from grok-build are intentionally omitted.
	GrokScopes = "openid profile email offline_access grok-cli:access api:access"

	GrokDeviceEndpoint   = "https://auth.x.ai/oauth2/device/code"
	GrokTokenEndpoint    = "https://auth.x.ai/oauth2/token"
	GrokUserInfoEndpoint = "https://auth.x.ai/oauth2/userinfo"
	GrokRevokeEndpoint   = "https://auth.x.ai/oauth2/revoke"

	maxGrokOAuthBodyBytes         = 64 * 1024
	maxGrokOAuthDiagnosticBytes   = 256
	maxGrokDeviceURLBytes         = 2048
	maxGrokDeviceDisplayTextBytes = 128
)

var (
	ErrGrokAccessDenied        = errors.New("Grok authorization was denied")
	ErrGrokDeviceCodeExpired   = errors.New("Grok device authorization expired")
	ErrGrokRefreshTokenInvalid = errors.New("Grok refresh token expired or revoked")

	grokOAuthLabeledSecretPattern = regexp.MustCompile(`(?i)(access_token|refresh_token|id_token|device_code|authorization|token)\s*[:=]\s*[^\s,;]+`)
	grokOAuthBearerSecretPattern  = regexp.MustCompile(`(?i)bearer\s+[^\s,;]+`)
)

type GrokDeviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}

type GrokTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

type grokOAuthEndpoints struct {
	device   string
	token    string
	userinfo string
	revoke   string
}

// GrokOAuthClient implements xAI's RFC 8628 device flow. Endpoint replacement is
// intentionally private; tests inject a transport that rewrites production URLs.
type GrokOAuthClient struct {
	HTTPClient *http.Client
	now        func() time.Time
	sleep      func(context.Context, time.Duration) error
	endpoints  grokOAuthEndpoints
}

func NewGrokOAuthClient(httpClient *http.Client) *GrokOAuthClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &GrokOAuthClient{
		HTTPClient: httpClient,
		now:        time.Now,
		sleep:      sleepGrokOAuth,
		endpoints: grokOAuthEndpoints{
			device:   GrokDeviceEndpoint,
			token:    GrokTokenEndpoint,
			userinfo: GrokUserInfoEndpoint,
			revoke:   GrokRevokeEndpoint,
		},
	}
}

func sleepGrokOAuth(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *GrokOAuthClient) RequestDeviceCode(ctx context.Context) (*GrokDeviceCode, error) {
	form := url.Values{
		"client_id": {GrokClientID},
		"scope":     {GrokScopes},
		// referrer is usage-attribution analytics, so identify term-llm rather
		// than impersonating grok-build.
		"referrer": {"term-llm"},
	}
	body, status, err := c.doDeviceForm(ctx, c.endpoints.device, form)
	if err != nil {
		return nil, fmt.Errorf("request Grok device code: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, grokOAuthStatusError("request Grok device code", status, body)
	}
	var device GrokDeviceCode
	if err := json.Unmarshal(body, &device); err != nil {
		return nil, fmt.Errorf("decode Grok device response: %w", err)
	}
	if device.DeviceCode == "" || device.UserCode == "" || device.VerificationURI == "" || device.ExpiresIn <= 0 {
		return nil, errors.New("invalid Grok device response")
	}
	if device.Interval <= 0 {
		device.Interval = 5
	}
	if device.Interval > 300 || device.ExpiresIn > 24*60*60 {
		return nil, errors.New("invalid Grok device response timing")
	}
	if err := ValidateGrokDeviceCodeForDisplay(&device); err != nil {
		return nil, err
	}
	return &device, nil
}

func (c *GrokOAuthClient) PollDeviceToken(ctx context.Context, device *GrokDeviceCode) (*GrokTokenResponse, error) {
	if device == nil || device.DeviceCode == "" || device.ExpiresIn <= 0 || device.Interval <= 0 {
		return nil, errors.New("invalid Grok device code")
	}
	deadline := c.now().Add(time.Duration(device.ExpiresIn) * time.Second)
	interval := time.Duration(device.Interval) * time.Second
	for {
		remaining := deadline.Sub(c.now())
		if remaining <= 0 {
			return nil, ErrGrokDeviceCodeExpired
		}
		wait := interval
		if wait > remaining {
			wait = remaining
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
		if !c.now().Before(deadline) {
			return nil, ErrGrokDeviceCodeExpired
		}
		form := url.Values{
			"client_id":   {GrokClientID},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {device.DeviceCode},
		}
		body, status, err := c.doDeviceForm(ctx, c.endpoints.token, form)
		if err != nil {
			return nil, fmt.Errorf("poll Grok device authorization: %w", err)
		}
		if status >= 200 && status < 300 {
			return parseGrokTokenResponse(body, true)
		}
		info := parseGrokOAuthError(body)
		switch info.Code {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
			if interval > 5*time.Minute {
				return nil, errors.New("invalid Grok device polling interval")
			}
		case "access_denied":
			return nil, wrapGrokOAuthSentinel(ErrGrokAccessDenied, info, device.DeviceCode)
		case "expired_token":
			return nil, wrapGrokOAuthSentinel(ErrGrokDeviceCodeExpired, info, device.DeviceCode)
		default:
			return nil, grokOAuthStatusError("poll Grok device authorization", status, body, device.DeviceCode)
		}
	}
}

func (c *GrokOAuthClient) RefreshToken(ctx context.Context, refreshToken string) (*GrokTokenResponse, error) {
	if refreshToken == "" {
		return nil, errors.New("missing Grok refresh token")
	}
	form := url.Values{
		"client_id":     {GrokClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	body, status, err := c.doForm(ctx, c.endpoints.token, "", form)
	if err != nil {
		return nil, fmt.Errorf("refresh Grok session: %w", err)
	}
	if status < 200 || status >= 300 {
		info := parseGrokOAuthError(body)
		if info.Code == "invalid_grant" || info.Code == "invalid_token" {
			return nil, wrapGrokOAuthSentinel(ErrGrokRefreshTokenInvalid, info, refreshToken)
		}
		return nil, grokOAuthStatusError("refresh Grok session", status, body, refreshToken)
	}
	return parseGrokTokenResponse(body, false)
}

func (c *GrokOAuthClient) UserInfo(ctx context.Context, accessToken string) (string, error) {
	if accessToken == "" {
		return "", errors.New("missing Grok access token")
	}
	body, status, err := c.doForm(ctx, c.endpoints.userinfo, "Bearer "+accessToken, nil)
	if err != nil {
		return "", fmt.Errorf("fetch Grok user info: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", grokOAuthStatusError("fetch Grok user info", status, body, accessToken)
	}
	var info struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(body, &info); err != nil || !ValidGrokAccountID(info.Sub) {
		return "", errors.New("invalid Grok user info response")
	}
	return info.Sub, nil
}

func (c *GrokOAuthClient) Revoke(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	form := url.Values{"client_id": {GrokClientID}, "token": {token}, "token_type_hint": {"refresh_token"}}
	body, status, err := c.doForm(ctx, c.endpoints.revoke, "", form)
	if err != nil {
		return fmt.Errorf("revoke Grok session: %w", err)
	}
	if status < 200 || status >= 300 {
		return grokOAuthStatusError("revoke Grok session", status, body, token)
	}
	return nil
}

func (c *GrokOAuthClient) doDeviceForm(ctx context.Context, endpoint string, form url.Values) ([]byte, int, error) {
	return c.doFormWithHeaders(ctx, endpoint, "", form, http.Header{
		"x-grok-client-version": {grokprotocol.ClientVersion},
		"x-grok-client-surface": {grokprotocol.ClientSurfaceCLI},
	})
}

func (c *GrokOAuthClient) doForm(ctx context.Context, endpoint, authorization string, form url.Values) ([]byte, int, error) {
	return c.doFormWithHeaders(ctx, endpoint, authorization, form, nil)
}

func (c *GrokOAuthClient) doFormWithHeaders(ctx context.Context, endpoint, authorization string, form url.Values, headers http.Header) ([]byte, int, error) {
	method := http.MethodGet
	var reader io.Reader
	if form != nil {
		method = http.MethodPost
		reader = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, 0, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json")
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, maxGrokOAuthBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(body) > maxGrokOAuthBodyBytes {
		return nil, resp.StatusCode, errors.New("Grok OAuth response exceeded size limit")
	}
	return body, resp.StatusCode, nil
}

func parseGrokTokenResponse(body []byte, requireRefresh bool) (*GrokTokenResponse, error) {
	var token GrokTokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, errors.New("invalid Grok token response")
	}
	if token.AccessToken == "" || token.ExpiresIn <= 0 || (token.TokenType != "" && !strings.EqualFold(token.TokenType, "Bearer")) {
		return nil, errors.New("invalid Grok token response")
	}
	if requireRefresh && token.RefreshToken == "" {
		return nil, errors.New("Grok token response missing refresh token")
	}
	return &token, nil
}

type grokOAuthErrorInfo struct {
	Code        string
	Description string
}

func parseGrokOAuthError(body []byte) grokOAuthErrorInfo {
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil {
		return grokOAuthErrorInfo{}
	}
	info := grokOAuthErrorInfo{}
	switch value := parsed["error"].(type) {
	case string:
		info.Code = value
	case map[string]any:
		info.Code = firstGrokOAuthString(value, "code", "error")
		info.Description = firstGrokOAuthString(value, "error_description", "description", "message")
	}
	if info.Description == "" {
		info.Description = firstGrokOAuthString(parsed, "error_description", "description", "message")
	}
	return info
}

func firstGrokOAuthString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok {
			return value
		}
	}
	return ""
}

func grokOAuthStatusError(operation string, status int, body []byte, sensitive ...string) error {
	base := fmt.Sprintf("%s: authorization server returned HTTP %d", operation, status)
	if diagnostic := grokOAuthDiagnostic(parseGrokOAuthError(body), sensitive...); diagnostic != "" {
		base += ": " + diagnostic
	}
	return errors.New(base)
}

func wrapGrokOAuthSentinel(target error, info grokOAuthErrorInfo, sensitive ...string) error {
	if diagnostic := grokOAuthDiagnostic(info, sensitive...); diagnostic != "" {
		return fmt.Errorf("%w: %s", target, diagnostic)
	}
	return target
}

func grokOAuthDiagnostic(info grokOAuthErrorInfo, sensitive ...string) string {
	code := sanitizeGrokOAuthDiagnostic(info.Code, sensitive...)
	description := sanitizeGrokOAuthDiagnostic(info.Description, sensitive...)
	switch {
	case code != "" && description != "":
		return code + ": " + description
	case code != "":
		return code
	default:
		return description
	}
}

func sanitizeGrokOAuthDiagnostic(value string, sensitive ...string) string {
	value = strings.TrimSpace(value)
	value = grokOAuthLabeledSecretPattern.ReplaceAllString(value, "[redacted]")
	value = grokOAuthBearerSecretPattern.ReplaceAllString(value, "Bearer [redacted]")
	for _, secret := range sensitive {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	var b strings.Builder
	lastSpace := false
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			if !lastSpace && b.Len() > 0 {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		if r > 0x7e {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > maxGrokOAuthDiagnosticBytes {
			break
		}
		b.WriteRune(r)
		lastSpace = r == ' '
	}
	return strings.TrimSpace(b.String())
}

// ValidateGrokDeviceCodeForDisplay ensures device-flow values are safe to render
// in a terminal and that verification URLs remain on x.ai-owned HTTPS hosts.
func ValidateGrokDeviceCodeForDisplay(device *GrokDeviceCode) error {
	if device == nil || !validGrokDisplayText(device.UserCode, maxGrokDeviceDisplayTextBytes) {
		return errors.New("invalid Grok device response display text")
	}
	for i := 0; i < len(device.UserCode); i++ {
		c := device.UserCode[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
			return errors.New("invalid Grok device response user code")
		}
	}
	if err := validateGrokVerificationURL(device.VerificationURI); err != nil {
		return err
	}
	if device.VerificationURIComplete != "" {
		if err := validateGrokVerificationURL(device.VerificationURIComplete); err != nil {
			return err
		}
	}
	return nil
}

func validateGrokVerificationURL(value string) error {
	if !validGrokDisplayText(value, maxGrokDeviceURLBytes) {
		return errors.New("invalid Grok verification URL")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Host == "" {
		return errors.New("invalid Grok verification URL")
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "x.ai" && !strings.HasSuffix(host, ".x.ai") {
		return errors.New("invalid Grok verification URL host")
	}
	return nil
}

func validGrokDisplayText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || isGrokBidiControl(r) {
			return false
		}
	}
	return true
}

func isGrokBidiControl(r rune) bool {
	table := unicode.Properties["Bidi_Control"]
	return table != nil && unicode.Is(table, r)
}

func GrokExpiryUnix(now, expiresIn int64) (int64, error) {
	if expiresIn <= 0 || now > math.MaxInt64-expiresIn {
		return 0, errors.New("invalid Grok token expiration")
	}
	return now + expiresIn, nil
}

// ValidGrokAccountID accepts only bounded visible ASCII so userinfo.sub is safe
// to copy into x-grok-user-id/x-userid headers.
func ValidGrokAccountID(accountID string) bool {
	if len(accountID) == 0 || len(accountID) > 1024 {
		return false
	}
	for i := 0; i < len(accountID); i++ {
		if accountID[i] < 0x21 || accountID[i] > 0x7e {
			return false
		}
	}
	return true
}

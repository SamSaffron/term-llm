package responsesproviders

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/oauth"
)

func validGrokUUIDv4(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' {
		return false
	}
	variant := value[19]
	if variant != '8' && variant != '9' && variant != 'a' && variant != 'b' {
		return false
	}
	_, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	return err == nil
}

func grokSSE(body ...string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(strings.Join(body, "\n") + "\n"))}
}

type fakeGrokOAuthAPI struct {
	device      *oauth.GrokDeviceCode
	token       *oauth.GrokTokenResponse
	accountID   string
	refreshErr  error
	userInfoErr error
	revokeErr   error
	revoked     string
}

func (f *fakeGrokOAuthAPI) RequestDeviceCode(context.Context) (*oauth.GrokDeviceCode, error) {
	if f.device == nil {
		return nil, errors.New("unexpected device request")
	}
	return f.device, nil
}

func (f *fakeGrokOAuthAPI) PollDeviceToken(context.Context, *oauth.GrokDeviceCode) (*oauth.GrokTokenResponse, error) {
	if f.token == nil {
		return nil, errors.New("unexpected token poll")
	}
	return f.token, nil
}

func (f *fakeGrokOAuthAPI) RefreshToken(context.Context, string) (*oauth.GrokTokenResponse, error) {
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	if f.token == nil {
		return nil, errors.New("unexpected refresh")
	}
	return f.token, nil
}

func (f *fakeGrokOAuthAPI) UserInfo(context.Context, string) (string, error) {
	return f.accountID, f.userInfoErr
}

func (f *fakeGrokOAuthAPI) Revoke(_ context.Context, token string) error {
	f.revoked = token
	return f.revokeErr
}

func isolateGrokLLMTestEnv(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root+"/config")
	t.Setenv("XDG_DATA_HOME", root+"/data")
	t.Setenv("XDG_CACHE_HOME", root+"/cache")
}

func TestPromptForGrokAuthUsesInjectedClientAndRejectsNonInteractive(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	oldClient, oldInteractive, oldOutput, oldContext := grokOAuthClient, grokInteractiveTerminal, grokAuthOutput, grokAuthContext
	t.Cleanup(func() {
		grokOAuthClient, grokInteractiveTerminal, grokAuthOutput, grokAuthContext = oldClient, oldInteractive, oldOutput, oldContext
	})
	grokInteractiveTerminal = func() bool { return false }
	if _, err := PromptForGrokAuth(); err == nil || !strings.Contains(err.Error(), "non-interactive") {
		t.Fatalf("noninteractive error = %v", err)
	}

	var output bytes.Buffer
	grokInteractiveTerminal = func() bool { return true }
	grokAuthOutput = &output
	grokAuthContext = func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }
	grokOAuthClient = &fakeGrokOAuthAPI{device: &oauth.GrokDeviceCode{DeviceCode: "device", UserCode: "ABCD", VerificationURI: "https://accounts.x.ai.evil.test/device", ExpiresIn: 600, Interval: 5}}
	if _, err := PromptForGrokAuth(); err == nil || !strings.Contains(err.Error(), "verification URL host") {
		t.Fatalf("unsafe verification URL error = %v", err)
	}
	if strings.Contains(output.String(), "evil.test") {
		t.Fatalf("unsafe URL was displayed: %q", output.String())
	}
	output.Reset()

	fake := &fakeGrokOAuthAPI{
		device:    &oauth.GrokDeviceCode{DeviceCode: "device", UserCode: "ABCD", VerificationURI: "https://accounts.x.ai/device", VerificationURIComplete: "https://accounts.x.ai/device?code=ABCD", ExpiresIn: 600, Interval: 5},
		token:     &oauth.GrokTokenResponse{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600},
		accountID: "acct_1",
	}
	grokOAuthClient = fake
	creds, err := PromptForGrokAuth()
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccountID != "acct_1" || creds.AccessToken != "access" || !strings.Contains(output.String(), "https://accounts.x.ai/device") {
		t.Fatalf("credentials=%+v output=%q", creds, output.String())
	}
	stored, err := credentials.GetGrokCredentials()
	if err != nil || stored.RefreshToken != "refresh" {
		t.Fatalf("stored credentials=%+v err=%v", stored, err)
	}
}

func TestLogoutGrokRevocationFailureStillDeletesLocalCredentials(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	if err := credentials.SaveGrokCredentials(&credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}); err != nil {
		t.Fatal(err)
	}
	if err := saveGrokModelsCache(grokModelsCache{AccountID: "acct_1", FetchedAt: time.Now(), Models: []ModelInfo{{ID: "grok-4.6"}}}); err != nil {
		t.Fatal(err)
	}
	oldClient := grokOAuthClient
	fake := &fakeGrokOAuthAPI{revokeErr: errors.New("offline")}
	grokOAuthClient = fake
	t.Cleanup(func() { grokOAuthClient = oldClient })
	warning, err := LogoutGrok(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fake.revoked != "refresh" || !strings.Contains(warning, "revocation failed") || credentials.GrokCredentialsExist() {
		t.Fatalf("revoked=%q warning=%q credentialsExist=%v", fake.revoked, warning, credentials.GrokCredentialsExist())
	}
	cachePath, err := grokModelsCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("Grok model cache still exists after logout: %v", err)
	}
}

func TestGrokProvider401InvalidRefreshConditionallyClearsCredentialsWithGuidance(t *testing.T) {
	for _, tc := range []struct {
		name          string
		storedRefresh string
		wantExists    bool
	}{
		{name: "matching stale generation", storedRefresh: "stale-refresh"},
		{name: "concurrent login preserved", storedRefresh: "new-login-refresh", wantExists: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateGrokLLMTestEnv(t)
			stored := &credentials.GrokCredentials{AccessToken: "stale-access", RefreshToken: tc.storedRefresh, ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}
			if err := credentials.SaveGrokCredentials(stored); err != nil {
				t.Fatal(err)
			}
			oldHTTP, oldOAuth := grokHTTPClient, grokOAuthClient
			grokHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"expired"}`))}, nil
			})}
			grokOAuthClient = &fakeGrokOAuthAPI{refreshErr: oauth.ErrGrokRefreshTokenInvalid}
			t.Cleanup(func() { grokHTTPClient, grokOAuthClient = oldHTTP, oldOAuth })

			provider := NewGrokProviderWithCreds(&credentials.GrokCredentials{AccessToken: "stale-access", RefreshToken: "stale-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}, "grok-4.6")
			_, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
			if err == nil || !strings.Contains(err.Error(), "auth login grok") || !errors.Is(err, oauth.ErrGrokRefreshTokenInvalid) {
				t.Fatalf("401 recovery error = %v", err)
			}
			if got := credentials.GrokCredentialsExist(); got != tc.wantExists {
				t.Fatalf("credentials exist = %v, want %v", got, tc.wantExists)
			}
		})
	}
}

func TestNewGrokProviderPreflightInvalidRefreshClearsCredentials(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	if err := credentials.SaveGrokCredentials(&credentials.GrokCredentials{AccessToken: "expired", RefreshToken: "invalid-refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix(), AccountID: "acct_1"}); err != nil {
		t.Fatal(err)
	}
	oldClient, oldInteractive := grokOAuthClient, grokInteractiveTerminal
	grokOAuthClient = &fakeGrokOAuthAPI{refreshErr: oauth.ErrGrokRefreshTokenInvalid}
	grokInteractiveTerminal = func() bool { return false }
	t.Cleanup(func() { grokOAuthClient, grokInteractiveTerminal = oldClient, oldInteractive })
	_, err := NewGrokProvider("grok-4.6")
	if err == nil || !errors.Is(err, oauth.ErrGrokRefreshTokenInvalid) || !strings.Contains(err.Error(), "auth login grok") {
		t.Fatalf("preflight error = %v", err)
	}
	if credentials.GrokCredentialsExist() {
		t.Fatal("invalid preflight refresh did not clear stale credentials")
	}
}

func TestGrokCachedOutputLimitClampsRequest(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	creds := &credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}
	if err := credentials.SaveGrokCredentials(creds); err != nil {
		t.Fatal(err)
	}
	if err := saveGrokModelsCache(grokModelsCache{AccountID: "acct_1", FetchedAt: time.Now(), Models: []ModelInfo{{ID: "grok-4.6", InputLimit: 500_000, OutputLimit: 8_000}}}); err != nil {
		t.Fatal(err)
	}
	oldClient := grokHTTPClient
	defer func() { grokHTTPClient = oldClient }()
	var maxOutput float64
	grokHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		maxOutput, _ = payload["max_output_tokens"].(float64)
		return grokSSE(`event: response.completed`, `data: {"type":"response.completed","response":{}}`, `data: [DONE]`), nil
	})}
	provider := NewGrokProviderWithCreds(creds, "grok-4.6")
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}, MaxOutputTokens: 20_000})
	if err != nil {
		t.Fatal(err)
	}
	drainStreamToDone(t, stream)
	if maxOutput != 8_000 {
		t.Fatalf("max_output_tokens = %v, want 8000", maxOutput)
	}
}

func TestGrokProviderOmitsToolControlsWithoutTools(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	oldClient := grokHTTPClient
	defer func() { grokHTTPClient = oldClient }()

	var payload map[string]any
	grokHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return grokSSE(`event: response.completed`, `data: {"type":"response.completed","response":{}}`, `data: [DONE]`), nil
	})}

	creds := &credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}
	provider := NewGrokProviderWithCreds(creds, "grok-4.6")
	stream, err := provider.Stream(context.Background(), Request{
		Messages:   []Message{UserText("hi")},
		ToolChoice: ToolChoice{Mode: ToolChoiceAuto},
	})
	if err != nil {
		t.Fatal(err)
	}
	drainStreamToDone(t, stream)

	for _, field := range []string{"tool_choice", "parallel_tool_calls"} {
		if value, ok := payload[field]; ok {
			t.Errorf("payload includes %s=%v without tools: %v", field, value, payload)
		}
	}
}

func TestGrokProvider401ReplaysOnceWithConcurrentFreshCredentials(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	fresh := &credentials.GrokCredentials{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}
	if err := credentials.SaveGrokCredentials(fresh); err != nil {
		t.Fatal(err)
	}
	oldClient := grokHTTPClient
	defer func() { grokHTTPClient = oldClient }()
	var auth []string
	var requestIDs []string
	var bodies [][]byte
	grokHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		auth = append(auth, req.Header.Get("Authorization"))
		requestIDs = append(requestIDs, req.Header.Get("x-grok-req-id"))
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
		if len(auth) == 1 {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"expired"}`))}, nil
		}
		return grokSSE(`event: response.completed`, `data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`, `data: [DONE]`), nil
	})}
	provider := NewGrokProviderWithCreds(&credentials.GrokCredentials{AccessToken: "stale-access", RefreshToken: "stale-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}, "grok-4.6")
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	drainStreamToDone(t, stream)
	stream, err = provider.Stream(context.Background(), Request{Messages: []Message{UserText("second call")}})
	if err != nil {
		t.Fatal(err)
	}
	drainStreamToDone(t, stream)
	if len(auth) != 3 || auth[0] != "Bearer stale-access" || auth[1] != "Bearer fresh-access" || auth[2] != "Bearer fresh-access" {
		t.Fatalf("authorization attempts = %v", auth)
	}
	if !validGrokUUIDv4(requestIDs[0]) || requestIDs[0] != requestIDs[1] {
		t.Fatalf("auth retry request IDs = %v, want one preserved UUIDv4", requestIDs[:2])
	}
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("auth retry body changed:\nfirst:  %s\nsecond: %s", bodies[0], bodies[1])
	}
	if !validGrokUUIDv4(requestIDs[2]) || requestIDs[2] == requestIDs[0] {
		t.Fatalf("logical call request IDs = %v, want unique UUIDv4 values", requestIDs)
	}
}

func TestGrokProvider403DoesNotRefreshAnd429IsTyped(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	for _, tc := range []struct {
		status int
		check  func(error) bool
	}{{http.StatusForbidden, func(err error) bool {
		return strings.Contains(err.Error(), "subscription") && strings.Contains(err.Error(), "entitlement")
	}}, {http.StatusTooManyRequests, func(err error) bool {
		var rate *RateLimitError
		return errors.As(err, &rate) && rate.RetryAfter == 2*time.Second
	}}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			oldClient := grokHTTPClient
			defer func() { grokHTTPClient = oldClient }()
			calls := 0
			grokHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": {"2"}}, Body: io.NopCloser(strings.NewReader(`{"error":"denied"}`))}, nil
			})}
			provider := NewGrokProviderWithCreds(&credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}, "grok-4.6")
			_, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
			if err == nil || !tc.check(err) {
				t.Fatalf("error = %T %v", err, err)
			}
			if calls != 1 {
				t.Fatalf("requests = %d, want 1", calls)
			}
		})
	}
}

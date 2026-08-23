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
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/grokprotocol"
)

type grokRoundTripFunc func(*http.Request) (*http.Response, error)

func (f grokRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func grokHTTPResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func isolateGrokOAuthTestEnv(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root+"/config")
}

func TestGrokDeviceAuthorizationRequestAndPolling(t *testing.T) {
	isolateGrokOAuthTestEnv(t)
	if grokprotocol.ClientVersion != "1.0.6" || grokprotocol.ClientSurfaceCLI != "cli" {
		t.Fatalf("Grok protocol metadata = version %q surface %q", grokprotocol.ClientVersion, grokprotocol.ClientSurfaceCLI)
	}
	var calls int
	client := NewGrokOAuthClient(&http.Client{Transport: grokRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(req.Body)
		form, _ := url.ParseQuery(string(body))
		for name, want := range map[string]string{
			"x-grok-client-version": "1.0.6",
			"x-grok-client-surface": "cli",
		} {
			if got := req.Header.Get(name); got != want {
				t.Fatalf("request %d header %s = %q, want %q", calls, name, got, want)
			}
		}
		switch calls {
		case 1:
			if req.URL.String() != GrokDeviceEndpoint || form.Get("client_id") != GrokClientID || form.Get("scope") != GrokScopes || form.Get("referrer") != "term-llm" {
				t.Fatalf("device request url/form = %s %v", req.URL, form)
			}
			return grokHTTPResponse(200, `{"device_code":"secret-device","user_code":"ABCD","verification_uri":"https://auth.x.ai/device","verification_uri_complete":"https://auth.x.ai/device?code=ABCD","expires_in":600,"interval":2}`), nil
		case 2, 3, 4:
			if req.URL.String() != GrokTokenEndpoint || form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || form.Get("device_code") != "secret-device" {
				t.Fatalf("poll request url/form = %s %v", req.URL, form)
			}
			if calls == 2 {
				return grokHTTPResponse(400, `{"error":"authorization_pending"}`), nil
			}
			if calls == 3 {
				return grokHTTPResponse(400, `{"error":"slow_down"}`), nil
			}
			return grokHTTPResponse(200, `{"access_token":"access","refresh_token":"refresh","expires_in":3600,"token_type":"Bearer"}`), nil
		default:
			t.Fatalf("unexpected request %d", calls)
			return nil, nil
		}
	})})
	now := time.Unix(1000, 0)
	client.now = func() time.Time { return now }
	var waits []time.Duration
	client.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		now = now.Add(d)
		return nil
	}
	device, err := client.RequestDeviceCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	token, err := client.PollDeviceToken(context.Background(), device)
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "access" || token.RefreshToken != "refresh" || token.ExpiresIn != 3600 {
		t.Fatalf("token = %+v", token)
	}
	if len(waits) != 3 || waits[0] != 2*time.Second || waits[1] != 2*time.Second || waits[2] != 7*time.Second {
		t.Fatalf("poll waits = %v, want [2s 2s 7s]", waits)
	}
}

func TestGrokDevicePollingTerminalErrors(t *testing.T) {
	isolateGrokOAuthTestEnv(t)
	for _, tc := range []struct {
		code string
		want error
	}{{"access_denied", ErrGrokAccessDenied}, {"expired_token", ErrGrokDeviceCodeExpired}} {
		t.Run(tc.code, func(t *testing.T) {
			client := NewGrokOAuthClient(&http.Client{Transport: grokRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return grokHTTPResponse(400, `{"error":"`+tc.code+`"}`), nil
			})})
			client.sleep = func(context.Context, time.Duration) error { return nil }
			_, err := client.PollDeviceToken(context.Background(), &GrokDeviceCode{DeviceCode: "secret", ExpiresIn: 60, Interval: 1})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestGrokOAuthBoundsAndTokenValidation(t *testing.T) {
	isolateGrokOAuthTestEnv(t)
	client := NewGrokOAuthClient(&http.Client{Transport: grokRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return grokHTTPResponse(200, strings.Repeat("x", maxGrokOAuthBodyBytes+1)), nil
	})})
	if _, err := client.RequestDeviceCode(context.Background()); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized response error = %v", err)
	}
	for _, body := range []string{
		`{"access_token":"access","refresh_token":"refresh","expires_in":0,"token_type":"Bearer"}`,
		`{"access_token":"access","refresh_token":"refresh","expires_in":1,"token_type":"Basic"}`,
	} {
		if _, err := parseGrokTokenResponse([]byte(body), true); err == nil {
			t.Fatalf("accepted invalid token response %s", body)
		}
	}
}

func TestGrokExpiryUnixRequiresPositiveNonOverflowingLifetime(t *testing.T) {
	isolateGrokOAuthTestEnv(t)
	if got, err := GrokExpiryUnix(100, 60); err != nil || got != 160 {
		t.Fatalf("expiry = %d, %v", got, err)
	}
	for _, expires := range []int64{0, -1, math.MaxInt64} {
		if _, err := GrokExpiryUnix(100, expires); err == nil {
			t.Fatalf("accepted expires_in %d", expires)
		}
	}
}

func TestValidGrokAccountID(t *testing.T) {
	isolateGrokOAuthTestEnv(t)
	if !ValidGrokAccountID("acct_123") {
		t.Fatal("valid account rejected")
	}
	for _, value := range []string{"", "acct\r\ninjected", "with space", strings.Repeat("a", 1025)} {
		if ValidGrokAccountID(value) {
			t.Fatalf("unsafe account accepted: %q", value)
		}
	}
}

func TestGrokTokenTypeOptionalForLoginAndRefresh(t *testing.T) {
	isolateGrokOAuthTestEnv(t)
	for _, tc := range []struct {
		name       string
		tokenType  string
		wantErr    bool
		useRefresh bool
	}{
		{name: "login omitted", tokenType: ""},
		{name: "login bearer", tokenType: `,"token_type":"Bearer"`},
		{name: "login wrong", tokenType: `,"token_type":"Basic"`, wantErr: true},
		{name: "refresh omitted", tokenType: "", useRefresh: true},
		{name: "refresh bearer", tokenType: `,"token_type":"bearer"`, useRefresh: true},
		{name: "refresh wrong", tokenType: `,"token_type":"Basic"`, wantErr: true, useRefresh: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"access_token":"access","refresh_token":"refresh","expires_in":3600` + tc.tokenType + `}`
			client := NewGrokOAuthClient(&http.Client{Transport: grokRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return grokHTTPResponse(200, body), nil
			})})
			var err error
			if tc.useRefresh {
				_, err = client.RefreshToken(context.Background(), "old-refresh")
			} else {
				client.sleep = func(context.Context, time.Duration) error { return nil }
				_, err = client.PollDeviceToken(context.Background(), &GrokDeviceCode{DeviceCode: "device", ExpiresIn: 60, Interval: 1})
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestGrokRefreshTokenErrorMappings(t *testing.T) {
	isolateGrokOAuthTestEnv(t)
	for _, tc := range []struct {
		name        string
		body        string
		wantInvalid bool
		wantText    string
	}{
		{name: "invalid grant", body: `{"error":"invalid_grant","error_description":"refresh expired"}`, wantInvalid: true, wantText: "refresh expired"},
		{name: "invalid token", body: `{"error":"invalid_token"}`, wantInvalid: true},
		{name: "nested", body: `{"error":{"code":"invalid_grant","message":"rotation rejected"}}`, wantInvalid: true, wantText: "rotation rejected"},
		{name: "unknown", body: `{"error":"temporarily_unavailable","error_description":"try again later"}`, wantText: "temporarily_unavailable: try again later"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewGrokOAuthClient(&http.Client{Transport: grokRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return grokHTTPResponse(400, tc.body), nil
			})})
			_, err := client.RefreshToken(context.Background(), "secret-refresh")
			if err == nil {
				t.Fatal("expected refresh error")
			}
			if got := errors.Is(err, ErrGrokRefreshTokenInvalid); got != tc.wantInvalid {
				t.Fatalf("errors.Is(invalid) = %v, want %v: %v", got, tc.wantInvalid, err)
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error %q missing %q", err, tc.wantText)
			}
		})
	}
}

func TestGrokOAuthDiagnosticsAreBoundedSanitizedAndRedacted(t *testing.T) {
	isolateGrokOAuthTestEnv(t)
	description := "bad\u001b[31m refresh_token=secret-refresh " + strings.Repeat("x", 500)
	descriptionJSON, err := json.Marshal(description)
	if err != nil {
		t.Fatal(err)
	}
	client := NewGrokOAuthClient(&http.Client{Transport: grokRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return grokHTTPResponse(400, `{"error":{"code":"server_error","error_description":`+string(descriptionJSON)+`}}`), nil
	})})
	_, err = client.RefreshToken(context.Background(), "secret-refresh")
	if err == nil {
		t.Fatal("expected refresh error")
	}
	text := err.Error()
	if !strings.Contains(text, "server_error") || !strings.Contains(text, "[redacted]") {
		t.Fatalf("missing sanitized diagnostic: %q", text)
	}
	if strings.Contains(text, "secret-refresh") || strings.ContainsRune(text, '\x1b') || len(text) > 400 {
		t.Fatalf("unsafe diagnostic: %q", text)
	}
}

func TestGrokDeviceVerificationURLValidation(t *testing.T) {
	isolateGrokOAuthTestEnv(t)
	for _, tc := range []struct {
		name string
		uri  string
		code string
		ok   bool
	}{
		{name: "accounts", uri: "https://accounts.x.ai/device", code: "ABCD-EFGH", ok: true},
		{name: "ASCII lowercase", uri: "https://accounts.x.ai/device", code: "ab12-CD34", ok: true},
		{name: "auth subdomain", uri: "https://auth.x.ai/device", code: "ABCD", ok: true},
		{name: "user code space", uri: "https://accounts.x.ai/device", code: "ABCD EFGH"},
		{name: "user code punctuation", uri: "https://accounts.x.ai/device", code: "ABCD_EFGH"},
		{name: "user code non-ASCII", uri: "https://accounts.x.ai/device", code: "ABCD-é"},
		{name: "http", uri: "http://accounts.x.ai/device", code: "ABCD"},
		{name: "phishing suffix", uri: "https://accounts.x.ai.evil.test/device", code: "ABCD"},
		{name: "userinfo", uri: "https://user@accounts.x.ai/device", code: "ABCD"},
		{name: "ansi url", uri: "https://accounts.x.ai/device\x1b[31m", code: "ABCD"},
		{name: "bidi url", uri: "https://accounts.x.ai/device\u202eevil", code: "ABCD"},
		{name: "control code", uri: "https://accounts.x.ai/device", code: "ABCD\nPWN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"device_code":"device","user_code":%q,"verification_uri":%q,"expires_in":600,"interval":5}`, tc.code, tc.uri)
			client := NewGrokOAuthClient(&http.Client{Transport: grokRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return grokHTTPResponse(200, body), nil
			})})
			_, err := client.RequestDeviceCode(context.Background())
			if (err == nil) != tc.ok {
				t.Fatalf("error = %v, ok %v", err, tc.ok)
			}
		})
	}
}

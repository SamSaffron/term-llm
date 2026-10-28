package oauth

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRefreshTokenClassifiesOnlyInvalidGrantAsInvalid(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantInvalid bool
	}{
		{
			name:        "invalid grant",
			status:      http.StatusBadRequest,
			body:        `{"error":"invalid_grant","error_description":"refresh token already used"}`,
			wantInvalid: true,
		},
		{
			name:        "openai nested token_expired",
			status:      http.StatusUnauthorized,
			body:        `{"error":{"message":"Could not validate your token. Please try signing in again.","type":"invalid_request_error","param":null,"code":"token_expired"}}`,
			wantInvalid: true,
		},
		{
			name:        "bare 401 without body",
			status:      http.StatusUnauthorized,
			body:        ``,
			wantInvalid: true,
		},
		{
			name:        "transient server error",
			status:      http.StatusInternalServerError,
			body:        `{"error":"server_error","error_description":"try again later"}`,
			wantInvalid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldClient := oauthHTTPClient
			oauthHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: tt.status,
					Status:     http.StatusText(tt.status),
					Body:       io.NopCloser(strings.NewReader(tt.body)),
					Header:     make(http.Header),
				}, nil
			})}
			t.Cleanup(func() { oauthHTTPClient = oldClient })

			_, err := RefreshToken("refresh-token")
			if err == nil {
				t.Fatal("RefreshToken succeeded unexpectedly")
			}
			if got := errors.Is(err, ErrChatGPTRefreshTokenInvalid); got != tt.wantInvalid {
				t.Fatalf("errors.Is(invalid) = %v, want %v; error: %v", got, tt.wantInvalid, err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

package oauth

import (
	"context"
	"errors"
	"strings"
	"unicode"

	"golang.org/x/oauth2"
)

const maximumSafeErrSize = 300

// callbackError retains only recognized protocol codes, never server-provided
// error descriptions, URIs, or arbitrary callback text.
type callbackError struct{ code string }

func newCallbackError(code string) error {
	switch code {
	case "invalid_request", "unauthorized_client", "access_denied",
		"unsupported_response_type", "invalid_scope", "server_error",
		"temporarily_unavailable", "invalid_client", "invalid_grant",
		"unsupported_grant_type", "invalid_target":
		return &callbackError{code: code}
	default:
		return &callbackError{}
	}
}

func (e *callbackError) Error() string {
	if e.code == "" {
		return "authorization was denied"
	}
	return "authorization failed: " + e.code
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrAuthenticationRequired) {
		return "sign-in required"
	}
	if errors.Is(err, ErrRefreshRejected) {
		return "stored authorization expired or was revoked; sign in again"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "authorization expired"
	}
	if errors.Is(err, context.Canceled) {
		return "authorization canceled"
	}
	var callbackErr *callbackError
	if errors.As(err, &callbackErr) {
		return callbackErr.Error()
	}
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) {
		return "authorization server rejected the token request"
	}
	text := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, err.Error())
	text = strings.TrimSpace(text)
	if len(text) > maximumSafeErrSize {
		text = text[:maximumSafeErrSize] + "…"
	}
	return text
}

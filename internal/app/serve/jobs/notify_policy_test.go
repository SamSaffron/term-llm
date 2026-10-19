package jobs

import (
	"errors"
	"net/http"
	"testing"
	"time"

	appnotify "github.com/samsaffron/term-llm/internal/app/notify"
)

func TestTelegramAPIResponseErrorUsesHTTPStatusWhenErrorCodeMissing(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header)}
	err := appnotify.TelegramAPIResponseError(resp, appnotify.TelegramSendResponse{Description: "request rejected"}, []byte(`{"ok":false}`))

	var statusErr interface{ HTTPStatusCode() int }
	if !errors.As(err, &statusErr) {
		t.Fatalf("error type = %T, want HTTP status error", err)
	}
	if got := statusErr.HTTPStatusCode(); got != http.StatusOK {
		t.Fatalf("HTTP status = %d, want %d", got, http.StatusOK)
	}
	if NotifyErrorRetryable(err) {
		t.Fatal("Telegram API rejection without error_code classified as retryable")
	}
}

func TestTelegramAPIResponseErrorPreservesRetryAfter(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Status: "200 OK"}
	apiResp := appnotify.TelegramSendResponse{ErrorCode: http.StatusTooManyRequests, Description: "rate limited"}
	apiResp.Parameters.RetryAfter = 7
	err := appnotify.TelegramAPIResponseError(resp, apiResp, []byte(`{"ok":false}`))

	var retryAfterErr interface {
		RetryAfterDelay() (time.Duration, bool)
	}
	if !errors.As(err, &retryAfterErr) {
		t.Fatalf("error type = %T, want Retry-After metadata", err)
	}
	if delay, ok := retryAfterErr.RetryAfterDelay(); !ok || delay != 7*time.Second {
		t.Fatalf("Retry-After = %v, %v; want 7s, true", delay, ok)
	}
	if !NotifyErrorRetryable(err) {
		t.Fatal("Telegram 429 classified as permanent")
	}
}

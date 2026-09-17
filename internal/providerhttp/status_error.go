package providerhttp

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// StatusError captures a non-success HTTP response from a provider while
// preserving response headers for retry/backoff decisions (notably
// Retry-After). The Error text remains provider-oriented for user display.
type StatusError struct {
	Provider   string
	StatusCode int
	Status     string
	Body       string
	Header     http.Header
	Message    string
}

// NewStatusErrorFromResponse reads and closes resp.Body, then returns a typed
// status error. It is intended for the common non-2xx path where callers do not
// need to inspect the body separately.
func NewStatusErrorFromResponse(provider string, resp *http.Response) *StatusError {
	body := ReadBodyAndClose(resp, 0)
	return NewStatusError(provider, resp, body)
}

// NewStatusErrorMessageFromResponse reads and closes resp.Body, then returns a
// typed status error with a caller-built display message.
func NewStatusErrorMessageFromResponse(resp *http.Response, buildMessage func(body []byte) string) *StatusError {
	body := ReadBodyAndClose(resp, 0)
	message := ""
	if buildMessage != nil {
		message = buildMessage(body)
	}
	return NewStatusErrorMessage(message, resp, body)
}

// ReadBodyAndClose reads resp.Body and closes it. If limit is positive, at most
// limit bytes are returned. Read errors are intentionally ignored because status
// error bodies are best-effort diagnostics.
func ReadBodyAndClose(resp *http.Response, limit int64) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	defer resp.Body.Close()
	var reader io.Reader = resp.Body
	if limit > 0 {
		reader = io.LimitReader(resp.Body, limit)
	}
	body, _ := io.ReadAll(reader)
	return body
}

// NewStatusError returns a typed status error using the default
// "<provider> API error (status N): body" message shape. If provider is empty,
// the message starts with "API error".
func NewStatusError(provider string, resp *http.Response, body []byte) *StatusError {
	return NewStatusErrorString(provider, responseStatusCode(resp), responseStatus(resp), responseHeader(resp), string(body))
}

// NewStatusErrorString is like NewStatusError but accepts already-normalized
// body text.
func NewStatusErrorString(provider string, statusCode int, status string, header http.Header, body string) *StatusError {
	return &StatusError{
		Provider:   provider,
		StatusCode: statusCode,
		Status:     status,
		Body:       body,
		Header:     cloneHeader(header),
	}
}

// NewStatusErrorMessage returns a typed status error with a caller-supplied
// display message. Use this when existing code has a more specific error string
// that should be preserved while still attaching status/header metadata.
func NewStatusErrorMessage(message string, resp *http.Response, body []byte) *StatusError {
	return NewStatusErrorMessageString(message, responseStatusCode(resp), responseStatus(resp), responseHeader(resp), string(body))
}

// NewStatusErrorMessagef returns a typed status error with a formatted display
// message.
func NewStatusErrorMessagef(resp *http.Response, body []byte, format string, args ...any) *StatusError {
	return NewStatusErrorMessage(fmt.Sprintf(format, args...), resp, body)
}

// NewStatusErrorMessageString is like NewStatusErrorMessage but accepts
// already-normalized body text.
func NewStatusErrorMessageString(message string, statusCode int, status string, header http.Header, body string) *StatusError {
	return &StatusError{
		StatusCode: statusCode,
		Status:     status,
		Body:       body,
		Header:     cloneHeader(header),
		Message:    message,
	}
}

func (e *StatusError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message != "" {
		return e.Message
	}
	body := strings.TrimSpace(e.Body)
	if e.Provider != "" {
		if body != "" {
			return fmt.Sprintf("%s API error (status %d): %s", e.Provider, e.StatusCode, body)
		}
		return fmt.Sprintf("%s API error (status %d)", e.Provider, e.StatusCode)
	}
	if body != "" {
		return fmt.Sprintf("API error (status %d): %s", e.StatusCode, body)
	}
	return fmt.Sprintf("API error (status %d)", e.StatusCode)
}

// HTTPStatusCode exposes the status code for generic retryability checks.
func (e *StatusError) HTTPStatusCode() int {
	if e == nil {
		return 0
	}
	return e.StatusCode
}

// RetryAfterDelay exposes Retry-After metadata for generic retry/backoff logic.
func (e *StatusError) RetryAfterDelay() (time.Duration, bool) {
	if e == nil {
		return 0, false
	}
	return ParseRetryAfter(e.Header, time.Now())
}

// RetryableStatus reports whether statusCode is commonly transient and worth
// retrying for provider/API calls.
func RetryableStatus(statusCode int) bool {
	if statusCode >= http.StatusInternalServerError && statusCode <= 599 {
		return true
	}

	switch statusCode {
	case http.StatusRequestTimeout, // 408
		http.StatusTooEarly,        // 425
		http.StatusTooManyRequests: // 429
		return true
	}
	return false
}

// ParseRetryAfter parses Retry-After metadata from HTTP headers. It supports:
//   - Retry-After-Ms / retry-after-ms: integer milliseconds
//   - Retry-After: integer seconds
//   - Retry-After: HTTP-date
func ParseRetryAfter(header http.Header, now time.Time) (time.Duration, bool) {
	if header == nil {
		return 0, false
	}
	if wait, ok := ParseRetryAfterMillisecondsValue(getHeader(header, "Retry-After-Ms")); ok {
		return wait, true
	}
	return ParseRetryAfterValue(getHeader(header, "Retry-After"), now)
}

func getHeader(header http.Header, name string) string {
	if value := header.Get(name); value != "" {
		return value
	}
	for key, values := range header {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

// ParseRetryAfterMillisecondsValue parses a Retry-After-Ms header value.
func ParseRetryAfterMillisecondsValue(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	ms, err := strconv.ParseInt(firstRetryAfterToken(value), 10, 64)
	if err != nil || ms <= 0 {
		return 0, false
	}
	return boundedDuration(ms, time.Millisecond), true
}

// ParseRetryAfterValue parses a Retry-After header value as either seconds or
// an HTTP-date. now is used for deterministic tests; pass a zero time to use
// time.Now().
func ParseRetryAfterValue(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(firstRetryAfterToken(value), 10, 64); err == nil && secs > 0 {
		return boundedDuration(secs, time.Second), true
	}
	if when, err := http.ParseTime(value); err == nil {
		if now.IsZero() {
			now = time.Now()
		}
		wait := when.Sub(now)
		if wait > 0 {
			return wait, true
		}
	}
	return 0, false
}

// Saturate instead of wrapping a large server delay into an earlier retry.
func boundedDuration(value int64, unit time.Duration) time.Duration {
	const max = time.Duration(1<<63 - 1)
	if value > int64(max/unit) {
		return max
	}
	return time.Duration(value) * unit
}

func firstRetryAfterToken(value string) string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return ""
	}
	return strings.Trim(fields[0], ",;.")
}

func responseStatusCode(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func responseStatus(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	return resp.Status
}

func responseHeader(resp *http.Response) http.Header {
	if resp == nil {
		return nil
	}
	return resp.Header
}

func cloneHeader(header http.Header) http.Header {
	if header == nil {
		return nil
	}
	return header.Clone()
}

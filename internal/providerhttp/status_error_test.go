package providerhttp

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStatusErrorPreservesHeadersAndParsesRetryAfter(t *testing.T) {
	header := http.Header{}
	header.Set("Retry-After", "7")
	err := NewStatusErrorString("Venice", http.StatusTooManyRequests, "429 Too Many Requests", header, "slow down")

	if !strings.Contains(err.Error(), "Venice API error (status 429): slow down") {
		t.Fatalf("unexpected error text: %q", err.Error())
	}
	if err.HTTPStatusCode() != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatusCode = %d", err.HTTPStatusCode())
	}
	wait, ok := err.RetryAfterDelay()
	if !ok || wait != 7*time.Second {
		t.Fatalf("RetryAfterDelay = %s, %v; want 7s true", wait, ok)
	}

	header.Set("Retry-After", "99")
	wait, ok = err.RetryAfterDelay()
	if !ok || wait != 7*time.Second {
		t.Fatalf("RetryAfterDelay changed after source header mutation: %s, %v", wait, ok)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Minute).UTC().Format(http.TimeFormat)
	past := now.Add(-time.Minute).UTC().Format(http.TimeFormat)

	cases := []struct {
		name   string
		header http.Header
		want   time.Duration
		ok     bool
	}{
		{
			name:   "seconds",
			header: http.Header{"Retry-After": {"12"}},
			want:   12 * time.Second,
			ok:     true,
		},
		{
			name:   "seconds with suffix text",
			header: http.Header{"Retry-After": {"12 seconds"}},
			want:   12 * time.Second,
			ok:     true,
		},
		{
			name:   "http date",
			header: http.Header{"Retry-After": {future}},
			want:   2 * time.Minute,
			ok:     true,
		},
		{
			name:   "retry after ms preferred",
			header: http.Header{"Retry-After-Ms": {"250"}, "Retry-After": {"12"}},
			want:   250 * time.Millisecond,
			ok:     true,
		},
		{
			name:   "lowercase retry after ms",
			header: http.Header{"retry-after-ms": {"125"}},
			want:   125 * time.Millisecond,
			ok:     true,
		},
		{
			name:   "invalid",
			header: http.Header{"Retry-After": {"nonsense"}},
			ok:     false,
		},
		{
			name:   "past date",
			header: http.Header{"Retry-After": {past}},
			ok:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRetryAfter(tc.header, now)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("ParseRetryAfter = %s, %v; want %s, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestRetryableStatus(t *testing.T) {
	for _, code := range []int{408, 425, 429, 500, 501, 502, 503, 504, 520, 524, 525, 599} {
		if !RetryableStatus(code) {
			t.Fatalf("RetryableStatus(%d) = false, want true", code)
		}
	}
	for _, code := range []int{400, 401, 403, 404, 409, 418, 600} {
		if RetryableStatus(code) {
			t.Fatalf("RetryableStatus(%d) = true, want false", code)
		}
	}
}

func TestRetryAfterSaturatesOverflow(t *testing.T) {
	for _, name := range []string{"Retry-After", "Retry-After-Ms"} {
		delay, ok := ParseRetryAfter(http.Header{name: {"9223372036854775807"}}, time.Now())
		if !ok || delay != time.Duration(1<<63-1) {
			t.Fatalf("%s: %v, %v", name, delay, ok)
		}
	}
}

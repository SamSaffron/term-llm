package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	appnotify "github.com/samsaffron/term-llm/internal/app/notify"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestSendCompletionPushFallback(t *testing.T) {
	for _, tt := range []struct {
		name        string
		outcome     string
		firstStatus int
		firstErr    error
		finalStatus int
		largeRoute  bool
		wantCalls   int
	}{
		{name: "local overflow", outcome: "completed", firstErr: fmt.Errorf("encrypt: %w", webpush.ErrMaxPadExceeded), finalStatus: http.StatusCreated, wantCalls: 2},
		{name: "relay overflow", outcome: "failed", firstStatus: http.StatusRequestEntityTooLarge, firstErr: fmt.Errorf("HTTP 413"), finalStatus: http.StatusCreated, wantCalls: 2},
		{name: "transient fallback failure", outcome: "completed", firstErr: webpush.ErrMaxPadExceeded, finalStatus: http.StatusServiceUnavailable, wantCalls: 2},
		{name: "irreducible route", outcome: "completed", firstErr: webpush.ErrMaxPadExceeded, largeRoute: true, wantCalls: 1},
		{name: "unrelated failure", outcome: "completed", firstStatus: http.StatusServiceUnavailable, firstErr: fmt.Errorf("HTTP 503"), wantCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := completionPushPayload{
				Version: 1, EventID: "completion:response:subscription", ResponseID: "response",
				SessionID: "session", Outcome: tt.outcome, Title: strings.Repeat("节点<&😀", 1000),
				Body: strings.Repeat("body", 1000), URL: "/hub/nodes/artist/ui/chat/session", CreatedAt: "2026-10-21T00:00:00Z",
			}
			if tt.largeRoute {
				original.URL += strings.Repeat("x", appnotify.WebPushRecordSize)
			}
			payload, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			opts := appnotify.NewWebPushOptions("public", "private", "test@example.com", 300)
			originalOpts := *opts
			sub := &session.PushSubscription{ID: "subscription"}
			calls := 0
			send := func(_ context.Context, gotSub *session.PushSubscription, body []byte, gotOpts *webpush.Options) (int, time.Duration, error) {
				calls++
				if gotSub != sub {
					t.Fatal("fallback changed subscription")
				}
				if calls == 1 {
					return tt.firstStatus, 0, tt.firstErr
				}
				var compact completionPushPayload
				if err := json.Unmarshal(body, &compact); err != nil {
					t.Fatalf("fallback is not valid JSON: %v", err)
				}
				wantTitle, wantBody := "Response complete", "Your term-llm response is ready."
				if tt.outcome == "failed" {
					wantTitle, wantBody = "Response failed", "Your term-llm response stopped with an error."
				}
				if compact.Title != wantTitle || compact.Body != wantBody {
					t.Errorf("fallback display text = %q / %q", compact.Title, compact.Body)
				}
				compact.Title, compact.Body = original.Title, original.Body
				if compact != original {
					t.Errorf("fallback changed identity or routing metadata: %+v", compact)
				}
				if gotOpts.RecordSize != uint32(len(body)+completionPushEncryptionOverhead) || gotOpts.RecordSize >= opts.RecordSize {
					t.Errorf("fallback record size = %d, JSON size = %d", gotOpts.RecordSize, len(body))
				}
				wantOpts := originalOpts
				wantOpts.RecordSize = gotOpts.RecordSize
				if !reflect.DeepEqual(*gotOpts, wantOpts) {
					t.Errorf("fallback changed transport options: %+v", gotOpts)
				}
				if tt.finalStatus >= 400 {
					return tt.finalStatus, time.Minute, fmt.Errorf("HTTP %d", tt.finalStatus)
				}
				return tt.finalStatus, 0, nil
			}
			status, retryAfter, sendErr := sendCompletionPush(context.Background(), sub, payload, opts, send)
			if calls != tt.wantCalls {
				t.Fatalf("send calls = %d, want %d", calls, tt.wantCalls)
			}
			if tt.wantCalls == 1 {
				if status != tt.firstStatus || sendErr != tt.firstErr {
					t.Errorf("original failure changed: %d / %v", status, sendErr)
				}
			} else if status != tt.finalStatus || (sendErr != nil) != (tt.finalStatus >= 400) {
				t.Errorf("fallback result = %d / %v", status, sendErr)
			}
			if tt.finalStatus >= 400 && retryAfter != time.Minute {
				t.Errorf("fallback retry-after = %s, want 1m", retryAfter)
			}
			if !reflect.DeepEqual(*opts, originalOpts) {
				t.Error("fallback mutated shared transport options")
			}
		})
	}
}

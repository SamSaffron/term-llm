package live

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestOpenAISidebandUsesPublicURLAndBearerAuth(t *testing.T) {
	gotRequest := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCopy := r.Clone(r.Context())
		gotRequest <- requestCopy
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{
			"type":  "conversation.item.input_audio_transcription.delta",
			"delta": "hello",
		})
		<-time.After(100 * time.Millisecond)
	}))
	defer server.Close()

	channel, err := dialOpenAISideband(context.Background(), openAISidebandConfig{
		BaseURL: server.URL + "/v1",
		CallID:  "rtc_abc/unsafe?",
		APIKey:  "sk-public",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()

	event := receiveEvent(t, channel.Events())
	if event.Kind != EventUserTranscript || event.Text != "hello" {
		t.Fatalf("event = %+v", event)
	}
	r := <-gotRequest
	if r.URL.Path != "/v1/realtime" || r.URL.Query().Get("call_id") != "rtc_abc/unsafe?" {
		t.Fatalf("sideband URL = %s", r.URL.String())
	}
	if got := r.Header.Get("Authorization"); got != "Bearer sk-public" {
		t.Fatalf("authorization = %q", got)
	}
	if r.Header.Get("OpenAI-Alpha") != "" || r.Header.Get("OpenAI-Beta") != "" || r.Header.Get("ChatGPT-Account-ID") != "" {
		t.Fatalf("public sideband carried proprietary headers: %v", r.Header)
	}
}

func TestOpenAILiveSidebandUsesAttachURLBearerAuthAndLiveParser(t *testing.T) {
	gotRequest := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequest <- r.Clone(r.Context())
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{
			"type":     "session.input_transcript.delta",
			"delta":    "hello",
			"start_ms": 10,
			"end_ms":   20,
		})
		<-time.After(100 * time.Millisecond)
	}))
	defer server.Close()

	channel, err := dialOpenAISideband(context.Background(), openAISidebandConfig{
		BaseURL:    server.URL + "/v1",
		CallID:     "live_abc",
		APIKey:     "sk-public",
		Endpoint:   openAILiveSidebandURL,
		ParseEvent: parseOpenAILiveEvent,
		Protocol:   "GPT-Live",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()

	event := receiveEvent(t, channel.Events())
	if event.Kind != EventUserTranscript || event.Text != "hello" {
		t.Fatalf("event = %+v", event)
	}
	r := <-gotRequest
	if r.URL.Path != "/v1/live/sessions/live_abc/attach" || r.URL.RawQuery != "" {
		t.Fatalf("sideband URL = %s", r.URL.String())
	}
	if got := r.Header.Get("Authorization"); got != "Bearer sk-public" {
		t.Fatalf("authorization = %q", got)
	}
	if r.Header.Get("OpenAI-Alpha") != "" || r.Header.Get("OpenAI-Beta") != "" || r.Header.Get("ChatGPT-Account-ID") != "" {
		t.Fatalf("GPT-Live sideband carried proprietary headers: %v", r.Header)
	}
}
func TestOpenAISidebandReconnectsAndSends(t *testing.T) {
	var attempts atomic.Int32
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		if attempts.Add(1) == 1 {
			_ = conn.Close()
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err == nil {
			received <- string(payload)
		}
	}))
	defer server.Close()

	channel, err := dialOpenAISideband(context.Background(), openAISidebandConfig{
		BaseURL: server.URL,
		CallID:  "rtc_1",
		APIKey:  "key",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Give the read loop a chance to observe the first close; Send then waits for
	// the replacement socket rather than writing successfully to a dead one.
	for attempts.Load() < 2 {
		select {
		case <-ctx.Done():
			t.Fatal("sideband did not reconnect")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := channel.Send(ctx, openAIClientEvent{Type: "response.create"}); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-received:
		if !strings.Contains(payload, `"response.create"`) {
			t.Fatalf("payload = %s", payload)
		}
	case <-ctx.Done():
		t.Fatal("reconnected sideband did not receive message")
	}
}

func TestDialOpenAISidebandClassifiesHandshakeFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		want   error
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, want: ErrUnauthorized},
		{name: "forbidden", status: http.StatusForbidden, want: ErrForbidden},
		{name: "gone", status: http.StatusGone, want: errOpenAISidebandEnded},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
			}))
			defer server.Close()
			_, err := dialOpenAISideband(context.Background(), openAISidebandConfig{
				BaseURL: server.URL,
				CallID:  "rtc_1",
				APIKey:  "bad",
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want errors.Is %v", err, test.want)
			}
		})
	}
}

func TestOpenAISidebandURLValidation(t *testing.T) {
	got, err := openAISidebandURL("https://api.openai.com/v1/", "rtc_1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "wss://api.openai.com/v1/realtime?call_id=rtc_1" {
		t.Fatalf("URL = %q", got)
	}
	liveURL, err := openAILiveSidebandURL("https://api.openai.com/v1/", "live_1")
	if err != nil {
		t.Fatal(err)
	}
	if liveURL != "wss://api.openai.com/v1/live/sessions/live_1/attach" {
		t.Fatalf("Live URL = %q", liveURL)
	}
	if _, err := openAISidebandURL("https://api.openai.com/v1", " "); err == nil {
		t.Fatal("empty call ID was accepted")
	}
	if _, err := openAILiveSidebandURL("https://api.openai.com/v1", " "); err == nil {
		t.Fatal("empty Live session ID was accepted")
	}
}

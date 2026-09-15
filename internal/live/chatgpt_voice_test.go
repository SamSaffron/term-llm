package live

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestChatGPTSessionSetVoiceWaitsForMatchingAckAndPreservesEvents(t *testing.T) {
	received := make(chan []byte, 1)
	allowAck := make(chan struct{})
	server := sidebandServer(t, func(conn *websocket.Conn, _ int, _ http.Header) {
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		received <- payload
		_ = conn.WriteJSON(map[string]any{
			"type":    "session.updated",
			"session": map[string]any{"audio": map[string]any{"output": map[string]any{"voice": "cove"}}},
		})
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"output_transcript.added","item":{"text":"still here"}}`))
		<-allowAck
		_ = conn.WriteJSON(map[string]any{
			"type":    "session.updated",
			"session": map[string]any{"audio": map[string]any{"output": map[string]any{"voice": "maple"}}},
		})
		_, _, _ = conn.ReadMessage()
	}, nil)
	defer server.Close()

	channel := dialTestSideband(t, server)
	defer channel.Close()
	session := &chatGPTSession{channel: channel}
	result := make(chan error, 1)
	go func() { result <- session.SetVoice(context.Background(), " maple ") }()

	payload := <-received
	if string(payload) != `{"type":"session.update","session":{"audio":{"output":{"voice":"maple"}}}}` {
		t.Fatalf("voice update = %s", payload)
	}
	first := receiveEvent(t, channel.Events())
	if first.Kind != EventSessionUpdated || first.Voice != "cove" {
		t.Fatalf("mismatched update event = %+v", first)
	}
	second := receiveEvent(t, channel.Events())
	if second.Kind != EventAssistantTranscript || second.Text != "still here" {
		t.Fatalf("normal event was lost while awaiting voice ACK: %+v", second)
	}
	select {
	case err := <-result:
		t.Fatalf("SetVoice returned before matching ACK: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	close(allowAck)
	if err := <-result; err != nil {
		t.Fatalf("SetVoice: %v", err)
	}
	ack := receiveEvent(t, channel.Events())
	if ack.Kind != EventSessionUpdated || ack.Voice != "maple" {
		t.Fatalf("matching ACK was stolen from normal events: %+v", ack)
	}
	if session.CurrentVoice() != "maple" {
		t.Fatalf("current voice = %q", session.CurrentVoice())
	}
}

func TestChatGPTSessionSetVoiceReturnsProviderErrorWithoutStealingIt(t *testing.T) {
	server := sidebandServer(t, func(conn *websocket.Conn, _ int, _ http.Header) {
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"message":"voice cannot change now"}}`))
		_, _, _ = conn.ReadMessage()
	}, nil)
	defer server.Close()

	channel := dialTestSideband(t, server)
	defer channel.Close()
	session := &chatGPTSession{channel: channel}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := session.SetVoice(ctx, "spruce"); err == nil || !strings.Contains(err.Error(), "voice cannot change now") {
		t.Fatalf("SetVoice error = %v", err)
	}
	event := receiveEvent(t, channel.Events())
	if event.Kind != EventError || event.Text != "voice cannot change now" {
		t.Fatalf("provider error was stolen from normal events: %+v", event)
	}
	if !event.ErrorHandled {
		t.Fatal("operation error would also fail the conversational UI")
	}
}

func TestChatGPTSessionSetVoiceSerializesAfterCanceledUpdateUntilLateAck(t *testing.T) {
	messages := make(chan []byte, 2)
	allowFirstAck := make(chan struct{})
	server := sidebandServer(t, func(conn *websocket.Conn, _ int, _ http.Header) {
		defer conn.Close()
		_, first, err := conn.ReadMessage()
		if err != nil {
			return
		}
		messages <- first

		secondRead := make(chan []byte, 1)
		go func() {
			_, second, readErr := conn.ReadMessage()
			if readErr == nil {
				secondRead <- second
			}
		}()
		<-allowFirstAck
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.updated","session":{"audio":{"output":{"voice":"maple"}}}}`))
		second := <-secondRead
		messages <- second
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.updated","session":{"audio":{"output":{"voice":"spruce"}}}}`))
		_, _, _ = conn.ReadMessage()
	}, nil)
	defer server.Close()

	channel := dialTestSideband(t, server)
	defer channel.Close()
	session := &chatGPTSession{channel: channel}

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() { firstResult <- session.SetVoice(firstCtx, "maple") }()
	first := <-messages
	if !voiceMessageHas(t, first, "maple") {
		t.Fatalf("first update = %s", first)
	}
	cancelFirst()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled SetVoice = %v", err)
	}

	secondResult := make(chan error, 1)
	go func() { secondResult <- session.SetVoice(context.Background(), "spruce") }()
	select {
	case second := <-messages:
		t.Fatalf("second update %s was sent before the late first ACK", second)
	case <-time.After(30 * time.Millisecond):
	}
	close(allowFirstAck)
	second := <-messages
	if !voiceMessageHas(t, second, "spruce") {
		t.Fatalf("second update = %s", second)
	}
	if err := <-secondResult; err != nil {
		t.Fatalf("second SetVoice: %v", err)
	}
	if session.CurrentVoice() != "spruce" {
		t.Fatalf("voice after late ACK = %q", session.CurrentVoice())
	}
}

func TestChatGPTSessionSetVoiceHandlesValidationAndClose(t *testing.T) {
	session := &chatGPTSession{}
	for _, voice := range []string{"", "marin"} {
		if err := session.SetVoice(context.Background(), voice); err == nil {
			t.Fatalf("SetVoice(%q) succeeded", voice)
		}
	}

	requestRead := make(chan struct{})
	server := sidebandServer(t, func(conn *websocket.Conn, _ int, _ http.Header) {
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err == nil {
			close(requestRead)
		}
		_, _, _ = conn.ReadMessage()
	}, nil)
	defer server.Close()
	channel := dialTestSideband(t, server)
	liveSession := &chatGPTSession{channel: channel}
	result := make(chan error, 1)
	go func() { result <- liveSession.SetVoice(context.Background(), "cove") }()
	<-requestRead
	channel.Close()
	select {
	case err := <-result:
		if !errors.Is(err, errSidebandEnded) {
			t.Fatalf("SetVoice after close = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SetVoice did not unblock on close")
	}
}

func dialTestSideband(t *testing.T, server *httptest.Server) *sideband {
	t.Helper()
	channel, err := dialSideband(context.Background(), sidebandConfig{
		BaseURL: wsURL(t, server), CallID: "rtc_voice", SessionID: "sess_voice",
		Auth: func(context.Context, bool) (Auth, error) { return testAuth(), nil },
	})
	if err != nil {
		t.Fatalf("dialSideband: %v", err)
	}
	return channel
}

func voiceMessageHas(t *testing.T, payload []byte, voice string) bool {
	t.Helper()
	var message struct {
		Type    string `json:"type"`
		Session struct {
			Audio struct {
				Output struct {
					Voice string `json:"voice"`
				} `json:"output"`
			} `json:"audio"`
		} `json:"session"`
	}
	if err := json.Unmarshal(payload, &message); err != nil {
		t.Fatalf("decode voice update: %v", err)
	}
	return message.Type == "session.update" && message.Session.Audio.Output.Voice == voice
}

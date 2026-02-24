package chat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	servechat "github.com/samsaffron/term-llm/internal/serve/chat"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

// RemoteBackend connects to a term-llm chat server via WebSocket.
// Implements StreamBackend.
type RemoteBackend struct {
	url   string
	token string
	agent string
	conn  *websocket.Conn

	sendCh   chan servechat.ClientEvent
	requestCh chan any

	mu       sync.Mutex
	streamCh chan ui.StreamEvent
}

// NewRemoteBackend creates a RemoteBackend and opens the WebSocket connection.
func NewRemoteBackend(urlStr, token, agent string) (*RemoteBackend, error) {
	wsURL, err := normalizeWSURL(urlStr)
	if err != nil {
		return nil, err
	}

	headers := http.Header{}
	if strings.TrimSpace(token) != "" {
		headers.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		return nil, err
	}

	var ready servechat.WireEvent
	if err := conn.ReadJSON(&ready); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read session_ready: %w", err)
	}
	if ready.Type != "session_ready" {
		_ = conn.Close()
		return nil, fmt.Errorf("unexpected event: %s", ready.Type)
	}

	backend := &RemoteBackend{
		url:       wsURL,
		token:     token,
		agent:     agent,
		conn:      conn,
		sendCh:    make(chan servechat.ClientEvent, 32),
		requestCh: make(chan any, 16),
	}

	go backend.writeLoop()
	go backend.readLoop()

	return backend, nil
}

// Requests returns a channel of backend request messages for the TUI.
func (r *RemoteBackend) Requests() <-chan any {
	return r.requestCh
}

// SendMessage sends a user message and returns a stream event channel.
func (r *RemoteBackend) SendMessage(ctx context.Context, text string) (<-chan ui.StreamEvent, error) {
	if r == nil {
		return nil, errors.New("remote backend is nil")
	}
	r.mu.Lock()
	if r.streamCh != nil {
		close(r.streamCh)
	}
	r.streamCh = make(chan ui.StreamEvent, ui.DefaultStreamBufferSize)
	streamCh := r.streamCh
	r.mu.Unlock()

	r.sendCh <- servechat.ClientEvent{Type: "message", Text: text}
	return streamCh, nil
}

// Interrupt cancels the current stream.
func (r *RemoteBackend) Interrupt() {
	if r == nil {
		return
	}
	r.sendCh <- servechat.ClientEvent{Type: "interrupt"}
}

// Reset clears the conversation state.
func (r *RemoteBackend) Reset() {
	if r == nil {
		return
	}
	r.sendCh <- servechat.ClientEvent{Type: "reset"}
}

func (r *RemoteBackend) writeLoop() {
	for ev := range r.sendCh {
		if r.conn == nil {
			return
		}
		if err := r.conn.WriteJSON(ev); err != nil {
			return
		}
	}
}

func (r *RemoteBackend) readLoop() {
	for {
		if r.conn == nil {
			return
		}
		var ev servechat.WireEvent
		if err := r.conn.ReadJSON(&ev); err != nil {
			r.closeStream(err)
			return
		}
		r.handleWireEvent(ev)
	}
}

func (r *RemoteBackend) handleWireEvent(ev servechat.WireEvent) {
	switch ev.Type {
	case "session_ready":
		return
	case "catchup":
		for _, item := range ev.Events {
			r.handleWireEvent(item)
		}
		return
	case "approval_request":
		r.handleApprovalRequest(ev)
		return
	case "ask_user_request":
		r.handleAskUserRequest(ev)
		return
	}

	streamEv, ok := wireToStreamEvent(ev)
	if !ok {
		return
	}
	r.emitStreamEvent(streamEv, ev.Type == "message_done" || ev.Type == "error")
}

func (r *RemoteBackend) handleApprovalRequest(ev servechat.WireEvent) {
	doneCh := make(chan tools.ApprovalResult, 1)
	request := ApprovalRequestMsg{
		Path:    ev.Description,
		IsWrite: ev.IsWrite,
		IsShell: ev.IsShell,
		DoneCh:  doneCh,
	}
	r.requestCh <- request

	go func() {
		result := <-doneCh
		approved := result.Choice != tools.ApprovalChoiceDeny && result.Choice != tools.ApprovalChoiceCancelled && !result.Cancelled
		r.sendCh <- servechat.ClientEvent{Type: "approval_response", RequestID: ev.RequestID, Approved: approved}
	}()
}

func (r *RemoteBackend) handleAskUserRequest(ev servechat.WireEvent) {
	questions := make([]tools.AskUserQuestion, 0, len(ev.Questions))
	for _, q := range ev.Questions {
		opts := make([]tools.AskUserOption, 0, len(q.Options))
		for _, opt := range q.Options {
			opts = append(opts, tools.AskUserOption{Label: opt.Label, Description: opt.Description})
		}
		questions = append(questions, tools.AskUserQuestion{
			Header:      q.Header,
			Question:    q.Question,
			Options:     opts,
			MultiSelect: q.MultiSelect,
		})
	}

	doneCh := make(chan []tools.AskUserAnswer, 1)
	request := AskUserRequestMsg{Questions: questions, DoneCh: doneCh}
	r.requestCh <- request

	go func() {
		answers := <-doneCh
		responses := make([]servechat.AskUserAnswer, 0, len(answers))
		for _, ans := range answers {
			selected := ans.SelectedList
			if len(selected) == 0 && ans.Selected != "" {
				selected = []string{ans.Selected}
			}
			responses = append(responses, servechat.AskUserAnswer{
				QuestionIndex: ans.QuestionIndex,
				Selected:      selected,
			})
		}
		r.sendCh <- servechat.ClientEvent{Type: "ask_user_response", RequestID: ev.RequestID, Responses: responses}
	}()
}

func (r *RemoteBackend) emitStreamEvent(ev ui.StreamEvent, closeAfter bool) {
	r.mu.Lock()
	streamCh := r.streamCh
	if streamCh == nil {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	streamCh <- ev
	if closeAfter {
		r.mu.Lock()
		if r.streamCh != nil {
			close(r.streamCh)
			r.streamCh = nil
		}
		r.mu.Unlock()
	}
}

func (r *RemoteBackend) closeStream(err error) {
	r.mu.Lock()
	streamCh := r.streamCh
	if streamCh != nil {
		streamCh <- ui.ErrorEvent(err)
		close(streamCh)
		r.streamCh = nil
	}
	r.mu.Unlock()
}

func normalizeWSURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("remote URL is required")
	}
	if !strings.HasPrefix(value, "ws://") && !strings.HasPrefix(value, "wss://") && !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		value = "ws://" + value
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/chat/sessions/new"
	return parsed.String(), nil
}

func wireToStreamEvent(ev servechat.WireEvent) (ui.StreamEvent, bool) {
	switch ev.Type {
	case "text_delta":
		return ui.StreamEvent{Type: ui.StreamEventText, Text: ev.Text}, true
	case "reasoning_delta":
		return ui.StreamEvent{Type: ui.StreamEventText, Text: ev.Text}, true
	case "phase_change":
		return ui.StreamEvent{Type: ui.StreamEventPhase, Phase: ev.Phase}, true
	case "tool_start":
		return ui.StreamEvent{Type: ui.StreamEventToolStart, ToolName: ev.Tool, ToolInfo: ev.Description}, true
	case "tool_end":
		summary := ev.Summary
		if summary == "" {
			summary = ev.Description
		}
		return ui.StreamEvent{Type: ui.StreamEventToolEnd, ToolName: ev.Tool, ToolInfo: summary, ToolSuccess: ev.Success}, true
	case "message_done":
		input, output, cached := 0, 0, 0
		if ev.Usage != nil {
			input = ev.Usage.Input
			output = ev.Usage.Output
			cached = ev.Usage.Cached
		}
		return ui.StreamEvent{Type: ui.StreamEventDone, Done: true, InputTokens: input, OutputTokens: output, CachedTokens: cached}, true
	case "error":
		return ui.StreamEvent{Type: ui.StreamEventError, Err: errors.New(ev.Message)}, true
	default:
		return ui.StreamEvent{}, false
	}
}

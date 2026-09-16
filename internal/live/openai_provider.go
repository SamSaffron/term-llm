package live

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/samsaffron/term-llm/internal/config"
)

const (
	openAICallTimeout   = 30 * time.Second
	openAILiveCloseWait = 15 * time.Second
)

// OpenAIProvider runs the public OpenAI GPT-Live or legacy Realtime API with
// WebRTC media and a server-owned sideband WebSocket. It intentionally does not
// use ChatGPT OAuth or proprietary headers.
type OpenAIProvider struct {
	cfg    config.LiveConfig
	client *http.Client
	dialer *websocket.Dialer
}

// NewOpenAIProvider builds the public OpenAI live provider.
func NewOpenAIProvider(cfg config.LiveConfig, client *http.Client) *OpenAIProvider {
	if client == nil {
		client = &http.Client{Timeout: openAICallTimeout}
	}
	return &OpenAIProvider{cfg: cfg, client: client}
}

// Name reports the provider name.
func (p *OpenAIProvider) Name() string { return config.LiveProviderOpenAI }

// Ready reports whether an API key is available without making a network call.
func (p *OpenAIProvider) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := p.apiKey()
	return err
}

// Start routes explicitly configured Realtime models through the legacy
// Realtime API. All other models, including the default gpt-live-1, use the
// public GPT-Live transport.
func (p *OpenAIProvider) Start(ctx context.Context, offerSDP string, opts SessionOptions) (Session, error) {
	if strings.TrimSpace(offerSDP) == "" {
		return nil, errors.New("live: empty sdp offer")
	}
	apiKey, err := p.apiKey()
	if err != nil {
		return nil, err
	}
	if p.cfg.OpenAI.UsesRealtime() {
		return p.startRealtime(ctx, apiKey, offerSDP, opts)
	}
	return p.startLive(ctx, apiKey, offerSDP, opts)
}

func (p *OpenAIProvider) startRealtime(ctx context.Context, apiKey, offerSDP string, opts SessionOptions) (Session, error) {
	sessionJSON, err := openAIRealtimeSessionJSON(p.cfg, opts)
	if err != nil {
		return nil, err
	}
	call, err := createOpenAICall(ctx, p.client, p.baseURL(), apiKey, offerSDP, sessionJSON)
	if err != nil {
		return nil, err
	}
	channel, err := dialOpenAISideband(ctx, openAISidebandConfig{
		BaseURL:     p.baseURL(),
		CallID:      call.CallID,
		APIKey:      apiKey,
		Dialer:      p.dialer,
		EmitStarted: true,
	})
	if err != nil {
		return nil, err
	}
	session := newOpenAISession(call.AnswerSDP, channel)
	if err := session.seedHistory(ctx, opts.InitialItems); err != nil {
		channel.Close()
		return nil, err
	}
	return session, nil
}

func (p *OpenAIProvider) startLive(ctx context.Context, apiKey, offerSDP string, opts SessionOptions) (Session, error) {
	payload, err := openAILiveSessionPayloadFor(p.cfg, opts)
	if err != nil {
		return nil, err
	}
	call, err := createOpenAILiveCall(ctx, p.client, p.baseURL(), apiKey, offerSDP, payload)
	if err != nil {
		return nil, err
	}
	channel, err := dialOpenAISideband(ctx, openAISidebandConfig{
		BaseURL:    p.baseURL(),
		CallID:     call.CallID,
		APIKey:     apiKey,
		Dialer:     p.dialer,
		Endpoint:   openAILiveSidebandURL,
		ParseEvent: parseOpenAILiveEvent,
		Protocol:   "GPT-Live",
	})
	if err != nil {
		return nil, err
	}
	return newOpenAILiveSession(call.AnswerSDP, channel), nil
}

func (p *OpenAIProvider) apiKey() (string, error) {
	key := strings.TrimSpace(p.cfg.OpenAI.APIKey)
	if key == "" {
		key = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	}
	if key == "" {
		return "", errors.New("live OpenAI API key is required (set live.openai.api_key or OPENAI_API_KEY)")
	}
	return key, nil
}

func (p *OpenAIProvider) baseURL() string {
	if base := strings.TrimSpace(p.cfg.OpenAI.BaseURL); base != "" {
		return base
	}
	return config.DefaultLiveOpenAIBaseURL
}

// openAISession is the legacy Realtime session implementation. GPT-Live uses
// openAILiveSession below and never sends function outputs or response.create.
type openAISession struct {
	answerSDP string
	channel   *openAISideband

	delegationMu   sync.Mutex
	delegations    map[string]*openAIPendingDelegation
	completedCalls map[string]struct{}
	closed         bool
}

type openAIPendingDelegation struct {
	text strings.Builder
}

func newOpenAISession(answerSDP string, channel *openAISideband) *openAISession {
	return &openAISession{
		answerSDP:      answerSDP,
		channel:        channel,
		delegations:    make(map[string]*openAIPendingDelegation),
		completedCalls: make(map[string]struct{}),
	}
}

func (s *openAISession) AnswerSDP() string { return s.answerSDP }

func (s *openAISession) Events() <-chan Event { return s.channel.Events() }

// AppendDelegation coalesces the controller's streaming chunks into the single
// function_call_output item required by the public Realtime protocol.
func (s *openAISession) AppendDelegation(ctx context.Context, delegationID string, chunk DelegationChunk) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delegationID = strings.TrimSpace(delegationID)
	if delegationID == "" {
		return errors.New("live: OpenAI delegation is missing a call id")
	}
	if strings.TrimSpace(chunk.Text) == "" {
		return nil
	}

	s.delegationMu.Lock()
	defer s.delegationMu.Unlock()
	if s.closed {
		return errOpenAISidebandEnded
	}
	if _, completed := s.completedCalls[delegationID]; completed {
		return fmt.Errorf("live: OpenAI delegation %q is already completed", delegationID)
	}
	pending := s.delegations[delegationID]
	if pending == nil {
		pending = &openAIPendingDelegation{}
		s.delegations[delegationID] = pending
	}
	if chunk.Channel == ChannelCommentary {
		if pending.text.Len() > 0 {
			pending.text.WriteByte('\n')
		}
		pending.text.WriteString("[Progress update] ")
		pending.text.WriteString(strings.TrimSpace(chunk.Text))
		pending.text.WriteByte('\n')
	} else {
		pending.text.WriteString(chunk.Text)
	}
	return nil
}

// CompleteDelegation publishes exactly one Realtime function result after the
// execution turn ends.
func (s *openAISession) CompleteDelegation(ctx context.Context, delegationID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delegationID = strings.TrimSpace(delegationID)
	if delegationID == "" {
		return errors.New("live: OpenAI delegation is missing a call id")
	}
	s.delegationMu.Lock()
	if s.closed {
		s.delegationMu.Unlock()
		return errOpenAISidebandEnded
	}
	if _, done := s.completedCalls[delegationID]; done {
		s.delegationMu.Unlock()
		return nil
	}
	output := "The execution turn completed without text output."
	if pending := s.delegations[delegationID]; pending != nil {
		output = pending.text.String()
	}
	delete(s.delegations, delegationID)
	s.completedCalls[delegationID] = struct{}{}
	s.delegationMu.Unlock()
	return s.sendFunctionOutput(ctx, delegationID, output)
}

func (s *openAISession) sendFunctionOutput(ctx context.Context, callID, output string) error {
	if err := s.channel.Send(ctx, newOpenAIConversationItemEvent(openAIConversationItem{
		Type:   "function_call_output",
		CallID: callID,
		Output: output,
	})); err != nil {
		return err
	}
	return s.channel.Send(ctx, openAIClientEvent{Type: "response.create"})
}

func (s *openAISession) AppendText(ctx context.Context, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if err := s.channel.Send(ctx, openAIMessageEvent(RoleUser, text)); err != nil {
		return err
	}
	return s.channel.Send(ctx, openAIClientEvent{Type: "response.create"})
}

func (s *openAISession) seedHistory(ctx context.Context, items []InitialItem) error {
	for _, initial := range items {
		text := strings.TrimSpace(initial.Text)
		if text == "" {
			continue
		}
		role := strings.TrimSpace(initial.Role)
		switch role {
		case RoleAssistant, RoleUser, "system":
		case "developer":
			role = "system"
		default:
			role = RoleUser
		}
		if err := s.channel.Send(ctx, openAIMessageEvent(role, text)); err != nil {
			return fmt.Errorf("seed OpenAI Realtime history: %w", err)
		}
	}
	return nil
}

func (s *openAISession) Close(context.Context) error {
	s.delegationMu.Lock()
	if s.closed {
		s.delegationMu.Unlock()
		return nil
	}
	s.closed = true
	s.delegations = nil
	s.delegationMu.Unlock()
	s.channel.Close()
	return nil
}

type openAILiveSession struct {
	answerSDP string
	channel   *openAISideband

	closeMu          sync.Mutex
	closed           bool
	typedSequence    uint64
	typedDelegations map[string]struct{}
}

func newOpenAILiveSession(answerSDP string, channel *openAISideband) *openAILiveSession {
	return &openAILiveSession{answerSDP: answerSDP, channel: channel, typedDelegations: make(map[string]struct{})}
}

func (s *openAILiveSession) AnswerSDP() string { return s.answerSDP }

func (s *openAILiveSession) Events() <-chan Event { return s.channel.Events() }

func (s *openAILiveSession) AppendDelegation(ctx context.Context, delegationID string, chunk DelegationChunk) error {
	delegationID = strings.TrimSpace(delegationID)
	if delegationID == "" {
		return errors.New("live: OpenAI GPT-Live delegation is missing an id")
	}
	eventType := openAILiveCommentaryAppend
	if chunk.Channel == ChannelCommentary {
		eventType = openAILiveThinkingAppend
	}
	s.closeMu.Lock()
	_, typed := s.typedDelegations[delegationID]
	s.closeMu.Unlock()
	if typed {
		return s.appendContext(ctx, eventType, nil, chunk.Text)
	}
	return s.appendContext(ctx, eventType, &delegationID, chunk.Text)
}

func (s *openAILiveSession) AppendText(ctx context.Context, text string) error {
	// Client-delegation mode has no user-message command. Mirror the text as
	// quiet context, then ask the host controller to execute/steer the typed task.
	// Its output is session-wide context, not a fabricated provider delegation.
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if err := s.appendContext(ctx, openAILiveThinkingAppend, nil, text); err != nil {
		return err
	}
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return errOpenAISidebandEnded
	}
	s.typedSequence++
	id := fmt.Sprintf("term-llm-typed-%d", s.typedSequence)
	s.typedDelegations[id] = struct{}{}
	s.closeMu.Unlock()
	return s.channel.emitContext(ctx, Event{Kind: EventDelegationCreated, DelegationID: id, Text: strings.TrimPrefix(text, userTextPrefix)})
}

func (s *openAILiveSession) appendContext(ctx context.Context, eventType string, delegationID *string, text string) error {
	switch eventType {
	case openAILiveInstructionsAppend, openAILiveThinkingAppend, openAILiveCommentaryAppend:
	default:
		return fmt.Errorf("live: unsupported OpenAI GPT-Live append event %q", eventType)
	}
	// GPT-Live limits each append to 500 tokens. MaxAppendBytes is a
	// deliberately conservative wire bound: no chunk can contain more tokens
	// than bytes, while ChunkText preserves UTF-8 rune boundaries.
	for _, part := range ChunkText(text, MaxAppendBytes) {
		if err := s.channel.Send(ctx, openAILiveAppendEvent{
			Type:         eventType,
			DelegationID: delegationID,
			Content:      part,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *openAILiveSession) Close(ctx context.Context) error {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return nil
	}
	s.closed = true
	s.closeMu.Unlock()

	sendErr := s.channel.Send(ctx, openAIClientEvent{Type: "session.close"})
	if sendErr != nil && !errors.Is(sendErr, errOpenAISidebandEnded) {
		s.channel.Close()
		return sendErr
	}
	timer := time.NewTimer(openAILiveCloseWait)
	defer timer.Stop()
	select {
	case <-s.channel.Done():
		s.channel.Close()
		s.channel.mu.Lock()
		finalized := s.channel.sessionEnded
		s.channel.mu.Unlock()
		if !finalized {
			return errors.New("live: OpenAI GPT-Live disconnected before session.closed")
		}
		return nil
	case <-ctx.Done():
		s.channel.Close()
		return ctx.Err()
	case <-timer.C:
		s.channel.Close()
		return errors.New("live: timed out waiting for OpenAI GPT-Live session.closed")
	}
}

var _ Provider = (*OpenAIProvider)(nil)
var _ Session = (*openAISession)(nil)
var _ Session = (*openAILiveSession)(nil)

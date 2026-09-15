package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/livecall"
)

const codexOutboundBuffer = 64

// CodexProvider negotiates live calls through a local Codex executable.
//
// Codex performs call creation and joins the provider control channel itself,
// authenticating with term-llm's stored OAuth tokens in an isolated ephemeral
// process. This is an optional alternative to the direct ChatGPT provider.
// Codex's client-managed handoffs deliver delegations over the peer's data
// channel, which the host relays here.
type CodexProvider struct {
	cfg     config.LiveConfig
	manager *livecall.Manager
}

// NewCodexProvider builds the Codex-backed live provider. A nil token source
// uses the stored ChatGPT OAuth credentials.
func NewCodexProvider(cfg config.LiveConfig, tokens livecall.TokenSource) *CodexProvider {
	if tokens == nil {
		tokens = chatGPTLiveTokens
	}
	return &CodexProvider{
		cfg: cfg,
		manager: &livecall.Manager{
			Path:   codexPath(cfg),
			Tokens: tokens,
		},
	}
}

func codexPath(cfg config.LiveConfig) string {
	if path := strings.TrimSpace(cfg.Codex.Path); path != "" {
		return path
	}
	return config.DefaultLiveCodexPath
}

// Name reports the provider name.
func (p *CodexProvider) Name() string { return config.LiveProviderCodex }

// Ready reports whether the executable and credentials are both present.
func (p *CodexProvider) Ready(ctx context.Context) error {
	path := codexPath(p.cfg)
	if _, err := exec.LookPath(path); err != nil {
		return fmt.Errorf("live voice needs a Codex executable (%q): %w", path, err)
	}
	if _, err := p.manager.Tokens(ctx, false); err != nil {
		return err
	}
	return nil
}

// Start negotiates the call and returns a session whose control channel the
// host relays through the media peer.
func (p *CodexProvider) Start(ctx context.Context, offerSDP string, opts SessionOptions) (Session, error) {
	instructions := resolvedInstructions(p.cfg.Instructions, opts)
	offer := livecall.Offer{SDP: offerSDP, Instructions: instructions}
	for _, item := range opts.InitialItems {
		role := item.Role
		if role != RoleAssistant {
			role = RoleUser
		}
		if strings.TrimSpace(item.Text) == "" {
			continue
		}
		offer.InitialItems = append(offer.InitialItems, livecall.Item{Role: role, Text: item.Text})
	}
	answer, err := p.manager.Start(ctx, offer)
	if err != nil {
		return nil, err
	}
	return newCodexSession(answer), nil
}

// Close stops every call this provider negotiated.
func (p *CodexProvider) Close() { p.manager.Close() }

// chatGPTLiveTokens reads the stored OAuth session, refreshing when the token
// is expired or Codex reports it stale.
func chatGPTLiveTokens(_ context.Context, refresh bool) (livecall.Tokens, error) {
	creds, err := credentials.GetChatGPTCredentials()
	if err != nil {
		return livecall.Tokens{}, err
	}
	if refresh || creds.IsExpired() {
		if err := credentials.RefreshChatGPTCredentials(creds); err != nil {
			return livecall.Tokens{}, fmt.Errorf("refresh ChatGPT session: %w", err)
		}
	}
	return livecall.Tokens{AccessToken: creds.AccessToken, AccountID: creds.AccountID}, nil
}

// codexSession relays the provider control channel through the media peer: the
// browser forwards the data-channel frames it receives, and writes back the
// frames this session produces.
type codexSession struct {
	answer   livecall.Answer
	events   chan Event
	outbound chan []byte

	closeOnce sync.Once
	closed    chan struct{}
}

func newCodexSession(answer livecall.Answer) *codexSession {
	return &codexSession{
		answer:   answer,
		events:   make(chan Event, sidebandEventBuffer),
		outbound: make(chan []byte, codexOutboundBuffer),
		closed:   make(chan struct{}),
	}
}

func (s *codexSession) AnswerSDP() string { return s.answer.SDP }

func (s *codexSession) Events() <-chan Event { return s.events }

func (s *codexSession) Outbound() <-chan []byte { return s.outbound }

// Deliver parses one frame the peer received and publishes it as an event.
func (s *codexSession) Deliver(frame []byte) {
	event, err := ParseEvent(frame)
	if err != nil || event.Kind == EventUnknown {
		return
	}
	select {
	case s.events <- event:
	case <-s.closed:
	}
}

func (s *codexSession) AppendDelegation(ctx context.Context, delegationID string, chunk DelegationChunk) error {
	for _, part := range ChunkText(chunk.Text, MaxAppendBytes) {
		if err := s.send(ctx, DelegationContextAppend(delegationID, chunk.Channel, part)); err != nil {
			return err
		}
	}
	return nil
}

func (s *codexSession) AppendText(ctx context.Context, text string) error {
	for _, part := range ChunkText(text, MaxAppendBytes) {
		if err := s.send(ctx, SessionContextAppend(ChannelSpeakable, part)); err != nil {
			return err
		}
	}
	return nil
}

func (s *codexSession) send(ctx context.Context, message Outbound) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode live message: %w", err)
	}
	select {
	case s.outbound <- payload:
		return nil
	case <-s.closed:
		return errors.New("live: session closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close ends the Codex call and the relay.
func (s *codexSession) Close(context.Context) error {
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.answer.Cancel != nil {
			s.answer.Cancel()
		}
		close(s.events)
	})
	return nil
}

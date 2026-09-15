package live

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/llm"
)

const chatGPTCallTimeout = 30 * time.Second

// ChatGPTProvider runs gpt-live over the ChatGPT backend using the stored
// OAuth session. The browser owns the media path; this side owns auth, the
// control channel, and delegation.
type ChatGPTProvider struct {
	cfg    config.LiveConfig
	auth   AuthFunc
	ready  func(ctx context.Context) error
	client *http.Client
	dialer *websocket.Dialer
}

// NewChatGPTProvider builds the ChatGPT live provider. A nil auth uses the
// stored OAuth credentials; a nil client uses a default with a call timeout.
func NewChatGPTProvider(cfg config.LiveConfig, auth AuthFunc, client *http.Client) *ChatGPTProvider {
	ready := func(ctx context.Context) error {
		_, err := auth(ctx, false)
		return err
	}
	if auth == nil {
		auth = ChatGPTAuth
		// Capability checks run on every UI bootstrap, so readiness only looks
		// for stored credentials; refreshing is left to session start.
		ready = func(context.Context) error {
			_, err := credentials.GetChatGPTCredentials()
			return err
		}
	}
	if client == nil {
		client = &http.Client{Timeout: chatGPTCallTimeout}
	}
	return &ChatGPTProvider{cfg: cfg, auth: auth, ready: ready, client: client}
}

// Name reports the provider name.
func (p *ChatGPTProvider) Name() string { return config.LiveProviderChatGPT }

// Ready reports whether a live session could be started right now.
func (p *ChatGPTProvider) Ready(ctx context.Context) error { return p.ready(ctx) }

// Start creates the call and joins the control channel.
func (p *ChatGPTProvider) Start(ctx context.Context, offerSDP string, opts SessionOptions) (Session, error) {
	if strings.TrimSpace(offerSDP) == "" {
		return nil, errors.New("live: empty sdp offer")
	}
	sessionJSON, err := SessionJSON(p.cfg, opts)
	if err != nil {
		return nil, err
	}
	request := CallRequest{SDP: offerSDP, Session: sessionJSON}

	auth, err := p.auth(ctx, false)
	if err != nil {
		return nil, err
	}
	call, err := CreateCall(ctx, p.client, p.callBaseURL(), auth, opts.SessionID, request)
	if errors.Is(err, ErrUnauthorized) {
		refreshed, refreshErr := p.auth(ctx, true)
		if refreshErr != nil {
			return nil, fmt.Errorf("%w (refresh failed: %v)", err, refreshErr)
		}
		call, err = CreateCall(ctx, p.client, p.callBaseURL(), refreshed, opts.SessionID, request)
	}
	if err != nil {
		return nil, err
	}

	channel, err := dialSideband(ctx, sidebandConfig{
		BaseURL:   p.sidebandBaseURL(),
		CallID:    call.CallID,
		SessionID: opts.SessionID,
		Auth:      p.auth,
		Dialer:    p.dialer,
	})
	if err != nil {
		return nil, err
	}
	return &chatGPTSession{answerSDP: call.AnswerSDP, channel: channel, initialVoice: ConfigCapabilities(p.cfg).Voice}, nil
}

func (p *ChatGPTProvider) callBaseURL() string {
	if base := strings.TrimSpace(p.cfg.ChatGPT.CallBaseURL); base != "" {
		return base
	}
	return config.DefaultLiveChatGPTCallBaseURL
}

func (p *ChatGPTProvider) sidebandBaseURL() string {
	if base := strings.TrimSpace(p.cfg.ChatGPT.SidebandBaseURL); base != "" {
		return base
	}
	return config.DefaultLiveChatGPTSidebandBaseURL
}

// ChatGPTAuth resolves the stored OAuth session, refreshing when the token is
// expired or the provider rejected it.
func ChatGPTAuth(_ context.Context, refresh bool) (Auth, error) {
	creds, err := credentials.GetChatGPTCredentials()
	if err != nil {
		return Auth{}, err
	}
	if refresh || creds.IsExpired() {
		if err := credentials.RefreshChatGPTCredentials(creds); err != nil {
			return Auth{}, fmt.Errorf("refresh ChatGPT session: %w", err)
		}
	}
	return Auth{
		AccessToken: creds.AccessToken,
		AccountID:   creds.AccountID,
		Headers:     llm.ChatGPTRequestHeaders(creds),
	}, nil
}

type chatGPTSession struct {
	initialVoice string
	answerSDP    string
	channel      *sideband
}

func (s *chatGPTSession) AnswerSDP() string { return s.answerSDP }

func (s *chatGPTSession) Events() <-chan Event { return s.channel.Events() }

func (s *chatGPTSession) AppendDelegation(ctx context.Context, delegationID string, chunk DelegationChunk) error {
	for _, part := range ChunkText(chunk.Text, MaxAppendBytes) {
		if err := s.channel.Send(ctx, DelegationContextAppend(delegationID, chunk.Channel, part)); err != nil {
			return err
		}
	}
	return nil
}

func (s *chatGPTSession) AppendText(ctx context.Context, text string) error {
	for _, part := range ChunkText(text, MaxAppendBytes) {
		if err := s.channel.Send(ctx, SessionContextAppend(ChannelSpeakable, part)); err != nil {
			return err
		}
	}
	return nil
}

func (s *chatGPTSession) CurrentVoice() string {
	s.channel.voiceMu.Lock()
	defer s.channel.voiceMu.Unlock()
	if s.channel.currentVoice != "" {
		return s.channel.currentVoice
	}
	return s.initialVoice
}

// SetVoice updates only the session's audio output voice and returns after the
// provider acknowledges that exact voice in session.updated.
func (s *chatGPTSession) SetVoice(ctx context.Context, voice string) error {
	voice = strings.TrimSpace(voice)
	if voice == "" {
		return errors.New("live: voice must not be empty")
	}
	if err := (config.LiveChatGPTConfig{Voice: voice}).ValidateVoice(); err != nil {
		return err
	}
	payload, err := voiceUpdateJSON(voice)
	if err != nil {
		return err
	}
	return s.channel.updateVoice(ctx, voice, SessionUpdate(payload))
}

var _ VoiceSession = (*chatGPTSession)(nil)

func (s *chatGPTSession) Close(ctx context.Context) error {
	sendErr := s.channel.Send(ctx, SessionClose())
	s.channel.Close()
	if sendErr != nil && !errors.Is(sendErr, errSidebandEnded) {
		return sendErr
	}
	return nil
}

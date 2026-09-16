// Package live implements bidirectional voice ("live") sessions: the protocol
// client for the voice model, and a controller that turns the voice model's
// delegation requests into ordinary agent turns.
//
// The package is host-agnostic. A host supplies the media peer (today the
// browser, which owns the WebRTC audio path) and a Delegator that runs the
// delegated work; nothing here knows about HTTP handlers or a terminal UI.
package live

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

// SessionOptions carries the provider-neutral inputs for a new live session.
type SessionOptions struct {
	// SessionID is the chat session the live call is bound to. It is sent as
	// x-session-id so provider-side logs line up with ours.
	SessionID string
	// Instructions replaces the default voice-model prompt when set.
	Instructions string
	// Context is authoritative host context appended to either the default or
	// custom instructions. It is kept separate so a custom prompt cannot erase
	// the current session capabilities supplied by the host.
	Context string
	// InitialItems seeds the voice model with prior conversation.
	InitialItems []InitialItem
}

// DelegationRequest is the structured input for one delegated agent turn.
type DelegationRequest struct {
	// ID is the provider's stable delegation identifier when one was supplied.
	ID              string
	Input           string
	TranscriptDelta string
}

// InitialItem seeds a live session with one prior message.
type InitialItem struct {
	Role string
	Text string
}

// DelegationChunk is a piece of delegated-turn output sent back to the voice
// model. Channel is ChannelSpeakable or ChannelCommentary.
type DelegationChunk struct {
	Text    string
	Channel string
}

// Provider creates live sessions for one vendor.
type Provider interface {
	// Name reports the configured provider name.
	Name() string
	// Ready reports whether a session could be started right now.
	Ready(ctx context.Context) error
	// Start exchanges the caller's SDP offer for a provider answer and opens
	// the control channel.
	Start(ctx context.Context, offerSDP string, opts SessionOptions) (Session, error)
}

// Session is one open live call.
type Session interface {
	// AnswerSDP returns the provider's SDP answer for the media peer.
	AnswerSDP() string
	// Events streams normalised provider events until the session ends.
	Events() <-chan Event
	// AppendDelegation streams delegated-turn output back to the voice model.
	AppendDelegation(ctx context.Context, delegationID string, chunk DelegationChunk) error
	// AppendText injects text into the conversation outside a delegation.
	AppendText(ctx context.Context, text string) error
	// Close ends the session.
	Close(ctx context.Context) error
}

// DelegationCompletionSession is implemented by protocols requiring a complete
// function result rather than accepting an open-ended stream of context.
type DelegationCompletionSession interface {
	CompleteDelegation(ctx context.Context, delegationID string) error
}

// VoiceSession is implemented by sessions whose provider can change the audio
// output voice during an authenticated current call.
type VoiceSession interface {
	// CurrentVoice returns the most recently acknowledged voice, including late
	// acknowledgements after a canceled SetVoice request.
	CurrentVoice() string
	SetVoice(ctx context.Context, voice string) error
}

// NewProvider builds the configured live provider.
func NewProvider(cfg config.LiveConfig) (Provider, error) {
	return NewProviderWithClient(cfg, nil)
}

// NewProviderWithClient builds the configured live provider with an explicit
// HTTP client. Tests use it to point at an in-process fake.
func NewProviderWithClient(cfg config.LiveConfig, client *http.Client) (Provider, error) {
	switch name := strings.TrimSpace(cfg.Provider); name {
	case config.LiveProviderOpenAI:
		return NewOpenAIProvider(cfg, client), nil
	case "", config.LiveProviderChatGPT:
		return NewChatGPTProvider(cfg, nil, client), nil
	default:
		return nil, fmt.Errorf("live provider %q is not supported", name)
	}
}

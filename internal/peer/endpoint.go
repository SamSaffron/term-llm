//go:build !js

package peer

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/pion/webrtc/v4"
)

// Endpoint is one side of a WebRTC peer connection that carries the bridge.
// Signalling is non-trickle: the descriptions it returns already contain all
// gathered ICE candidates, so a single offer/answer exchange is enough.
type Endpoint struct {
	pc *webrtc.PeerConnection

	// bridgeClaimed is set, permanently, by the first remote channel
	// carrying ChannelLabel; every later remote channel is closed.
	bridgeMu      sync.Mutex
	bridgeClaimed bool
}

// NewEndpoint creates a Pion peer connection.
func NewEndpoint(cfg EndpointConfig) (*Endpoint, error) {
	var se webrtc.SettingEngine
	if cfg.SettingEngine != nil {
		se = *cfg.SettingEngine
	}
	maxFrame := cfg.MaxFrameBytes
	if maxFrame <= 0 {
		maxFrame = DefaultMaxFrameBytes
	}
	se.SetSCTPMaxMessageSize(uint32(maxFrame))
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers:         cfg.ICEServers,
		ICETransportPolicy: cfg.ICETransportPolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("peer: create peer connection: %w", err)
	}
	return &Endpoint{pc: pc}, nil
}

// PeerConnection exposes the underlying Pion connection, for example so a
// pairing layer can bind the DTLS fingerprint or watch connection state.
func (e *Endpoint) PeerConnection() *webrtc.PeerConnection { return e.pc }

// Close tears down the peer connection and every channel on it.
func (e *Endpoint) Close() error {
	if err := e.pc.Close(); err != nil {
		return fmt.Errorf("peer: close peer connection: %w", err)
	}
	return nil
}

// NewClient opens the reliable, ordered bridge channel and returns a Client
// for it. On the offering side, call it before CreateOffer so the channel is
// negotiated in the first offer.
func (e *Endpoint) NewClient(cfg ClientConfig) (*Client, error) {
	ordered := true
	dc, err := e.pc.CreateDataChannel(ChannelLabel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return nil, fmt.Errorf("peer: create bridge channel: %w", err)
	}
	c, err := NewClient(dc, cfg)
	if err != nil {
		_ = dc.Close()
		return nil, err
	}
	return c, nil
}

// Serve hands the first bridge channel the remote peer opens to srv. An
// endpoint serves at most one bridge channel for its whole lifetime: the
// first channel labelled ChannelLabel claims the slot (even if it is then
// rejected as unreliable), and every other remote channel, including later
// correctly labelled ones and those opened after the bridge closes, is closed.
// Channels arriving after ctx is done are closed too.
func (e *Endpoint) Serve(ctx context.Context, srv *Server) {
	e.pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if !e.claimBridge(ctx, dc) {
			_ = dc.Close()
			return
		}
		if err := srv.ServeChannel(ctx, dc); err != nil {
			srv.cfg.Logf("peer: rejected data channel %q: %v", dc.Label(), err)
		}
	})
}

func (e *Endpoint) claimBridge(ctx context.Context, dc *webrtc.DataChannel) bool {
	if ctx.Err() != nil || dc.Label() != ChannelLabel {
		return false
	}
	e.bridgeMu.Lock()
	defer e.bridgeMu.Unlock()
	if e.bridgeClaimed {
		return false
	}
	e.bridgeClaimed = true
	return true
}

// CreateOffer creates and applies a local offer, returning it once ICE
// gathering completes.
func (e *Endpoint) CreateOffer(ctx context.Context) (webrtc.SessionDescription, error) {
	offer, err := e.pc.CreateOffer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("peer: create offer: %w", err)
	}
	return e.setLocal(ctx, offer)
}

// AcceptOffer applies a remote offer and returns the complete local answer.
func (e *Endpoint) AcceptOffer(ctx context.Context, offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	if offer.Type != webrtc.SDPTypeOffer {
		return webrtc.SessionDescription{}, errors.New("peer: remote description is not an offer")
	}
	if err := e.pc.SetRemoteDescription(offer); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("peer: apply offer: %w", err)
	}
	answer, err := e.pc.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("peer: create answer: %w", err)
	}
	return e.setLocal(ctx, answer)
}

// AcceptAnswer applies the remote answer to a previously created offer.
func (e *Endpoint) AcceptAnswer(answer webrtc.SessionDescription) error {
	if answer.Type != webrtc.SDPTypeAnswer {
		return errors.New("peer: remote description is not an answer")
	}
	if err := e.pc.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("peer: apply answer: %w", err)
	}
	return nil
}

func (e *Endpoint) setLocal(ctx context.Context, desc webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	gathered := webrtc.GatheringCompletePromise(e.pc)
	if err := e.pc.SetLocalDescription(desc); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("peer: set local description: %w", err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return webrtc.SessionDescription{}, fmt.Errorf("peer: gather ICE candidates: %w", ctx.Err())
	}
	local := e.pc.LocalDescription()
	if local == nil {
		return webrtc.SessionDescription{}, errors.New("peer: no local description after gathering")
	}
	return *local, nil
}

// PlatformSupported reports whether native peer serving is available.
func PlatformSupported() bool { return true }

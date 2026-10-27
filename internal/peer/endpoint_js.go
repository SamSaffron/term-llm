//go:build js

package peer

import (
	"context"
	"errors"
	"github.com/pion/webrtc/v4"
)

// ErrUnsupportedPlatform explicitly rejects native peer hosting in js/wasm.
// Existing browser/front-end WebRTC functionality is independent of this API.
var ErrUnsupportedPlatform = errors.New("native peer endpoint is not supported on js/wasm")

type Endpoint struct{}

func PlatformSupported() bool                             { return false }
func NewEndpoint(EndpointConfig) (*Endpoint, error)       { return nil, ErrUnsupportedPlatform }
func (*Endpoint) PeerConnection() *webrtc.PeerConnection  { return nil }
func (*Endpoint) Close() error                            { return ErrUnsupportedPlatform }
func (*Endpoint) NewClient(ClientConfig) (*Client, error) { return nil, ErrUnsupportedPlatform }
func (*Endpoint) Serve(context.Context, *Server)          { panic(ErrUnsupportedPlatform) }
func (*Endpoint) CreateOffer(context.Context) (webrtc.SessionDescription, error) {
	return webrtc.SessionDescription{}, ErrUnsupportedPlatform
}
func (*Endpoint) AcceptOffer(context.Context, webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	return webrtc.SessionDescription{}, ErrUnsupportedPlatform
}
func (*Endpoint) AcceptAnswer(webrtc.SessionDescription) error { return ErrUnsupportedPlatform }
func (*Endpoint) SelectedPair() (SelectedPairStats, bool, error) {
	return SelectedPairStats{}, false, ErrUnsupportedPlatform
}

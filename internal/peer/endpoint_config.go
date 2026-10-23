package peer

import "github.com/pion/webrtc/v4"

// EndpointConfig configures an Endpoint.
type EndpointConfig struct {
	ICEServers []webrtc.ICEServer
	// ICETransportPolicy restricts gathered candidates; the zero value
	// (ICETransportPolicyAll) uses every candidate type, while
	// ICETransportPolicyRelay forces all traffic through ICEServers' TURN.
	ICETransportPolicy webrtc.ICETransportPolicy
	// SettingEngine customises Pion (network types, interface filters,
	// timeouts). Nil uses Pion defaults. Its SCTP max message size is always
	// overridden with MaxFrameBytes.
	SettingEngine *webrtc.SettingEngine
	// MaxFrameBytes bounds inbound DataChannel messages; defaults to
	// DefaultMaxFrameBytes and should match the bridge Limits.
	MaxFrameBytes int
}

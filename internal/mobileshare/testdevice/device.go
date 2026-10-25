package testdevice

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/samsaffron/term-llm/internal/pairing"
	"github.com/samsaffron/term-llm/internal/peer"
	"github.com/samsaffron/term-llm/internal/peerbroker"
)

var ErrNotPaired = errors.New("testdevice: device is not paired")

// Config configures a synthetic device. StatePath is optional; when present,
// the identity and approved grant are kept together in one private file.
type Config struct {
	DeviceName string
	StatePath  string
	BrokerHTTP *http.Client
	Endpoint   peer.EndpointConfig
	Bridge     peer.ClientConfig
	Now        func() time.Time
	OnSAS      func(string)
}

// Pairing describes the durable authority received from the host. Secret
// reconnect material is intentionally not exposed.
type Pairing struct {
	Broker    string
	HostKey   ed25519.PublicKey
	DeviceID  string
	Scope     string
	BasePath  string
	HostLabel string
}

// Device is an intentionally synthetic implementation of the mobile side of
// pairing v2 and the peer bridge.
type Device struct {
	cfg      Config
	identity *pairing.Identity
	state    persistedState
}

// New loads an optional private state file or creates an ephemeral identity.
func New(cfg Config) (*Device, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	var state persistedState
	var err error
	if cfg.StatePath != "" {
		state, err = loadState(cfg.StatePath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if state.Version == 0 {
		if cfg.DeviceName == "" {
			cfg.DeviceName = "Synthetic test device"
		}
		if err := pairing.ValidateDeviceName(cfg.DeviceName); err != nil {
			return nil, fmt.Errorf("testdevice: device name: %w", err)
		}
		state, err = newPersistedState(cfg.DeviceName)
		if err != nil {
			return nil, err
		}
	} else if cfg.DeviceName != "" && cfg.DeviceName != state.DeviceName {
		return nil, fmt.Errorf("testdevice: configured device name %q does not match state name %q", cfg.DeviceName, state.DeviceName)
	}
	identity, err := pairing.IdentityFromSeed(state.IdentitySeed)
	if err != nil {
		return nil, fmt.Errorf("testdevice: load identity: %w", err)
	}
	return &Device{cfg: cfg, identity: identity, state: state}, nil
}

// Pairing returns a copy of the current approved pairing.
func (d *Device) Pairing() (Pairing, bool) {
	if !d.state.paired() {
		return Pairing{}, false
	}
	return Pairing{
		Broker: d.state.Broker, HostKey: ed25519.PublicKey(append([]byte(nil), d.state.HostKey...)),
		DeviceID: d.state.DeviceID.String(), Scope: d.state.Scope, BasePath: d.state.BasePath, HostLabel: d.state.HostLabel,
	}, true
}

// Pair parses a real v2 invitation link, claims its broker slot, sends the
// authenticated PairRequest, exposes the SAS through OnSAS, and waits for an
// authenticated approved PairAnswer.
func (d *Device) Pair(ctx context.Context, rawLink string) (string, error) {
	link, err := pairing.ParseLink(rawLink)
	if err != nil {
		return "", fmt.Errorf("testdevice: parse pairing link: %w", err)
	}
	broker, err := peerbroker.NewClient(link.Broker, d.cfg.BrokerHTTP)
	if err != nil {
		return "", err
	}
	conn, err := broker.DialInvitation(ctx, hostID(link.HostKey), link.BrokerInvitationID(), link.InvitationCapability)
	if err != nil {
		return "", fmt.Errorf("testdevice: dial invitation: %w", err)
	}
	defer conn.Close()
	attempt, err := pairing.StartPairing(link, d.identity, d.state.DeviceName, d.cfg.Now())
	if err != nil {
		return "", fmt.Errorf("testdevice: start pairing: %w", err)
	}
	if err := sendMessage(conn, attempt.Request()); err != nil {
		return "", err
	}
	sas := attempt.SAS()
	if d.cfg.OnSAS != nil {
		d.cfg.OnSAS(sas)
	}
	message, err := receiveMessage(ctx, conn)
	if err != nil {
		return sas, fmt.Errorf("testdevice: receive pairing answer: %w", err)
	}
	switch answer := message.(type) {
	case *pairing.PairReject:
		return sas, fmt.Errorf("testdevice: pairing rejected: %s", answer.Reason)
	case *pairing.PairAnswer:
		approved, err := attempt.VerifyAnswer(answer)
		if err != nil {
			return sas, fmt.Errorf("testdevice: verify pairing answer: %w", err)
		}
		d.state.Broker = link.Broker
		d.state.HostKey = append(d.state.HostKey[:0], approved.HostKey...)
		d.state.DeviceID = append(d.state.DeviceID[:0], approved.DeviceID...)
		d.state.ReconnectCapability = append(d.state.ReconnectCapability[:0], approved.ReconnectCapability...)
		d.state.Scope, d.state.BasePath, d.state.HostLabel = approved.Scope, approved.BasePath, approved.HostLabel
		if d.cfg.StatePath != "" {
			if err := writeState(d.cfg.StatePath, d.state); err != nil {
				return sas, err
			}
		}
		return sas, nil
	default:
		return sas, fmt.Errorf("testdevice: expected pair answer, got %T", message)
	}
}

// Connect reconnects with DialDevice (never an invitation), completes the
// signed transport handshake, and establishes a real Pion DataChannel.
func (d *Device) Connect(ctx context.Context) (*Session, error) {
	if !d.state.paired() {
		return nil, ErrNotPaired
	}
	broker, err := peerbroker.NewClient(d.state.Broker, d.cfg.BrokerHTTP)
	if err != nil {
		return nil, err
	}
	host := hostID(d.state.HostKey)
	deviceID := d.state.DeviceID.String()
	capability := d.state.ReconnectCapability.String()
	ice, err := broker.ICE(ctx, host, peerbroker.PeerDevice, deviceID, capability)
	if err != nil {
		return nil, fmt.Errorf("testdevice: obtain ICE configuration: %w", err)
	}
	conn, err := broker.DialDevice(ctx, host, deviceID, capability)
	if err != nil {
		return nil, fmt.Errorf("testdevice: dial paired device: %w", err)
	}
	defer conn.Close()

	endpointCfg := d.cfg.Endpoint
	if endpointCfg.ICEServers == nil {
		endpointCfg.ICEServers = pionICEServers(ice.ICEServers)
	}
	endpoint, err := peer.NewEndpoint(endpointCfg)
	if err != nil {
		return nil, err
	}
	bridge, err := endpoint.NewClient(d.cfg.Bridge)
	if err != nil {
		_ = endpoint.Close()
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			_ = bridge.Close()
			_ = endpoint.Close()
		}
	}()

	hello, err := pairing.NewTransportHello(d.identity, ed25519.PublicKey(d.state.HostKey), d.state.DeviceID, d.state.ReconnectCapability)
	if err != nil {
		return nil, fmt.Errorf("testdevice: create transport hello: %w", err)
	}
	if err := sendMessage(conn, hello); err != nil {
		return nil, err
	}
	message, err := receiveMessage(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("testdevice: receive transport challenge: %w", err)
	}
	challenge, ok := message.(*pairing.TransportChallenge)
	if !ok {
		return nil, fmt.Errorf("testdevice: expected transport challenge, got %T", message)
	}
	offer, err := endpoint.CreateOffer(ctx)
	if err != nil {
		return nil, err
	}
	attempt, err := pairing.StartTransport(challenge, ed25519.PublicKey(d.state.HostKey), d.identity, d.state.DeviceID, offer.SDP, d.cfg.Now())
	if err != nil {
		return nil, fmt.Errorf("testdevice: start transport: %w", err)
	}
	if err := sendMessage(conn, attempt.Request()); err != nil {
		return nil, err
	}
	message, err = receiveMessage(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("testdevice: receive transport answer: %w", err)
	}
	answer, ok := message.(*pairing.TransportAnswer)
	if !ok {
		return nil, fmt.Errorf("testdevice: expected transport answer, got %T", message)
	}
	answerSDP, err := attempt.VerifyAnswer(answer)
	if err != nil {
		return nil, fmt.Errorf("testdevice: verify transport answer: %w", err)
	}
	if err := endpoint.AcceptAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerSDP}); err != nil {
		return nil, err
	}
	if err := bridge.WaitOpen(ctx); err != nil {
		return nil, fmt.Errorf("testdevice: wait for bridge channel: %w", err)
	}
	failed = false
	return &Session{endpoint: endpoint, bridge: bridge, client: &http.Client{Transport: bridge}, pairing: d.currentPairing()}, nil
}

func (d *Device) currentPairing() Pairing {
	p, _ := d.Pairing()
	return p
}

// Session is one authenticated peer bridge connection.
type Session struct {
	endpoint *peer.Endpoint
	bridge   *peer.Client
	client   *http.Client
	pairing  Pairing
}

// HTTPClient returns an HTTP client backed by the peer DataChannel.
func (s *Session) HTTPClient() *http.Client { return s.client }

// Pairing returns the authority metadata applying to bridged requests.
func (s *Session) Pairing() Pairing { return s.pairing }

// Close closes the HTTP bridge and Pion peer connection.
func (s *Session) Close() error {
	return errors.Join(s.bridge.Close(), s.endpoint.Close())
}

func sendMessage(conn *peerbroker.Conn, message any) error {
	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("testdevice: encode signal: %w", err)
	}
	if err := conn.Send("", "", body); err != nil {
		return fmt.Errorf("testdevice: send signal: %w", err)
	}
	return nil
}

func receiveMessage(ctx context.Context, conn *peerbroker.Conn) (any, error) {
	type result struct {
		message any
		err     error
	}
	results := make(chan result, 1)
	go func() {
		for {
			frame, err := conn.Recv()
			if err != nil {
				results <- result{err: err}
				return
			}
			switch frame.Type {
			case peerbroker.FrameSignal:
				message, err := pairing.DecodeMessage(frame.Body)
				results <- result{message: message, err: err}
				return
			case peerbroker.FrameError:
				results <- result{err: fmt.Errorf("broker signal error: %s", frame.Error)}
				return
			}
		}
	}()
	select {
	case result := <-results:
		return result.message, result.err
	case <-ctx.Done():
		_ = conn.Close()
		<-results
		return nil, ctx.Err()
	}
}

func hostID(hostKey []byte) string {
	sum := sha256.Sum256(hostKey)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func pionICEServers(servers []peerbroker.ICEServer) []webrtc.ICEServer {
	out := make([]webrtc.ICEServer, len(servers))
	for i, server := range servers {
		out[i] = webrtc.ICEServer{URLs: append([]string(nil), server.URLs...), Username: server.Username, Credential: server.Credential}
	}
	return out
}

// Package peerconnect joins authenticated signalling, device pairing and the
// WebRTC HTTP bridge. It never opens a public listener or deploys a service.
package peerconnect

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/samsaffron/term-llm/internal/pairing"
	"github.com/samsaffron/term-llm/internal/peer"
	"github.com/samsaffron/term-llm/internal/peerbroker"
)

// Config defines one foreground pilot host. A fresh broker rendezvous is
// created on each run. Persisted identities/grants do NOT imply discovery after
// this rendezvous expires: reconnect is limited to the printed live session.
type Config struct {
	Audit             *AuditFile // optional private bounded actual-request audit; never a broker endpoint
	BrokerURL         string
	RegistrationToken string // operator capability, never put in the pairing link
	StateDir          string
	Name              string
	Handler           http.Handler // fixed private Hub ingress, never a caller-selected URL
	Approver          pairing.Approver
	Endpoint          peer.EndpointConfig
	// TURN requests short-lived credentials from the broker. If TURN is absent
	// or minting fails startup fails; we never silently claim fallback coverage.
	TURN          bool
	PairingTTL    time.Duration            // default5m; operator override1m..30m, never beyond rendezvous
	RendezvousTTL time.Duration            // default/max 30m; pairing link expires after 5m
	GrantTTL      time.Duration            // default 24h; no non-expiring pilot grant
	OnPairingLink func(pairing.Link) error // caller must display only on trusted terminal
	OnStatus      func(string)             // fixed, non-secret diagnostic strings only
}

// Host owns one broker session and zero or one active device peer. Grants and
// host identity persist; private capabilities and pairing PSK live in memory.
type Host struct {
	cfg            Config
	pairing        *pairing.Host
	broker         *peerbroker.Client
	session        *peerbroker.Session
	link           pairing.Link
	conn           *peerbroker.Conn
	endpointConfig peer.EndpointConfig
	mu             sync.Mutex
	active         *activePeer
	runCancel      context.CancelFunc
	closed         bool
	closeOnce      sync.Once
	closeErr       error
}
type authenticatedGrantKey struct{}

// AuthenticatedGrant is present only while the verified peer dispatcher calls
// the private Hub handler. It is not derived from any HTTP header or cookie.
// Handlers should still recheck the persisted grant if they use this context
// as an authorization boundary.
func AuthenticatedGrant(ctx context.Context) (pairing.Grant, bool) {
	g, ok := ctx.Value(authenticatedGrantKey{}).(pairing.Grant)
	return g, ok
}

type activePeer struct {
	endpoint *peer.Endpoint
	cancel   context.CancelFunc
	grant    pairing.Grant
}

// NewHost registers the pilot rendezvous; it does not start a listener, dial
// peers or grant access. Call Close even if Run is never called.
func NewHost(ctx context.Context, cfg Config) (*Host, error) {
	if !peer.PlatformSupported() {
		return nil, errors.New("native peer hosting is not supported on js/wasm")
	}
	if cfg.Handler == nil || cfg.Approver == nil || cfg.OnPairingLink == nil {
		return nil, errors.New("peer connect requires handler, explicit approver and pairing-link sink")
	}
	if cfg.StateDir == "" {
		return nil, errors.New("peer connect requires a private state directory")
	}
	if cfg.RegistrationToken == "" {
		return nil, errors.New("peer connect requires broker registration capability")
	}
	if cfg.RendezvousTTL == 0 {
		cfg.RendezvousTTL = peerbroker.MaxSessionTTL
	}
	if cfg.RendezvousTTL < peerbroker.MinSessionTTL || cfg.RendezvousTTL > peerbroker.MaxSessionTTL {
		return nil, errors.New("peer rendezvous TTL must be between 1m and 30m")
	}
	if cfg.PairingTTL == 0 {
		cfg.PairingTTL = 5 * time.Minute
	}
	if cfg.PairingTTL < time.Minute || cfg.PairingTTL > peerbroker.MaxSessionTTL || cfg.PairingTTL > cfg.RendezvousTTL {
		return nil, errors.New("peer pairing TTL must be between 1m and the broker rendezvous TTL (at most 30m)")
	}
	if cfg.GrantTTL == 0 {
		cfg.GrantTTL = 24 * time.Hour
	}
	if cfg.GrantTTL < time.Minute || cfg.GrantTTL > 24*time.Hour {
		return nil, errors.New("peer pilot grant TTL must be between 1m and 24h")
	}
	if cfg.OnStatus == nil {
		cfg.OnStatus = func(string) {}
	}
	identity, err := pairing.LoadOrCreateIdentity(filepath.Join(cfg.StateDir, "host-identity.json"))
	if err != nil {
		return nil, err
	}
	grants, err := pairing.OpenGrantStore(filepath.Join(cfg.StateDir, "device-grants.json"))
	if err != nil {
		return nil, err
	}
	b, err := peerbroker.NewClient(cfg.BrokerURL, nil)
	if err != nil {
		return nil, err
	}
	session, err := b.Create(ctx, peerbroker.CreateOptions{TTL: cfg.RendezvousTTL, CreateToken: cfg.RegistrationToken})
	if err != nil {
		return nil, err
	}
	ph, err := pairing.NewHost(identity, grants, pairing.HostOptions{Broker: cfg.BrokerURL, Name: cfg.Name, PairingTTL: cfg.PairingTTL, GrantTTL: cfg.GrantTTL})
	if err != nil {
		_ = b.Delete(ctx, session.ID, session.HostToken)
		return nil, err
	}
	link, err := ph.NewPairingForSession(session.ID, session.DeviceToken, session.ExpiresAt)
	if err != nil {
		_ = b.Delete(ctx, session.ID, session.HostToken)
		return nil, err
	}
	cfg.RegistrationToken = "" // registration authority is no longer needed in memory
	h := &Host{cfg: cfg, pairing: ph, broker: b, session: session, link: link, endpointConfig: cfg.Endpoint}
	if cfg.TURN {
		creds, err := b.MintTURN(ctx, session.ID, session.HostToken)
		if err != nil {
			_ = h.Close()
			return nil, fmt.Errorf("peer TURN credential mint: %w", err)
		}
		for _, ice := range creds.ICEServers {
			h.endpointConfig.ICEServers = append(h.endpointConfig.ICEServers, webrtc.ICEServer{URLs: ice.URLs, Username: ice.Username, Credential: ice.Credential})
		}
	}
	return h, nil
}

// Grants exposes the protected grant store for local listing/revocation.
func (h *Host) Grants() *pairing.GrantStore { return h.pairing.Grants() }

// Run connects the signalling websocket and displays the one-use link. It
// continues serving an established peer when signalling expires, but explicitly
// reports that reconnect discovery is unavailable after rendezvous expiry.
func (h *Host) Run(ctx context.Context) error {
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	h.mu.Lock()
	if h.closed || h.runCancel != nil {
		h.mu.Unlock()
		return errors.New("peer host closed or already running")
	}
	h.runCancel = cancelRun
	h.mu.Unlock()
	defer func() { h.mu.Lock(); h.runCancel = nil; h.conn = nil; h.mu.Unlock() }()
	conn, err := h.broker.Dial(ctx, h.session.ID, h.session.HostToken)
	if err != nil {
		return err
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = conn.Close()
		return context.Canceled
	}
	h.conn = conn
	h.mu.Unlock()
	defer conn.Close()
	linkCopy, err := pairing.ParseLink(h.link.String())
	if err != nil {
		return err
	}
	if err := h.cfg.OnPairingLink(linkCopy); err != nil {
		return err
	}
	defer h.pairing.CancelPairing(h.link.SessionID)
	monitorCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go h.monitorGrants(monitorCtx)
	frames := make(chan *peerbroker.Frame, 1)
	readErrors := make(chan error, 1)
	go func() {
		for {
			f, err := conn.Recv()
			if err != nil {
				select {
				case readErrors <- err:
				case <-ctx.Done():
				}
				return
			}
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-readErrors:
			h.cfg.OnStatus("Signalling unavailable; established peer may continue. Reconnect requires this still-live rendezvous; start a new pairing after expiry.")
			<-ctx.Done()
			return ctx.Err()
		case f := <-frames:
			if f.Type == peerbroker.FrameError {
				h.cfg.OnStatus("Broker rejected a signalling frame.")
				continue
			}
			if f.Type != peerbroker.FrameSignal {
				continue
			}
			if err := h.handleSignal(ctx, f.Body); err != nil {
				// Errors are intentionally not reflected verbatim: malformed incoming
				// messages and upstream APIs can contain untrusted/private data.
				h.cfg.OnStatus("Peer handshake rejected; no access granted.")
			}
		}
	}
}

func (h *Host) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	h.mu.Lock()
	conn := h.conn
	h.mu.Unlock()
	if conn == nil {
		return errors.New("peer signalling unavailable")
	}
	return conn.Send(b)
}

func (h *Host) handleSignal(ctx context.Context, body json.RawMessage) error {
	var header struct {
		V    int    `json:"v"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &header); err != nil {
		return err
	}
	if header.V != pairing.Version {
		return errors.New("unsupported handshake version")
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	switch header.Type {
	case pairing.TypePairRequest:
		var req pairing.PairRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return err
		}
		pending, err := h.pairing.VerifyPairRequest(&req)
		if err != nil {
			return err
		}
		if err = h.pairing.Approve(handshakeCtx, pending, h.cfg.Approver); err != nil {
			_ = h.send(h.pairing.Reject(pending, "denied"))
			return err
		}
		ep, err := peer.NewEndpoint(h.endpointConfig)
		if err != nil {
			h.pairing.Reject(pending, "invalid")
			return err
		}
		answer, err := ep.AcceptOffer(handshakeCtx, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: pending.OfferSDP})
		if err != nil {
			_ = ep.Close()
			h.pairing.Reject(pending, "invalid")
			return err
		}
		signed, grant, err := h.pairing.CompletePairing(pending, answer.SDP)
		if err != nil {
			_ = ep.Close()
			return err
		}
		if err = h.attach(ctx, ep, grant); err != nil {
			_ = ep.Close()
			return err
		}
		if err = h.send(signed); err != nil {
			h.closeActive()
			return err
		}
		clear(h.link.Secret)
		h.link.Secret = nil
		h.cfg.OnStatus("Device approved for Hub browsing and agent chat. Hub security and registration management are excluded.")
		return nil
	case pairing.TypeReconnectHello:
		var hello pairing.ReconnectHello
		if err := json.Unmarshal(body, &hello); err != nil {
			return err
		}
		challenge, err := h.pairing.NewChallenge(&hello, h.session.ID)
		if err != nil {
			return err
		}
		return h.send(challenge)
	case pairing.TypeReconnectRequest:
		var req pairing.ReconnectRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return err
		}
		verified, err := h.pairing.VerifyReconnect(&req)
		if err != nil {
			return err
		}
		ep, err := peer.NewEndpoint(h.endpointConfig)
		if err != nil {
			return err
		}
		answer, err := ep.AcceptOffer(handshakeCtx, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: verified.OfferSDP})
		if err != nil {
			_ = ep.Close()
			return err
		}
		signed, err := h.pairing.AnswerReconnect(verified, answer.SDP)
		if err != nil {
			_ = ep.Close()
			return err
		}
		if err = h.attach(ctx, ep, verified.Grant); err != nil {
			_ = ep.Close()
			return err
		}
		if err = h.send(signed); err != nil {
			h.closeActive()
			return err
		}
		h.cfg.OnStatus("Approved device reconnected within the live pilot rendezvous.")
		return nil
	default:
		return errors.New("unknown peer handshake")
	}
}

func (h *Host) attach(ctx context.Context, ep *peer.Endpoint, grant pairing.Grant) error {
	check := func(context.Context) error {
		_, err := h.Grants().Authorize(grant.ID, ed25519.PublicKey(grant.DeviceKey), time.Now())
		return err
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auditedHandler(h.cfg.Handler, auditRecord(h.cfg.Audit), h.cfg.OnStatus).ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authenticatedGrantKey{}, grant)))
	})
	srv, err := peer.NewServer(peer.ServerConfig{Handler: handler, SessionGate: check, Authorize: func(*http.Request) error { return nil }})
	if err != nil {
		return err
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	ep.Serve(sessionCtx, srv)
	h.closeActive()
	h.mu.Lock()
	h.active = &activePeer{endpoint: ep, cancel: cancel, grant: grant}
	h.mu.Unlock()
	return nil
}

func (h *Host) monitorGrants(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.mu.Lock()
			active := h.active
			h.mu.Unlock()
			if active != nil {
				if _, err := h.Grants().Authorize(active.grant.ID, ed25519.PublicKey(active.grant.DeviceKey), time.Now()); err != nil {
					h.mu.Lock()
					if h.active == active {
						h.active = nil
						active.cancel()
						_ = active.endpoint.Close()
					}
					h.mu.Unlock()
					h.cfg.OnStatus("Paired device grant expired or was revoked; active peer closed.")
				}
			}
		}
	}
}
func (h *Host) closeActive() {
	h.mu.Lock()
	active := h.active
	h.active = nil
	h.mu.Unlock()
	if active != nil {
		active.cancel()
		_ = active.endpoint.Close()
	}
}

// Close tears down owned peers and removes the owned broker rendezvous. It
// never changes TURN service configuration or another host's sessions.
func (h *Host) Close() error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		stop := h.runCancel
		conn := h.conn
		h.mu.Unlock()
		if stop != nil {
			stop()
		}
		h.closeActive()
		if h.cfg.Audit != nil {
			defer h.cfg.Audit.Close()
		}
		if conn != nil {
			_ = conn.Close()
		}
		if h.session != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			h.closeErr = h.broker.Delete(ctx, h.session.ID, h.session.HostToken)
		}
	})
	return h.closeErr
}

func auditRecord(a *AuditFile) func(AuditEvent) error {
	if a == nil {
		return nil
	}
	return a.Record
}

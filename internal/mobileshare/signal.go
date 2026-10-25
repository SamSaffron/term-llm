package mobileshare

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/samsaffron/term-llm/internal/pairing"
	"github.com/samsaffron/term-llm/internal/peer"
	"github.com/samsaffron/term-llm/internal/peerbroker"
)

func (r *Runtime) handleSignal(conn *peerbroker.Conn, frame *peerbroker.Frame) {
	message, err := pairing.DecodeMessage(frame.Body)
	if err != nil {
		r.setWarning("a peer sent an invalid protocol message")
		return
	}
	switch value := message.(type) {
	case *pairing.PairRequest:
		if frame.PeerKind != peerbroker.PeerInvitation || frame.PeerID != value.InvitationID.String() {
			return
		}
		r.handlePairRequest(conn, frame.PeerKind, frame.PeerID, value)
	case *pairing.TransportHello:
		if frame.PeerKind != peerbroker.PeerDevice || frame.PeerID != value.DeviceID.String() {
			return
		}
		challenge, err := r.host.NewTransportChallenge(value)
		if err != nil {
			r.setWarning("a device reconnect was refused")
			return
		}
		_ = conn.Send(frame.PeerKind, frame.PeerID, marshalSignal(challenge))
	case *pairing.TransportRequest:
		if frame.PeerKind != peerbroker.PeerDevice || frame.PeerID != value.DeviceID.String() {
			return
		}
		go r.acceptTransport(conn, frame.PeerKind, frame.PeerID, value)
	}
}

func (r *Runtime) handlePairRequest(conn *peerbroker.Conn, kind, id string, request *pairing.PairRequest) {
	pending, err := r.host.VerifyPairRequest(request)
	if err != nil {
		if errors.Is(err, pairing.ErrReplay) {
			r.mu.Lock()
			delete(r.pending, id)
			r.invitation = nil
			r.warnings = append(r.warnings, "the invitation was used by more than one device and was cancelled")
			r.mu.Unlock()
		}
		return
	}
	r.mu.Lock()
	r.pending[id] = &pendingRequest{pairing: pending, kind: kind, id: id, created: r.opts.Now().UTC()}
	r.mu.Unlock()
}

type fixedApprover bool

func (a fixedApprover) ApprovePairing(context.Context, pairing.PairingPrompt) (bool, error) {
	return bool(a), nil
}

func (r *Runtime) Approve(ctx context.Context, id, code string) error {
	r.mu.Lock()
	pending := r.pending[id]
	conn := r.brokerConn
	r.mu.Unlock()
	if pending == nil {
		return pairing.ErrUnknownInvitation
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(code)), []byte(pending.pairing.SAS)) != 1 {
		r.host.Reject(pending.pairing, "code mismatch")
		r.mu.Lock()
		delete(r.pending, id)
		r.invitation = nil
		r.warnings = append(r.warnings, "the confirmation code did not match; the invitation was cancelled")
		r.mu.Unlock()
		return pairing.ErrDenied
	}
	if err := r.host.Approve(ctx, pending.pairing, fixedApprover(true)); err != nil {
		return err
	}
	answer, grant, err := r.host.CompletePairing(pending.pairing)
	if err != nil {
		return err
	}
	// Publish the new reconnect hash before revealing the raw capability. This
	// makes pair_answer the commit point from the device's perspective.
	if err := r.renewNow(ctx); err != nil {
		_ = r.grants.Remove(grant.DeviceID)
		return err
	}
	if conn == nil {
		_ = r.grants.Remove(grant.DeviceID)
		return errors.New("rendezvous disconnected before approval completed")
	}
	if err := conn.Send(pending.kind, pending.id, marshalSignal(answer)); err != nil {
		_ = r.grants.Remove(grant.DeviceID)
		_ = r.renewNow(ctx)
		return err
	}
	r.mu.Lock()
	delete(r.pending, id)
	r.invitation = nil
	r.mu.Unlock()
	return nil
}

func (r *Runtime) Deny(id string) error {
	r.mu.Lock()
	pending := r.pending[id]
	conn := r.brokerConn
	delete(r.pending, id)
	r.invitation = nil
	r.mu.Unlock()
	if pending == nil {
		return pairing.ErrUnknownInvitation
	}
	reject := r.host.Reject(pending.pairing, "denied by owner")
	if conn != nil {
		_ = conn.Send(pending.kind, pending.id, marshalSignal(reject))
	}
	return nil
}

func (r *Runtime) renewNow(ctx context.Context) error {
	hashes, err := r.reconnectHashes()
	if err != nil {
		return err
	}
	r.mu.Lock()
	hostID, capability, connected := r.hostID, r.leaseCapability, r.brokerConnected
	r.mu.Unlock()
	if !connected {
		return errors.New("rendezvous is not connected")
	}
	_, err = r.broker.Renew(ctx, hostID, capability, hashes)
	return err
}

func (r *Runtime) acceptTransport(conn *peerbroker.Conn, kind, id string, request *pairing.TransportRequest) {
	verified, err := r.host.VerifyTransport(request)
	if err != nil {
		return
	}
	r.mu.Lock()
	if !r.settings.Enabled || len(r.sessions) >= maxConnections && r.sessions[id] == nil {
		r.mu.Unlock()
		return
	}
	hostID, leaseCapability := r.hostID, r.leaseCapability
	r.mu.Unlock()
	ice, err := r.broker.ICE(r.ctx, hostID, peerbroker.PeerHost, "", leaseCapability)
	if err != nil {
		return
	}
	endpoint, err := peer.NewEndpoint(peer.EndpointConfig{ICEServers: toICEServers(ice.ICEServers)})
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(r.ctx)
	server, err := peer.NewServer(peer.ServerConfig{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			r.opts.Handler.ServeHTTP(w, WithDevicePrincipal(request, DevicePrincipal{DeviceID: id, Scope: ScopeWebV1}))
		}),
		SessionGate: func(context.Context) error { _, err := r.grants.Authorize(id, verified.DeviceKey); return err },
		Authorize: func(request *http.Request) error {
			if !RouteAllowed(request.Method, request.URL.Path) {
				return errors.New("route is not available to paired devices")
			}
			return nil
		},
		MaxChannels: 1,
	})
	if err != nil {
		cancel()
		_ = endpoint.Close()
		return
	}
	endpoint.Serve(ctx, server)
	answerDescription, err := endpoint.AcceptOffer(ctx, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: verified.OfferSDP})
	if err != nil {
		cancel()
		_ = endpoint.Close()
		return
	}
	answer, err := r.host.AnswerTransport(verified, answerDescription.SDP)
	if err != nil {
		cancel()
		_ = endpoint.Close()
		return
	}
	r.mu.Lock()
	if old := r.sessions[id]; old != nil {
		old.cancel()
		_ = old.endpoint.Close()
	}
	r.sessions[id] = &deviceSession{endpoint: endpoint, cancel: cancel}
	r.mu.Unlock()
	if err := conn.Send(kind, id, marshalSignal(answer)); err != nil {
		r.mu.Lock()
		if r.sessions[id] != nil && r.sessions[id].endpoint == endpoint {
			delete(r.sessions, id)
		}
		r.mu.Unlock()
		cancel()
		_ = endpoint.Close()
	}
}

func (r *Runtime) Enable(ctx context.Context) error {
	r.mu.Lock()
	if r.availableReason != "" {
		reason := r.availableReason
		r.mu.Unlock()
		return errors.New(reason)
	}
	already := r.settings.Enabled && r.brokerConnected
	r.started = true
	r.settings.Enabled = true
	if err := r.persistSettingsLocked(); err != nil {
		r.mu.Unlock()
		return err
	}
	r.mu.Unlock()
	if already {
		return nil
	}
	return r.connectBroker(ctx)
}

func (r *Runtime) Stop() error {
	r.mu.Lock()
	r.settings.Enabled = false
	err := r.persistSettingsLocked()
	if r.invitation != nil {
		r.host.CancelPairing(r.invitation.link.InvitationID)
		r.invitation = nil
	}
	r.pending = map[string]*pendingRequest{}
	if r.brokerConn != nil {
		_ = r.brokerConn.Close()
		r.brokerConn = nil
	}
	r.brokerConnected = false
	r.closeSessionsLocked()
	r.mu.Unlock()
	return err
}

func (r *Runtime) CancelInvitation() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.invitation != nil {
		r.host.CancelPairing(r.invitation.link.InvitationID)
		r.invitation = nil
	}
	r.pending = map[string]*pendingRequest{}
}

func (r *Runtime) Revoke(ctx context.Context, id string) error {
	if err := r.grants.Revoke(id, r.opts.Now()); err != nil {
		return err
	}
	r.mu.Lock()
	if session := r.sessions[id]; session != nil {
		session.cancel()
		_ = session.endpoint.Close()
		delete(r.sessions, id)
	}
	r.mu.Unlock()
	return r.renewNow(ctx)
}

func (r *Runtime) Reset(ctx context.Context) error {
	if err := r.Stop(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range []string{"identity.json", "devices.json", "identity.json.lock", "devices.json.lock"} {
		_ = os.Remove(filepath.Join(r.dir, name))
	}
	if err := writeJSONPrivate(filepath.Join(r.dir, "target.json"), targetState{Version: stateVersion, Target: r.opts.Target, StoreInstanceID: r.opts.StoreInstanceID, BrokerURL: r.opts.BrokerURL}); err != nil {
		return err
	}
	id, err := pairing.LoadOrCreateIdentity(filepath.Join(r.dir, "identity.json"))
	if err != nil {
		return err
	}
	grants, err := pairing.OpenGrantStore(filepath.Join(r.dir, "devices.json"))
	if err != nil {
		return err
	}
	host, err := pairing.NewHost(id, grants, pairing.HostOptions{Broker: r.opts.BrokerURL, HostLabel: r.opts.HostLabel, BasePath: r.opts.BasePath, PairingTTL: invitationTTL, MaxPending: maxDevices, Now: r.opts.Now})
	if err != nil {
		return err
	}
	r.identity, r.grants, r.host, r.warnings = id, grants, host, nil
	r.availableReason = ""
	r.evaluateAvailability()
	return nil
}

func (r *Runtime) NewInvitation(ctx context.Context, qr func(string) (string, error)) error {
	if err := r.Enable(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	if len(r.mustListGrants()) >= maxDevices {
		r.mu.Unlock()
		return errors.New("the maximum of 16 paired devices has been reached")
	}
	if r.invitation != nil {
		r.host.CancelPairing(r.invitation.link.InvitationID)
	}
	hostID, capability := r.hostID, r.leaseCapability
	r.mu.Unlock()
	id, err := randomToken(pairing.InvitationIDSize)
	if err != nil {
		return err
	}
	invitationCapability, err := peerbroker.GenerateCapability()
	if err != nil {
		return err
	}
	expires := r.opts.Now().Add(invitationTTL).UTC().Truncate(time.Second)
	if _, err := r.broker.RegisterInvitation(ctx, hostID, capability, id, peerbroker.HashCapability(invitationCapability), expires); err != nil {
		return err
	}
	link, err := r.host.NewPairingForInvitation(id, invitationCapability, expires)
	if err != nil {
		return err
	}
	qrDataURL, err := qr(link.String())
	if err != nil {
		r.host.CancelPairing(link.InvitationID)
		return fmt.Errorf("encode pairing QR: %w", err)
	}
	r.mu.Lock()
	r.invitation = &invitation{id: id, capability: invitationCapability, link: link, qrDataURL: qrDataURL}
	r.pending = map[string]*pendingRequest{}
	r.mu.Unlock()
	return nil
}

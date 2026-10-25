package mobileshare

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/samsaffron/term-llm/internal/appdata"
	"github.com/samsaffron/term-llm/internal/filelock"
	"github.com/samsaffron/term-llm/internal/pairing"
	"github.com/samsaffron/term-llm/internal/peer"
	"github.com/samsaffron/term-llm/internal/peerbroker"
)

const (
	invitationTTL  = 5 * time.Minute
	leaseRenewal   = time.Minute
	maxDevices     = 16
	maxConnections = 4
)

type Options struct {
	DataDir             string
	Target              string
	StoreInstanceID     string
	BasePath            string
	Authenticated       bool
	AgentName           string
	HostLabel           string
	ApprovalDescription string
	BrokerURL           string
	AdmissionCredential string
	Handler             http.Handler
	HTTPClient          *http.Client
	Now                 func() time.Time
}

type invitation struct {
	id, capability string
	link           pairing.Link
	qrDataURL      string
}

type pendingRequest struct {
	pairing  *pairing.PendingPairing
	kind, id string
	created  time.Time
}

type deviceSession struct {
	endpoint *peer.Endpoint
	cancel   context.CancelFunc
}

// Runtime owns one target's durable grants, rendezvous lease and live peers.
type Runtime struct {
	mu                      sync.Mutex
	opts                    Options
	dir                     string
	availableReason         string
	settings                settingsState
	identity                *pairing.Identity
	grants                  *pairing.GrantStore
	host                    *pairing.Host
	broker                  *peerbroker.Client
	hostID, leaseCapability string
	brokerConn              *peerbroker.Conn
	brokerConnected         bool
	connecting              bool
	invitation              *invitation
	pending                 map[string]*pendingRequest
	sessions                map[string]*deviceSession
	warnings                []string
	ctx                     context.Context
	cancel                  context.CancelFunc
	wg                      sync.WaitGroup
	unlock                  func() error
	started                 bool
}

// NewServer constructs the standard serve-web runtime without exposing
// operator-only broker injection as ordinary serve configuration.
func NewServer(target, storeID, basePath string, authenticated bool, hostLabel, approval string) (*Runtime, error) {
	return New(Options{Target: target, StoreInstanceID: storeID, BasePath: basePath, Authenticated: authenticated, AgentName: target, HostLabel: hostLabel, ApprovalDescription: approval})
}

func New(opts Options) (*Runtime, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.BasePath == "" {
		opts.BasePath = "/"
	}
	if opts.Target == "" {
		opts.Target = "default"
	}
	if opts.BrokerURL == "" {
		opts.BrokerURL = strings.TrimSpace(os.Getenv(BrokerURLEnv))
	}
	if opts.AdmissionCredential == "" {
		opts.AdmissionCredential = strings.TrimSpace(os.Getenv(AdmissionEnv))
	}
	r := &Runtime{opts: opts, settings: settingsState{Version: stateVersion}, pending: map[string]*pendingRequest{}, sessions: map[string]*deviceSession{}}
	if opts.DataDir == "" {
		var err error
		opts.DataDir, err = appdata.GetDataDir()
		if err != nil {
			return nil, fmt.Errorf("resolve mobile sharing data directory: %w", err)
		}
		r.opts.DataDir = opts.DataDir
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.dir = filepath.Join(opts.DataDir, "mobile-sharing", targetDirectory(opts.Target))
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return nil, fmt.Errorf("mobile sharing state directory: %w", err)
	}
	if err := os.Chmod(r.dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure mobile sharing state directory: %w", err)
	}
	unlock, err := filelock.TryLock(filepath.Join(r.dir, "runtime.lock"))
	if err != nil {
		r.availableReason = "mobile sharing is active in another server"
		return r, nil
	}
	r.unlock = unlock
	if err := r.loadState(); err != nil {
		_ = r.Close()
		return nil, err
	}
	r.evaluateAvailability()
	return r, nil
}

func targetDirectory(target string) string {
	sum := sha256.Sum256([]byte(target))
	name := strings.Map(func(ch rune) rune {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' {
			return ch
		}
		return '_'
	}, target)
	if len(name) > 40 {
		name = name[:40]
	}
	return name + "-" + base64.RawURLEncoding.EncodeToString(sum[:6])
}

func (r *Runtime) loadState() error {
	targetPath := filepath.Join(r.dir, "target.json")
	var target targetState
	exists, err := loadJSON(targetPath, &target)
	if err != nil {
		return err
	}
	if exists {
		if target.Version != stateVersion || target.Target != r.opts.Target || target.StoreInstanceID != r.opts.StoreInstanceID {
			r.availableReason = "mobile sharing state belongs to a different session store; reset is required"
			return nil
		}
		if target.BrokerURL != "" && r.opts.BrokerURL != "" && target.BrokerURL != r.opts.BrokerURL {
			r.availableReason = "the rendezvous service changed; forget all phones before continuing"
			return nil
		}
	} else {
		target = targetState{Version: stateVersion, Target: r.opts.Target, StoreInstanceID: r.opts.StoreInstanceID, BrokerURL: r.opts.BrokerURL}
		if err := writeJSONPrivate(targetPath, target); err != nil {
			return err
		}
	}
	if ok, err := loadJSON(filepath.Join(r.dir, "settings.json"), &r.settings); err != nil {
		return err
	} else if ok && r.settings.Version != stateVersion {
		return errors.New("mobile sharing settings use an unsupported version")
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
	r.identity, r.grants, r.host = id, grants, host
	if r.opts.BrokerURL != "" {
		r.broker, err = peerbroker.NewClient(r.opts.BrokerURL, r.opts.HTTPClient)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) evaluateAvailability() {
	if r.availableReason != "" {
		return
	}
	switch {
	case !r.opts.Authenticated:
		r.availableReason = "mobile sharing requires bearer or passkey authentication"
	case !peer.PlatformSupported():
		r.availableReason = "mobile sharing is not supported on this platform"
	case r.opts.BrokerURL == "":
		r.availableReason = "no rendezvous service is configured in this build"
	}
}

func (r *Runtime) SetHandler(handler http.Handler) {
	r.mu.Lock()
	r.opts.Handler = handler
	r.mu.Unlock()
}

func (r *Runtime) Start() error {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return nil
	}
	r.started = true
	enabled := r.settings.Enabled && r.availableReason == ""
	r.mu.Unlock()
	if enabled {
		return r.connectBroker(r.ctx)
	}
	return nil
}

func (r *Runtime) persistSettingsLocked() error {
	return writeJSONPrivate(filepath.Join(r.dir, "settings.json"), r.settings)
}

func (r *Runtime) reconnectHashes() ([]peerbroker.DeviceCapabilityHash, error) {
	grants, err := r.grants.List()
	if err != nil {
		return nil, err
	}
	out := make([]peerbroker.DeviceCapabilityHash, 0, len(grants))
	for _, grant := range grants {
		if grant.RevokedAt == nil {
			out = append(out, peerbroker.DeviceCapabilityHash{DeviceID: grant.DeviceID, Hash: pairing.B64(grant.ReconnectCapabilityHash).String()})
		}
	}
	return out, nil
}

func (r *Runtime) pinBroker() error {
	path := filepath.Join(r.dir, "target.json")
	var target targetState
	if ok, err := loadJSON(path, &target); err != nil || !ok {
		return err
	}
	if target.BrokerURL == "" {
		target.BrokerURL = r.opts.BrokerURL
		return writeJSONPrivate(path, target)
	}
	if target.BrokerURL != r.opts.BrokerURL {
		return errors.New("the rendezvous service changed; forget all phones before continuing")
	}
	return nil
}

func (r *Runtime) connectBroker(ctx context.Context) error {
	r.mu.Lock()
	if r.brokerConnected {
		r.mu.Unlock()
		return nil
	}
	if r.connecting {
		r.mu.Unlock()
		return errors.New("rendezvous connection is already starting")
	}
	r.connecting = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.connecting = false; r.mu.Unlock() }()
	if err := r.pinBroker(); err != nil {
		return err
	}
	hashes, err := r.reconnectHashes()
	if err != nil {
		return err
	}
	registration, err := r.broker.RegisterHostWithSigner(ctx, r.identity.PublicKey(), r.opts.AdmissionCredential, hashes, func(payload []byte) ([]byte, error) { return r.identity.Sign(payload), nil })
	if err != nil {
		r.setWarning("rendezvous unreachable")
		return err
	}
	conn, err := r.broker.DialHost(ctx, registration.HostID, registration.LeaseCapability)
	if err != nil {
		r.setWarning("rendezvous unreachable")
		return err
	}
	r.mu.Lock()
	if !r.settings.Enabled || r.ctx.Err() != nil {
		r.mu.Unlock()
		_ = conn.Close()
		return context.Canceled
	}
	old := r.brokerConn
	r.hostID, r.leaseCapability, r.brokerConn, r.brokerConnected = registration.HostID, registration.LeaseCapability, conn, true
	r.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	r.wg.Add(2)
	go r.receiveLoop(conn)
	go r.renewLoop(conn)
	return nil
}

func (r *Runtime) renewLoop(conn *peerbroker.Conn) {
	defer r.wg.Done()
	ticker := time.NewTicker(leaseRenewal)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.mu.Lock()
			current := r.brokerConn == conn && r.settings.Enabled
			hostID, capability := r.hostID, r.leaseCapability
			r.mu.Unlock()
			if !current {
				return
			}
			hashes, err := r.reconnectHashes()
			if err == nil {
				_, err = r.broker.Renew(r.ctx, hostID, capability, hashes)
			}
			if err != nil {
				r.setWarning("rendezvous unreachable")
			}
		}
	}
}

func (r *Runtime) receiveLoop(conn *peerbroker.Conn) {
	defer r.wg.Done()
	for {
		frame, err := conn.Recv()
		if err != nil {
			r.mu.Lock()
			if r.brokerConn == conn {
				r.brokerConnected = false
				r.brokerConn = nil
			}
			enabled := r.settings.Enabled
			r.mu.Unlock()
			if enabled && r.ctx.Err() == nil {
				r.setWarning("rendezvous disconnected; retry from Resume")
			}
			return
		}
		if frame.Type == peerbroker.FrameSignal {
			r.handleSignal(conn, frame)
		}
	}
}

func (r *Runtime) setWarning(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.warnings {
		if existing == message {
			return
		}
	}
	r.warnings = append(r.warnings, message)
	if len(r.warnings) > 4 {
		r.warnings = r.warnings[len(r.warnings)-4:]
	}
}

func randomToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func toICEServers(in []peerbroker.ICEServer) []webrtc.ICEServer {
	out := make([]webrtc.ICEServer, 0, len(in))
	for _, server := range in {
		out = append(out, webrtc.ICEServer{URLs: append([]string(nil), server.URLs...), Username: server.Username, Credential: server.Credential})
	}
	return out
}

func (r *Runtime) closeSessionsLocked() {
	for id, session := range r.sessions {
		session.cancel()
		_ = session.endpoint.Close()
		delete(r.sessions, id)
	}
}

func (r *Runtime) Close() error {
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Lock()
	if r.brokerConn != nil {
		_ = r.brokerConn.Close()
		r.brokerConn = nil
	}
	r.closeSessionsLocked()
	unlock := r.unlock
	r.unlock = nil
	r.mu.Unlock()
	r.wg.Wait()
	if unlock != nil {
		return unlock()
	}
	return nil
}

func (r *Runtime) Status() Status { return r.status(false) }

func (r *Runtime) status(includeInvitation bool) Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.invitation != nil && !r.opts.Now().Before(r.invitation.link.ExpiresAt) {
		r.host.CancelPairing(r.invitation.link.InvitationID)
		r.invitation = nil
		r.pending = map[string]*pendingRequest{}
	}
	status := Status{Enabled: r.settings.Enabled, BrokerConnected: r.brokerConnected, PendingRequests: []RequestStatus{}, Devices: []DeviceStatus{}, Warnings: append([]string(nil), r.warnings...), Authority: r.authority()}
	if r.identity != nil {
		status.HostFingerprint = r.identity.Fingerprint()
	}
	if r.availableReason != "" {
		status.State, status.Reason = StateUnavailable, r.availableReason
		return status
	}
	if !r.settings.Enabled {
		if len(r.mustListGrants()) == 0 {
			status.State = StateOff
		} else {
			status.State = StateStopped
		}
		status.Devices = r.deviceStatuses()
		return status
	}
	status.Devices = r.deviceStatuses()
	for id, request := range r.pending {
		status.PendingRequests = append(status.PendingRequests, RequestStatus{ID: id, Name: request.pairing.DeviceName, RequestedAt: request.created})
	}
	sort.Slice(status.PendingRequests, func(i, j int) bool {
		return status.PendingRequests[i].RequestedAt.Before(status.PendingRequests[j].RequestedAt)
	})
	if len(status.PendingRequests) > 0 {
		status.State = StatePending
	} else if r.invitation != nil {
		status.State = StateInviting
		if includeInvitation {
			status.Invitation = &InvitationStatus{URL: r.invitation.link.String(), QRDataURL: r.invitation.qrDataURL, ExpiresAt: r.invitation.link.ExpiresAt}
		}
	} else {
		status.State = StateSharing
	}
	return status
}

func (r *Runtime) mustListGrants() []pairing.Grant {
	if r.grants == nil {
		return nil
	}
	grants, err := r.grants.List()
	if err != nil {
		return nil
	}
	return grants
}
func (r *Runtime) deviceStatuses() []DeviceStatus {
	grants := r.mustListGrants()
	out := make([]DeviceStatus, 0, len(grants))
	for _, grant := range grants {
		if grant.RevokedAt != nil {
			continue
		}
		connection := "offline"
		if session, ok := r.sessions[grant.DeviceID]; ok {
			connection = "connected"
			if stats, selected, _ := session.endpoint.SelectedPair(); selected {
				if stats.LocalCandidateType == "relay" || stats.RemoteCandidateType == "relay" {
					connection = "relay"
				} else {
					connection = "direct"
				}
			}
		}
		out = append(out, DeviceStatus{ID: grant.DeviceID, Name: grant.DeviceName, ApprovedAt: grant.PairedAt, LastSeenAt: grant.LastSeenAt, Connection: connection})
	}
	return out
}
func (r *Runtime) authority() AuthorityStatement {
	userName, host := "this server user", r.opts.HostLabel
	if current, err := user.Current(); err == nil {
		userName = current.Username
	}
	if host == "" {
		host, _ = os.Hostname()
	}
	description := r.opts.ApprovalDescription
	if description == "" {
		description = fmt.Sprintf("can see every conversation this server shows; messages drive tools under this server's approval mode, running as %s on %s", userName, host)
	}
	return AuthorityStatement{AgentName: r.opts.AgentName, Description: description, SameUIDNote: "The agent and this sharing runtime use the same OS identity. Revocation stops future network connections only; it is not isolation from the agent."}
}

func marshalSignal(value any) json.RawMessage { data, _ := json.Marshal(value); return data }

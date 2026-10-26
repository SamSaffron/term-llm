package linkeddevices

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
	"slices"
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
	invitationTTL    = 5 * time.Minute
	leaseRenewal     = time.Minute
	renewRetryDelay  = 250 * time.Millisecond
	reconnectInitial = 250 * time.Millisecond
	reconnectMaximum = 30 * time.Second
	maxDevices       = 16
	maxConnections   = 4
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

type brokerRegistrationAttempt struct {
	ctx        context.Context
	generation uint64
	broker     *peerbroker.Client
	identity   *pairing.Identity
	grants     *pairing.GrantStore
}

type deviceSession struct {
	endpoint *peer.Endpoint
	cancel   context.CancelFunc
}

// Runtime owns one target's durable grants, rendezvous lease and live peers.
type Runtime struct {
	mu                      sync.Mutex
	lifecycleMu             sync.Mutex
	renewMu                 sync.Mutex
	opts                    Options
	dir                     string
	availableReason         string
	resetRequired           bool
	settings                settingsState
	identity                *pairing.Identity
	grants                  *pairing.GrantStore
	host                    *pairing.Host
	broker                  *peerbroker.Client
	hostID, leaseCapability string
	brokerConn              *peerbroker.Conn
	brokerConnected         bool
	connecting              bool
	reconnecting            bool
	invitation              *invitation
	pending                 map[string]*pendingRequest
	sessions                map[string]*deviceSession
	warnings                []string
	ctx                     context.Context
	cancel                  context.CancelFunc
	sharingCtx              context.Context
	sharingCancel           context.CancelFunc
	generation              uint64
	handshakes              int
	newHandshakes           int
	peerRequests            sync.WaitGroup
	wg                      sync.WaitGroup
	unlock                  func() error
	started                 bool
}

// NewServer constructs the standard serve-web runtime without exposing
// operator-only broker injection as ordinary serve configuration.
func NewServer(target, storeID, basePath string, authenticated bool, hostLabel, approval string) (*Runtime, error) {
	opts := Options{Target: target, StoreInstanceID: storeID, BasePath: basePath, Authenticated: authenticated, AgentName: target, HostLabel: safeHostLabel(hostLabel, target), ApprovalDescription: approval}
	if storeID == "" {
		return newUnavailableRuntime(opts, "linked devices cannot identify this session store"), nil
	}
	runtime, err := New(opts)
	if err != nil {
		return newUnavailableRuntime(opts, "linked devices local state could not be initialized"), nil
	}
	return runtime, nil
}

func newUnavailableRuntime(opts Options, reason string) *Runtime {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runtime{
		opts: opts, availableReason: reason, settings: settingsState{Version: stateVersion},
		pending: map[string]*pendingRequest{}, sessions: map[string]*deviceSession{}, ctx: ctx, cancel: cancel,
	}
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
			return nil, fmt.Errorf("resolve linked devices data directory: %w", err)
		}
		r.opts.DataDir = opts.DataDir
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.dir = filepath.Join(opts.DataDir, "linked-devices", targetDirectory(opts.Target))
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return nil, fmt.Errorf("linked devices state directory: %w", err)
	}
	if err := os.Chmod(r.dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure linked devices state directory: %w", err)
	}
	unlock, err := filelock.TryLock(filepath.Join(r.dir, "runtime.lock"))
	if err != nil {
		r.availableReason = "linked devices are active in another server"
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
			r.availableReason = "linked devices state belongs to a different session store; reset is required"
			r.resetRequired = true
			return nil
		}
		if target.BrokerURL != "" && r.opts.BrokerURL != "" && target.BrokerURL != r.opts.BrokerURL {
			r.availableReason = "the rendezvous service changed; forget all devices before continuing"
			r.resetRequired = true
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
		return errors.New("linked devices settings use an unsupported version")
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
		r.availableReason = "linked devices requires bearer or passkey authentication"
	case !peer.PlatformSupported():
		r.availableReason = "linked devices are not supported on this platform"
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
	r.lifecycleMu.Lock()
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		r.lifecycleMu.Unlock()
		return nil
	}
	r.started = true
	enabled := r.settings.Enabled && r.availableReason == ""
	if !enabled {
		r.mu.Unlock()
		r.lifecycleMu.Unlock()
		return nil
	}
	r.startSharingLocked()
	attempt := r.registrationAttemptLocked()
	r.wg.Add(1)
	r.mu.Unlock()
	r.lifecycleMu.Unlock()
	if err := r.connectBroker(attempt.ctx, attempt); err != nil {
		r.scheduleReconnect()
	}
	r.wg.Done()
	return nil
}

func (r *Runtime) persistSettingsLocked() error {
	return writeJSONPrivate(filepath.Join(r.dir, "settings.json"), r.settings)
}

func reconnectHashes(grants *pairing.GrantStore) ([]peerbroker.DeviceCapabilityHash, error) {
	values, err := grants.List()
	if err != nil {
		return nil, err
	}
	out := make([]peerbroker.DeviceCapabilityHash, 0, len(values))
	for _, grant := range values {
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
		return errors.New("the rendezvous service changed; forget all devices before continuing")
	}
	return nil
}

func (r *Runtime) registrationAttemptLocked() brokerRegistrationAttempt {
	return brokerRegistrationAttempt{
		ctx: r.sharingCtx, generation: r.generation,
		broker: r.broker, identity: r.identity, grants: r.grants,
	}
}

func (r *Runtime) connectBroker(connectCtx context.Context, attempt brokerRegistrationAttempt) error {
	r.mu.Lock()
	if r.brokerConnected && r.generation == attempt.generation {
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
	hashes, err := reconnectHashes(attempt.grants)
	if err != nil {
		return err
	}
	registration, err := attempt.broker.RegisterHostWithSigner(connectCtx, attempt.identity.PublicKey(), r.opts.AdmissionCredential, hashes, func(payload []byte) ([]byte, error) { return attempt.identity.Sign(payload), nil })
	if err != nil {
		r.setWarning("rendezvous unreachable")
		return err
	}
	conn, err := attempt.broker.DialHost(connectCtx, registration.HostID, registration.LeaseCapability)
	if err != nil {
		r.setWarning("rendezvous unreachable")
		return err
	}
	r.mu.Lock()
	if !r.settings.Enabled || r.ctx.Err() != nil || attempt.ctx.Err() != nil ||
		r.generation != attempt.generation || r.identity != attempt.identity || r.broker != attempt.broker {
		r.mu.Unlock()
		_ = conn.Close()
		return context.Canceled
	}
	old := r.brokerConn
	r.wg.Add(2)
	r.hostID, r.leaseCapability, r.brokerConn, r.brokerConnected = registration.HostID, registration.LeaseCapability, conn, true
	r.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	r.clearWarnings("rendezvous unreachable", "rendezvous disconnected; reconnecting")
	go r.receiveLoop(conn)
	go r.renewLoop(attempt.ctx, conn)
	return nil
}

func (r *Runtime) renewLoop(ctx context.Context, conn *peerbroker.Conn) {
	defer r.wg.Done()
	ticker := time.NewTicker(leaseRenewal)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.mu.Lock()
			current := r.brokerConn == conn && r.settings.Enabled
			ctx := r.sharingCtx
			r.mu.Unlock()
			if !current || ctx == nil {
				return
			}
			if err := r.renewWithRetry(ctx); err != nil {
				r.setWarning("rendezvous unreachable")
				_ = conn.Close()
				return
			}
		}
	}
}

func (r *Runtime) renewWithRetry(ctx context.Context) error {
	if err := r.renewNow(ctx); err == nil {
		return nil
	}
	timer := time.NewTimer(renewRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return r.renewNow(ctx)
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
				r.setWarning("rendezvous disconnected; reconnecting")
				r.scheduleReconnect()
			}
			return
		}
		if frame.Type == peerbroker.FrameSignal {
			r.handleSignal(conn, frame)
		} else if frame.Type == peerbroker.FramePeer && frame.PeerKind == peerbroker.PeerInvitation && frame.PeerConnected != nil && !*frame.PeerConnected {
			r.cancelDisconnectedInvitation(frame.PeerID)
		}
	}
}

func (r *Runtime) scheduleReconnect() {
	r.mu.Lock()
	if r.reconnecting || !r.settings.Enabled || r.availableReason != "" || r.sharingCtx == nil || r.sharingCtx.Err() != nil {
		r.mu.Unlock()
		return
	}
	ctx, generation := r.sharingCtx, r.generation
	r.reconnecting = true
	r.wg.Add(1)
	r.mu.Unlock()
	go r.reconnectLoop(ctx, generation)
}

func (r *Runtime) reconnectLoop(ctx context.Context, generation uint64) {
	defer r.wg.Done()
	defer func() {
		r.mu.Lock()
		r.reconnecting = false
		r.mu.Unlock()
	}()
	delay := reconnectInitial
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		r.mu.Lock()
		current := r.settings.Enabled && r.generation == generation && !r.brokerConnected
		attempt := r.registrationAttemptLocked()
		r.mu.Unlock()
		if !current {
			return
		}
		if err := r.connectBroker(ctx, attempt); err == nil {
			return
		}
		delay = min(delay*2, reconnectMaximum)
	}
}

func (r *Runtime) clearWarnings(messages ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, message := range messages {
		r.warnings = slices.DeleteFunc(r.warnings, func(existing string) bool { return existing == message })
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

func (r *Runtime) startSharingLocked() (context.Context, uint64) {
	if r.sharingCtx == nil || r.sharingCtx.Err() != nil {
		r.generation++
		r.sharingCtx, r.sharingCancel = context.WithCancel(r.ctx)
	}
	return r.sharingCtx, r.generation
}

func (r *Runtime) stopSharingLocked() {
	r.generation++
	if r.sharingCancel != nil {
		r.sharingCancel()
	}
	r.sharingCtx, r.sharingCancel = nil, nil
}

func (r *Runtime) beginPeerRequest(generation uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.settings.Enabled || r.generation != generation || r.ctx.Err() != nil {
		return false
	}
	r.peerRequests.Add(1)
	return true
}

func (r *Runtime) closeSessionsLocked() {
	for id, session := range r.sessions {
		session.cancel()
		_ = session.endpoint.Close()
		delete(r.sessions, id)
	}
}

func (r *Runtime) Close() error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Lock()
	r.settings.Enabled = false
	r.stopSharingLocked()
	if r.brokerConn != nil {
		_ = r.brokerConn.Close()
		r.brokerConn = nil
	}
	r.closeSessionsLocked()
	unlock := r.unlock
	r.unlock = nil
	r.mu.Unlock()
	r.peerRequests.Wait()
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
	now := r.opts.Now()
	if r.invitation != nil && !now.Before(r.invitation.link.ExpiresAt) {
		r.host.CancelPairing(r.invitation.link.InvitationID)
		r.invitation = nil
		r.pending = map[string]*pendingRequest{}
	}
	for id, pending := range r.pending {
		if !now.Before(pending.pairing.ExpiresAt) {
			r.host.CancelPairing(pending.pairing.InvitationID)
			delete(r.pending, id)
			if r.invitation != nil && r.invitation.id == id {
				r.invitation = nil
			}
		}
	}
	status := Status{Enabled: r.settings.Enabled, ResetRequired: r.resetRequired, BrokerConnected: r.brokerConnected, PendingRequests: []RequestStatus{}, Devices: []DeviceStatus{}, Warnings: append([]string(nil), r.warnings...), Authority: r.authority()}
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
	if !r.brokerConnected {
		status.State = StateReconnecting
		return status
	}
	for id, request := range r.pending {
		status.PendingRequests = append(status.PendingRequests, RequestStatus{ID: id, Name: request.pairing.DeviceName, RequestedAt: request.created, ExpiresAt: request.pairing.ExpiresAt})
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

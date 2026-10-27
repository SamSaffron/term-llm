package pairing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

type HostOptions struct {
	Broker       string
	HostLabel    string
	BasePath     string
	PairingTTL   time.Duration
	ApprovalTTL  time.Duration
	ChallengeTTL time.Duration
	MaxPending   int
	Now          func() time.Time
}

type invitationState uint8

const (
	invitationOpen invitationState = iota
	invitationClaimed
	invitationApproved
)

type pairInvitation struct {
	id, secret []byte
	expires    time.Time
	state      invitationState
	pending    *PendingPairing
}
type transportChallenge struct {
	id, nonce, deviceKey []byte
	deviceID             string
	expires              time.Time
}

// Host runs identity pairing and signed transport authentication.
type Host struct {
	id          *Identity
	grants      *GrantStore
	opts        HostOptions
	mu          sync.Mutex
	invitations map[string]*pairInvitation
	challenges  map[string]*transportChallenge
}

func NewHost(id *Identity, grants *GrantStore, opts HostOptions) (*Host, error) {
	if id == nil || grants == nil {
		return nil, fmt.Errorf("pairing: host needs an identity and grant store")
	}
	if opts.PairingTTL <= 0 {
		opts.PairingTTL = 5 * time.Minute
	}
	if opts.ApprovalTTL <= 0 {
		opts.ApprovalTTL = 2 * time.Minute
	}
	if opts.ChallengeTTL <= 0 {
		opts.ChallengeTTL = time.Minute
	}
	if opts.MaxPending <= 0 {
		opts.MaxPending = 32
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.BasePath == "" {
		opts.BasePath = "/"
	}
	if !validBasePath(opts.BasePath) {
		return nil, fmt.Errorf("%w: invalid base path", ErrInvalid)
	}
	if opts.HostLabel != "" {
		if err := ValidateDeviceName(opts.HostLabel); err != nil {
			return nil, fmt.Errorf("host label: %w", err)
		}
	}
	return &Host{id: id, grants: grants, opts: opts, invitations: map[string]*pairInvitation{}, challenges: map[string]*transportChallenge{}}, nil
}

func validBasePath(path string) bool {
	return strings.HasPrefix(path, "/") && !strings.ContainsAny(path, "?#\\\r\n") &&
		!strings.Contains(path, "..") && (&url.URL{Path: path}).EscapedPath() == path
}
func (h *Host) Identity() *Identity { return h.id }
func (h *Host) Grants() *GrantStore { return h.grants }

// NewPairingForInvitation creates a one-use pairing link bound to a broker
// invitation. brokerExpiresAt caps the local invitation lifetime.
func (h *Host) NewPairingForInvitation(invitationID, invitationCapability string, brokerExpiresAt time.Time) (Link, error) {
	id, err := DecodeB64(invitationID)
	if err != nil {
		return Link{}, fmt.Errorf("%w: invitation id", ErrInvalid)
	}
	if err := wantLen("invitation id", id, InvitationIDSize); err != nil {
		return Link{}, err
	}
	if brokerExpiresAt.IsZero() {
		return Link{}, fmt.Errorf("%w: invitation expiry required", ErrInvalid)
	}
	if invitationCapability != "" {
		if err := ValidateCapability(invitationCapability); err != nil {
			return Link{}, err
		}
	}
	if err := validateBroker(h.opts.Broker); err != nil {
		return Link{}, err
	}
	secret, err := randomBytes(SecretSize)
	if err != nil {
		return Link{}, err
	}
	now := h.opts.Now()
	expires := now.Add(h.opts.PairingTTL)
	if brokerExpiresAt.Before(expires) {
		expires = brokerExpiresAt
	}
	expires = expires.Truncate(time.Second)
	if !now.Before(expires) {
		return Link{}, ErrExpired
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked(now)
	if len(h.invitations) >= h.opts.MaxPending {
		return Link{}, ErrTooManyPending
	}
	key := string(id)
	if _, ok := h.invitations[key]; ok {
		return Link{}, ErrReplay
	}
	h.invitations[key] = &pairInvitation{id: bytes.Clone(id), secret: secret, expires: expires}
	return Link{Broker: h.opts.Broker, HostKey: h.id.PublicKey(), InvitationID: bytes.Clone(id), ExpiresAt: expires, HostLabel: h.opts.HostLabel, PairingSecret: bytes.Clone(secret), InvitationCapability: invitationCapability}, nil
}

func (h *Host) CancelPairing(invitationID []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.burnLocked(string(invitationID))
}
func (h *Host) burnLocked(key string) {
	if s, ok := h.invitations[key]; ok {
		clear(s.secret)
		delete(h.invitations, key)
	}
}
func (h *Host) pruneLocked(now time.Time) {
	for k, s := range h.invitations {
		if !now.Before(s.expires) {
			h.burnLocked(k)
		}
	}
	for k, c := range h.challenges {
		if !now.Before(c.expires) {
			delete(h.challenges, k)
		}
	}
}

// PendingPairing is an authenticated device identity awaiting approval.
type PendingPairing struct {
	InvitationID      []byte
	DeviceKey         ed25519.PublicKey
	DeviceName        string
	DeviceFingerprint string
	SAS               string
	ExpiresAt         time.Time
	req               PairRequest
	requestHash       []byte
}

// VerifyPairRequest checks the MAC before duplicate state. A MAC-authenticated
// duplicate or post-MAC verification failure burns the one-use invitation.
func (h *Host) VerifyPairRequest(req *PairRequest) (*PendingPairing, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	now := h.opts.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	key := string(req.InvitationID)
	s, ok := h.invitations[key]
	if !ok {
		return nil, ErrUnknownInvitation
	}
	if !now.Before(s.expires) {
		h.burnLocked(key)
		return nil, ErrExpired
	}
	t := PairRequestTranscript(req, h.id.PublicKey())
	if !hmac.Equal(TranscriptMAC(s.secret, t), req.MAC) {
		return nil, ErrBadMAC
	}
	if s.state != invitationOpen {
		h.burnLocked(key)
		return nil, ErrReplay
	}
	if err := VerifySignature(req.DeviceKey, t, req.Sig); err != nil {
		h.burnLocked(key)
		return nil, err
	}
	sas, err := ShortAuthenticationString(s.secret, req.InvitationID, h.id.PublicKey(), req.DeviceKey)
	if err != nil {
		h.burnLocked(key)
		return nil, err
	}
	p := &PendingPairing{InvitationID: bytes.Clone(req.InvitationID), DeviceKey: ed25519.PublicKey(bytes.Clone(req.DeviceKey)), DeviceName: req.DeviceName, DeviceFingerprint: KeyFingerprint(req.DeviceKey), SAS: sas, req: *req, requestHash: RequestHash(t)}
	s.state, s.pending = invitationClaimed, p
	if deadline := now.Add(h.opts.ApprovalTTL); deadline.Before(s.expires) {
		s.expires = deadline
	}
	p.ExpiresAt = s.expires
	return p, nil
}
func (h *Host) invitationFor(p *PendingPairing, now time.Time) (*pairInvitation, error) {
	s, ok := h.invitations[string(p.InvitationID)]
	if !ok || s.pending != p {
		return nil, ErrUnknownInvitation
	}
	if !now.Before(s.expires) {
		h.burnLocked(string(p.InvitationID))
		return nil, ErrExpired
	}
	return s, nil
}
func (h *Host) Approve(ctx context.Context, p *PendingPairing, approver Approver) error {
	if approver == nil {
		return fmt.Errorf("%w: no approver", ErrNotApproved)
	}
	h.mu.Lock()
	s, err := h.invitationFor(p, h.opts.Now())
	var expires time.Time
	if err == nil && s.state != invitationClaimed {
		err = ErrReplay
	}
	if s != nil {
		expires = s.expires
	}
	h.mu.Unlock()
	if err != nil {
		return err
	}
	ok, approveErr := approver.ApprovePairing(ctx, PairingPrompt{DeviceName: p.DeviceName, DeviceFingerprint: p.DeviceFingerprint, HostFingerprint: h.id.Fingerprint(), SAS: p.SAS, ExpiresAt: expires})
	h.mu.Lock()
	defer h.mu.Unlock()
	s, err = h.invitationFor(p, h.opts.Now())
	if err != nil {
		return err
	}
	if approveErr != nil || !ok {
		h.burnLocked(string(p.InvitationID))
		if approveErr != nil {
			return fmt.Errorf("approval: %w", approveErr)
		}
		return ErrDenied
	}
	if s.state != invitationClaimed {
		h.burnLocked(string(p.InvitationID))
		return ErrReplay
	}
	s.state = invitationApproved
	return nil
}
func (h *Host) Reject(p *PendingPairing, reason string) *PairReject {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.invitations[string(p.InvitationID)]; ok && s.pending == p {
		h.burnLocked(string(p.InvitationID))
	}
	return &PairReject{V: WireVersion, Type: TypePairReject, InvitationID: p.InvitationID, Reason: reason}
}

// CompletePairing persists a revocation-only web-v1 grant and returns the raw
// reconnect capability exactly once in the authenticated answer.
func (h *Host) CompletePairing(p *PendingPairing) (*PairAnswer, Grant, error) {
	now := h.opts.Now()
	h.mu.Lock()
	s, err := h.invitationFor(p, now)
	if err == nil && s.state != invitationApproved {
		err = ErrNotApproved
	}
	var secret []byte
	if err == nil {
		secret = bytes.Clone(s.secret)
		h.burnLocked(string(p.InvitationID))
	}
	h.mu.Unlock()
	if err != nil {
		return nil, Grant{}, err
	}
	defer clear(secret)
	deviceID, err := randomBytes(DeviceIDSize)
	if err != nil {
		return nil, Grant{}, err
	}
	reconnect, err := randomBytes(SecretSize)
	if err != nil {
		return nil, Grant{}, err
	}
	reconnectHash := sha256.Sum256(reconnect)
	g := Grant{DeviceID: B64(deviceID).String(), DeviceKey: B64(bytes.Clone(p.DeviceKey)), DeviceName: p.DeviceName, Scope: ScopeWebV1, BasePath: h.opts.BasePath, ReconnectCapabilityHash: reconnectHash[:], PairedAt: now.UTC()}
	if err := h.grants.Add(g); err != nil {
		clear(reconnect)
		return nil, Grant{}, fmt.Errorf("persist grant: %w", err)
	}
	a := &PairAnswer{V: WireVersion, Type: TypePairAnswer, InvitationID: p.req.InvitationID, HostKey: B64(h.id.PublicKey()), DeviceKey: p.req.DeviceKey, DeviceNonce: p.req.DeviceNonce, RequestHash: p.requestHash, DeviceID: deviceID, ReconnectCapability: reconnect, Scope: ScopeWebV1, BasePath: h.opts.BasePath, HostLabel: h.opts.HostLabel}
	t := PairAnswerTranscript(a)
	a.MAC = TranscriptMAC(secret, t)
	a.Sig = h.id.Sign(t)
	return a, g, nil
}

package pairing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/subtle"
	"fmt"
	"sync"
	"time"
)

// HostOptions configures a Host. Zero values select the documented defaults.
type HostOptions struct {
	Broker       string        // broker base URL placed in links
	Name         string        // optional host label placed in links
	PairingTTL   time.Duration // link lifetime (default 5m)
	ApprovalTTL  time.Duration // time allowed for terminal approval after a valid request (default 2m)
	ChallengeTTL time.Duration // reconnect challenge lifetime (default 60s)
	GrantTTL     time.Duration // device grant lifetime (default 24h; must not be negative — there is no non-expiring grant)
	MaxPending   int           // max outstanding pairing sessions and challenges each (default 32)
	Now          func() time.Time
}

type sessionState int

const (
	sessionOpen sessionState = iota
	sessionClaimed
	sessionApproved
)

type pairSession struct {
	id, nonce, secret []byte
	expires           time.Time
	hardExpires       time.Time // broker mailbox expiry
	state             sessionState
	pending           *PendingPairing
}

type challenge struct {
	id, nonce []byte
	deviceKey []byte
	grantID   string
	expires   time.Time
}

// Host runs the host side of pairing and reconnect. It is safe for concurrent use.
// Pairing sessions and reconnect challenges live only in memory.
type Host struct {
	id     *Identity
	grants *GrantStore
	opts   HostOptions

	mu         sync.Mutex
	sessions   map[string]*pairSession
	challenges map[string]*challenge
}

// DefaultGrantTTL is the pilot device grant lifetime.
const DefaultGrantTTL = 24 * time.Hour

// NewHost creates a host using a persistent identity and grant store.
func NewHost(id *Identity, grants *GrantStore, opts HostOptions) (*Host, error) {
	if id == nil || grants == nil {
		return nil, fmt.Errorf("pairing: host needs an identity and a grant store")
	}
	if opts.GrantTTL < 0 {
		return nil, fmt.Errorf("pairing: negative GrantTTL: non-expiring grants are not supported")
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
	if opts.GrantTTL == 0 {
		opts.GrantTTL = DefaultGrantTTL
	}
	if opts.MaxPending <= 0 {
		opts.MaxPending = 32
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Host{id: id, grants: grants, opts: opts,
		sessions: map[string]*pairSession{}, challenges: map[string]*challenge{}}, nil
}

// Identity returns the host identity.
func (h *Host) Identity() *Identity { return h.id }

// Grants returns the host grant store.
func (h *Host) Grants() *GrantStore { return h.grants }

// NewPairingForSession creates a one-use pairing session bound to a broker
// session the caller has already created. brokerSessionID is the broker's
// session ID string (unpadded base64url of SessionIDSize random bytes) and is
// placed in the link verbatim; deviceCapability is the broker device
// capability (may be empty) and travels only in the link fragment.
// brokerExpiresAt is required: it caps the link and approval lifetimes and
// is recorded in the grant, because pilot reconnects are only possible through
// this same mailbox while it is live (see Host.NewChallenge).
func (h *Host) NewPairingForSession(brokerSessionID, deviceCapability string, brokerExpiresAt time.Time) (Link, error) {
	sid, err := DecodeB64(brokerSessionID)
	if err != nil {
		return Link{}, fmt.Errorf("%w: broker session ID: %v", ErrInvalid, err)
	}
	if err := wantLen("broker session ID", sid, SessionIDSize); err != nil {
		return Link{}, err
	}
	if brokerExpiresAt.IsZero() {
		return Link{}, fmt.Errorf("%w: broker session expiry is required", ErrInvalid)
	}
	if deviceCapability != "" {
		if err := ValidateCapability(deviceCapability); err != nil {
			return Link{}, err
		}
	}
	return h.newPairing(sid, deviceCapability, brokerExpiresAt)
}

func (h *Host) newPairing(sid []byte, capability string, hardExpires time.Time) (Link, error) {
	if err := validateBroker(h.opts.Broker); err != nil {
		return Link{}, err
	}
	if h.opts.Name != "" {
		if err := ValidateDeviceName(h.opts.Name); err != nil {
			return Link{}, fmt.Errorf("host name: %w", err)
		}
	}
	nonce, err := randomBytes(NonceSize)
	if err != nil {
		return Link{}, err
	}
	secret, err := randomBytes(SecretSize)
	if err != nil {
		return Link{}, err
	}
	now := h.opts.Now()
	exp := now.Add(h.opts.PairingTTL)
	if hardExpires.Before(exp) {
		exp = hardExpires
	}
	exp = exp.Truncate(time.Second)
	if !now.Before(exp) {
		return Link{}, ErrExpired
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked(now)
	if len(h.sessions) >= h.opts.MaxPending {
		return Link{}, ErrTooManyPending
	}
	if _, dup := h.sessions[string(sid)]; dup {
		return Link{}, fmt.Errorf("%w: session ID already in use", ErrReplay)
	}
	h.sessions[string(sid)] = &pairSession{id: bytes.Clone(sid), nonce: nonce, secret: secret, expires: exp, hardExpires: hardExpires}
	return Link{Broker: h.opts.Broker, SessionID: bytes.Clone(sid), HostKey: h.id.PublicKey(),
		HostNonce: bytes.Clone(nonce), Secret: bytes.Clone(secret), ExpiresAt: exp, Name: h.opts.Name,
		Capability: capability}, nil
}

// CancelPairing discards a pairing session.
func (h *Host) CancelPairing(sessionID []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.burnLocked(string(sessionID))
}

func (h *Host) burnLocked(key string) {
	if s, ok := h.sessions[key]; ok {
		clear(s.secret)
		delete(h.sessions, key)
	}
}

func (h *Host) pruneLocked(now time.Time) {
	for k, s := range h.sessions {
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

// PendingPairing is a cryptographically verified pair request awaiting
// explicit host approval.
type PendingPairing struct {
	SessionID         []byte
	DeviceKey         ed25519.PublicKey
	DeviceName        string
	DeviceFingerprint string
	OfferSDP          string
	OfferFingerprint  string // normalized DTLS fingerprint of OfferSDP

	req         PairRequest
	requestHash []byte
}

// VerifyPairRequest authenticates a pair request (session, host nonce, MAC,
// device signature, SDP fingerprint) and claims the session so that no other
// request can use it. It does not approve anything.
func (h *Host) VerifyPairRequest(req *PairRequest) (*PendingPairing, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	fp, err := SDPFingerprint(req.OfferSDP)
	if err != nil {
		return nil, err
	}
	now := h.opts.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	key := string(req.SessionID)
	s, ok := h.sessions[key]
	if !ok {
		return nil, ErrUnknownSession
	}
	if !now.Before(s.expires) {
		h.burnLocked(key)
		return nil, ErrExpired
	}
	if s.state != sessionOpen {
		return nil, ErrReplay
	}
	if subtle.ConstantTimeCompare(s.nonce, req.HostNonce) != 1 {
		return nil, fmt.Errorf("%w: host nonce mismatch", ErrInvalid)
	}
	t := pairRequestTranscript(req, h.id.PublicKey())
	if !hmac.Equal(TranscriptMAC(s.secret, t), req.MAC) {
		return nil, ErrBadMAC
	}
	if err := verifySig(req.DeviceKey, t, req.Sig); err != nil {
		return nil, err
	}
	p := &PendingPairing{
		SessionID: bytes.Clone(req.SessionID), DeviceKey: ed25519.PublicKey(bytes.Clone(req.DeviceKey)),
		DeviceName: req.DeviceName, DeviceFingerprint: KeyFingerprint(req.DeviceKey),
		OfferSDP: req.OfferSDP, OfferFingerprint: fp, req: *req, requestHash: hashTranscript(t),
	}
	s.state = sessionClaimed
	s.pending = p
	if approvalDeadline := now.Add(h.opts.ApprovalTTL); approvalDeadline.After(s.expires) {
		if approvalDeadline.After(s.hardExpires) {
			approvalDeadline = s.hardExpires
		}
		s.expires = approvalDeadline
	}
	return p, nil
}

func (h *Host) sessionFor(p *PendingPairing, now time.Time) (*pairSession, error) {
	key := string(p.SessionID)
	s, ok := h.sessions[key]
	if !ok || s.pending != p {
		return nil, ErrUnknownSession
	}
	if !now.Before(s.expires) {
		h.burnLocked(key)
		return nil, ErrExpired
	}
	return s, nil
}

// Approve asks approver (normally a TerminalApprover) to accept the pending
// pairing. A denial or error burns the session. Approval does not itself send
// anything; call CompletePairing with the WebRTC answer afterwards.
func (h *Host) Approve(ctx context.Context, p *PendingPairing, approver Approver) error {
	if approver == nil {
		return fmt.Errorf("%w: no approver", ErrNotApproved)
	}
	h.mu.Lock()
	s, err := h.sessionFor(p, h.opts.Now())
	if err == nil && s.state != sessionClaimed {
		err = ErrReplay
	}
	var expires time.Time
	if s != nil {
		expires = s.expires
	}
	h.mu.Unlock()
	if err != nil {
		return err
	}
	ok, aerr := approver.ApprovePairing(ctx, PairingPrompt{
		DeviceName: p.DeviceName, DeviceFingerprint: p.DeviceFingerprint,
		HostFingerprint: h.id.Fingerprint(), OfferFingerprint: p.OfferFingerprint, ExpiresAt: expires,
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	s, err = h.sessionFor(p, h.opts.Now())
	if err != nil {
		return err
	}
	if aerr != nil || !ok {
		h.burnLocked(string(p.SessionID))
		if aerr != nil {
			return fmt.Errorf("approval: %w", aerr)
		}
		return ErrDenied
	}
	if s.state != sessionClaimed {
		return ErrReplay
	}
	s.state = sessionApproved
	return nil
}

// Reject burns the pending session and returns an advisory rejection message.
func (h *Host) Reject(p *PendingPairing, reason string) *PairReject {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.sessions[string(p.SessionID)]; ok && s.pending == p {
		h.burnLocked(string(p.SessionID))
	}
	return &PairReject{V: Version, Type: TypePairReject, SessionID: p.SessionID, Reason: reason}
}

// CompletePairing atomically consumes an approved session, persists a device
// grant, and returns the MACed and signed answer. It fails with ErrNotApproved
// unless Approve succeeded for this exact pending request.
func (h *Host) CompletePairing(p *PendingPairing, answerSDP string) (*PairAnswer, Grant, error) {
	if _, err := SDPFingerprint(answerSDP); err != nil {
		return nil, Grant{}, fmt.Errorf("answer: %w", err)
	}
	now := h.opts.Now()
	h.mu.Lock()
	s, err := h.sessionFor(p, now)
	if err == nil && s.state != sessionApproved {
		err = ErrNotApproved
	}
	var secret []byte
	var mailboxExpires time.Time
	if err == nil {
		secret = bytes.Clone(s.secret)
		mailboxExpires = s.hardExpires
		h.burnLocked(string(p.SessionID)) // atomic consume
	}
	h.mu.Unlock()
	if err != nil {
		return nil, Grant{}, err
	}
	defer clear(secret)

	gid, err := randomBytes(GrantIDSize)
	if err != nil {
		return nil, Grant{}, err
	}
	g := Grant{ID: B64(gid).String(), Scope: ScopeHubChat, DeviceKey: B64(p.DeviceKey), DeviceName: p.DeviceName,
		CreatedAt: now.UTC(), ExpiresAt: now.Add(h.opts.GrantTTL).UTC().Truncate(time.Second),
		MailboxID: B64(p.SessionID).String(), MailboxExpiresAt: mailboxExpires.UTC()}
	if err := h.grants.Add(g); err != nil {
		return nil, Grant{}, fmt.Errorf("persist grant: %w", err)
	}
	exp := g.ExpiresAt.Unix()
	a := &PairAnswer{
		V: Version, Type: TypePairAnswer, SessionID: p.req.SessionID, HostKey: B64(h.id.PublicKey()),
		HostNonce: p.req.HostNonce, DeviceKey: p.req.DeviceKey, DeviceNonce: p.req.DeviceNonce,
		RequestHash: p.requestHash, GrantID: gid, GrantExpiresAt: exp, AnswerSDP: answerSDP,
	}
	t := pairAnswerTranscript(a)
	a.MAC = TranscriptMAC(secret, t)
	a.Sig = h.id.sign(t)
	return a, g, nil
}

package pairing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/subtle"
	"fmt"
)

// NewChallenge answers a reconnect hello with a fresh one-use challenge bound
// to the hello's device key and grant. mailboxID is the broker session ID the
// hello arrived on. The grant must be usable, and (pilot v1) mailboxID must be
// the grant's original pairing mailbox and still live; otherwise
// ErrReconnectUnavailable. The challenge never outlives that mailbox.
func (h *Host) NewChallenge(hello *ReconnectHello, mailboxID string) (*ReconnectChallenge, error) {
	if err := checkHeader(hello.V, hello.Type, TypeReconnectHello); err != nil {
		return nil, err
	}
	if err := wantLen("device_key", hello.DeviceKey, ed25519.PublicKeySize); err != nil {
		return nil, err
	}
	if err := wantLen("grant_id", hello.GrantID, GrantIDSize); err != nil {
		return nil, err
	}
	now := h.opts.Now()
	g, err := h.grants.authorize(hello.GrantID.String(), hello.DeviceKey, now, false)
	if err != nil {
		return nil, err
	}
	if g.MailboxID == "" || subtle.ConstantTimeCompare([]byte(g.MailboxID), []byte(mailboxID)) != 1 ||
		!now.Before(g.MailboxExpiresAt) {
		return nil, ErrReconnectUnavailable
	}
	expires := now.Add(h.opts.ChallengeTTL)
	if g.MailboxExpiresAt.Before(expires) {
		expires = g.MailboxExpiresAt
	}
	id, err := randomBytes(ChallengeIDSize)
	if err != nil {
		return nil, err
	}
	nonce, err := randomBytes(NonceSize)
	if err != nil {
		return nil, err
	}
	c := &challenge{id: id, nonce: nonce, deviceKey: bytes.Clone(hello.DeviceKey),
		grantID: hello.GrantID.String(), expires: expires.Truncate(0)}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked(now)
	if len(h.challenges) >= h.opts.MaxPending {
		return nil, ErrTooManyPending
	}
	h.challenges[string(id)] = c
	return &ReconnectChallenge{V: Version, Type: TypeReconnectChallenge, ChallengeID: id,
		HostKey: B64(h.id.PublicKey()), HostNonce: nonce, ExpiresAt: c.expires.Unix()}, nil
}

// VerifiedReconnect is an authenticated reconnect request from a granted device.
type VerifiedReconnect struct {
	Grant            Grant
	DeviceKey        ed25519.PublicKey
	OfferSDP         string
	OfferFingerprint string

	req         ReconnectRequest
	requestHash []byte
}

// VerifyReconnect consumes the referenced challenge (whether or not
// verification succeeds) and authenticates the request against the grant.
func (h *Host) VerifyReconnect(req *ReconnectRequest) (*VerifiedReconnect, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	fp, err := SDPFingerprint(req.OfferSDP)
	if err != nil {
		return nil, err
	}
	now := h.opts.Now()
	h.mu.Lock()
	c, ok := h.challenges[string(req.ChallengeID)]
	delete(h.challenges, string(req.ChallengeID))
	h.mu.Unlock()
	if !ok {
		return nil, ErrUnknownSession
	}
	if !now.Before(c.expires) {
		return nil, ErrExpired
	}
	if subtle.ConstantTimeCompare(c.nonce, req.HostNonce) != 1 ||
		!bytes.Equal(c.deviceKey, req.DeviceKey) || c.grantID != req.GrantID.String() {
		return nil, fmt.Errorf("%w: request does not match challenge", ErrInvalid)
	}
	t := reconnectRequestTranscript(req, h.id.PublicKey())
	if err := verifySig(req.DeviceKey, t, req.Sig); err != nil {
		return nil, err
	}
	g, err := h.grants.authorize(c.grantID, req.DeviceKey, now, true)
	if err != nil {
		return nil, err
	}
	return &VerifiedReconnect{Grant: g, DeviceKey: ed25519.PublicKey(bytes.Clone(req.DeviceKey)),
		OfferSDP: req.OfferSDP, OfferFingerprint: fp, req: *req, requestHash: hashTranscript(t)}, nil
}

// AnswerReconnect re-checks the grant and returns the signed answer.
func (h *Host) AnswerReconnect(v *VerifiedReconnect, answerSDP string) (*ReconnectAnswer, error) {
	if _, err := SDPFingerprint(answerSDP); err != nil {
		return nil, fmt.Errorf("answer: %w", err)
	}
	if _, err := h.grants.authorize(v.Grant.ID, v.DeviceKey, h.opts.Now(), false); err != nil {
		return nil, err
	}
	a := &ReconnectAnswer{
		V: Version, Type: TypeReconnectAnswer, ChallengeID: v.req.ChallengeID, HostKey: B64(h.id.PublicKey()),
		HostNonce: v.req.HostNonce, DeviceKey: v.req.DeviceKey, DeviceNonce: v.req.DeviceNonce,
		RequestHash: v.requestHash, GrantID: v.req.GrantID, AnswerSDP: answerSDP,
	}
	a.Sig = h.id.sign(reconnectAnswerTranscript(a))
	return a, nil
}

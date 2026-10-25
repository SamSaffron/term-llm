package pairing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/subtle"
	"fmt"
)

// NewTransportChallenge authenticates hello and returns a signed one-use
// challenge. Grant validity and revocation are checked from disk each time.
func (h *Host) NewTransportChallenge(hello *TransportHello) (*TransportChallenge, error) {
	if err := checkHeader(hello.V, hello.Type, TypeTransportHello); err != nil {
		return nil, err
	}
	for _, c := range []struct {
		name string
		b    []byte
		n    int
	}{
		{"device_key", hello.DeviceKey, ed25519.PublicKeySize}, {"device_id", hello.DeviceID, DeviceIDSize}, {"reconnect_capability", hello.ReconnectCapability, SecretSize},
		{"device_nonce", hello.DeviceNonce, NonceSize}, {"sig", hello.Sig, ed25519.SignatureSize},
	} {
		if err := wantLen(c.name, c.b, c.n); err != nil {
			return nil, err
		}
	}
	if err := VerifySignature(hello.DeviceKey, TransportHelloTranscript(hello, h.id.PublicKey()), hello.Sig); err != nil {
		return nil, err
	}
	now := h.opts.Now()
	if _, err := h.grants.authorize(hello.DeviceID.String(), hello.DeviceKey, hello.ReconnectCapability, now, false); err != nil {
		return nil, err
	}
	id, err := randomBytes(ChallengeIDSize)
	if err != nil {
		return nil, err
	}
	nonce, err := randomBytes(NonceSize)
	if err != nil {
		return nil, err
	}
	c := &transportChallenge{id: id, nonce: nonce, deviceKey: bytes.Clone(hello.DeviceKey), deviceID: hello.DeviceID.String(), expires: now.Add(h.opts.ChallengeTTL).Truncate(0)}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked(now)
	if len(h.challenges) >= h.opts.MaxPending {
		return nil, ErrTooManyPending
	}
	h.challenges[string(id)] = c
	out := &TransportChallenge{V: WireVersion, Type: TypeTransportChallenge, ChallengeID: bytes.Clone(id), HostKey: B64(h.id.PublicKey()), HostNonce: bytes.Clone(nonce), DeviceKey: bytes.Clone(hello.DeviceKey), DeviceID: bytes.Clone(hello.DeviceID), ExpiresAt: c.expires.Unix()}
	out.Sig = h.id.Sign(TransportChallengeTranscript(out))
	return out, nil
}

// VerifiedTransport is an authenticated, grant-authorized WebRTC offer.
type VerifiedTransport struct {
	Grant            Grant
	DeviceKey        ed25519.PublicKey
	OfferSDP         string
	OfferFingerprint string
	req              TransportRequest
	requestHash      []byte
}

// VerifyTransport burns the referenced challenge on every attempt after its ID
// is recognized, then authenticates the signed offer and live grant.
func (h *Host) VerifyTransport(req *TransportRequest) (*VerifiedTransport, error) {
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
		return nil, ErrUnknownChallenge
	}
	if !now.Before(c.expires) {
		return nil, ErrExpired
	}
	if subtle.ConstantTimeCompare(c.nonce, req.HostNonce) != 1 || !bytes.Equal(c.deviceKey, req.DeviceKey) || subtle.ConstantTimeCompare([]byte(c.deviceID), []byte(req.DeviceID.String())) != 1 {
		return nil, fmt.Errorf("%w: request does not match challenge", ErrInvalid)
	}
	t := TransportRequestTranscript(req, h.id.PublicKey())
	if err := VerifySignature(req.DeviceKey, t, req.Sig); err != nil {
		return nil, err
	}
	g, err := h.grants.authorize(c.deviceID, req.DeviceKey, nil, now, true)
	if err != nil {
		return nil, err
	}
	return &VerifiedTransport{Grant: g, DeviceKey: ed25519.PublicKey(bytes.Clone(req.DeviceKey)), OfferSDP: req.OfferSDP, OfferFingerprint: fp, req: *req, requestHash: RequestHash(t)}, nil
}

// AnswerTransport re-checks the grant immediately before signing the answer.
func (h *Host) AnswerTransport(v *VerifiedTransport, answerSDP string) (*TransportAnswer, error) {
	if _, err := SDPFingerprint(answerSDP); err != nil {
		return nil, fmt.Errorf("answer: %w", err)
	}
	if _, err := h.grants.authorize(v.Grant.DeviceID, v.DeviceKey, nil, h.opts.Now(), false); err != nil {
		return nil, err
	}
	a := &TransportAnswer{V: WireVersion, Type: TypeTransportAnswer, ChallengeID: v.req.ChallengeID, HostKey: B64(h.id.PublicKey()), HostNonce: v.req.HostNonce, DeviceKey: v.req.DeviceKey, DeviceNonce: v.req.DeviceNonce, RequestHash: v.requestHash, DeviceID: v.req.DeviceID, AnswerSDP: answerSDP}
	a.Sig = h.id.Sign(TransportAnswerTranscript(a))
	return a, nil
}

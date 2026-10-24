package pairing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"fmt"
	"time"
)

// PairingAttempt is the device side of one initial pairing.
type PairingAttempt struct {
	link        Link
	req         PairRequest
	requestHash []byte
}

// StartPairing builds the MACed and signed pair request for offerSDP. The
// caller sends Request() to the broker mailbox Link.SessionID and must not
// apply any answer until VerifyAnswer succeeds.
func StartPairing(link Link, device *Identity, deviceName, offerSDP string, now time.Time) (*PairingAttempt, error) {
	if !now.Before(link.ExpiresAt) {
		return nil, ErrExpired
	}
	if err := ValidateDeviceName(deviceName); err != nil {
		return nil, err
	}
	if _, err := SDPFingerprint(offerSDP); err != nil {
		return nil, err
	}
	nonce, err := randomBytes(NonceSize)
	if err != nil {
		return nil, err
	}
	r := PairRequest{
		V: Version, Type: TypePairRequest, SessionID: link.SessionID, HostNonce: link.HostNonce,
		DeviceKey: B64(device.PublicKey()), DeviceNonce: nonce, DeviceName: deviceName, OfferSDP: offerSDP,
	}
	t := pairRequestTranscript(&r, link.HostKey)
	r.MAC = TranscriptMAC(link.Secret, t)
	r.Sig = device.sign(t)
	return &PairingAttempt{link: link, req: r, requestHash: hashTranscript(t)}, nil
}

// Request returns the message to send to the host.
func (a *PairingAttempt) Request() *PairRequest { r := a.req; return &r }

// DevicePairing is the verified pairing result to persist on the device.
type DevicePairing struct {
	HostKey        ed25519.PublicKey
	GrantID        []byte
	GrantExpiresAt time.Time
	// BrokerSessionID is the pairing mailbox; in pilot v1 reconnect is only
	// possible through it, until the broker session expires.
	BrokerSessionID   string
	AnswerSDP         string
	AnswerFingerprint string
}

// VerifyAnswer authenticates the host answer (pinned host key, echoed values,
// request hash, MAC, host signature, answer SDP fingerprint).
func (a *PairingAttempt) VerifyAnswer(ans *PairAnswer) (*DevicePairing, error) {
	if err := checkHeader(ans.V, ans.Type, TypePairAnswer); err != nil {
		return nil, err
	}
	if !bytes.Equal(ans.HostKey, a.link.HostKey) {
		return nil, ErrHostKeyMismatch
	}
	if !bytes.Equal(ans.SessionID, a.req.SessionID) || !bytes.Equal(ans.HostNonce, a.req.HostNonce) ||
		!bytes.Equal(ans.DeviceKey, a.req.DeviceKey) || !bytes.Equal(ans.DeviceNonce, a.req.DeviceNonce) ||
		!bytes.Equal(ans.RequestHash, a.requestHash) {
		return nil, fmt.Errorf("%w: answer does not match request", ErrInvalid)
	}
	if err := wantLen("grant_id", ans.GrantID, GrantIDSize); err != nil {
		return nil, err
	}
	if ans.GrantExpiresAt <= 0 {
		return nil, fmt.Errorf("%w: grant must expire", ErrInvalid)
	}
	t := pairAnswerTranscript(ans)
	if !hmac.Equal(TranscriptMAC(a.link.Secret, t), ans.MAC) {
		return nil, ErrBadMAC
	}
	if err := verifySig(ans.HostKey, t, ans.Sig); err != nil {
		return nil, err
	}
	fp, err := SDPFingerprint(ans.AnswerSDP)
	if err != nil {
		return nil, err
	}
	out := &DevicePairing{HostKey: bytes.Clone(a.link.HostKey), GrantID: bytes.Clone(ans.GrantID),
		AnswerSDP: ans.AnswerSDP, AnswerFingerprint: fp, GrantExpiresAt: time.Unix(ans.GrantExpiresAt, 0),
		BrokerSessionID: B64(a.link.SessionID).String()}
	return out, nil
}

// NewReconnectHello builds the hello that asks a host for a challenge.
func NewReconnectHello(device *Identity, grantID []byte) *ReconnectHello {
	return &ReconnectHello{V: Version, Type: TypeReconnectHello, DeviceKey: B64(device.PublicKey()), GrantID: grantID}
}

// ReconnectAttempt is the device side of one reconnect.
type ReconnectAttempt struct {
	hostKey     ed25519.PublicKey
	req         ReconnectRequest
	requestHash []byte
}

// StartReconnect signs a reconnect request answering challenge ch from the
// host pinned at pairing time.
func StartReconnect(ch *ReconnectChallenge, pinnedHost ed25519.PublicKey, device *Identity, grantID []byte, offerSDP string, now time.Time) (*ReconnectAttempt, error) {
	if err := checkHeader(ch.V, ch.Type, TypeReconnectChallenge); err != nil {
		return nil, err
	}
	if !bytes.Equal(ch.HostKey, pinnedHost) {
		return nil, ErrHostKeyMismatch
	}
	if err := wantLen("challenge_id", ch.ChallengeID, ChallengeIDSize); err != nil {
		return nil, err
	}
	if err := wantLen("hn", ch.HostNonce, NonceSize); err != nil {
		return nil, err
	}
	if now.Unix() >= ch.ExpiresAt {
		return nil, ErrExpired
	}
	if _, err := SDPFingerprint(offerSDP); err != nil {
		return nil, err
	}
	nonce, err := randomBytes(NonceSize)
	if err != nil {
		return nil, err
	}
	r := ReconnectRequest{V: Version, Type: TypeReconnectRequest, ChallengeID: ch.ChallengeID, HostNonce: ch.HostNonce,
		DeviceKey: B64(device.PublicKey()), DeviceNonce: nonce, GrantID: grantID, OfferSDP: offerSDP}
	t := reconnectRequestTranscript(&r, pinnedHost)
	r.Sig = device.sign(t)
	return &ReconnectAttempt{hostKey: bytes.Clone(pinnedHost), req: r, requestHash: hashTranscript(t)}, nil
}

// Request returns the message to send to the host.
func (a *ReconnectAttempt) Request() *ReconnectRequest { r := a.req; return &r }

// VerifyAnswer authenticates the host's reconnect answer and returns the
// answer SDP that may then be applied.
func (a *ReconnectAttempt) VerifyAnswer(ans *ReconnectAnswer) (string, error) {
	if err := checkHeader(ans.V, ans.Type, TypeReconnectAnswer); err != nil {
		return "", err
	}
	if !bytes.Equal(ans.HostKey, a.hostKey) {
		return "", ErrHostKeyMismatch
	}
	if !bytes.Equal(ans.ChallengeID, a.req.ChallengeID) || !bytes.Equal(ans.HostNonce, a.req.HostNonce) ||
		!bytes.Equal(ans.DeviceKey, a.req.DeviceKey) || !bytes.Equal(ans.DeviceNonce, a.req.DeviceNonce) ||
		!bytes.Equal(ans.GrantID, a.req.GrantID) || !bytes.Equal(ans.RequestHash, a.requestHash) {
		return "", fmt.Errorf("%w: answer does not match request", ErrInvalid)
	}
	if err := verifySig(a.hostKey, reconnectAnswerTranscript(ans), ans.Sig); err != nil {
		return "", err
	}
	if _, err := SDPFingerprint(ans.AnswerSDP); err != nil {
		return "", err
	}
	return ans.AnswerSDP, nil
}

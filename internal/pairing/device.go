package pairing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"fmt"
	"time"
)

type PairingAttempt struct {
	link        Link
	req         PairRequest
	requestHash []byte
	sas         string
}

// StartPairing builds an authenticated identity-only PairRequest.
func StartPairing(link Link, device *Identity, deviceName string, now time.Time) (*PairingAttempt, error) {
	if device == nil {
		return nil, fmt.Errorf("%w: device identity required", ErrInvalid)
	}
	if !now.Before(link.ExpiresAt) {
		return nil, ErrExpired
	}
	if err := ValidateDeviceName(deviceName); err != nil {
		return nil, err
	}
	if err := wantLen("invitation id", link.InvitationID, InvitationIDSize); err != nil {
		return nil, err
	}
	if err := wantLen("host key", link.HostKey, ed25519.PublicKeySize); err != nil {
		return nil, err
	}
	if err := wantLen("pairing secret", link.PairingSecret, SecretSize); err != nil {
		return nil, err
	}
	nonce, err := randomBytes(NonceSize)
	if err != nil {
		return nil, err
	}
	r := PairRequest{V: WireVersion, Type: TypePairRequest, InvitationID: bytes.Clone(link.InvitationID), DeviceKey: B64(device.PublicKey()), DeviceNonce: nonce, DeviceName: deviceName}
	t := PairRequestTranscript(&r, link.HostKey)
	r.MAC, r.Sig = TranscriptMAC(link.PairingSecret, t), device.Sign(t)
	sas, err := ShortAuthenticationString(link.PairingSecret, link.InvitationID, link.HostKey, r.DeviceKey)
	if err != nil {
		return nil, err
	}
	return &PairingAttempt{link: link, req: r, requestHash: RequestHash(t), sas: sas}, nil
}
func (a *PairingAttempt) Request() *PairRequest { r := a.req; return &r }
func (a *PairingAttempt) SAS() string           { return a.sas }

// DevicePairing is the durable client-side result. ReconnectCapability must be
// stored as secret bearer material; unlike the host, the client needs the raw value.
type DevicePairing struct {
	HostKey             ed25519.PublicKey
	DeviceID            []byte
	ReconnectCapability []byte
	Scope               string
	BasePath            string
	HostLabel           string
}

func (a *PairingAttempt) VerifyAnswer(ans *PairAnswer) (*DevicePairing, error) {
	if err := checkHeader(ans.V, ans.Type, TypePairAnswer); err != nil {
		return nil, err
	}
	if !bytes.Equal(ans.HostKey, a.link.HostKey) {
		return nil, ErrHostKeyMismatch
	}
	if !bytes.Equal(ans.InvitationID, a.req.InvitationID) || !bytes.Equal(ans.DeviceKey, a.req.DeviceKey) || !bytes.Equal(ans.DeviceNonce, a.req.DeviceNonce) || !bytes.Equal(ans.RequestHash, a.requestHash) {
		return nil, fmt.Errorf("%w: answer does not match request", ErrInvalid)
	}
	if err := wantLen("device_id", ans.DeviceID, DeviceIDSize); err != nil {
		return nil, err
	}
	if err := wantLen("reconnect_capability", ans.ReconnectCapability, SecretSize); err != nil {
		return nil, err
	}
	if ans.Scope != ScopeWebV1 || !validBasePath(ans.BasePath) {
		return nil, fmt.Errorf("%w: unsupported scope", ErrInvalid)
	}
	if ans.HostLabel != "" {
		if err := ValidateDeviceName(ans.HostLabel); err != nil {
			return nil, err
		}
	}
	if err := wantLen("mac", ans.MAC, MACSize); err != nil {
		return nil, err
	}
	if err := wantLen("sig", ans.Sig, ed25519.SignatureSize); err != nil {
		return nil, err
	}
	t := PairAnswerTranscript(ans)
	if !hmac.Equal(TranscriptMAC(a.link.PairingSecret, t), ans.MAC) {
		return nil, ErrBadMAC
	}
	if err := VerifySignature(ans.HostKey, t, ans.Sig); err != nil {
		return nil, err
	}
	return &DevicePairing{HostKey: ed25519.PublicKey(bytes.Clone(a.link.HostKey)), DeviceID: bytes.Clone(ans.DeviceID), ReconnectCapability: bytes.Clone(ans.ReconnectCapability), Scope: ans.Scope, BasePath: ans.BasePath, HostLabel: ans.HostLabel}, nil
}

// NewTransportHello builds a signed challenge request for a paired device.
func NewTransportHello(device *Identity, pinnedHost ed25519.PublicKey, deviceID, reconnectCapability []byte) (*TransportHello, error) {
	if device == nil {
		return nil, fmt.Errorf("%w: device identity required", ErrInvalid)
	}
	if err := wantLen("host key", pinnedHost, ed25519.PublicKeySize); err != nil {
		return nil, err
	}
	if err := wantLen("device_id", deviceID, DeviceIDSize); err != nil {
		return nil, err
	}
	if err := wantLen("reconnect_capability", reconnectCapability, SecretSize); err != nil {
		return nil, err
	}
	nonce, err := randomBytes(NonceSize)
	if err != nil {
		return nil, err
	}
	h := &TransportHello{V: WireVersion, Type: TypeTransportHello, DeviceKey: B64(device.PublicKey()), DeviceID: bytes.Clone(deviceID), ReconnectCapability: bytes.Clone(reconnectCapability), DeviceNonce: nonce}
	h.Sig = device.Sign(TransportHelloTranscript(h, pinnedHost))
	return h, nil
}

type TransportAttempt struct {
	hostKey     ed25519.PublicKey
	req         TransportRequest
	requestHash []byte
}

func StartTransport(ch *TransportChallenge, pinnedHost ed25519.PublicKey, device *Identity, deviceID []byte, offerSDP string, now time.Time) (*TransportAttempt, error) {
	if err := checkHeader(ch.V, ch.Type, TypeTransportChallenge); err != nil {
		return nil, err
	}
	if !bytes.Equal(ch.HostKey, pinnedHost) {
		return nil, ErrHostKeyMismatch
	}
	if now.Unix() >= ch.ExpiresAt {
		return nil, ErrExpired
	}
	if !bytes.Equal(ch.DeviceKey, device.PublicKey()) || !bytes.Equal(ch.DeviceID, deviceID) {
		return nil, fmt.Errorf("%w: challenge identity", ErrInvalid)
	}
	if err := VerifySignature(pinnedHost, TransportChallengeTranscript(ch), ch.Sig); err != nil {
		return nil, err
	}
	if _, err := SDPFingerprint(offerSDP); err != nil {
		return nil, err
	}
	nonce, err := randomBytes(NonceSize)
	if err != nil {
		return nil, err
	}
	r := TransportRequest{V: WireVersion, Type: TypeTransportRequest, ChallengeID: bytes.Clone(ch.ChallengeID), HostNonce: bytes.Clone(ch.HostNonce), DeviceKey: B64(device.PublicKey()), DeviceNonce: nonce, DeviceID: bytes.Clone(deviceID), OfferSDP: offerSDP}
	t := TransportRequestTranscript(&r, pinnedHost)
	r.Sig = device.Sign(t)
	return &TransportAttempt{hostKey: ed25519.PublicKey(bytes.Clone(pinnedHost)), req: r, requestHash: RequestHash(t)}, nil
}
func (a *TransportAttempt) Request() *TransportRequest { r := a.req; return &r }
func (a *TransportAttempt) VerifyAnswer(ans *TransportAnswer) (string, error) {
	if err := checkHeader(ans.V, ans.Type, TypeTransportAnswer); err != nil {
		return "", err
	}
	if !bytes.Equal(ans.HostKey, a.hostKey) {
		return "", ErrHostKeyMismatch
	}
	if !bytes.Equal(ans.ChallengeID, a.req.ChallengeID) || !bytes.Equal(ans.HostNonce, a.req.HostNonce) || !bytes.Equal(ans.DeviceKey, a.req.DeviceKey) || !bytes.Equal(ans.DeviceNonce, a.req.DeviceNonce) || !bytes.Equal(ans.DeviceID, a.req.DeviceID) || !bytes.Equal(ans.RequestHash, a.requestHash) {
		return "", fmt.Errorf("%w: answer does not match request", ErrInvalid)
	}
	if err := VerifySignature(a.hostKey, TransportAnswerTranscript(ans), ans.Sig); err != nil {
		return "", err
	}
	if _, err := SDPFingerprint(ans.AnswerSDP); err != nil {
		return "", err
	}
	return ans.AnswerSDP, nil
}

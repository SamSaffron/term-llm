package pairing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode"
	"unicode/utf8"
)

// WireVersion is the frozen pairing and transport wire format version.
// It is deliberately independent of StateVersion, the on-disk schema version.
const WireVersion = 2

// Version is retained as the wire-version spelling used in message literals.
const Version = WireVersion

// StateVersion is the current private identity and grant-store schema version.
const StateVersion = 1

const (
	InvitationIDSize = 16
	ChallengeIDSize  = 16
	DeviceIDSize     = 16
	NonceSize        = 32
	SecretSize       = 32
	MACSize          = sha256.Size

	MaxDeviceNameBytes = 64
	MaxSDPBytes        = 64 << 10
	MaxMessageBytes    = 256 << 10

	transcriptDomain = "term-llm/pairing/v2"
	macKeyLabel      = "term-llm/pairing/v2/mac-key"

	labelPairRequest        = "pair-request"
	labelPairAnswer         = "pair-answer"
	labelPairSAS            = "pair-sas"
	labelTransportHello     = "transport-hello"
	labelTransportChallenge = "transport-challenge"
	labelTransportRequest   = "transport-request"
	labelTransportAnswer    = "transport-answer"

	roleDevice = "device"
	roleHost   = "host"
)

// Message type discriminators carried in the "type" JSON field.
const (
	TypePairRequest        = "pair_request"
	TypePairAnswer         = "pair_answer"
	TypePairReject         = "pair_reject"
	TypeTransportHello     = "transport_hello"
	TypeTransportChallenge = "transport_challenge"
	TypeTransportRequest   = "transport_request"
	TypeTransportAnswer    = "transport_answer"
)

var (
	ErrInvalid           = errors.New("pairing: invalid message")
	ErrUnknownInvitation = errors.New("pairing: unknown invitation")
	ErrUnknownChallenge  = errors.New("pairing: unknown transport challenge")
	ErrExpired           = errors.New("pairing: expired")
	ErrReplay            = errors.New("pairing: already used")
	ErrBadMAC            = errors.New("pairing: MAC verification failed")
	ErrBadSignature      = errors.New("pairing: signature verification failed")
	ErrNotApproved       = errors.New("pairing: not approved")
	ErrDenied            = errors.New("pairing: denied by host")
	ErrGrantNotFound     = errors.New("pairing: grant not found")
	ErrRevoked           = errors.New("pairing: grant revoked")
	ErrTooManyPending    = errors.New("pairing: too many pending attempts")
	ErrHostKeyMismatch   = errors.New("pairing: host key mismatch")
)

// B64 is a byte slice encoded in JSON as canonical, unpadded base64url.
type B64 []byte

var b64 = base64.RawURLEncoding.Strict()

func (b B64) String() string               { return b64.EncodeToString(b) }
func (b B64) MarshalJSON() ([]byte, error) { return json.Marshal(b.String()) }
func (b *B64) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v, err := DecodeB64(s)
	if err != nil {
		return err
	}
	*b = v
	return nil
}

// DecodeB64 decodes canonical, unpadded base64url.
func DecodeB64(s string) ([]byte, error) {
	v, err := b64.DecodeString(s)
	if err != nil || b64.EncodeToString(v) != s {
		return nil, fmt.Errorf("base64url: non-canonical encoding")
	}
	return v, nil
}

// PairRequest introduces only a device identity. Media and SDP are forbidden
// in the pairing exchange and belong to the later signed transport handshake.
type PairRequest struct {
	V            int    `json:"v"`
	Type         string `json:"type"`
	InvitationID B64    `json:"invitation_id"`
	DeviceKey    B64    `json:"device_key"`
	DeviceNonce  B64    `json:"device_nonce"`
	DeviceName   string `json:"device_name"`
	MAC          B64    `json:"mac"`
	Sig          B64    `json:"sig"`
}

// PairAnswer grants the introduced device identity durable, revocable access.
type PairAnswer struct {
	V                   int    `json:"v"`
	Type                string `json:"type"`
	InvitationID        B64    `json:"invitation_id"`
	HostKey             B64    `json:"host_key"`
	DeviceKey           B64    `json:"device_key"`
	DeviceNonce         B64    `json:"device_nonce"`
	RequestHash         B64    `json:"request_hash"`
	DeviceID            B64    `json:"device_id"`
	ReconnectCapability B64    `json:"reconnect_capability"`
	Scope               string `json:"scope"`
	BasePath            string `json:"base_path"`
	HostLabel           string `json:"host_label"`
	MAC                 B64    `json:"mac"`
	Sig                 B64    `json:"sig"`
}

type PairReject struct {
	V            int    `json:"v"`
	Type         string `json:"type"`
	InvitationID B64    `json:"invitation_id"`
	Reason       string `json:"reason"`
}

// TransportHello asks for a fresh one-use transport challenge. It is signed so
// signalling implementations can reject unauthenticated traffic immediately.
type TransportHello struct {
	V                   int    `json:"v"`
	Type                string `json:"type"`
	DeviceKey           B64    `json:"device_key"`
	DeviceID            B64    `json:"device_id"`
	ReconnectCapability B64    `json:"reconnect_capability"`
	DeviceNonce         B64    `json:"device_nonce"`
	Sig                 B64    `json:"sig"`
}

// TransportChallenge is the host's signed, short-lived one-use challenge.
type TransportChallenge struct {
	V           int    `json:"v"`
	Type        string `json:"type"`
	ChallengeID B64    `json:"challenge_id"`
	HostKey     B64    `json:"host_key"`
	HostNonce   B64    `json:"host_nonce"`
	DeviceKey   B64    `json:"device_key"`
	DeviceID    B64    `json:"device_id"`
	ExpiresAt   int64  `json:"expires_at"`
	Sig         B64    `json:"sig"`
}

// TransportRequest carries the device's signed WebRTC offer.
type TransportRequest struct {
	V           int    `json:"v"`
	Type        string `json:"type"`
	ChallengeID B64    `json:"challenge_id"`
	HostNonce   B64    `json:"host_nonce"`
	DeviceKey   B64    `json:"device_key"`
	DeviceNonce B64    `json:"device_nonce"`
	DeviceID    B64    `json:"device_id"`
	OfferSDP    string `json:"offer_sdp"`
	Sig         B64    `json:"sig"`
}

// TransportAnswer carries the host's signed WebRTC answer.
type TransportAnswer struct {
	V           int    `json:"v"`
	Type        string `json:"type"`
	ChallengeID B64    `json:"challenge_id"`
	HostKey     B64    `json:"host_key"`
	HostNonce   B64    `json:"host_nonce"`
	DeviceKey   B64    `json:"device_key"`
	DeviceNonce B64    `json:"device_nonce"`
	RequestHash B64    `json:"request_hash"`
	DeviceID    B64    `json:"device_id"`
	AnswerSDP   string `json:"answer_sdp"`
	Sig         B64    `json:"sig"`
}

// DecodeMessage strictly parses one v2 wire message.
func DecodeMessage(data []byte) (any, error) {
	if len(data) > MaxMessageBytes {
		return nil, fmt.Errorf("%w: message too large", ErrInvalid)
	}
	var head struct {
		V    int    `json:"v"`
		Type string `json:"type"`
	}
	if err := decodeJSON(data, &head, false); err != nil {
		return nil, err
	}
	if head.V != WireVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrInvalid, head.V)
	}
	var msg any
	switch head.Type {
	case TypePairRequest:
		msg = &PairRequest{}
	case TypePairAnswer:
		msg = &PairAnswer{}
	case TypePairReject:
		msg = &PairReject{}
	case TypeTransportHello:
		msg = &TransportHello{}
	case TypeTransportChallenge:
		msg = &TransportChallenge{}
	case TypeTransportRequest:
		msg = &TransportRequest{}
	case TypeTransportAnswer:
		msg = &TransportAnswer{}
	default:
		return nil, fmt.Errorf("%w: unknown type %q", ErrInvalid, head.Type)
	}
	if err := decodeJSON(data, msg, true); err != nil {
		return nil, err
	}
	return msg, nil
}

func decodeJSON(data []byte, dst any, strict bool) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("%w: trailing JSON", ErrInvalid)
	}
	return nil
}

// Transcript returns canonical transcript bytes: each domain, label, and field
// is prefixed with a four-byte unsigned big-endian byte length.
func Transcript(label string, fields ...[]byte) []byte {
	var buf bytes.Buffer
	writeField(&buf, []byte(transcriptDomain))
	writeField(&buf, []byte(label))
	for _, f := range fields {
		writeField(&buf, f)
	}
	return buf.Bytes()
}

func writeField(buf *bytes.Buffer, f []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(f)))
	buf.Write(n[:])
	buf.Write(f)
}

func intField(v int64) []byte { return []byte(strconv.FormatInt(v, 10)) }

// PairRequestTranscript returns the bytes MACed and signed for req.
func PairRequestTranscript(req *PairRequest, hostKey []byte) []byte {
	return Transcript(labelPairRequest, []byte(roleDevice), req.InvitationID, hostKey, req.DeviceKey, req.DeviceNonce, []byte(req.DeviceName))
}

// PairAnswerTranscript returns the bytes MACed and signed for answer.
func PairAnswerTranscript(a *PairAnswer) []byte {
	return Transcript(labelPairAnswer, []byte(roleHost), a.InvitationID, a.HostKey, a.DeviceKey, a.DeviceNonce, a.RequestHash, a.DeviceID, a.ReconnectCapability, []byte(a.Scope), []byte(a.BasePath), []byte(a.HostLabel))
}

func TransportHelloTranscript(h *TransportHello, hostKey []byte) []byte {
	return Transcript(labelTransportHello, []byte(roleDevice), hostKey, h.DeviceKey, h.DeviceID, h.ReconnectCapability, h.DeviceNonce)
}
func TransportChallengeTranscript(c *TransportChallenge) []byte {
	return Transcript(labelTransportChallenge, []byte(roleHost), c.ChallengeID, c.HostKey, c.HostNonce,
		c.DeviceKey, c.DeviceID, intField(c.ExpiresAt))
}
func TransportRequestTranscript(r *TransportRequest, hostKey []byte) []byte {
	return Transcript(labelTransportRequest, []byte(roleDevice), r.ChallengeID, hostKey, r.HostNonce,
		r.DeviceKey, r.DeviceNonce, r.DeviceID, []byte(r.OfferSDP))
}
func TransportAnswerTranscript(a *TransportAnswer) []byte {
	return Transcript(labelTransportAnswer, []byte(roleHost), a.ChallengeID, a.HostKey, a.HostNonce,
		a.DeviceKey, a.DeviceNonce, a.RequestHash, a.DeviceID, []byte(a.AnswerSDP))
}

// MACKey derives the v2 pairing MAC key from a pairing secret.
func MACKey(secret []byte) []byte {
	k := hmac.New(sha256.New, secret)
	_, _ = k.Write([]byte(macKeyLabel))
	return k.Sum(nil)
}

// TranscriptMAC computes the pairing MAC over transcript.
func TranscriptMAC(secret, transcript []byte) []byte {
	m := hmac.New(sha256.New, MACKey(secret))
	_, _ = m.Write(transcript)
	return m.Sum(nil)
}

// RequestHash returns SHA-256(transcript).
func RequestHash(transcript []byte) []byte {
	h := sha256.Sum256(transcript)
	return h[:]
}

// ShortAuthenticationString returns the canonical six decimal digit SAS for
// an initial pairing. Both sides display this only after authenticating the
// PairRequest. Its inputs are raw wire values, not their base64 encodings.
func ShortAuthenticationString(secret, invitationID, hostKey, deviceKey []byte) (string, error) {
	if len(secret) != SecretSize || len(invitationID) != InvitationIDSize || len(hostKey) != ed25519.PublicKeySize || len(deviceKey) != ed25519.PublicKeySize {
		return "", fmt.Errorf("%w: invalid SAS input length", ErrInvalid)
	}
	m := hmac.New(sha256.New, MACKey(secret))
	_, _ = m.Write(Transcript(labelPairSAS, invitationID, hostKey, deviceKey))
	sum := m.Sum(nil)
	n := binary.BigEndian.Uint32(sum[:4]) % 1_000_000
	return fmt.Sprintf("%06d", n), nil
}

func wantLen(name string, b []byte, n int) error {
	if len(b) != n {
		return fmt.Errorf("%w: %s must be %d bytes, got %d", ErrInvalid, name, n, len(b))
	}
	return nil
}
func checkHeader(v int, typ, want string) error {
	if v != WireVersion || typ != want {
		return fmt.Errorf("%w: unexpected v/type %d/%q", ErrInvalid, v, typ)
	}
	return nil
}

// ValidateDeviceName validates a terminal-safe UTF-8 label.
func ValidateDeviceName(name string) error {
	if name == "" || len(name) > MaxDeviceNameBytes || !utf8.ValidString(name) {
		return fmt.Errorf("%w: device name must be 1-%d bytes of UTF-8", ErrInvalid, MaxDeviceNameBytes)
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%w: device name contains control characters", ErrInvalid)
		}
	}
	return nil
}

func (r *PairRequest) validate() error {
	if err := checkHeader(r.V, r.Type, TypePairRequest); err != nil {
		return err
	}
	for _, c := range []struct {
		name string
		b    []byte
		n    int
	}{
		{"invitation_id", r.InvitationID, InvitationIDSize},
		{"device_key", r.DeviceKey, ed25519.PublicKeySize}, {"device_nonce", r.DeviceNonce, NonceSize},
		{"mac", r.MAC, MACSize}, {"sig", r.Sig, ed25519.SignatureSize},
	} {
		if err := wantLen(c.name, c.b, c.n); err != nil {
			return err
		}
	}
	return ValidateDeviceName(r.DeviceName)
}

func (r *TransportRequest) validate() error {
	if err := checkHeader(r.V, r.Type, TypeTransportRequest); err != nil {
		return err
	}
	for _, c := range []struct {
		name string
		b    []byte
		n    int
	}{
		{"challenge_id", r.ChallengeID, ChallengeIDSize}, {"hn", r.HostNonce, NonceSize},
		{"device_key", r.DeviceKey, ed25519.PublicKeySize}, {"device_nonce", r.DeviceNonce, NonceSize},
		{"device_id", r.DeviceID, DeviceIDSize}, {"sig", r.Sig, ed25519.SignatureSize},
	} {
		if err := wantLen(c.name, c.b, c.n); err != nil {
			return err
		}
	}
	if len(r.OfferSDP) == 0 || len(r.OfferSDP) > MaxSDPBytes {
		return fmt.Errorf("%w: offer SDP size", ErrInvalid)
	}
	return nil
}

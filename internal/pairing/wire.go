package pairing

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode"
	"unicode/utf8"
)

// Wire constants for format v1. See WIRE.md.
const (
	Version = 1

	SessionIDSize   = 16
	ChallengeIDSize = 16
	GrantIDSize     = 16
	NonceSize       = 32
	SecretSize      = 32
	MACSize         = sha256.Size

	MaxDeviceNameBytes = 64
	MaxSDPBytes        = 64 << 10
	MaxMessageBytes    = 256 << 10

	transcriptDomain = "term-llm/pairing/v1"
	macKeyLabel      = "term-llm/pairing/v1/mac-key"

	labelPairRequest      = "pair-request"
	labelPairAnswer       = "pair-answer"
	labelReconnectRequest = "reconnect-request"
	labelReconnectAnswer  = "reconnect-answer"

	roleDevice = "device"
	roleHost   = "host"
)

// Message type discriminators carried in the "type" JSON field.
const (
	TypePairRequest        = "pair_request"
	TypePairAnswer         = "pair_answer"
	TypePairReject         = "pair_reject"
	TypeReconnectHello     = "reconnect_hello"
	TypeReconnectChallenge = "reconnect_challenge"
	TypeReconnectRequest   = "reconnect_request"
	TypeReconnectAnswer    = "reconnect_answer"
)

// Errors returned by verification. Callers should treat all of them as a
// failed attempt; wrapped detail is for logs only.
var (
	ErrInvalid         = errors.New("pairing: invalid message")
	ErrUnknownSession  = errors.New("pairing: unknown session or challenge")
	ErrExpired         = errors.New("pairing: expired")
	ErrReplay          = errors.New("pairing: already used")
	ErrBadMAC          = errors.New("pairing: MAC verification failed")
	ErrBadSignature    = errors.New("pairing: signature verification failed")
	ErrNotApproved     = errors.New("pairing: not approved")
	ErrDenied          = errors.New("pairing: denied by host")
	ErrGrantNotFound   = errors.New("pairing: grant not found")
	ErrRevoked         = errors.New("pairing: grant revoked")
	ErrTooManyPending  = errors.New("pairing: too many pending attempts")
	ErrHostKeyMismatch = errors.New("pairing: host key mismatch")
	// ErrReconnectUnavailable means the grant may still be valid but pilot v1
	// has no way to rendezvous: the original broker mailbox expired or the
	// hello arrived through a different mailbox. The device must pair again.
	ErrReconnectUnavailable = errors.New("pairing: reconnect unavailable: original broker mailbox not live")
)

// B64 is a byte slice encoded in JSON as unpadded base64url.
type B64 []byte

var b64 = base64.RawURLEncoding.Strict()

// String returns the unpadded base64url encoding.
func (b B64) String() string { return b64.EncodeToString(b) }

// MarshalJSON implements json.Marshaler.
func (b B64) MarshalJSON() ([]byte, error) { return json.Marshal(b.String()) }

// UnmarshalJSON implements json.Unmarshaler, rejecting padded or non-url input.
func (b *B64) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v, err := b64.DecodeString(s)
	if err != nil {
		return fmt.Errorf("base64url: %w", err)
	}
	*b = v
	return nil
}

// DecodeB64 decodes an unpadded base64url string.
func DecodeB64(s string) ([]byte, error) { return b64.DecodeString(s) }

// PairRequest is sent by the device to the host through the broker mailbox.
type PairRequest struct {
	V           int    `json:"v"`
	Type        string `json:"type"`
	SessionID   B64    `json:"sid"`
	HostNonce   B64    `json:"hn"`
	DeviceKey   B64    `json:"device_key"`
	DeviceNonce B64    `json:"device_nonce"`
	DeviceName  string `json:"device_name"`
	OfferSDP    string `json:"offer_sdp"`
	MAC         B64    `json:"mac"`
	Sig         B64    `json:"sig"`
}

// PairAnswer is sent by the host after explicit approval.
type PairAnswer struct {
	V              int    `json:"v"`
	Type           string `json:"type"`
	SessionID      B64    `json:"sid"`
	HostKey        B64    `json:"hk"`
	HostNonce      B64    `json:"hn"`
	DeviceKey      B64    `json:"device_key"`
	DeviceNonce    B64    `json:"device_nonce"`
	RequestHash    B64    `json:"request_hash"`
	GrantID        B64    `json:"grant_id"`
	GrantExpiresAt int64  `json:"grant_expires_at"`
	AnswerSDP      string `json:"answer_sdp"`
	MAC            B64    `json:"mac"`
	Sig            B64    `json:"sig"`
}

// PairReject is an advisory, unauthenticated rejection.
type PairReject struct {
	V         int    `json:"v"`
	Type      string `json:"type"`
	SessionID B64    `json:"sid"`
	Reason    string `json:"reason"`
}

// ReconnectHello asks the host for a fresh reconnect challenge.
type ReconnectHello struct {
	V         int    `json:"v"`
	Type      string `json:"type"`
	DeviceKey B64    `json:"device_key"`
	GrantID   B64    `json:"grant_id"`
}

// ReconnectChallenge is a one-use host challenge.
type ReconnectChallenge struct {
	V           int    `json:"v"`
	Type        string `json:"type"`
	ChallengeID B64    `json:"challenge_id"`
	HostKey     B64    `json:"hk"`
	HostNonce   B64    `json:"hn"`
	ExpiresAt   int64  `json:"expires_at"`
}

// ReconnectRequest is the device's signed reconnect offer.
type ReconnectRequest struct {
	V           int    `json:"v"`
	Type        string `json:"type"`
	ChallengeID B64    `json:"challenge_id"`
	HostNonce   B64    `json:"hn"`
	DeviceKey   B64    `json:"device_key"`
	DeviceNonce B64    `json:"device_nonce"`
	GrantID     B64    `json:"grant_id"`
	OfferSDP    string `json:"offer_sdp"`
	Sig         B64    `json:"sig"`
}

// ReconnectAnswer is the host's signed reconnect answer.
type ReconnectAnswer struct {
	V           int    `json:"v"`
	Type        string `json:"type"`
	ChallengeID B64    `json:"challenge_id"`
	HostKey     B64    `json:"hk"`
	HostNonce   B64    `json:"hn"`
	DeviceKey   B64    `json:"device_key"`
	DeviceNonce B64    `json:"device_nonce"`
	RequestHash B64    `json:"request_hash"`
	GrantID     B64    `json:"grant_id"`
	AnswerSDP   string `json:"answer_sdp"`
	Sig         B64    `json:"sig"`
}

// DecodeMessage parses a mailbox message and returns a pointer to one of the
// message structs above. Unknown fields are ignored; unknown versions/types fail.
func DecodeMessage(data []byte) (any, error) {
	if len(data) > MaxMessageBytes {
		return nil, fmt.Errorf("%w: message too large", ErrInvalid)
	}
	var head struct {
		V    int    `json:"v"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if head.V != Version {
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
	case TypeReconnectHello:
		msg = &ReconnectHello{}
	case TypeReconnectChallenge:
		msg = &ReconnectChallenge{}
	case TypeReconnectRequest:
		msg = &ReconnectRequest{}
	case TypeReconnectAnswer:
		msg = &ReconnectAnswer{}
	default:
		return nil, fmt.Errorf("%w: unknown type %q", ErrInvalid, head.Type)
	}
	if err := json.Unmarshal(data, msg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return msg, nil
}

// Transcript returns the canonical bytes that are MACed and signed:
// field(domain) || field(label) || field(f1) || ... with field(x) = u32be(len(x)) || x.
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

func pairRequestTranscript(r *PairRequest, hostKey []byte) []byte {
	return Transcript(labelPairRequest, []byte(roleDevice), r.SessionID, hostKey, r.HostNonce,
		r.DeviceKey, r.DeviceNonce, []byte(r.DeviceName), []byte(r.OfferSDP))
}

func pairAnswerTranscript(a *PairAnswer) []byte {
	return Transcript(labelPairAnswer, []byte(roleHost), a.SessionID, a.HostKey, a.HostNonce,
		a.DeviceKey, a.DeviceNonce, a.RequestHash, a.GrantID, intField(a.GrantExpiresAt), []byte(a.AnswerSDP))
}

func reconnectRequestTranscript(r *ReconnectRequest, hostKey []byte) []byte {
	return Transcript(labelReconnectRequest, []byte(roleDevice), r.ChallengeID, hostKey, r.HostNonce,
		r.DeviceKey, r.DeviceNonce, r.GrantID, []byte(r.OfferSDP))
}

func reconnectAnswerTranscript(a *ReconnectAnswer) []byte {
	return Transcript(labelReconnectAnswer, []byte(roleHost), a.ChallengeID, a.HostKey, a.HostNonce,
		a.DeviceKey, a.DeviceNonce, a.RequestHash, a.GrantID, []byte(a.AnswerSDP))
}

// TranscriptMAC computes the pairing MAC for transcript bytes under secret.
func TranscriptMAC(secret, transcript []byte) []byte {
	m := hmac.New(sha256.New, macKeyFor(secret))
	m.Write(transcript)
	return m.Sum(nil)
}

// macKeyFor derives the MAC key: HMAC-SHA256(key=secret, msg=macKeyLabel).
func macKeyFor(secret []byte) []byte {
	k := hmac.New(sha256.New, secret)
	k.Write([]byte(macKeyLabel))
	return k.Sum(nil)
}

func hashTranscript(t []byte) []byte {
	h := sha256.Sum256(t)
	return h[:]
}

func wantLen(name string, b []byte, n int) error {
	if len(b) != n {
		return fmt.Errorf("%w: %s must be %d bytes, got %d", ErrInvalid, name, n, len(b))
	}
	return nil
}

func checkHeader(v int, typ, want string) error {
	if v != Version {
		return fmt.Errorf("%w: unsupported version %d", ErrInvalid, v)
	}
	if typ != want {
		return fmt.Errorf("%w: type %q, want %q", ErrInvalid, typ, want)
	}
	return nil
}

// ValidateDeviceName rejects names that are empty, too long, invalid UTF-8,
// or contain control/format characters (they are shown in a terminal prompt).
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
		{"sid", r.SessionID, SessionIDSize}, {"hn", r.HostNonce, NonceSize},
		{"device_key", r.DeviceKey, 32}, {"device_nonce", r.DeviceNonce, NonceSize},
		{"mac", r.MAC, MACSize}, {"sig", r.Sig, 64},
	} {
		if err := wantLen(c.name, c.b, c.n); err != nil {
			return err
		}
	}
	return ValidateDeviceName(r.DeviceName)
}

func (r *ReconnectRequest) validate() error {
	if err := checkHeader(r.V, r.Type, TypeReconnectRequest); err != nil {
		return err
	}
	for _, c := range []struct {
		name string
		b    []byte
		n    int
	}{
		{"challenge_id", r.ChallengeID, ChallengeIDSize}, {"hn", r.HostNonce, NonceSize},
		{"device_key", r.DeviceKey, 32}, {"device_nonce", r.DeviceNonce, NonceSize},
		{"grant_id", r.GrantID, GrantIDSize}, {"sig", r.Sig, 64},
	} {
		if err := wantLen(c.name, c.b, c.n); err != nil {
			return err
		}
	}
	return nil
}

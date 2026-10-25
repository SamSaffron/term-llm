package peerbroker

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	APIVersion    = "v2"
	WSSubprotocol = "peerbroker.v2"
)

const (
	CodeBadRequest      = "bad_request"
	CodeUnauthorized    = "unauthorized"
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeExpired         = "expired"
	CodeRateLimited     = "rate_limited"
	CodeQuotaExceeded   = "quota_exceeded"
	CodePeerOffline     = "peer_offline"
	CodeQueueFull       = "queue_full"
	CodeTooLarge        = "too_large"
	CodeBrokerClosed    = "broker_closed"
	CodeTURNUnavailable = "turn_unavailable"
	CodeInternal        = "internal"
)

var (
	ErrUnauthorized    = errors.New("peerbroker: unauthorized")
	ErrExpired         = errors.New("peerbroker: expired")
	ErrRateLimited     = errors.New("peerbroker: rate or quota exceeded")
	ErrPeerOffline     = errors.New("peerbroker: peer offline")
	ErrTooLarge        = errors.New("peerbroker: payload too large")
	ErrTURNUnavailable = errors.New("peerbroker: TURN unavailable")
)

type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("peerbroker: HTTP %d %s: %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("peerbroker: HTTP %d %s", e.StatusCode, e.Code)
}
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.Code == CodeUnauthorized
	case ErrExpired:
		return e.Code == CodeExpired
	case ErrRateLimited:
		return e.Code == CodeRateLimited || e.Code == CodeQuotaExceeded || e.Code == CodeQueueFull
	case ErrPeerOffline:
		return e.Code == CodePeerOffline
	case ErrTooLarge:
		return e.Code == CodeTooLarge
	case ErrTURNUnavailable:
		return e.Code == CodeTURNUnavailable
	}
	return false
}

type Challenge struct {
	ID        string    `json:"challenge_id"`
	Nonce     string    `json:"nonce"`
	ExpiresAt time.Time `json:"expires_at"`
}

// DeviceCapabilityHash names one active device reconnect capability. The host
// generates the capability and sends only its Hash to the broker.
type DeviceCapabilityHash struct {
	DeviceID string `json:"device_id"`
	Hash     string `json:"capability_hash"`
}

type Registration struct {
	HostID          string    `json:"host_id"`
	LeaseCapability string    `json:"lease_capability"`
	LeaseExpiresAt  time.Time `json:"lease_expires_at"`
}

func (r Registration) String() string {
	return fmt.Sprintf("peerbroker.Registration{HostID:%s, LeaseCapability:[REDACTED], LeaseExpiresAt:%s}", r.HostID, r.LeaseExpiresAt.Format(time.RFC3339))
}
func (r Registration) GoString() string { return r.String() }

type Lease struct {
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

type InvitationSlot struct {
	InvitationID string    `json:"invitation_id"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

func (s ICEServer) String() string {
	credential := ""
	if s.Credential != "" {
		credential = "[REDACTED]"
	}
	return fmt.Sprintf("peerbroker.ICEServer{URLs:%v, Username:%q, Credential:%s}", s.URLs, s.Username, credential)
}
func (s ICEServer) GoString() string { return s.String() }

type ICECredentials struct {
	ICEServers []ICEServer `json:"ice_servers"`
	ExpiresAt  time.Time   `json:"expires_at,omitempty"`
}

func (c ICECredentials) String() string {
	return fmt.Sprintf("peerbroker.ICECredentials{ICEServers:%v, ExpiresAt:%s}", c.ICEServers, c.ExpiresAt.Format(time.RFC3339))
}
func (c ICECredentials) GoString() string { return c.String() }

const (
	FrameReady     = "ready"
	FrameSignal    = "signal"
	FramePeer      = "peer"
	FrameError     = "error"
	PeerHost       = "host"
	PeerInvitation = "invitation"
	PeerDevice     = "device"
)

type Frame struct {
	Type           string          `json:"type"`
	PeerID         string          `json:"peer_id,omitempty"`
	PeerKind       string          `json:"peer_kind,omitempty"`
	Body           json.RawMessage `json:"body,omitempty"`
	Role           string          `json:"role,omitempty"`
	PeerConnected  *bool           `json:"peer_connected,omitempty"`
	LeaseExpiresAt *time.Time      `json:"lease_expires_at,omitempty"`
	Error          string          `json:"error,omitempty"`
}

func (f Frame) String() string {
	return fmt.Sprintf("peerbroker.Frame{Type:%s, PeerID:%s, PeerKind:%s, Role:%s, Error:%s}", f.Type, f.PeerID, f.PeerKind, f.Role, f.Error)
}
func (f Frame) GoString() string { return f.String() }

type registerRequest struct {
	PublicKey   string                 `json:"public_key"`
	ChallengeID string                 `json:"challenge_id"`
	Signature   string                 `json:"signature"`
	Devices     []DeviceCapabilityHash `json:"devices"`
}
type challengeRequest struct {
	PublicKey string `json:"public_key"`
}
type renewRequest struct {
	Devices []DeviceCapabilityHash `json:"devices"`
}
type invitationRequest struct {
	InvitationID   string    `json:"invitation_id"`
	CapabilityHash string    `json:"capability_hash"`
	ExpiresAt      time.Time `json:"expires_at"`
}
type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

func validatePublicKey(key ed25519.PublicKey) error {
	if len(key) != ed25519.PublicKeySize {
		return errors.New("peerbroker: Ed25519 public key must be 32 bytes")
	}
	return nil
}

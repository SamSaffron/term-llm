package peerbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Error codes returned in the "error" field of broker error responses.
const (
	CodeUnauthorized    = "unauthorized"
	CodeBadRequest      = "bad_request"
	CodeTooLarge        = "too_large"
	CodeMailboxFull     = "mailbox_full"
	CodeMessageLimit    = "session_message_limit"
	CodeRateLimited     = "rate_limited"
	CodeTooManyWaiters  = "too_many_waiters"
	CodeSessionLimit    = "session_limit"
	CodeSessionClosed   = "session_closed"
	CodeTURNUnavailable = "turn_unavailable"
	CodeTURNQuota       = "turn_quota_exceeded"
	CodeTURNFailed      = "turn_mint_failed"
	CodeBrokerClosed    = "broker_closed"
	CodeTooManyConns    = "too_many_connections"
	CodeConnectQuota    = "connect_quota_exceeded"
	CodeNotFound        = "not_found"
)

// Sentinel errors matched by errors.Is against *APIError values returned by Client.
var (
	// ErrUnauthorized covers unknown, expired or closed sessions and bad capabilities.
	ErrUnauthorized = errors.New("peerbroker: unauthorized or expired session")
	// ErrSessionClosed is returned when a session is deleted while a receive is waiting.
	ErrSessionClosed = errors.New("peerbroker: session closed")
	// ErrMailboxFull is returned when the peer has too many unacknowledged messages.
	ErrMailboxFull = errors.New("peerbroker: peer mailbox full")
	// ErrRateLimited covers global rate limits, waiter limits and per-session quotas.
	ErrRateLimited = errors.New("peerbroker: rate or quota limit exceeded")
	// ErrTooLarge is returned when a message exceeds the broker's size limit.
	ErrTooLarge = errors.New("peerbroker: message too large")
	// ErrTURNUnavailable is returned when the broker has no TURN minter configured.
	ErrTURNUnavailable = errors.New("peerbroker: TURN unavailable")
)

// APIError is a non-success broker response. It never contains capabilities.
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

// Is maps broker error codes onto the package sentinel errors.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.Code == CodeUnauthorized
	case ErrSessionClosed:
		return e.Code == CodeSessionClosed
	case ErrMailboxFull:
		return e.Code == CodeMailboxFull
	case ErrRateLimited:
		switch e.Code {
		case CodeRateLimited, CodeTooManyWaiters, CodeTooManyConns, CodeConnectQuota, CodeMessageLimit, CodeTURNQuota, CodeSessionLimit:
			return true
		}
	case ErrTooLarge:
		return e.Code == CodeTooLarge
	case ErrTURNUnavailable:
		return e.Code == CodeTURNUnavailable
	}
	return false
}

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

type createRequest struct {
	TTLSeconds int `json:"ttl_seconds,omitempty"`
}

// Session is a freshly created rendezvous. HostToken stays with the creator;
// ID, DeviceToken and the broker URL are what the device needs (e.g. via QR).
// Neither token is derived from, or related to, any pairing secret.
type Session struct {
	ID          string    `json:"session_id"`
	HostToken   string    `json:"host_token"`
	DeviceToken string    `json:"device_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// String redacts both capabilities.
func (s Session) String() string {
	return fmt.Sprintf("peerbroker.Session{ID:%s, HostToken:[REDACTED], DeviceToken:[REDACTED], ExpiresAt:%s}", s.ID, s.ExpiresAt.Format(time.RFC3339))
}

// GoString redacts both capabilities in %#v output.
func (s Session) GoString() string { return s.String() }

type sendRequest struct {
	Body json.RawMessage `json:"body"`
}

type sendResponse struct {
	Seq uint64 `json:"seq"`
}

// Message is one opaque mailbox entry. Seq increases monotonically per mailbox.
type Message struct {
	Seq  uint64          `json:"seq"`
	Body json.RawMessage `json:"body"`
}

type receiveResponse struct {
	Messages []Message `json:"messages"`
}

// ICEServer mirrors the WebRTC RTCIceServer dictionary.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// ICECredentials is a short-lived ICE/TURN configuration.
type ICECredentials struct {
	ICEServers []ICEServer `json:"ice_servers"`
	ExpiresAt  time.Time   `json:"expires_at"`
}

// TURNMinter mints short-lived TURN credentials. Implementations must not log
// or return long-term secrets in errors.
type TURNMinter interface {
	MintICEServers(ctx context.Context, ttl time.Duration) (*ICECredentials, error)
}

package oauth

import (
	"errors"
	"net/http"
	"time"
)

var (
	// ErrAuthenticationRequired indicates that an interactive authorization flow
	// is required. Background MCP connections never open a browser themselves.
	ErrAuthenticationRequired = errors.New("MCP authentication required")
	// ErrRefreshRejected indicates that the authorization server rejected the
	// stored refresh grant and the user must sign in again.
	ErrRefreshRejected = errors.New("MCP OAuth refresh grant expired or revoked")
	// ErrStaleVersion prevents an older process from replacing newer credentials.
	ErrStaleVersion = errors.New("stale MCP OAuth credential version")
	// ErrNotFound indicates that no grant is stored for an endpoint.
	ErrNotFound = errors.New("MCP OAuth credentials not found")
)

// Options configures the SDK authorization-code handler for an MCP endpoint.
type Options struct {
	ClientID     string
	ClientSecret string
	Scopes       []string
	// ScopesConfigured distinguishes an explicitly empty scope list from an
	// omitted list. When omitted, login requests every scope advertised by the
	// authorization server.
	ScopesConfigured    bool
	ClientIDMetadataURL string
	HTTPClient          *http.Client
}

// AuthState is the product-facing state of an MCP OAuth grant.
type AuthState string

const (
	AuthNotNeeded AuthState = "not_needed"
	AuthSignedOut AuthState = "signed_out"
	AuthSignedIn  AuthState = "signed_in"
	AuthExpired   AuthState = "expired"
	AuthRequired  AuthState = "needs_sign_in"
	AuthWaiting   AuthState = "waiting"
	AuthRetry     AuthState = "retry"
)

// AuthStatus contains safe grant metadata. It never contains credentials.
type AuthStatus struct {
	State       AuthState `json:"state"`
	Issuer      string    `json:"issuer,omitempty"`
	Scopes      []string  `json:"scopes,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
	StoragePath string    `json:"storage_path,omitempty"`
	CanSignIn   bool      `json:"can_sign_in"`
	CanSignOut  bool      `json:"can_sign_out"`
}

// FlowState is the state of an interactive browser authorization flow.
type FlowState string

const (
	FlowStarting  FlowState = "starting"
	FlowPending   FlowState = "pending"
	FlowSucceeded FlowState = "succeeded"
	FlowFailed    FlowState = "failed"
	FlowCanceled  FlowState = "canceled"
	FlowExpired   FlowState = "expired"
)

// Flow is the safe, public view of an OAuth flow. OAuth codes, PKCE values,
// tokens, client secrets, and the callback state capability are never exposed.
type Flow struct {
	ID               string    `json:"flow_id"`
	Endpoint         string    `json:"-"`
	AuthorizationURL string    `json:"authorization_url,omitempty"`
	ExpiresAt        time.Time `json:"expires_at"`
	State            FlowState `json:"state"`
	Error            string    `json:"error,omitempty"`
	Created          bool      `json:"-"`
}

// Package notification defines the shared Web Push wire envelope and event identity helpers.
package notification

import (
	"crypto/sha256"
	"encoding/base64"
	"time"
)

const (
	Version        = 2
	KindManual     = "manual"
	KindCompletion = "completion"
	// MaxTTL bounds both transport retention and receiver replay tracking.
	MaxTTL = 24 * time.Hour
)

// Envelope is the version 2 notification wire contract. Manual events have no
// completion ownership or read semantics; their completion fields are omitted.
// Timestamps are UTC RFC3339 strings, including fractional seconds when needed.
type Envelope struct {
	Version         int    `json:"version"`
	Kind            string `json:"kind"`
	EventID         string `json:"event_id"`
	Title           string `json:"title"`
	Body            string `json:"body"`
	CreatedAt       string `json:"created_at"`
	ExpiresAt       string `json:"expires_at"`
	URL             string `json:"url"`
	TurnID          string `json:"turn_id,omitempty"`
	ResponseID      string `json:"response_id,omitempty"`
	WindowID        string `json:"window_id,omitempty"`
	StoreInstanceID string `json:"store_instance_id,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	AttentionSeq    int64  `json:"attention_seq,omitempty"`
	RunEpoch        int64  `json:"run_epoch,omitempty"`
	Outcome         string `json:"outcome,omitempty"`
	Source          string `json:"source,omitempty"`
}

// Topic returns a stable, URL-safe, 32-character Web Push topic scoped to one
// event. Repeated attempts for an event replace only that event, not its siblings.
func Topic(eventID string) string {
	digest := sha256.Sum256([]byte(eventID))
	return base64.RawURLEncoding.EncodeToString(digest[:])[:32]
}

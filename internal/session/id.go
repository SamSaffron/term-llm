package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// NewID generates a unique session ID using a timestamp prefix and random suffix.
// Format: YYYYMMDD-HHMMSS-RANDOM (e.g., "20240115-143052-a1b2c3")
// This format:
//   - Sorts chronologically by default
//   - Is human-readable for debugging
//   - Has enough randomness to prevent collisions
func NewID() string {
	now := time.Now()
	random := make([]byte, 3) // 6 hex chars
	rand.Read(random)
	return fmt.Sprintf("%s-%s",
		now.Format("20060102-150405"),
		hex.EncodeToString(random),
	)
}

// ParseIDTime extracts the timestamp from a session ID.
// Returns zero time if parsing fails.
func ParseIDTime(id string) time.Time {
	if len(id) < 15 {
		return time.Time{}
	}
	t, _ := time.Parse("20060102-150405", id[:15])
	return t
}

// ShortID returns a shortened version of the session ID for display.
// Example: "20240115-143052-a1b2c3" -> "240115-1430"
func ShortID(id string) string {
	if len(id) < 15 {
		return id
	}
	// Skip first 2 chars (century), take YYMMDD-HHMM
	return id[2:8] + "-" + id[9:13]
}

// ExpandShortID converts a short ID to a SQL LIKE pattern for prefix matching.
// Example: "240115-1430" -> "20240115-1430%"
func ExpandShortID(shortID string) string {
	// If already looks like a full ID (starts with 20), use as-is
	if strings.HasPrefix(shortID, "20") && len(shortID) >= 15 {
		return shortID
	}
	// Short ID format: YYMMDD-HHMM (11 chars)
	if len(shortID) == 11 && shortID[6] == '-' {
		return "20" + shortID[:6] + "-" + shortID[7:] + "%"
	}
	// Partial match - just append wildcard
	return shortID + "%"
}

// Package lifecycle defines the dependency-free lifecycle domain shared by chat
// and terminal-host integrations.
package lifecycle

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// SchemaVersion is the current JSON lifecycle event schema.
	SchemaVersion = 1

	maxProducerRunes  = 64
	maxSessionRunes   = 256
	maxMessageRunes   = 512
	maxCWDRunes       = 4096
	maxResumeArgs     = 64
	maxResumeArgRunes = 4096
)

// State is the coarse foreground lifecycle state.
type State string

const (
	Idle    State = "idle"
	Working State = "working"
	Blocked State = "blocked"
)

// Kind identifies whether an event publishes state or relinquishes authority.
type Kind string

const (
	KindState   Kind = "state"
	KindRelease Kind = "release"
)

// Snapshot is a side-effect-free view of the currently visible chat.
// Message is an optional, stable detail suitable for display by a terminal host.
// CWD is the visible session's effective working directory when available; the
// manager uses the producer process working directory as its event fallback.
type Snapshot struct {
	State     State
	SessionID string
	Message   string
	CWD       string
}

// Metadata identifies the term-llm process producing lifecycle events.
type Metadata struct {
	Producer   string
	PID        int
	CWD        string
	ResumeArgv []string
}

// Event is the stable version 1 JSON contract consumed by generic command
// sinks. Field order is intentional so encoded events remain byte-compatible.
type Event struct {
	SchemaVersion int      `json:"schema_version"`
	Producer      string   `json:"producer"`
	Kind          Kind     `json:"kind"`
	Sequence      int64    `json:"sequence"`
	Timestamp     string   `json:"timestamp"`
	State         State    `json:"state"`
	Message       string   `json:"message"`
	SessionID     string   `json:"session_id"`
	PID           int      `json:"pid"`
	CWD           string   `json:"cwd"`
	ResumeArgv    []string `json:"resume_argv,omitempty"`
}

// NewEvent constructs a normalized version 1 event. Release events retain the
// last session and resume context when available but never claim a live state.
func NewEvent(kind Kind, sequence int64, timestamp time.Time, metadata Metadata, snapshot Snapshot) Event {
	snapshot = NormalizeSnapshot(snapshot)
	metadata = NormalizeMetadata(metadata)
	if kind == KindRelease {
		snapshot.State = ""
		snapshot.Message = ""
	}
	return Event{
		SchemaVersion: SchemaVersion,
		Producer:      metadata.Producer,
		Kind:          kind,
		Sequence:      sequence,
		Timestamp:     timestamp.UTC().Format(time.RFC3339Nano),
		State:         snapshot.State,
		Message:       snapshot.Message,
		SessionID:     snapshot.SessionID,
		PID:           metadata.PID,
		CWD:           metadata.CWD,
		ResumeArgv:    metadata.ResumeArgv,
	}
}

// NormalizeSnapshot removes controls, normalizes whitespace, and bounds values
// before they can reach a terminal host or external command.
func NormalizeSnapshot(snapshot Snapshot) Snapshot {
	if !ValidState(snapshot.State) {
		snapshot.State = ""
	}
	snapshot.SessionID = sanitizeCollapsed(snapshot.SessionID, maxSessionRunes)
	snapshot.Message = sanitizeCollapsed(snapshot.Message, maxMessageRunes)
	snapshot.CWD = sanitizePreservingSpace(snapshot.CWD, maxCWDRunes)
	if snapshot.State == Idle {
		snapshot.Message = ""
	}
	return snapshot
}

// NormalizeMetadata sanitizes and bounds process metadata and resume arguments.
func NormalizeMetadata(metadata Metadata) Metadata {
	metadata.Producer = sanitizeCollapsed(metadata.Producer, maxProducerRunes)
	metadata.CWD = sanitizePreservingSpace(metadata.CWD, maxCWDRunes)
	if metadata.PID < 0 {
		metadata.PID = 0
	}
	if len(metadata.ResumeArgv) > maxResumeArgs {
		metadata.ResumeArgv = metadata.ResumeArgv[:maxResumeArgs]
	}
	if len(metadata.ResumeArgv) > 0 {
		args := make([]string, 0, len(metadata.ResumeArgv))
		for _, arg := range metadata.ResumeArgv {
			args = append(args, sanitizePreservingSpace(arg, maxResumeArgRunes))
		}
		metadata.ResumeArgv = args
	}
	return metadata
}

// ValidState reports whether state belongs to the public lifecycle vocabulary.
func ValidState(state State) bool {
	switch state {
	case Idle, Working, Blocked:
		return true
	default:
		return false
	}
}

func sanitizeCollapsed(value string, maxRunes int) string {
	value = sanitizePreservingSpace(value, maxRunes)
	return strings.Join(strings.Fields(value), " ")
}

func sanitizePreservingSpace(value string, maxRunes int) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, strings.TrimSpace(value))
	if maxRunes <= 0 || utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	return string([]rune(value)[:maxRunes])
}

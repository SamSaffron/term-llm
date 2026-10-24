package clibin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

func (p *ClaudeBinProvider) recordTurnBoundary(req core.Request, resumedSessionID string, err error) {
	// An API-error result still acknowledges the input and may follow committed
	// MCP tools. Save that boundary for explicit continuation, never replay the
	// stream automatically. Startup/transport errors do not acknowledge input.
	accepted := err == nil || (p.sessionID != "" && strings.HasPrefix(err.Error(), "claude API error:"))
	if !req.Ephemeral && err != nil && isPromptTooLong(err) {
		// The native session now holds a context the API rejects outright.
		// Resuming it can never succeed: every later turn, however short, re-sends
		// the same oversized context and fails again, and Claude Code cannot
		// natively compact a single oversized exchange. Drop it so the next turn
		// or a manual compaction starts from term-llm's durable transcript.
		runtimeoutput.Warn("claude-bin prompt too long; discarding native session so it is not resumed",
			"session_id", p.sessionID)
		p.ResetConversation()
		return
	}
	if !req.Ephemeral && !accepted {
		// A process/transport failure leaves it unknown how much input the native
		// session consumed. Neither a zero nor a prior turn's cursor is safe:
		// persisting either would replay input into an already-mutated session.
		// Discard only native state; durable tool results/history remain intact
		// and explicit continuation can fall back to a fresh conversation.
		p.ResetConversation()
		return
	}

	// Track messages sent so we don't re-send them on resume, and fingerprint
	// that prefix so a later history rewrite cannot be mistaken for a clean
	// continuation. Ephemeral requests are isolated and must not alter resume
	// accounting.
	if !req.Ephemeral && accepted {
		p.messagesSent = len(req.Messages)
		p.transcriptDigest = claudeTranscriptDigest(req.Messages, p.messagesSent)
		// A branch owns the session Claude Code reported back to it. Continuing
		// to pass --fork-session would branch again on every later turn, leaving
		// an orphan session per exchange and no accumulating session for a
		// promoted conversation to continue. Only stop forking once the CLI has
		// actually moved us onto a different session.
		if p.forkSession && p.sessionID != "" && p.sessionID != resumedSessionID {
			p.forkSession = false
		}
	}
}

// claudeTranscriptDigest fingerprints the first n messages of a transcript so a
// resumed Claude Code session can be checked against the conversation term-llm
// believes it already delivered. Undo, branching and other history rewrites can
// replace earlier turns without shortening the transcript, and resuming on a
// stale boundary silently drops or duplicates conversation.
//
// System messages are excluded: they travel as --system-prompt on every turn,
// not in Claude's session, and a changed agent prompt is not a history rewrite.
// Including them made every long session replay its whole transcript as one
// message after a prompt change plus restart.
func claudeTranscriptDigest(messages []core.Message, n int) string {
	return claudeDigest(messages, n, false)
}

// claudeDigestMatches accepts the current digest and the legacy one, which
// included system messages, so previously persisted state keeps resuming.
func claudeDigestMatches(messages []core.Message, n int, digest string) bool {
	return digest != "" && (digest == claudeDigest(messages, n, false) || digest == claudeDigest(messages, n, true))
}

func claudeDigest(messages []core.Message, n int, includeSystem bool) string {
	if n <= 0 || n > len(messages) {
		return ""
	}
	h := sha256.New()
	for _, msg := range messages[:n] {
		if msg.Role == core.RoleSystem && !includeSystem {
			continue
		}
		fmt.Fprintf(h, "\x00%s", msg.Role)
		for _, part := range msg.Parts {
			encoded, err := json.Marshal(part)
			if err != nil {
				// Message parts are expected to contain JSON-compatible transcript
				// data. Preserve a stable minimum fingerprint if that changes.
				fmt.Fprintf(h, "\x01%s\x02%s", part.Type, part.Text)
				continue
			}
			h.Write([]byte{1})
			h.Write(encoded)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

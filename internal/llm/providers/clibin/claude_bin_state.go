package clibin

import (
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func (p *ClaudeBinProvider) recordTurnBoundary(req core.Request, resumedSessionID string, err error) {
	// An API-error result still acknowledges the input and may follow committed
	// MCP tools. Save that boundary for explicit continuation, never replay the
	// stream automatically. Startup/transport errors do not acknowledge input.
	accepted := err == nil || (p.sessionID != "" && strings.HasPrefix(err.Error(), "claude API error:"))
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

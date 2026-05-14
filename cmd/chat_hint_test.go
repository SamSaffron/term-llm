package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/session"
)

func TestChatResumeCommandPrefersSessionNumber(t *testing.T) {
	got := chatResumeCommand(&session.Session{ID: "20260514-171854-deadbeef", Number: 4400})
	if got != "term-llm chat --resume=4400" {
		t.Fatalf("chatResumeCommand() = %q", got)
	}
}

func TestChatResumeCommandUsesEqualsSyntaxForTimestampIDFallback(t *testing.T) {
	got := chatResumeCommand(&session.Session{ID: "20260514-171854-deadbeef"})
	if got != "term-llm chat --resume=260514-1718" {
		t.Fatalf("chatResumeCommand() = %q", got)
	}
}

func TestChatResumeCommandPreservesNonTimestampIDsWithoutNumber(t *testing.T) {
	got := chatResumeCommand(&session.Session{ID: "ss_779-e03dabcdef"})
	if got != "term-llm chat --resume=ss_779-e03dabcdef" {
		t.Fatalf("chatResumeCommand() = %q", got)
	}
}

package cmd

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
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

func TestChatResumeHintUsesPersistedFinalSession(t *testing.T) {
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, tc := range []struct {
		name         string
		users, turns int
		want         bool
	}{
		{"empty", 0, 0, false}, {"completed", 1, 1, true}, {"interrupted", 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := &session.Session{ID: session.NewID(), Provider: "test", Model: "test", Mode: session.ModeChat, UserTurns: tc.users, LLMTurns: tc.turns}
			if err := store.Create(context.Background(), sess); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			printChatResumeHint(&output, store, sess.ID)
			if got := strings.Contains(output.String(), "Resume: term-llm chat --resume="); got != tc.want {
				t.Fatalf("hint=%q want visible=%v", output.String(), tc.want)
			}
			if tc.want {
				saved, err := store.Get(context.Background(), sess.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(output.String(), chatResumeCommand(saved)) {
					t.Fatal("hint did not use persisted session number", output.String())
				}
			}
		})
	}
	for _, id := range []string{"", "missing"} {
		var output bytes.Buffer
		printChatResumeHint(&output, store, id)
		if output.Len() != 0 {
			t.Fatal("hint for unavailable session", output.String())
		}
	}
	var output bytes.Buffer
	printChatResumeHint(&output, nil, "disabled")
	if output.Len() != 0 {
		t.Fatal("hint with session storage disabled")
	}
}

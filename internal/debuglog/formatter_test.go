package debuglog

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestFormatSessionShowsClaudeCLIReproDetails(t *testing.T) {
	session := &Session{
		ID:        "session-1",
		StartTime: time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC),
		EndTime:   time.Date(2026, 4, 27, 0, 0, 1, 0, time.UTC),
		Provider:  "Claude CLI (sonnet)",
		Model:     "sonnet",
		HasErrors: true,
		Entries: []any{
			EventEntry{
				Timestamp: time.Date(2026, 4, 27, 0, 0, 1, 0, time.UTC),
				EventType: "error",
				Data: map[string]any{
					"error":               "claude command failed (exit 7): exit status 7",
					"provider_error_type": "claude_cli_command",
					"cwd":                 "/repo",
					"command_line":        "claude --print --model sonnet",
					"env": map[string]any{
						"CLAUDE_CODE_EFFORT_LEVEL": "high",
					},
					"removed_env":  []any{"ANTHROPIC_API_KEY"},
					"prefer_oauth": true,
					"stdin_len":    float64(11),
					"stdin_sha256": "abc123",
					"stdin":        "User: hello",
					"stdout_tail":  `{"type":"system"}`,
					"stderr_tail":  "fatal problem",
				},
			},
		},
	}

	var buf bytes.Buffer
	FormatSession(&buf, session, FormatOptions{NoColor: true, ShowTimestamp: true})
	out := buf.String()
	for _, want := range []string{
		"Claude CLI repro details:",
		"cwd: /repo",
		"command: claude --print --model sonnet",
		"env: CLAUDE_CODE_EFFORT_LEVEL=high",
		"auth: prefer Claude OAuth",
		"removed env: ANTHROPIC_API_KEY",
		"stdin: 11 bytes sha256=abc123",
		"stdin preview:",
		"User: hello",
		"stdout tail:",
		`{"type":"system"}`,
		"stderr tail:",
		"fatal problem",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("formatted session missing %q:\n%s", want, out)
		}
	}
}

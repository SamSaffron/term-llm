package sessions

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/session"
)

func TestView_NarrowWidth_DoesNotPanic(t *testing.T) {
	m := New(nil, 2, 10, nil)
	m.sessions = []session.SessionSummary{
		{
			ID:           "s1",
			Number:       1,
			Summary:      "this summary is long enough to trigger truncation",
			Model:        "gpt-5.2-codex",
			MessageCount: 2,
			UpdatedAt:    time.Now(),
		},
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("View panicked for narrow width: %v", r)
		}
	}()

	_ = m.View()
}

func TestView_Truncation_PreservesUTF8(t *testing.T) {
	m := New(nil, 120, 10, nil)
	m.sessions = []session.SessionSummary{
		{
			ID:           "s1",
			Number:       1,
			Summary:      strings.Repeat("🙂", 8), // >25 bytes, easy to cut mid-rune if byte-truncated
			Model:        strings.Repeat("模", 6), // >10 bytes, easy to cut mid-rune if byte-truncated
			MessageCount: 1,
			UpdatedAt:    time.Now(),
		},
	}

	out := m.View()
	if !utf8.ValidString(out) {
		t.Fatalf("View output must be valid UTF-8, got invalid bytes")
	}
}

package termimage

import (
	"strings"
	"testing"
)

func TestCleanupSequenceKittyOnly(t *testing.T) {
	if got := CleanupSequence(Environment{ForcedProtocol: "kitty"}); !strings.Contains(got, "a=d,d=A") {
		t.Fatalf("forced kitty cleanup = %q, want delete visible placements", got)
	}
	if got := CleanupSequence(Environment{KittyWindowID: "1"}); !strings.Contains(got, "a=d,d=A") {
		t.Fatalf("detected kitty cleanup = %q, want delete visible placements", got)
	}
	if got := CleanupSequence(Environment{ForcedProtocol: "ansi", KittyWindowID: "1"}); got != "" {
		t.Fatalf("forced ansi cleanup = %q, want empty", got)
	}
	if got := CleanupSequence(Environment{Term: "xterm-256color"}); got != "" {
		t.Fatalf("non-kitty cleanup = %q, want empty", got)
	}
}

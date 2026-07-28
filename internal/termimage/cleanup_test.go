package termimage

import (
	"strings"
	"testing"
)

func TestKittyDeleteImageSequenceDeduplicatesInLinearOrder(t *testing.T) {
	got := KittyDeleteImageSequence(7, 0, 42, 7)
	want := "\x1b_Ga=d,i=7,q=2\x1b\\\x1b_Ga=d,i=42,q=2\x1b\\"
	if got != want {
		t.Fatalf("delete sequence = %q, want %q", got, want)
	}
}

func BenchmarkKittyDeleteImageSequence(b *testing.B) {
	ids := make([]uint32, 4096)
	for i := range ids {
		ids[i] = uint32(i + 1)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(ids)))
	for b.Loop() {
		_ = KittyDeleteImageSequence(ids...)
	}
}

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

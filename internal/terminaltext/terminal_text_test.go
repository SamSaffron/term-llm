package terminaltext

import (
	"bytes"
	"testing"
)

func TestSanitizeBytesCleanInputReturnsOriginalSlice(t *testing.T) {
	input := []byte("clean markdown\nwith a tab\tand unicode 世界")
	got := SanitizeBytes(input)
	if !bytes.Equal(got, input) {
		t.Fatalf("SanitizeBytes() = %q, want %q", got, input)
	}
	if len(got) > 0 && &got[0] != &input[0] {
		t.Fatal("SanitizeBytes copied clean input")
	}
	if allocs := testing.AllocsPerRun(1000, func() { got = SanitizeBytes(input) }); allocs != 0 {
		t.Fatalf("SanitizeBytes clean-input allocations = %v, want 0", allocs)
	}
}

func TestSanitizeBytesSanitizesControlsAndInvalidUTF8(t *testing.T) {
	input := append([]byte("before\x1b[2Jafter\x07"), 0xff)
	if got, want := string(SanitizeBytes(input)), "beforeafter"; got != want {
		t.Fatalf("SanitizeBytes() = %q, want %q", got, want)
	}
}

package main

import (
	"strings"
	"testing"
	"time"
)

func TestExtractCodeFencedGoBlock(t *testing.T) {
	code, err := extractCode("prose\n```go\npackage main\nfunc X() {}\n```\nmore")
	if err != nil {
		t.Fatalf("extractCode failed: %v", err)
	}
	if !strings.Contains(code, "func X") {
		t.Fatalf("unexpected code: %q", code)
	}
}

func TestScoreGoFunctionPasses(t *testing.T) {
	result := scoreGoFunction(`package main

func BinarySearch(xs []int, target int) int {
	lo, hi := 0, len(xs)-1
	for lo <= hi {
		mid := lo + (hi-lo)/2
		if xs[mid] == target { return mid }
		if xs[mid] < target { lo = mid + 1 } else { hi = mid - 1 }
	}
	return -1
}`, 10*time.Second, `
func TestGenerated(t *testing.T) {
	if got := BinarySearch([]int{1, 3, 5}, 3); got != 1 {
		t.Fatalf("got %d", got)
	}
}
`)
	if !result.Pass || result.Score != 1 {
		t.Fatalf("expected pass, got %#v", result)
	}
}

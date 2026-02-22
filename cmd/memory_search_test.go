package cmd

import "testing"

func TestNormalizeCosineScoreNegative(t *testing.T) {
	if got := normalizeCosineScore(-0.25); got != 0 {
		t.Fatalf("normalizeCosineScore(-0.25) = %f, want 0", got)
	}
}

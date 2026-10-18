package formatutil

import (
	"math"
	"strconv"
	"testing"
)

func TestFormatTokens(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0"}, {1, "1"}, {999, "999"}, {1000, "1k"},
		{1004, "1k"}, {1005, "1.01k"}, {1010, "1.01k"},
		{1100, "1.1k"}, {1234, "1.23k"}, {1235, "1.24k"},
		{120000, "120k"}, {12780000, "12.78M"},
		{987398793874, "987.4B"},
		{999994, "999.99k"}, {999995, "1M"}, {999999, "1M"},
		{1000000, "1M"}, {999994999, "999.99M"}, {999995000, "1B"},
		{1000000000, "1B"}, {999995000000, "1T"},
		{1000000000000, "1T"}, {999995000000000, "1P"},
		{1000000000000000, "1P"}, {999995000000000000, "1E"},
		{1000000000000000000, "1E"}, {math.MaxInt64, "9.22E"},
		{math.MinInt64, "-9.22E"},
	}
	for _, tt := range tests {
		t.Run(strconv.FormatInt(tt.n, 10), func(t *testing.T) {
			if got := FormatTokens(tt.n); got != tt.want {
				t.Errorf("FormatTokens(%d) = %q, want %q", tt.n, got, tt.want)
			}
			if tt.n > 0 {
				if got := FormatTokens(-tt.n); got != "-"+tt.want {
					t.Errorf("FormatTokens(%d) = %q, want %q", -tt.n, got, "-"+tt.want)
				}
			}
		})
	}
}

package cmd

import (
	"testing"
)

func TestServeWidgetsEnabledContract(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hasWeb   bool
		disabled bool
		want     bool
	}{
		{name: "web default", hasWeb: true, want: true},
		{name: "web opt out", hasWeb: true, disabled: true, want: false},
		{name: "non-web serve", hasWeb: false, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := serveWidgetsEnabled(tc.hasWeb, tc.disabled); got != tc.want {
				t.Fatalf("serveWidgetsEnabled(%t, %t) = %t, want %t", tc.hasWeb, tc.disabled, got, tc.want)
			}
		})
	}
}

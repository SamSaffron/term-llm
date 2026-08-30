package cmd

import "testing"

func TestServeWidgetsAreOptOut(t *testing.T) {
	disableFlag := serveCmd.Flags().Lookup("disable-widgets")
	if disableFlag == nil {
		t.Fatal("serve --disable-widgets flag is not registered")
	}
	if disableFlag.DefValue != "false" {
		t.Fatalf("serve --disable-widgets default = %q, want false", disableFlag.DefValue)
	}
	if enableFlag := serveCmd.Flags().Lookup("enable-widgets"); enableFlag != nil {
		t.Fatal("serve --enable-widgets should be replaced by --disable-widgets")
	}

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

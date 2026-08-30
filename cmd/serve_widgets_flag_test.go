package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestServeWidgetsAreOptOut(t *testing.T) {
	disableFlag := serveCmd.Flags().Lookup("disable-widgets")
	if disableFlag == nil {
		t.Fatal("serve --disable-widgets flag is not registered")
	}
	if disableFlag.DefValue != "false" {
		t.Fatalf("serve --disable-widgets default = %q, want false", disableFlag.DefValue)
	}

	enableFlag := serveCmd.Flags().Lookup("enable-widgets")
	if enableFlag == nil {
		t.Fatal("serve --enable-widgets compatibility flag is not registered")
	}
	if enableFlag.DefValue != "false" {
		t.Fatalf("serve --enable-widgets default = %q, want false", enableFlag.DefValue)
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

func TestDeprecatedEnableWidgetsWarning(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().Bool("enable-widgets", false, "")
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)

	warnDeprecatedEnableWidgets(cmd)
	if stderr.Len() != 0 {
		t.Fatalf("stderr without flag = %q, want empty", stderr.String())
	}

	if err := cmd.Flags().Set("enable-widgets", "true"); err != nil {
		t.Fatal(err)
	}
	warnDeprecatedEnableWidgets(cmd)

	warning := stderr.String()
	for _, want := range []string{"--enable-widgets is deprecated", "has no effect", "--disable-widgets"} {
		if !strings.Contains(warning, want) {
			t.Fatalf("stderr = %q, want it to contain %q", warning, want)
		}
	}
}

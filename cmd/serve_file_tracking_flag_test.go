package cmd

import (
	"testing"
)

func TestServeEnableFileTrackingFlag(t *testing.T) {
	flag := serveCmd.Flags().Lookup("enable-file-tracking")
	if flag == nil {
		t.Fatal("serve --enable-file-tracking flag is not registered")
	}
	if flag.DefValue != "false" {
		t.Fatalf("--enable-file-tracking default = %q, want false", flag.DefValue)
	}
}

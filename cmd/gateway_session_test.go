package cmd

import (
	"strings"
	"testing"
)

func TestGatewayProviderSessionIdleTimeoutFlag(t *testing.T) {
	flag := gatewayServeCmd.Flags().Lookup("provider-session-idle-timeout")
	if flag == nil {
		t.Fatal("gateway serve is missing --provider-session-idle-timeout")
	}
	if flag.DefValue != "30s" {
		t.Fatalf("provider session idle timeout default = %q, want 30s", flag.DefValue)
	}
	if !strings.Contains(flag.Usage, "0 disables") || !strings.Contains(flag.Usage, "WebSocket") {
		t.Fatalf("provider session idle timeout help is incomplete: %q", flag.Usage)
	}
}

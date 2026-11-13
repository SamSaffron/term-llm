package mcp

import (
	"strings"
	"testing"
)

// Existing configuration can predate name validation. The runtime boundary must
// still refuse to route server tools through the client's reserved namespace.
func TestLegacyReservedServerFailsRuntimePolicy(t *testing.T) {
	for _, name := range []string{"webmcp", "WebMCP", "WEBMCP", "webmcp_", "webmcp."} {
		manager := &Manager{}
		if err := manager.policyError(name); err == nil || !strings.Contains(err.Error(), "reserved for client tool routing") {
			t.Fatalf("legacy server %q bypassed the runtime reservation: %v", name, err)
		}
	}
}

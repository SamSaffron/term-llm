package cmd

import (
	"context"
	"testing"
)

// serveServer values built without an owner (tests, legacy paths) must still
// shut down cleanly.
func TestAgentHostOwnerNilShutdown(t *testing.T) {
	var owner *agentHostOwner
	if err := owner.Shutdown(context.Background()); err != nil {
		t.Fatalf("nil owner Shutdown() = %v", err)
	}
	s := &serveServer{}
	if err := s.agentOwner.Shutdown(context.Background()); err != nil {
		t.Fatalf("ownerless server Shutdown() = %v", err)
	}
}

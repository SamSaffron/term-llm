package llm_test

import (
	"context"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
)

func TestCoreContractsInteroperateWithLegacyCallers(t *testing.T) {
	var provider core.Provider = mockprovider.NewMockProvider("mock")
	if provider.Name() != "mock" {
		t.Fatalf("core provider name = %q", provider.Name())
	}

	checkpoint := &core.Continuation{Request: core.Request{Messages: []core.Message{core.AssistantText("ready")}}}
	var request core.Request = core.Request{Resume: checkpoint}
	if got := request.Resume.CommittedText(); got != "ready" {
		t.Fatalf("continuation committed text = %q", got)
	}

	ctx := core.ContextWithSessionID(context.Background(), "session")
	if got := core.SessionIDFromContext(ctx); got != "session" {
		t.Fatalf("core session ID = %q", got)
	}
	ctx = core.ContextWithCallID(ctx, "call")
	if got := core.CallIDFromContext(ctx); got != "call" {
		t.Fatalf("legacy call ID = %q", got)
	}
}

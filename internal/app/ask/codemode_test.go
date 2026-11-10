package ask

import (
	"context"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestCodeModeAskNativeApprovalUsesCellCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var after bool
	prompts := terminalCodeModePromptBinder(func(got context.Context) error {
		if got != ctx {
			t.Fatal("prompt inherited session instead of cell context")
		}
		return nil
	}, func() { after = true })(core.CodeModeCellRef{Ctx: ctx, CellID: "owned"})
	if !prompts.Serial {
		t.Fatal("native terminal prompts must serialize")
	}
	result, err := prompts.Prompt("file", false, false, "")
	if err != context.Canceled || !result.Cancelled || !after {
		t.Fatalf("native prompt cancellation: %+v %v after=%t", result, err, after)
	}
}
func TestCodeModeAskCancelledFlushDoesNotReleaseTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	after := false
	prompts := terminalCodeModePromptBinder(func(context.Context) error { return context.Canceled }, func() { after = true })(core.CodeModeCellRef{Ctx: ctx})
	_, err := prompts.Workspace("workspace")
	if err != context.Canceled || after {
		t.Fatalf("cancelled flush changed terminal ownership: %v after=%t", err, after)
	}
}

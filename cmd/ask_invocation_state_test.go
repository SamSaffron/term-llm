package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestRunAskRestoresInvocationGlobalsOnEarlyFailure(t *testing.T) {
	oldAgent, oldApproval, oldYolo, oldAuto := askAgent, askApproval, askYolo, askAuto
	oldText, oldPorcelain, oldProgressive, oldStopWhen := askText, askPorcelain, askProgressive, askStopWhen
	t.Cleanup(func() {
		askAgent, askApproval, askYolo, askAuto = oldAgent, oldApproval, oldYolo, oldAuto
		askText, askPorcelain, askProgressive, askStopWhen = oldText, oldPorcelain, oldProgressive, oldStopWhen
	})
	askAgent, askApproval, askYolo, askAuto = "", "prompt", false, false
	askText, askPorcelain = false, true
	askProgressive, askStopWhen = true, "not-a-valid-stop-condition"
	cmd := &cobra.Command{}
	if err := runAsk(cmd, []string{"@temporary", "question"}); err == nil {
		t.Fatal("expected validation error")
	}
	if askAgent != "" || askApproval != "prompt" || askYolo || askAuto || askText || !askPorcelain {
		t.Fatalf("invocation globals leaked: agent=%q approval=%q yolo=%v auto=%v text=%v porcelain=%v", askAgent, askApproval, askYolo, askAuto, askText, askPorcelain)
	}
}

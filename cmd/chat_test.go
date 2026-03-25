package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestBuildChatHandoverApprovalManager_SeedsShellPolicy(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.Enabled = []string{"read_file"}
	cfg.Tools.ShellAllow = []string{"git *"}

	mgr, err := buildChatHandoverApprovalManager(cfg, SessionSettings{
		ShellAllow: []string{"go test *"},
		Scripts:    []string{"./handover.sh"},
	})
	if err != nil {
		t.Fatalf("buildChatHandoverApprovalManager() error = %v", err)
	}

	cases := []struct {
		command string
		want    tools.ConfirmOutcome
	}{
		{command: "git status", want: tools.ProceedOnce},
		{command: "go test ./...", want: tools.ProceedOnce},
		{command: "./handover.sh", want: tools.ProceedOnce},
	}

	for _, tc := range cases {
		got, err := mgr.CheckShellApproval(tc.command, "")
		if err != nil {
			t.Fatalf("CheckShellApproval(%q) error = %v", tc.command, err)
		}
		if got != tc.want {
			t.Fatalf("CheckShellApproval(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}

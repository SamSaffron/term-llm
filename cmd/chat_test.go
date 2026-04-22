package cmd

import (
	"errors"
	"os"
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

func TestBuildChatProgramInput_AutoSendDisablesInput(t *testing.T) {
	got, err := buildChatProgramInput(true)
	if err != nil {
		t.Fatalf("buildChatProgramInput(true) error = %v", err)
	}
	if !got.disableInput {
		t.Fatal("buildChatProgramInput(true) should disable input")
	}
	if got.reader != nil {
		t.Fatalf("buildChatProgramInput(true) reader = %v, want nil", got.reader)
	}
	if got.cleanup == nil {
		t.Fatal("buildChatProgramInput(true) cleanup should not be nil")
	}
}

func TestBuildChatProgramInput_InteractiveUsesTTYInput(t *testing.T) {
	origOpenTTY := chatOpenTTY
	defer func() {
		chatOpenTTY = origOpenTTY
	}()

	ttyIn, ttyInWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() input error = %v", err)
	}
	defer ttyInWriter.Close()

	ttyOutReader, ttyOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() output error = %v", err)
	}
	defer ttyOutReader.Close()

	chatOpenTTY = func() (*os.File, *os.File, error) {
		return ttyIn, ttyOut, nil
	}

	got, err := buildChatProgramInput(false)
	if err != nil {
		t.Fatalf("buildChatProgramInput(false) error = %v", err)
	}
	if got.disableInput {
		t.Fatal("buildChatProgramInput(false) should keep input enabled")
	}
	if got.reader != ttyIn {
		t.Fatalf("buildChatProgramInput(false) reader = %v, want %v", got.reader, ttyIn)
	}
	if got.cleanup == nil {
		t.Fatal("buildChatProgramInput(false) cleanup should not be nil")
	}

	got.cleanup()

	if err := ttyIn.Close(); err == nil {
		t.Fatal("expected tty input to be closed by cleanup")
	}
	if err := ttyOut.Close(); err == nil {
		t.Fatal("expected tty output to be closed by cleanup")
	}
}

func TestBuildChatProgramInput_InteractivePropagatesTTYError(t *testing.T) {
	origOpenTTY := chatOpenTTY
	defer func() {
		chatOpenTTY = origOpenTTY
	}()

	chatOpenTTY = func() (*os.File, *os.File, error) {
		return nil, nil, errors.New("boom")
	}

	_, err := buildChatProgramInput(false)
	if err == nil {
		t.Fatal("expected error when opening chat TTY fails")
	}
}

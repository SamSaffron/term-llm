package mcp

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/procutil"
)

func TestCommandTransportPipeSetupFailureClosesOwner(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*exec.Cmd)
	}{
		{name: "stdout pipe", setup: func(cmd *exec.Cmd) { cmd.Stdout = io.Discard }},
		{name: "stdin pipe", setup: func(cmd *exec.Cmd) { cmd.Stdin = strings.NewReader("") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			tc.setup(cmd)
			owned, err := procutil.PrepareCommand(cmd)
			if err != nil {
				t.Fatal(err)
			}
			_, err = (&commandTransport{Command: owned}).Connect(context.Background())
			if err == nil {
				t.Fatal("Connect succeeded with a pre-set pipe")
			}
			if err := owned.Start(); !errors.Is(err, procutil.ErrCommandClosed) {
				t.Fatalf("owner remained startable after pipe setup failure: %v", err)
			}
		})
	}
}

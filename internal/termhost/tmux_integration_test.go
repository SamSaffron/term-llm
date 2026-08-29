package termhost

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/lifecycle"
)

func TestCommandSinkTmuxEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	server := fmt.Sprintf("termhost-test-%d", os.Getpid())
	runTmux := func(args ...string) ([]byte, error) {
		return exec.Command("tmux", append([]string{"-L", server, "-f", "/dev/null"}, args...)...).CombinedOutput()
	}
	if output, err := runTmux("new-session", "-d", "-s", "termhost", "sleep", "30"); err != nil {
		t.Skipf("tmux cannot start a self-contained test server: %v (%s)", err, output)
	}
	t.Cleanup(func() { _, _ = runTmux("kill-server") })
	paneOutput, err := runTmux("display-message", "-p", "-t", "termhost", "#{pane_id}")
	if err != nil {
		t.Fatalf("resolve tmux pane: %v (%s)", err, paneOutput)
	}
	pane := strings.TrimSpace(string(paneOutput))
	t.Setenv("TERMHOST_TMUX_HELPER", "1")
	sink, err := newCommandSink(config.LifecycleCommandConfig{
		Name:    "tmux",
		Command: []string{os.Args[0], "-test.run=^TestTermhostTmuxBridgeHelper$", "--", server, pane},
	}, runCommand)
	if err != nil {
		t.Fatal(err)
	}

	state := lifecycle.NewEvent(lifecycle.KindState, 1, time.Now(), lifecycle.Metadata{Producer: "term-llm"}, lifecycle.Snapshot{State: lifecycle.Blocked})
	if err := sink.Send(context.Background(), state); err != nil {
		t.Fatalf("state sink: %v", err)
	}
	value, err := runTmux("show-options", "-p", "-v", "-t", pane, "@term_llm_lifecycle")
	if err != nil || strings.TrimSpace(string(value)) != "blocked" {
		t.Fatalf("tmux state = %q, err=%v", value, err)
	}

	release := lifecycle.NewEvent(lifecycle.KindRelease, 2, time.Now(), lifecycle.Metadata{Producer: "term-llm"}, lifecycle.Snapshot{})
	if err := sink.Send(context.Background(), release); err != nil {
		t.Fatalf("release sink: %v", err)
	}
	value, err = runTmux("show-options", "-p", "-v", "-t", pane, "@term_llm_lifecycle")
	if err != nil || strings.TrimSpace(string(value)) != "release" {
		t.Fatalf("tmux release = %q, err=%v", value, err)
	}
}

func TestTermhostTmuxBridgeHelper(t *testing.T) {
	if os.Getenv("TERMHOST_TMUX_HELPER") != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || len(os.Args) != separator+3 {
		t.Fatalf("helper args = %#v", os.Args)
	}
	server, pane := os.Args[separator+1], os.Args[separator+2]
	var event lifecycle.Event
	if err := json.NewDecoder(os.Stdin).Decode(&event); err != nil {
		t.Fatal(err)
	}
	value := string(event.State)
	if event.Kind == lifecycle.KindRelease {
		value = "release"
	}
	command := exec.Command("tmux", "-L", server, "-f", "/dev/null", "set-option", "-p", "-t", pane, "@term_llm_lifecycle", value)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("tmux helper: %v (%s)", err, output)
	}
}

package termhost

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/lifecycle"
)

func TestOSCPolicyAndStateMapping(t *testing.T) {
	rt := testRuntime(map[string]string{"TERM_PROGRAM": "ghostty"})
	if status := inspectOSC("off", rt); status.Enabled {
		t.Fatalf("off status = %#v", status)
	}
	if status := inspectOSC("auto", rt); !status.Enabled || !status.Detected {
		t.Fatalf("auto status = %#v", status)
	}
	controller := newOSCController(Status{}, rt)
	working := controller.control(lifecycle.Snapshot{State: lifecycle.Working})
	if working.Sequence != "\x1b]9;4;3\x07" || working.RefreshAfter == 0 {
		t.Fatalf("working control = %#v", working)
	}
	blocked := controller.control(lifecycle.Snapshot{State: lifecycle.Blocked})
	if blocked.Sequence != "\x1b]9;4;4;100\x07" || blocked.RefreshAfter == 0 {
		t.Fatalf("blocked control = %#v", blocked)
	}
	idle := controller.control(lifecycle.Snapshot{State: lifecycle.Idle})
	if idle.Sequence != "\x1b]9;4;0\x07" || idle.RefreshAfter != 0 {
		t.Fatalf("idle control = %#v", idle)
	}
	if repeat := controller.control(lifecycle.Snapshot{State: lifecycle.Idle}); repeat != (Control{}) {
		t.Fatalf("repeat idle = %#v", repeat)
	}
}

func TestOSCRestoreRepeatsClearWhenQuitCanBeatRawClear(t *testing.T) {
	controller := newOSCController(Status{}, testRuntime(nil))
	if control := controller.control(lifecycle.Snapshot{State: lifecycle.Working}); control.Sequence == "" {
		t.Fatal("working state did not claim OSC")
	}
	// Bubble Tea can receive Quit from the same update that returns this clear
	// and stop before rendering the tea.Raw command.
	if clear := controller.control(lifecycle.Snapshot{State: lifecycle.Idle}); clear.Sequence != "\x1b]9;4;0\x07" {
		t.Fatalf("raw clear = %#v", clear)
	}
	if restore := controller.restore(); restore != "\x1b]9;4;0\x07" {
		t.Fatalf("post-Run safety clear = %q", restore)
	}
	if restore := controller.restore(); restore != "" {
		t.Fatalf("second post-Run restore = %q", restore)
	}
}

func TestOSCTmuxPassthroughAndRestore(t *testing.T) {
	controller := newOSCController(Status{}, testRuntime(map[string]string{"TMUX": "/tmp/tmux"}))
	control := controller.control(lifecycle.Snapshot{State: lifecycle.Working})
	want := "\x1bPtmux;\x1b\x1b]9;4;3\x07\x1b\\"
	if control.Sequence != want {
		t.Fatalf("tmux sequence = %q, want %q", control.Sequence, want)
	}
	clear := controller.restore()
	if clear != "\x1bPtmux;\x1b\x1b]9;4;0\x07\x1b\\" {
		t.Fatalf("tmux clear = %q", clear)
	}
	if controller.restore() != "" {
		t.Fatal("restore emitted clear twice")
	}
}

func TestDeprecatedTerminalProgressEnablesOnlyAutoOSCCompatibility(t *testing.T) {
	rt := testRuntime(map[string]string{"TERM_PROGRAM": "ghostty"})
	cfg := config.LifecycleConfig{Enabled: true, Adapters: []string{}, OSC: "off"}
	_, report, err := discoverAll(cfg, true, rt)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OSC.Enabled || !strings.Contains(report.OSC.Reason, "deprecated chat.terminal_progress") {
		t.Fatalf("legacy OSC status = %#v", report.OSC)
	}
	_, report, err = discoverAll(cfg, false, rt)
	if err != nil {
		t.Fatal(err)
	}
	if report.OSC.Enabled {
		t.Fatalf("OSC enabled without compatibility setting: %#v", report.OSC)
	}
}

func TestOSCOnCanBeForcedAndAutoIsConservative(t *testing.T) {
	rt := testRuntime(nil)
	if status := inspectOSC("auto", rt); status.Enabled || status.Detected {
		t.Fatalf("auto without supported terminal = %#v", status)
	}
	if status := inspectOSC("on", rt); !status.Enabled || !strings.Contains(status.Reason, "explicitly") {
		t.Fatalf("on status = %#v", status)
	}
}

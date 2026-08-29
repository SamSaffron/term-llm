package termhost

import (
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/lifecycle"
)

const oscRefreshInterval = 5 * time.Second

type oscController struct {
	tmuxActive bool
	active     bool
	// owned stays set after a clear is returned to Bubble Tea. The renderer may
	// stop on a concurrent quit before writing that tea.Raw message, so only the
	// post-Run direct restore is allowed to disarm this safety net.
	owned bool
}

func inspectOSC(policy string, rt runtimeContext) Status {
	detected := isGhostty(rt.getenv)
	status := Status{Name: "osc", Type: "terminal", Detected: detected}
	switch policy {
	case "on":
		status.Enabled = true
		status.Reason = "explicitly enabled by lifecycle.osc=on"
	case "auto":
		status.Enabled = detected
		if detected {
			status.Reason = "explicit auto policy detected a Ghostty-compatible terminal"
		} else {
			status.Reason = "auto policy found no supported terminal"
		}
	default:
		status.Reason = "disabled"
	}
	return status
}

func newOSCController(_ Status, rt runtimeContext) *oscController {
	return &oscController{tmuxActive: strings.TrimSpace(rt.getenv("TMUX")) != ""}
}

func (o *oscController) control(snapshot lifecycle.Snapshot) Control {
	if o == nil {
		return Control{}
	}
	sequence := ""
	switch snapshot.State {
	case lifecycle.Working:
		o.active = true
		o.owned = true
		sequence = oscProgressSequence(3)
	case lifecycle.Blocked:
		o.active = true
		o.owned = true
		sequence = oscProgressSequence(4, 100)
	case lifecycle.Idle:
		if !o.active {
			return Control{}
		}
		o.active = false
		sequence = oscProgressSequence(0)
	}
	if o.tmuxActive {
		sequence = tmuxPassthroughSequence(sequence)
	}
	control := Control{Sequence: sequence}
	if o.active {
		control.RefreshAfter = oscRefreshInterval
	}
	return control
}

func (o *oscController) restore() string {
	if o == nil || !o.owned {
		return ""
	}
	o.active = false
	o.owned = false
	sequence := oscProgressSequence(0)
	if o.tmuxActive {
		sequence = tmuxPassthroughSequence(sequence)
	}
	return sequence
}

func isGhostty(getenv func(string) string) bool {
	if strings.EqualFold(strings.TrimSpace(getenv("TERM_PROGRAM")), "ghostty") {
		return true
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(getenv("TERM"))), "xterm-ghostty") {
		return true
	}
	return strings.TrimSpace(getenv("GHOSTTY_RESOURCES_DIR")) != ""
}

func oscProgressSequence(state int, value ...int) string {
	sequence := "\x1b]9;4;" + strconv.Itoa(state)
	if len(value) > 0 {
		sequence += ";" + strconv.Itoa(value[0])
	}
	return sequence + "\x07"
}

func tmuxPassthroughSequence(sequence string) string {
	if sequence == "" {
		return ""
	}
	return "\x1bPtmux;" + strings.ReplaceAll(sequence, "\x1b", "\x1b\x1b") + "\x1b\\"
}

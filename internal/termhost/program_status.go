package termhost

import (
	"encoding/base64"
	"strings"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/lifecycle"
)

// Herdr Core reports program status through its PTY, independently of the
// original Herdr TUI's CLI and the optional OSC 9;4 progress indicator.
func inspectProgramStatus(enabled, selected bool, rt runtimeContext) Status {
	status := Status{Name: "herdr-core", Type: "terminal"}
	switch {
	case !enabled:
		status.Reason = "lifecycle disabled; detection skipped"
	case !selected:
		status.Reason = "Herdr not selected by lifecycle.adapters; detection skipped"
	case strings.TrimSpace(rt.getenv("TERM_LLM_HERDR")) == "0":
		status.Reason = "disabled by TERM_LLM_HERDR=0"
	case strings.TrimSpace(rt.getenv("HC_TERMINAL_ID")) == "":
		status.Reason = "HC_TERMINAL_ID is not present"
	case strings.TrimSpace(rt.getenv("TMUX")) != "":
		status.Detected = true
		status.Reason = "nested tmux is not supported; run directly in a Herdr pane"
	default:
		status.Detected = true
		status.Enabled = true
		status.Reason = "Herdr Core terminal detected; using OSC 7501"
	}
	return status
}

type programStatusController struct {
	last  string
	owned bool
}

func (p *programStatusController) control(snapshot lifecycle.Snapshot) string {
	snapshot = lifecycle.NormalizeSnapshot(snapshot)
	if !lifecycle.ValidState(snapshot.State) {
		return ""
	}
	sequence := "\x1b]7501;state=" + string(snapshot.State) + ":app=term-llm"
	if snapshot.Title != "" {
		sequence += ":title=" + encodeProgramStatusText(snapshot.Title, 192)
	}
	if snapshot.Message != "" {
		sequence += ":msg=" + encodeProgramStatusText(snapshot.Message, 2048)
	}
	sequence += "\x1b\\"
	if sequence == p.last {
		return ""
	}
	p.last = sequence
	p.owned = true
	return sequence
}

func (p *programStatusController) restore() string {
	if !p.owned {
		return ""
	}
	p.owned = false
	p.last = ""
	return "\x1b]7501;state=clear\x1b\\"
}

func encodeProgramStatusText(value string, limit int) string {
	if len(value) > limit {
		value = value[:limit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return base64.StdEncoding.EncodeToString([]byte(value))
}

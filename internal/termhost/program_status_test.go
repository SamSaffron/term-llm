package termhost

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/lifecycle"
)

func TestProgramStatusDiscovery(t *testing.T) {
	for _, test := range []struct {
		name                    string
		env                     map[string]string
		enabled, selected, want bool
	}{
		{"native", map[string]string{"HC_TERMINAL_ID": "t1"}, true, true, true},
		{"ordinary terminal", nil, true, true, false},
		{"old Herdr", map[string]string{"HERDR_ENV": "1"}, true, true, false},
		{"global opt out", map[string]string{"HC_TERMINAL_ID": "t1"}, false, true, false},
		{"adapter not selected", map[string]string{"HC_TERMINAL_ID": "t1"}, true, false, false},
		{"Herdr opt out", map[string]string{"HC_TERMINAL_ID": "t1", "TERM_LLM_HERDR": "0"}, true, true, false},
		{"nested tmux", map[string]string{"HC_TERMINAL_ID": "t1", "TMUX": "/tmp/tmux"}, true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := inspectProgramStatus(test.enabled, test.selected, testRuntime(test.env))
			if status.Enabled != test.want || status.Reason == "" {
				t.Fatalf("status = %#v, want enabled=%v", status, test.want)
			}
		})
	}
}

func TestProgramStatusRespectsLifecyclePolicy(t *testing.T) {
	for _, test := range []struct {
		name     string
		adapters []string
		env      map[string]string
		want     bool
	}{
		{"auto", []string{"auto"}, map[string]string{"HC_TERMINAL_ID": "t1"}, true},
		{"explicit", []string{"herdr"}, map[string]string{"HC_TERMINAL_ID": "t1"}, true},
		{"empty", []string{}, map[string]string{"HC_TERMINAL_ID": "t1"}, false},
		{"other adapter", []string{"cmux"}, map[string]string{"HC_TERMINAL_ID": "t1"}, false},
		{"global env", []string{"auto"}, map[string]string{"HC_TERMINAL_ID": "t1", "TERM_LLM_LIFECYCLE": "0"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, report, err := discoverAll(config.LifecycleConfig{Enabled: true, Adapters: test.adapters, OSC: "off"}, false, testRuntime(test.env))
			if err != nil {
				t.Fatal(err)
			}
			if report.ProgramStatus.Enabled != test.want || report.OSC.Enabled {
				t.Fatalf("report = %#v", report)
			}
		})
	}
}

func TestProgramStatusStateAndMetadata(t *testing.T) {
	p := &programStatusController{}
	for _, state := range []lifecycle.State{lifecycle.Idle, lifecycle.Working, lifecycle.Blocked, lifecycle.Idle} {
		snapshot := lifecycle.Snapshot{State: state, Title: "Fix: tests", Message: "Waiting for approval"}
		want := "\x1b]7501;state=" + string(state) + ":app=term-llm:title=Rml4OiB0ZXN0cw=="
		if state != lifecycle.Idle {
			want += ":msg=V2FpdGluZyBmb3IgYXBwcm92YWw="
		}
		want += "\x1b\\"
		if got := p.control(snapshot); got != want {
			t.Fatalf("%s: got %q, want %q", state, got, want)
		}
		if got := p.control(snapshot); got != "" {
			t.Fatalf("unchanged report repeated: %q", got)
		}
	}
	if got := p.control(lifecycle.Snapshot{State: lifecycle.Idle}); got != "\x1b]7501;state=idle:app=term-llm\x1b\\" {
		t.Fatalf("titleless session = %q", got)
	}
	if got := p.restore(); got != "\x1b]7501;state=clear\x1b\\" {
		t.Fatalf("restore = %q", got)
	}
	if got := p.restore(); got != "" {
		t.Fatalf("second restore = %q", got)
	}
}

func TestProgramStatusTextSafetyAndLimits(t *testing.T) {
	p := &programStatusController{}
	sequence := p.control(lifecycle.Snapshot{State: lifecycle.Blocked, Title: strings.Repeat("界", 100), Message: "Wait\nfor\x1b\x07 approval"})
	body := strings.TrimSuffix(strings.TrimPrefix(sequence, "\x1b]7501;"), "\x1b\\")
	for _, pair := range strings.Split(body, ":") {
		key, value, _ := strings.Cut(pair, "=")
		if key != "title" && key != "msg" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil || !utf8.Valid(decoded) || strings.ContainsAny(string(decoded), "\n\x1b\x07") {
			t.Fatalf("unsafe %s = %q (%v)", key, decoded, err)
		}
		if key == "title" && len(decoded) != 192 {
			t.Fatalf("title length = %d", len(decoded))
		}
	}
	// Truncation must also handle a byte boundary in the middle of a rune.
	decoded, err := base64.StdEncoding.DecodeString(encodeProgramStatusText(strings.Repeat("界", 100), 191))
	if err != nil || !utf8.Valid(decoded) || len(decoded) > 191 {
		t.Fatalf("truncated UTF-8 = %q (%v)", decoded, err)
	}
}

func TestManagerProgramStatusOutputAndCleanup(t *testing.T) {
	t.Setenv("HC_TERMINAL_ID", "t1")
	t.Setenv("TMUX", "")
	t.Setenv("TERM_LLM_HERDR", "")
	t.Setenv("TERM_LLM_LIFECYCLE", "")
	t.Setenv("HERDR_ENV", "")
	manager, err := New(config.LifecycleConfig{Enabled: true, Adapters: []string{"herdr"}, OSC: "off"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	control := manager.Report(lifecycle.Snapshot{State: lifecycle.Idle})
	if control.Sequence != "\x1b]7501;state=idle:app=term-llm\x1b\\" || control.RefreshAfter != 0 {
		t.Fatalf("initial report = %#v", control)
	}
	manager.Report(lifecycle.Snapshot{State: lifecycle.Working})
	manager.Report(lifecycle.Snapshot{State: lifecycle.Idle})
	// Cleanup still clears ownership if Quit races with the final Raw report.
	if got := manager.RestoreOSC(); got != "\x1b]7501;state=clear\x1b\\" {
		t.Fatalf("cleanup = %q", got)
	}
}

func TestManagerProgramStatusCoexistsWithProgressOSC(t *testing.T) {
	rt := testRuntime(nil)
	manager := newManager(nil, newOSCController(Status{}, rt), rt, 0)
	manager.programStatus = &programStatusController{}
	defer manager.Close()
	control := manager.Report(lifecycle.Snapshot{State: lifecycle.Working})
	if control.Sequence != "\x1b]9;4;3\x07\x1b]7501;state=working:app=term-llm\x1b\\" || control.RefreshAfter == 0 {
		t.Fatalf("combined control = %#v", control)
	}
	if got := manager.RestoreOSC(); got != "\x1b]9;4;0\x07\x1b]7501;state=clear\x1b\\" {
		t.Fatalf("combined cleanup = %q", got)
	}
}

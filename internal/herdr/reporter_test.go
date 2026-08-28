package herdr

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

type recordedCommand struct {
	path string
	args []string
}

func TestNewReporterFromEnvRequiresHerdrPaneVariables(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "outside herdr", env: map[string]string{}},
		{name: "missing binary", env: map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": "w1:p1"}},
		{name: "missing pane", env: map[string]string{"HERDR_ENV": "1", "HERDR_BIN_PATH": "/usr/local/bin/herdr"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newReporterFromEnv(func(name string) string { return tt.env[name] }, nil, nil)
			if r != nil {
				t.Fatal("reporter should be disabled")
			}
		})
	}
}

func TestNewReporterFromEnvFindsHerdrOnPath(t *testing.T) {
	r := newReporterFromEnv(func(name string) string {
		return map[string]string{
			"HERDR_ENV":     "1",
			"HERDR_PANE_ID": "w1:p1",
		}[name]
	}, func(name string) (string, error) {
		if name != "herdr" {
			t.Fatalf("binary lookup = %q, want herdr", name)
		}
		return "/opt/herdr/bin/herdr", nil
	}, nil)
	if r == nil {
		t.Fatal("reporter should use herdr from PATH")
	}
	if r.binPath != "/opt/herdr/bin/herdr" {
		t.Fatalf("binary path = %q", r.binPath)
	}
	r.Close()
}

func TestReporterReportsOrderedLifecycleAndRelease(t *testing.T) {
	var mu sync.Mutex
	var commands []recordedCommand
	r := newReporterFromEnv(func(name string) string {
		return map[string]string{
			"HERDR_ENV":      "1",
			"HERDR_BIN_PATH": "/tmp/herdr",
			"HERDR_PANE_ID":  "w1:p2",
		}[name]
	}, nil, func(_ context.Context, path string, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		commands = append(commands, recordedCommand{path: path, args: append([]string(nil), args...)})
		return nil
	})

	r.Report("idle", "session-a")
	r.Report("idle", "session-a") // deduplicated
	r.Report("working", "session-a")
	r.Report("blocked", "session-a")
	r.Report("idle", "session-b") // session identity changed
	r.Close()

	mu.Lock()
	defer mu.Unlock()
	want := [][]string{
		{"pane", "report-agent", "w1:p2", "--source", source, "--agent", agent, "--state", "idle", "--agent-session-id", "session-a"},
		{"pane", "report-agent", "w1:p2", "--source", source, "--agent", agent, "--state", "working", "--agent-session-id", "session-a"},
		{"pane", "report-agent", "w1:p2", "--source", source, "--agent", agent, "--state", "blocked", "--agent-session-id", "session-a"},
		{"pane", "report-agent", "w1:p2", "--source", source, "--agent", agent, "--state", "idle", "--agent-session-id", "session-b"},
		{"pane", "release-agent", "w1:p2", "--source", source, "--agent", agent},
	}
	if len(commands) != len(want) {
		t.Fatalf("commands = %#v, want %d commands", commands, len(want))
	}
	for i, got := range commands {
		if got.path != "/tmp/herdr" || !reflect.DeepEqual(got.args, want[i]) {
			t.Errorf("command %d = %#v, want path %q args %#v", i, got, "/tmp/herdr", want[i])
		}
	}
}

func TestReporterIgnoresUnknownLifecycleState(t *testing.T) {
	called := false
	r := newReporterFromEnv(func(name string) string {
		return map[string]string{
			"HERDR_ENV":      "1",
			"HERDR_BIN_PATH": "/tmp/herdr",
			"HERDR_PANE_ID":  "w1:p2",
		}[name]
	}, nil, func(context.Context, string, ...string) error {
		called = true
		return nil
	})
	r.Report("unknown", "session-a")
	r.Close()
	if called {
		t.Fatal("invalid state should not invoke Herdr")
	}
}

func TestReporterDoesNotBlockWhileHerdrCommandIsRunning(t *testing.T) {
	commandStarted := make(chan struct{})
	unblockCommand := make(chan struct{})
	var once sync.Once
	r := newReporterFromEnv(func(name string) string {
		return map[string]string{
			"HERDR_ENV":      "1",
			"HERDR_BIN_PATH": "/tmp/herdr",
			"HERDR_PANE_ID":  "w1:p2",
		}[name]
	}, nil, func(context.Context, string, ...string) error {
		once.Do(func() { close(commandStarted) })
		<-unblockCommand
		return nil
	})

	r.Report("idle", "session-a")
	<-commandStarted
	reported := make(chan struct{})
	go func() {
		for i := 0; i < 32; i++ {
			if i%2 == 0 {
				r.Report("working", "session-a")
			} else {
				r.Report("idle", "session-a")
			}
		}
		close(reported)
	}()

	select {
	case <-reported:
	case <-time.After(time.Second):
		t.Fatal("Report blocked behind the Herdr command")
	}
	close(unblockCommand)
	r.Close()
}

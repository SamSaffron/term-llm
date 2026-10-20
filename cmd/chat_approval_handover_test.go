//go:build linux || darwin

package cmd

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// Run the real Cobra capture and application in a separate terminal process.
// No replacement resolver or runtime hook can hide a capture/precedence bug.
func TestChatApprovalHandoverProcess(t *testing.T) {
	state := os.Getenv("TERM_LLM_TEST_APPROVAL_HANDOVER")
	if state == "" {
		return
	}
	// TestMain's procenv isolation installs its own data/cache directories.
	// Point this child back at the parent's private, observable fixture.
	t.Setenv("XDG_DATA_HOME", filepath.Join(state, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(state, "cache"))
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		t.Fatal("missing chat flags")
	}
	if err := chatCmd.ParseFlags(args[1:]); err != nil {
		t.Fatal(err)
	}
	if err := runChat(chatCmd, nil); err != nil {
		t.Fatal(err)
	}
}

func TestChatApprovalActualHandover(t *testing.T) {
	for _, tt := range []struct {
		name, explicit, initial, selected string
		toggles                           int
	}{
		{"explicit yolo to prompt", "yolo", "yolo", "prompt", 1},
		{"explicit prompt to yolo", "prompt", "prompt", "yolo", 2},
		{"no explicit flag", "", "auto", "yolo", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state := t.TempDir()
			for _, sub := range []string{"home", "config/term-llm", "data", "cache", "tmp", "workspace", "agents/source", "agents/target"} {
				if err := os.MkdirAll(filepath.Join(state, sub), 0700); err != nil {
					t.Fatal(err)
				}
			}
			cfg := fmt.Sprintf(`default_provider: debug
providers:
  debug:
    model: normal
sessions:
  enabled: true
skills:
  enabled: false
lifecycle:
  enabled: false
chat:
  approval_mode: auto
  terminal_title: off
agents:
  use_builtin: false
  search_paths: [%q]
`, filepath.Join(state, "agents"))
			if err := os.WriteFile(filepath.Join(state, "config/term-llm/config.yaml"), []byte(cfg), 0600); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"source", "target"} {
				definition := "name: " + name + "\ndescription: isolated approval probe\nhandover_mode: light\ntools:\n  enabled: []\n"
				if err := os.WriteFile(filepath.Join(state, "agents", name, "agent.yaml"), []byte(definition), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(state, "agents", name, "system.md"), []byte("Use the debug provider."), 0600); err != nil {
					t.Fatal(err)
				}
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"-test.run=^TestChatApprovalHandoverProcess$", "--", "--text", "--no-search", "--debug", "--agent=source"}
			if tt.explicit != "" {
				args = append(args, "--approval="+tt.explicit)
			}
			command := exec.Command(executable, args...)
			command.Dir = filepath.Join(state, "workspace")
			command.Env = append(os.Environ(), "TERM_LLM_TEST_APPROVAL_HANDOVER="+state, "CI=", "TERM=xterm-256color", "NO_COLOR=1",
				"HOME="+filepath.Join(state, "home"), "XDG_CONFIG_HOME="+filepath.Join(state, "config"),
				"XDG_DATA_HOME="+filepath.Join(state, "data"), "XDG_CACHE_HOME="+filepath.Join(state, "cache"), "TMPDIR="+filepath.Join(state, "tmp"))
			terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 36, Cols: 150})
			if err != nil {
				t.Fatal(err)
			}
			defer terminal.Close()
			var mu sync.Mutex
			var output bytes.Buffer
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				buf := make([]byte, 65536)
				for {
					n, err := terminal.Read(buf)
					mu.Lock()
					output.Write(buf[:n])
					mu.Unlock()
					if bytes.Contains(buf[:n], []byte("\x1b[6n")) {
						_, _ = io.WriteString(terminal, "\x1b[1;1R")
					}
					if err != nil {
						return
					}
				}
			}()
			text := func() string { mu.Lock(); defer mu.Unlock(); return output.String() }
			exited := make(chan error, 1)
			go func() { exited <- command.Wait() }()
			finished := false
			defer func() {
				if !finished {
					_ = command.Process.Kill()
					<-exited
				}
				_ = terminal.Close()
				<-readDone
				if t.Failed() {
					t.Logf("PTY transcript:\n%s", text())
				}
			}()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			deadline := time.NewTimer(30 * time.Second)
			defer deadline.Stop()
			wait := func(label string, ready func() bool) {
				t.Helper()
				for !ready() {
					select {
					case <-ticker.C:
					case err := <-exited:
						finished = true
						t.Fatalf("exited before %s: %v", label, err)
					case <-deadline.C:
						t.Fatalf("timeout waiting for %s", label)
					}
				}
			}
			send := func(s string) {
				t.Helper()
				if _, err := io.WriteString(terminal, s); err != nil {
					t.Fatal(err)
				}
			}
			// Query durable session state, not guessed redraw timings.
			dbPath := filepath.Join(state, "data/term-llm/sessions.db")
			var db *sql.DB
			defer func() {
				if db != nil {
					_ = db.Close()
				}
			}()
			var lastQuery string
			defer func() {
				if t.Failed() {
					t.Logf("last session query: %s", lastQuery)
				}
			}()
			policy := func(agent string, completed bool) string {
				if db == nil {
					if _, err := os.Stat(dbPath); err != nil {
						return ""
					}
					db, err = sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
					if err != nil {
						t.Fatal(err)
					}
				}
				var mode string
				var users, llms int
				if err := db.QueryRow("SELECT approval_mode, user_turns, llm_turns FROM sessions WHERE agent = ?", agent).Scan(&mode, &users, &llms); err != nil {
					lastQuery = err.Error()
					return ""
				}
				lastQuery = fmt.Sprintf("%s %s %d %d", agent, mode, users, llms)
				if completed && (users < 1 || llms < 1) {
					return ""
				}
				return mode
			}
			wait("initial input", func() bool { return strings.Contains(text(), "\x1b[?2004h") })
			send("markdown*30,markdown*10\r")
			wait("source first turn", func() bool { return policy("source", true) == tt.initial })
			for i := 0; i < tt.toggles; i++ {
				before := policy("source", false)
				send("\x1b[Z")
				wait("persisted toggle", func() bool { return policy("source", false) != before })
			}
			if got := policy("source", false); got != tt.selected {
				t.Fatalf("source selected=%q want=%q", got, tt.selected)
			}
			mark := len(text())
			send("/handover @target debug:normal\r")
			wait("handover preview", func() bool { return strings.Contains(text()[mark:], "Confirm handover") })
			send("\r")
			wait("target first turn", func() bool { return policy("target", true) != "" })
			if got := policy("target", true); got != tt.selected {
				t.Errorf("persisted target approval=%q want=%q", got, tt.selected)
			}
			// Report uses the target's constructed ApprovalManager, not just the
			// requested mode or source session metadata.
			wantEffective := "approval: requested=" + tt.selected + " source=cli actual=" + tt.selected
			if !strings.Contains(text()[mark:], wantEffective) {
				t.Errorf("target effective approval report missing %q", wantEffective)
			}
			send("/quit\r")
			select {
			case err := <-exited:
				finished = true
				if err != nil {
					t.Fatal(err)
				}
			case <-deadline.C:
				t.Fatal("timeout quitting target")
			}
		})
	}
}

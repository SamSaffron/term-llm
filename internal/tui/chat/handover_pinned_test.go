package chat

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

// TestCmdHandoverFileModeUsesSessionPromptPath ensures /handover reads the
// exact file this session's agent was told about via {{handover_path}} (as
// recorded in its persisted system prompt), even when another .md in the
// handover directory — e.g. from a concurrent session — has a newer
// modification time.
func TestCmdHandoverFileModeUsesSessionPromptPath(t *testing.T) {
	tmp := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "xdg-data"))

	planPath, err := session.GetHandoverPath(".", time.Now().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("GetHandoverPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(planPath, []byte("the session plan"), 0o644); err != nil {
		t.Fatalf("WriteFile plan: %v", err)
	}

	// A concurrent session's document with a newer mtime must not shadow it.
	decoy := filepath.Join(filepath.Dir(planPath), "2026-01-01-other-session-plan.md")
	if err := os.WriteFile(decoy, []byte("someone else's plan"), 0o644); err != nil {
		t.Fatalf("WriteFile decoy: %v", err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(decoy, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	m := newTestChatModel(false)
	m.store = &mockStore{}
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "prompt-path-handover", CreatedAt: time.Now().Add(-time.Minute)}
	m.agentName = "planner"
	m.currentAgent = &agents.Agent{Name: "planner", EnableHandover: true, HandoverMode: "file"}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return &agents.Agent{Name: name, SystemPrompt: "You are target."}, nil
	}
	sysPrompt := "You are a planner. Maintain your plan in `" + planPath + "`."
	m.messages = []session.Message{
		*session.NewMessage(m.sess.ID, llm.SystemText(sysPrompt), -1),
	}

	_, cmd := m.cmdHandover([]string{"@developer"})
	if cmd == nil {
		t.Fatal("expected handover command")
	}
	msg := cmd()
	done, ok := msg.(handoverDoneMsg)
	if !ok {
		t.Fatalf("handover command returned %T, want handoverDoneMsg", msg)
	}
	if done.result == nil {
		t.Fatal("expected handover result")
	}
	if done.result.Document != "the session plan" {
		t.Fatalf("handover used wrong document: %q", done.result.Document)
	}
}

// TestCmdHandoverFileModeIgnoresDecoyWhenPinnedUnwritten ensures that when
// the prompt names a pinned plan file that has not been written yet, file
// mode does NOT fall back to a newer .md from a concurrent session.
func TestCmdHandoverFileModeIgnoresDecoyWhenPinnedUnwritten(t *testing.T) {
	tmp := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "xdg-data"))

	planPath, err := session.GetHandoverPath(".", time.Now().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("GetHandoverPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// The pinned plan is never written; only a concurrent session's doc exists.
	decoy := filepath.Join(filepath.Dir(planPath), "2026-01-01-other-session-plan.md")
	if err := os.WriteFile(decoy, []byte("someone else's plan"), 0o644); err != nil {
		t.Fatalf("WriteFile decoy: %v", err)
	}

	m := newTestChatModel(false)
	m.store = &mockStore{}
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "unwritten-pinned", CreatedAt: time.Now().Add(-time.Minute)}
	m.agentName = "planner"
	m.currentAgent = &agents.Agent{Name: "planner", EnableHandover: true, HandoverMode: "file"}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return &agents.Agent{Name: name, SystemPrompt: "You are target."}, nil
	}
	sysPrompt := "You are a planner. Maintain your plan in `" + planPath + "`."
	m.messages = []session.Message{
		*session.NewMessage(m.sess.ID, llm.SystemText(sysPrompt), -1),
	}

	_, cmd := m.cmdHandover([]string{"@developer"})
	if cmd != nil {
		if done, ok := cmd().(handoverDoneMsg); ok {
			t.Fatalf("handover must not proceed with another session's document: %q", done.result.Document)
		}
	}
}

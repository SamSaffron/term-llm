package worktrees

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/worktree"
)

func testModel() *Model {
	m := New("/repo", nil, "/repo/wt-2", 100, 20, nil)
	m.SetItems([]worktree.Worktree{
		{Name: "one", Dir: "/repo/wt-1", Branch: "feature/one", Status: worktree.StatusReady},
		{Name: "two", Dir: "/repo/wt-2", HeadSHA: "1234567890", Detached: true, DirtyFiles: 2, Status: worktree.StatusReady},
		{Name: "three", Dir: "/repo/wt-3", Status: worktree.StatusReady},
	})
	return m
}

func press(m *Model, code rune, text string) tea.Cmd {
	_, cmd := m.Update(tea.KeyPressMsg{Code: code, Text: text})
	return cmd
}

func TestBrowseNavigationClampsAndSupportsJK(t *testing.T) {
	m := testModel()
	press(m, 'j', "j")
	if m.cursor != 2 { // preferred current binding starts at row 1
		t.Fatalf("down cursor = %d, want 2", m.cursor)
	}
	press(m, 'j', "j")
	if m.cursor != 2 {
		t.Fatalf("cursor did not clamp: %d", m.cursor)
	}
	press(m, 'k', "k")
	if m.cursor != 1 {
		t.Fatalf("up cursor = %d, want 1", m.cursor)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	if m.cursor != 0 {
		t.Fatalf("home cursor = %d", m.cursor)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if m.cursor != 2 {
		t.Fatalf("end cursor = %d", m.cursor)
	}
}

func TestEmptyBrowseActionsAreSafeAndCloseEmitsMessage(t *testing.T) {
	m := New("/repo", nil, "", 20, 5, nil)
	for _, key := range []rune{'j', 'k', 'i', 'd'} {
		press(m, key, string(key))
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.cursor != 0 {
		t.Fatalf("empty cursor = %d", m.cursor)
	}
	cmd := press(m, 'q', "q")
	if _, ok := cmd().(CloseMsg); !ok {
		t.Fatalf("quit emitted %T, want CloseMsg", cmd())
	}
}

func TestBrowseActionMessages(t *testing.T) {
	m := testModel()
	cmd := mKey(m, tea.KeyEnter)
	open, ok := cmd().(OpenMsg)
	if !ok || open.Worktree.Name != "two" {
		t.Fatalf("open = %#v", cmd())
	}

	press(m, 'd', "d")
	cmd = press(m, 'y', "y")
	remove, ok := cmd().(RemoveMsg)
	if !ok || remove.Force {
		t.Fatalf("remove = %#v", cmd())
	}

	m.EscalateRemove([]string{"dirty changes"})
	cmd = press(m, 'y', "y")
	remove, ok = cmd().(RemoveMsg)
	if !ok || !remove.Force {
		t.Fatalf("force remove = %#v", cmd())
	}
}

func TestDeleteConfirmationRetainsTargetAcrossRefresh(t *testing.T) {
	m := testModel()
	press(m, 'd', "d")
	m.SetItems([]worktree.Worktree{{Name: "replacement", Dir: "/repo/replacement"}})
	cmd := press(m, 'y', "y")
	remove, ok := cmd().(RemoveMsg)
	if !ok || remove.Worktree.Name != "two" {
		t.Fatalf("remove after refresh = %#v, want original target two", cmd())
	}

	m = testModel()
	press(m, 'd', "d")
	m.EscalateRemove([]string{"dirty changes"})
	m.refreshing = true
	m.status = "Refreshing…"
	m.SetItems([]worktree.Worktree{{Name: "replacement", Dir: "/repo/replacement"}})
	if out := m.View().Content; !strings.Contains(out, "FORCE REMOVE?") || strings.Contains(out, "Refreshing…") {
		t.Fatalf("force prompt hidden by refresh: %q", out)
	}
	cmd = press(m, 'y', "y")
	remove, ok = cmd().(RemoveMsg)
	if !ok || !remove.Force || remove.Worktree.Name != "two" {
		t.Fatalf("force remove after refresh = %#v, want original target two", cmd())
	}
}

func TestCreateFormDefaultsFocusAndSubmission(t *testing.T) {
	m := testModel()
	press(m, 'n', "n")
	if m.mode != modeCreate || m.inputs[1].Value() != "HEAD" || m.inputFocus != 0 {
		t.Fatalf("create defaults mode=%v base=%q focus=%d", m.mode, m.inputs[1].Value(), m.inputFocus)
	}
	press(m, 'x', "x")
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.inputFocus != 1 {
		t.Fatalf("tab focus = %d", m.inputFocus)
	}
	cmd := mKey(m, tea.KeyEnter)
	created, ok := cmd().(CreateMsg)
	if !ok || created.Options.Name != "x" || created.Options.Base != "HEAD" || !created.Options.MoveChanges {
		t.Fatalf("create = %#v", cmd())
	}
}

func TestDetailsRendersMetadataSessionsAndDiffStates(t *testing.T) {
	m := testModel()
	_, _ = m.openDetails()
	m.Update(inUseResultMsg{dir: "/repo/wt-2", generation: m.detailGeneration, sessions: []worktree.InUseSession{{Number: 7, Name: "chat", Status: "active"}}})
	m.Update(diffResultMsg{dir: "/repo/wt-2", generation: m.detailGeneration, diff: "+changed"})
	out := m.View().Content
	for _, want := range []string{"Worktree Details", "Name: two", "#7 chat [active]", "+changed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("details missing %q in %q", want, out)
		}
	}

	m.Update(diffResultMsg{dir: "/repo/wt-2", generation: m.detailGeneration, diff: ""})
	if !strings.Contains(m.View().Content, "Clean (no changes)") {
		t.Fatal("clean detail state not rendered")
	}
	m.Update(diffResultMsg{dir: "/repo/wt-2", generation: m.detailGeneration, err: errors.New("diff failed")})
	if !strings.Contains(m.View().Content, "diff failed") {
		t.Fatal("diff error not rendered")
	}
}

func TestRefreshPreservesSelectionAndClamps(t *testing.T) {
	m := testModel()
	m.cursor = 2
	m.SetItems([]worktree.Worktree{{Name: "three", Dir: "/repo/wt-3"}, {Name: "four", Dir: "/repo/wt-4"}})
	if m.cursor != 0 {
		t.Fatalf("selection was not preserved: %d", m.cursor)
	}
	m.SetItems(nil)
	if m.cursor != 0 {
		t.Fatalf("empty refresh cursor = %d", m.cursor)
	}
}

func TestNarrowUnicodeRenderIsValidUTF8(t *testing.T) {
	m := New("/repo", nil, "", 9, 5, nil)
	m.SetItems([]worktree.Worktree{{Name: "機能🌳-非常に長い", Dir: "/tmp/機能", Branch: "分支"}})
	out := m.View().Content
	if !utf8.ValidString(out) {
		t.Fatalf("view is invalid UTF-8: %q", out)
	}
}

func mKey(m *Model, code rune) tea.Cmd {
	_, cmd := m.Update(tea.KeyPressMsg{Code: code})
	return cmd
}

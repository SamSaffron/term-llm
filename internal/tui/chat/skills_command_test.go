package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/skills"
)

func skillsCommandTestModel(t *testing.T, altScreen bool) *Model {
	t.Helper()
	registry := chatTestSkillRegistry(t, map[string]string{
		"explain": "---\nname: explain\ndescription: Explain a package\nargument-hint: <path>\n---\nExplain $ARGUMENTS.\n",
		"deploy":  "---\nname: deploy\ndescription: Deploy the app\n---\nDeploy.\n",
		"review":  "---\nname: review\ndescription: Review changes\nargument-hint: \"[scope]\"\ncontext: fork\nagent: reviewer\n---\nReview $ARGUMENTS.\n",
		"compact": "---\nname: compact\ndescription: Collides with built-in\n---\nBody\n",
		"broken":  "---\nname: broken\ndescription: Broken metadata\ncontext: sideways\n---\nBody\n",
	})
	m := newTestChatModel(altScreen)
	m.SetSkillsSetup(&skills.Setup{Registry: registry})
	m.SetChildRunner(&fakeSkillChildRunner{})
	return m
}

func typeComposerKeys(m *Model, text string) *Model {
	for _, r := range text {
		msg := tea.KeyPressMsg{Code: r, Text: string(r)}
		if r == ' ' {
			msg = tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
		}
		updated, _ := m.Update(msg)
		m = updated.(*Model)
	}
	return m
}

func pressEnter(m *Model) *Model {
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	return updated.(*Model)
}

func lastUserText(m *Model) string {
	for i := len(m.messages) - 1; i >= 0; i-- {
		if m.messages[i].Role == core.RoleUser {
			return m.messages[i].TextContent
		}
	}
	return ""
}

// tea.Println output is dropped on the alternate screen, so /skills listings
// must use the content dialog to be visible at all.
func TestSkillsListingsOpenContentDialogOnAltScreen(t *testing.T) {
	m := skillsCommandTestModel(t, true)
	for _, input := range []string{"/skills", "/skill", "/sk list", "/skills LIST"} {
		m.dialog.Close()
		m.setTextareaValue(input)
		updated, _ := m.ExecuteCommand(input)
		m = updated.(*Model)
		if !m.dialog.IsOpen() || m.dialog.Type() != DialogContent {
			t.Fatalf("%s did not open the skills dialog", input)
		}
		content := m.dialog.Content()
		for _, want := range []string{"/explain <path>", "/review [scope]", "isolated @reviewer", "/skills run compact", "broken", "sideways"} {
			if !strings.Contains(content, want) {
				t.Fatalf("%s listing missing %q:\n%s", input, want, content)
			}
		}
		if got := m.textarea.Value(); got != "" {
			t.Fatalf("%s left composer text %q", input, got)
		}
	}

	m.dialog.Close()
	updated, _ := m.ExecuteCommand("/skills show Explain")
	m = updated.(*Model)
	if content := m.dialog.Content(); m.dialog.Type() != DialogContent || !strings.Contains(content, "/explain <path>") || !strings.Contains(content, "Explain a package") {
		t.Fatalf("/skills show dialog = %v %q", m.dialog.Type(), content)
	}

	m.dialog.Close()
	m.setTextareaValue("/skills active")
	updated, _ = m.ExecuteCommand("/skills active")
	m = updated.(*Model)
	if m.dialog.IsOpen() || !strings.Contains(m.footerMessage, "No active isolated skill runs") || m.textarea.Value() != "" {
		t.Fatalf("empty /skills active: dialog=%v footer=%q composer=%q", m.dialog.IsOpen(), m.footerMessage, m.textarea.Value())
	}
	m.skillRuns = map[string]*skillRunState{"skill-7": {ID: "skill-7", Name: "review", Agent: "reviewer", Status: "running", StartedAt: time.Now()}}
	updated, _ = m.ExecuteCommand("/skills active")
	m = updated.(*Model)
	if content := m.dialog.Content(); m.dialog.Type() != DialogContent || !strings.Contains(content, "skill-7") || !strings.Contains(content, "/review") {
		t.Fatalf("/skills active dialog = %v %q", m.dialog.Type(), content)
	}
}

func TestSkillsShorthandInvokesNamedSkill(t *testing.T) {
	m := skillsCommandTestModel(t, false)
	updated, cmd := m.ExecuteCommand("/skill explain internal/config")
	m = updated.(*Model)
	if cmd == nil || !m.streaming {
		t.Fatalf("/skill <name> did not start the skill: cmd=%v streaming=%v footer=%q", cmd != nil, m.streaming, m.footerMessage)
	}
	if got := lastUserText(m); got != "/explain internal/config" {
		t.Fatalf("visible invocation = %q", got)
	}

	m = skillsCommandTestModel(t, false)
	m.setTextareaValue("/skills nope")
	updated, _ = m.ExecuteCommand("/skills nope")
	m = updated.(*Model)
	if !strings.Contains(m.footerMessage, `Unknown skill "nope"`) || m.textarea.Value() != "/skills nope" {
		t.Fatalf("unknown shorthand footer=%q composer=%q", m.footerMessage, m.textarea.Value())
	}
}

// While the parent streams, /skills input must run locally (or queue a
// main-context skill) instead of being delivered to the model as steering.
func TestStreamingSkillsCommandsDoNotBecomeSteering(t *testing.T) {
	m := skillsCommandTestModel(t, false)
	m.streaming = true
	m.phase = "Thinking"
	m.skillRuns = map[string]*skillRunState{"skill-1": {ID: "skill-1", Name: "review", Agent: "reviewer", Status: "running", StartedAt: time.Now(), Cancel: func() {}}}
	for _, tt := range []struct{ input, footer string }{
		{"/skills cancel skill-1", "Cancelling isolated skill run skill-1"},
		{"/skills run explain internal/config", "Queued /skills run explain internal/config"},
		{"/skill explain lifecycle", "Queued /explain lifecycle"},
		{"/skills active", "No active isolated skill runs"},
	} {
		if tt.input == "/skills active" {
			m.skillRuns = nil
		}
		m.setTextareaValue(tt.input)
		m = pressEnter(m)
		if len(m.pendingSteering) != 0 {
			t.Fatalf("%s was queued as steering: %#v", tt.input, m.pendingSteering)
		}
		if !strings.Contains(m.footerMessage, tt.footer) || m.textarea.Value() != "" {
			t.Fatalf("%s footer=%q composer=%q", tt.input, m.footerMessage, m.textarea.Value())
		}
	}
	if len(m.queuedMainSkillActivations) != 2 || !m.streaming {
		t.Fatalf("queued main skills = %d streaming=%v", len(m.queuedMainSkillActivations), m.streaming)
	}
	if names := completionNames(m.filterStreamingSlashEntries("sk")); !containsString(names, "skills") {
		t.Fatalf("streaming completions omit /skills: %v", names)
	}
}

func TestSlashCompletionsFollowEveryComposerEdit(t *testing.T) {
	m := skillsCommandTestModel(t, false)
	m = typeComposerKeys(m, "/skills ")
	if names := completionNames(m.completions.filtered); !m.completions.IsVisible() || !containsString(names, "skills list") {
		t.Fatalf("space after /skills did not show subcommands: visible=%v %v", m.completions.IsVisible(), names)
	}
	m = typeComposerKeys(m, "run ")
	if names := completionNames(m.completions.filtered); !containsString(names, "skills run explain") {
		t.Fatalf("/skills run did not list skills: %v", names)
	}

	// Clearing the line must close the popup; otherwise plain text typed next
	// is matched as a slash query and Enter runs a skill nobody asked for.
	updated, _ := m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	m = updated.(*Model)
	if m.completions.IsVisible() {
		t.Fatal("clearing the composer left the slash popup open")
	}
	m = pressEnter(typeComposerKeys(m, "rev"))
	if len(m.skillRuns) != 0 || lastUserText(m) != "rev" {
		t.Fatalf("plain text ran a skill: runs=%d user=%q", len(m.skillRuns), lastUserText(m))
	}
}

func TestEnterCompletesPartialSkillThatTakesArguments(t *testing.T) {
	m := pressEnter(typeComposerKeys(skillsCommandTestModel(t, false), "/expl"))
	if got := m.textarea.Value(); got != "/explain " || m.streaming {
		t.Fatalf("Enter on partial /expl: composer=%q streaming=%v", got, m.streaming)
	}
	m = pressEnter(typeComposerKeys(m, "internal/config"))
	if !m.streaming || lastUserText(m) != "/explain internal/config" {
		t.Fatalf("completed skill did not run with arguments: streaming=%v user=%q", m.streaming, lastUserText(m))
	}

	m = pressEnter(typeComposerKeys(skillsCommandTestModel(t, false), "/dep"))
	if !m.streaming || lastUserText(m) != "/deploy" {
		t.Fatalf("Enter on argument-free skill did not run it: streaming=%v user=%q", m.streaming, lastUserText(m))
	}
}

// The skill's hidden instructions are written only once the turn is claimed,
// directly before the user row, so /undo can remove them with the turn.
func TestDirectSkillInstructionsPrecedeTheirUserRow(t *testing.T) {
	m := skillsCommandTestModel(t, false)
	m.messages = append(m.messages, *session.NewMessage(m.sess.ID, core.AssistantText("earlier"), -1))
	m.pendingModelSwitch = &core.ModelSwapMarker{FromProvider: "a", FromModel: "one", ToProvider: "b", ToModel: "two", Status: "started"}
	updated, _ := m.ExecuteCommand("/explain internal/config")
	m = updated.(*Model)
	var roles []core.Role
	for _, message := range m.messages {
		roles = append(roles, message.Role)
	}
	if n := len(roles); !m.streaming || n < 3 || roles[n-3] != core.RoleEvent || roles[n-2] != core.RoleDeveloper || roles[n-1] != core.RoleUser {
		t.Fatalf("turn rows = %v (streaming=%v), want marker, skill instructions, user", roles, m.streaming)
	}
	if !core.IsDirectSkillActivation(m.messages[len(m.messages)-2].Role, m.messages[len(m.messages)-2].Parts) {
		t.Fatalf("row before the user turn is not the skill activation: %#v", m.messages[len(m.messages)-2])
	}
}

// A skill turn refused because another process owns the session must not leave
// the skill's instructions in the transcript or its tool filter on the engine.
func TestRefusedSkillTurnLeavesNoInstructionsOrToolFilter(t *testing.T) {
	m, store := newTurnLeaseTestModel(t)
	m.SetSkillsSetup(&skills.Setup{Registry: chatTestSkillRegistry(t, map[string]string{
		"locked": "---\nname: locked\ndescription: Restricts tools\nallowed-tools: []\n---\nWork on $ARGUMENTS.\n",
	})})
	admitForeignTurn(t, store, m.SessionID())

	updated, _ := m.ExecuteCommand("/locked internal/config")
	m = updated.(*Model)
	stored, err := store.GetMessages(context.Background(), m.SessionID(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.streaming || len(m.messages) != 1 || len(stored) != 1 || !strings.Contains(m.footerMessage, "another process") {
		t.Fatalf("refused turn: streaming=%v memory=%d stored=%d footer=%q", m.streaming, len(m.messages), len(stored), m.footerMessage)
	}
	if m.pendingSkillActivation != nil || m.skillFilterPending || !m.engine.IsToolAllowed("read_file") {
		t.Fatalf("refused turn leaked skill state: pending=%v filter=%v", m.pendingSkillActivation != nil, m.skillFilterPending)
	}
}

func TestCompletesBeforeRunning(t *testing.T) {
	explain := Command{Name: "explain", Kind: SlashEntrySkill, ArgumentHint: "<path>"}
	run := Command{Name: "skills run explain", Kind: SlashEntrySkill, ArgumentHint: "<path>"}
	for _, tt := range []struct {
		selected Command
		input    string
		want     bool
	}{
		{explain, "/expl", true},
		{explain, "/explain", false},
		{explain, "/Explain", false},
		{explain, "/explain internal/config", false},
		{run, "/skills run expl", true},
		{run, "/skills run explain", false},
		{Command{Name: "deploy", Kind: SlashEntrySkill}, "/dep", false},
		{Command{Name: "copy", ArgumentHint: "[N]"}, "/co", false},
	} {
		if got := completesBeforeRunning(tt.selected, tt.input); got != tt.want {
			t.Errorf("completesBeforeRunning(%q, %q) = %v, want %v", tt.selected.Name, tt.input, got, tt.want)
		}
	}
}

package cmd

import (
	"strings"
	"testing"
)

func TestInteractiveCommandsFailFastInCI(t *testing.T) {
	t.Setenv("CI", "1")
	t.Setenv("TERM", "xterm-256color")

	t.Run("config theme", func(t *testing.T) {
		err := configTheme(configThemeCmd, nil)
		if err == nil || !strings.Contains(err.Error(), "requires an interactive terminal") {
			t.Fatalf("configTheme() error = %v, want interactive-terminal error", err)
		}
	})

	t.Run("chat", func(t *testing.T) {
		oldAutoSend := chatAutoSend
		chatAutoSend = nil
		t.Cleanup(func() { chatAutoSend = oldAutoSend })

		err := runChat(chatCmd, nil)
		if err == nil || !strings.Contains(err.Error(), "requires an interactive terminal") {
			t.Fatalf("runChat() error = %v, want interactive-terminal error", err)
		}
	})

	t.Run("skills add", func(t *testing.T) {
		oldNoTUI, oldAll := skillsAddNoTUI, skillsAddAll
		skillsAddNoTUI, skillsAddAll = false, false
		t.Cleanup(func() {
			skillsAddNoTUI, skillsAddAll = oldNoTUI, oldAll
		})

		err := runSkillsAdd(skillsAddCmd, []string{"owner/repo"})
		if err == nil || !strings.Contains(err.Error(), "requires an interactive terminal") {
			t.Fatalf("runSkillsAdd() error = %v, want interactive-terminal error", err)
		}
	})
}

func TestBrowseCommandsUseCLIFallbackInCI(t *testing.T) {
	t.Setenv("CI", "1")
	oldMCPNoTUI, oldSkillsNoTUI := mcpBrowseTUI, skillsBrowseTUI
	mcpBrowseTUI, skillsBrowseTUI = false, false
	t.Cleanup(func() {
		mcpBrowseTUI, skillsBrowseTUI = oldMCPNoTUI, oldSkillsNoTUI
	})

	var mcpErr error
	mcpOutput := captureStdout(t, func() { mcpErr = mcpBrowse(mcpBrowseCmd, nil) })
	if mcpErr != nil || !strings.Contains(mcpOutput, "Curated MCP servers") {
		t.Fatalf("mcpBrowse() = (%q, %v), want CLI fallback", mcpOutput, mcpErr)
	}

	var skillsErr error
	skillsOutput := captureStdout(t, func() { skillsErr = runSkillsBrowse(skillsBrowseCmd, nil) })
	if skillsErr != nil || !strings.Contains(skillsOutput, "Usage: term-llm skills browse") {
		t.Fatalf("runSkillsBrowse() = (%q, %v), want CLI fallback", skillsOutput, skillsErr)
	}
}

func TestPromptForResetFailsClosedInCI(t *testing.T) {
	t.Setenv("CI", "1")
	if promptForReset() {
		t.Fatal("promptForReset() = true in CI, want false")
	}

	oldYes := sessionsResetYes
	sessionsResetYes = false
	t.Cleanup(func() { sessionsResetYes = oldYes })
	if err := runSessionsReset(sessionsResetCmd, nil); err == nil || !strings.Contains(err.Error(), "use --yes") {
		t.Fatalf("runSessionsReset() error = %v, want --yes guidance", err)
	}
}

func TestAskRichRendererDisabledInCI(t *testing.T) {
	oldText, oldDebugRaw := askText, debugRaw
	askText, debugRaw = false, false
	t.Cleanup(func() {
		askText, debugRaw = oldText, oldDebugRaw
	})
	t.Setenv("CI", "1")
	if shouldUseAskRichRenderer() {
		t.Fatal("shouldUseAskRichRenderer() = true in CI, want false")
	}
}

package chat

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestSkillSlashCompletionAndExactRecognition(t *testing.T) {
	registry := chatTestSkillRegistry(t, map[string]string{
		"review": `---
name: review
description: Review the working tree
argument-hint: "[scope]"
disable-model-invocation: true
context: fork
agent: reviewer
---
Review $ARGUMENTS.
`,
		"model-only": `---
name: model-only
description: Model only
user-invocable: false
---
Body
`,
		"compact": `---
name: compact
description: Collides with built-in
---
Skill compact
`,
	})
	m := newTestChatModel(false)
	m.SetSkillsSetup(&skills.Setup{Registry: registry})

	items := m.filterSlashEntries("")
	var gotSkillNames []string
	for _, item := range items {
		if item.Kind == SlashEntrySkill {
			gotSkillNames = append(gotSkillNames, item.Name)
			if item.Name == "review" {
				if item.ArgumentHint != "[scope]" || !strings.Contains(item.Description, "isolated") || !strings.Contains(item.Description, "skill") {
					t.Fatalf("review completion = %#v", item)
				}
			}
		}
	}
	seenSkills := make(map[string]bool)
	for _, name := range gotSkillNames {
		seenSkills[name] = true
	}
	if !seenSkills["review"] || seenSkills["compact"] || seenSkills["model-only"] {
		t.Fatalf("skill completion names = %#v", gotSkillNames)
	}

	if !m.isSlashCommandLike("/review internal/config") {
		t.Fatal("exact skill invocation was not recognized")
	}
	if !m.isSlashCommandLike("/Review internal/config") {
		t.Fatal("mixed-case skill invocation was not recognized")
	}
	if _, err := m.skillShowMarkdown("Review"); err != nil {
		t.Fatalf("mixed-case /skills show lookup failed: %v", err)
	}
	if m.isSlashCommandLike("/rev internal/config") {
		t.Fatal("skill prefix should not execute")
	}
	if m.isSlashCommandLike("/model-only") {
		t.Fatal("model-only skill should not be recognized for user invocation")
	}
	if m.isSlashCommandLike("/tmp/file") {
		t.Fatal("absolute path should remain ordinary prompt text")
	}

	streamingItems := m.filterStreamingSlashEntries("")
	var streamingSkillNames []string
	for _, item := range streamingItems {
		if item.Kind == SlashEntrySkill {
			streamingSkillNames = append(streamingSkillNames, item.Name)
		}
	}
	if !slices.Contains(streamingSkillNames, "review") {
		t.Fatalf("streaming skill completion names = %#v, want review", streamingSkillNames)
	}
}

func TestExecuteMainContextSkillPersistsConciseInvocationAndProvenance(t *testing.T) {
	registry := chatTestSkillRegistry(t, map[string]string{
		"explain": `---
name: explain
description: Explain a package
allowed-tools: []
---
Explain $ARGUMENTS[0]. Raw: $ARGUMENTS
`,
	})
	m := newTestChatModel(false)
	m.SetSkillsSetup(&skills.Setup{Registry: registry})
	m.setTextareaValue(`/explain "internal config" lifecycle`)

	updated, cmd := m.ExecuteCommand(m.textarea.Value())
	m = updated.(*Model)
	if cmd == nil || !m.streaming {
		t.Fatalf("main skill did not start normal stream: cmd=%v streaming=%v", cmd != nil, m.streaming)
	}
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("composer = %q, want cleared after successful activation", got)
	}

	var userText, developerText string
	var provenance *llm.SkillActivationProvenance
	for i := range m.messages {
		message := &m.messages[i]
		switch message.Role {
		case llm.RoleUser:
			userText = message.TextContent
		case llm.RoleDeveloper:
			developerText = message.TextContent
			for _, part := range message.Parts {
				if part.Type == llm.PartSkillActivation {
					provenance = part.SkillActivation
				}
			}
		}
	}
	if userText != `/explain "internal config" lifecycle` {
		t.Fatalf("persisted user invocation = %q", userText)
	}
	if strings.Contains(userText, "Explain internal config") {
		t.Fatalf("expanded body leaked into visible user text: %q", userText)
	}
	for _, want := range []string{"# Skill: explain", "**Source:**", `Explain internal config. Raw: "internal config" lifecycle`} {
		if !strings.Contains(developerText, want) {
			t.Fatalf("developer activation prompt missing %q: %q", want, developerText)
		}
	}
	if provenance == nil || provenance.Name != "explain" || provenance.Origin != "user" || provenance.Execution != "main" || provenance.RawArguments != `"internal config" lifecycle` || provenance.SourcePath == "" {
		t.Fatalf("skill provenance = %#v", provenance)
	}
	if !provenance.AllowedToolsPresent || len(provenance.AllowedTools) != 0 {
		t.Fatalf("provenance allowlist = (%v, %#v), want explicit empty", provenance.AllowedToolsPresent, provenance.AllowedTools)
	}
	if m.engine.IsToolAllowed("anything") {
		t.Fatal("explicit empty allowlist was not applied for the skill turn")
	}
	if current := m.CurrentAgentMentionEngine(); current != m.engine || current.IsToolAllowed(tools.SpawnAgentToolName) {
		t.Fatal("agent mention engine did not observe the live skill filter")
	}

	built := m.buildMessagesForStream()
	for _, message := range built {
		for _, part := range message.Parts {
			if part.Type == llm.PartSkillActivation || part.SkillActivation != nil {
				t.Fatalf("provider context leaked provenance control part: %#v", part)
			}
		}
	}
	m.restoreSkillAllowedTools()
	if !m.engine.IsToolAllowed("anything") {
		t.Fatal("prior engine tool policy was not restored")
	}
	if !m.CurrentAgentMentionEngine().IsToolAllowed(tools.SpawnAgentToolName) {
		t.Fatal("agent mention engine did not observe restored skill filter")
	}
}

func TestMainSkillDynamicToolsAreScopedToTurn(t *testing.T) {
	registry := chatTestSkillRegistry(t, map[string]string{
		"helper": `---
name: helper
description: Use a helper
allowed-tools: scoped_helper
tools:
  - name: scoped_helper
    description: Scoped helper
    script: scripts/helper.sh
---
Use the helper.
`,
	})
	toolConfig := tools.DefaultToolConfig()
	localRegistry, err := tools.NewLocalToolRegistry(&toolConfig, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := newTestChatModel(false)
	m.toolMgr = &tools.ToolManager{Registry: localRegistry}
	activation, err := skills.NewActivator(registry).Activate(skills.ActivationRequest{Name: "helper", Origin: skills.SkillActivationUser})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.registerSkillActivationTools(activation); err != nil {
		t.Fatal(err)
	}
	m.applySkillAllowedTools(activation)
	if _, ok := m.engine.Tools().Get("scoped_helper"); !ok {
		t.Fatal("skill tool was not registered with engine")
	}
	if _, ok := localRegistry.Get("scoped_helper"); !ok {
		t.Fatal("skill tool was not registered locally")
	}

	m.restoreSkillAllowedTools()
	if _, ok := m.engine.Tools().Get("scoped_helper"); ok {
		t.Fatal("skill tool leaked in engine after turn")
	}
	if _, ok := localRegistry.Get("scoped_helper"); ok {
		t.Fatal("skill tool leaked in local registry after turn")
	}
}

func TestMainContextSkillQueuesDuringParentStream(t *testing.T) {
	registry := chatTestSkillRegistry(t, map[string]string{
		"explain": "---\nname: explain\ndescription: Explain a package\n---\nExplain $ARGUMENTS.\n",
	})
	m := newTestChatModel(false)
	m.SetSkillsSetup(&skills.Setup{Registry: registry})
	m.streaming = true
	m.setTextareaValue("/explain internal/config")

	updated, _, handled := m.queueMainSkillDuringStream(m.textarea.Value())
	m = updated.(*Model)
	if !handled || len(m.queuedMainSkillActivations) != 1 {
		t.Fatalf("queued activations = %#v handled=%v", m.queuedMainSkillActivations, handled)
	}
	if m.textarea.Value() != "" {
		t.Fatalf("composer = %q, want cleared queued invocation", m.textarea.Value())
	}
	if got := m.queuedMainSkillActivations[0].Activation.Prompt; got != "Explain internal/config." {
		t.Fatalf("queued expanded prompt = %q", got)
	}

	m.streaming = false
	if cmd := m.startNextQueuedMainSkill(); cmd == nil || !m.streaming {
		t.Fatalf("queued main skill did not start at turn boundary: cmd=%v streaming=%v", cmd != nil, m.streaming)
	}
	if len(m.queuedMainSkillActivations) != 0 {
		t.Fatalf("queued activation was not consumed: %#v", m.queuedMainSkillActivations)
	}
	if got := m.messages[len(m.messages)-1].TextContent; got != "/explain internal/config" {
		t.Fatalf("visible queued invocation = %q", got)
	}
}

func TestQueuedMainContextSkillWaitsForWorktreeOperation(t *testing.T) {
	registry := chatTestSkillRegistry(t, map[string]string{
		"explain": "---\nname: explain\ndescription: Explain a package\nallowed-tools: []\n---\nExplain $ARGUMENTS.\n",
	})
	m := newTestChatModel(false)
	m.SetSkillsSetup(&skills.Setup{Registry: registry})
	m.streaming = true
	updated, _, handled := m.queueMainSkillDuringStream("/explain internal/config")
	m = updated.(*Model)
	m.streaming = false
	m.worktreeOperation = "promote"
	beforeMessages := len(m.messages)

	if cmd := m.startNextQueuedMainSkill(); cmd == nil {
		t.Fatal("busy queued skill did not schedule a retry")
	}
	if len(m.queuedMainSkillActivations) != 1 || m.streaming || m.skillFilterPending || len(m.messages) != beforeMessages {
		t.Fatalf("busy queued skill mutated state: queued=%d streaming=%v filter=%v messages=%d/%d", len(m.queuedMainSkillActivations), m.streaming, m.skillFilterPending, len(m.messages), beforeMessages)
	}

	m.worktreeOperation = ""
	updated, _ = m.Update(queuedMainSkillRetryMsg{})
	m = updated.(*Model)
	if !handled || !m.streaming || len(m.queuedMainSkillActivations) != 0 || !m.skillFilterPending {
		t.Fatalf("retried queued skill state: handled=%v streaming=%v queued=%d filter=%v", handled, m.streaming, len(m.queuedMainSkillActivations), m.skillFilterPending)
	}
}

func TestMainSkillValidatesAgentMentionBeforeAnyActivationSideEffect(t *testing.T) {
	registry := chatTestSkillRegistry(t, map[string]string{
		"scoped": `---
name: scoped
description: Scoped delegation
allowed-tools: []
tools:
  - name: scoped_helper
    description: Scoped helper
    script: scripts/helper.sh
---
Work with $ARGUMENTS.
`,
	})
	toolConfig := tools.DefaultToolConfig()
	localRegistry, err := tools.NewLocalToolRegistry(&toolConfig, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := newTestChatModel(false)
	m.toolMgr = &tools.ToolManager{Registry: localRegistry}
	m.SetSkillsSetup(&skills.Setup{Registry: registry})
	m.agentMentionCapability = &fakeAgentMentionCapability{names: []string{"reviewer"}, denied: make(map[string]error)}
	const invocation = "/scoped @agent:reviewer"
	m.setTextareaValue(invocation)

	updated, _ := m.ExecuteCommand(invocation)
	m = updated.(*Model)
	if m.streaming || len(m.messages) != 0 || m.skillFilterPending || len(m.skillDynamicToolNames) != 0 {
		t.Fatalf("rejected activation committed state: streaming=%v messages=%d filter=%v dynamic=%#v", m.streaming, len(m.messages), m.skillFilterPending, m.skillDynamicToolNames)
	}
	if _, ok := localRegistry.Get("scoped_helper"); ok {
		t.Fatal("rejected activation registered its custom tool")
	}
	if _, ok := m.engine.Tools().Get("scoped_helper"); ok {
		t.Fatal("rejected activation registered its engine tool")
	}
	if m.textarea.Value() != invocation || !strings.Contains(m.footerMessage, "blocks spawn_agent") {
		t.Fatalf("rejected activation did not preserve actionable draft: text=%q footer=%q", m.textarea.Value(), m.footerMessage)
	}
}

func TestSkillActivationErrorPreservesComposer(t *testing.T) {
	registry := chatTestSkillRegistry(t, map[string]string{
		"explain": `---
name: explain
description: Explain a package
---
Explain $ARGUMENTS[0]
`,
	})
	m := newTestChatModel(false)
	m.SetSkillsSetup(&skills.Setup{Registry: registry})
	const invocation = `/explain "unterminated`
	m.setTextareaValue(invocation)

	updated, _ := m.ExecuteCommand(invocation)
	m = updated.(*Model)
	if got := m.textarea.Value(); got != invocation {
		t.Fatalf("composer = %q, want failed invocation preserved", got)
	}
	if m.streaming {
		t.Fatal("invalid invocation unexpectedly started a stream")
	}
	if !strings.Contains(m.footerMessage, "unterminated") {
		t.Fatalf("footer error = %q, want argument diagnostic", m.footerMessage)
	}
}

func chatTestSkillRegistry(t *testing.T, manifests map[string]string) *skills.Registry {
	t.Helper()
	root := t.TempDir()
	for name, manifest := range manifests {
		dir := filepath.Join(root, ".skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := skills.NewRegistry(skills.RegistryConfig{
		AutoInvoke:            true,
		IncludeProjectSkills:  true,
		IncludeEcosystemPaths: false,
		ProjectDir:            root,
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

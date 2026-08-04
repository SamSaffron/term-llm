package chat

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/tools"
)

func (m *Model) skillSlashEntries() []Command {
	if m == nil || m.skillsSetup == nil || m.skillsSetup.Registry == nil {
		return nil
	}
	catalog, _, err := m.skillsSetup.Registry.ListUserInvocable()
	if err != nil {
		return nil
	}
	reserved := builtInSlashNames()
	entries := make([]Command, 0, len(catalog))
	for _, skill := range catalog {
		if reserved[skill.Name] {
			continue
		}
		metadata, err := skills.InvocationFor(skill)
		if err != nil {
			continue
		}
		description := skill.Description + " · skill:" + skill.Source.SourceName()
		if metadata.Execution == skills.SkillExecutionIsolatedAgent {
			description += " · isolated"
		}
		entries = append(entries, Command{
			Name:         skill.Name,
			Description:  description,
			Usage:        "/" + skill.Name + completionHintSuffix(metadata.ArgumentHint),
			Kind:         SlashEntrySkill,
			ArgumentHint: metadata.ArgumentHint,
			Source:       skill.Source.SourceName(),
		})
	}
	return entries
}

func completionHintSuffix(hint string) string {
	if strings.TrimSpace(hint) == "" {
		return ""
	}
	return " " + strings.TrimSpace(hint)
}

func builtInSlashNames() map[string]bool {
	reserved := make(map[string]bool)
	for _, command := range AllCommands() {
		reserved[command.Name] = true
		for _, alias := range command.Aliases {
			reserved[alias] = true
		}
	}
	return reserved
}

func (m *Model) slashEntries() []Command {
	entries := append([]Command(nil), AllCommands()...)
	entries = append(entries, m.skillSlashEntries()...)
	return entries
}

func (m *Model) filterSlashEntries(query string) []Command {
	return filterCommandEntries(m.slashEntries(), query)
}

func (m *Model) filterStreamingSlashEntries(query string) []Command {
	items := m.filterSlashEntries(query)
	streaming := make([]Command, 0, len(items))
	for _, item := range items {
		if item.Kind == SlashEntrySkill {
			_, metadata, ok := m.userInvocableSkill(item.Name)
			if ok && metadata.Execution == skills.SkillExecutionIsolatedAgent {
				streaming = append(streaming, item)
			}
			continue
		}
		if isStreamingLocalSlashCommand("/" + item.Name) {
			streaming = append(streaming, item)
		}
	}
	return streaming
}

func (m *Model) userInvocableSkill(name string) (*skills.Skill, skills.InvocationMetadata, bool) {
	if m == nil || m.skillsSetup == nil || m.skillsSetup.Registry == nil {
		return nil, skills.InvocationMetadata{}, false
	}
	name = strings.ToLower(strings.TrimSpace(name))
	catalog, _, err := m.skillsSetup.Registry.ListUserInvocable()
	if err != nil {
		return nil, skills.InvocationMetadata{}, false
	}
	for _, skill := range catalog {
		if skill.Name != name {
			continue
		}
		metadata, err := skills.InvocationFor(skill)
		if err != nil {
			return nil, skills.InvocationMetadata{}, false
		}
		return skill, metadata, true
	}
	return nil, skills.InvocationMetadata{}, false
}

func (m *Model) isSlashCommandLike(input string) bool {
	if isSlashCommandLike(input) {
		return true
	}
	parts := strings.Fields(input)
	if len(parts) == 0 || !strings.HasPrefix(parts[0], "/") {
		return false
	}
	name := strings.TrimPrefix(parts[0], "/")
	_, _, ok := m.userInvocableSkill(name)
	return ok
}

func (m *Model) isStreamingSlashCommand(input string) bool {
	if isStreamingLocalSlashCommand(input) {
		return true
	}
	parts := strings.Fields(input)
	if len(parts) == 0 || !strings.HasPrefix(parts[0], "/") {
		return false
	}
	_, metadata, ok := m.userInvocableSkill(strings.TrimPrefix(parts[0], "/"))
	return ok && metadata.Execution == skills.SkillExecutionIsolatedAgent
}

type queuedMainSkillActivation struct {
	Activation        *skills.Activation
	DisplayInvocation string
}

type queuedMainSkillRetryMsg struct{}

func (m *Model) validateMainSkillAgentMentions(activation *skills.Activation, displayInvocation string) error {
	context, err := m.agentMentionDelegationContext(displayInvocation)
	if err != nil || context == "" {
		return err
	}
	if activation == nil || !activation.AllowedToolsPresent {
		return nil
	}
	for _, name := range activation.AllowedTools {
		if name == tools.SpawnAgentToolName {
			return nil
		}
	}
	skillName := "active skill"
	if activation.Skill != nil && activation.Skill.Name != "" {
		skillName = fmt.Sprintf("skill %q", activation.Skill.Name)
	}
	return fmt.Errorf("cannot submit agent mention: %s blocks %s through its allowed-tools restriction", skillName, tools.SpawnAgentToolName)
}

func (m *Model) executeMainSkillActivation(activation *skills.Activation, displayInvocation string) (tea.Model, tea.Cmd) {
	if m.worktreeOperationBusy() {
		return m.showFooterWarning("Wait for the current worktree operation to finish before invoking a skill.")
	}
	if err := m.validateMainSkillAgentMentions(activation, displayInvocation); err != nil {
		return m.showFooterError(err.Error())
	}
	if m.skillFilterPending || len(m.skillDynamicToolNames) > 0 {
		m.restoreSkillAllowedTools()
	}
	if err := m.registerSkillActivationTools(activation); err != nil {
		return m.showFooterError(err.Error())
	}
	m.applySkillAllowedTools(activation)
	m.persistSkillActivationContext(activation)
	return m.sendMessage(displayInvocation)
}

func (m *Model) queueMainSkillDuringStream(input string) (tea.Model, tea.Cmd, bool) {
	token, rawArgs := splitRawCommandToken(input)
	name := strings.ToLower(strings.TrimPrefix(token, "/"))
	if name == "" || builtInSlashNames()[name] {
		return m, nil, false
	}
	_, metadata, ok := m.userInvocableSkill(name)
	if !ok || metadata.Execution != skills.SkillExecutionMain {
		return m, nil, false
	}
	activation, err := skills.NewActivator(m.skillsSetup.Registry).Activate(skills.ActivationRequest{
		Name: name, RawArgs: rawArgs, Origin: skills.SkillActivationUser,
	})
	if err != nil {
		updated, cmd := m.showFooterError(err.Error())
		return updated, cmd, true
	}
	if err := m.validateMainSkillAgentMentions(activation, strings.TrimSpace(input)); err != nil {
		updated, cmd := m.showFooterError(err.Error())
		return updated, cmd, true
	}
	m.queuedMainSkillActivations = append(m.queuedMainSkillActivations, queuedMainSkillActivation{
		Activation: activation, DisplayInvocation: strings.TrimSpace(input),
	})
	m.setTextareaValue("")
	m.completions.Hide()
	updated, cmd := m.showFooterMuted("Queued " + strings.TrimSpace(input) + " for the next parent turn.")
	return updated, cmd, true
}

func (m *Model) startNextQueuedMainSkill() tea.Cmd {
	if m.streaming || len(m.queuedMainSkillActivations) == 0 {
		return nil
	}
	if m.worktreeOperationBusy() {
		return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg {
			return queuedMainSkillRetryMsg{}
		})
	}
	queued := m.queuedMainSkillActivations[0]
	if err := m.validateMainSkillAgentMentions(queued.Activation, queued.DisplayInvocation); err != nil {
		_, cmd := m.showFooterError(err.Error())
		return cmd
	}
	m.queuedMainSkillActivations = m.queuedMainSkillActivations[1:]
	_, cmd := m.executeMainSkillActivation(queued.Activation, queued.DisplayInvocation)
	return cmd
}

func (m *Model) executeSkill(name, rawArgs, displayInvocation string) (tea.Model, tea.Cmd) {
	if m.skillsSetup == nil || m.skillsSetup.Registry == nil {
		return m.showFooterError("Skills are not enabled for this session.")
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if m.worktreeOperationBusy() {
		return m.showFooterWarning("Wait for the current worktree operation to finish before invoking a skill.")
	}

	activation, err := skills.NewActivator(m.skillsSetup.Registry).Activate(skills.ActivationRequest{
		Name:    name,
		RawArgs: rawArgs,
		Origin:  skills.SkillActivationUser,
	})
	if err != nil {
		return m.showFooterError(err.Error())
	}
	if activation.Metadata.Execution == skills.SkillExecutionIsolatedAgent {
		return m.startIsolatedSkillActivation(activation, displayInvocation)
	}
	return m.executeMainSkillActivation(activation, displayInvocation)
}

func (m *Model) registerSkillActivationTools(activation *skills.Activation) error {
	if activation == nil || len(activation.ToolDefs) == 0 {
		return nil
	}
	if m.toolMgr == nil {
		return fmt.Errorf("skill %q declares tools, but this session has no local tool registry", activation.Skill.Name)
	}

	names := make([]string, 0, len(activation.ToolDefs))
	enginePrevious := make(map[string]llm.Tool)
	registryPrevious := make(map[string]llm.Tool)
	seen := make(map[string]bool)
	for _, definition := range activation.ToolDefs {
		if seen[definition.Name] {
			continue
		}
		seen[definition.Name] = true
		names = append(names, definition.Name)
		if tool, ok := m.toolMgr.Registry.Get(definition.Name); ok {
			registryPrevious[definition.Name] = tool
		}
		if m.engine != nil {
			if tool, ok := m.engine.Tools().Get(definition.Name); ok {
				enginePrevious[definition.Name] = tool
			}
		}
	}
	if err := m.toolMgr.Registry.RegisterSkillTools(activation.ToolDefs, activation.BaseDir); err != nil {
		m.toolMgr.Registry.RestoreSkillTools(names, registryPrevious)
		return fmt.Errorf("register tools for skill %q: %w", activation.Skill.Name, err)
	}
	for _, definition := range activation.ToolDefs {
		if tool, ok := m.toolMgr.Registry.Get(definition.Name); ok {
			m.engine.AddDynamicTool(tool)
		}
	}
	m.skillDynamicToolNames = names
	m.skillDynamicEnginePrevious = enginePrevious
	m.skillDynamicRegistryPrevious = registryPrevious
	return nil
}

func (m *Model) applySkillAllowedTools(activation *skills.Activation) {
	if activation == nil || !activation.AllowedToolsPresent || m.engine == nil {
		return
	}
	prior, priorPresent := m.engine.AllowedToolsFilter()
	effective := append([]string(nil), activation.AllowedTools...)
	if priorPresent {
		effective = intersectToolNames(effective, prior)
	}
	if !m.skillFilterPending {
		m.skillFilterRestoreTools = prior
		m.skillFilterRestorePresent = priorPresent
		m.skillFilterPending = true
	}
	m.engine.SetAllowedToolsFilter(effective)
}

func intersectToolNames(left, right []string) []string {
	rightSet := make(map[string]bool, len(right))
	for _, name := range right {
		rightSet[name] = true
	}
	result := make([]string, 0, len(left))
	for _, name := range left {
		if rightSet[name] {
			result = append(result, name)
		}
	}
	return result
}

func (m *Model) restoreSkillAllowedTools() {
	if m == nil {
		return
	}
	if m.skillFilterPending && m.engine != nil {
		m.engine.RestoreAllowedToolsFilter(m.skillFilterRestoreTools, m.skillFilterRestorePresent)
	}
	m.skillFilterRestoreTools = nil
	m.skillFilterRestorePresent = false
	m.skillFilterPending = false

	if len(m.skillDynamicToolNames) > 0 {
		if m.engine != nil {
			for _, name := range m.skillDynamicToolNames {
				m.engine.UnregisterTool(name)
				if previous := m.skillDynamicEnginePrevious[name]; previous != nil {
					m.engine.RegisterTool(previous)
				}
			}
		}
		if m.toolMgr != nil && m.toolMgr.Registry != nil {
			m.toolMgr.Registry.RestoreSkillTools(m.skillDynamicToolNames, m.skillDynamicRegistryPrevious)
		}
	}
	m.skillDynamicToolNames = nil
	m.skillDynamicEnginePrevious = nil
	m.skillDynamicRegistryPrevious = nil
}

func (m *Model) persistSkillActivationContext(activation *skills.Activation) {
	if activation == nil || activation.Skill == nil || m.sess == nil {
		return
	}
	m.ensureContextMessages()
	provenance := skillActivationProvenance(activation)
	instructions := skills.RenderActivationInstructions(activation)
	message := &session.Message{
		SessionID: m.sess.ID,
		Role:      llm.RoleDeveloper,
		Parts: []llm.Part{
			{Type: llm.PartSkillActivation, SkillActivation: provenance},
			{Type: llm.PartText, Text: instructions},
		},
		TextContent: instructions,
		CreatedAt:   time.Now(),
		Sequence:    -1,
	}
	m.messages = append(m.messages, *message)
	if m.store != nil {
		_ = m.store.AddMessage(context.Background(), m.sess.ID, message)
		m.messages[len(m.messages)-1].ID = message.ID
	}
	m.invalidateHistoryCache()
}

func skillActivationProvenance(activation *skills.Activation) *llm.SkillActivationProvenance {
	if activation == nil || activation.Skill == nil {
		return nil
	}
	return &llm.SkillActivationProvenance{
		Name:                activation.Skill.Name,
		Source:              activation.Skill.Source.SourceName(),
		SourcePath:          activation.Skill.SourcePath,
		Origin:              activation.Origin.String(),
		Execution:           activation.Metadata.Execution.String(),
		RawArguments:        activation.RawArgs,
		Agent:               activation.Metadata.Agent,
		Model:               activation.Metadata.Model,
		AllowedTools:        append([]string(nil), activation.AllowedTools...),
		AllowedToolsPresent: activation.AllowedToolsPresent,
		ActivatedAt:         time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func (m *Model) executeSkillsCommand(args []string, rawArgs string) (tea.Model, tea.Cmd) {
	if m.skillsSetup == nil || m.skillsSetup.Registry == nil {
		return m.showFooterError("Skills are not enabled for this session.")
	}
	if len(args) == 0 || args[0] == "list" {
		content, err := m.skillListMarkdown()
		if err != nil {
			return m.showFooterError(err.Error())
		}
		return m.showSystemMessage(content)
	}

	switch args[0] {
	case "show":
		if len(args) != 2 {
			return m.showFooterError("Usage: /skills show <name>")
		}
		content, err := m.skillShowMarkdown(args[1])
		if err != nil {
			return m.showFooterError(err.Error())
		}
		return m.showSystemMessage(content)
	case "run":
		_, afterSubcommand := splitRawCommandToken(rawArgs)
		name, invocationArgs := splitRawCommandToken(afterSubcommand)
		if name == "" {
			return m.showFooterError("Usage: /skills run <name> [arguments]")
		}
		display := "/skills run " + name
		if invocationArgs != "" {
			display += " " + invocationArgs
		}
		return m.executeSkill(name, invocationArgs, display)
	case "active":
		content := m.activeSkillRunsMarkdown()
		if strings.Contains(content, "\n") {
			return m.showSystemMessage(content)
		}
		return m.showFooterMuted(content)
	case "cancel":
		if len(args) != 2 {
			return m.showFooterError("Usage: /skills cancel <run-id>")
		}
		if err := m.cancelSkillRun(args[1]); err != nil {
			return m.showFooterError(err.Error())
		}
		return m.showFooterMuted("Cancelling isolated skill run " + args[1] + "…")
	default:
		return m.showFooterError("Usage: /skills [list|show|run|active|cancel]")
	}
}

func splitRawCommandToken(raw string) (token, rest string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	index := strings.IndexAny(raw, " \t\r\n")
	if index < 0 {
		return raw, ""
	}
	return raw[:index], strings.TrimSpace(raw[index:])
}

func (m *Model) skillArgumentCompletionItems(query string) ([]Command, bool) {
	lower := strings.ToLower(query)
	var prefix, partial string
	switch {
	case strings.HasPrefix(lower, "skills run "):
		prefix = "skills run "
		partial = strings.TrimSpace(query[len(prefix):])
	case strings.HasPrefix(lower, "sk run "):
		prefix = "sk run "
		partial = strings.TrimSpace(query[len(prefix):])
	case strings.HasPrefix(lower, "skills show "):
		prefix = "skills show "
		partial = strings.TrimSpace(query[len(prefix):])
	case strings.HasPrefix(lower, "sk show "):
		prefix = "sk show "
		partial = strings.TrimSpace(query[len(prefix):])
	case strings.HasPrefix(lower, "skills cancel "):
		prefix = "skills cancel "
		partial = strings.TrimSpace(query[len(prefix):])
	case strings.HasPrefix(lower, "sk cancel "):
		prefix = "sk cancel "
		partial = strings.TrimSpace(query[len(prefix):])
	default:
		return nil, false
	}
	if strings.Contains(prefix, " cancel ") {
		var items []Command
		for _, state := range m.skillRuns {
			if state == nil || (state.Status != "running" && state.Status != "cancelling") {
				continue
			}
			if partial != "" && !strings.HasPrefix(state.ID, partial) {
				continue
			}
			items = append(items, Command{Name: prefix + state.ID, Description: "/" + state.Name + " · " + state.Status})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
		return items, true
	}
	if m.skillsSetup == nil || m.skillsSetup.Registry == nil {
		return nil, true
	}

	var catalog []*skills.Skill
	var err error
	if strings.Contains(prefix, " run ") {
		catalog, _, err = m.skillsSetup.Registry.ListUserInvocable()
	} else {
		catalog, err = m.skillsSetup.Registry.List()
	}
	if err != nil {
		return nil, true
	}
	reserved := builtInSlashNames()
	items := make([]Command, 0, len(catalog))
	for _, skill := range catalog {
		if partial != "" && !strings.HasPrefix(skill.Name, strings.ToLower(partial)) {
			continue
		}
		if m.streaming && strings.Contains(prefix, " run ") {
			metadata, metadataErr := skills.InvocationFor(skill)
			if metadataErr != nil || metadata.Execution != skills.SkillExecutionIsolatedAgent {
				continue
			}
		}
		description := skill.Description + " · " + skill.Source.SourceName()
		if reserved[skill.Name] {
			description += " · built-in collision"
		}
		items = append(items, Command{Name: prefix + skill.Name, Description: description, Kind: SlashEntrySkill})
	}
	return items, true
}

func (m *Model) skillListMarkdown() (string, error) {
	catalog, diagnostics, err := m.skillsSetup.Registry.ListUserInvocable()
	if err != nil {
		return "", fmt.Errorf("list skills: %w", err)
	}
	var builder strings.Builder
	builder.WriteString("## User-invocable skills\n\n")
	if len(catalog) == 0 {
		builder.WriteString("No user-invocable skills are installed.\n")
	}
	reserved := builtInSlashNames()
	for _, skill := range catalog {
		metadata, err := skills.InvocationFor(skill)
		if err != nil {
			continue
		}
		invocation := "/" + skill.Name
		if reserved[skill.Name] {
			invocation = "/skills run " + skill.Name + " (built-in collision)"
		}
		builder.WriteString(fmt.Sprintf("- `%s%s` — %s · %s", invocation, completionHintSuffix(metadata.ArgumentHint), skill.Description, skill.Source.SourceName()))
		if metadata.Execution == skills.SkillExecutionIsolatedAgent {
			builder.WriteString(" · isolated agent `" + metadata.Agent + "`")
		}
		builder.WriteString("\n")
	}
	for _, diagnostic := range diagnostics {
		builder.WriteString(fmt.Sprintf("- ⚠ `%s` — %v\n", diagnostic.Name, diagnostic.Err))
	}
	builder.WriteString("\nUse `/skills show <name>` for metadata or `/skills run <name> ...` for explicit invocation.\n")
	return builder.String(), nil
}

func (m *Model) skillShowMarkdown(name string) (string, error) {
	skill, err := m.skillsSetup.Registry.Get(strings.ToLower(strings.TrimSpace(name)))
	if err != nil {
		return "", err
	}
	metadata, err := skills.InvocationFor(skill)
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	builder.WriteString("## Skill: `" + skill.Name + "`\n\n")
	builder.WriteString("- Description: " + skill.Description + "\n")
	builder.WriteString("- Source: " + skill.Source.SourceName() + "\n")
	builder.WriteString("- Path: `" + skill.SourcePath + "`\n")
	builder.WriteString("- User invocable: " + fmt.Sprint(metadata.UserInvocable) + "\n")
	builder.WriteString("- Model invocable: " + fmt.Sprint(!metadata.DisableModelInvocation) + "\n")
	builder.WriteString("- Execution: " + metadata.Execution.String() + "\n")
	if metadata.Agent != "" {
		builder.WriteString("- Agent: `" + metadata.Agent + "`\n")
	}
	if metadata.Model != "" {
		builder.WriteString("- Model: `" + metadata.Model + "`\n")
	}
	if builtInSlashNames()[skill.Name] {
		builder.WriteString("- Collision: reserved built-in; invoke with `/skills run " + skill.Name + "`\n")
	}
	if skill.HasResources() {
		builder.WriteString("\n" + skill.ResourceTree() + "\n")
	}
	return builder.String(), nil
}

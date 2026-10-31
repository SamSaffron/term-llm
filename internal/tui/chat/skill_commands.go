package chat

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/muesli/reflow/wordwrap"
	"github.com/muesli/reflow/wrap"
	"github.com/samsaffron/term-llm/internal/llm/core"
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

const skillsCommandName = "skills"

// isSkillsCommandToken reports whether a slash token names /skills or one of
// its aliases.
func isSkillsCommandToken(token string) bool {
	token = strings.ToLower(strings.TrimPrefix(token, "/"))
	for _, command := range AllCommands() {
		if command.Name == skillsCommandName {
			return token == command.Name || slices.Contains(command.Aliases, token)
		}
	}
	return false
}

// isSkillSlashInput reports whether input invokes /skills or a user-invocable
// skill by its exact slash name, following ExecuteCommand's resolution order.
func (m *Model) isSkillSlashInput(input string) bool {
	token, _ := splitRawCommandToken(input)
	if !strings.HasPrefix(token, "/") {
		return false
	}
	if isSkillsCommandToken(token) {
		return true
	}
	name := strings.ToLower(strings.TrimPrefix(token, "/"))
	if builtInSlashNames()[name] {
		return false
	}
	_, _, ok := m.userInvocableSkill(name)
	return ok
}

// handleStreamingSkillCommand runs skill slash input while the parent streams
// instead of sending it as steering. /skills subcommands are UI-local, isolated
// skills start immediately, and executeSkill queues main-context skills for the
// next parent turn. Failures keep the composer text for editing.
func (m *Model) handleStreamingSkillCommand(input string) (tea.Model, tea.Cmd, bool) {
	if !m.isSkillSlashInput(input) {
		return m, nil, false
	}
	updated, cmd := m.ExecuteCommand(input)
	return updated, cmd, true
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
		// executeSkill queues main-context skills for the next parent turn and
		// starts isolated ones immediately, so every skill entry is safe.
		if item.Kind == SlashEntrySkill || item.Name == skillsCommandName || isStreamingLocalSlashCommand("/"+item.Name) {
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
	m.pendingSkillActivation = activation
	updated, cmd := m.sendMessage(displayInvocation)
	if m.pendingSkillActivation != nil {
		// sendMessage refused before claiming the turn (another process owns
		// it, a shell command is running, ...), so nothing was written. Drop the
		// activation's scoped tools and filter instead of leaking them.
		m.pendingSkillActivation = nil
		m.restoreSkillAllowedTools()
	}
	return updated, cmd
}

// appendTurnPreamble writes the rows that belong directly before a claimed
// user turn: a deferred model-switch marker, then the pending direct skill
// activation, which /undo removes together with the turn.
func (m *Model) appendTurnPreamble() {
	m.appendPendingModelSwitchMarker()
	if activation := m.pendingSkillActivation; activation != nil {
		m.pendingSkillActivation = nil
		m.persistSkillActivationContext(activation)
	}
}

// SetSkillsSetup installs the session-bound skill registry used for slash
// discovery and direct activation. The setup is reused across keystrokes; its
// registry cache notices ordinary SKILL.md edits through file fingerprints.
func (m *Model) SetSkillsSetup(setup *skills.Setup) {
	m.skillsSetup = setup
}

// queueMainSkillActivation defers a main-context skill while the parent turn
// owns the engine; startNextQueuedMainSkill runs it at the turn boundary.
func (m *Model) queueMainSkillActivation(activation *skills.Activation, displayInvocation string) (tea.Model, tea.Cmd) {
	if err := m.validateMainSkillAgentMentions(activation, displayInvocation); err != nil {
		return m.showFooterError(err.Error())
	}
	m.queuedMainSkillActivations = append(m.queuedMainSkillActivations, queuedMainSkillActivation{
		Activation: activation, DisplayInvocation: displayInvocation,
	})
	m.setTextareaValue("")
	m.completions.Hide()
	return m.showFooterMuted("Queued " + displayInvocation + " for the next parent turn.")
}

func (m *Model) startNextQueuedMainSkill() tea.Cmd {
	if m.streaming || len(m.queuedMainSkillActivations) == 0 {
		return nil
	}
	if m.worktreeOperationBusy() {
		return m.presentationTick(100*time.Millisecond, func(time.Time) tea.Msg {
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
	activation, err := skills.NewActivator(m.skillsSetup.Registry).Activate(skills.ActivationRequest{
		Name:    strings.ToLower(strings.TrimSpace(name)),
		RawArgs: rawArgs,
		Origin:  skills.SkillActivationUser,
	})
	if err != nil {
		return m.showFooterError(err.Error())
	}
	isolated := activation.Metadata.Execution == skills.SkillExecutionIsolatedAgent
	if m.streaming && !isolated {
		return m.queueMainSkillActivation(activation, displayInvocation)
	}
	if m.worktreeOperationBusy() {
		return m.showFooterWarning("Wait for the current worktree operation to finish before invoking a skill.")
	}
	if isolated {
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
	enginePrevious := make(map[string]core.Tool)
	registryPrevious := make(map[string]core.Tool)
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
		Role:      core.RoleDeveloper,
		Parts: []core.Part{
			{Type: core.PartSkillActivation, SkillActivation: provenance},
			{Type: core.PartText, Text: instructions},
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

func skillActivationProvenance(activation *skills.Activation) *core.SkillActivationProvenance {
	if activation == nil || activation.Skill == nil {
		return nil
	}
	return &core.SkillActivationProvenance{
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

// executeSkillsCommand handles /skills. Listings open a content dialog because
// tea.Println output is dropped while the chat owns the alternate screen.
func (m *Model) executeSkillsCommand(args []string, rawArgs string) (tea.Model, tea.Cmd) {
	if m.skillsSetup == nil || m.skillsSetup.Registry == nil {
		return m.showFooterError("Skills are not enabled for this session.")
	}
	subcommand := ""
	if len(args) > 0 {
		subcommand = strings.ToLower(args[0])
	}
	switch subcommand {
	case "", "list":
		content, err := m.skillListText()
		if err != nil {
			return m.showFooterError(err.Error())
		}
		return m.showSkillsDialog("Skills", content)
	case "show":
		if len(args) != 2 {
			return m.showFooterError("Usage: /skills show <name>")
		}
		content, err := m.skillShowText(args[1])
		if err != nil {
			return m.showFooterError(err.Error())
		}
		return m.showSkillsDialog("Skill: "+strings.ToLower(args[1]), content)
	case "run":
		_, afterSubcommand := splitRawCommandToken(rawArgs)
		name, invocationArgs := splitRawCommandToken(afterSubcommand)
		if name == "" {
			return m.showFooterError("Usage: /skills run <name> [arguments]")
		}
		return m.executeSkill(name, invocationArgs, joinInvocation("/skills run "+name, invocationArgs))
	case "active":
		if content := m.activeSkillRunsText(); content != "" {
			return m.showSkillsDialog("Active skill runs", content)
		}
		m.setTextareaValue("")
		return m.showFooterMuted("No active isolated skill runs.")
	case "cancel":
		if len(args) != 2 {
			return m.showFooterError("Usage: /skills cancel <run-id>")
		}
		if err := m.cancelSkillRun(args[1]); err != nil {
			return m.showFooterError(err.Error())
		}
		m.setTextareaValue("")
		return m.showFooterMuted("Cancelling isolated skill run " + args[1] + "…")
	}
	// "/skills <name> [arguments]" is shorthand for /skills run.
	name, invocationArgs := splitRawCommandToken(rawArgs)
	if _, _, ok := m.userInvocableSkill(name); ok {
		name = strings.ToLower(name)
		display := "/" + name
		if builtInSlashNames()[name] {
			display = "/skills run " + name
		}
		return m.executeSkill(name, invocationArgs, joinInvocation(display, invocationArgs))
	}
	return m.showFooterError(fmt.Sprintf("Unknown skill %q. Usage: /skills [list|show|run|active|cancel] or /skills <name> [arguments]", name))
}

func joinInvocation(command, args string) string {
	if args == "" {
		return command
	}
	return command + " " + args
}

func (m *Model) showSkillsDialog(title, content string) (tea.Model, tea.Cmd) {
	m.setTextareaValue("")
	m.completions.Hide()
	m.clearFooterMessage()
	m.dialog.ShowContent(title, content)
	return m, nil
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

// skillArgumentCompletionItems completes /skills (and its aliases): the
// subcommand itself, skill names after run/show, and active run IDs after
// cancel. Every /skills subcommand is safe while the parent streams.
func (m *Model) skillArgumentCompletionItems(query string) ([]Command, bool) {
	token, rest, ok := strings.Cut(query, " ")
	if !ok || !isSkillsCommandToken(token) {
		return nil, false
	}
	subcommand, partial, ok := strings.Cut(rest, " ")
	if !ok {
		return filterCommandEntries(AllCommands(), query), true
	}
	subcommand = strings.ToLower(subcommand)
	prefix := strings.ToLower(token) + " " + subcommand + " "
	partial = strings.ToLower(strings.TrimSpace(partial))
	if subcommand == "cancel" {
		var items []Command
		for _, state := range m.skillRuns {
			if state != nil && (state.Status == "running" || state.Status == "cancelling") && strings.HasPrefix(state.ID, partial) {
				items = append(items, Command{Name: prefix + state.ID, Description: "/" + state.Name + " · " + state.Status})
			}
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
		return items, true
	}
	if (subcommand != "run" && subcommand != "show") || m.skillsSetup == nil || m.skillsSetup.Registry == nil {
		return nil, true
	}
	var catalog []*skills.Skill
	var err error
	if subcommand == "run" {
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
		if !strings.HasPrefix(skill.Name, partial) {
			continue
		}
		item := Command{Name: prefix + skill.Name, Description: skill.Description + " · " + skill.Source.SourceName(), Kind: SlashEntrySkill}
		if reserved[skill.Name] {
			item.Description += " · built-in collision"
		}
		if subcommand == "run" {
			metadata, metadataErr := skills.InvocationFor(skill)
			if metadataErr != nil {
				continue
			}
			item.ArgumentHint = metadata.ArgumentHint
		}
		items = append(items, item)
	}
	return items, true
}

func (m *Model) skillListText() (string, error) {
	catalog, diagnostics, err := m.skillsSetup.Registry.ListUserInvocable()
	if err != nil {
		return "", fmt.Errorf("list skills: %w", err)
	}
	reserved := builtInSlashNames()
	var rows [][2]string
	width := 0
	for _, skill := range catalog {
		metadata, err := skills.InvocationFor(skill)
		if err != nil {
			continue
		}
		invocation, detail := "/"+skill.Name, skill.Description+" · "+skill.Source.SourceName()
		if reserved[skill.Name] {
			invocation = "/skills run " + skill.Name
			detail += " · /" + skill.Name + " is a built-in command"
		}
		if metadata.Execution == skills.SkillExecutionIsolatedAgent {
			detail += " · isolated @" + metadata.Agent
		}
		invocation += completionHintSuffix(metadata.ArgumentHint)
		rows = append(rows, [2]string{invocation, detail})
		width = max(width, len(invocation))
	}
	content := ""
	if len(rows) == 0 {
		content = "No user-invocable skills are installed.\n"
	}
	for _, diagnostic := range diagnostics {
		rows = append(rows, [2]string{"⚠ " + diagnostic.Name, diagnostic.Err.Error()})
	}
	content += m.alignedDialogRows(rows, min(width, 32))
	return content + "\n" + wordwrap.String("Run a skill with /<name> [arguments]. /skills show <name> shows its details.", m.dialogTextWidth()), nil
}

func (m *Model) skillShowText(name string) (string, error) {
	skill, err := m.skillsSetup.Registry.Get(strings.ToLower(strings.TrimSpace(name)))
	if err != nil {
		return "", err
	}
	metadata, err := skills.InvocationFor(skill)
	if err != nil {
		return "", err
	}
	invocation := "/" + skill.Name + completionHintSuffix(metadata.ArgumentHint)
	if builtInSlashNames()[skill.Name] {
		invocation = "/skills run " + skill.Name + completionHintSuffix(metadata.ArgumentHint) + " (/" + skill.Name + " is a built-in command)"
	}
	if !metadata.UserInvocable {
		invocation = "model only (user-invocable: false)"
	}
	execution := "main (adds instructions to this conversation)"
	if metadata.Execution == skills.SkillExecutionIsolatedAgent {
		execution = "isolated child agent @" + metadata.Agent
	}
	modelInvocable := "yes"
	if metadata.DisableModelInvocation {
		modelInvocable = "no (disable-model-invocation)"
	}
	rows := [][2]string{
		{"Description", skill.Description},
		{"Invoke", invocation},
		{"Execution", execution},
		{"Model invocable", modelInvocable},
		{"Source", skill.Source.SourceName()},
		{"Path", skill.SourcePath},
	}
	if metadata.Model != "" {
		rows = append(rows, [2]string{"Model", metadata.Model})
	}
	content := m.alignedDialogRows(rows, 15)
	if skill.HasResources() {
		content += "\n" + skill.ResourceTree()
	}
	return content, nil
}

// alignedDialogRows formats label/detail rows for a content dialog, wrapping
// each detail under its column (at word boundaries where possible).
func (m *Model) alignedDialogRows(rows [][2]string, labelWidth int) string {
	indent := strings.Repeat(" ", labelWidth+4)
	detailWidth := max(20, m.dialogTextWidth()-len(indent))
	var builder strings.Builder
	for _, row := range rows {
		lines := strings.Split(wrap.String(wordwrap.String(row[1], detailWidth), detailWidth), "\n")
		fmt.Fprintf(&builder, "  %-*s  %s\n", labelWidth, row[0], lines[0])
		for _, line := range lines[1:] {
			builder.WriteString(indent + line + "\n")
		}
	}
	return builder.String()
}

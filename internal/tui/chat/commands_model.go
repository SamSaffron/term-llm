package chat

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/sahilm/fuzzy"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/tools"
)

func (m *Model) cmdReload() (tea.Model, tea.Cmd) {
	if m.reloadEnabled {
		m.setTextareaValue("")
		restart.Default.Request()
		return m, nil
	}
	if m.branchContextInFlight() {
		return m.showSystemMessage("Cannot reload while path notes are being created. Cancel first (Esc).")
	}
	if m.streaming {
		return m.showSystemMessage("Cannot reload while streaming. Cancel first (Esc).")
	}
	m.setTextareaValue("")
	m.quitting = true
	m.reloadRequested = true
	if m.sess != nil {
		m.reloadSessionID = m.sess.ID
	}
	m.cancelActiveSkillRuns()
	if m.activeSkillRunCount() > 0 {
		m.quitAfterSkillRuns = true
		return m, nil
	}
	return m, m.quitCmd()
}

func (m *Model) cmdModel(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		// Show model picker dialog with MRU ordering
		history, _ := config.LoadModelHistory()
		m.dialog.ShowModelPicker(m.providerKey+":"+m.modelName, GetAvailableProviders(m.config), config.ModelHistoryOrder(history))
		m.setTextareaValue("")
		return m, nil
	}

	// Switch to specified model (format: provider:model or just model/alias)
	m.pauseGoalForLocalAction("paused for model switch")
	modelArg := args[0]
	fallbackProvider := strings.TrimSpace(m.providerKey)
	if fallbackProvider == "" {
		fallbackProvider = strings.TrimSpace(m.providerName)
	}
	resolved, ok := resolveProviderModelArg(modelArg, m.config, fallbackProvider)
	if !ok {
		return m.showSystemMessage(fmt.Sprintf("Invalid model format: %s", modelArg))
	}
	return m.switchModel(resolved)
}

func (m *Model) currentProviderAndModel() (provider, model string) {
	provider = strings.TrimSpace(m.providerKey)
	if provider == "" && m.sess != nil {
		provider = strings.TrimSpace(m.sess.ProviderKey)
	}
	if provider == "" {
		provider = strings.TrimSpace(m.providerName)
	}

	model = strings.TrimSpace(m.modelName)
	if model == "" && m.sess != nil {
		model = strings.TrimSpace(m.sess.Model)
	}
	return provider, model
}

func (m *Model) currentProviderAndModelForEffortCycle() (provider, model string) {
	provider, model = m.currentProviderAndModel()
	if !m.streaming || m.pendingStreamModelSwitch == nil {
		return provider, model
	}
	if pendingProvider := strings.TrimSpace(m.pendingStreamModelSwitch.provider); pendingProvider != "" {
		provider = pendingProvider
	}
	if pendingModel := strings.TrimSpace(m.pendingStreamModelSwitch.model); pendingModel != "" {
		model = pendingModel
	}
	return provider, model
}

func (m *Model) cachedOllamaModelEffort(provider, model string) (base, effort string, efforts []string, ok bool) {
	if m == nil || m.config == nil {
		return "", "", nil, false
	}
	pc, exists := m.config.Providers[strings.TrimSpace(provider)]
	if !exists || config.InferProviderType(provider, pc.Type) != config.ProviderTypeOllama {
		return "", "", nil, false
	}
	if err := pc.ResolveForInference(); err != nil {
		return "", "", nil, false
	}
	return llm.CachedOllamaModelEffort(pc.BaseURL, model)
}

func (m *Model) baseModelAndEffort(provider, model string) (string, string) {
	if base, effort, _, ok := m.configuredModelEffort(provider, model); ok {
		return base, effort
	}
	if base, effort, _, ok := m.cachedOllamaModelEffort(provider, model); ok {
		return base, effort
	}
	model = strings.TrimSpace(model)
	// Dynamic provider metadata must be consulted exact-first. A natural model
	// ID can itself end in an effort word (qwen3.8-max), while its variants add
	// another suffix (qwen3.8-max-high and qwen3.8-max-max).
	for _, metadata := range m.modelMetadata {
		base := strings.TrimSpace(metadata.ID)
		if strings.EqualFold(model, base) && len(metadata.ReasoningEfforts) > 0 {
			return base, ""
		}
	}
	for _, metadata := range m.modelMetadata {
		base := strings.TrimSpace(metadata.ID)
		for _, effort := range normalizedModelReasoningEfforts(metadata.ReasoningEfforts) {
			if strings.EqualFold(model, base+"-"+effort) {
				return base, effort
			}
		}
	}
	return llm.BaseModelAndEffortForProvider(provider, model)
}

func (m *Model) configuredEffortForCycle(provider, model string) (string, bool) {
	if m == nil || m.config == nil {
		return "", false
	}
	entry, ok := config.ModelConfigForProviderModel(m.config, provider, model)
	if !ok || len(entry.ReasoningEfforts) == 0 {
		return "", false
	}
	_, effort := m.baseModelAndEffort(provider, model)
	if effort != "" {
		return effort, true
	}
	defaultEffort := strings.ToLower(strings.TrimSpace(entry.DefaultReasoningEffort))
	for _, supported := range entry.ReasoningEfforts {
		if strings.EqualFold(strings.TrimSpace(supported), defaultEffort) {
			return defaultEffort, true
		}
	}
	return "", true
}

func (m *Model) reasoningEffortsForModel(provider, model string) []string {
	if _, _, efforts, ok := m.configuredModelEffort(provider, model); ok {
		return efforts
	}
	if _, _, efforts, ok := m.cachedOllamaModelEffort(provider, model); ok {
		return normalizedModelReasoningEfforts(efforts)
	}
	base, _ := m.baseModelAndEffort(provider, model)
	for _, metadata := range m.modelMetadata {
		if strings.EqualFold(strings.TrimSpace(metadata.ID), strings.TrimSpace(base)) && len(metadata.ReasoningEfforts) > 0 {
			return normalizedModelReasoningEfforts(metadata.ReasoningEfforts)
		}
	}
	return llm.ReasoningEffortsForProviderModel(provider, model)
}

func (m *Model) configuredModelEffort(provider, model string) (base, effort string, efforts []string, ok bool) {
	if m == nil || m.config == nil {
		return "", "", nil, false
	}
	pc, exists := m.config.Providers[strings.TrimSpace(provider)]
	if !exists || len(pc.ModelConfigs) == 0 {
		return "", "", nil, false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", nil, false
	}
	modelLower := strings.ToLower(model)
	for _, entry := range pc.ModelConfigs {
		efforts = normalizedModelReasoningEfforts(entry.ReasoningEfforts)
		if len(efforts) == 0 {
			continue
		}
		for _, name := range configuredModelNamesForEffort(entry) {
			nameLower := strings.ToLower(name)
			if modelLower == nameLower {
				return name, "", efforts, true
			}
			for _, candidateEffort := range efforts {
				suffix := "-" + strings.ToLower(candidateEffort)
				if strings.HasSuffix(modelLower, suffix) && strings.TrimSuffix(modelLower, suffix) == nameLower {
					return name, candidateEffort, efforts, true
				}
			}
		}
	}
	return "", "", nil, false
}

func configuredModelNamesForEffort(entry config.ProviderModelConfig) []string {
	seen := make(map[string]bool, 2)
	var names []string
	appendName := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	appendName(entry.DisplayName())
	appendName(entry.ID)
	return names
}

func normalizedModelReasoningEfforts(efforts []string) []string {
	seen := make(map[string]bool, len(efforts))
	out := make([]string, 0, len(efforts))
	for _, effort := range efforts {
		effort = strings.ToLower(strings.TrimSpace(effort))
		if effort == "" || seen[effort] {
			continue
		}
		seen[effort] = true
		out = append(out, effort)
	}
	return out
}

func (m *Model) pendingStreamEffortStatus() (label string, applied bool, ok bool) {
	if m == nil || m.pendingStreamModelSwitch == nil {
		return "", false, false
	}
	provider := strings.TrimSpace(m.pendingStreamModelSwitch.provider)
	model := strings.TrimSpace(m.pendingStreamModelSwitch.model)
	if provider == "" || model == "" {
		return "", false, false
	}
	_, effort := m.baseModelAndEffort(provider, model)
	if effort == "" {
		effort = "default"
	}
	return effort, m.pendingStreamModelSwitch.applied, true
}

func (m *Model) markPendingStreamModelSwitchApplied(model string) bool {
	if m == nil || m.pendingStreamModelSwitch == nil {
		return false
	}
	if strings.TrimSpace(m.pendingStreamModelSwitch.model) != strings.TrimSpace(model) {
		return false
	}
	m.pendingStreamModelSwitch.applied = true
	return true
}

func (m *Model) clearPendingStreamModelSwitch() {
	m.pendingStreamModelSwitch = nil
	if m.engine != nil {
		m.engine.ClearPendingRequestModelSwitch()
	}
}

func (m *Model) queuePendingStreamModelSwitch(provider, model string) {
	m.pendingStreamModelSwitch = &pendingStreamModelSwitch{provider: provider, model: model}
	currentProvider, _ := m.currentProviderAndModel()
	if m.engine != nil && provider == currentProvider {
		m.engine.QueueRequestModelSwitch(model)
	}
}

func (m *Model) cmdPro(args []string) (tea.Model, tea.Cmd) {
	m.setTextareaValue("")
	provider, model := m.currentProviderAndModel()
	supported := llm.SupportsReasoningMode(provider, model)
	current := "off"
	if m.sess != nil && strings.EqualFold(m.sess.ReasoningMode, "pro") {
		current = "on"
	}
	requested := "status"
	if len(args) > 0 {
		requested = strings.ToLower(strings.TrimSpace(args[0]))
	}
	if requested == "status" {
		if !supported && current == "on" {
			m.sess.ReasoningMode = ""
			if m.store != nil {
				_ = m.store.Update(context.Background(), m.sess)
			}
			return m.showFooterWarning("Pro mode was disabled because the current provider/model does not support it.")
		}
		return m.showFooterMuted(fmt.Sprintf("Pro mode: %s (OpenAI API GPT-5.6 only)", current))
	}
	if requested != "on" && requested != "off" {
		return m.showFooterWarning("Usage: /pro [on|off|status]")
	}
	if requested == "on" && !supported {
		return m.showFooterWarning(fmt.Sprintf("Pro mode is not supported by %s:%s; it is available only through the OpenAI API GPT-5.6 models.", provider, model))
	}
	if m.sess == nil {
		return m.showFooterWarning("Cannot change Pro mode: no active session.")
	}
	if requested == "on" {
		m.sess.ReasoningMode = "pro"
	} else {
		m.sess.ReasoningMode = ""
	}
	if m.store != nil {
		_ = m.store.Update(context.Background(), m.sess)
	}
	return m.showFooterMuted("Pro mode: " + requested)
}

func (m *Model) cmdEffort(args []string) (tea.Model, tea.Cmd) {
	provider, model := m.currentProviderAndModel()

	m.setTextareaValue("")
	if provider == "" || model == "" {
		return m.showFooterWarning("Cannot switch effort: no current provider/model is active.")
	}
	if m.streaming && len(args) == 0 {
		if queued, applied, ok := m.pendingStreamEffortStatus(); ok {
			_, currentEffort := m.baseModelAndEffort(provider, model)
			if applied {
				currentEffort = queued
			}
			if currentEffort == "" {
				currentEffort = "default"
			}
			efforts := m.reasoningEffortsForModel(provider, model)
			available := append(cloneStrings(efforts), "default")
			queuedText := fmt.Sprintf("Queued: %s (applies at next model turn)", queued)
			if applied {
				queuedText = fmt.Sprintf("Active for current run: %s (persists after response)", queued)
			}
			return m.showFooterMuted(fmt.Sprintf("Current effort: %s. %s. Available: %s. Usage: /effort <value>", currentEffort, queuedText, strings.Join(available, ", ")))
		}
	}

	resolved := m.resolveEffortSwitch(provider, model, args)
	if m.streaming && resolved.already {
		if m.pendingStreamModelSwitch != nil {
			m.clearPendingStreamModelSwitch()
			return m.showFooterMuted(fmt.Sprintf("Effort %s already active; cleared queued effort change.", resolved.label))
		}
		return m.showEffortResolutionMessage(resolved)
	}
	if !resolved.ok {
		return m.showEffortResolutionMessage(resolved)
	}

	if m.streaming {
		m.queuePendingStreamModelSwitch(provider, resolved.targetModel)
		return m.showFooterMuted(fmt.Sprintf("Effort %s queued; will apply at the next model turn.", resolved.label))
	}

	return m.switchEffortResolved(resolved, false)
}

func (m *Model) cycleEffort() (tea.Model, tea.Cmd) {
	provider, model := m.currentProviderAndModelForEffortCycle()
	if provider == "" || model == "" {
		return m.showFooterWarning("Cannot switch effort: no current provider/model is active.")
	}

	_, currentEffort := m.baseModelAndEffort(provider, model)
	efforts := m.reasoningEffortsForModel(provider, model)
	if len(efforts) == 0 {
		return m.showFooterWarning(fmt.Sprintf("Model %s:%s does not expose switchable reasoning efforts.", provider, model))
	}

	next := efforts[0]
	if configuredEffort, configuredCycle := m.configuredEffortForCycle(provider, model); configuredCycle {
		currentEffort = configuredEffort
		if idx := slices.Index(efforts, currentEffort); idx >= 0 {
			next = efforts[(idx+1)%len(efforts)]
		}
	} else if currentEffort != "" {
		// Preserve the legacy cycle for curated/provider-wide effort metadata,
		// where the bare model remains a distinct "default" state.
		if idx := slices.Index(efforts, currentEffort); idx >= 0 {
			if idx == len(efforts)-1 {
				next = "default"
			} else {
				next = efforts[idx+1]
			}
		}
	}

	draft := m.captureComposerSnapshot()
	if m.streaming {
		resolved := m.resolveEffortSwitch(provider, model, []string{next})
		if !resolved.ok {
			return m.showEffortResolutionMessage(resolved)
		}
		m.queuePendingStreamModelSwitch(provider, resolved.targetModel)
		_, cmd := m.showFooterMuted(fmt.Sprintf("Effort %s queued; will apply at the next model turn.", resolved.label))
		m.restoreComposerSnapshot(draft)
		return m, cmd
	}

	result, cmd := m.switchEffort(provider, model, []string{next}, true)
	if rm, ok := result.(*Model); ok {
		rm.restoreComposerSnapshot(draft)
		return rm, cmd
	}
	return result, cmd
}

type effortSwitchResolution struct {
	provider    string
	targetModel string
	label       string
	message     string
	tone        string
	ok          bool
	already     bool
}

func (m *Model) resolveEffortSwitch(provider, model string, args []string) effortSwitchResolution {
	base, currentEffort := m.baseModelAndEffort(provider, model)
	efforts := m.reasoningEffortsForModel(provider, model)
	if len(args) == 0 {
		if len(efforts) == 0 {
			return effortSwitchResolution{message: fmt.Sprintf("Model %s:%s does not expose switchable reasoning efforts.", provider, model), tone: "warning"}
		}
		current := currentEffort
		if current == "" {
			current = "default"
		}
		available := append(cloneStrings(efforts), "default")
		return effortSwitchResolution{message: fmt.Sprintf("Current effort: %s. Available: %s. Usage: /effort <value>", current, strings.Join(available, ", ")), tone: "muted"}
	}

	requested := strings.ToLower(strings.TrimSpace(args[0]))
	if requested == "" {
		return effortSwitchResolution{message: "Usage: /effort <value>", tone: "warning"}
	}

	label := requested
	var targetModel string
	switch requested {
	case "default", "auto":
		targetModel = base
		label = "default"
	case "none", "off":
		if slices.Contains(efforts, "none") {
			targetModel = base + "-none"
			label = "none"
		} else {
			targetModel = base
			label = "default"
		}
	default:
		if len(efforts) == 0 || !slices.Contains(efforts, requested) {
			allowed := append(cloneStrings(efforts), "default")
			allowedText := strings.Join(allowed, ", ")
			if allowedText == "" {
				allowedText = "none"
			}
			return effortSwitchResolution{message: fmt.Sprintf("Unsupported effort %q for %s:%s. Available: %s.", requested, provider, base, allowedText), tone: "warning"}
		}
		targetModel = base + "-" + requested
	}

	if targetModel == model {
		return effortSwitchResolution{
			provider:    provider,
			targetModel: targetModel,
			label:       label,
			message:     fmt.Sprintf("Effort already %s for %s:%s.", label, provider, model),
			tone:        "muted",
			already:     true,
		}
	}

	return effortSwitchResolution{
		provider:    provider,
		targetModel: targetModel,
		label:       label,
		ok:          true,
	}
}

func (m *Model) showEffortResolutionMessage(resolved effortSwitchResolution) (tea.Model, tea.Cmd) {
	if resolved.tone == "warning" {
		return m.showFooterWarning(resolved.message)
	}
	return m.showFooterMuted(resolved.message)
}

func (m *Model) switchEffort(provider, model string, args []string, deferMarker bool) (tea.Model, tea.Cmd) {
	resolved := m.resolveEffortSwitch(provider, model, args)
	if !resolved.ok {
		return m.showEffortResolutionMessage(resolved)
	}
	return m.switchEffortResolved(resolved, deferMarker)
}

func (m *Model) switchEffortResolved(resolved effortSwitchResolution, deferMarker bool) (tea.Model, tea.Cmd) {
	if m.config == nil {
		m.config = &config.Config{}
	}
	currentProvider, _ := m.currentProviderAndModel()
	if strings.TrimSpace(currentProvider) == strings.TrimSpace(resolved.provider) {
		return m.switchEffortStateOnly(resolved, deferMarker)
	}
	return m.switchModelWithOptions(resolved.provider+":"+resolved.targetModel, switchModelOptions{deferMarker: deferMarker})
}

func (m *Model) switchEffortStateOnly(resolved effortSwitchResolution, deferMarker bool) (tea.Model, tea.Cmd) {
	oldProvider := strings.TrimSpace(m.providerKey)
	if oldProvider == "" {
		oldProvider = strings.TrimSpace(m.providerName)
	}
	oldModel := strings.TrimSpace(m.modelName)

	m.clearPendingStreamModelSwitch()
	if !deferMarker {
		m.pendingModelSwitch = nil
	}

	m.providerKey = resolved.provider
	m.modelName = resolved.targetModel
	m.refreshEffectiveFastMode()

	if m.sess != nil {
		m.sess.ProviderKey = m.providerKey
		m.sess.Model = resolved.targetModel
		if m.store != nil {
			_ = m.store.Update(context.Background(), m.sess)
		}
	}

	m.recordCurrentModelUse()
	m.setTextareaValue("")
	guardianErr := m.refreshGuardianReviewer(resolved.provider, resolved.targetModel)

	if m.sess != nil && len(m.messages) > 0 {
		marker := llm.ModelSwapMarker{
			FromProvider: oldProvider,
			FromModel:    oldModel,
			ToProvider:   resolved.provider,
			ToModel:      resolved.targetModel,
			Status:       "started",
		}
		if deferMarker {
			m.deferModelSwitchMarker(marker)
		} else {
			m.appendModelSwitchMarker(marker)
		}
	}

	if guardianErr != nil {
		return m.showFooterWarning(fmt.Sprintf("Switched effort to %s, but guardian auto-approval was disabled: %v", resolved.label, guardianErr))
	}
	return m.showFooterMuted(fmt.Sprintf("Switched effort to %s for %s:%s. Next response will use the selected reasoning effort.", resolved.label, resolved.provider, resolved.targetModel))
}

func (m *Model) refreshGuardianReviewer(providerKey, modelName string) error {
	if m.guardianReviewerRefresh == nil {
		return nil
	}
	if err := m.guardianReviewerRefresh(providerKey, modelName); err != nil {
		if m.approvalMgr != nil {
			m.approvalMgr.SetPolicyReviewFunc(nil, nil)
			if m.approvalMgr.ApprovalMode() == tools.ModeAuto {
				m.approvalMgr.SetApprovalMode(tools.ModePrompt)
			}
		}
		return err
	}
	return nil
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

// providerModelEntry captures a concrete provider:model combination.
type providerModelEntry struct {
	provider string
	model    string
	combined string
}

func availableProviderModels(cfg *config.Config) []providerModelEntry {
	var entries []providerModelEntry
	for _, provider := range GetAvailableProviders(cfg) {
		for _, model := range provider.Models {
			entries = append(entries, providerModelEntry{
				provider: provider.Name,
				model:    model,
				combined: provider.Name + ":" + model,
			})
		}
	}
	return entries
}

func matchProviderModels(arg string, cfg *config.Config) []providerModelEntry {
	query := strings.ToLower(strings.TrimSpace(arg))
	entries := availableProviderModels(cfg)
	if query == "" {
		return reorderProviderModelsByRecency(entries, recentProviderModels())
	}

	var direct []providerModelEntry
	for _, entry := range entries {
		providerLower := strings.ToLower(entry.provider)
		modelLower := strings.ToLower(entry.model)
		combinedLower := strings.ToLower(entry.combined)

		match := false
		if strings.Contains(query, ":") {
			match = strings.HasPrefix(combinedLower, query)
		} else {
			match = strings.HasPrefix(providerLower, query) ||
				strings.HasPrefix(modelLower, query) ||
				strings.Contains(modelLower, query) ||
				strings.HasPrefix(combinedLower, query)
		}
		if match {
			direct = append(direct, entry)
		}
	}
	if len(direct) > 0 {
		return reorderProviderModelsByRecency(direct, recentProviderModels())
	}

	return reorderProviderModelsByRecency(fuzzyProviderModelMatches(query, entries), recentProviderModels())
}

func (m *Model) currentProviderModel() string {
	provider, model := m.currentProviderAndModel()
	if provider == "" || model == "" {
		return ""
	}
	return provider + ":" + model
}

func (m *Model) recordCurrentModelUse() {
	if providerModel := m.currentProviderModel(); providerModel != "" {
		config.RecordModelUseAsync(providerModel)
	}
}

func recentProviderModels() []string {
	history, err := config.LoadModelHistory()
	if err != nil {
		return nil
	}
	return config.ModelHistoryOrder(history)
}

func reorderProviderModelsByRecency(entries []providerModelEntry, recent []string) []providerModelEntry {
	if len(entries) <= 1 || len(recent) == 0 {
		return entries
	}

	byCombined := make(map[string]providerModelEntry, len(entries))
	for _, entry := range entries {
		byCombined[entry.combined] = entry
	}

	ordered := make([]providerModelEntry, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, combined := range recent {
		entry, ok := byCombined[combined]
		if !ok {
			continue
		}
		ordered = append(ordered, entry)
		seen[combined] = struct{}{}
	}
	for _, entry := range entries {
		if _, ok := seen[entry.combined]; ok {
			continue
		}
		ordered = append(ordered, entry)
	}
	return ordered
}

func fuzzyProviderModelMatches(query string, entries []providerModelEntry) []providerModelEntry {
	if query == "" || len(entries) == 0 {
		return nil
	}

	rawCandidates := make([]string, len(entries))
	normalizedCandidates := make([]string, len(entries))
	for i, entry := range entries {
		rawCandidates[i] = strings.ToLower(entry.combined)
		normalizedCandidates[i] = normalizeProviderModelMatcher(entry.provider + entry.model)
	}

	orderedIndexes := fuzzyMatchIndexes(query, rawCandidates)
	normalizedQuery := normalizeProviderModelMatcher(query)
	if normalizedQuery != "" && normalizedQuery != query {
		orderedIndexes = appendUniqueIndexes(orderedIndexes, fuzzyMatchIndexes(normalizedQuery, normalizedCandidates)...)
	}

	matches := make([]providerModelEntry, 0, len(orderedIndexes))
	for _, idx := range orderedIndexes {
		matches = append(matches, entries[idx])
	}
	return matches
}

func fuzzyMatchIndexes(query string, candidates []string) []int {
	results := fuzzy.Find(query, candidates)
	indexes := make([]int, 0, len(results))
	for _, match := range results {
		indexes = append(indexes, match.Index)
	}
	return indexes
}

func appendUniqueIndexes(existing []int, additional ...int) []int {
	seen := make(map[int]struct{}, len(existing))
	for _, idx := range existing {
		seen[idx] = struct{}{}
	}
	for _, idx := range additional {
		if _, ok := seen[idx]; ok {
			continue
		}
		existing = append(existing, idx)
		seen[idx] = struct{}{}
	}
	return existing
}

func normalizeProviderModelMatcher(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func providerModelCompletionItems(commandPrefix, arg string, cfg *config.Config) []Command {
	entries := matchProviderModels(arg, cfg)
	items := make([]Command, 0, len(entries))
	for _, entry := range entries {
		items = append(items, Command{
			Name:        commandPrefix + entry.combined,
			Description: entry.provider,
		})
	}
	return items
}

func (m *Model) effortCompletionItems(commandPrefix, partial string) []Command {
	provider := strings.TrimSpace(m.providerKey)
	if provider == "" && m.sess != nil {
		provider = strings.TrimSpace(m.sess.ProviderKey)
	}
	if provider == "" {
		provider = strings.TrimSpace(m.providerName)
	}
	model := strings.TrimSpace(m.modelName)
	if model == "" && m.sess != nil {
		model = strings.TrimSpace(m.sess.Model)
	}
	if provider == "" || model == "" {
		return nil
	}

	base, _ := m.baseModelAndEffort(provider, model)
	efforts := m.reasoningEffortsForModel(provider, model)
	if len(efforts) == 0 {
		return nil
	}

	partial = strings.ToLower(strings.TrimSpace(partial))
	items := make([]Command, 0, len(efforts)+2)
	appendOption := func(option, target string) {
		if partial != "" && !strings.HasPrefix(option, partial) {
			return
		}
		desc := "switch to " + target
		if target == model {
			desc = "current"
		}
		items = append(items, Command{Name: commandPrefix + option, Description: desc})
	}
	for _, effort := range efforts {
		appendOption(effort, base+"-"+effort)
	}
	appendOption("default", base)
	appendOption("auto", base)
	return items
}

func resolveProviderModelArg(arg string, cfg *config.Config, fallbackProvider string) (string, bool) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", false
	}
	if strings.Contains(arg, ":") {
		return arg, true
	}
	if matches := matchProviderModels(arg, cfg); len(matches) > 0 {
		return matches[0].combined, true
	}
	if fallbackProvider != "" {
		return fallbackProvider + ":" + arg, true
	}
	return "", false
}

// toggleSearch toggles web search and persists to session.
// Called by both Ctrl+S and /search command.
func (m *Model) toggleSearch() {
	m.searchEnabled = !m.searchEnabled

	// Persist to session
	if m.sess != nil {
		m.sess.Search = m.searchEnabled
		if m.store != nil {
			_ = m.store.Update(context.Background(), m.sess)
		}
	}
}

func (m *Model) cmdSearch() (tea.Model, tea.Cmd) {
	m.toggleSearch()
	m.setTextareaValue("")

	status := "disabled"
	if m.searchEnabled {
		status = "enabled"
	}
	return m.showFooterMuted(fmt.Sprintf("Web search %s.", status))
}

func (m *Model) cmdFast() (tea.Model, tea.Cmd) {
	m.setTextareaValue("")
	return m.toggleFast()
}

// toggleFast also resolves deferred metadata loads without clearing a new draft.
func (m *Model) toggleFast() (tea.Model, tea.Cmd) {
	if !m.supportsServiceTierToggle() {
		return m.showFooterError(fmt.Sprintf("Fast mode is not supported for %s:%s.", m.providerKey, m.modelName))
	}

	// If fast is currently effective (from provider config or a prior /fast), /fast
	// explicitly clears the provider default for this chat.
	m.refreshEffectiveFastMode()
	if m.fastMode {
		m.setFastOverride(serviceTierClear)
		return m.showFooterMuted(m.fastToggleMessage(false))
	}

	// OpenAI and cursor-bin support fast mode at the request layer without live
	// catalog metadata. OpenAI sends Responses service_tier; cursor-bin appends
	// the Cursor model -fast suffix. Let users opt in and let the provider reject
	// unsupported models, just as provider-level config does.
	switch m.providerType() {
	case config.ProviderTypeOpenAI, config.ProviderTypeCursorBin:
		m.setFastOverride(serviceTierFast)
		return m.showFooterSuccess(m.fastToggleMessage(true))
	}

	if !m.fastMetadataLoaded {
		m.pendingFastToggle = true
		if m.fastMetadataLoading {
			return m.showFooterMuted("Loading model metadata…")
		}
		if cmd := m.loadModelMetadataCmd(); cmd != nil {
			return m.showFooterMutedWithCmd("Loading model metadata…", cmd)
		}
		return m.showFooterError("Could not load model metadata; fast support unknown.")
	}
	if !m.currentModelSupportsFast() {
		return m.showFooterError(fmt.Sprintf("Fast mode is not supported for %s:%s.", m.providerKey, m.modelName))
	}
	m.setFastOverride(serviceTierFast)
	return m.showFooterSuccess(m.fastToggleMessage(true))
}

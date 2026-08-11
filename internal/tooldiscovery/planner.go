package tooldiscovery

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
)

const (
	ToolSearchName          = "tool_search"
	MaxSearchResults        = 5
	MaxActiveDeferred       = 16
	maxRecentEvents         = 16
	maxPlannerSessionStates = 256
)

type Mode string

const (
	ModeAuto     Mode = "auto"
	ModeEager    Mode = "eager"
	ModeDeferred Mode = "deferred"
)

type Strategy string

const (
	StrategyAuto      Strategy = "auto"
	StrategyPortable  Strategy = "portable"
	StrategyNative    Strategy = "native"
	StrategyDelegated Strategy = "delegated"
)

type activeToolState struct {
	SchemaHash  string
	ActivatedBy string
	ActivatedAt time.Time
	LastUsed    uint64
	Pinned      bool
	Executed    bool
}

type sessionState struct {
	mode            Mode
	strategy        Strategy
	active          map[string]activeToolState
	sent            map[string]string
	lastUsed        uint64
	lastAccess      uint64
	catalogueHash   string
	resetRequired   string
	fallbackCount   int
	fallbackReason  string
	lastDiagnostics llm.ToolDiscoveryDiagnostics
	recent          []llm.ToolActivationDiagnostic
}

// Planner owns catalogue indexing, session activation, and authoritative MCP
// provider visibility. Engine queues are only per-run delivery.
type Planner struct {
	manager   *mcp.Manager
	engine    *llm.Engine
	engineMu  sync.RWMutex
	mode      Mode
	strategy  Strategy
	threshold int
	index     *searchIndex

	mu            sync.Mutex
	sessions      map[string]*sessionState
	runs          map[string]string
	runStrategies map[string]Strategy
	fallbackUsed  map[string]bool
	searchAllowed map[string]bool
	stateClock    uint64
	registered    map[string]string
	warnedAlways  map[string]uint64
}

func NewPlanner(cfg config.ToolDiscoveryConfig, manager *mcp.Manager, engine *llm.Engine) (*Planner, error) {
	if manager == nil || engine == nil {
		return nil, fmt.Errorf("tool discovery requires an MCP manager and engine")
	}
	mode := Mode(strings.ToLower(strings.TrimSpace(cfg.Mode)))
	if mode == "" {
		mode = ModeAuto
	}
	if mode != ModeAuto && mode != ModeEager && mode != ModeDeferred {
		return nil, fmt.Errorf("invalid tool discovery mode %q", cfg.Mode)
	}
	strategy := Strategy(strings.ToLower(strings.TrimSpace(cfg.Strategy)))
	if strategy == "" {
		strategy = StrategyAuto
	}
	if strategy != StrategyAuto && strategy != StrategyPortable && strategy != StrategyNative {
		return nil, fmt.Errorf("invalid tool discovery strategy %q", cfg.Strategy)
	}
	threshold := cfg.Threshold
	if cfg.Mode == "" && threshold == 0 {
		threshold = config.DefaultToolDiscoveryThreshold
	}
	if threshold < 0 {
		return nil, fmt.Errorf("tool discovery threshold must be non-negative")
	}
	snapshot := manager.CatalogueSnapshot()
	if snapshot == nil {
		snapshot = &mcp.CatalogueSnapshot{}
	}
	planner := &Planner{
		manager:       manager,
		engine:        engine,
		mode:          mode,
		strategy:      strategy,
		threshold:     threshold,
		index:         newSearchIndex(snapshot.Tools, snapshot.Generation),
		sessions:      make(map[string]*sessionState),
		runs:          make(map[string]string),
		runStrategies: make(map[string]Strategy),
		fallbackUsed:  make(map[string]bool),
		searchAllowed: make(map[string]bool),
		registered:    make(map[string]string),
		warnedAlways:  make(map[string]uint64),
	}
	planner.syncCatalogue(snapshot)
	manager.SetCatalogueChangeHandler(planner.handleCatalogueEvent)
	engine.SetToolSurfacePlanner(planner)
	return planner, nil
}

// AttachEngine moves planner delivery/registry wiring to a replacement engine
// while preserving durable session activation state.
func (p *Planner) AttachEngine(engine *llm.Engine) {
	if p == nil || engine == nil {
		return
	}
	p.engineMu.Lock()
	oldEngine := p.engine
	if oldEngine == engine {
		p.engineMu.Unlock()
		return
	}
	p.engine = engine
	p.engineMu.Unlock()

	p.mu.Lock()
	oldRegistered := make([]string, 0, len(p.registered))
	for name := range p.registered {
		oldRegistered = append(oldRegistered, name)
	}
	p.registered = make(map[string]string)
	p.mu.Unlock()
	if oldEngine != nil && oldEngine.ClearToolSurfacePlanner(p) {
		for _, name := range oldRegistered {
			oldEngine.UnregisterTool(name)
		}
		oldEngine.UnregisterTool(ToolSearchName)
	}
	p.syncCatalogue(p.manager.CatalogueSnapshot())
	engine.SetToolSurfacePlanner(p)
}

func (p *Planner) currentEngine() *llm.Engine {
	p.engineMu.RLock()
	defer p.engineMu.RUnlock()
	return p.engine
}

func (p *Planner) handleCatalogueEvent(event mcp.CatalogueEvent) {
	if event.Err != nil {
		slog.Warn("MCP tool catalogue refresh failed; retaining previous catalogue", "server", event.Server, "error", event.Err)
		return
	}
	if event.Snapshot == nil {
		return
	}
	p.syncCatalogue(event.Snapshot)
}

func (p *Planner) syncCatalogue(snapshot *mcp.CatalogueSnapshot) {
	if snapshot == nil {
		return
	}
	engine := p.currentEngine()
	if engine == nil {
		return
	}
	current := make(map[string]mcp.CatalogTool, len(snapshot.Tools))
	for _, tool := range snapshot.Tools {
		current[tool.Name] = tool
		engine.Tools().RegisterDeferred(mcp.NewCatalogMCPTool(p.manager, tool))
	}

	p.mu.Lock()
	for name := range p.registered {
		if _, ok := current[name]; !ok {
			engine.UnregisterTool(name)
		}
	}
	for _, state := range p.sessions {
		for name, active := range state.active {
			tool, exists := current[name]
			if !exists {
				delete(state.active, name)
				if _, sent := state.sent[name]; sent {
					state.resetRequired = "active MCP tool was removed from the catalogue"
				}
				continue
			}
			if active.SchemaHash != tool.SchemaHash {
				active.SchemaHash = tool.SchemaHash
				state.active[name] = active
				if _, sent := state.sent[name]; sent {
					state.resetRequired = "an active MCP tool schema changed"
				}
			}
		}
	}
	p.registered = make(map[string]string, len(current))
	for name, tool := range current {
		p.registered[name] = tool.SchemaHash
	}
	p.mu.Unlock()
	p.index.replace(snapshot.Tools, snapshot.Generation)
}

func (p *Planner) stateKey(sessionID, runID string) string {
	if strings.TrimSpace(sessionID) != "" {
		return "session:" + strings.TrimSpace(sessionID)
	}
	return "run:" + runID
}

func (p *Planner) stateLocked(key string) *sessionState {
	p.stateClock++
	if state := p.sessions[key]; state != nil {
		state.lastAccess = p.stateClock
		return state
	}
	if strings.HasPrefix(key, "session:") {
		persistentCount := 0
		activeKeys := make(map[string]bool, len(p.runs))
		for _, activeKey := range p.runs {
			activeKeys[activeKey] = true
		}
		victimKey := ""
		victimAccess := ^uint64(0)
		for candidateKey, candidate := range p.sessions {
			if !strings.HasPrefix(candidateKey, "session:") {
				continue
			}
			persistentCount++
			if activeKeys[candidateKey] || candidate.lastAccess >= victimAccess {
				continue
			}
			victimKey = candidateKey
			victimAccess = candidate.lastAccess
		}
		if persistentCount >= maxPlannerSessionStates && victimKey != "" {
			delete(p.sessions, victimKey)
		}
	}
	state := &sessionState{
		active:     make(map[string]activeToolState),
		sent:       make(map[string]string),
		lastAccess: p.stateClock,
	}
	p.sessions[key] = state
	return state
}

func (p *Planner) BeginRun(_ context.Context, provider llm.Provider, req *llm.Request, runID string) (string, error) {
	if req == nil {
		return "", fmt.Errorf("request is nil")
	}
	key := p.stateKey(req.SessionID, runID)
	p.mu.Lock()
	p.runs[runID] = key
	p.searchAllowed[runID] = false
	p.stateLocked(key)
	p.mu.Unlock()
	p.restoreNativeHistory(req, key)
	return p.selectSurface(provider, req, key, runID, true)
}

func (p *Planner) restoreNativeHistory(req *llm.Request, key string) {
	if req == nil {
		return
	}
	snapshot := p.manager.CatalogueSnapshot()
	engine := p.currentEngine()
	if snapshot == nil || engine == nil {
		return
	}
	current := make(map[string]mcp.CatalogTool, len(snapshot.Tools))
	for _, tool := range snapshot.Tools {
		if engine.IsToolAllowed(tool.Name) {
			current[tool.Name] = tool
		}
	}
	calls := make(map[string]bool)
	for _, msg := range req.Messages {
		for _, part := range msg.Parts {
			if part.Type == llm.PartDiscoveryCall && part.DiscoveryCall != nil {
				calls[part.DiscoveryCall.ID] = true
			}
		}
	}
	staleReason := ""
	resolved := make(map[string]mcp.CatalogTool)
	for mi := range req.Messages {
		for pi := range req.Messages[mi].Parts {
			part := &req.Messages[mi].Parts[pi]
			if part.Type != llm.PartDiscoveryOutput || part.DiscoveryOutput == nil {
				continue
			}
			if !calls[part.DiscoveryOutput.CallID] {
				staleReason = "native discovery output is missing its call replay item"
				break
			}
			for ti := range part.DiscoveryOutput.Tools {
				selected := &part.DiscoveryOutput.Tools[ti]
				tool, ok := current[selected.Spec.Name]
				if !ok {
					staleReason = "a native-discovered MCP tool is no longer authorised or available"
					break
				}
				if selected.SchemaHash != tool.SchemaHash {
					staleReason = "a native-discovered MCP tool schema changed"
					break
				}
				selected.Spec = tool.ToolSpec()
				resolved[tool.Name] = tool
			}
			part.DiscoveryOutput.CatalogueHash = snapshot.Hash
			part.DiscoveryOutput.CatalogueGen = snapshot.Generation
			if staleReason != "" {
				break
			}
		}
		if staleReason != "" {
			break
		}
	}
	p.mu.Lock()
	state := p.stateLocked(key)
	if staleReason != "" {
		for name, active := range state.active {
			if !active.Pinned {
				delete(state.active, name)
				delete(state.sent, name)
			}
		}
		state.resetRequired = staleReason
	} else {
		for name, tool := range resolved {
			active := state.active[name]
			active.SchemaHash = tool.SchemaHash
			if active.ActivatedBy == "" {
				active.ActivatedBy = "native replay"
				active.ActivatedAt = time.Now().UTC()
			}
			state.active[name] = active
			state.sent[name] = tool.SchemaHash
		}
	}
	p.mu.Unlock()
	if staleReason != "" {
		for mi := range req.Messages {
			parts := req.Messages[mi].Parts[:0]
			for _, part := range req.Messages[mi].Parts {
				if part.Type != llm.PartDiscoveryCall && part.Type != llm.PartDiscoveryOutput {
					parts = append(parts, part)
				}
			}
			req.Messages[mi].Parts = parts
		}
	}
}

func (p *Planner) PrepareTurn(_ context.Context, provider llm.Provider, req *llm.Request, runID string, attempt, maxTurns int) (string, error) {
	p.mu.Lock()
	key := p.runs[runID]
	p.mu.Unlock()
	if key == "" {
		return "", fmt.Errorf("unknown discovery run %q", runID)
	}
	resetReason, err := p.selectSurface(provider, req, key, runID, false)
	if err != nil {
		return "", err
	}
	// The engine's last attempt is answer-only, so discovery must finish before
	// the penultimate attempt in order to leave one turn to invoke the loaded
	// schema and one final turn to answer.
	searchCutoff := llm.ToolDiscoverySearchCutoff(maxTurns)
	forcedSearch := req.ToolChoice.Mode == llm.ToolChoiceName && req.ToolChoice.Name == ToolSearchName
	if attempt == maxTurns-1 && attempt > 0 && req.LastTurnToolChoice != nil {
		forcedSearch = forcedSearch || (req.LastTurnToolChoice.Mode == llm.ToolChoiceName && req.LastTurnToolChoice.Name == ToolSearchName)
	}
	searchVisible := attempt < searchCutoff && (hasTool(req.Tools, ToolSearchName) || req.NativeToolDiscovery != nil)
	if attempt >= searchCutoff {
		req.Tools = withoutTool(req.Tools, ToolSearchName)
		req.NativeToolDiscovery = nil
		if forcedSearch {
			p.mu.Lock()
			p.searchAllowed[runID] = false
			p.mu.Unlock()
			return "", fmt.Errorf("tool_search cannot be forced after the discovery cutoff")
		}
	} else if attempt == searchCutoff-1 && hasTool(req.Tools, ToolSearchName) && !messagesContain(req.Messages, "Use tool_search before the discovery cutoff") {
		req.Messages = append(req.Messages, llm.SystemText("Use tool_search now if another capability must be loaded; discovery closes before the answer-only final attempt."))
	}
	p.mu.Lock()
	if p.runs[runID] == key {
		p.searchAllowed[runID] = searchVisible
	}
	p.mu.Unlock()
	return resetReason, nil
}

// ResolveMode applies the public count-only auto boundary. Equality remains eager.
func ResolveMode(configured Mode, threshold, authorisedMCPCount int, externalHarness bool) Mode {
	if externalHarness || configured == ModeEager {
		return ModeEager
	}
	if configured == ModeDeferred {
		return ModeDeferred
	}
	if authorisedMCPCount <= threshold {
		return ModeEager
	}
	return ModeDeferred
}

func (p *Planner) resolveStrategy(provider llm.Provider, model string) (Strategy, string, error) {
	if isFixedBridgeProvider(provider) {
		return StrategyDelegated, "fixed CLI bridge owns its eager/delegated tool surface", nil
	}
	support := llm.NativeToolDiscoverySupport{Reason: fmt.Sprintf("provider %q has no native tool discovery adapter", provider.Name())}
	if native, ok := provider.(llm.NativeToolDiscoveryProvider); ok {
		support = native.NativeToolDiscoverySupport(model)
	}
	switch p.strategy {
	case StrategyPortable:
		return StrategyPortable, "portable strategy was forced by configuration", nil
	case StrategyNative:
		if !support.Supported {
			return "", "", fmt.Errorf("tool_discovery.strategy native is unsupported for provider/model %q/%q: %s", provider.Name(), model, support.Reason)
		}
		return StrategyNative, support.Reason, nil
	default:
		if support.Supported {
			return StrategyNative, support.Reason, nil
		}
		return StrategyPortable, "native support is not proven; using portable tool_search: " + support.Reason, nil
	}
}

func (p *Planner) selectSurface(provider llm.Provider, req *llm.Request, key, runID string, beginning bool) (string, error) {
	snapshot := p.manager.CatalogueSnapshot()
	if snapshot == nil {
		return "", fmt.Errorf("MCP catalogue is unavailable")
	}
	initialToolNames := make(map[string]bool, len(req.Tools))
	for _, spec := range req.Tools {
		initialToolNames[spec.Name] = true
	}
	caps := provider.Capabilities()
	if !caps.ToolCalls {
		req.Tools = removeMCPAndSearch(req.Tools, snapshot.Tools)
		return "", nil
	}

	engine := p.currentEngine()
	if engine == nil {
		return "", fmt.Errorf("tool discovery engine is unavailable")
	}
	authorized := make(map[string]mcp.CatalogTool, len(snapshot.Tools))
	for _, tool := range snapshot.Tools {
		if engine.IsToolAllowed(tool.Name) {
			authorized[tool.Name] = tool
		}
	}
	strategy, strategyReason, err := p.resolveStrategy(provider, req.Model)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	if selected := p.runStrategies[runID]; selected != "" {
		strategy = selected
		if selected == StrategyPortable && p.fallbackUsed[runID] {
			strategyReason = "native discovery fell back to portable before committed output"
		}
	} else {
		p.runStrategies[runID] = strategy
	}
	p.mu.Unlock()
	externalHarness := strategy == StrategyDelegated
	resolved := ResolveMode(p.mode, p.threshold, len(authorized), externalHarness)
	removedNativeReplay := false
	if strategy != StrategyNative {
		removedNativeReplay = stripNativeDiscoveryParts(req)
	}

	always := p.alwaysLoadSet(snapshot)
	explicit, explicitPresent := engine.AllowedToolsFilter()
	explicitSet := make(map[string]bool, len(explicit))
	if explicitPresent {
		for _, name := range explicit {
			explicitSet[name] = true
		}
	}
	if req.Responses != nil && req.Responses.ProgrammaticToolCalling.Enabled {
		for _, name := range req.Responses.ProgrammaticToolCalling.Tools {
			explicitSet[name] = true
		}
	}
	forced := ""
	if req.ToolChoice.Mode == llm.ToolChoiceName {
		forced = req.ToolChoice.Name
	} else if req.LastTurnToolChoice != nil && req.LastTurnToolChoice.Mode == llm.ToolChoiceName {
		forced = req.LastTurnToolChoice.Name
	}
	if forced != "" {
		if _, isMCP := authorized[forced]; !isMCP && containsMCPName(snapshot.Tools, forced) {
			return "", fmt.Errorf("selected MCP tool %q is unavailable or denied", forced)
		}
	}
	pinnedNow := make(map[string]bool, len(authorized))
	for name := range authorized {
		pinnedNow[name] = resolved == ModeEager || always[name] || explicitSet[name] || forced == name
	}

	p.mu.Lock()
	state := p.stateLocked(key)
	priorMode := state.mode
	priorStrategy := state.strategy
	state.mode = resolved
	state.strategy = strategy
	if removedNativeReplay {
		state.resetRequired = "native discovery replay was removed for the portable strategy"
	}
	for name, active := range state.active {
		tool, ok := authorized[name]
		if !ok {
			delete(state.active, name)
			if _, sent := state.sent[name]; sent {
				state.resetRequired = "tool policy removed an active MCP tool"
			}
			continue
		}
		if active.SchemaHash != tool.SchemaHash {
			active.SchemaHash = tool.SchemaHash
			state.active[name] = active
			if _, sent := state.sent[name]; sent {
				state.resetRequired = "an active MCP tool schema changed"
			}
		}
	}
	for name, active := range state.active {
		if pinnedNow[name] {
			if !active.Pinned {
				active.Pinned = true
				state.active[name] = active
			}
			continue
		}
		if !active.Pinned {
			continue
		}
		if isDynamicActivation(active.ActivatedBy) {
			active.Pinned = false
			state.active[name] = active
			continue
		}
		delete(state.active, name)
		if _, sent := state.sent[name]; sent {
			state.resetRequired = "an MCP tool is no longer pinned"
		}
	}
	for name, tool := range authorized {
		if pinnedNow[name] {
			active := state.active[name]
			active.SchemaHash = tool.SchemaHash
			active.Pinned = true
			if active.ActivatedBy == "" {
				active.ActivatedBy = pinReason(always[name], explicitSet[name], forced == name, resolved == ModeEager)
				active.ActivatedAt = time.Now().UTC()
			}
			state.active[name] = active
		}
	}
	if beginning && priorMode != "" && priorMode != resolved && len(state.sent) > 0 {
		state.resetRequired = fmt.Sprintf("tool discovery mode changed from %s to %s", priorMode, resolved)
	}
	if beginning && priorStrategy != "" && priorStrategy != strategy && len(state.sent) > 0 {
		state.resetRequired = fmt.Sprintf("tool discovery strategy changed from %s to %s", priorStrategy, strategy)
	}

	base := removeMCPAndSearch(req.Tools, snapshot.Tools)
	activeSpecs := make([]llm.ToolSpec, 0, len(authorized))
	deferredCount := 0
	pinnedCount, activeCount := 0, 0
	pinnedTokens, activeTokens, deferredTokens := 0, 0, 0
	activeHashes := make(map[string]string)
	serverDiagnostics := make(map[string]*llm.ToolDiscoveryServerDiagnostic)
	names := make([]string, 0, len(authorized))
	for name, tool := range authorized {
		names = append(names, name)
		diagnostic := serverDiagnostics[tool.Server]
		if diagnostic == nil {
			diagnostic = &llm.ToolDiscoveryServerDiagnostic{Name: tool.Server, ResolvedMode: string(resolved)}
			serverDiagnostics[tool.Server] = diagnostic
		}
		diagnostic.Total++
	}
	sort.Strings(names)
	for _, name := range names {
		tool := authorized[name]
		serverDiagnostic := serverDiagnostics[tool.Server]
		active, visible := state.active[name]
		if resolved == ModeEager {
			visible = true
		}
		if visible {
			activeHashes[name] = tool.SchemaHash
			if strategy != StrategyNative || active.Pinned || resolved == ModeEager {
				activeSpecs = append(activeSpecs, tool.ToolSpec())
			}
			if active.Pinned {
				pinnedCount++
				pinnedTokens += tool.EstimatedTokens
				serverDiagnostic.Pinned++
			} else {
				activeCount++
				activeTokens += tool.EstimatedTokens
				serverDiagnostic.Active++
			}
		} else {
			deferredCount++
			deferredTokens += tool.EstimatedTokens
			serverDiagnostic.Deferred++
		}
	}
	serverNames := make([]string, 0, len(serverDiagnostics))
	for name := range serverDiagnostics {
		serverNames = append(serverNames, name)
	}
	sort.Strings(serverNames)
	serverDetails := make([]llm.ToolDiscoveryServerDiagnostic, 0, len(serverNames))
	for _, name := range serverNames {
		serverDetails = append(serverDetails, *serverDiagnostics[name])
	}
	req.Tools = append(base, activeSpecs...)
	req.NativeToolDiscovery = nil
	if resolved == ModeDeferred && deferredCount > 0 && strategy == StrategyPortable {
		searchTool := &SearchTool{planner: p}
		engine.Tools().RegisterDeferred(searchTool)
		req.Tools = append(req.Tools, searchTool.Spec())
	} else if resolved == ModeDeferred && strategy == StrategyNative {
		req.NativeToolDiscovery = &llm.NativeToolDiscoveryRequest{Search: (&SearchTool{planner: p}).Spec()}
	}
	toolsAdded := false
	for _, spec := range req.Tools {
		if !initialToolNames[spec.Name] {
			toolsAdded = true
			break
		}
	}
	if len(req.Tools) == 0 && req.NativeToolDiscovery == nil {
		req.ToolChoice = llm.ToolChoice{}
	} else if req.ToolChoice.Mode == "" && (toolsAdded || req.NativeToolDiscovery != nil) {
		req.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	}

	resetReason := state.resetRequired
	if resetReason == "" {
		for name, oldHash := range state.sent {
			newHash, ok := activeHashes[name]
			if !ok {
				resetReason = "an already-sent MCP tool is no longer visible"
				break
			}
			if newHash != oldHash {
				resetReason = "an already-sent MCP tool schema changed"
				break
			}
		}
	}
	state.resetRequired = ""
	for name, hash := range activeHashes {
		state.sent[name] = hash
	}
	for name := range state.sent {
		if _, ok := activeHashes[name]; !ok {
			delete(state.sent, name)
		}
	}
	state.catalogueHash = snapshot.Hash
	reason := fmt.Sprintf("%d authorised MCP tools", len(authorized))
	if p.mode == ModeAuto {
		if resolved == ModeDeferred {
			reason = fmt.Sprintf("%d MCP tools exceed configured threshold %d", len(authorized), p.threshold)
		} else {
			reason = fmt.Sprintf("%d MCP tools are at or below configured threshold %d", len(authorized), p.threshold)
		}
	}
	state.lastDiagnostics = llm.ToolDiscoveryDiagnostics{
		ConfiguredMode:     string(p.mode),
		ResolvedMode:       string(resolved),
		ConfiguredStrategy: string(p.strategy),
		Strategy:           string(strategy),
		Reason:             reason,
		StrategyReason:     strategyReason,
		FallbackCount:      state.fallbackCount,
		FallbackReason:     state.fallbackReason,
		CatalogueHash:      snapshot.Hash,
		CatalogueGen:       snapshot.Generation,
		PinnedCount:        pinnedCount,
		ActiveMCPCount:     activeCount,
		DeferredCount:      deferredCount,
		PinnedTokens:       pinnedTokens,
		ActiveMCPTokens:    activeTokens,
		DeferredTokens:     deferredTokens,
		DynamicActive:      activeCount,
		DynamicLimit:       MaxActiveDeferred,
		Recent:             append([]llm.ToolActivationDiagnostic(nil), state.recent...),
		Servers:            serverDetails,
		ResetReason:        resetReason,
	}
	p.mu.Unlock()

	slog.Debug("MCP tool discovery selection", "session_id", req.SessionID, "provider", provider.Name(), "model", req.Model, "catalogue_generation", snapshot.Generation, "catalogue_hash", snapshot.Hash, "configured_mode", p.mode, "resolved_mode", resolved, "strategy", strategy, "authorised", len(authorized), "active", pinnedCount+activeCount, "deferred", deferredCount, "active_tokens", pinnedTokens+activeTokens, "deferred_tokens", deferredTokens, "reset_reason", resetReason)
	if forced != "" && containsMCPName(snapshot.Tools, forced) && !hasTool(req.Tools, forced) {
		return "", fmt.Errorf("selected MCP tool %q could not be made provider-visible", forced)
	}
	return resetReason, nil
}

func (p *Planner) ResolveNativeToolDiscovery(_ context.Context, runID string, call llm.ToolDiscoveryCall) (llm.ToolDiscoveryOutput, error) {
	p.mu.Lock()
	strategy := p.runStrategies[runID]
	allowed := p.searchAllowed[runID]
	key := p.runs[runID]
	p.mu.Unlock()
	if strategy != StrategyNative || key == "" {
		return llm.ToolDiscoveryOutput{}, fmt.Errorf("native tool discovery is not active for run %q", runID)
	}
	if !allowed {
		return llm.ToolDiscoveryOutput{}, fmt.Errorf("tool_search cannot run after the discovery cutoff")
	}
	input, err := decodeSearchInput(call.Arguments)
	if err != nil {
		return llm.ToolDiscoveryOutput{}, err
	}
	loaded, already, _, _, err := p.activate(runID, input)
	if err != nil {
		return llm.ToolDiscoveryOutput{}, err
	}
	selected := make(map[string]mcp.CatalogTool, len(loaded)+len(already))
	for _, tool := range loaded {
		selected[tool.Name] = tool
	}
	if len(already) > 0 {
		snapshot := p.manager.CatalogueSnapshot()
		if snapshot != nil {
			for _, tool := range snapshot.Tools {
				if containsString(already, tool.Name) {
					selected[tool.Name] = tool
				}
			}
		}
	}
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	output := llm.ToolDiscoveryOutput{CallID: call.ID}
	if snapshot := p.manager.CatalogueSnapshot(); snapshot != nil {
		output.CatalogueHash = snapshot.Hash
		output.CatalogueGen = snapshot.Generation
	}
	for _, name := range names {
		tool := selected[name]
		output.Tools = append(output.Tools, llm.DiscoveredTool{Spec: tool.ToolSpec(), SchemaHash: tool.SchemaHash})
	}
	return output, nil
}

func (p *Planner) FallbackNativeToolDiscovery(runID string, cause error, committed bool) (bool, string) {
	if p == nil || committed || p.strategy != StrategyAuto {
		return false, ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runStrategies[runID] != StrategyNative || p.fallbackUsed[runID] {
		return false, ""
	}
	key := p.runs[runID]
	state := p.sessions[key]
	if key == "" || state == nil {
		return false, ""
	}
	reason := "native discovery failed before committed output"
	if cause != nil {
		reason += ": " + cause.Error()
	}
	p.fallbackUsed[runID] = true
	p.runStrategies[runID] = StrategyPortable
	state.strategy = StrategyPortable
	state.fallbackCount++
	state.fallbackReason = reason
	return true, reason
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func stripNativeDiscoveryParts(req *llm.Request) bool {
	if req == nil {
		return false
	}
	removed := false
	for mi := range req.Messages {
		parts := req.Messages[mi].Parts[:0]
		for _, part := range req.Messages[mi].Parts {
			if part.Type == llm.PartDiscoveryCall || part.Type == llm.PartDiscoveryOutput {
				removed = true
				continue
			}
			parts = append(parts, part)
		}
		req.Messages[mi].Parts = parts
	}
	return removed
}

func (p *Planner) EndRun(runID string) {
	p.mu.Lock()
	key := p.runs[runID]
	delete(p.runs, runID)
	delete(p.runStrategies, runID)
	delete(p.fallbackUsed, runID)
	delete(p.searchAllowed, runID)
	if strings.HasPrefix(key, "run:") {
		delete(p.sessions, key)
	}
	p.mu.Unlock()
}

// CanActivateDeferredTool reports whether the exact forced name is a current,
// authorised MCP catalogue entry with an executable wrapper on the owned engine.
func (p *Planner) CanActivateDeferredTool(name string) bool {
	if p == nil || name == "" {
		return false
	}
	engine := p.currentEngine()
	if engine == nil || !engine.IsToolAllowed(name) {
		return false
	}
	if _, ok := engine.Tools().Get(name); !ok {
		return false
	}
	snapshot := p.manager.CatalogueSnapshot()
	if snapshot == nil {
		return false
	}
	for _, tool := range snapshot.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// AllowsPlannerTool grants only tool_search for a run where PrepareTurn made
// that planner-owned schema provider-visible. It deliberately does not affect
// Engine.IsToolAllowed or authorise arbitrary tool implementations.
func (p *Planner) AllowsPlannerTool(runID, name string) bool {
	if p == nil || name != ToolSearchName || runID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs[runID] != "" && p.searchAllowed[runID]
}

func (p *Planner) ResetSession(sessionID string) {
	p.mu.Lock()
	delete(p.sessions, p.stateKey(sessionID, ""))
	p.mu.Unlock()
}

func (p *Planner) ToolExecuted(sessionID, name string) {
	p.mu.Lock()
	state := p.sessions[p.stateKey(sessionID, "")]
	if state != nil {
		if active, ok := state.active[name]; ok {
			state.lastUsed++
			active.LastUsed = state.lastUsed
			active.Executed = true
			state.active[name] = active
		}
	}
	p.mu.Unlock()
}

func (p *Planner) Diagnostics(sessionID string) llm.ToolDiscoveryDiagnostics {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.sessions[p.stateKey(sessionID, "")]
	if state == nil {
		return llm.ToolDiscoveryDiagnostics{ConfiguredMode: string(p.mode), ConfiguredStrategy: string(p.strategy), DynamicLimit: MaxActiveDeferred}
	}
	diagnostics := state.lastDiagnostics
	diagnostics.Recent = append([]llm.ToolActivationDiagnostic(nil), diagnostics.Recent...)
	diagnostics.Servers = append([]llm.ToolDiscoveryServerDiagnostic(nil), diagnostics.Servers...)
	return diagnostics
}

func (p *Planner) alwaysLoadSet(snapshot *mcp.CatalogueSnapshot) map[string]bool {
	result := make(map[string]bool)
	cfg := p.manager.Config()
	if cfg == nil {
		return result
	}
	available := make(map[string]bool)
	generation := uint64(0)
	if snapshot != nil {
		generation = snapshot.Generation
		for _, tool := range snapshot.Tools {
			available[tool.Name] = true
		}
	}
	for server, serverCfg := range cfg.Servers {
		for _, original := range serverCfg.AlwaysLoad {
			name := server + "__" + original
			if available[name] {
				result[name] = true
				continue
			}
			p.mu.Lock()
			warnedGeneration, warned := p.warnedAlways[name]
			warn := !warned || warnedGeneration != generation
			if warn {
				p.warnedAlways[name] = generation
			}
			p.mu.Unlock()
			if warn {
				slog.Warn("MCP always_load entry is not present in the current catalogue", "server", server, "tool", original)
			}
		}
	}
	return result
}

func isFixedBridgeProvider(provider llm.Provider) bool {
	if provider == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(provider.Credential())) {
	case "claude-bin", "cursor-bin", "grok-bin", "agy-bin":
		return true
	default:
		return false
	}
}

func isDynamicActivation(reason string) bool {
	return strings.HasPrefix(reason, "search: ") || strings.HasPrefix(reason, "exact: ")
}

func pinReason(always, explicit, forced, eager bool) string {
	switch {
	case forced:
		return "forced"
	case always:
		return "always_load"
	case explicit:
		return "explicit"
	case eager:
		return "eager"
	default:
		return "pinned"
	}
}

func removeMCPAndSearch(specs []llm.ToolSpec, tools []mcp.CatalogTool) []llm.ToolSpec {
	mcpNames := make(map[string]bool, len(tools))
	for _, tool := range tools {
		mcpNames[tool.Name] = true
	}
	out := make([]llm.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		if spec.Name != ToolSearchName && !mcpNames[spec.Name] {
			out = append(out, spec)
		}
	}
	return out
}

func withoutTool(specs []llm.ToolSpec, name string) []llm.ToolSpec {
	out := make([]llm.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		if spec.Name != name {
			out = append(out, spec)
		}
	}
	return out
}

func hasTool(specs []llm.ToolSpec, name string) bool {
	for _, spec := range specs {
		if spec.Name == name {
			return true
		}
	}
	return false
}

func containsMCPName(tools []mcp.CatalogTool, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func messagesContain(messages []llm.Message, text string) bool {
	for _, message := range messages {
		if strings.Contains(llm.MessageText(message), text) {
			return true
		}
	}
	return false
}

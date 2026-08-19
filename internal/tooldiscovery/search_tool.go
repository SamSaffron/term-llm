package tooldiscovery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
)

// SearchTool is the automatically allowed local control-plane tool used to load
// complete MCP schemas for the next provider turn.
type SearchTool struct {
	planner *Planner
}

func (t *SearchTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        ToolSearchName,
		Description: "Search the authorised MCP tool catalogue and load matching tool schemas for the next turn. Use query when the exact tool name is unknown, or tool_names when it is known. Keep semantic queries short and focused on one capability; use separate searches for unrelated capabilities. Do not supply both. If both are supplied, tool_names takes precedence. Exact-name batches load valid tools even when another requested name is unavailable. Use this before the final turn when the needed MCP capability is not already available.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Describe one capability with a short focused query when the exact tool name is unknown. Use separate searches for unrelated capabilities. Omit this when using tool_names. Do not supply both.",
				},
				"tool_names": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"maxItems":    8,
					"description": "Exact available tool names to load. Prefer this when names are known and omit query. Do not supply both.",
				},
				"max_results": map[string]any{
					"type":    "integer",
					"minimum": 1,
					"maximum": 8,
					"default": MaxSearchResults,
				},
			},
			"additionalProperties": false,
		},
	}
}

func (t *SearchTool) Preview(args json.RawMessage) string {
	var input searchInput
	if json.Unmarshal(args, &input) == nil {
		if len(input.ToolNames) > 0 {
			return "Loading MCP tools: " + strings.Join(input.ToolNames, ", ")
		}
		if query := strings.TrimSpace(input.Query); query != "" {
			return fmt.Sprintf("Searching MCP tools for %q", query)
		}
	}
	return "Searching MCP tools"
}

type searchInput struct {
	Query      string   `json:"query"`
	ToolNames  []string `json:"tool_names"`
	MaxResults int      `json:"max_results"`
}

func decodeSearchInput(args json.RawMessage) (searchInput, error) {
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	var input searchInput
	if err := decoder.Decode(&input); err != nil {
		return searchInput{}, fmt.Errorf("invalid tool_search input: %w", err)
	}
	input.Query = strings.TrimSpace(input.Query)
	names := make([]string, 0, len(input.ToolNames))
	for _, name := range input.ToolNames {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	input.ToolNames = names
	if len(input.ToolNames) > 0 {
		// Exact names are an unambiguous, authorization-checked selector. Models
		// sometimes redundantly include a semantic query as well; prefer the exact
		// request instead of rejecting the call and wasting an agentic turn.
		input.Query = ""
	} else if input.Query == "" {
		return searchInput{}, fmt.Errorf("provide a non-empty query or non-empty tool_names")
	}
	if utf8.RuneCountInString(input.Query) > 500 {
		return searchInput{}, fmt.Errorf("query is too long: maximum is 500 Unicode code points")
	}
	if len(input.ToolNames) > 8 {
		return searchInput{}, fmt.Errorf("too many tool_names: maximum is 8")
	}
	if input.MaxResults == 0 {
		input.MaxResults = MaxSearchResults
	}
	if input.MaxResults < 1 || input.MaxResults > 8 {
		return searchInput{}, fmt.Errorf("max_results must be between 1 and 8")
	}
	return input, nil
}

func (t *SearchTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	if t == nil || t.planner == nil {
		return llm.ToolOutput{}, fmt.Errorf("tool catalogue is unavailable")
	}
	input, err := decodeSearchInput(args)
	if err != nil {
		return llm.ToolOutput{}, err
	}

	runID := llm.ToolRunIDFromContext(ctx)
	if runID == "" {
		return llm.ToolOutput{}, fmt.Errorf("tool_search is not attached to an active agentic run")
	}
	loaded, already, evicted, omitted, unavailable, label, err := t.planner.activate(runID, input)
	if err != nil {
		return llm.ToolOutput{}, err
	}
	if len(loaded) == 0 && len(already) == 0 {
		return llm.TextOutput(fmt.Sprintf("No available tools matched %q. Try a broader capability description or a server/tool name.", label)), nil
	}

	var b strings.Builder
	if len(loaded) > 0 {
		fmt.Fprintf(&b, "Loaded %d tools for %q:\n\n", len(loaded), label)
		for _, tool := range loaded {
			fmt.Fprintf(&b, "- %s — %s%s\n", tool.Name, conciseDescription(tool.Description), annotationSummary(tool))
		}
		b.WriteString("\nTheir complete schemas are available on the next model turn. Call them directly.")
	}
	if len(already) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "Already active: %s.", strings.Join(already, ", "))
	}
	if len(unavailable) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "Unavailable or denied requested tool(s): %s.", strings.Join(unavailable, ", "))
	}
	if len(evicted) > 0 {
		fmt.Fprintf(&b, "\n\nEvicted %d inactive/LRU tool(s) from the visible working set: %s.", len(evicted), strings.Join(evicted, ", "))
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "\n\n%d additional matching tool(s) were not loaded because this single request exceeds the dynamic working-set limit.", omitted)
	}
	return llm.TextOutput(b.String()), nil
}

func (p *Planner) activate(runID string, input searchInput) (loaded []mcp.CatalogTool, already []string, evicted []string, omitted int, unavailable []string, label string, err error) {
	p.mu.Lock()
	key := p.runs[runID]
	state := p.sessions[key]
	p.mu.Unlock()
	if key == "" || state == nil {
		return nil, nil, nil, 0, nil, "", fmt.Errorf("tool_search run is no longer active")
	}
	snapshot := p.manager.CatalogueSnapshot()
	if snapshot == nil {
		return nil, nil, nil, 0, nil, "", fmt.Errorf("tool catalogue is unavailable")
	}
	engine := p.currentEngine()
	if engine == nil {
		return nil, nil, nil, 0, nil, "", fmt.Errorf("tool discovery engine is unavailable")
	}
	byName := make(map[string]mcp.CatalogTool, len(snapshot.Tools))
	byOriginal := make(map[string][]string)
	for _, tool := range snapshot.Tools {
		if !engine.IsToolAllowed(tool.Name) {
			continue
		}
		byName[tool.Name] = tool
		byOriginal[strings.ToLower(tool.OriginalName)] = append(byOriginal[strings.ToLower(tool.OriginalName)], tool.Name)
	}

	var candidateIDs []string
	var ranked []SearchResult
	label = input.Query
	if len(input.ToolNames) > 0 {
		label = strings.Join(input.ToolNames, ", ")
		seen := make(map[string]bool)
		for _, requested := range input.ToolNames {
			if tool, ok := byName[requested]; ok {
				if !seen[tool.Name] {
					candidateIDs = append(candidateIDs, tool.Name)
					seen[tool.Name] = true
				}
				continue
			}
			matches := byOriginal[strings.ToLower(requested)]
			sort.Strings(matches)
			switch len(matches) {
			case 0:
				unavailable = append(unavailable, requested)
				continue
			case 1:
				if !seen[matches[0]] {
					candidateIDs = append(candidateIDs, matches[0])
					seen[matches[0]] = true
				}
			default:
				return nil, nil, nil, 0, unavailable, label, fmt.Errorf("original tool name %q is ambiguous; use one of: %s", requested, strings.Join(matches, ", "))
			}
		}
		if len(candidateIDs) == 0 && len(unavailable) > 0 {
			return nil, nil, nil, 0, unavailable, label, fmt.Errorf("requested tool(s) are unavailable or denied: %s", strings.Join(unavailable, ", "))
		}
	} else {
		ranked = p.index.search(input.Query, input.MaxResults, func(name string) bool {
			_, ok := byName[name]
			if !ok {
				return false
			}
			p.mu.Lock()
			state := p.sessions[key]
			active := false
			if state != nil {
				_, active = state.active[name]
			}
			p.mu.Unlock()
			return !active
		})
		for _, result := range ranked {
			candidateIDs = append(candidateIDs, result.ID)
		}
	}
	slog.Debug("MCP tool catalogue search ranked candidates", "run_id", runID, "query", input.Query, "exact_names", input.ToolNames, "catalogue_generation", snapshot.Generation, "catalogue_hash", snapshot.Hash, "ranked", ranked)

	// Resolve ranked IDs against the latest committed catalogue and policy. The
	// index may have been replaced while the model/tool call was in flight.
	latest := p.manager.CatalogueSnapshot()
	latestByName := make(map[string]mcp.CatalogTool)
	if latest != nil {
		latestByName = make(map[string]mcp.CatalogTool, len(latest.Tools))
		for _, tool := range latest.Tools {
			if engine.IsToolAllowed(tool.Name) {
				latestByName[tool.Name] = tool
			}
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	state = p.sessions[key]
	if state == nil || p.runs[runID] != key {
		return nil, nil, nil, 0, unavailable, label, fmt.Errorf("tool_search run ended before activation")
	}

	requested := make(map[string]bool, len(candidateIDs))
	newTools := make([]mcp.CatalogTool, 0, len(candidateIDs))
	protectedDynamic := 0
	for _, name := range candidateIDs {
		tool, ok := latestByName[name]
		if !ok {
			continue
		}
		requested[name] = true
		if active, ok := state.active[name]; ok {
			delete(state.evicted, name)
			state.lastUsed++
			active.LastUsed = state.lastUsed
			state.active[name] = active
			if !active.Pinned {
				protectedDynamic++
			}
			if len(input.ToolNames) > 0 {
				already = append(already, name)
			}
			continue
		}
		newTools = append(newTools, tool)
	}

	admitCount := len(newTools)
	if available := p.maxActiveTools - protectedDynamic; admitCount > available {
		admitCount = available
		if admitCount < 0 {
			admitCount = 0
		}
	}
	omitted = len(newTools) - admitCount
	newTools = newTools[:admitCount]

	dynamicCount := 0
	for _, active := range state.active {
		if !active.Pinned {
			dynamicCount++
		}
	}
	evictionsNeeded := dynamicCount + len(newTools) - p.maxActiveTools
	if evictionsNeeded > 0 {
		type evictionCandidate struct {
			name  string
			state activeToolState
			sent  bool
		}
		victims := make([]evictionCandidate, 0, dynamicCount)
		for name, active := range state.active {
			if active.Pinned || requested[name] {
				continue
			}
			_, sent := state.sent[name]
			victims = append(victims, evictionCandidate{name: name, state: active, sent: sent})
		}
		sort.Slice(victims, func(i, j int) bool {
			if victims[i].sent != victims[j].sent {
				return !victims[i].sent
			}
			if victims[i].state.Executed != victims[j].state.Executed {
				return !victims[i].state.Executed
			}
			if victims[i].state.LastUsed != victims[j].state.LastUsed {
				return victims[i].state.LastUsed < victims[j].state.LastUsed
			}
			if !victims[i].state.ActivatedAt.Equal(victims[j].state.ActivatedAt) {
				return victims[i].state.ActivatedAt.Before(victims[j].state.ActivatedAt)
			}
			return victims[i].name < victims[j].name
		})
		if evictionsNeeded > len(victims) {
			evictionsNeeded = len(victims)
		}
		for _, victim := range victims[:evictionsNeeded] {
			delete(state.active, victim.name)
			state.evicted[victim.name] = true
			reason := "never executed"
			if victim.state.Executed {
				reason = "least recently used"
			}
			state.evictionCount++
			state.recentEvictions = append(state.recentEvictions, llm.ToolEvictionDiagnostic{Name: victim.name, Reason: reason, Executed: victim.state.Executed})
			if len(state.recentEvictions) > maxRecentEvents {
				state.recentEvictions = append([]llm.ToolEvictionDiagnostic(nil), state.recentEvictions[len(state.recentEvictions)-maxRecentEvents:]...)
			}
			if _, sent := state.sent[victim.name]; sent {
				state.resetRequired = "an LRU-evicted MCP tool is no longer visible"
			}
			evicted = append(evicted, victim.name)
		}
	}

	for _, tool := range newTools {
		delete(state.evicted, tool.Name)
		wrapper := mcp.NewCatalogMCPTool(p.manager, tool)
		engine.Tools().RegisterDeferred(wrapper)
		state.lastUsed++
		state.active[tool.Name] = activeToolState{
			SchemaHash:  tool.SchemaHash,
			ActivatedBy: activationReason(input),
			ActivatedAt: timeNowUTC(),
			LastUsed:    state.lastUsed,
		}
		state.recent = append(state.recent, llm.ToolActivationDiagnostic{
			Name:      tool.Name,
			Namespace: tool.Namespace,
			ChildName: tool.ChildName,
			Reason:    activationReason(input),
		})
		if len(state.recent) > maxRecentEvents {
			state.recent = append([]llm.ToolActivationDiagnostic(nil), state.recent[len(state.recent)-maxRecentEvents:]...)
		}
		loaded = append(loaded, tool)
	}
	if len(candidateIDs) > 0 && len(loaded) == 0 && len(already) == 0 && omitted == 0 {
		return nil, nil, nil, 0, unavailable, label, fmt.Errorf("no requested tools remain available in the current catalogue; search again")
	}
	loadedNames := make([]string, 0, len(loaded))
	for _, tool := range loaded {
		loadedNames = append(loadedNames, tool.Name)
	}
	slog.Debug("MCP tool catalogue activation", "run_id", runID, "loaded", loadedNames, "already_active", already, "evicted", evicted, "working_set_omitted", omitted, "dynamic_active", dynamicCount-len(evicted)+len(loaded), "dynamic_limit", p.maxActiveTools)
	return loaded, already, evicted, omitted, unavailable, label, nil
}

var timeNowUTC = func() time.Time { return time.Now().UTC() }

func activationReason(input searchInput) string {
	if len(input.ToolNames) > 0 {
		return "exact: " + strings.Join(input.ToolNames, ", ")
	}
	return "search: " + input.Query
}

func conciseDescription(description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return "MCP tool."
	}
	if len(description) > 180 {
		description = strings.TrimSpace(description[:177]) + "..."
	}
	return description
}

func annotationSummary(tool mcp.CatalogTool) string {
	if !tool.Annotations.Present {
		return " Behavior hints unspecified."
	}
	if tool.Annotations.ReadOnly {
		return " Read-only."
	}
	if tool.Annotations.Destructive != nil && *tool.Annotations.Destructive {
		return " Potentially destructive write operation."
	}
	return " Write operation."
}

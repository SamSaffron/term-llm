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
		Description: "Search the authorised MCP tool catalogue and load matching tool schemas for the next turn. Use this before the final turn when the needed MCP capability is not already available.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Describe the capability or operation needed.",
				},
				"tool_names": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"maxItems":    8,
					"description": "Exact available tool names to load when already known.",
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
		if query := strings.TrimSpace(input.Query); query != "" {
			return fmt.Sprintf("Searching MCP tools for %q", query)
		}
		if len(input.ToolNames) > 0 {
			return "Loading MCP tools: " + strings.Join(input.ToolNames, ", ")
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
	if (input.Query == "") == (len(input.ToolNames) == 0) {
		return searchInput{}, fmt.Errorf("exactly one of a non-empty query or non-empty tool_names is required")
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
	loaded, already, omitted, label, err := t.planner.activate(runID, input)
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
	if omitted > 0 {
		fmt.Fprintf(&b, "\n\n%d additional matching tool(s) were not loaded because the dynamic active-tool budget is full.", omitted)
	}
	return llm.TextOutput(b.String()), nil
}

func (p *Planner) activate(runID string, input searchInput) (loaded []mcp.CatalogTool, already []string, omitted int, label string, err error) {
	p.mu.Lock()
	key := p.runs[runID]
	state := p.sessions[key]
	p.mu.Unlock()
	if key == "" || state == nil {
		return nil, nil, 0, "", fmt.Errorf("tool_search run is no longer active")
	}
	snapshot := p.manager.CatalogueSnapshot()
	if snapshot == nil {
		return nil, nil, 0, "", fmt.Errorf("tool catalogue is unavailable")
	}
	engine := p.currentEngine()
	if engine == nil {
		return nil, nil, 0, "", fmt.Errorf("tool discovery engine is unavailable")
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
				return nil, nil, 0, label, fmt.Errorf("requested tool %q is unavailable or denied", requested)
			case 1:
				if !seen[matches[0]] {
					candidateIDs = append(candidateIDs, matches[0])
					seen[matches[0]] = true
				}
			default:
				return nil, nil, 0, label, fmt.Errorf("original tool name %q is ambiguous; use one of: %s", requested, strings.Join(matches, ", "))
			}
		}
	} else {
		ranked = p.index.search(input.Query, input.MaxResults, func(name string) bool {
			_, ok := byName[name]
			if !ok {
				return false
			}
			p.mu.Lock()
			_, active := p.sessions[key].active[name]
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
		return nil, nil, 0, label, fmt.Errorf("tool_search run ended before activation")
	}
	dynamicCount := 0
	for _, active := range state.active {
		if !active.Pinned {
			dynamicCount++
		}
	}
	for _, name := range candidateIDs {
		tool, ok := latestByName[name]
		if !ok {
			continue
		}
		if _, ok := state.active[name]; ok {
			if len(input.ToolNames) > 0 {
				already = append(already, name)
			}
			continue
		}
		if dynamicCount >= MaxActiveDeferred {
			omitted++
			continue
		}
		wrapper := mcp.NewCatalogMCPTool(p.manager, tool)
		engine.Tools().RegisterDeferred(wrapper)
		state.lastUsed++
		state.active[name] = activeToolState{
			SchemaHash:  tool.SchemaHash,
			ActivatedBy: activationReason(input),
			ActivatedAt: timeNowUTC(),
			LastUsed:    state.lastUsed,
		}
		state.recent = append(state.recent, llm.ToolActivationDiagnostic{Name: name, Reason: activationReason(input)})
		if len(state.recent) > maxRecentEvents {
			state.recent = append([]llm.ToolActivationDiagnostic(nil), state.recent[len(state.recent)-maxRecentEvents:]...)
		}
		loaded = append(loaded, tool)
		dynamicCount++
	}
	if len(candidateIDs) > 0 && len(loaded) == 0 && len(already) == 0 && omitted == 0 {
		return nil, nil, 0, label, fmt.Errorf("no requested tools remain available in the current catalogue; search again")
	}
	if len(candidateIDs) > 0 && len(loaded) == 0 && len(already) == 0 && omitted > 0 {
		return nil, nil, omitted, label, fmt.Errorf("dynamic MCP tool budget is full (%d/%d); no additional tool was activated", dynamicCount, MaxActiveDeferred)
	}
	loadedNames := make([]string, 0, len(loaded))
	for _, tool := range loaded {
		loadedNames = append(loadedNames, tool.Name)
	}
	slog.Debug("MCP tool catalogue activation", "run_id", runID, "loaded", loadedNames, "already_active", already, "budget_omitted", omitted, "dynamic_active", dynamicCount)
	return loaded, already, omitted, label, nil
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
	if tool.Annotations.ReadOnly {
		return " Read-only."
	}
	if tool.Annotations.Destructive != nil && *tool.Annotations.Destructive {
		return " Potentially destructive write operation."
	}
	return " Write operation."
}

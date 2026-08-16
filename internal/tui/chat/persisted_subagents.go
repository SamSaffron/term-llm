package chat

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/subagentview"
	"github.com/samsaffron/term-llm/internal/tools"
)

type persistedSubagentSource struct {
	call   llm.ToolCall
	result llm.ToolResult
	parsed tools.SpawnAgentResult
}

type persistedSubagentsLoadedMsg struct {
	sessionID  string
	generation uint64
	runs       map[string]subagentview.CompletedRun
}

func discoverPersistedSubagents(messages []session.Message) []persistedSubagentSource {
	calls := make(map[string]llm.ToolCall)
	order := make([]string, 0)
	results := make(map[string]llm.ToolResult)
	for _, message := range messages {
		for _, part := range message.Parts {
			switch part.Type {
			case llm.PartToolCall:
				if part.ToolCall == nil || part.ToolCall.Name != tools.SpawnAgentToolName || strings.TrimSpace(part.ToolCall.ID) == "" {
					continue
				}
				if _, exists := calls[part.ToolCall.ID]; !exists {
					order = append(order, part.ToolCall.ID)
				}
				calls[part.ToolCall.ID] = *part.ToolCall
			case llm.PartToolResult:
				if part.ToolResult == nil || strings.TrimSpace(part.ToolResult.ID) == "" {
					continue
				}
				results[part.ToolResult.ID] = *part.ToolResult
			}
		}
	}
	sources := make([]persistedSubagentSource, 0, len(order))
	for _, callID := range order {
		result, ok := results[callID]
		if !ok {
			continue
		}
		parsed, err := tools.ParseSpawnAgentResult(result.Content)
		if err != nil && result.Display != "" {
			parsed, err = tools.ParseSpawnAgentResult(result.Display)
		}
		if err != nil {
			continue
		}
		sources = append(sources, persistedSubagentSource{call: calls[callID], result: result, parsed: parsed})
	}
	return sources
}

func persistedSubagentMatchesSource(run subagentview.CompletedRun, result tools.SpawnAgentResult) bool {
	childID := strings.TrimSpace(result.SessionID)
	if childID != run.ChildSessionID || result.Output != run.Output || result.Error != run.Error || result.Type != run.ErrorType || result.Duration != run.DurationMs {
		return false
	}
	return childID == "" || run.ChildAvailable
}

func (m *Model) loadPersistedSubagentsCmd() tea.Cmd {
	if m == nil || m.sess == nil {
		return nil
	}
	m.persistedSubagentGeneration++
	generation := m.persistedSubagentGeneration
	sessionID := m.sess.ID
	m.messagesMu.Lock()
	sources := discoverPersistedSubagents(append([]session.Message(nil), m.messages...))
	m.messagesMu.Unlock()
	store := m.store
	existing := make(map[string]subagentview.CompletedRun, len(m.persistedSubagents))
	for callID, run := range m.persistedSubagents {
		existing[callID] = run
	}
	// Parent result parsing is local and cheap. Install fallback cards now so the
	// first frame after resume already shows durable output; the command below
	// upgrades them with validated child activity.
	fallback := make(map[string]subagentview.CompletedRun, len(sources))
	for i := range sources {
		source := &sources[i]
		if cached, ok := existing[source.call.ID]; ok && persistedSubagentMatchesSource(cached, source.parsed) {
			fallback[source.call.ID] = cached
			continue
		}
		fallback[source.call.ID] = subagentview.Build(&source.call, &source.result, source.parsed, nil, nil)
	}
	m.persistedSubagents = fallback
	m.invalidateHistoryCache()
	rootCtx := m.rootContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(rootCtx, 5*time.Second)
		defer cancel()
		runs := make(map[string]subagentview.CompletedRun, len(fallback))
		for callID, run := range fallback {
			runs[callID] = run
		}
		for i := range sources {
			if ctx.Err() != nil {
				break
			}
			source := &sources[i]
			if cached, ok := existing[source.call.ID]; ok && persistedSubagentMatchesSource(cached, source.parsed) {
				runs[source.call.ID] = cached
				continue
			}
			var child *session.Session
			var childMessages []session.Message
			childID := strings.TrimSpace(source.parsed.SessionID)
			if store != nil && childID != "" && childID != sessionID {
				loadedChild, err := store.Get(ctx, childID)
				if err == nil && loadedChild != nil && loadedChild.ParentID == sessionID {
					child = loadedChild
					if loaded, _, loadErr := session.LoadScrollbackWithBoundary(ctx, store, loadedChild); loadErr == nil {
						childMessages = loaded
					}
				}
			}
			runs[source.call.ID] = subagentview.Build(&source.call, &source.result, source.parsed, child, childMessages)
		}
		return persistedSubagentsLoadedMsg{sessionID: sessionID, generation: generation, runs: runs}
	}
}

func (m *Model) applyPersistedSubagents(msg persistedSubagentsLoadedMsg) {
	if m == nil || m.sess == nil || msg.sessionID != m.sess.ID || msg.generation != m.persistedSubagentGeneration {
		return
	}
	m.persistedSubagents = msg.runs
	m.invalidateHistoryCache()
}

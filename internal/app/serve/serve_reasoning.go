package serve

import (
	"fmt"
	"time"

	"github.com/term-llm/term-llm/internal/app/serve/wire"
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/reasoning"
	"github.com/term-llm/term-llm/internal/session"
)

func appendSessionMessageTextWithReasoning(entry *sessionMessageEntry, msg *session.Message, p core.Part, files map[string]bool, displayText string, cfg config.ReasoningConfig) {
	kind, title, body := wire.ReasoningPartDisplay(p, msg.Role, cfg)
	if body != "" {
		entry.Parts = append(entry.Parts, sessionMessagePartEntry{Type: "reasoning", ReasoningKind: kind, ReasoningTitle: title, Text: body, CreatedAt: p.CreatedAt})
	}
	appendSessionMessageText(entry, msg, p, files, displayText)
}

func (s *serveServer) appendResponseReasoning(run *responseRun, state *responseRunStreamState, ev core.Event) error {
	if !state.thought.Configured() {
		state.thought.Configure(s.cfgRef.ResolveReasoning("serve"))
	}
	if payload := state.thought.Append(ev); payload != nil {
		run.mu.Lock()
		defer run.mu.Unlock()
		// The recovery cursor is an open text segment, already saved on reload.
		// A visible thought closes it; live text must open a new segment too.
		if run.currentAssistant >= 0 {
			state.assistantBoundaryPending = true
		}
		return run.appendEventLocked("response.reasoning.delta", payload, false)
	}
	return nil
}

func (r *responseRun) applyRecoveryReasoning(payload map[string]any) {
	index := responseRunIntValue(payload["reasoning_index"], 0)
	for i := range r.recoveryMessages {
		msg := &r.recoveryMessages[i]
		if msg.Role == "reasoning" && msg.ReasoningIndex == index {
			updateRecoveryReasoning(msg, payload)
			return
		}
	}
	r.closeToolGroupLocked()
	r.currentAssistant = -1
	msg := responseRunRecoveryMessage{ID: fmt.Sprintf("%s_reasoning_%d", r.id, index), Role: "reasoning", Created: time.Now().UnixMilli(), ReasoningIndex: index}
	updateRecoveryReasoning(&msg, payload)
	r.recoveryMessages = append(r.recoveryMessages, msg)
}

func updateRecoveryReasoning(msg *responseRunRecoveryMessage, payload map[string]any) {
	msg.ReasoningKind, msg.ReasoningTitle = stringValue(payload["kind"]), stringValue(payload["title"])
	if text, ok := payload["text"].(string); ok {
		msg.Content = []byte(text)
	} else {
		msg.Content = append(msg.Content, stringValue(payload["delta"])...)
	}
}

func (s *serveServer) reasoningCapability() map[string]any {
	cfg := s.cfgRef.ResolveReasoning("serve")
	return map[string]any{"display": reasoning.EffectiveDisplay(cfg), "status": cfg.Status, "history": cfg.History, "raw": cfg.Raw, "hidden_label": cfg.HiddenLabel}
}

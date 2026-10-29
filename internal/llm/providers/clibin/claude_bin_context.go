package clibin

import (
	"encoding/json"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// claudeContextReport tracks the context occupancy Claude Code reports while
// it runs its own tool loop. It is display-only: Claude Code owns compaction
// and term-llm never acts on these numbers.
//
// Each top-level `assistant` stream event carries the usage of the API call
// that produced it, so its prompt size (input + cache read + cache write) is
// the live context size at every tool-call checkpoint. The terminal `result`
// usage is summed across every call in the turn and is unusable for this;
// only its modelUsage[*].contextWindow is taken from it.
type claudeContextReport struct {
	model     string // Model of the latest top-level assistant message
	messageID string // Assistant message the last reading came from
	used      int    // Prompt tokens of the latest top-level API call
	window    int    // Context window Claude Code reported for model
}

type claudeAssistantUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type claudeAssistantContextLine struct {
	ParentToolUseID string `json:"parent_tool_use_id"`
	Message         struct {
		ID    string                `json:"id"`
		Model string                `json:"model"`
		Usage *claudeAssistantUsage `json:"usage"`
	} `json:"message"`
}

type claudeModelUsage struct {
	ContextWindow int `json:"contextWindow"`
}

type claudeResultContextLine struct {
	ModelUsage map[string]claudeModelUsage `json:"modelUsage"`
}

// observe updates the report from one stream-json line and returns the
// reading to publish, or nil when the line carries nothing new.
func (r *claudeContextReport) observe(lineType, line string) *core.ContextUsage {
	switch lineType {
	case "assistant":
		return r.observeAssistant(line)
	case "result":
		return r.observeResult(line)
	}
	return nil
}

func (r *claudeContextReport) observeAssistant(line string) *core.ContextUsage {
	var msg claudeAssistantContextLine
	if err := json.Unmarshal([]byte(line), &msg); err != nil || msg.Message.Usage == nil {
		return nil
	}
	// Subagent (sidechain) calls run in their own context window.
	if msg.ParentToolUseID != "" {
		return nil
	}
	u := msg.Message.Usage
	used := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	if used <= 0 {
		return nil
	}
	// Claude Code repeats the message usage on every content block event of
	// the same API call.
	if msg.Message.ID != "" && msg.Message.ID == r.messageID && used == r.used {
		return nil
	}
	if msg.Message.Model != "" && msg.Message.Model != r.model {
		if r.model != "" {
			r.window = 0
		}
		r.model = msg.Message.Model
	}
	r.messageID, r.used = msg.Message.ID, used
	return r.reading()
}

func (r *claudeContextReport) observeResult(line string) *core.ContextUsage {
	var msg claudeResultContextLine
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return nil
	}
	window := 0
	if usage, ok := msg.ModelUsage[r.model]; ok {
		window = usage.ContextWindow
	} else if len(msg.ModelUsage) == 1 {
		for _, usage := range msg.ModelUsage {
			window = usage.ContextWindow
		}
	}
	if window <= 0 || window == r.window {
		return nil
	}
	r.window = window
	if r.used <= 0 {
		return nil
	}
	return r.reading()
}

func (r *claudeContextReport) reading() *core.ContextUsage {
	return &core.ContextUsage{UsedTokens: r.used, Window: r.window}
}

// reportClaudeContext publishes a context reading derived from line. Ephemeral
// one-shot requests run outside the conversation and never report.
func (p *ClaudeBinProvider) reportClaudeContext(lineType, line string, ephemeral bool, send eventSender) error {
	if ephemeral {
		return nil
	}
	reading := p.contextReport.observe(lineType, line)
	if reading == nil {
		return nil
	}
	return send.Send(core.Event{Type: core.EventContextUsage, Context: reading})
}

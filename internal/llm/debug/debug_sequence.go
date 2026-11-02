package debug

import (
	"context"
	"strconv"
	"strings"
	"time"

	. "github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/core/stream"
)

// streamActionSequence streams a sequence of interleaved text and tool calls.
func (d *DebugProvider) streamActionSequence(ctx context.Context, send stream.Sender, actions []debugAction) error {
	chunkSize := d.preset.ChunkSize
	delay := d.preset.Delay
	var totalTextLen, toolCount int

	for _, action := range actions {
		switch action.Type {
		case actionText:
			totalTextLen += len(action.Text)
			text := action.Text
			for len(text) > 0 {
				end := chunkSize
				if end > len(text) {
					end = len(text)
				}
				chunk := text[:end]
				text = text[end:]

				if err := send.Send(Event{Type: EventTextDelta, Text: chunk}); err != nil {
					return err
				}

				if delay > 0 && len(text) > 0 {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(delay):
					}
				}
			}

		case actionThink, actionThinkRaw:
			totalTextLen += len(action.Text)
			if err := d.streamThinkingAction(ctx, send, action); err != nil {
				return err
			}
		case actionTool:
			toolCount++
			if err := send.Send(Event{Type: EventToolCall, Tool: action.ToolCall}); err != nil {
				return err
			}
		}
	}

	// Emit usage stats
	return send.Send(Event{Type: EventUsage, Use: &Usage{
		InputTokens:  10,
		OutputTokens: totalTextLen/4 + toolCount*10,
	}})
}

// parseSequenceDSL parses a comma-separated DSL prompt into a sequence of actions.
// Example: "markdown*50,read README.md,glob **/*.go,markdown*200"
// Returns nil if the prompt doesn't look like a DSL sequence.
func parseSequenceDSL(prompt string, tools []ToolSpec) []debugAction {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" || (!strings.Contains(prompt, ",") && prompt != "think" && prompt != "thinkraw") {
		return nil
	}

	// NOTE: Simple comma split - tool arguments containing commas will be split unexpectedly.
	// This is a known limitation; escape sequences are not supported.
	segments := strings.Split(prompt, ",")
	var actions []debugAction

	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}

		if action, ok := parseThinkingAction(seg); ok {
			actions = append(actions, action)
			continue
		}

		// Check for markdown segment (with optional length)
		if match := markdownLengthRegex.FindStringSubmatch(strings.ToLower(seg)); match != nil {
			length := 50 // default length
			if match[1] != "" {
				if n, err := strconv.Atoi(match[1]); err == nil && n > 0 {
					length = n
					if length > len(debugMarkdown) {
						length = len(debugMarkdown)
					}
				}
			}
			actions = append(actions, debugAction{
				Type: actionText,
				Text: debugMarkdown[:length],
			})
			continue
		}

		// Try to parse as a tool command
		if calls := parseCommand(seg, tools); len(calls) > 0 {
			for _, call := range calls {
				actions = append(actions, debugAction{
					Type:     actionTool,
					ToolCall: call,
				})
			}
		}
		// Invalid segments are silently skipped
	}

	// Only return if we have at least one action
	if len(actions) == 0 {
		return nil
	}

	return actions
}

func parseThinkingAction(seg string) (debugAction, bool) {
	switch strings.ToLower(seg) {
	case "think":
		return debugAction{Type: actionThink, Text: "**Checking the next step**\n\nI will inspect the relevant context, compare the available options, and verify the result with a focused test."}, true
	case "thinkraw":
		return debugAction{Type: actionThinkRaw, Text: "Considering the available context and checking which step to take next."}, true
	}
	return debugAction{}, false
}

func (d *DebugProvider) streamThinkingAction(ctx context.Context, send stream.Sender, action debugAction) error {
	kind := ReasoningKindSummary
	if action.Type == actionThinkRaw {
		kind = ReasoningKindRaw
	}
	id := nextDebugCallID()
	text := action.Text
	for len(text) > 0 {
		n := min(max(1, d.preset.ChunkSize), len(text))
		if err := send.Send(Event{Type: EventReasoningDelta, Text: text[:n], ReasoningKind: kind, ReasoningItemID: id}); err != nil {
			return err
		}
		text = text[n:]
		if d.preset.Delay > 0 && text != "" {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d.preset.Delay):
			}
		}
	}
	return nil
}

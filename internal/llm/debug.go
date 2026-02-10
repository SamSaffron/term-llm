package llm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// DebugToolCall prints a tool call in debug mode with readable formatting.
func DebugToolCall(enabled bool, call ToolCall) {
	if !enabled {
		return
	}

	args := formatJSON(call.Arguments)
	body := fmt.Sprintf("name: %s\nid: %s\nargs:\n%s", call.Name, call.ID, args)
	debugSection(enabled, "Tool Call", body)
}

// DebugToolResult prints a tool result in debug mode with readable formatting.
func DebugToolResult(enabled bool, id, name, content string) {
	if !enabled {
		return
	}

	result := content
	if result == "" {
		result = "(empty)"
	}
	body := fmt.Sprintf("name: %s\nid: %s\nresult:\n%s", name, id, result)
	debugSection(enabled, "Tool Result", body)
}

// DebugRawRequest prints the raw request with all message parts in debug mode.
func DebugRawRequest(enabled bool, providerName, credential string, req Request, label string) {
	if !enabled {
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "provider: %s\n", providerName)
	fmt.Fprintf(&b, "credential: %s\n", credential)
	if req.Model != "" {
		fmt.Fprintf(&b, "model: %s\n", req.Model)
	}
	fmt.Fprintf(&b, "search: %t\n", req.Search)
	fmt.Fprintf(&b, "parallel_tool_calls: %t\n", req.ParallelToolCalls)
	if req.ToolChoice.Mode != "" {
		if req.ToolChoice.Name != "" {
			fmt.Fprintf(&b, "tool_choice: %s (%s)\n", req.ToolChoice.Mode, req.ToolChoice.Name)
		} else {
			fmt.Fprintf(&b, "tool_choice: %s\n", req.ToolChoice.Mode)
		}
	}
	if req.MaxOutputTokens > 0 {
		fmt.Fprintf(&b, "max_output_tokens: %d\n", req.MaxOutputTokens)
	}
	if req.Temperature > 0 {
		fmt.Fprintf(&b, "temperature: %.2f\n", req.Temperature)
	}
	if req.TopP > 0 {
		fmt.Fprintf(&b, "top_p: %.2f\n", req.TopP)
	}
	if len(req.Tools) > 0 {
		b.WriteString("tools:\n")
		for _, tool := range req.Tools {
			fmt.Fprintf(&b, "- name: %s\n", tool.Name)
			if tool.Description != "" {
				fmt.Fprintf(&b, "  description: %s\n", tool.Description)
			}
			if tool.Schema != nil {
				fmt.Fprintf(&b, "  schema:\n%s\n", formatJSONSchema(tool.Schema))
			}
		}
	}

	if len(req.Messages) > 0 {
		b.WriteString("messages:\n")
		for i, msg := range req.Messages {
			fmt.Fprintf(&b, "[%d] role=%s\n", i+1, msg.Role)
			for _, part := range msg.Parts {
				switch part.Type {
				case PartText:
					b.WriteString("text:\n")
					b.WriteString(part.Text)
					b.WriteString("\n")
				case PartToolCall:
					if part.ToolCall != nil {
						fmt.Fprintf(&b, "tool_call name=%s id=%s\n", part.ToolCall.Name, part.ToolCall.ID)
						b.WriteString("args:\n")
						b.WriteString(rawJSON(part.ToolCall.Arguments))
						b.WriteString("\n")
					}
				case PartToolResult:
					if part.ToolResult != nil {
						fmt.Fprintf(&b, "tool_result name=%s id=%s\n", part.ToolResult.Name, part.ToolResult.ID)
						b.WriteString("result:\n")
						b.WriteString(part.ToolResult.Content)
						b.WriteString("\n")
					}
				}
			}
			b.WriteString("---\n")
		}
	}

	DebugRawSection(enabled, label, strings.TrimRight(b.String(), "\n"))
}

// DebugRawToolCall prints a tool call with raw JSON arguments and a timestamp.
func DebugRawToolCall(enabled bool, call ToolCall) {
	if !enabled {
		return
	}

	args := rawJSON(call.Arguments)
	if args == "" {
		args = "(empty)"
	}
	body := fmt.Sprintf("name: %s\nid: %s\nargs:\n%s", call.Name, call.ID, args)
	DebugRawSection(enabled, "Tool Call", body)
}

// DebugRawToolResult prints a tool result payload with a timestamp.
func DebugRawToolResult(enabled bool, id, name, content string) {
	if !enabled {
		return
	}

	result := content
	if result == "" {
		result = "(empty)"
	}
	body := fmt.Sprintf("name: %s\nid: %s\nresult:\n%s", name, id, result)
	DebugRawSection(enabled, "Tool Result", body)
}

// DebugRawEvent prints each stream event with a timestamp.
func DebugRawEvent(enabled bool, event Event) {
	if !enabled {
		return
	}

	switch event.Type {
	case EventTextDelta:
		DebugRawSection(enabled, "Event Text Delta", event.Text)
	case EventToolCall:
		if event.Tool != nil {
			DebugRawToolCall(enabled, *event.Tool)
		} else {
			DebugRawSection(enabled, "Event Tool Call", "(nil)")
		}
	case EventUsage:
		if event.Use != nil {
			body := fmt.Sprintf(
				"input_tokens: %d\noutput_tokens: %d\ncached_input_tokens: %d",
				event.Use.InputTokens,
				event.Use.OutputTokens,
				event.Use.CachedInputTokens,
			)
			DebugRawSection(enabled, "Event Usage", body)
		} else {
			DebugRawSection(enabled, "Event Usage", "(nil)")
		}
	case EventReasoningDelta:
		var body strings.Builder
		if event.ReasoningItemID != "" {
			fmt.Fprintf(&body, "reasoning_item_id: %s\n", event.ReasoningItemID)
		}
		fmt.Fprintf(&body, "text_len: %d\n", len(event.Text))
		if event.Text != "" {
			text := event.Text
			if len(text) > 500 {
				text = text[:500] + "...[truncated]"
			}
			body.WriteString("text:\n")
			body.WriteString(text)
			body.WriteString("\n")
		}
		if event.ReasoningEncryptedContent != "" {
			fmt.Fprintf(&body, "encrypted_content_len: %d\n", len(event.ReasoningEncryptedContent))
			fmt.Fprintf(&body, "encrypted_content_hash: %s\n", shortDebugHash(event.ReasoningEncryptedContent))
		}
		DebugRawSection(enabled, "Event Reasoning Delta", strings.TrimRight(body.String(), "\n"))
	case EventToolExecStart:
		info := event.ToolName
		if event.ToolInfo != "" {
			info = event.ToolName + ": " + event.ToolInfo
		}
		DebugRawSection(enabled, "Event Tool Exec Start", info)
	case EventToolExecEnd:
		status := "success"
		if !event.ToolSuccess {
			status = "error"
		}
		info := fmt.Sprintf("%s (%s)", event.ToolName, status)
		if event.ToolInfo != "" {
			info = fmt.Sprintf("%s: %s (%s)", event.ToolName, event.ToolInfo, status)
		}
		DebugRawSection(enabled, "Event Tool Exec End", info)
	case EventDone:
		DebugRawSection(enabled, "Event Done", "")
	case EventError:
		if event.Err != nil {
			DebugRawSection(enabled, "Event Error", event.Err.Error())
		} else {
			DebugRawSection(enabled, "Event Error", "(nil)")
		}
	default:
		DebugRawSection(enabled, "Event", fmt.Sprintf("type: %s", event.Type))
	}
}

func shortDebugHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	hexHash := hex.EncodeToString(sum[:])
	if len(hexHash) <= 16 {
		return hexHash
	}
	return hexHash[:16]
}

// DebugRawSection prints a timestamped debug section.
func DebugRawSection(enabled bool, label, body string) {
	if !enabled {
		return
	}

	ts := time.Now().Format(time.RFC3339Nano)
	fmt.Fprintf(os.Stderr, "\n[%s] %s\n", ts, label)
	if body != "" {
		fmt.Fprintln(os.Stderr, body)
	}
	fmt.Fprintf(os.Stderr, "[%s] END %s\n", ts, label)
	fmt.Fprintln(os.Stderr)
}

type debugStream struct {
	inner   Stream
	enabled bool
}

func WrapDebugStream(enabled bool, inner Stream) Stream {
	if !enabled {
		return inner
	}
	return &debugStream{inner: inner, enabled: enabled}
}

func (s *debugStream) Recv() (Event, error) {
	event, err := s.inner.Recv()
	if err != nil && err != io.EOF {
		DebugRawSection(s.enabled, "Stream Recv Error", err.Error())
	}
	if err == nil {
		DebugRawEvent(s.enabled, event)
	}
	return event, err
}

func (s *debugStream) Close() error {
	return s.inner.Close()
}

func debugSection(enabled bool, title, body string) {
	if !enabled {
		return
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "=== DEBUG: %s ===\n", title)
	if body != "" {
		fmt.Fprintln(os.Stderr, body)
	}
	fmt.Fprintf(os.Stderr, "=== DEBUG: END %s ===\n", title)
	fmt.Fprintln(os.Stderr)
}

func formatJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "(empty)"
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return string(raw)
	}
	return out.String()
}

func rawJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return string(raw)
}

func formatJSONSchema(schema map[string]interface{}) string {
	data, err := json.MarshalIndent(schema, "  ", "  ")
	if err != nil {
		return "  (invalid schema)"
	}
	return string(data)
}

package debuglog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ExportFormat specifies the export format
type ExportFormat string

const (
	FormatJSON     ExportFormat = "json"
	FormatMarkdown ExportFormat = "markdown"
	FormatRaw      ExportFormat = "raw"
)

// ExportOptions controls export behavior
type ExportOptions struct {
	Format ExportFormat
	Redact bool // Redact sensitive content (API keys, file contents, paths)
}

// Export exports a session in the specified format
func Export(w io.Writer, session *Session, opts ExportOptions) error {
	switch opts.Format {
	case FormatJSON:
		return exportJSON(w, session, opts)
	case FormatMarkdown:
		return exportMarkdown(w, session, opts)
	case FormatRaw:
		return exportRaw(w, session.FilePath)
	default:
		return fmt.Errorf("unknown format: %s", opts.Format)
	}
}

// exportJSON exports the session as pretty-printed JSON
func exportJSON(w io.Writer, session *Session, opts ExportOptions) error {
	// Parse raw lines for complete data
	lines, err := ParseRawLines(session.FilePath)
	if err != nil {
		return err
	}

	var entries []map[string]any
	for _, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		if opts.Redact {
			redactEntry(entry)
		}

		entries = append(entries, entry)
	}

	// Create export structure
	export := map[string]any{
		"session_id": session.ID,
		"provider":   session.Provider,
		"model":      session.Model,
		"start_time": session.StartTime,
		"end_time":   session.EndTime,
		"turns":      session.Turns,
		"tokens": map[string]int{
			"input":  session.TotalTokens.Input,
			"output": session.TotalTokens.Output,
			"cached": session.TotalTokens.Cached,
		},
		"has_errors": session.HasErrors,
		"entries":    entries,
	}

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(export)
}

// exportMarkdown exports the session as readable markdown
func exportMarkdown(w io.Writer, session *Session, opts ExportOptions) error {
	fmt.Fprintf(w, "# Debug Session: %s\n\n", session.ID)
	fmt.Fprintf(w, "**Provider:** %s  \n", session.Provider)
	fmt.Fprintf(w, "**Model:** %s  \n", session.Model)
	fmt.Fprintf(w, "**Started:** %s  \n", session.StartTime.Local().Format("2006-01-02 15:04:05"))
	if !session.EndTime.IsZero() {
		fmt.Fprintf(w, "**Ended:** %s  \n", session.EndTime.Local().Format("2006-01-02 15:04:05"))
	}
	fmt.Fprintf(w, "**Turns:** %d  \n", session.Turns)
	fmt.Fprintf(w, "**Tokens:** input=%d, output=%d, cached=%d  \n\n",
		session.TotalTokens.Input, session.TotalTokens.Output, session.TotalTokens.Cached)

	if session.HasErrors {
		fmt.Fprintf(w, "**Status:** Has errors\n\n")
	}

	fmt.Fprintf(w, "---\n\n")
	fmt.Fprintf(w, "## Conversation Log\n\n")

	for _, entry := range session.Entries {
		switch e := entry.(type) {
		case RequestEntry:
			exportRequestMarkdown(w, e, opts)
		case EventEntry:
			exportEventMarkdown(w, e, opts)
		}
	}

	return nil
}

// exportRequestMarkdown exports a request entry as markdown
func exportRequestMarkdown(w io.Writer, req RequestEntry, opts ExportOptions) {
	fmt.Fprintf(w, "### Request (%s)\n\n", req.Timestamp.Local().Format("15:04:05"))
	fmt.Fprintf(w, "- Provider: %s\n", req.Provider)
	fmt.Fprintf(w, "- Model: %s\n", req.Model)
	fmt.Fprintf(w, "- Messages: %d\n", len(req.Request.Messages))
	fmt.Fprintf(w, "- Tools: %d\n", len(req.Request.Tools))
	fmt.Fprintln(w)

	// Show messages
	for i, msg := range req.Request.Messages {
		role := capitalizeFirst(msg.Role)
		fmt.Fprintf(w, "**%s Message %d:**\n", role, i+1)

		content := formatMessageContent(msg.Content, opts)
		if opts.Redact {
			content = redactText(content)
		}

		if len(content) > 500 {
			content = content[:500] + "\n...(truncated)"
		}
		fmt.Fprintf(w, "```\n%s\n```\n\n", content)
	}
}

// exportEventMarkdown exports an event entry as markdown
func exportEventMarkdown(w io.Writer, evt EventEntry, opts ExportOptions) {
	switch evt.EventType {
	case "tool_call":
		name, _ := evt.Data["name"].(string)
		args, _ := evt.Data["arguments"].(string)
		fmt.Fprintf(w, "#### Tool Call: %s (%s)\n\n", name, evt.Timestamp.Local().Format("15:04:05"))
		if args != "" {
			if opts.Redact {
				args = redactText(args)
			}
			if len(args) > 500 {
				args = args[:500] + "\n...(truncated)"
			}
			fmt.Fprintf(w, "```json\n%s\n```\n\n", args)
		}

	case "tool_exec_end":
		name, _ := evt.Data["tool_name"].(string)
		success, _ := evt.Data["success"].(bool)
		status := "success"
		if !success {
			status = "failed"
		}
		fmt.Fprintf(w, "- Tool %s: %s\n\n", name, status)

	case "error":
		errMsg, _ := evt.Data["error"].(string)
		if opts.Redact {
			errMsg = redactText(errMsg)
		}
		fmt.Fprintf(w, "**Error:** %s\n\n", errMsg)

	case "usage":
		input, _ := evt.Data["input_tokens"].(float64)
		output, _ := evt.Data["output_tokens"].(float64)
		cached, _ := evt.Data["cached_input_tokens"].(float64)
		fmt.Fprintf(w, "**Usage:** input=%d, output=%d, cached=%d\n\n", int(input), int(output), int(cached))

	case "done":
		fmt.Fprintf(w, "---\n\n")
	}
}

// exportRaw exports the raw JSONL file
func exportRaw(w io.Writer, filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(w, file)
	return err
}

// formatMessageContent converts message content to a string
func formatMessageContent(content any, opts ExportOptions) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, part := range c {
			if p, ok := part.(map[string]any); ok {
				if text, ok := p["text"].(string); ok {
					parts = append(parts, text)
				}
				if tc, ok := p["tool_call"].(map[string]any); ok {
					name, _ := tc["name"].(string)
					parts = append(parts, fmt.Sprintf("[Tool Call: %s]", name))
				}
				if tr, ok := p["tool_result"].(map[string]any); ok {
					name, _ := tr["name"].(string)
					parts = append(parts, fmt.Sprintf("[Tool Result: %s]", name))
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		data, _ := json.Marshal(content)
		return string(data)
	}
}

// redactEntry redacts sensitive data from a parsed entry
func redactEntry(entry map[string]any) {
	// Redact request data
	if req, ok := entry["request"].(map[string]any); ok {
		if messages, ok := req["messages"].([]any); ok {
			for _, msg := range messages {
				if m, ok := msg.(map[string]any); ok {
					if content, ok := m["content"].(string); ok {
						m["content"] = redactText(content)
					}
				}
			}
		}
	}

	// Redact event data
	if data, ok := entry["data"].(map[string]any); ok {
		if text, ok := data["text"].(string); ok {
			data["text"] = redactText(text)
		}
		if args, ok := data["arguments"].(string); ok {
			data["arguments"] = redactText(args)
		}
		if content, ok := data["content"].(string); ok {
			data["content"] = redactText(content)
		}
		if err, ok := data["error"].(string); ok {
			data["error"] = redactText(err)
		}
	}
}

// redactText redacts sensitive patterns from text
func redactText(text string) string {
	// API keys (common patterns)
	text = regexp.MustCompile(`(?i)(sk-[a-zA-Z0-9]{20,})`).ReplaceAllString(text, "[REDACTED_API_KEY]")
	text = regexp.MustCompile(`(?i)(api[_-]?key[\"']?\s*[:=]\s*[\"']?)[a-zA-Z0-9_-]{20,}`).ReplaceAllString(text, "${1}[REDACTED]")

	// Home directory paths
	homeDir, _ := os.UserHomeDir()
	if homeDir != "" {
		text = strings.ReplaceAll(text, homeDir, "~")
	}

	// File contents that look like code (long lines with common patterns)
	// This is a heuristic - actual file contents in tool results
	lines := strings.Split(text, "\n")
	if len(lines) > 50 {
		// Truncate very long content
		text = strings.Join(lines[:50], "\n") + "\n[...content truncated for export...]"
	}

	return text
}

// ExportSessionByID exports a session by ID or number
func ExportSessionByID(dir, identifier string, w io.Writer, opts ExportOptions) error {
	summary, err := ResolveSession(dir, identifier)
	if err != nil {
		return err
	}
	if summary == nil {
		return fmt.Errorf("session not found: %s", identifier)
	}

	session, err := ParseSession(summary.FilePath)
	if err != nil {
		return err
	}

	return Export(w, session, opts)
}

// ExportRawByID exports raw JSONL by session ID or number
func ExportRawByID(dir, identifier string, w io.Writer) error {
	summary, err := ResolveSession(dir, identifier)
	if err != nil {
		return err
	}
	if summary == nil {
		return fmt.Errorf("session not found: %s", identifier)
	}

	return exportRaw(w, summary.FilePath)
}

// GetSessionFilePath returns the file path for a session
func GetSessionFilePath(dir, identifier string) (string, error) {
	summary, err := ResolveSession(dir, identifier)
	if err != nil {
		return "", err
	}
	if summary == nil {
		return "", fmt.Errorf("session not found: %s", identifier)
	}
	return summary.FilePath, nil
}

// DirSize returns the total size of all JSONL files in the directory
func DirSize(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	var total int64
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total, nil
}

// capitalizeFirst capitalizes the first letter of a string
func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-32) + s[1:]
	}
	return s
}

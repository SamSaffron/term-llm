package debuglog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	// Redact forces metadata-only output, even if IncludeSensitive is also set.
	// Redaction is the default; false alone does not opt out.
	Redact bool
	// IncludeSensitive explicitly opts out of structural redaction. Intended for
	// local diagnostics only: output can contain credentials and private content.
	IncludeSensitive bool
}

// Export exports a session in the specified format
func Export(w io.Writer, session *Session, opts ExportOptions) error {
	if opts.shouldRedact() {
		session = redactSession(session)
	}
	switch opts.Format {
	case FormatJSON:
		return exportJSON(w, session, opts)
	case FormatMarkdown:
		return exportMarkdown(w, session)
	case FormatRaw:
		if opts.shouldRedact() {
			return exportRedactedRaw(w, session.FilePath)
		}
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

		if opts.shouldRedact() {
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
func exportMarkdown(w io.Writer, session *Session) error {
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
			exportRequestMarkdown(w, e)
		case EventEntry:
			exportEventMarkdown(w, e)
		}
	}

	return nil
}

// exportRequestMarkdown exports a request entry as markdown
func exportRequestMarkdown(w io.Writer, req RequestEntry) {
	fmt.Fprintf(w, "### Request (%s)\n\n", req.Timestamp.Local().Format("15:04:05"))
	fmt.Fprintf(w, "- Provider: %s\n", req.Provider)
	fmt.Fprintf(w, "- Model: %s\n", req.Model)
	if req.Request.SessionID != "" {
		fmt.Fprintf(w, "- Session key: %s\n", req.Request.SessionID)
	}
	fmt.Fprintf(w, "- Messages: %d\n", len(req.Request.Messages))
	fmt.Fprintf(w, "- Tools: %d\n", len(req.Request.Tools))
	if req.Request.ReasoningReplayParts > 0 || req.Request.ReasoningEncryptedParts > 0 {
		fmt.Fprintf(
			w,
			"- Reasoning replay: parts=%d, encrypted=%d\n",
			req.Request.ReasoningReplayParts,
			req.Request.ReasoningEncryptedParts,
		)
	}
	fmt.Fprintln(w)

	// Show messages
	for i, msg := range req.Request.Messages {
		role := capitalizeFirst(msg.Role)
		fmt.Fprintf(w, "**%s Message %d:**\n", role, i+1)

		content := formatMessageContent(msg.Content)

		if len(content) > 500 {
			content = content[:500] + "\n...(truncated)"
		}
		fmt.Fprintf(w, "```\n%s\n```\n\n", content)
	}
}

// exportEventMarkdown exports an event entry as markdown
func exportEventMarkdown(w io.Writer, evt EventEntry) {
	switch evt.EventType {
	case "tool_call":
		name, _ := evt.Data["name"].(string)
		args, _ := evt.Data["arguments"].(string)
		fmt.Fprintf(w, "#### Tool Call: %s (%s)\n\n", name, evt.Timestamp.Local().Format("15:04:05"))
		if args != "" {
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
		fmt.Fprintf(w, "**Error:** %s\n\n", errMsg)

	case "usage":
		fmt.Fprintf(w, "**Usage:** %s\n\n", usageDebugLine(evt.Data))

	case "reasoning_delta":
		itemID, _ := evt.Data["reasoning_item_id"].(string)
		textLen, _ := evt.Data["text_len"].(float64)
		encLen, _ := evt.Data["reasoning_encrypted_content_len"].(float64)
		encHash, _ := evt.Data["reasoning_encrypted_content_hash"].(string)
		fmt.Fprintf(
			w,
			"**Reasoning:** item=%s, text_len=%d, encrypted_len=%d, encrypted_hash=%s\n\n",
			itemID,
			int(textLen),
			int(encLen),
			encHash,
		)

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
func formatMessageContent(content any) string {
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
				if reasoning, ok := p["reasoning_content"].(string); ok && strings.TrimSpace(reasoning) != "" {
					parts = append(parts, fmt.Sprintf("[Reasoning Summary] %s", reasoning))
				}
				if reasoningID, ok := p["reasoning_item_id"].(string); ok && strings.TrimSpace(reasoningID) != "" {
					parts = append(parts, fmt.Sprintf("[Reasoning Item ID] %s", reasoningID))
				}
				if encLen, ok := p["reasoning_encrypted_content_len"].(float64); ok && int(encLen) > 0 {
					hash, _ := p["reasoning_encrypted_content_hash"].(string)
					parts = append(parts, fmt.Sprintf("[Reasoning Encrypted] len=%d hash=%s", int(encLen), hash))
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

// ExportRawByID exports metadata-only JSONL by session ID or number.
// Use ExportSessionByID with IncludeSensitive for unredacted local diagnostics.
func ExportRawByID(dir, identifier string, w io.Writer) error {
	return ExportSessionByID(dir, identifier, w, ExportOptions{Format: FormatRaw})
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

package debuglog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/ui"
)

// FormatOptions controls how session output is formatted
type FormatOptions struct {
	ShowTools     bool // Highlight tool calls/results
	RequestsOnly  bool // Only show requests, not streaming events
	NoColor       bool // Disable colors
	ShowTimestamp bool // Show timestamp for each entry
}

// FormatSessionList formats a list of sessions as a table
func FormatSessionList(w io.Writer, sessions []SessionSummary, days int) {
	if len(sessions) == 0 {
		fmt.Fprintln(w, "No debug sessions found.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Enable debug logging with: term-llm debug-log enable")
		return
	}

	styles := ui.NewStyles(os.Stderr)

	fmt.Fprintf(w, "%s\n\n", styles.Muted.Render(fmt.Sprintf("Debug Sessions (last %d days)", days)))

	var totIn, totOut, totCache int
	for i, s := range sessions {
		num := i + 1

		// Build provider/model display string
		providerModel := s.Provider
		if s.Model != "" {
			providerModel = fmt.Sprintf("%s / %s", s.Provider, s.Model)
		}
		// Truncate if too long
		if len(providerModel) > 40 {
			providerModel = providerModel[:37] + "..."
		}

		totIn += s.Input
		totOut += s.Output
		totCache += s.Cached

		// Error indicator
		errMark := " "
		if s.HasErrors {
			errMark = styles.Error.Render("!")
		}

		// Token display: in/out (cached) - compact format
		tokenStr := formatTokens(s.Input, s.Output, s.Cached)

		timeStr := s.StartTime.Local().Format("Jan 02 15:04")
		fmt.Fprintf(w, "%s%2d. %s  %-40s  %s\n",
			errMark,
			num,
			styles.Muted.Render(timeStr),
			providerModel,
			tokenStr,
		)
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s\n", styles.Muted.Render(
		fmt.Sprintf("Total: %d sessions  %s", len(sessions), formatTokens(totIn, totOut, totCache)),
	))
	fmt.Fprintln(w)
	fmt.Fprintln(w, styles.Muted.Render("Use `term-llm debug-log show 1` to view a session"))
}

// formatTokens formats token counts in a compact readable way
func formatTokens(in, out, cached int) string {
	// Format: in→out (cached)
	parts := []string{}

	if in > 0 || out > 0 {
		parts = append(parts, fmt.Sprintf("%s→%s", compactNum(in), compactNum(out)))
	}
	if cached > 0 {
		parts = append(parts, fmt.Sprintf("cached:%s", compactNum(cached)))
	}

	if len(parts) == 0 {
		return "0 tokens"
	}
	return strings.Join(parts, " ")
}

// compactNum formats a number in a compact way (1.2K, 1.5M, etc.)
func compactNum(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 10000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	if n < 1000000 {
		return fmt.Sprintf("%dK", n/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1000000)
}

// FormatSession formats a full session for display
func FormatSession(w io.Writer, session *Session, opts FormatOptions) {
	styles := ui.NewStyles(os.Stderr)

	// Header
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s %s\n",
		styles.Highlighted.Render("Session:"),
		session.ID,
	)
	fmt.Fprintf(w, "%s %s/%s\n",
		styles.Muted.Render("Provider:"),
		session.Provider,
		session.Model,
	)
	fmt.Fprintf(w, "%s %s\n",
		styles.Muted.Render("Started:"),
		session.StartTime.Local().Format("2006-01-02 15:04:05"),
	)
	if !session.EndTime.IsZero() && session.EndTime.After(session.StartTime) {
		duration := session.EndTime.Sub(session.StartTime).Round(time.Millisecond)
		fmt.Fprintf(w, "%s %s\n", styles.Muted.Render("Duration:"), duration)
	}
	fmt.Fprintf(w, "%s %d requests\n", styles.Muted.Render("Turns:"), session.Turns)
	fmt.Fprintf(w, "%s input=%s output=%s cached=%s\n",
		styles.Muted.Render("Tokens:"),
		formatNumber(session.TotalTokens.Input),
		formatNumber(session.TotalTokens.Output),
		formatNumber(session.TotalTokens.Cached),
	)
	if session.HasErrors {
		fmt.Fprintf(w, "%s\n", styles.Error.Render("Has errors"))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, styles.Muted.Render(strings.Repeat("─", 78)))
	fmt.Fprintln(w)

	// Format entries
	for _, entry := range session.Entries {
		switch e := entry.(type) {
		case RequestEntry:
			formatRequestEntry(w, e, opts, styles)
		case EventEntry:
			if !opts.RequestsOnly {
				formatEventEntry(w, e, opts, styles)
			}
		}
	}
}

// formatRequestEntry formats a single request entry
func formatRequestEntry(w io.Writer, req RequestEntry, opts FormatOptions, styles *ui.Styles) {
	ts := ""
	if opts.ShowTimestamp {
		ts = req.Timestamp.Local().Format("15:04:05") + " "
	}

	fmt.Fprintf(w, "%s%s %s/%s\n",
		ts,
		styles.Highlighted.Render("REQUEST"),
		req.Provider,
		req.Model,
	)

	// Summary of messages
	msgCount := len(req.Request.Messages)
	toolCount := len(req.Request.Tools)
	fmt.Fprintf(w, "         Messages: %d, Tools: %d\n", msgCount, toolCount)

	// Show tool choice if specified
	if req.Request.ToolChoice != nil && req.Request.ToolChoice.Mode != "" {
		fmt.Fprintf(w, "         Tool choice: %s", req.Request.ToolChoice.Mode)
		if req.Request.ToolChoice.Name != "" {
			fmt.Fprintf(w, " (%s)", req.Request.ToolChoice.Name)
		}
		fmt.Fprintln(w)
	}

	// Show last user message if it's a string
	for i := msgCount - 1; i >= 0; i-- {
		msg := req.Request.Messages[i]
		if msg.Role == "user" {
			if text, ok := msg.Content.(string); ok && text != "" {
				// Truncate long messages
				if len(text) > 200 {
					text = text[:197] + "..."
				}
				// Replace newlines for compact display
				text = strings.ReplaceAll(text, "\n", " ")
				fmt.Fprintf(w, "         %s: %s\n", styles.Muted.Render("User"), text)
			}
			break
		}
	}
	fmt.Fprintln(w)
}

// formatEventEntry formats a single event entry
func formatEventEntry(w io.Writer, evt EventEntry, opts FormatOptions, styles *ui.Styles) {
	ts := ""
	if opts.ShowTimestamp {
		ts = evt.Timestamp.Local().Format("15:04:05") + " "
	}

	switch evt.EventType {
	case "text_delta":
		if !opts.ShowTools {
			return // Skip text deltas unless showing tools
		}
		if text, ok := evt.Data["text"].(string); ok {
			// Truncate long text deltas
			if len(text) > 100 {
				text = text[:97] + "..."
			}
			text = strings.ReplaceAll(text, "\n", "\\n")
			fmt.Fprintf(w, "%sTEXT_DELTA %q\n", ts, text)
		}

	case "tool_call":
		name, _ := evt.Data["name"].(string)
		id, _ := evt.Data["id"].(string)
		fmt.Fprintf(w, "%s%s %s",
			ts,
			styles.Bold.Render("TOOL_CALL"),
			name,
		)
		if opts.ShowTools {
			if args, ok := evt.Data["arguments"].(string); ok && args != "" {
				// Truncate arguments
				if len(args) > 100 {
					args = args[:97] + "..."
				}
				fmt.Fprintf(w, " %s", styles.Muted.Render(args))
			}
		}
		if id != "" {
			fmt.Fprintf(w, " %s", styles.Muted.Render("["+id+"]"))
		}
		fmt.Fprintln(w)

	case "tool_exec_start":
		name, _ := evt.Data["tool_name"].(string)
		fmt.Fprintf(w, "%s%s %s\n", ts, styles.Muted.Render("TOOL_START"), name)

	case "tool_exec_end":
		name, _ := evt.Data["tool_name"].(string)
		success, _ := evt.Data["success"].(bool)
		status := styles.Success.Render("success")
		if !success {
			status = styles.Error.Render("failed")
		}
		fmt.Fprintf(w, "%s%s %s (%s)\n", ts, styles.Muted.Render("TOOL_END"), name, status)

	case "usage":
		input, _ := evt.Data["input_tokens"].(float64)
		output, _ := evt.Data["output_tokens"].(float64)
		cached, _ := evt.Data["cached_input_tokens"].(float64)
		fmt.Fprintf(w, "%s%s input=%d output=%d cached=%d\n",
			ts,
			styles.Muted.Render("USAGE"),
			int(input),
			int(output),
			int(cached),
		)

	case "done":
		fmt.Fprintf(w, "%s%s\n", ts, styles.Success.Render("DONE"))
		fmt.Fprintln(w)

	case "error":
		errMsg, _ := evt.Data["error"].(string)
		fmt.Fprintf(w, "%s%s %s\n", ts, styles.Error.Render("ERROR"), errMsg)

	case "retry":
		attempt, _ := evt.Data["attempt"].(float64)
		maxAttempts, _ := evt.Data["max_attempts"].(float64)
		waitSecs, _ := evt.Data["wait_secs"].(float64)
		fmt.Fprintf(w, "%s%s attempt %d/%d (waiting %.1fs)\n",
			ts,
			styles.Bold.Render("RETRY"),
			int(attempt),
			int(maxAttempts),
			waitSecs,
		)

	case "phase":
		phase, _ := evt.Data["phase"].(string)
		fmt.Fprintf(w, "%s%s %s\n", ts, styles.Muted.Render("PHASE"), phase)

	default:
		// Unknown event type - show it generically
		if opts.ShowTools {
			data, _ := json.Marshal(evt.Data)
			fmt.Fprintf(w, "%s%s %s\n", ts, evt.EventType, string(data))
		}
	}
}

// FormatTailEntry formats a single entry for tail mode (compact)
func FormatTailEntry(w io.Writer, line []byte) {
	var entry rawEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return
	}

	ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
	if err != nil {
		return
	}

	styles := ui.NewStyles(os.Stderr)
	timeStr := ts.Local().Format("15:04:05")

	switch entry.Type {
	case "request":
		fmt.Fprintf(w, "[%s] %s %s/%s\n",
			timeStr,
			styles.Highlighted.Render("REQUEST"),
			entry.Provider,
			entry.Model,
		)

		var req RequestData
		if entry.Request != nil {
			json.Unmarshal(entry.Request, &req)
			fmt.Fprintf(w, "           Messages: %d, Tools: %d\n", len(req.Messages), len(req.Tools))
		}

	case "event":
		var data map[string]any
		if entry.Data != nil {
			json.Unmarshal(entry.Data, &data)
		}

		switch entry.EventType {
		case "text_delta":
			if text, ok := data["text"].(string); ok {
				if len(text) > 60 {
					text = text[:57] + "..."
				}
				text = strings.ReplaceAll(text, "\n", "\\n")
				fmt.Fprintf(w, "[%s] TEXT_DELTA %q\n", timeStr, text)
			}

		case "tool_call":
			name, _ := data["name"].(string)
			fmt.Fprintf(w, "[%s] %s %s\n", timeStr, styles.Bold.Render("TOOL_CALL"), name)

		case "tool_exec_end":
			name, _ := data["tool_name"].(string)
			success, _ := data["success"].(bool)
			status := styles.Success.Render("success")
			if !success {
				status = styles.Error.Render("failed")
			}
			fmt.Fprintf(w, "[%s] TOOL_END %s (%s)\n", timeStr, name, status)

		case "usage":
			input, _ := data["input_tokens"].(float64)
			output, _ := data["output_tokens"].(float64)
			cached, _ := data["cached_input_tokens"].(float64)
			fmt.Fprintf(w, "[%s] %s input=%d output=%d cached=%d\n",
				timeStr,
				styles.Muted.Render("USAGE"),
				int(input),
				int(output),
				int(cached),
			)

		case "done":
			fmt.Fprintf(w, "[%s] %s\n", timeStr, styles.Success.Render("DONE"))

		case "error":
			errMsg, _ := data["error"].(string)
			fmt.Fprintf(w, "[%s] %s %s\n", timeStr, styles.Error.Render("ERROR"), errMsg)
		}
	}
}

// formatNumber formats a number with commas
func formatNumber(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}

	s := fmt.Sprintf("%d", n)
	result := make([]byte, 0, len(s)+len(s)/3)
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

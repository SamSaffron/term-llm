package debuglog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SearchOptions controls search behavior
type SearchOptions struct {
	Query      string // Text query to search for
	ToolName   string // Filter by tool name
	Provider   string // Filter by provider
	ErrorsOnly bool   // Only show sessions/entries with errors
	Days       int    // Only search sessions from last N days
}

// SearchResult represents a search match
type SearchResult struct {
	SessionID string
	FilePath  string
	LineNum   int
	Timestamp time.Time
	EntryType string // "request" or "event"
	EventType string // For events: "text_delta", "tool_call", etc.
	Match     string // The matching content
	Context   string // Additional context
}

// Search searches across all sessions for matching entries
func Search(dir string, opts SearchOptions) ([]SearchResult, error) {
	sessions, err := ListSessions(dir)
	if err != nil {
		return nil, err
	}

	// Filter by days
	if opts.Days > 0 {
		cutoff := time.Now().AddDate(0, 0, -opts.Days)
		var filtered []SessionSummary
		for _, s := range sessions {
			if s.StartTime.After(cutoff) {
				filtered = append(filtered, s)
			}
		}
		sessions = filtered
	}

	// Filter by provider
	if opts.Provider != "" {
		var filtered []SessionSummary
		for _, s := range sessions {
			if strings.EqualFold(s.Provider, opts.Provider) {
				filtered = append(filtered, s)
			}
		}
		sessions = filtered
	}

	// Filter by errors
	if opts.ErrorsOnly {
		var filtered []SessionSummary
		for _, s := range sessions {
			if s.HasErrors {
				filtered = append(filtered, s)
			}
		}
		sessions = filtered
	}

	var results []SearchResult
	for _, session := range sessions {
		sessionResults, err := searchSession(session.FilePath, opts)
		if err != nil {
			continue
		}
		results = append(results, sessionResults...)
	}

	return results, nil
}

// searchSession searches a single session file
func searchSession(filePath string, opts SearchOptions) ([]SearchResult, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	sessionID := strings.TrimSuffix(filepath.Base(filePath), ".jsonl")

	var results []SearchResult
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()

		var entry rawEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		ts, _ := time.Parse(time.RFC3339Nano, entry.Timestamp)

		// Check if entry matches search criteria
		match, matchStr, context := matchEntry(entry, line, opts)
		if !match {
			continue
		}

		results = append(results, SearchResult{
			SessionID: sessionID,
			FilePath:  filePath,
			LineNum:   lineNum,
			Timestamp: ts,
			EntryType: entry.Type,
			EventType: entry.EventType,
			Match:     matchStr,
			Context:   context,
		})
	}

	return results, scanner.Err()
}

// matchEntry checks if an entry matches the search criteria
func matchEntry(entry rawEntry, line []byte, opts SearchOptions) (bool, string, string) {
	// Tool name filter
	if opts.ToolName != "" {
		if entry.Type == "event" {
			switch entry.EventType {
			case "tool_call", "tool_exec_start", "tool_exec_end":
				var data map[string]any
				if entry.Data != nil {
					json.Unmarshal(entry.Data, &data)
				}

				name, _ := data["name"].(string)
				if name == "" {
					name, _ = data["tool_name"].(string)
				}

				if !strings.EqualFold(name, opts.ToolName) {
					return false, "", ""
				}

				return true, name, entry.EventType
			default:
				return false, "", ""
			}
		}
		return false, "", ""
	}

	// Errors only filter
	if opts.ErrorsOnly {
		if entry.Type == "event" && entry.EventType == "error" {
			var data map[string]any
			if entry.Data != nil {
				json.Unmarshal(entry.Data, &data)
			}
			errMsg, _ := data["error"].(string)
			return true, errMsg, "error"
		}
		return false, "", ""
	}

	// Text query
	if opts.Query != "" {
		query := strings.ToLower(opts.Query)
		lineStr := strings.ToLower(string(line))

		if strings.Contains(lineStr, query) {
			// Find the actual matching part
			idx := strings.Index(lineStr, query)
			start := idx - 30
			if start < 0 {
				start = 0
			}
			end := idx + len(query) + 30
			if end > len(lineStr) {
				end = len(lineStr)
			}

			matchStr := string(line)[start:end]
			if start > 0 {
				matchStr = "..." + matchStr
			}
			if end < len(lineStr) {
				matchStr = matchStr + "..."
			}

			context := entry.EventType
			if context == "" {
				context = entry.Type
			}

			return true, matchStr, context
		}
		return false, "", ""
	}

	// No filters - return nothing (user must specify something to search for)
	return false, "", ""
}

// FilterSessions filters sessions by various criteria
func FilterSessions(sessions []SessionSummary, opts SearchOptions) []SessionSummary {
	result := sessions

	// Filter by days
	if opts.Days > 0 {
		cutoff := time.Now().AddDate(0, 0, -opts.Days)
		var filtered []SessionSummary
		for _, s := range result {
			if s.StartTime.After(cutoff) {
				filtered = append(filtered, s)
			}
		}
		result = filtered
	}

	// Filter by provider
	if opts.Provider != "" {
		var filtered []SessionSummary
		for _, s := range result {
			if strings.EqualFold(s.Provider, opts.Provider) {
				filtered = append(filtered, s)
			}
		}
		result = filtered
	}

	// Filter by errors
	if opts.ErrorsOnly {
		var filtered []SessionSummary
		for _, s := range result {
			if s.HasErrors {
				filtered = append(filtered, s)
			}
		}
		result = filtered
	}

	return result
}

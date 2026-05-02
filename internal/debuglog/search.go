package debuglog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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
	if canUseTextQueryFastPath(opts) {
		return searchTextQuerySessions(dir, opts)
	}

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

func canUseTextQueryFastPath(opts SearchOptions) bool {
	// Plain text search is the common case for `debug-log search`. It does not
	// need provider/error summaries, so avoid ListSessions' full JSON parse of
	// every log before scanning the same files again.
	return opts.Query != "" && opts.ToolName == "" && opts.Provider == "" && !opts.ErrorsOnly
}

func searchTextQuerySessions(dir string, opts SearchOptions) ([]SearchResult, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	cutoff := time.Time{}
	if opts.Days > 0 {
		cutoff = time.Now().AddDate(0, 0, -opts.Days)
	}

	sessions := make([]SessionSummary, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}

		filePath := filepath.Join(dir, entry.Name())
		startTime, err := firstSessionTimestamp(filePath)
		if err != nil {
			continue
		}
		if opts.Days > 0 && (startTime.IsZero() || !startTime.After(cutoff)) {
			continue
		}

		sessions = append(sessions, SessionSummary{
			ID:        strings.TrimSuffix(entry.Name(), ".jsonl"),
			FilePath:  filePath,
			StartTime: startTime,
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartTime.After(sessions[j].StartTime)
	})

	var results []SearchResult
	for _, session := range sessions {
		sessionResults, err := searchSessionTextQuery(session.FilePath, opts.Query)
		if err != nil {
			continue
		}
		results = append(results, sessionResults...)
	}
	return results, nil
}

func firstSessionTimestamp(filePath string) (time.Time, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return time.Time{}, err
	}
	defer file.Close()

	scanner := newDebugLogScanner(file)
	for scanner.Scan() {
		var entry rawEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil {
			continue
		}
		return ts, nil
	}
	return time.Time{}, scanner.Err()
}

func searchSessionTextQuery(filePath, query string) ([]SearchResult, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	sessionID := strings.TrimSuffix(filepath.Base(filePath), ".jsonl")
	queryLower := strings.ToLower(query)
	queryASCII := isASCII(queryLower)

	var results []SearchResult
	scanner := newDebugLogScanner(file)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()

		match, matchStr := matchTextQueryLine(line, query, queryLower, queryASCII)
		if !match {
			continue
		}

		var entry rawEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		ts, _ := time.Parse(time.RFC3339Nano, entry.Timestamp)
		context := entry.EventType
		if context == "" {
			context = entry.Type
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

func matchTextQueryLine(line []byte, query, queryLower string, queryASCII bool) (bool, string) {
	if query == "" {
		return false, ""
	}

	if queryASCII {
		idx := indexFoldASCII(line, queryLower)
		if idx < 0 {
			return false, ""
		}
		return true, snippetBytes(line, idx, len(query))
	}

	lineStr := string(line)
	lineLower := strings.ToLower(lineStr)
	idx := strings.Index(lineLower, queryLower)
	if idx < 0 || idx > len(lineStr) {
		return false, ""
	}
	return true, snippetString(lineStr, idx, len(queryLower), len(lineLower))
}

func snippetBytes(line []byte, idx, queryLen int) string {
	start := idx - 30
	if start < 0 {
		start = 0
	}
	end := idx + queryLen + 30
	if end > len(line) {
		end = len(line)
	}

	matchStr := string(line[start:end])
	if start > 0 {
		matchStr = "..." + matchStr
	}
	if end < len(line) {
		matchStr += "..."
	}
	return matchStr
}

func snippetString(line string, idx, queryLen, lowerLen int) string {
	start := idx - 30
	if start < 0 {
		start = 0
	}
	end := idx + queryLen + 30
	if end > lowerLen {
		end = lowerLen
	}
	if end > len(line) {
		end = len(line)
	}
	if start > len(line) {
		start = len(line)
	}
	if end < start {
		end = start
	}

	matchStr := line[start:end]
	if start > 0 {
		matchStr = "..." + matchStr
	}
	if end < len(line) {
		matchStr += "..."
	}
	return matchStr
}

func indexFoldASCII(line []byte, queryLower string) int {
	if len(queryLower) == 0 {
		return 0
	}
	if len(queryLower) > len(line) {
		return -1
	}

	first := queryLower[0]
	limit := len(line) - len(queryLower)
	for i := 0; i <= limit; i++ {
		if asciiLower(line[i]) != first {
			continue
		}
		matched := true
		for j := 1; j < len(queryLower); j++ {
			if asciiLower(line[i+j]) != queryLower[j] {
				matched = false
				break
			}
		}
		if matched {
			return i
		}
	}
	return -1
}

func asciiLower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
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
	scanner := newDebugLogScanner(file)

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
		queryLower := strings.ToLower(opts.Query)
		match, matchStr := matchTextQueryLine(line, opts.Query, queryLower, isASCII(queryLower))
		if match {
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

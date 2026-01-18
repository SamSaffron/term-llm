package debuglog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// rawEntry is the raw JSON structure for parsing
type rawEntry struct {
	Timestamp string          `json:"timestamp"`
	SessionID string          `json:"session_id"`
	Type      string          `json:"type"`
	Provider  string          `json:"provider,omitempty"`
	Model     string          `json:"model,omitempty"`
	Request   json.RawMessage `json:"request,omitempty"`
	EventType string          `json:"event_type,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// ListSessions returns summaries of all sessions in the debug log directory,
// sorted by start time (most recent first).
func ListSessions(dir string) ([]SessionSummary, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var sessions []SessionSummary
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}

		filePath := filepath.Join(dir, entry.Name())
		summary, err := parseSessionSummary(filePath)
		if err != nil {
			continue // Skip malformed files
		}
		sessions = append(sessions, summary)
	}

	// Sort by start time, most recent first
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartTime.After(sessions[j].StartTime)
	})

	return sessions, nil
}

// parseSessionSummary extracts summary info from a session file
func parseSessionSummary(filePath string) (SessionSummary, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return SessionSummary{}, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return SessionSummary{}, err
	}

	summary := SessionSummary{
		ID:       strings.TrimSuffix(filepath.Base(filePath), ".jsonl"),
		FilePath: filePath,
		FileSize: info.Size(),
	}

	scanner := bufio.NewScanner(file)
	// Increase buffer size for large log lines
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	requestSeen := false

	for scanner.Scan() {
		var entry rawEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}

		ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil {
			continue
		}

		if summary.StartTime.IsZero() || ts.Before(summary.StartTime) {
			summary.StartTime = ts
		}

		switch entry.Type {
		case "request":
			if !requestSeen {
				summary.Provider = entry.Provider
				summary.Model = entry.Model
				requestSeen = true
			}
			summary.Calls++

		case "event":
			switch entry.EventType {
			case "usage":
				var usage struct {
					InputTokens       int `json:"input_tokens"`
					OutputTokens      int `json:"output_tokens"`
					CachedInputTokens int `json:"cached_input_tokens"`
				}
				if err := json.Unmarshal(entry.Data, &usage); err == nil {
					summary.Input += usage.InputTokens
					summary.Output += usage.OutputTokens
					summary.Cached += usage.CachedInputTokens
				}
			case "error":
				summary.HasErrors = true
			}
		}
	}
	return summary, scanner.Err()
}

// ParseSession parses a full session file into a Session struct
func ParseSession(filePath string) (*Session, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	session := &Session{
		ID:       strings.TrimSuffix(filepath.Base(filePath), ".jsonl"),
		FilePath: filePath,
	}

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		var entry rawEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}

		ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil {
			continue
		}

		if session.StartTime.IsZero() || ts.Before(session.StartTime) {
			session.StartTime = ts
		}
		if ts.After(session.EndTime) {
			session.EndTime = ts
		}

		switch entry.Type {
		case "request":
			reqEntry := parseRequestEntry(entry, ts)
			session.Entries = append(session.Entries, reqEntry)
			session.Turns++
			if session.Provider == "" {
				session.Provider = reqEntry.Provider
				session.Model = reqEntry.Model
			}

		case "event":
			evtEntry := parseEventEntry(entry, ts)
			session.Entries = append(session.Entries, evtEntry)

			if evtEntry.EventType == "usage" {
				if input, ok := evtEntry.Data["input_tokens"].(float64); ok {
					session.TotalTokens.Input += int(input)
				}
				if output, ok := evtEntry.Data["output_tokens"].(float64); ok {
					session.TotalTokens.Output += int(output)
				}
				if cached, ok := evtEntry.Data["cached_input_tokens"].(float64); ok {
					session.TotalTokens.Cached += int(cached)
				}
			}
			if evtEntry.EventType == "error" {
				session.HasErrors = true
			}
		}
	}

	return session, scanner.Err()
}

// parseRequestEntry parses a request entry from raw JSON
func parseRequestEntry(entry rawEntry, ts time.Time) RequestEntry {
	req := RequestEntry{
		Timestamp: ts,
		SessionID: entry.SessionID,
		Provider:  entry.Provider,
		Model:     entry.Model,
	}

	if entry.Request != nil {
		json.Unmarshal(entry.Request, &req.Request)
	}

	return req
}

// parseEventEntry parses an event entry from raw JSON
func parseEventEntry(entry rawEntry, ts time.Time) EventEntry {
	evt := EventEntry{
		Timestamp: ts,
		SessionID: entry.SessionID,
		EventType: entry.EventType,
	}

	if entry.Data != nil {
		json.Unmarshal(entry.Data, &evt.Data)
	}

	return evt
}

// GetSessionByNumber returns the session at the given 1-based index
// (1 = most recent)
func GetSessionByNumber(dir string, num int) (*SessionSummary, error) {
	sessions, err := ListSessions(dir)
	if err != nil {
		return nil, err
	}

	if num < 1 || num > len(sessions) {
		return nil, nil
	}

	return &sessions[num-1], nil
}

// GetSessionByID returns the session with the given ID
func GetSessionByID(dir, id string) (*SessionSummary, error) {
	sessions, err := ListSessions(dir)
	if err != nil {
		return nil, err
	}

	for _, s := range sessions {
		if s.ID == id {
			return &s, nil
		}
	}

	return nil, nil
}

// ResolveSession resolves a session identifier (number or ID) to a session summary
func ResolveSession(dir, identifier string) (*SessionSummary, error) {
	// Try as number first
	if num, err := parsePositiveInt(identifier); err == nil && num > 0 {
		return GetSessionByNumber(dir, num)
	}

	// Try as session ID
	return GetSessionByID(dir, identifier)
}

// parsePositiveInt parses a string as a positive integer
func parsePositiveInt(s string) (int, error) {
	if s == "" {
		return 0, &parseError{"empty string"}
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, &parseError{"not a number"}
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

type parseError struct {
	msg string
}

func (e *parseError) Error() string {
	return e.msg
}

// GetMostRecentSession returns the most recent session
func GetMostRecentSession(dir string) (*SessionSummary, error) {
	return GetSessionByNumber(dir, 1)
}

// ParseRawLines parses a session file and returns raw JSON lines
func ParseRawLines(filePath string) ([]json.RawMessage, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []json.RawMessage
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		// Make a copy since scanner reuses the buffer
		lineCopy := make([]byte, len(line))
		copy(lineCopy, line)
		lines = append(lines, json.RawMessage(lineCopy))
	}

	return lines, scanner.Err()
}

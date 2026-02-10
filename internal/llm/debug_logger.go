package llm

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DebugLogger logs LLM requests and events to JSONL files for debugging.
// Each session gets its own file based on the session ID.
type DebugLogger struct {
	baseDir   string
	sessionID string
	mu        sync.Mutex
	file      *os.File
	writer    *bufio.Writer
	closeOnce sync.Once
	closed    bool
}

// debugLogEntry is the common structure for all log entries
type debugLogEntry struct {
	Timestamp string `json:"timestamp"`
	SessionID string `json:"session_id"`
	Type      string `json:"type"` // "request" or "event"
}

// debugRequestEntry logs an LLM request
type debugRequestEntry struct {
	debugLogEntry
	Provider string           `json:"provider"`
	Model    string           `json:"model"`
	Request  debugRequestData `json:"request"`
}

// debugTurnRequestEntry logs a request for a specific turn in an agentic loop
type debugTurnRequestEntry struct {
	debugLogEntry
	Turn     int              `json:"turn"`
	Provider string           `json:"provider"`
	Model    string           `json:"model"`
	Request  debugRequestData `json:"request"`
}

// debugRequestData contains the request details
type debugRequestData struct {
	SessionID               string           `json:"session_id,omitempty"`
	Messages                []debugMessage   `json:"messages"`
	Tools                   []debugTool      `json:"tools,omitempty"`
	ToolChoice              *debugToolChoice `json:"tool_choice,omitempty"`
	Search                  bool             `json:"search,omitempty"`
	ForceExternalSearch     bool             `json:"force_external_search,omitempty"`
	ParallelToolCalls       bool             `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens         int              `json:"max_output_tokens,omitempty"`
	Temperature             float32          `json:"temperature,omitempty"`
	TopP                    float32          `json:"top_p,omitempty"`
	ReasoningEffort         string           `json:"reasoning_effort,omitempty"`
	ReasoningReplayParts    int              `json:"reasoning_replay_parts,omitempty"`
	ReasoningEncryptedParts int              `json:"reasoning_encrypted_parts,omitempty"`
	MaxTurns                int              `json:"max_turns,omitempty"`
}

// debugToolChoice represents tool choice settings
type debugToolChoice struct {
	Mode string `json:"mode"`
	Name string `json:"name,omitempty"`
}

// debugMessage is a simplified message for logging
type debugMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []debugPart
}

// debugPart represents a message part
type debugPart struct {
	Type                          string           `json:"type"`
	Text                          string           `json:"text,omitempty"`
	ReasoningContent              string           `json:"reasoning_content,omitempty"`
	ReasoningItemID               string           `json:"reasoning_item_id,omitempty"`
	ReasoningEncryptedContentLen  int              `json:"reasoning_encrypted_content_len,omitempty"`
	ReasoningEncryptedContentHash string           `json:"reasoning_encrypted_content_hash,omitempty"`
	ToolCall                      *debugToolCall   `json:"tool_call,omitempty"`
	ToolResult                    *debugToolResult `json:"tool_result,omitempty"`
}

// debugToolCall is a simplified tool call for logging
type debugToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// debugToolResult is a simplified tool result for logging
type debugToolResult struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// debugTool is a simplified tool spec for logging
type debugTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// debugEventEntry logs an LLM event
type debugEventEntry struct {
	debugLogEntry
	EventType string `json:"event_type"`
	Data      any    `json:"data,omitempty"`
}

// debugSessionStartEntry logs the session start with CLI args
type debugSessionStartEntry struct {
	debugLogEntry
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Cwd     string   `json:"cwd"`
}

// NewDebugLogger creates a new DebugLogger.
// The sessionID is used to create a unique filename for this session.
// Old log files (>7 days) are automatically cleaned up.
func NewDebugLogger(baseDir, sessionID string) (*DebugLogger, error) {
	if err := os.MkdirAll(baseDir, 0700); err != nil {
		return nil, err
	}

	// Clean up old log files (7-day retention)
	_ = CleanupOldLogs(baseDir, 7*24*time.Hour)

	filename := filepath.Join(baseDir, sessionID+".jsonl")
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}

	return &DebugLogger{
		baseDir:   baseDir,
		sessionID: sessionID,
		file:      file,
		writer:    bufio.NewWriter(file),
	}, nil
}

// LogSessionStart logs the session start with CLI invocation details.
func (l *DebugLogger) LogSessionStart(command string, args []string, cwd string) {
	if l == nil {
		return
	}

	entry := debugSessionStartEntry{
		debugLogEntry: debugLogEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			SessionID: l.sessionID,
			Type:      "session_start",
		},
		Command: command,
		Args:    args,
		Cwd:     cwd,
	}

	l.writeEntry(entry)
	l.Flush()
}

// LogRequest logs an LLM request.
func (l *DebugLogger) LogRequest(provider, model string, req Request) {
	if l == nil {
		return
	}

	// Use model from request if provided, otherwise use the passed model
	logModel := req.Model
	if logModel == "" {
		logModel = model
	}
	replayParts, encryptedParts := countReasoningReplayParts(req.Messages)

	entry := debugRequestEntry{
		debugLogEntry: debugLogEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			SessionID: l.sessionID,
			Type:      "request",
		},
		Provider: provider,
		Model:    logModel,
		Request: debugRequestData{
			SessionID:               req.SessionID,
			Messages:                convertMessages(req.Messages),
			Tools:                   convertTools(req.Tools),
			ToolChoice:              convertToolChoice(req.ToolChoice),
			Search:                  req.Search,
			ForceExternalSearch:     req.ForceExternalSearch,
			ParallelToolCalls:       req.ParallelToolCalls,
			MaxOutputTokens:         req.MaxOutputTokens,
			Temperature:             req.Temperature,
			TopP:                    req.TopP,
			ReasoningEffort:         req.ReasoningEffort,
			ReasoningReplayParts:    replayParts,
			ReasoningEncryptedParts: encryptedParts,
			MaxTurns:                req.MaxTurns,
		},
	}

	l.writeEntry(entry)
	// Flush requests immediately since they're infrequent and important
	l.Flush()
}

// convertToolChoice converts ToolChoice to debug format
func convertToolChoice(tc ToolChoice) *debugToolChoice {
	if tc.Mode == "" {
		return nil
	}
	return &debugToolChoice{
		Mode: string(tc.Mode),
		Name: tc.Name,
	}
}

// LogTurnRequest logs a request for a specific turn in an agentic loop.
// This captures the state after tool results have been appended.
func (l *DebugLogger) LogTurnRequest(turn int, provider, model string, req Request) {
	if l == nil {
		return
	}

	// Use model from request if provided, otherwise use the passed model
	logModel := req.Model
	if logModel == "" {
		logModel = model
	}
	replayParts, encryptedParts := countReasoningReplayParts(req.Messages)

	entry := debugTurnRequestEntry{
		debugLogEntry: debugLogEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			SessionID: l.sessionID,
			Type:      "turn_request",
		},
		Turn:     turn,
		Provider: provider,
		Model:    logModel,
		Request: debugRequestData{
			SessionID:               req.SessionID,
			Messages:                convertMessages(req.Messages),
			Tools:                   convertTools(req.Tools),
			ToolChoice:              convertToolChoice(req.ToolChoice),
			Search:                  req.Search,
			ForceExternalSearch:     req.ForceExternalSearch,
			ParallelToolCalls:       req.ParallelToolCalls,
			MaxOutputTokens:         req.MaxOutputTokens,
			Temperature:             req.Temperature,
			TopP:                    req.TopP,
			ReasoningEffort:         req.ReasoningEffort,
			ReasoningReplayParts:    replayParts,
			ReasoningEncryptedParts: encryptedParts,
			MaxTurns:                req.MaxTurns,
		},
	}

	l.writeEntry(entry)
	// Flush turn requests for debugging visibility
	l.Flush()
}

// LogEvent logs an LLM event.
func (l *DebugLogger) LogEvent(event Event) {
	if l == nil {
		return
	}

	entry := debugEventEntry{
		debugLogEntry: debugLogEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			SessionID: l.sessionID,
			Type:      "event",
		},
		EventType: string(event.Type),
	}

	// Add event-specific data
	switch event.Type {
	case EventTextDelta:
		entry.Data = map[string]string{"text": event.Text}
	case EventReasoningDelta:
		data := map[string]any{}
		if event.Text != "" {
			text := event.Text
			if len(text) > 500 {
				text = text[:500] + "...[truncated]"
			}
			data["text"] = text
			data["text_len"] = len(event.Text)
		}
		if event.ReasoningItemID != "" {
			data["reasoning_item_id"] = event.ReasoningItemID
		}
		if event.ReasoningEncryptedContent != "" {
			data["reasoning_encrypted_content_len"] = len(event.ReasoningEncryptedContent)
			data["reasoning_encrypted_content_hash"] = shortContentHash(event.ReasoningEncryptedContent)
		}
		if len(data) > 0 {
			entry.Data = data
		}
	case EventToolCall:
		if event.Tool != nil {
			entry.Data = map[string]any{
				"id":        event.Tool.ID,
				"name":      event.Tool.Name,
				"arguments": event.Tool.Arguments,
			}
		}
	case EventToolExecStart, EventToolExecEnd:
		data := map[string]any{
			"tool_call_id": event.ToolCallID,
			"tool_name":    event.ToolName,
		}
		if event.ToolInfo != "" {
			data["tool_info"] = event.ToolInfo
		}
		if event.Type == EventToolExecEnd {
			data["success"] = event.ToolSuccess
			if event.ToolOutput != "" {
				// Truncate long outputs to avoid bloating logs
				output := event.ToolOutput
				if len(output) > 500 {
					output = output[:500] + "...[truncated]"
				}
				data["output"] = output
			}
		}
		entry.Data = data
	case EventUsage:
		if event.Use != nil {
			entry.Data = map[string]int{
				"input_tokens":        event.Use.InputTokens,
				"output_tokens":       event.Use.OutputTokens,
				"cached_input_tokens": event.Use.CachedInputTokens,
			}
		}
	case EventPhase:
		entry.Data = map[string]string{"phase": event.Text}
	case EventError:
		if event.Err != nil {
			entry.Data = map[string]string{"error": event.Err.Error()}
		}
	case EventRetry:
		entry.Data = map[string]any{
			"attempt":      event.RetryAttempt,
			"max_attempts": event.RetryMaxAttempts,
			"wait_secs":    event.RetryWaitSecs,
		}
	}

	l.writeEntry(entry)

	// Flush on EventDone to ensure all events for a response are persisted
	// without flushing on every high-frequency event like text deltas
	if event.Type == EventDone {
		l.Flush()
	}
}

// Close closes the debug logger and flushes any buffered data.
// Close is idempotent and safe to call multiple times.
func (l *DebugLogger) Close() error {
	if l == nil {
		return nil
	}

	var closeErr error
	l.closeOnce.Do(func() {
		l.mu.Lock()
		defer l.mu.Unlock()

		if l.file == nil {
			return
		}

		if err := l.writer.Flush(); err != nil {
			closeErr = err
		}
		if err := l.file.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
		l.closed = true
	})
	return closeErr
}

// writeEntry writes a single log entry as a JSON line.
// Does not flush the buffer - caller is responsible for flushing when appropriate.
func (l *DebugLogger) writeEntry(entry any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}

	l.writer.Write(data)
	l.writer.WriteString("\n")
}

// flush flushes the buffered writer. Must be called with l.mu held.
func (l *DebugLogger) flushLocked() {
	if l.closed || l.writer == nil {
		return
	}
	l.writer.Flush()
}

// Flush flushes the buffered writer to disk.
func (l *DebugLogger) Flush() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushLocked()
}

// convertMessages converts llm.Message slice to debug format
func convertMessages(messages []Message) []debugMessage {
	result := make([]debugMessage, len(messages))
	for i, msg := range messages {
		result[i] = debugMessage{
			Role:    string(msg.Role),
			Content: convertParts(msg.Parts),
		}
	}
	return result
}

// convertParts converts message parts to debug format
func convertParts(parts []Part) any {
	// If there's a single text part, return just the text string
	if len(parts) == 1 && parts[0].Type == PartText && !hasReasoningMetadata(parts[0]) {
		return parts[0].Text
	}

	// Otherwise return the full parts structure
	result := make([]debugPart, len(parts))
	for i, part := range parts {
		dp := debugPart{Type: string(part.Type)}
		switch part.Type {
		case PartText:
			dp.Text = part.Text
			dp.ReasoningContent = part.ReasoningContent
			dp.ReasoningItemID = part.ReasoningItemID
			if part.ReasoningEncryptedContent != "" {
				dp.ReasoningEncryptedContentLen = len(part.ReasoningEncryptedContent)
				dp.ReasoningEncryptedContentHash = shortContentHash(part.ReasoningEncryptedContent)
			}
		case PartToolCall:
			if part.ToolCall != nil {
				dp.ToolCall = &debugToolCall{
					ID:        part.ToolCall.ID,
					Name:      part.ToolCall.Name,
					Arguments: part.ToolCall.Arguments,
				}
			}
		case PartToolResult:
			if part.ToolResult != nil {
				dp.ToolResult = &debugToolResult{
					ID:      part.ToolResult.ID,
					Name:    part.ToolResult.Name,
					Content: part.ToolResult.Content,
					IsError: part.ToolResult.IsError,
				}
			}
		}
		result[i] = dp
	}
	return result
}

// convertTools converts tool specs to debug format
func convertTools(tools []ToolSpec) []debugTool {
	if len(tools) == 0 {
		return nil
	}
	result := make([]debugTool, len(tools))
	for i, tool := range tools {
		result[i] = debugTool{
			Name:        tool.Name,
			Description: tool.Description,
		}
	}
	return result
}

func hasReasoningMetadata(part Part) bool {
	return part.ReasoningContent != "" || part.ReasoningItemID != "" || part.ReasoningEncryptedContent != ""
}

func countReasoningReplayParts(messages []Message) (replayParts int, encryptedParts int) {
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if !hasReasoningMetadata(part) {
				continue
			}
			replayParts++
			if part.ReasoningEncryptedContent != "" {
				encryptedParts++
			}
		}
	}
	return replayParts, encryptedParts
}

func shortContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	hexHash := hex.EncodeToString(sum[:])
	if len(hexHash) <= 16 {
		return hexHash
	}
	return hexHash[:16]
}

// CleanupOldLogs removes JSONL log files older than maxAge from the specified directory.
// This prevents debug logs from accumulating indefinitely.
func CleanupOldLogs(baseDir string, maxAge time.Duration) error {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(baseDir, entry.Name()))
		}
	}

	return nil
}

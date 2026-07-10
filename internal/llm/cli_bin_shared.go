package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/mcphttp"
)

const cliDiagnosticLineMaxBytes = 4 * 1024

// mcpCallCounter generates process-unique IDs for CLI-provider MCP tool calls.
var mcpCallCounter atomic.Int64

// UserFacingProviderError keeps detailed subprocess diagnostics available to
// debug logging while presenting a concise error to users.
type UserFacingProviderError struct {
	Summary string
	Detail  string
	Cause   error
}

func (e *UserFacingProviderError) Error() string {
	if e == nil {
		return "provider error"
	}
	if e.Detail != "" {
		return e.Summary + ": " + e.Detail
	}
	return e.Summary
}

func (e *UserFacingProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *UserFacingProviderError) DebugFields() map[string]any {
	if e == nil || e.Cause == nil {
		return nil
	}
	fieldsErr, ok := e.Cause.(interface{ DebugFields() map[string]any })
	if !ok {
		return nil
	}
	return fieldsErr.DebugFields()
}

// CLICommandError carries bounded, redacted diagnostics for a failed local CLI
// transport. Prompt content should not be included for prompt-file transports;
// PromptLen and PromptSHA256 are sufficient to correlate failures safely.
type CLICommandError struct {
	BinName        string
	ErrorType      string
	ExitCode       int
	Err            error
	Args           []string
	CommandLine    string
	Cwd            string
	Effort         string
	ToolsExecuted  bool
	PreferOAuth    bool
	Env            map[string]string
	RemovedEnv     []string
	Stdin          string
	StdinLen       int
	StdinSHA256    string
	StdinTruncated bool
	PromptLen      int
	PromptSHA256   string
	StdoutTail     string
	StderrTail     string
}

func (e *CLICommandError) commandName() string {
	if e == nil || strings.TrimSpace(e.BinName) == "" {
		return "CLI"
	}
	return strings.TrimSpace(e.BinName)
}

func (e *CLICommandError) Error() string {
	if e == nil {
		return "CLI command failed"
	}
	msg := fmt.Sprintf("%s command failed (exit %d): %v", e.commandName(), e.ExitCode, e.Err)
	if e.StderrTail != "" {
		msg += "\nstderr:\n" + e.StderrTail
	}
	return msg
}

func (e *CLICommandError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// DebugFields is consumed by DebugLogger when this error is emitted as an
// EventError. Keep field values JSON-friendly.
func (e *CLICommandError) DebugFields() map[string]any {
	if e == nil {
		return nil
	}
	errorType := e.ErrorType
	if errorType == "" {
		errorType = strings.ToLower(strings.ReplaceAll(e.commandName(), "-", "_")) + "_cli_command"
	}
	fields := map[string]any{
		"provider_error_type": errorType,
		"exit_code":           e.ExitCode,
		"command":             e.commandName(),
		"args":                e.Args,
		"command_line":        e.CommandLine,
		"cwd":                 e.Cwd,
		"tools_executed":      e.ToolsExecuted,
		"prefer_oauth":        e.PreferOAuth,
		"stdin_len":           e.StdinLen,
		"stdin_sha256":        e.StdinSHA256,
		"stdin_truncated":     e.StdinTruncated,
		"stdin":               e.Stdin,
	}
	if e.PromptLen > 0 || e.PromptSHA256 != "" {
		fields["prompt_len"] = e.PromptLen
		fields["prompt_sha256"] = e.PromptSHA256
	}
	if e.Effort != "" {
		fields["effort"] = e.Effort
	}
	if len(e.Env) > 0 {
		fields["env"] = e.Env
	}
	if len(e.RemovedEnv) > 0 {
		fields["removed_env"] = e.RemovedEnv
	}
	if e.StdoutTail != "" {
		fields["stdout_tail"] = e.StdoutTail
	}
	if e.StderrTail != "" {
		fields["stderr_tail"] = e.StderrTail
	}
	return fields
}

type cliToolRequest struct {
	ctx    context.Context
	callID string
	name   string
	args   json.RawMessage
	// response is completed by engine tool execution once EventToolCall is handled.
	response chan<- ToolExecutionResponse
	// ack is completed by the turn dispatcher after the request is either forwarded
	// to the stream events channel or rejected (stream closed/cancelled).
	ack chan error
}

type cliTurnBridge struct {
	// toolReqCh routes wrapped MCP tool requests through the active turn dispatcher,
	// ensuring deterministic ordering relative to streamed stdout lines.
	toolReqCh chan cliToolRequest
	// done closes when the active CLI subprocess turn exits.
	done chan struct{}
}

// cliToolBridgeState is embedded by local CLI providers. The MCP server can
// persist across turns while this state points each incoming tool call at the
// currently active stream.
type cliToolBridgeState struct {
	currentBridge *cliTurnBridge
	currentEvents chan<- Event
	eventsMu      sync.Mutex
}

func (s *cliToolBridgeState) activate(bridge *cliTurnBridge, events chan<- Event) {
	s.eventsMu.Lock()
	s.currentBridge = bridge
	s.currentEvents = events
	s.eventsMu.Unlock()
}

func (s *cliToolBridgeState) deactivate(bridge *cliTurnBridge) {
	s.eventsMu.Lock()
	if s.currentBridge == bridge {
		s.currentBridge = nil
		s.currentEvents = nil
	}
	s.eventsMu.Unlock()
}

// wrappedExecutor routes an HTTP MCP call through the active provider stream so
// the engine remains the sole owner of tool execution and event emission.
func (s *cliToolBridgeState) wrappedExecutor(formatOutput func(ToolOutput) string) mcphttp.ToolExecutor {
	return func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		s.eventsMu.Lock()
		bridge := s.currentBridge
		events := s.currentEvents
		s.eventsMu.Unlock()

		if bridge == nil || events == nil {
			return "", fmt.Errorf("tool execution rejected: no active stream bridge for tool call %q", name)
		}

		callID := fmt.Sprintf("mcp-%s-%d", name, mcpCallCounter.Add(1))
		responseChan := make(chan ToolExecutionResponse, 1)
		req := cliToolRequest{
			ctx:      ctx,
			callID:   callID,
			name:     name,
			args:     args,
			response: responseChan,
			ack:      make(chan error, 1),
		}

		select {
		case bridge.toolReqCh <- req:
		case <-bridge.done:
			return "", fmt.Errorf("tool execution rejected: stream closed during tool call %q", name)
		case <-ctx.Done():
			return "", ctx.Err()
		}

		select {
		case err := <-req.ack:
			if err != nil {
				return "", err
			}
		case <-bridge.done:
			return "", fmt.Errorf("tool execution rejected: stream closed during tool call %q", name)
		case <-ctx.Done():
			return "", ctx.Err()
		}

		select {
		case response := <-responseChan:
			if formatOutput == nil {
				return response.Result.Content, response.Err
			}
			return formatOutput(response.Result), response.Err
		case <-bridge.done:
			return "", fmt.Errorf("tool execution rejected: stream closed during tool call %q", name)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func handleCLIToolRequest(req cliToolRequest, send eventSender) {
	event := Event{
		Type:         EventToolCall,
		ToolCallID:   req.callID,
		ToolName:     req.name,
		Tool:         &ToolCall{ID: req.callID, Name: req.name, Arguments: req.args},
		ToolResponse: req.response,
	}
	if err := send.Send(event); err != nil {
		req.ack <- err
		return
	}
	req.ack <- nil
}

func loadCLIToolLineDrainGrace(envName string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(envName))
	if v == "" {
		return fallback
	}
	ms, err := time.ParseDuration(v + "ms")
	if err != nil || ms < 0 {
		return fallback
	}
	return ms
}

// drainCLILinesWithGrace preserves stdout/tool-call ordering when the MCP call
// reaches the HTTP server just before its preceding streamed text reaches the
// stdout scanner.
func drainCLILinesWithGrace(ctx context.Context, lineCh <-chan string, grace time.Duration, handleLine func(string) error) error {
	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				return nil
			}
			if err := handleLine(line); err != nil {
				return err
			}
		default:
			goto wait
		}
	}

wait:
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				return nil
			}
			if err := handleLine(line); err != nil {
				return err
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(grace)
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// tempFileTracker owns files materialized for a single provider turn. It is
// safe for idempotent cleanup from both the stream defer and engine wrapper.
type tempFileTracker struct {
	tempFiles   []string
	tempFilesMu sync.Mutex
	activeRuns  atomic.Int32
	logName     string
}

func (t *tempFileTracker) trackTempFile(path string) string {
	if path == "" {
		return ""
	}
	t.tempFilesMu.Lock()
	t.tempFiles = append(t.tempFiles, path)
	t.tempFilesMu.Unlock()
	return path
}

func (t *tempFileTracker) imageDataToTempFile(mediaType, base64Data string) string {
	raw, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return ""
	}
	ext := mediaTypeToExt(mediaType)
	f, err := os.CreateTemp("", "term-llm-img-*."+ext)
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		_ = os.Remove(f.Name())
		return ""
	}
	return t.trackTempFile(f.Name())
}

func (t *tempFileTracker) finishStreamCleanup() {
	if t.activeRuns.Add(-1) == 0 {
		t.cleanupTempFiles()
	}
}

func (t *tempFileTracker) cleanupTempFilesIfIdle() {
	if t.activeRuns.Load() == 0 {
		t.cleanupTempFiles()
	}
}

func (t *tempFileTracker) cleanupTempFiles() {
	t.tempFilesMu.Lock()
	paths := t.tempFiles
	t.tempFiles = nil
	t.tempFilesMu.Unlock()

	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			name := t.logName
			if name == "" {
				name = "CLI provider"
			}
			slog.Warn(name+" failed to remove temp file", "path", path, "err", err)
		}
	}
}

func firstUsefulCLIDiagnosticLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "{") {
			continue
		}
		return truncateOneLine(line, 240)
	}
	return ""
}

func recordCLITailLine(mu *sync.Mutex, tail *[]string, line string, maxLines int) {
	line, _ = truncateCLIDiagnosticString(line, cliDiagnosticLineMaxBytes)
	mu.Lock()
	defer mu.Unlock()
	*tail = append(*tail, line)
	if len(*tail) > maxLines {
		*tail = (*tail)[len(*tail)-maxLines:]
	}
}

func snapshotCLITail(mu *sync.Mutex, tail []string) []string {
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), tail...)
}

func normalizeCLITail(tail []string) []string {
	out := make([]string, len(tail))
	for i, line := range tail {
		out[i], _ = truncateCLIDiagnosticString(line, cliDiagnosticLineMaxBytes)
	}
	return out
}

func truncateCLIDiagnosticString(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, false
	}
	return s[:maxBytes] + fmt.Sprintf("\n...[truncated %d bytes]", len(s)-maxBytes), true
}

// extractSystemPrompt joins all system-role text into the native system prompt.
func extractSystemPrompt(messages []Message) string {
	var systemParts []string
	for _, msg := range messages {
		if msg.Role == RoleSystem {
			systemParts = append(systemParts, collectTextParts(msg.Parts))
		}
	}
	return strings.TrimSpace(strings.Join(systemParts, "\n\n"))
}

const cliBinResumeReplayInstruction = "The block above is the earlier part of this same conversation, included only so you have the full context. You have already written those assistant replies and already performed any actions shown there — do not repeat them and do not re-answer earlier user messages. Continue the conversation naturally, responding only to the user's most recent message below."

func buildCLIConversationPrompt(messages []Message, render func([]Message) []string) string {
	finalTurnStart := conversationFinalTurnStart(messages)
	if finalTurnStart <= 0 || !messagesContainPriorAssistantTurn(messages[:finalTurnStart]) {
		return strings.TrimSpace(strings.Join(render(messages), "\n\n"))
	}

	historyText := strings.TrimSpace(strings.Join(render(messages[:finalTurnStart]), "\n\n"))
	finalText := strings.TrimSpace(strings.Join(render(messages[finalTurnStart:]), "\n\n"))
	switch {
	case historyText == "":
		return finalText
	case finalText == "":
		return historyText
	}

	return "<conversation_history>\n" + historyText + "\n</conversation_history>\n\n" + cliBinResumeReplayInstruction + "\n\n" + finalText
}

// conversationFinalTurnStart returns the index where the latest user turn
// begins, including an immediately preceding developer message.
func conversationFinalTurnStart(messages []Message) int {
	lastUser := -1
	for i, msg := range messages {
		if msg.Role == RoleUser {
			lastUser = i
		}
	}
	if lastUser <= 0 {
		return 0
	}
	if messages[lastUser-1].Role == RoleDeveloper {
		return lastUser - 1
	}
	return lastUser
}

func messagesContainPriorAssistantTurn(messages []Message) bool {
	for _, msg := range messages {
		if msg.Role == RoleAssistant {
			return true
		}
	}
	return false
}

func mcpToolSpecs(tools []ToolSpec) []mcphttp.ToolSpec {
	out := make([]mcphttp.ToolSpec, len(tools))
	for i, tool := range tools {
		out[i] = mcphttp.ToolSpec{Name: tool.Name, Description: tool.Description, Schema: tool.Schema}
	}
	return out
}

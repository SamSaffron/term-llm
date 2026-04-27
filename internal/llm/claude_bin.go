package llm

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/mcphttp"
)

// claudeStderrTailMaxLines caps the number of trailing stderr lines we retain
// from the claude CLI subprocess for inclusion in error logs. Older lines are
// discarded so memory stays bounded even if the CLI is chatty.
const claudeStderrTailMaxLines = 40

// claudeStdoutTailMaxLines caps trailing stdout lines captured for failed
// claude CLI runs. These are the raw stream-json lines before term-llm parsing.
const claudeStdoutTailMaxLines = 40

// claudeDiagnosticLineMaxBytes caps any single captured stdout/stderr line so
// a single huge JSON event cannot make the debug log unparsable.
const claudeDiagnosticLineMaxBytes = 4 * 1024

// claudeDiagnosticStdinMaxBytes is the maximum stdin payload embedded directly
// in a failure event. The full length and SHA-256 are always logged.
const claudeDiagnosticStdinMaxBytes = 128 * 1024

// mcpCallCounter generates unique IDs for MCP tool calls
var mcpCallCounter atomic.Int64

// getEuid returns the effective user ID. Overridable in tests.
var getEuid = os.Geteuid

// ClaudeBinProvider implements Provider using the claude CLI binary.
// This provider shells out to the claude command for inference,
// using Claude Code's existing authentication.
//
// Note: This provider is NOT safe for concurrent use. Each Stream() call
// modifies shared state (sessionID, messagesSent). Create separate instances
// for concurrent streams.
type ClaudeBinProvider struct {
	model        string
	effort       string // reasoning effort for opus: "low", "medium", "high", "max", or ""
	sessionID    string // For session continuity with --resume
	messagesSent int    // Track messages already in session to avoid re-sending
	toolExecutor mcphttp.ToolExecutor
	preferOAuth  bool              // If true, clear ANTHROPIC_API_KEY to force OAuth auth
	extraEnv     map[string]string // Extra subprocess env vars from provider config
	enableHooks  bool              // Opt in to Claude Code hooks (disabled by default)

	// Persistent MCP server for multi-turn conversations.
	// The server is kept alive across turns so Claude CLI can maintain
	// its connection to the same URL/token throughout the session.
	mcpServer     *mcphttp.Server
	mcpConfigPath string

	// currentEvents holds the events channel for the current turn.
	// currentBridge is updated at the start of each turn so the MCP executor
	// can route tool execution requests to the correct active stream.
	currentBridge *claudeTurnBridge
	// currentEvents is kept for fallback/direct execution paths.
	currentEvents chan<- Event
	eventsMu      sync.Mutex

	// tempFiles tracks image files materialised for Claude CLI prompts/tool results
	// so they can be removed when the current turn finishes.
	tempFiles   []string
	tempFilesMu sync.Mutex
}

// ClaudeCommandError carries enough diagnostics for debug logs to reproduce or
// inspect a failed claude CLI invocation without dumping the whole process
// environment (which may contain secrets).
type ClaudeCommandError struct {
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
	StdoutTail     string
	StderrTail     string
}

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

func (e *ClaudeCommandError) Error() string {
	if e == nil {
		return "claude command failed"
	}
	msg := fmt.Sprintf("claude command failed (exit %d): %v", e.ExitCode, e.Err)
	if e.StderrTail != "" {
		msg += "\nstderr:\n" + e.StderrTail
	}
	return msg
}

func (e *ClaudeCommandError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// DebugFields is consumed by DebugLogger when this error is emitted as an
// EventError. Keep field values JSON-friendly.
func (e *ClaudeCommandError) DebugFields() map[string]any {
	if e == nil {
		return nil
	}
	fields := map[string]any{
		"provider_error_type": "claude_cli_command",
		"exit_code":           e.ExitCode,
		"command":             "claude",
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

type claudeToolRequest struct {
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

type claudeTurnBridge struct {
	// toolReqCh routes wrapped MCP tool requests through the active turn dispatcher,
	// ensuring deterministic ordering relative to streamed stdout lines.
	toolReqCh chan claudeToolRequest
	// done closes when the active runClaudeCommand turn exits.
	done chan struct{}
}

const (
	claudeToolLineDrainGraceDefault = 75 * time.Millisecond
	claudeToolLineDrainGraceEnv     = "TERM_LLM_CLAUDE_TOOL_LINE_GRACE_MS"
)

var claudeToolLineDrainGrace = loadClaudeToolLineDrainGrace()

func loadClaudeToolLineDrainGrace() time.Duration {
	v := strings.TrimSpace(os.Getenv(claudeToolLineDrainGraceEnv))
	if v == "" {
		return claudeToolLineDrainGraceDefault
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms < 0 {
		return claudeToolLineDrainGraceDefault
	}
	return time.Duration(ms) * time.Millisecond
}

// claudeEffortLevels lists the reasoning-effort suffixes recognised on
// opus/sonnet model names (e.g. "opus-max", "sonnet-high"). "max" and
// "xhigh" are accepted only on opus; see parseClaudeEffort.
var claudeEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// parseClaudeEffort extracts effort suffix from opus or sonnet model names.
// "opus-max" -> ("opus", "max"), "opus-low" -> ("opus", "low")
// "sonnet-high" -> ("sonnet", "high"), "sonnet-low" -> ("sonnet", "low")
// "haiku" -> ("haiku", "") — non-opus/sonnet models are not modified.
// Note: "max" and "xhigh" efforts are only supported for opus.
func parseClaudeEffort(model string) (string, string) {
	isOpus := strings.HasPrefix(model, "opus")
	isSonnet := strings.HasPrefix(model, "sonnet")
	if !isOpus && !isSonnet {
		return model, ""
	}
	efforts := []string{"medium", "high", "low"}
	if isOpus {
		efforts = append(efforts, "max", "xhigh")
	}
	for _, effort := range efforts {
		suffix := "-" + effort
		if strings.HasSuffix(model, suffix) {
			return strings.TrimSuffix(model, suffix), effort
		}
	}
	return model, ""
}

// ValidateClaudeBinModel rejects model strings that are bare effort levels
// (e.g. "claude-bin:max"). Without this check the effort would be silently
// treated as the model name and CLAUDE_CODE_EFFORT_LEVEL would never be set.
func ValidateClaudeBinModel(model string) error {
	for _, effort := range claudeEffortLevels {
		if model == effort {
			return fmt.Errorf(
				"claude-bin model %q is an effort level, not a model; "+
					"did you mean \"claude-bin:opus-%s\"? "+
					"(max/xhigh require opus; low/medium/high also work with sonnet)",
				model, model,
			)
		}
	}
	return nil
}

// NewClaudeBinProvider creates a new provider that uses the claude binary.
func NewClaudeBinProvider(model string, env map[string]string) *ClaudeBinProvider {
	actualModel, effort := parseClaudeEffort(model)
	provider := &ClaudeBinProvider{
		model:       actualModel,
		effort:      effort,
		preferOAuth: true, // Default to OAuth to avoid API key limits
	}
	if len(env) > 0 {
		provider.extraEnv = make(map[string]string, len(env))
		for k, v := range env {
			provider.extraEnv[k] = v
		}
	}
	return provider
}

// SetPreferOAuth controls whether to prefer OAuth auth over API key.
// When true (default), clears ANTHROPIC_API_KEY for the subprocess
// so Claude CLI uses OAuth subscription auth instead.
func (p *ClaudeBinProvider) SetPreferOAuth(prefer bool) {
	p.preferOAuth = prefer
}

// SetEnableHooks controls whether Claude Code hooks are allowed to run.
// Hooks are disabled by default so term-llm sessions don't inherit user-defined
// Claude Code automation unexpectedly.
func (p *ClaudeBinProvider) SetEnableHooks(enable bool) {
	p.enableHooks = enable
}

// SetEnv configures extra environment variables for the Claude CLI subprocess.
func (p *ClaudeBinProvider) SetEnv(env map[string]string) {
	if len(env) == 0 {
		p.extraEnv = nil
		return
	}
	p.extraEnv = make(map[string]string, len(env))
	for k, v := range env {
		p.extraEnv[k] = v
	}
}

// SetToolExecutor sets the function used to execute tools.
// This must be called before Stream() if tools are needed.
// Note: The signature uses an anonymous function type (not mcphttp.ToolExecutor)
// to satisfy the ToolExecutorSetter interface in engine.go.
func (p *ClaudeBinProvider) SetToolExecutor(executor func(ctx context.Context, name string, args json.RawMessage) (ToolOutput, error)) {
	// Wrap the ToolOutput executor to satisfy the mcphttp.ToolExecutor (string, error) interface.
	// For tool outputs with image data, materialise images to temp files and include their
	// paths in the response text so Claude CLI can read them natively as vision inputs.
	p.toolExecutor = func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		output, err := executor(ctx, name, args)
		return p.formatToolOutputForClaude(output), err
	}
}

// ResetConversation clears Claude CLI resume state so the next turn starts a
// fresh conversation instead of resuming the previous CLI session.
func (p *ClaudeBinProvider) ResetConversation() {
	p.sessionID = ""
	p.messagesSent = 0
}

func (p *ClaudeBinProvider) Name() string {
	model := p.model
	if model == "" {
		model = "sonnet"
	}
	if p.effort != "" {
		return fmt.Sprintf("Claude CLI (%s, effort=%s)", model, p.effort)
	}
	return fmt.Sprintf("Claude CLI (%s)", model)
}

func (p *ClaudeBinProvider) Credential() string {
	return "claude-bin"
}

func (p *ClaudeBinProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch:    false, // Use term-llm's external tools instead
		NativeWebFetch:     false,
		ToolCalls:          true,
		SupportsToolChoice: false, // Claude CLI doesn't support forcing specific tool use
		ManagesOwnContext:  true,  // Claude CLI handles its own context window management
	}
}

func (p *ClaudeBinProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		defer p.cleanupTempFiles()

		// Build the command arguments, passing events channel for tool execution routing.
		// MCP server is kept alive across turns - caller should call CleanupMCP() when done.
		args, effort := p.buildArgs(ctx, req, send)

		systemPrompt := p.systemPromptForTurn(req.Messages)

		// When resuming a session, only send new messages (claude CLI has the rest)
		messagesToSend := req.Messages
		if p.sessionID != "" && p.messagesSent > 0 && p.messagesSent < len(req.Messages) {
			messagesToSend = req.Messages[p.messagesSent:]
		}

		// Build the conversation prompt from messages to send.
		// Use stream-json format when images are present so the model can vision-analyze them.
		useStreamJson := hasImages(messagesToSend)
		if useStreamJson && strings.TrimSpace(p.buildStreamJsonInput(messagesToSend, p.sessionID)) == "" {
			slog.Warn("claude-bin stream-json input was empty despite image detection; falling back to text prompt")
			useStreamJson = false
		}
		// buildPrompt produces the full stdin payload for a set of messages.
		// For text mode it prepends the system prompt so retries keep it too.
		// For stream-json mode the system prompt goes on argv instead.
		buildPrompt := func(msgs []Message) string {
			if useStreamJson {
				return p.buildStreamJsonInput(msgs, p.sessionID)
			}
			return p.combinePrompt(systemPrompt, p.buildConversationPrompt(msgs))
		}
		if useStreamJson {
			args = append(args, "--input-format", "stream-json")
			if systemPrompt != "" {
				args = append(args, "--system-prompt", systemPrompt)
			}
		}
		userPrompt := buildPrompt(messagesToSend)

		debug := req.Debug || req.DebugRaw

		err := p.runClaudeCommand(ctx, args, effort, userPrompt, debug, send)
		if err != nil && isPromptTooLong(err) {
			// Retry with progressively more aggressive truncation
			retryLimits := []int{maxToolResultCharsOnRetry, maxToolResultCharsOnAggressiveRetry}
			prevLen := len(userPrompt)
			for _, limit := range retryLimits {
				truncated := truncateToolResultsAt(messagesToSend, limit)
				retryPrompt := buildPrompt(truncated)
				if len(retryPrompt) >= prevLen {
					slog.Warn("prompt too long but truncation did not reduce size, not retrying",
						"limit", limit)
					break
				}
				slog.Info("prompt too long, retrying with truncated tool results",
					"original_len", prevLen, "truncated_len", len(retryPrompt), "limit", limit)
				prevLen = len(retryPrompt)
				err = p.runClaudeCommand(ctx, args, effort, retryPrompt, debug, send)
				if err == nil || !isPromptTooLong(err) {
					break
				}
			}
		}
		if err != nil {
			return err
		}

		// Track messages sent so we don't re-send them on resume
		p.messagesSent = len(req.Messages)

		return send.Send(Event{Type: EventDone})
	}), nil
}

func (p *ClaudeBinProvider) buildCommandEnv(effort string) []string {
	runningAsRoot := getEuid() == 0
	env := os.Environ()
	filtered := env[:0]
	for _, e := range env {
		key := e
		if idx := strings.IndexByte(e, '='); idx >= 0 {
			key = e[:idx]
		}
		if p.preferOAuth && key == "ANTHROPIC_API_KEY" {
			continue
		}
		if effort != "" && key == "CLAUDE_CODE_EFFORT_LEVEL" {
			continue
		}
		// Claude Code refuses bypassPermissions as root unless it sees an
		// explicit sandbox marker. term-llm owns tool execution, so force the
		// inert IS_SANDBOX marker below rather than inheriting a stale value.
		if runningAsRoot && key == "IS_SANDBOX" {
			continue
		}
		if len(p.extraEnv) > 0 {
			if _, ok := p.extraEnv[key]; ok {
				continue
			}
		}
		filtered = append(filtered, e)
	}
	if effort != "" {
		filtered = append(filtered, "CLAUDE_CODE_EFFORT_LEVEL="+effort)
	}
	for k, v := range p.extraEnv {
		if p.preferOAuth && k == "ANTHROPIC_API_KEY" {
			continue
		}
		if effort != "" && k == "CLAUDE_CODE_EFFORT_LEVEL" {
			continue
		}
		if runningAsRoot && k == "IS_SANDBOX" {
			continue
		}
		filtered = append(filtered, k+"="+v)
	}
	if runningAsRoot && !envHasTruthy(filtered, "CLAUDE_CODE_BUBBLEWRAP") {
		filtered = append(filtered, "IS_SANDBOX=1")
	}
	return filtered
}

func envHasTruthy(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			v := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(e, prefix)))
			return v != "" && v != "0" && v != "false"
		}
	}
	return false
}

func (p *ClaudeBinProvider) newClaudeCommandError(cmdErr error, exitCode int, args []string, effort, userPrompt string, toolsExecuted bool, stdoutTail, stderrTail []string) *ClaudeCommandError {
	cwd, _ := os.Getwd()
	stdin, stdinTruncated := truncateClaudeDiagnosticString(userPrompt, claudeDiagnosticStdinMaxBytes)
	sum := sha256.Sum256([]byte(userPrompt))
	env, removedEnv := p.commandEnvDebugFields(effort)
	return &ClaudeCommandError{
		ExitCode:       exitCode,
		Err:            cmdErr,
		Args:           append([]string(nil), args...),
		CommandLine:    shellJoin(append([]string{"claude"}, args...)),
		Cwd:            cwd,
		Effort:         effort,
		ToolsExecuted:  toolsExecuted,
		PreferOAuth:    p.preferOAuth,
		Env:            env,
		RemovedEnv:     removedEnv,
		Stdin:          stdin,
		StdinLen:       len(userPrompt),
		StdinSHA256:    hex.EncodeToString(sum[:]),
		StdinTruncated: stdinTruncated,
		StdoutTail:     strings.Join(normalizeClaudeTail(stdoutTail), "\n"),
		StderrTail:     strings.Join(normalizeClaudeTail(stderrTail), "\n"),
	}
}

func (p *ClaudeBinProvider) userFacingClaudeCommandError(err *ClaudeCommandError) error {
	if err == nil {
		return nil
	}
	detail := firstUsefulClaudeDiagnosticLine(err.StderrTail)
	if detail == "" {
		detail = firstUsefulClaudeDiagnosticLine(err.StdoutTail)
	}
	if detail == "" && err.Err != nil {
		detail = err.Err.Error()
	}

	combined := strings.ToLower(strings.TrimSpace(err.StderrTail + "\n" + err.StdoutTail))
	summary := fmt.Sprintf("Claude Code exited before completing the turn (exit %d)", err.ExitCode)
	switch {
	case strings.Contains(combined, "cannot be used with root/sudo privileges"):
		summary = "Claude Code refused permission bypass while running as root"
		if detail == "" {
			detail = "term-llm should set IS_SANDBOX=1 for root claude-bin runs"
		}
	case strings.Contains(combined, "not logged in") || strings.Contains(combined, "please run /login"):
		summary = "Claude Code is not logged in"
	case strings.Contains(combined, "bypasspermissions") &&
		(strings.Contains(combined, "policy") || strings.Contains(combined, "managed") || strings.Contains(combined, "disabled")):
		summary = "Claude Code managed policy blocked permission bypass"
	case strings.Contains(combined, "permission") &&
		(strings.Contains(combined, "denied") || strings.Contains(combined, "requires approval") || strings.Contains(combined, "refused")):
		summary = "Claude Code denied a tool call before term-llm could execute it"
	}

	return &UserFacingProviderError{
		Summary: summary,
		Detail:  detail,
		Cause:   err,
	}
}

func firstUsefulClaudeDiagnosticLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "{") {
			continue
		}
		return truncateOneLine(line, 240)
	}
	return ""
}

func truncateOneLine(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "…"
}

func (p *ClaudeBinProvider) commandEnvDebugFields(effort string) (map[string]string, []string) {
	env := make(map[string]string)
	removed := make([]string, 0, 2)
	if p.preferOAuth {
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			removed = append(removed, "ANTHROPIC_API_KEY")
		}
		if _, ok := p.extraEnv["ANTHROPIC_API_KEY"]; ok {
			removed = append(removed, "ANTHROPIC_API_KEY (provider env)")
		}
	}
	if effort != "" {
		env["CLAUDE_CODE_EFFORT_LEVEL"] = effort
	}
	for k, v := range p.extraEnv {
		if p.preferOAuth && k == "ANTHROPIC_API_KEY" {
			continue
		}
		if effort != "" && k == "CLAUDE_CODE_EFFORT_LEVEL" {
			continue
		}
		env[k] = redactEnvValue(k, v)
	}
	if len(env) == 0 {
		env = nil
	}
	if len(removed) == 0 {
		removed = nil
	}
	return env, removed
}

func redactEnvValue(key, value string) string {
	upper := strings.ToUpper(key)
	for _, marker := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL"} {
		if strings.Contains(upper, marker) {
			if value == "" {
				return ""
			}
			return "[redacted]"
		}
	}
	return value
}

func recordClaudeTailLine(mu *sync.Mutex, tail *[]string, line string, maxLines int) {
	line, _ = truncateClaudeDiagnosticString(line, claudeDiagnosticLineMaxBytes)
	mu.Lock()
	defer mu.Unlock()
	*tail = append(*tail, line)
	if len(*tail) > maxLines {
		*tail = (*tail)[len(*tail)-maxLines:]
	}
}

func snapshotClaudeTail(mu *sync.Mutex, tail []string) []string {
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), tail...)
}

func normalizeClaudeTail(tail []string) []string {
	out := make([]string, len(tail))
	for i, line := range tail {
		out[i], _ = truncateClaudeDiagnosticString(line, claudeDiagnosticLineMaxBytes)
	}
	return out
}

func truncateClaudeDiagnosticString(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, false
	}
	return s[:maxBytes] + fmt.Sprintf("\n...[truncated %d bytes]", len(s)-maxBytes), true
}

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_+-./:=,@%", r) {
			continue
		}
		return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
	}
	return s
}

// runClaudeCommand executes the claude CLI binary with the given arguments and prompt,
// parsing its streaming JSON output into events. Returns nil on success.
func (p *ClaudeBinProvider) runClaudeCommand(
	ctx context.Context,
	args []string,
	effort string,
	userPrompt string,
	debug bool,
	send eventSender,
) error {
	// Note: We pass the prompt via stdin instead of command line args
	// to avoid "argument list too long" errors with large tool results (e.g., base64 images)

	if debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Claude CLI Command ===")
		fmt.Fprintf(os.Stderr, "claude %s\n", strings.Join(args, " "))
		fmt.Fprintf(os.Stderr, "Prompt length: %d bytes (via stdin)\n", len(userPrompt))
		if effort != "" {
			fmt.Fprintf(os.Stderr, "CLAUDE_CODE_EFFORT_LEVEL=%s\n", effort)
		} else {
			fmt.Fprintln(os.Stderr, "CLAUDE_CODE_EFFORT_LEVEL=(unset)")
		}
		fmt.Fprintln(os.Stderr, "=================================")
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Env = p.buildCommandEnv(effort)

	// Set up stdin pipe for the prompt
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start claude: %w", err)
	}

	bridge := &claudeTurnBridge{
		toolReqCh: make(chan claudeToolRequest, 64),
		done:      make(chan struct{}),
	}
	p.eventsMu.Lock()
	p.currentBridge = bridge
	p.currentEvents = send.ch
	p.eventsMu.Unlock()
	defer func() {
		p.eventsMu.Lock()
		if p.currentBridge == bridge {
			p.currentBridge = nil
			p.currentEvents = nil
		}
		p.eventsMu.Unlock()
		close(bridge.done)
	}()

	// Capture stderr in a bounded ring buffer so we can include a tail
	// in error logs when claude exits non-zero. Also forward live to our
	// stderr in debug mode.
	var (
		stderrMu   sync.Mutex
		stderrTail []string
	)
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		stderrScanner := bufio.NewScanner(stderr)
		stderrScanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for stderrScanner.Scan() {
			line := stderrScanner.Text()
			if debug {
				fmt.Fprintf(os.Stderr, "[claude stderr] %s\n", line)
			}
			recordClaudeTailLine(&stderrMu, &stderrTail, line, claudeStderrTailMaxLines)
		}
	}()

	// Write prompt to stdin and close
	go func() {
		defer stdin.Close()
		stdin.Write([]byte(userPrompt))
	}()

	lineCh := make(chan string, 256)
	scanErrCh := make(chan error, 1)
	var (
		stdoutMu   sync.Mutex
		stdoutTail []string
	)
	go func() {
		scanner := bufio.NewScanner(stdout)
		// Increase buffer size for large JSON messages
		scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			recordClaudeTailLine(&stdoutMu, &stdoutTail, line, claudeStdoutTailMaxLines)
			if line != "" {
				select {
				case lineCh <- line:
				case <-bridge.done:
					close(lineCh)
					scanErrCh <- nil
					return
				case <-ctx.Done():
					close(lineCh)
					scanErrCh <- ctx.Err()
					return
				}
			}
		}
		close(lineCh)
		scanErrCh <- scanner.Err()
	}()

	lastUsage, toolsExecuted, handledTerminalResult, err := p.dispatchClaudeEvents(ctx, lineCh, bridge.toolReqCh, debug, send)
	if err != nil {
		// Kill the process if dispatch failed (e.g., context cancelled)
		// to avoid orphan processes.
		cmd.Process.Kill()
	}

	// Wait for scanner to finish BEFORE cmd.Wait().
	// Go docs: "It is incorrect to call Wait before all reads from the pipe have completed."
	scanErr := <-scanErrCh

	// Now safe to call Wait() — all pipe reads are done.
	cmdErr := cmd.Wait()

	// Wait for the stderr scanner goroutine to finish so the tail buffer
	// is fully populated before we format error messages or log diagnostics.
	<-stderrDone

	if err != nil {
		return err
	}
	if scanErr != nil {
		return fmt.Errorf("error reading claude output: %w", scanErr)
	}
	if cmdErr != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(cmdErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		stderrSnapshot := snapshotClaudeTail(&stderrMu, stderrTail)
		stdoutSnapshot := snapshotClaudeTail(&stdoutMu, stdoutTail)

		// When MCP tools were executed, the CLI exits with code 1 because
		// --max-turns 1 is exhausted after the tool call. This is expected;
		// the engine's outer loop will re-invoke us with the tool results.
		// Any other non-zero exit (crashes, OOMs, auth failures, etc.) must
		// still surface — don't swallow those just because tools ran.
		expectedToolExit := toolsExecuted && exitCode == 1
		expectedHandledTerminalResult := handledTerminalResult && exitCode == 1
		if !expectedToolExit && !expectedHandledTerminalResult {
			claudeErr := p.newClaudeCommandError(cmdErr, exitCode, args, effort, userPrompt, toolsExecuted, stdoutSnapshot, stderrSnapshot)
			slog.Error("claude command failed",
				"exit_code", exitCode,
				"tools_executed", toolsExecuted,
				"effort", effort,
				"command_line", claudeErr.CommandLine,
				"stdin_len", claudeErr.StdinLen,
				"stdin_sha256", claudeErr.StdinSHA256,
				"stderr_tail", claudeErr.StderrTail,
				"stdout_tail", claudeErr.StdoutTail,
			)
			return p.userFacingClaudeCommandError(claudeErr)
		}
	}

	if lastUsage != nil {
		if err := send.Send(Event{Type: EventUsage, Use: lastUsage}); err != nil {
			return err
		}
	}
	return nil
}

func (p *ClaudeBinProvider) dispatchClaudeEvents(
	ctx context.Context,
	lineCh <-chan string,
	toolReqCh <-chan claudeToolRequest,
	debug bool,
	send eventSender,
) (*Usage, bool, bool, error) {
	var (
		lastUsage             *Usage
		linesOpen             = true
		sawTextDelta          bool
		assistantFallbackText string
		toolsExecuted         bool
		handledTerminalResult bool
	)

	for linesOpen {
		// Process all ready stdout lines first to preserve text/tool ordering.
		hadLine := false
		for linesOpen {
			select {
			case line, ok := <-lineCh:
				if !ok {
					linesOpen = false
					break
				}
				hadLine = true
				if err := p.handleClaudeLine(ctx, line, debug, send, &lastUsage, &sawTextDelta, &assistantFallbackText, &handledTerminalResult); err != nil {
					return nil, false, false, err
				}
			default:
				goto drainDone
			}
		}
	drainDone:
		if hadLine {
			continue
		}

		select {
		case line, ok := <-lineCh:
			if !ok {
				linesOpen = false
				continue
			}
			if err := p.handleClaudeLine(ctx, line, debug, send, &lastUsage, &sawTextDelta, &assistantFallbackText, &handledTerminalResult); err != nil {
				return nil, false, false, err
			}
		case req := <-toolReqCh:
			if err := p.drainClaudeLinesWithGrace(ctx, lineCh, debug, send, &lastUsage, &sawTextDelta, &assistantFallbackText, &handledTerminalResult); err != nil {
				return nil, false, false, err
			}
			toolsExecuted = true
			p.handleClaudeToolRequest(req, send)
		case <-ctx.Done():
			return nil, false, false, ctx.Err()
		}
	}

	// Drain any queued tool requests that arrived before stream shutdown.
	for {
		select {
		case req := <-toolReqCh:
			toolsExecuted = true
			p.handleClaudeToolRequest(req, send)
		default:
			goto drained
		}
	}
drained:

	return lastUsage, toolsExecuted, handledTerminalResult, nil
}

func (p *ClaudeBinProvider) handleClaudeLine(
	ctx context.Context,
	line string,
	debug bool,
	send eventSender,
	lastUsage **Usage,
	sawTextDelta *bool,
	assistantFallbackText *string,
	handledTerminalResult *bool,
) error {
	var baseMsg struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(line), &baseMsg); err != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "Failed to parse JSON: %s\n", line[:min(100, len(line))])
		}
		return nil
	}

	switch baseMsg.Type {
	case "system":
		// Extract session ID for potential resume
		var sysMsg claudeSystemMessage
		if err := json.Unmarshal([]byte(line), &sysMsg); err == nil {
			p.sessionID = sysMsg.SessionID
			if debug {
				fmt.Fprintf(os.Stderr, "Session: %s, Model: %s, Tools: %v\n",
					sysMsg.SessionID, sysMsg.Model, sysMsg.Tools)
			}
		}

	case "stream_event":
		// Handle streaming text deltas
		var streamEvent claudeStreamEvent
		if err := json.Unmarshal([]byte(line), &streamEvent); err != nil {
			return nil
		}
		if streamEvent.Event.Type == "content_block_delta" &&
			streamEvent.Event.Delta.Type == "text_delta" &&
			streamEvent.Event.Delta.Text != "" {
			if err := send.Send(Event{Type: EventTextDelta, Text: streamEvent.Event.Delta.Text}); err != nil {
				return err
			}
			if sawTextDelta != nil {
				*sawTextDelta = true
			}
		}

	case "streamlined_text":
		var streamlinedMsg claudeStreamlinedTextMessage
		if err := json.Unmarshal([]byte(line), &streamlinedMsg); err != nil {
			return nil
		}
		if streamlinedMsg.Text != "" {
			if err := send.Send(Event{Type: EventTextDelta, Text: streamlinedMsg.Text}); err != nil {
				return err
			}
			if sawTextDelta != nil {
				*sawTextDelta = true
			}
		}

	case "assistant":
		// Buffer assistant text as a fallback for providers/versions that
		// don't emit stream_event text deltas.
		var assistantMsg claudeAssistantMessage
		if err := json.Unmarshal([]byte(line), &assistantMsg); err == nil && assistantFallbackText != nil {
			if text := extractClaudeAssistantText(assistantMsg); text != "" {
				*assistantFallbackText = text
			}
		}

	case "result":
		var resultMsg claudeResultMessage
		if err := json.Unmarshal([]byte(line), &resultMsg); err == nil {
			permissionDenied := resultMsg.hasPermissionDenial()
			// Check for API errors (rate limits, auth issues, etc.). Claude Code
			// reports permission denials as terminal result errors too; surface
			// those as model-visible text so the conversation can fail gracefully.
			if resultMsg.IsError && resultMsg.Result != "" && !permissionDenied {
				return fmt.Errorf("claude API error: %s", resultMsg.Result)
			}

			fallbackText := ""
			if permissionDenied {
				fallbackText = resultMsg.permissionDenialText()
			} else if assistantFallbackText != nil {
				fallbackText = strings.TrimSpace(*assistantFallbackText)
			}
			if fallbackText == "" {
				fallbackText = strings.TrimSpace(resultMsg.Result)
			}
			if permissionDenied && fallbackText == "" {
				fallbackText = "Claude Code denied a tool call before term-llm could execute it."
			}
			if permissionDenied && handledTerminalResult != nil {
				*handledTerminalResult = true
			}
			if sawTextDelta != nil && !*sawTextDelta && fallbackText != "" {
				if err := send.Send(Event{Type: EventTextDelta, Text: fallbackText}); err != nil {
					return err
				}
				*sawTextDelta = true
			}
			*lastUsage = &Usage{
				InputTokens:       resultMsg.Usage.InputTokens,
				OutputTokens:      resultMsg.Usage.OutputTokens,
				CachedInputTokens: resultMsg.Usage.CacheReadInputTokens,
			}
		}
	}

	return nil
}

func (p *ClaudeBinProvider) handleClaudeToolRequest(req claudeToolRequest, send eventSender) {
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

func (p *ClaudeBinProvider) drainClaudeLinesWithGrace(
	ctx context.Context,
	lineCh <-chan string,
	debug bool,
	send eventSender,
	lastUsage **Usage,
	sawTextDelta *bool,
	assistantFallbackText *string,
	handledTerminalResult *bool,
) error {
	// First, drain any already-buffered lines.
	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				return nil
			}
			if err := p.handleClaudeLine(ctx, line, debug, send, lastUsage, sawTextDelta, assistantFallbackText, handledTerminalResult); err != nil {
				return err
			}
		default:
			goto wait
		}
	}

wait:
	timer := time.NewTimer(claudeToolLineDrainGrace)
	defer timer.Stop()

	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				return nil
			}
			if err := p.handleClaudeLine(ctx, line, debug, send, lastUsage, sawTextDelta, assistantFallbackText, handledTerminalResult); err != nil {
				return err
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(claudeToolLineDrainGrace)
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// buildArgs constructs the command line arguments for the claude binary.
// The events channel is passed to the MCP server for routing tool execution events.
// The MCP server is kept alive across turns - call CleanupMCP() when the conversation ends.
// Returns the args and the effective reasoning effort (if any).
func (p *ClaudeBinProvider) buildArgs(ctx context.Context, req Request, send eventSender) ([]string, string) {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--include-partial-messages", // Stream text as it arrives
		"--verbose",
		"--strict-mcp-config", // Ignore Claude's configured MCPs
		// Ignore user/project/local settings so Claude Code cannot apply its own
		// permission rules or hooks. flagSettings (--settings below) and managed
		// policy settings are still loaded by Claude Code.
		"--setting-sources", "",
	}
	if getEuid() != 0 {
		args = append(args, "--dangerously-skip-permissions")
	} else {
		// Claude Code rejects --dangerously-skip-permissions when running as root.
		// Use the equivalent permission mode explicitly so claude-bin remains
		// non-interactive in rootful containers too.
		args = append(args, "--permission-mode", "bypassPermissions")
	}
	if !p.enableHooks {
		args = append(args, "--settings", `{"disableAllHooks":true}`)
	}

	// Always limit to 1 turn - term-llm handles tool execution loop
	args = append(args, "--max-turns", "1")

	// Effort precedence: req.ReasoningEffort wins over model suffix, which wins over provider-level effort.
	model := chooseModel(req.Model, p.model)
	strippedModel, reqEffort := parseClaudeEffort(model)
	effort := p.effort
	if reqEffort != "" {
		effort = reqEffort
	}
	if v := strings.TrimSpace(req.ReasoningEffort); v != "" {
		effort = v
	}
	if strippedModel != "" {
		args = append(args, "--model", mapModelToClaudeArg(strippedModel))
	}

	// Disable all built-in tools - we use MCP for custom tools
	args = append(args, "--tools", "")

	// If we have tools and a tool executor, use persistent MCP server
	debug := req.Debug || req.DebugRaw
	if len(req.Tools) > 0 {
		if p.toolExecutor == nil {
			slog.Warn("tools requested but no tool executor configured", "tool_count", len(req.Tools))
		} else {
			// Reuse existing MCP server if available, otherwise create new one
			mcpConfig := p.getOrCreateMCPConfig(ctx, req.Tools, debug)
			if mcpConfig != "" {
				args = append(args, "--mcp-config", mcpConfig)
			} else if debug {
				fmt.Fprintf(os.Stderr, "[claude-bin] ERROR: MCP config creation failed\n")
			}
		}
	}

	// Session resume for multi-turn conversations
	if p.sessionID != "" {
		args = append(args, "--resume", p.sessionID)
	}

	return args, effort
}

// getOrCreateMCPConfig returns the MCP config path, reusing existing server if available.
// This ensures the MCP server URL/token stays constant across turns in a multi-turn conversation.
func (p *ClaudeBinProvider) getOrCreateMCPConfig(ctx context.Context, tools []ToolSpec, debug bool) string {

	// If we already have a running MCP server, reuse its config
	if p.mcpServer != nil && p.mcpConfigPath != "" {
		if debug {
			fmt.Fprintf(os.Stderr, "[claude-bin] Reusing existing MCP server at %s\n", p.mcpServer.URL())
		}
		return p.mcpConfigPath
	}

	// Create new MCP server
	if debug {
		fmt.Fprintf(os.Stderr, "[claude-bin] Starting HTTP MCP server for %d tools\n", len(tools))
	}

	configPath := p.createHTTPMCPConfig(ctx, tools, debug)
	if configPath != "" && debug {
		fmt.Fprintf(os.Stderr, "[claude-bin] MCP config created: %s\n", configPath)
	}
	return configPath
}

// CleanupMCP stops the MCP server and removes the config file.
// This should be called when the conversation is complete (runtime eviction
// or server shutdown) — NOT per turn, because the MCP server is deliberately
// kept alive across turns so Claude CLI can reuse the same URL/token.
// Also removes any remaining tracked temp files as a safety net in case
// CleanupTurn was not invoked (e.g. mid-turn abort before stream terminates).
func (p *ClaudeBinProvider) CleanupMCP() {
	if p.mcpServer != nil {
		p.mcpServer.Stop(context.Background())
		p.mcpServer = nil
	}
	if p.mcpConfigPath != "" {
		os.Remove(p.mcpConfigPath)
		p.mcpConfigPath = ""
	}
	p.cleanupTempFiles()
}

// CleanupTurn removes per-turn resources (currently: tracked temp image
// files). Safe to call multiple times. Invoked by the engine stream wrapper
// on stream termination; also runs via defer inside Stream() so it is
// guaranteed even if the consumer drops the stream.
func (p *ClaudeBinProvider) CleanupTurn() {
	p.cleanupTempFiles()
}

// createHTTPMCPConfig starts an HTTP MCP server and creates a config file pointing to it.
// The server and config path are stored in the provider for reuse across turns.
// Tool execution events are routed through p.currentEvents (set by getOrCreateMCPConfig).
func (p *ClaudeBinProvider) createHTTPMCPConfig(ctx context.Context, tools []ToolSpec, debug bool) string {
	// Convert llm.ToolSpec to mcphttp.ToolSpec
	mcpTools := make([]mcphttp.ToolSpec, len(tools))
	for i, t := range tools {
		mcpTools[i] = mcphttp.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Schema:      t.Schema,
		}
		if debug {
			fmt.Fprintf(os.Stderr, "[claude-bin] Registering tool: %s\n", t.Name)
		}
	}

	// Create a wrapper executor that routes tool calls through the engine
	// by emitting EventToolCall with a response channel and waiting for the result.
	// NOTE: We read p.currentBridge/currentEvents under mutex each time to get the
	// current turn's stream sink. This is critical because the MCP server persists
	// across turns but the stream channels change every turn.
	wrappedExecutor := func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		// Get the current bridge/events for this turn.
		p.eventsMu.Lock()
		bridge := p.currentBridge
		events := p.currentEvents
		p.eventsMu.Unlock()

		// If no active stream bridge/events channel, reject execution.
		// Falling back to direct execution here would bypass stream-level sequencing
		// and could skip expected UI/event handling semantics.
		if bridge == nil || events == nil {
			return "", fmt.Errorf("tool execution rejected: no active stream bridge for tool call %q", name)
		}

		// Generate a unique call ID for this execution
		callID := fmt.Sprintf("mcp-%s-%d", name, mcpCallCounter.Add(1))

		// Create response channel for synchronous execution
		responseChan := make(chan ToolExecutionResponse, 1)

		req := claudeToolRequest{
			ctx:      ctx,
			callID:   callID,
			name:     name,
			args:     args,
			response: responseChan,
			ack:      make(chan error, 1),
		}

		// Route tool request through the turn bridge so ordering is handled centrally.
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

		// Wait for engine to execute and return result
		select {
		case response := <-responseChan:
			return p.formatToolOutputForClaude(response.Result), response.Err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	// Create and start HTTP server
	server := mcphttp.NewServer(wrappedExecutor)
	server.SetDebug(debug)
	url, token, err := server.Start(ctx, mcpTools)
	if err != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "[claude-bin] Failed to start MCP server: %v\n", err)
		}
		return ""
	}
	if debug {
		fmt.Fprintf(os.Stderr, "[claude-bin] MCP server started at %s\n", url)
	}

	// Create MCP config with HTTP URL
	// Note: "type": "http" is required for Claude Code to use HTTP transport
	mcpConfig := map[string]any{
		"mcpServers": map[string]any{
			"term-llm": map[string]any{
				"type": "http",
				"url":  url,
				"headers": map[string]string{
					"Authorization": "Bearer " + token,
				},
			},
		},
	}

	configJSON, err := json.Marshal(mcpConfig)
	if err != nil {
		server.Stop(ctx)
		return ""
	}

	// Write to temp file using os.CreateTemp to avoid symlink attacks
	tmpFile, err := os.CreateTemp("", "term-llm-mcp-*.json")
	if err != nil {
		server.Stop(ctx)
		return ""
	}
	configPath := tmpFile.Name()
	if _, err := tmpFile.Write(configJSON); err != nil {
		tmpFile.Close()
		os.Remove(configPath)
		server.Stop(ctx)
		return ""
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(configPath)
		server.Stop(ctx)
		return ""
	}

	// Store server and config for reuse across turns
	p.mcpServer = server
	p.mcpConfigPath = configPath

	return configPath
}

// extractSystemPrompt joins all RoleSystem message parts into a single string.
func (p *ClaudeBinProvider) extractSystemPrompt(messages []Message) string {
	var systemParts []string
	for _, msg := range messages {
		if msg.Role == RoleSystem {
			systemParts = append(systemParts, collectTextParts(msg.Parts))
		}
	}
	return strings.TrimSpace(strings.Join(systemParts, "\n\n"))
}

// systemPromptForTurn returns the system prompt to pass to Claude CLI for this
// turn. On resume (sessionID set) it returns "" because Claude CLI already
// holds the original system prompt in the session; re-sending it would
// duplicate it in text mode and override Claude's own system (defeating
// template expansion) in stream-json mode.
func (p *ClaudeBinProvider) systemPromptForTurn(messages []Message) string {
	if p.sessionID != "" {
		return ""
	}
	return p.extractSystemPrompt(messages)
}

// buildConversationPrompt constructs the conversation prompt from messages.
// This can be called with a subset of messages when resuming a session.
func (p *ClaudeBinProvider) buildConversationPrompt(messages []Message) string {
	var conversationParts []string
	var pendingDev string

	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			// System messages handled separately by extractSystemPrompt
			continue
		case RoleDeveloper:
			// Claude CLI has no native developer role. Buffer the text and prepend
			// it into the next user turn wrapped in <developer> tags.
			pendingDev = collectTextParts(msg.Parts)
		case RoleUser:
			var userParts []string
			if pendingDev != "" {
				userParts = append(userParts, fmt.Sprintf("<developer>\n%s\n</developer>", pendingDev))
				pendingDev = ""
			}
			for _, part := range msg.Parts {
				switch part.Type {
				case PartText:
					if part.Text != "" {
						userParts = append(userParts, part.Text)
					}
				case PartImage:
					// Prefer an existing filesystem path; otherwise materialise the
					// base64 data into a temp file so Claude CLI can read the image.
					path := part.ImagePath
					if path == "" && part.ImageData != nil && part.ImageData.Base64 != "" {
						path = p.imageDataToTempFile(part.ImageData.MediaType, part.ImageData.Base64)
					}
					if path != "" {
						userParts = append(userParts, path)
					}
				}
			}
			text := strings.Join(userParts, "\n")
			if text != "" {
				conversationParts = append(conversationParts, "User: "+text)
			}
		case RoleAssistant:
			text := collectTextParts(msg.Parts)
			// Also capture tool calls from assistant
			for _, part := range msg.Parts {
				if part.Type == PartToolCall && part.ToolCall != nil {
					conversationParts = append(conversationParts,
						fmt.Sprintf("Assistant called tool: %s", part.ToolCall.Name))
				}
			}
			if text != "" {
				conversationParts = append(conversationParts, "Assistant: "+text)
			}
		case RoleTool:
			// Format tool results
			for _, part := range msg.Parts {
				if part.Type == PartToolResult && part.ToolResult != nil {
					// Process content to keep prompts compact for image tool results.
					content := p.processToolResultContent(part.ToolResult)
					conversationParts = append(conversationParts,
						fmt.Sprintf("Tool result (%s): %s", part.ToolResult.Name, content))
				}
			}
		}
	}

	return strings.TrimSpace(strings.Join(conversationParts, "\n\n"))
}

func (p *ClaudeBinProvider) combinePrompt(systemPrompt, conversationPrompt string) string {
	systemPrompt = strings.TrimSpace(systemPrompt)
	conversationPrompt = strings.TrimSpace(conversationPrompt)

	switch {
	case systemPrompt == "":
		return conversationPrompt
	case conversationPrompt == "":
		return "System: " + systemPrompt
	default:
		return fmt.Sprintf("System: %s\n\n%s", systemPrompt, conversationPrompt)
	}
}

// mapModelToClaudeArg converts a model name to claude CLI argument.
func mapModelToClaudeArg(model string) string {
	model = strings.ToLower(model)
	if strings.Contains(model, "opus") {
		return "opus"
	}
	if strings.Contains(model, "haiku") {
		return "haiku"
	}
	// Default to sonnet
	return "sonnet"
}

// mapClaudeToolName converts claude tool names back to term-llm names.
// MCP tools are namespaced as mcp__term-llm__<tool>.
func mapClaudeToolName(claudeName string) string {
	if strings.HasPrefix(claudeName, "mcp__term-llm__") {
		return strings.TrimPrefix(claudeName, "mcp__term-llm__")
	}
	return claudeName
}

// processToolResultContent formats tool result content for Claude CLI prompts.
// Image data parts are written to temp files and their paths included inline so
// Claude CLI can read them natively rather than receiving truncated base64.
func (p *ClaudeBinProvider) processToolResultContent(result *ToolResult) string {
	if result == nil {
		return ""
	}
	if !toolResultHasImageData(result) {
		return toolResultTextContent(result)
	}

	// Build combined output: text parts inline, image parts as file paths.
	var parts []string
	for _, contentPart := range toolResultContentParts(result) {
		switch contentPart.Type {
		case ToolContentPartText:
			if contentPart.Text != "" {
				parts = append(parts, contentPart.Text)
			}
		case ToolContentPartImageData:
			mediaType, base64Data, ok := toolResultImageData(contentPart)
			if !ok {
				continue
			}
			path := p.imageDataToTempFile(mediaType, base64Data)
			if path != "" {
				parts = append(parts, path)
			}
		}
	}
	if len(parts) == 0 {
		return toolResultTextContent(result)
	}
	return strings.Join(parts, "\n")
}

// imageDataToTempFile decodes base64 image data, writes it to a temporary file,
// tracks it for cleanup, and returns the file path. Returns empty string on any error.
func (p *ClaudeBinProvider) imageDataToTempFile(mediaType, base64Data string) string {
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
		os.Remove(f.Name())
		return ""
	}
	return p.trackTempFile(f.Name())
}

func (p *ClaudeBinProvider) trackTempFile(path string) string {
	if path == "" {
		return ""
	}
	p.tempFilesMu.Lock()
	p.tempFiles = append(p.tempFiles, path)
	p.tempFilesMu.Unlock()
	return path
}

func (p *ClaudeBinProvider) cleanupTempFiles() {
	p.tempFilesMu.Lock()
	paths := p.tempFiles
	p.tempFiles = nil
	p.tempFilesMu.Unlock()

	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("claude-bin failed to remove temp image file", "path", path, "err", err)
		}
	}
}

// mediaTypeToExt maps an image MIME type to a file extension.
func mediaTypeToExt(mediaType string) string {
	switch mediaType {
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	default:
		return "jpg"
	}
}

func extractClaudeAssistantText(msg claudeAssistantMessage) string {
	var b strings.Builder
	for _, block := range msg.Message.Content {
		if block.Type == "text" && block.Text != "" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

// isPromptTooLong checks whether the error from claude CLI indicates the
// prompt exceeded the model's context window.
func isPromptTooLong(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "prompt is too long")
}

// maxToolResultCharsOnRetry is the maximum character length for each tool result
// when retrying after a "prompt too long" error (~5.7K tokens at 3.5 chars/token).
const maxToolResultCharsOnRetry = 20_000

// maxToolResultCharsOnAggressiveRetry is used for the second retry with much
// more aggressive truncation (~1.4K tokens at 3.5 chars/token).
const maxToolResultCharsOnAggressiveRetry = 5_000

// truncateToolResultsAt returns a copy of messages with oversized tool result
// content truncated to maxChars runes.
// Note: only copies Role and Parts — update if Message gains new fields.
func truncateToolResultsAt(messages []Message, maxChars int) []Message {
	out := make([]Message, len(messages))
	for i, msg := range messages {
		out[i] = Message{Role: msg.Role}
		out[i].Parts = make([]Part, len(msg.Parts))
		for j, part := range msg.Parts {
			out[i].Parts[j] = part
			if part.Type == PartToolResult && part.ToolResult != nil {
				content := part.ToolResult.Content
				runes := []rune(content)
				if len(runes) > maxChars {
					truncated := string(runes[:maxChars])
					truncated += fmt.Sprintf("\n[Truncated: showing first %d of %d chars]",
						maxChars, len(runes))
					// Clone ToolResult to avoid mutating original
					tr := *part.ToolResult
					tr.Content = truncated
					out[i].Parts[j].ToolResult = &tr
				}
			}
		}
	}
	return out
}

// hasImages returns true if any message in the list contains image data —
// either a PartImage in a user message or ToolContentPartImageData in a tool result.
func hasImages(messages []Message) bool {
	for _, msg := range messages {
		switch msg.Role {
		case RoleUser:
			for _, part := range msg.Parts {
				if part.Type == PartImage {
					return true
				}
			}
		case RoleTool:
			for _, part := range msg.Parts {
				if part.Type == PartToolResult && part.ToolResult != nil {
					if toolResultHasImageData(part.ToolResult) {
						return true
					}
				}
			}
		}
	}
	return false
}

// sdkUserMessage is the stream-json input format accepted by --input-format stream-json.
type sdkUserMessage struct {
	Type            string            `json:"type"`
	SessionID       string            `json:"session_id"`
	ParentToolUseID *string           `json:"parent_tool_use_id,omitempty"`
	Message         sdkMessageContent `json:"message"`
}

type sdkMessageContent struct {
	Role    string            `json:"role"`
	Content []sdkContentBlock `json:"content"`
}

type sdkContentBlock struct {
	Type   string          `json:"type"`
	Text   string          `json:"text,omitempty"`
	Source *sdkImageSource `json:"source,omitempty"`
}

type sdkImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// buildStreamJsonInput produces newline-delimited stream-json messages in
// message order. User messages are emitted as normal. Tool result images are
// replayed as synthetic follow-up user messages tied to the originating tool
// call via parent_tool_use_id so Claude can vision-analyze them on resume.
func (p *ClaudeBinProvider) buildStreamJsonInput(messages []Message, sessionID string) string {
	var lines []string
	appendUserMessage := func(content []sdkContentBlock, parentToolUseID *string) {
		if len(content) == 0 {
			return
		}
		msg := sdkUserMessage{
			Type:            "user",
			SessionID:       sessionID,
			ParentToolUseID: parentToolUseID,
			Message: sdkMessageContent{
				Role:    "user",
				Content: content,
			},
		}
		data, err := json.Marshal(msg)
		if err != nil {
			return
		}
		lines = append(lines, string(data))
	}

	var pendingDev string
	for _, msg := range messages {
		switch msg.Role {
		case RoleDeveloper:
			pendingDev = collectTextParts(msg.Parts)
		case RoleUser:
			blocks := buildSDKUserContentBlocks(msg.Parts)
			if pendingDev != "" {
				devBlock := sdkContentBlock{
					Type: "text",
					Text: fmt.Sprintf("<developer>\n%s\n</developer>\n\n", pendingDev),
				}
				blocks = append([]sdkContentBlock{devBlock}, blocks...)
				pendingDev = ""
			}
			appendUserMessage(blocks, nil)
		case RoleTool:
			for _, part := range msg.Parts {
				if part.Type != PartToolResult || part.ToolResult == nil {
					continue
				}
				blocks := buildSDKToolResultImageBlocks(part.ToolResult)
				if len(blocks) == 0 {
					continue
				}
				var parentToolUseID *string
				if id := strings.TrimSpace(part.ToolResult.ID); id != "" {
					parentToolUseID = &id
				}
				appendUserMessage(blocks, parentToolUseID)
			}
		}
	}

	return strings.Join(lines, "\n")
}

func (p *ClaudeBinProvider) formatToolOutputForClaude(output ToolOutput) string {
	return p.processToolResultContent(&ToolResult{
		Content:      output.Content,
		ContentParts: output.ContentParts,
	})
}

func buildSDKUserContentBlocks(parts []Part) []sdkContentBlock {
	blocks := make([]sdkContentBlock, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case PartText:
			if part.Text != "" {
				blocks = append(blocks, sdkContentBlock{Type: "text", Text: part.Text})
			}
		case PartImage:
			imageBlock, imagePath, ok := buildSDKImageBlock(part.ImagePath, part.ImageData)
			if !ok {
				continue
			}
			blocks = append(blocks, imageBlock)
			if imagePath != "" {
				blocks = append(blocks, sdkContentBlock{Type: "text", Text: "[image saved at: " + imagePath + "]"})
			}
		}
	}
	return blocks
}

func buildSDKToolResultImageBlocks(result *ToolResult) []sdkContentBlock {
	if result == nil {
		return nil
	}

	var blocks []sdkContentBlock
	for _, part := range toolResultContentParts(result) {
		mediaType, base64Data, ok := toolResultImageData(part)
		if !ok {
			continue
		}
		blocks = append(blocks, sdkContentBlock{
			Type: "image",
			Source: &sdkImageSource{
				Type:      "base64",
				MediaType: mediaType,
				Data:      base64Data,
			},
		})
	}
	return blocks
}

func buildSDKImageBlock(imagePath string, imageData *ToolImageData) (sdkContentBlock, string, bool) {
	mediaType, base64Data := "", ""
	switch {
	case imageData != nil && imageData.Base64 != "":
		mediaType = imageData.MediaType
		base64Data = imageData.Base64
	case imagePath != "":
		data, err := os.ReadFile(imagePath)
		if err != nil {
			return sdkContentBlock{}, "", false
		}
		mediaType = mediaTypeFromPath(imagePath)
		base64Data = base64.StdEncoding.EncodeToString(data)
	}
	if mediaType == "" || base64Data == "" {
		return sdkContentBlock{}, "", false
	}
	return sdkContentBlock{
		Type: "image",
		Source: &sdkImageSource{
			Type:      "base64",
			MediaType: mediaType,
			Data:      base64Data,
		},
	}, imagePath, true
}

// mediaTypeFromPath returns an image MIME type based on the file extension.
func mediaTypeFromPath(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

// JSON message types from claude CLI output

type claudeSystemMessage struct {
	Type      string   `json:"type"`
	SessionID string   `json:"session_id"`
	Model     string   `json:"model"`
	Tools     []string `json:"tools"`
}

type claudeAssistantMessage struct {
	Type    string `json:"type"`
	Message struct {
		Content []claudeContentBlock `json:"content"`
	} `json:"message"`
}

type claudeContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type claudeStreamlinedTextMessage struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	SessionID string `json:"session_id"`
}

type claudeResultMessage struct {
	Type              string            `json:"type"`
	IsError           bool              `json:"is_error"`
	Result            string            `json:"result"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
	Usage             struct {
		InputTokens          int `json:"input_tokens"`
		OutputTokens         int `json:"output_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

func (m claudeResultMessage) hasPermissionDenial() bool {
	if len(m.PermissionDenials) > 0 {
		return true
	}
	text := strings.ToLower(strings.TrimSpace(m.Result))
	return strings.Contains(text, "permission") &&
		(strings.Contains(text, "denied") || strings.Contains(text, "requires approval"))
}

func (m claudeResultMessage) permissionDenialText() string {
	tools := m.permissionDenialTools()
	if len(tools) == 0 {
		result := strings.TrimSpace(m.Result)
		if result != "" {
			return result
		}
		return "Claude Code denied a tool call before term-llm could execute it."
	}

	quoted := make([]string, 0, len(tools))
	for _, tool := range tools {
		quoted = append(quoted, "`"+tool+"`")
	}
	return fmt.Sprintf("Claude Code denied %s before term-llm could execute it.\nterm-llm did not execute the tool call.", strings.Join(quoted, ", "))
}

func (m claudeResultMessage) permissionDenialTools() []string {
	seen := make(map[string]struct{}, len(m.PermissionDenials))
	var tools []string
	for _, raw := range m.PermissionDenials {
		var denial map[string]any
		if err := json.Unmarshal(raw, &denial); err != nil {
			continue
		}
		for _, key := range []string{"tool_name", "toolName", "name", "tool"} {
			tool, ok := denial[key].(string)
			tool = strings.TrimSpace(tool)
			if !ok || tool == "" {
				continue
			}
			if _, exists := seen[tool]; exists {
				continue
			}
			seen[tool] = struct{}{}
			tools = append(tools, tool)
			break
		}
	}
	return tools
}

type claudeStreamEvent struct {
	Type  string `json:"type"`
	Event struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
}

package llm

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/mcphttp"
	"github.com/samsaffron/term-llm/internal/procutil"
)

const (
	grokStderrTailMaxLines = 40
	grokStdoutTailMaxLines = 40
	grokMaxTurns           = 30
	grokHomeMaxAge         = 30 * 24 * time.Hour
	grokToolLineGrace      = 25 * time.Millisecond
	grokToolLineGraceEnv   = "TERM_LLM_GROK_TOOL_LINE_GRACE_MS"
)

var grokCommandWaitDelay = time.Second
var grokToolDrainGrace = loadCLIToolLineDrainGrace(grokToolLineGraceEnv, grokToolLineGrace)

// grokEffortLevels is deliberately broader than grokBinEffortVariants: Grok
// accepts user-defined model names, which may support effort levels outside the
// curated grok-4.5 choices advertised by term-llm.
var grokEffortLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// GrokBinProvider uses the locally installed Grok Build CLI as an authenticated
// model transport. term-llm remains the sole tool owner: the CLI sees only an
// isolated in-process HTTP MCP server and all Grok built-in tools are disabled.
//
// Grok's streaming-json protocol does not include token usage, so this provider
// intentionally emits no EventUsage. Its isolated GROK_HOME also does not load
// custom [model.*] definitions from the user's normal ~/.grok/config.toml;
// model names remain open-ended and are passed through to the CLI.
// It is not safe for concurrent conversation streams; create one provider per conversation.
type GrokBinProvider struct {
	model                  string
	effort                 string
	sessionID              string
	messagesSent           int
	preferOAuth            bool
	extraEnv               map[string]string
	toolExecutorConfigured bool

	// grokHome is durable because Grok stores resumable session SQLite data under
	// GROK_HOME. CleanupMCP deliberately leaves this directory in place.
	grokHome string

	mcpServer *mcphttp.Server

	cliToolBridgeState
	tempFileTracker

	activeStream atomic.Bool
}

type grokBinProviderState struct {
	GrokHome     string `json:"grok_home"`
	SessionID    string `json:"session_id"`
	MessagesSent int    `json:"messages_sent"`
}

type grokACPPrompt struct {
	Type    string           `json:"type"`
	Content []grokACPContent `json:"content"`
}

type grokACPContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type grokStreamState struct {
	sawEnd          bool
	stopReason      string
	sessionID       string
	maxTurnsReached bool
}

type grokCommandResult struct {
	sawEnd    bool
	sessionID string
}

func parseGrokEffort(model string) (string, string) {
	for _, effort := range grokEffortLevels {
		suffix := "-" + effort
		if strings.HasSuffix(model, suffix) && len(model) > len(suffix) {
			return strings.TrimSuffix(model, suffix), effort
		}
	}
	return model, ""
}

// ValidateGrokBinModel rejects the common provider:model typo where an effort
// level is supplied as the whole model. Model names are otherwise deliberately
// open-ended because `grok models` depends on the user's subscription.
func ValidateGrokBinModel(model string) error {
	for _, effort := range grokEffortLevels {
		if model == effort {
			return fmt.Errorf("grok-bin model %q is an effort level, not a model; did you mean \"grok-bin:grok-4.5-%s\"?", model, model)
		}
	}
	return nil
}

func NewGrokBinProvider(model string, env map[string]string) *GrokBinProvider {
	actualModel, effort := parseGrokEffort(strings.TrimSpace(model))
	p := &GrokBinProvider{
		model:       actualModel,
		effort:      effort,
		preferOAuth: true,
	}
	p.tempFileTracker.logName = "grok-bin"
	p.SetEnv(env)
	return p
}

func (p *GrokBinProvider) Name() string {
	model := p.model
	if model == "" {
		model = "grok-4.5"
	}
	if p.effort != "" {
		return fmt.Sprintf("Grok CLI (%s, effort=%s)", model, p.effort)
	}
	return fmt.Sprintf("Grok CLI (%s)", model)
}

func (p *GrokBinProvider) Credential() string { return "grok-bin" }

func (p *GrokBinProvider) Capabilities() Capabilities {
	return Capabilities{
		ToolCalls:          true,
		SupportsToolChoice: false,
		ManagesOwnContext:  true,
		InlineToolLoop:     true,
	}
}

func (p *GrokBinProvider) SetPreferOAuth(prefer bool) { p.preferOAuth = prefer }

func (p *GrokBinProvider) SetEnv(env map[string]string) {
	if len(env) == 0 {
		p.extraEnv = nil
		return
	}
	p.extraEnv = make(map[string]string, len(env))
	for key, value := range env {
		p.extraEnv[key] = value
	}
}

func (p *GrokBinProvider) SetToolExecutor(executor func(context.Context, string, json.RawMessage) (ToolOutput, error)) {
	// The HTTP MCP executor routes through the engine via cliToolBridgeState. We
	// only need to remember whether engine wiring is available.
	p.toolExecutorConfigured = executor != nil
}

func (p *GrokBinProvider) ResetConversation() {
	p.sessionID = ""
	p.messagesSent = 0
}

func (p *GrokBinProvider) ExportProviderState() ([]byte, bool) {
	if strings.TrimSpace(p.sessionID) == "" || strings.TrimSpace(p.grokHome) == "" {
		return nil, false
	}
	data, err := json.Marshal(grokBinProviderState{
		GrokHome:     p.grokHome,
		SessionID:    p.sessionID,
		MessagesSent: p.messagesSent,
	})
	if err != nil {
		return nil, false
	}
	return data, true
}

func (p *GrokBinProvider) ImportProviderState(data []byte) error {
	var state grokBinProviderState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode grok-bin provider state: %w", err)
	}
	state.GrokHome = strings.TrimSpace(state.GrokHome)
	state.SessionID = strings.TrimSpace(state.SessionID)
	if state.GrokHome == "" {
		return fmt.Errorf("decode grok-bin provider state: missing grok_home")
	}
	if state.SessionID == "" {
		return fmt.Errorf("decode grok-bin provider state: missing session_id")
	}
	if state.MessagesSent < 0 {
		return fmt.Errorf("decode grok-bin provider state: negative messages_sent")
	}

	home, existed, err := validateGrokHomeState(state.GrokHome)
	if err != nil {
		return fmt.Errorf("decode grok-bin provider state: %w", err)
	}
	if err := ensureGrokHomeLayout(home); err != nil {
		return fmt.Errorf("restore grok-bin home: %w", err)
	}
	if !existed {
		// The cache may have been cleared manually or by age-based GC while the
		// term-llm session row survived. The old Grok session database is gone, so
		// resume from the full term-llm transcript instead of retrying a dead ID.
		slog.Warn("grok-bin durable home is missing; resetting resume state", "grok_home", home)
		state.SessionID = ""
		state.MessagesSent = 0
	}

	// Apply only after all untrusted state has been validated.
	p.grokHome = home
	p.sessionID = state.SessionID
	p.messagesSent = state.MessagesSent
	p.touchGrokHome()
	p.gcStaleGrokHomes(home)
	return nil
}

func (p *GrokBinProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		p.activeRuns.Add(1)
		defer p.finishStreamCleanup()

		if !p.activeStream.CompareAndSwap(false, true) {
			if req.Ephemeral {
				return fmt.Errorf("grok-bin ephemeral request cannot run while another stream is active on the same provider")
			}
			return fmt.Errorf("grok-bin provider already has an active stream; create a dedicated provider instance per conversation")
		}
		defer p.activeStream.Store(false)

		if err := p.ensureGrokHome(); err != nil {
			return err
		}
		messagesToSend, err := p.messagesForRequest(req)
		if err != nil {
			return err
		}

		debug := req.Debug || req.DebugRaw
		exposeToolBridge := false
		if len(req.Tools) > 0 {
			if !p.toolExecutorConfigured {
				slog.Warn("grok-bin tools requested but no tool executor configured", "tool_count", len(req.Tools))
				if err := p.writeConfig("", ""); err != nil {
					return err
				}
			} else if err := p.ensureMCPServer(ctx, req.Tools, debug); err != nil {
				return err
			} else {
				exposeToolBridge = true
			}
		} else if p.mcpServer == nil {
			if err := p.writeConfig("", ""); err != nil {
				return err
			}
		}

		prompt, err := buildGrokACPPrompt(messagesToSend)
		if err != nil {
			return err
		}
		promptPath, err := p.writePromptFile(prompt)
		if err != nil {
			return err
		}
		args, effort, err := p.buildArgs(req, promptPath)
		if err != nil {
			return err
		}

		result, err := p.runGrokCommand(ctx, args, effort, prompt, debug, send, req.Ephemeral, exposeToolBridge)
		if err != nil {
			return err
		}
		if !req.Ephemeral && result.sawEnd {
			if result.sessionID != "" {
				p.sessionID = result.sessionID
			}
			p.messagesSent = len(req.Messages)
			p.touchGrokHome()
		}
		return send.Send(Event{Type: EventDone})
	}), nil
}

func (p *GrokBinProvider) messagesForRequest(req Request) ([]Message, error) {
	if req.Ephemeral || p.sessionID == "" {
		return req.Messages, nil
	}

	switch {
	case p.messagesSent > len(req.Messages):
		slog.Warn("grok-bin resume message boundary exceeded request transcript; resetting conversation state",
			"messages_sent", p.messagesSent, "request_messages", len(req.Messages))
		p.ResetConversation()
		return req.Messages, nil
	case p.messagesSent == len(req.Messages):
		return nil, fmt.Errorf("grok-bin resumed session has no new messages to send")
	case p.messagesSent > 0:
		messages := grokResumeMessages(req.Messages[p.messagesSent:])
		if len(messages) == 0 {
			return nil, fmt.Errorf("grok-bin resumed session has no new messages to send")
		}
		return messages, nil
	default:
		return req.Messages, nil
	}
}

func grokResumeMessages(messages []Message) []Message {
	// Grok's durable session already contains generated assistant/tool output.
	// Drop only that leading replay, preserving every subsequent user or
	// developer message (including deferred interjections).
	for i, message := range messages {
		switch message.Role {
		case RoleSystem, RoleEvent, RoleAssistant, RoleTool:
			continue
		default:
			return messages[i:]
		}
	}
	return nil
}

func (p *GrokBinProvider) buildArgs(req Request, promptPath string) ([]string, string, error) {
	if strings.TrimSpace(p.grokHome) == "" {
		return nil, "", fmt.Errorf("grok-bin GROK_HOME is not initialized")
	}
	neutralCWD := filepath.Join(p.grokHome, "cwd")
	if err := os.MkdirAll(neutralCWD, 0o700); err != nil {
		return nil, "", fmt.Errorf("create grok-bin neutral cwd: %w", err)
	}

	args := []string{
		"--prompt-file", promptPath,
		"--output-format", "streaming-json",
		"--always-approve",
		"--tools", "",
		"--max-turns", strconv.Itoa(grokMaxTurns),
		"--cwd", neutralCWD,
		"--no-memory",
		"--no-subagents",
		"--no-plan",
		"--disable-web-search",
		"--no-auto-update",
	}

	model := chooseModel(req.Model, p.model)
	model, requestModelEffort := parseGrokEffort(model)
	effort := p.effort
	if requestModelEffort != "" {
		effort = requestModelEffort
	}
	if requested := strings.TrimSpace(req.ReasoningEffort); requested != "" {
		effort = requested
	}
	if model != "" {
		args = append(args, "-m", model)
	}
	if effort != "" {
		args = append(args, "--reasoning-effort", effort)
	}
	if !req.Ephemeral && p.sessionID != "" {
		args = append(args, "--resume", p.sessionID)
	}
	if systemPrompt := extractSystemPrompt(req.Messages); systemPrompt != "" {
		args = append(args, "--system-prompt-override", systemPrompt)
	}
	return args, effort, nil
}

func grokSystemPromptArg(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--system-prompt-override" {
			return args[i+1]
		}
	}
	return ""
}

func redactedGrokArgs(args []string) []string {
	redacted := append([]string(nil), args...)
	for i := 0; i+1 < len(redacted); i++ {
		if redacted[i] == "--system-prompt-override" {
			redacted[i+1] = "<redacted>"
		}
	}
	return redacted
}

func redactGrokSystemPrompt(text, systemPrompt string) string {
	if systemPrompt == "" {
		return text
	}
	return strings.ReplaceAll(text, systemPrompt, "<redacted>")
}

func (p *GrokBinProvider) buildCommandEnv() []string {
	authPath := strings.TrimSpace(p.extraEnv["GROK_AUTH_PATH"])
	if authPath == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			authPath = filepath.Join(home, ".grok", "auth.json")
		}
	}

	forced := map[string]string{
		"GROK_HOME":                p.grokHome,
		"GROK_DISABLE_AUTOUPDATER": "1",
		"GROK_AUTH_PATH":           authPath,
	}
	filtered := make([]string, 0, len(os.Environ())+len(p.extraEnv)+len(forced))
	for _, entry := range os.Environ() {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		if _, ok := forced[key]; ok {
			continue
		}
		if p.preferOAuth && key == "XAI_API_KEY" {
			continue
		}
		if _, ok := p.extraEnv[key]; ok {
			continue
		}
		filtered = append(filtered, entry)
	}
	for key, value := range p.extraEnv {
		if _, ok := forced[key]; ok {
			continue
		}
		if p.preferOAuth && key == "XAI_API_KEY" {
			continue
		}
		filtered = append(filtered, key+"="+value)
	}
	for key, value := range forced {
		filtered = append(filtered, key+"="+value)
	}
	return filtered
}

func (p *GrokBinProvider) prepareGrokCommand(ctx context.Context, args []string) (*exec.Cmd, func(), error) {
	cmd := exec.CommandContext(ctx, "grok", args...)
	cmd.WaitDelay = grokCommandWaitDelay
	cmd.Env = p.buildCommandEnv()
	cleanup, err := procutil.PrepareCommand(cmd)
	if err != nil {
		return nil, nil, fmt.Errorf("prepare grok command: %w", err)
	}
	return cmd, cleanup, nil
}

func (p *GrokBinProvider) runGrokCommand(
	ctx context.Context,
	args []string,
	effort string,
	prompt []byte,
	debug bool,
	send eventSender,
	ephemeral bool,
	exposeToolBridge bool,
) (grokCommandResult, error) {
	systemPrompt := grokSystemPromptArg(args)
	if debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Grok CLI Command ===")
		fmt.Fprintf(os.Stderr, "grok %s\n", shellJoin(redactedGrokArgs(args)))
		fmt.Fprintf(os.Stderr, "Prompt length: %d bytes (via --prompt-file)\n", len(prompt))
		fmt.Fprintln(os.Stderr, "================================")
	}

	cmd, cleanup, err := p.prepareGrokCommand(ctx, args)
	if err != nil {
		return grokCommandResult{}, err
	}
	defer cleanup()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return grokCommandResult{}, fmt.Errorf("get grok stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return grokCommandResult{}, fmt.Errorf("get grok stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return grokCommandResult{}, fmt.Errorf("start grok: %w", err)
	}

	bridge := &cliTurnBridge{toolReqCh: make(chan cliToolRequest, 64), done: make(chan struct{})}
	var bridgeDoneOnce sync.Once
	stopScanner := func() { bridgeDoneOnce.Do(func() { close(bridge.done) }) }
	if exposeToolBridge {
		p.cliToolBridgeState.activate(bridge, send.ch)
	}
	defer func() {
		if exposeToolBridge {
			p.cliToolBridgeState.deactivate(bridge)
		}
		stopScanner()
	}()

	var stderrMu sync.Mutex
	var stderrTail []string
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if debug {
				fmt.Fprintf(os.Stderr, "[grok stderr] %s\n", redactGrokSystemPrompt(line, systemPrompt))
			}
			recordCLITailLine(&stderrMu, &stderrTail, line, grokStderrTailMaxLines)
		}
	}()

	lineCh := make(chan string, 256)
	scanErrCh := make(chan error, 1)
	var stdoutMu sync.Mutex
	var stdoutTail []string
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			recordCLITailLine(&stdoutMu, &stdoutTail, line, grokStdoutTailMaxLines)
			if line == "" {
				continue
			}
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
		close(lineCh)
		scanErrCh <- scanner.Err()
	}()

	state, toolsExecuted, dispatchErr := p.dispatchGrokEvents(ctx, lineCh, bridge.toolReqCh, debug, send, ephemeral)
	if dispatchErr != nil {
		stopScanner()
		if cmd.Cancel != nil {
			_ = cmd.Cancel()
		} else if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	scanErr := <-scanErrCh
	cmdErr := cmd.Wait()
	<-stderrDone

	if dispatchErr != nil {
		return grokCommandResult{}, dispatchErr
	}
	if scanErr != nil {
		return grokCommandResult{}, fmt.Errorf("read grok output: %w", scanErr)
	}
	if cmdErr != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(cmdErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		if !state.maxTurnsReached {
			commandErr := p.newGrokCommandError(cmdErr, exitCode, args, effort, prompt, toolsExecuted,
				snapshotCLITail(&stdoutMu, stdoutTail), snapshotCLITail(&stderrMu, stderrTail))
			slog.Error("grok command failed",
				"exit_code", exitCode,
				"tools_executed", toolsExecuted,
				"command_line", commandErr.CommandLine,
				"prompt_len", commandErr.PromptLen,
				"prompt_sha256", commandErr.PromptSHA256,
				"stderr_tail", commandErr.StderrTail,
				"stdout_tail", commandErr.StdoutTail,
			)
			return grokCommandResult{}, p.userFacingGrokCommandError(commandErr)
		}
	}
	if !state.sawEnd {
		if state.maxTurnsReached {
			return grokCommandResult{}, nil
		}
		return grokCommandResult{}, fmt.Errorf("grok CLI output ended without an end event")
	}
	return grokCommandResult{sawEnd: true, sessionID: state.sessionID}, nil
}

func (p *GrokBinProvider) dispatchGrokEvents(
	ctx context.Context,
	lineCh <-chan string,
	toolReqCh <-chan cliToolRequest,
	debug bool,
	send eventSender,
	ephemeral bool,
) (grokStreamState, bool, error) {
	state := grokStreamState{}
	linesOpen := true
	toolsExecuted := false
	handleLine := func(line string) error {
		return p.handleGrokLine(line, debug, send, ephemeral, &state)
	}

	for linesOpen {
		hadLine := false
		for linesOpen {
			select {
			case line, ok := <-lineCh:
				if !ok {
					linesOpen = false
					break
				}
				hadLine = true
				if err := handleLine(line); err != nil {
					return state, toolsExecuted, err
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
			if err := handleLine(line); err != nil {
				return state, toolsExecuted, err
			}
		case req := <-toolReqCh:
			if err := drainCLILinesWithGrace(ctx, lineCh, grokToolDrainGrace, handleLine); err != nil {
				return state, toolsExecuted, err
			}
			toolsExecuted = true
			handleCLIToolRequest(req, send)
		case <-ctx.Done():
			return state, toolsExecuted, ctx.Err()
		}
	}

	for {
		select {
		case req := <-toolReqCh:
			toolsExecuted = true
			handleCLIToolRequest(req, send)
		default:
			return state, toolsExecuted, nil
		}
	}
}

func (p *GrokBinProvider) handleGrokLine(line string, debug bool, send eventSender, ephemeral bool, state *grokStreamState) error {
	var event struct {
		Type       string `json:"type"`
		Data       string `json:"data"`
		Message    string `json:"message"`
		StopReason string `json:"stopReason"`
		SessionID  string `json:"sessionId"`
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "[grok-bin] ignoring malformed streaming-json line: %s\n", truncateOneLine(line, 200))
		}
		return nil
	}

	switch event.Type {
	case "thought":
		if event.Data != "" {
			return send.Send(Event{Type: EventReasoningDelta, Text: event.Data, ReasoningKind: ReasoningKindRaw})
		}
	case "text":
		if event.Data != "" {
			return send.Send(Event{Type: EventTextDelta, Text: event.Data})
		}
	case "end":
		state.sawEnd = true
		state.stopReason = event.StopReason
		if !ephemeral {
			state.sessionID = strings.TrimSpace(event.SessionID)
		}
	case "error":
		message := strings.TrimSpace(event.Message)
		if message == "" {
			message = strings.TrimSpace(event.Data)
		}
		if message == "" {
			message = "unknown Grok CLI error"
		}
		return fmt.Errorf("grok CLI error: %s", message)
	case "max_turns_reached":
		if !state.maxTurnsReached {
			state.maxTurnsReached = true
			return send.Send(Event{Type: EventPhase, Text: WarningPhasePrefix + fmt.Sprintf("Grok CLI reached its %d-turn safety budget. Tool effects and output from this turn were preserved; send a follow-up to continue.", grokMaxTurns)})
		}
	default:
		// The documented event list is non-exhaustive. Ignore auto_compact and
		// future metadata events while continuing to parse text/end events.
	}
	return nil
}

func (p *GrokBinProvider) newGrokCommandError(cmdErr error, exitCode int, args []string, effort string, prompt []byte, toolsExecuted bool, stdoutTail, stderrTail []string) *CLICommandError {
	sum := sha256.Sum256(prompt)
	env, removed := p.commandEnvDebugFields()
	systemPrompt := grokSystemPromptArg(args)
	redactedArgs := redactedGrokArgs(args)
	stdoutText := redactGrokSystemPrompt(strings.Join(normalizeCLITail(stdoutTail), "\n"), systemPrompt)
	stderrText := redactGrokSystemPrompt(strings.Join(normalizeCLITail(stderrTail), "\n"), systemPrompt)
	return &CLICommandError{
		BinName:       "grok",
		ErrorType:     "grok_cli_command",
		ExitCode:      exitCode,
		Err:           cmdErr,
		Args:          redactedArgs,
		CommandLine:   shellJoin(append([]string{"grok"}, redactedArgs...)),
		Cwd:           filepath.Join(p.grokHome, "cwd"),
		Effort:        effort,
		ToolsExecuted: toolsExecuted,
		PreferOAuth:   p.preferOAuth,
		Env:           env,
		RemovedEnv:    removed,
		PromptLen:     len(prompt),
		PromptSHA256:  hex.EncodeToString(sum[:]),
		StdoutTail:    stdoutText,
		StderrTail:    stderrText,
	}
}

func (p *GrokBinProvider) commandEnvDebugFields() (map[string]string, []string) {
	env := map[string]string{
		"GROK_HOME":                p.grokHome,
		"GROK_DISABLE_AUTOUPDATER": "1",
	}
	if authPath := strings.TrimSpace(p.extraEnv["GROK_AUTH_PATH"]); authPath != "" {
		env["GROK_AUTH_PATH"] = redactEnvValue("GROK_AUTH_PATH", authPath)
	} else if home, err := os.UserHomeDir(); err == nil {
		env["GROK_AUTH_PATH"] = filepath.Join(home, ".grok", "auth.json")
	}
	var removed []string
	if p.preferOAuth {
		if os.Getenv("XAI_API_KEY") != "" {
			removed = append(removed, "XAI_API_KEY")
		}
		if _, ok := p.extraEnv["XAI_API_KEY"]; ok {
			removed = append(removed, "XAI_API_KEY (provider env)")
		}
	}
	for key, value := range p.extraEnv {
		if key == "GROK_HOME" || key == "GROK_DISABLE_AUTOUPDATER" || key == "GROK_AUTH_PATH" || (p.preferOAuth && key == "XAI_API_KEY") {
			continue
		}
		env[key] = redactEnvValue(key, value)
	}
	if len(removed) == 0 {
		removed = nil
	}
	return env, removed
}

func (p *GrokBinProvider) userFacingGrokCommandError(err *CLICommandError) error {
	if err == nil {
		return nil
	}
	detail := firstUsefulCLIDiagnosticLine(err.StderrTail)
	if detail == "" {
		detail = firstUsefulCLIDiagnosticLine(err.StdoutTail)
	}
	if detail == "" && err.Err != nil {
		detail = err.Err.Error()
	}
	combined := strings.ToLower(err.StderrTail + "\n" + err.StdoutTail)
	summary := fmt.Sprintf("Grok CLI exited before completing the turn (exit %d)", err.ExitCode)
	if strings.Contains(combined, "not logged in") || strings.Contains(combined, "login") || strings.Contains(combined, "authentication") {
		summary = "Grok CLI is not logged in"
	}
	return &UserFacingProviderError{Summary: summary, Detail: detail, Cause: err}
}

func (p *GrokBinProvider) ensureMCPServer(ctx context.Context, tools []ToolSpec, debug bool) error {
	// Match claude-bin's conversation-scoped bridge semantics: the first tool
	// list registered for a provider instance remains fixed until CleanupMCP.
	if p.mcpServer != nil {
		return nil
	}
	if debug {
		fmt.Fprintf(os.Stderr, "[grok-bin] starting HTTP MCP server for %d tools\n", len(tools))
	}
	server := mcphttp.NewServer(p.cliToolBridgeState.wrappedExecutor(formatToolOutputForGrok))
	server.SetDebug(debug)
	url, token, err := server.Start(ctx, mcpToolSpecs(tools))
	if err != nil {
		return fmt.Errorf("start grok-bin MCP server: %w", err)
	}
	if err := p.writeConfig(url, token); err != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), mcpStopTimeout)
		server.Stop(stopCtx)
		cancel()
		return err
	}
	p.mcpServer = server
	return nil
}

// CleanupMCP stops the per-provider HTTP bridge and removes per-turn prompt
// files. It intentionally keeps GROK_HOME because Grok's --resume data lives
// there and may be restored after serve runtime eviction.
func (p *GrokBinProvider) CleanupMCP() {
	if p.mcpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), mcpStopTimeout)
		p.mcpServer.Stop(ctx)
		cancel()
		p.mcpServer = nil
	}
	p.cleanupTempFiles()
}

func (p *GrokBinProvider) CleanupTurn() { p.cleanupTempFilesIfIdle() }

func renderGrokConfig(url, token string) string {
	var b strings.Builder
	for _, section := range []string{"claude", "cursor"} {
		fmt.Fprintf(&b, "[compat.%s]\n", section)
		for _, key := range []string{"skills", "rules", "agents", "mcps", "hooks"} {
			fmt.Fprintf(&b, "%s = false\n", key)
		}
		b.WriteString("\n")
	}
	if strings.TrimSpace(url) != "" {
		b.WriteString("[mcp_servers.term-llm]\n")
		fmt.Fprintf(&b, "url = %s\n", strconv.Quote(url))
		fmt.Fprintf(&b, "headers = { \"Authorization\" = %s }\n", strconv.Quote("Bearer "+token))
	}
	return b.String()
}

func (p *GrokBinProvider) writeConfig(url, token string) error {
	if strings.TrimSpace(p.grokHome) == "" {
		return fmt.Errorf("write grok-bin config: GROK_HOME is not initialized")
	}
	data := []byte(renderGrokConfig(url, token))
	tmp, err := os.CreateTemp(p.grokHome, ".config-*.toml")
	if err != nil {
		return fmt.Errorf("create grok-bin config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod grok-bin config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write grok-bin config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close grok-bin config: %w", err)
	}
	if err := os.Rename(tmpPath, filepath.Join(p.grokHome, "config.toml")); err != nil {
		return fmt.Errorf("install grok-bin config: %w", err)
	}
	return nil
}

func (p *GrokBinProvider) writePromptFile(prompt []byte) (string, error) {
	file, err := os.CreateTemp(p.grokHome, "prompt-*.json")
	if err != nil {
		return "", fmt.Errorf("create grok-bin prompt file: %w", err)
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		os.Remove(path)
		return "", fmt.Errorf("chmod grok-bin prompt file: %w", err)
	}
	if _, err := file.Write(prompt); err != nil {
		file.Close()
		os.Remove(path)
		return "", fmt.Errorf("write grok-bin prompt file: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close grok-bin prompt file: %w", err)
	}
	return p.trackTempFile(path), nil
}

func buildGrokACPPrompt(messages []Message) ([]byte, error) {
	var blocks []grokACPContent
	emitMessages := messages
	if start := conversationFinalTurnStart(messages); start > 0 && messagesContainPriorAssistantTurn(messages[:start]) {
		history := strings.TrimSpace(strings.Join(renderGrokConversationParts(messages[:start]), "\n\n"))
		if history != "" {
			blocks = append(blocks, grokACPContent{
				Type: "text",
				Text: "<conversation_history>\n" + history + "\n</conversation_history>\n\n" + cliBinResumeReplayInstruction + "\n\n",
			})
		}
		emitMessages = messages[start:]
	}

	var pendingDeveloper string
	for _, message := range emitMessages {
		switch message.Role {
		case RoleSystem, RoleEvent:
			continue
		case RoleDeveloper:
			pendingDeveloper = collectTextParts(message.Parts)
		case RoleUser:
			if pendingDeveloper != "" {
				blocks = append(blocks, grokACPContent{Type: "text", Text: "<developer>\n" + pendingDeveloper + "\n</developer>\n\n"})
				pendingDeveloper = ""
			}
			blocks = append(blocks, grokACPBlocksForParts(message.Parts)...)
		case RoleAssistant:
			if text := collectTextParts(message.Parts); text != "" {
				blocks = append(blocks, grokACPContent{Type: "text", Text: "Assistant: " + text})
			}
			for _, part := range message.Parts {
				if part.Type == PartToolCall && part.ToolCall != nil {
					blocks = append(blocks, grokACPContent{Type: "text", Text: "Assistant called tool: " + part.ToolCall.Name})
				}
			}
		case RoleTool:
			for _, part := range message.Parts {
				if part.Type != PartToolResult || part.ToolResult == nil {
					continue
				}
				if text := toolResultTextContent(part.ToolResult); text != "" {
					blocks = append(blocks, grokACPContent{Type: "text", Text: fmt.Sprintf("Tool result (%s): %s", part.ToolResult.Name, text)})
				}
				for _, contentPart := range toolResultContentParts(part.ToolResult) {
					mediaType, data, ok := toolResultImageData(contentPart)
					if ok {
						blocks = append(blocks, grokACPContent{Type: "image", Data: data, MimeType: mediaType})
					}
				}
			}
		}
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("build grok-bin ACP prompt: no user-visible content")
	}
	data, err := json.Marshal(grokACPPrompt{Type: "acp", Content: blocks})
	if err != nil {
		return nil, fmt.Errorf("encode grok-bin ACP prompt: %w", err)
	}
	return data, nil
}

func grokACPBlocksForParts(parts []Part) []grokACPContent {
	blocks := make([]grokACPContent, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case PartText, PartFile:
			if part.Text != "" {
				blocks = append(blocks, grokACPContent{Type: "text", Text: part.Text})
			}
		case PartImage:
			mediaType, data := "", ""
			switch {
			case part.ImageData != nil && strings.TrimSpace(part.ImageData.Base64) != "":
				mediaType = part.ImageData.MediaType
				data = part.ImageData.Base64
			case strings.TrimSpace(part.ImagePath) != "":
				raw, ok := readHydratableImagePath(part.ImagePath)
				if ok {
					mediaType = mediaTypeFromPath(part.ImagePath)
					data = base64.StdEncoding.EncodeToString(raw)
				}
			}
			if mediaType != "" && data != "" {
				blocks = append(blocks, grokACPContent{Type: "image", Data: data, MimeType: mediaType})
			}
		}
	}
	return blocks
}

func renderGrokConversationParts(messages []Message) []string {
	var out []string
	var pendingDeveloper string
	for _, message := range messages {
		switch message.Role {
		case RoleDeveloper:
			pendingDeveloper = collectTextParts(message.Parts)
		case RoleUser:
			var parts []string
			if pendingDeveloper != "" {
				parts = append(parts, "<developer>\n"+pendingDeveloper+"\n</developer>")
				pendingDeveloper = ""
			}
			for _, part := range message.Parts {
				switch part.Type {
				case PartText, PartFile:
					if part.Text != "" {
						parts = append(parts, part.Text)
					}
				case PartImage:
					parts = append(parts, "[image]")
				}
			}
			if len(parts) > 0 {
				out = append(out, "User: "+strings.Join(parts, "\n"))
			}
		case RoleAssistant:
			if text := collectTextParts(message.Parts); text != "" {
				out = append(out, "Assistant: "+text)
			}
			for _, part := range message.Parts {
				if part.Type == PartToolCall && part.ToolCall != nil {
					out = append(out, "Assistant called tool: "+part.ToolCall.Name)
				}
			}
		case RoleTool:
			for _, part := range message.Parts {
				if part.Type == PartToolResult && part.ToolResult != nil {
					out = append(out, fmt.Sprintf("Tool result (%s): %s", part.ToolResult.Name, toolResultTextContent(part.ToolResult)))
				}
			}
		}
	}
	return out
}

func formatToolOutputForGrok(output ToolOutput) string {
	result := &ToolResult{Content: output.Content, ContentParts: output.ContentParts}
	text := strings.TrimSpace(toolResultTextContent(result))
	if text != "" {
		return text
	}
	if toolResultHasImageData(result) {
		return "[Tool returned image data. The image can be attached to a follow-up turn if visual analysis is required.]"
	}
	return output.Content
}

func grokBinCacheBase() (string, error) {
	cacheDir := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME"))
	if cacheDir == "" {
		var err error
		cacheDir, err = os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve user cache directory: %w", err)
		}
	}
	base, err := filepath.Abs(filepath.Join(cacheDir, "term-llm", "grok-bin"))
	if err != nil {
		return "", fmt.Errorf("resolve grok-bin cache directory: %w", err)
	}
	return filepath.Clean(base), nil
}

func (p *GrokBinProvider) ensureGrokHome() error {
	if strings.TrimSpace(p.grokHome) == "" {
		base, err := grokBinCacheBase()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(base, 0o700); err != nil {
			return fmt.Errorf("create grok-bin cache directory: %w", err)
		}
		id, err := newGrokHomeID()
		if err != nil {
			return err
		}
		p.grokHome = filepath.Join(base, id)
	} else {
		home, err := validateGrokHome(p.grokHome)
		if err != nil {
			return err
		}
		p.grokHome = home
	}
	if err := ensureGrokHomeLayout(p.grokHome); err != nil {
		return err
	}
	p.touchGrokHome()
	p.gcStaleGrokHomes(p.grokHome)
	return nil
}

func ensureGrokHomeLayout(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create GROK_HOME: %w", err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return fmt.Errorf("chmod GROK_HOME: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(home, "cwd"), 0o700); err != nil {
		return fmt.Errorf("create grok-bin neutral cwd: %w", err)
	}
	return nil
}

func validateGrokHome(home string) (string, error) {
	candidate, _, err := validateGrokHomeState(home)
	return candidate, err
}

func validateGrokHomeState(home string) (string, bool, error) {
	base, err := grokBinCacheBase()
	if err != nil {
		return "", false, err
	}
	candidate, err := filepath.Abs(home)
	if err != nil {
		return "", false, fmt.Errorf("resolve grok_home: %w", err)
	}
	candidate = filepath.Clean(candidate)
	if filepath.Dir(candidate) != base || !isGrokHomeID(filepath.Base(candidate)) {
		return "", false, fmt.Errorf("grok_home must be a conversation directory directly under %s", base)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", false, fmt.Errorf("create grok-bin cache directory: %w", err)
	}
	existed := true
	if info, statErr := os.Lstat(candidate); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", false, fmt.Errorf("grok_home must not be a symlink")
		}
		if !info.IsDir() {
			return "", false, fmt.Errorf("grok_home is not a directory")
		}
	} else if !os.IsNotExist(statErr) {
		return "", false, fmt.Errorf("inspect grok_home: %w", statErr)
	} else {
		existed = false
		if err := os.Mkdir(candidate, 0o700); err != nil {
			return "", false, fmt.Errorf("recreate grok_home: %w", err)
		}
	}

	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", false, fmt.Errorf("resolve grok-bin cache symlinks: %w", err)
	}
	resolvedHome, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", false, fmt.Errorf("resolve grok_home symlinks: %w", err)
	}
	rel, err := filepath.Rel(resolvedBase, resolvedHome)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false, fmt.Errorf("grok_home resolves outside %s", resolvedBase)
	}
	return candidate, existed, nil
}

func newGrokHomeID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate grok-bin conversation id: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

func isGrokHomeID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func (p *GrokBinProvider) touchGrokHome() {
	if p.grokHome == "" {
		return
	}
	now := time.Now()
	marker := filepath.Join(p.grokHome, ".last_used")
	if err := os.WriteFile(marker, []byte(now.UTC().Format(time.RFC3339Nano)), 0o600); err != nil {
		slog.Debug("grok-bin could not update home age marker", "path", marker, "err", err)
	}
}

// gcStaleGrokHomes runs only after this provider has selected or imported its
// home, so it can never delete the directory about to be resumed.
func (p *GrokBinProvider) gcStaleGrokHomes(preserve string) {
	base, err := grokBinCacheBase()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-grokHomeMaxAge)
	for _, entry := range entries {
		if !entry.IsDir() || !isGrokHomeID(entry.Name()) {
			continue
		}
		path := filepath.Join(base, entry.Name())
		if filepath.Clean(path) == filepath.Clean(preserve) {
			continue
		}
		info, err := os.Stat(filepath.Join(path, ".last_used"))
		if os.IsNotExist(err) {
			info, err = entry.Info()
		}
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			slog.Debug("grok-bin stale home cleanup failed", "path", path, "err", err)
		}
	}
}

var _ Provider = (*GrokBinProvider)(nil)
var _ ToolExecutorSetter = (*GrokBinProvider)(nil)
var _ ProviderCleaner = (*GrokBinProvider)(nil)
var _ ProviderTurnCleaner = (*GrokBinProvider)(nil)
var _ ProviderStateExporter = (*GrokBinProvider)(nil)
var _ ProviderStateImporter = (*GrokBinProvider)(nil)

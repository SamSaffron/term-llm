package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/mcphttp"
	"github.com/samsaffron/term-llm/internal/procutil"
)

const (
	cursorStderrTailMaxLines    = 40
	cursorStdoutTailMaxLines    = 40
	cursorHomeMaxAge            = 30 * 24 * time.Hour
	cursorMCPServerName         = "term-llm"
	cursorAllowedTools          = "mcp_tool_call,get_mcp_tools_tool_call"
	cursorToolLineGrace         = 75 * time.Millisecond
	cursorToolLineGraceEnv      = "TERM_LLM_CURSOR_TOOL_LINE_GRACE_MS"
	cursorAllowBuiltinSkillsEnv = "TERM_LLM_CURSOR_ALLOW_BUILTIN_SKILLS"
	cursorUserHomeDir           = "user-home"
	cursorManagedSkillsDir      = "skills-cursor"
)

// Longer effort suffixes must come first so "extra-high" is not parsed as "high".
var cursorEffortLevels = []string{"extra-high", "none", "minimal", "medium", "xhigh", "high", "low", "max"}

var cursorToolDrainGrace = loadCLIToolLineDrainGrace(cursorToolLineGraceEnv, cursorToolLineGrace)

type CursorBinProvider struct {
	model, effort string
	fast          bool
	extraEnv      map[string]string
	realHome      string

	cursorHome   string
	sessionID    string
	messagesSent int

	toolExecutorConfigured bool
	mcpServer              *mcphttp.Server
	mcpURL, mcpToken       string
	cliToolBridgeState
	tempFileTracker

	active atomic.Bool
}

type cursorBinProviderState struct {
	CursorHome   string `json:"cursor_home"`
	SessionID    string `json:"session_id"`
	MessagesSent int    `json:"messages_sent"`
}

type cursorStreamState struct {
	sessionID      string
	sawResult      bool
	sawTextDelta   bool
	fallbackText   string
	reasoningItem  int
	usage          *Usage
	providerErr    error
	resultIsError  bool
	resultErrorMsg string
}

type cursorStreamMessage struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype"`
	Text        string          `json:"text"`
	SessionID   string          `json:"session_id"`
	ModelCallID json.RawMessage `json:"model_call_id"`
	TimestampMS *int64          `json:"timestamp_ms"`
	IsError     bool            `json:"is_error"`
	Result      string          `json:"result"`
	Message     struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	Usage struct {
		InputTokens      int `json:"inputTokens"`
		OutputTokens     int `json:"outputTokens"`
		CacheReadTokens  int `json:"cacheReadTokens"`
		CacheWriteTokens int `json:"cacheWriteTokens"`
	} `json:"usage"`
}

func parseCursorModel(value string) (model, effort string, fast bool) {
	model = strings.TrimSpace(value)
	if strings.HasSuffix(model, "-fast") {
		model = strings.TrimSuffix(model, "-fast")
		fast = true
	}
	for _, candidate := range cursorEffortLevels {
		suffix := "-" + candidate
		if strings.HasSuffix(model, suffix) && len(model) > len(suffix) {
			return strings.TrimSuffix(model, suffix), candidate, fast
		}
	}
	return model, "", fast
}

func cursorModelArgument(model, effort string, fast bool) string {
	model = strings.TrimSpace(model)
	if model == "" || model == "auto-smart" {
		return "auto"
	}
	if model == "grok-4.5" && effort == "" {
		// Cursor no longer exposes an effort-less Grok 4.5 model ID. Keep the
		// convenient base alias and map it to the catalog's default variant.
		effort = "high"
	}
	if strings.HasPrefix(model, "grok-") && effort != "" {
		// Model discovery removes Cursor's wire-only prefix from Grok IDs. Add
		// it back when selecting a concrete effort variant.
		model = "cursor-" + model
	}
	if effort != "" {
		model += "-" + effort
	}
	if fast {
		model += "-fast"
	}
	return model
}

func ValidateCursorBinModel(model string) error {
	for _, effort := range cursorEffortLevels {
		if model == effort {
			return fmt.Errorf("cursor-bin model %q is an effort level, not a model", model)
		}
	}
	return nil
}

func NewCursorBinProvider(model string, env map[string]string) *CursorBinProvider {
	model, effort, fast := parseCursorModel(model)
	realHome, _ := os.UserHomeDir()
	p := &CursorBinProvider{model: model, effort: effort, fast: fast, realHome: realHome}
	p.tempFileTracker.logName = "cursor-bin"
	p.SetEnv(env)
	return p
}

func (p *CursorBinProvider) Name() string {
	model := p.model
	if model == "" {
		model = "auto-smart"
	}
	details := model
	if p.effort != "" {
		details += ", effort=" + p.effort
	}
	if p.fast {
		details += ", fast"
	}
	return "Cursor CLI (" + details + ")"
}

func (p *CursorBinProvider) Credential() string { return "cursor-bin" }

func (p *CursorBinProvider) Capabilities() Capabilities {
	return Capabilities{
		ToolCalls:               true,
		ManagesOwnContext:       true,
		InlineToolLoop:          true,
		OrderedInlineToolEvents: true,
	}
}

// RequestInlineFlush marks the next tool result so Cursor Agent ends its
// current prompt. The engine then starts a new Stream that delivers queued
// interjections.
func (p *CursorBinProvider) RequestInlineFlush() {
	p.requestInlineFlush()
}

// SupportsInlineFlush reports that cursor-bin can stop its inline tool loop at
// a tool-result boundary.
func (p *CursorBinProvider) SupportsInlineFlush() bool { return true }

func (p *CursorBinProvider) formatToolOutput(output ToolOutput) string {
	return p.appendInlineFlushNotice(formatToolOutputForGrok(output))
}

func (p *CursorBinProvider) SetEnv(env map[string]string) {
	p.extraEnv = nil
	if len(env) == 0 {
		return
	}
	p.extraEnv = make(map[string]string, len(env))
	for key, value := range env {
		p.extraEnv[key] = value
	}
}

func (p *CursorBinProvider) SetToolExecutor(executor func(context.Context, string, json.RawMessage) (ToolOutput, error)) {
	p.toolExecutorConfigured = executor != nil
}

func (p *CursorBinProvider) ResetConversation() {
	p.sessionID = ""
	p.messagesSent = 0
}

func (p *CursorBinProvider) ExportProviderState() ([]byte, bool) {
	if p.cursorHome == "" || p.sessionID == "" {
		return nil, false
	}
	data, err := json.Marshal(cursorBinProviderState{CursorHome: p.cursorHome, SessionID: p.sessionID, MessagesSent: p.messagesSent})
	return data, err == nil
}

func (p *CursorBinProvider) ImportProviderState(data []byte) error {
	var state cursorBinProviderState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode cursor-bin provider state: %w", err)
	}
	if state.SessionID == "" || state.MessagesSent < 0 {
		return fmt.Errorf("decode cursor-bin provider state: invalid session state")
	}
	home, existed, err := validateCursorHomeState(state.CursorHome)
	if err != nil {
		return fmt.Errorf("decode cursor-bin provider state: %w", err)
	}
	hadData := false
	if info, err := os.Stat(filepath.Join(home, "data")); err == nil && info.IsDir() {
		hadData = true
	}
	if err := ensureCursorHomeLayout(home); err != nil {
		return err
	}
	if !existed || !hadData {
		state.SessionID, state.MessagesSent = "", 0
	}
	p.cursorHome, p.sessionID, p.messagesSent = home, state.SessionID, state.MessagesSent
	p.touchCursorHome()
	return nil
}

func (p *CursorBinProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		p.activeRuns.Add(1)
		defer p.finishStreamCleanup()
		if !p.active.CompareAndSwap(false, true) {
			return fmt.Errorf("cursor-bin provider already has an active stream")
		}
		defer p.active.Store(false)

		home, cleanupHome, err := p.homeForRequest(req.Ephemeral)
		if err != nil {
			return err
		}
		defer cleanupHome()

		messages, err := p.messagesForRequest(req)
		if err != nil {
			return err
		}
		exposeBridge := false
		if len(req.Tools) > 0 {
			if !p.toolExecutorConfigured {
				slog.Warn("cursor-bin tools requested but no tool executor configured", "tool_count", len(req.Tools))
			} else if err := p.ensureMCPServer(ctx, req.Tools, req.Debug || req.DebugRaw); err != nil {
				return err
			} else {
				exposeBridge = true
			}
		}
		if err := p.writeCursorMCPConfig(home, exposeBridge); err != nil {
			return err
		}

		prompt := buildCursorPrompt(messages)
		if strings.TrimSpace(prompt) == "" {
			return fmt.Errorf("build cursor-bin prompt: no user-visible content")
		}
		imagePaths := p.cursorImagePaths(messages)
		resumeID := ""
		if !req.Ephemeral {
			resumeID = p.sessionID
		}
		args := p.buildCursorArgs(req, resumeID, imagePaths, home)
		state, err := p.runCursorCommand(ctx, args, prompt, home, req.Debug || req.DebugRaw, send, exposeBridge)
		if err != nil {
			return err
		}
		if !req.Ephemeral {
			if state.sessionID != "" {
				p.sessionID = state.sessionID
			}
			p.messagesSent = len(req.Messages)
			p.touchCursorHome()
		}
		return send.Send(Event{Type: EventDone})
	}), nil
}

func (p *CursorBinProvider) messagesForRequest(req Request) ([]Message, error) {
	if req.Ephemeral || p.sessionID == "" {
		return req.Messages, nil
	}
	if p.messagesSent > len(req.Messages) {
		p.ResetConversation()
		return req.Messages, nil
	}
	if p.messagesSent == len(req.Messages) {
		return nil, fmt.Errorf("cursor-bin resumed session has no new messages to send")
	}
	if p.messagesSent > 0 {
		messages := grokResumeMessages(req.Messages[p.messagesSent:])
		if len(messages) == 0 {
			return nil, fmt.Errorf("cursor-bin resumed session has no new messages to send")
		}
		return messages, nil
	}
	return req.Messages, nil
}

func buildCursorPrompt(messages []Message) string {
	var parts []string
	if system := extractSystemPrompt(messages); system != "" {
		parts = append(parts, "<system>\n"+system+"\n</system>")
	}
	if conversation := buildCLIConversationPrompt(messages, renderGrokConversationParts); conversation != "" {
		parts = append(parts, conversation)
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func (p *CursorBinProvider) cursorImagePaths(messages []Message) []string {
	var paths []string
	for _, message := range messages {
		for _, part := range message.Parts {
			if part.Type == PartImage {
				path := strings.TrimSpace(part.ImagePath)
				if path == "" && part.ImageData != nil && part.ImageData.Base64 != "" {
					path = p.imageDataToTempFile(part.ImageData.MediaType, part.ImageData.Base64)
				}
				if path != "" {
					paths = append(paths, path)
				}
			}
			if part.Type == PartToolResult && part.ToolResult != nil {
				for _, content := range toolResultContentParts(part.ToolResult) {
					mediaType, data, ok := toolResultImageData(content)
					if ok {
						if path := p.imageDataToTempFile(mediaType, data); path != "" {
							paths = append(paths, path)
						}
					}
				}
			}
		}
	}
	return paths
}

func (p *CursorBinProvider) buildCursorArgs(req Request, resumeID string, imagePaths []string, home string) []string {
	model := chooseModel(req.Model, p.model)
	model, requestEffort, requestFast := parseCursorModel(model)
	effort := p.effort
	if requestEffort != "" {
		effort = requestEffort
	}
	if req.ReasoningEffort != "" {
		effort = req.ReasoningEffort
	}
	fast := p.fast || requestFast
	if req.ServiceTierSet {
		fast = NormalizeServiceTier(req.ServiceTier) == ServiceTierFast
	}
	args := []string{
		"--workspace", filepath.Join(home, "cwd"),
		"--trust",
		"--approve-mcps",
		"--force",
		"--allowed-tools", cursorAllowedTools,
		"--print",
		"--output-format", "stream-json",
		"--stream-partial-output",
		"--model", cursorModelArgument(model, effort, fast),
	}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	for _, path := range imagePaths {
		args = append(args, "--image", path)
	}
	return args
}

func (p *CursorBinProvider) runCursorCommand(ctx context.Context, args []string, prompt, home string, debug bool, send eventSender, exposeBridge bool) (*cursorStreamState, error) {
	neutralCWD := filepath.Join(home, "cwd")
	if debug {
		fmt.Fprintf(os.Stderr, "[cursor-bin] starting: cursor-agent %s\n", shellJoin(args))
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := newCLICommand(runCtx, "cursor-agent", args, neutralCWD)
	if err != nil {
		return nil, fmt.Errorf("prepare Cursor command: %w", err)
	}
	cmd.Env = p.buildCommandEnv(home)
	auditedEnv, wireAudit, err := startCLIWireAudit("cursor-bin", cmd.Env)
	if err != nil {
		return nil, err
	}
	cmd.Env = auditedEnv
	defer stopCLIWireAudit(wireAudit)
	cmd.Stdin = strings.NewReader(prompt)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("get Cursor stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("get Cursor stderr: %w", err)
	}
	cleanupProcess, err := procutil.PrepareCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("prepare Cursor process: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cleanupProcess()
		return nil, fmt.Errorf("start Cursor command: %w", err)
	}
	defer cleanupProcess()

	redactDiagnostic := p.cursorDiagnosticRedactor()
	lineCh := make(chan string, 64)
	scanErrCh := make(chan error, 1)
	stderrDone := make(chan struct{})
	var stderrMu sync.Mutex
	var stderrTail []string
	var stdoutMu sync.Mutex
	var stdoutTail []string
	go func() {
		defer close(lineCh)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64<<10), 16<<20)
		for scanner.Scan() {
			line := scanner.Text()
			recordCLITailLine(&stdoutMu, &stdoutTail, redactDiagnostic(line), cursorStdoutTailMaxLines)
			select {
			case lineCh <- line:
			case <-runCtx.Done():
				scanErrCh <- runCtx.Err()
				return
			}
		}
		scanErrCh <- scanner.Err()
	}()
	go func() {
		defer close(stderrDone)
		_ = drainCLIDiagnosticLines(stderr, func(rawLine string) {
			line := redactDiagnostic(rawLine)
			if debug {
				fmt.Fprintf(os.Stderr, "[cursor stderr] %s\n", line)
			}
			recordCLITailLine(&stderrMu, &stderrTail, line, cursorStderrTailMaxLines)
		})
	}()

	bridge := &cliTurnBridge{toolReqCh: make(chan cliToolRequest, 64), done: make(chan struct{})}
	if exposeBridge {
		p.cliToolBridgeState.activate(bridge, send.ch)
		defer p.cliToolBridgeState.deactivate(bridge)
	}
	defer close(bridge.done)

	state := &cursorStreamState{reasoningItem: 1}
	dispatchErr := p.dispatchCursorEvents(ctx, lineCh, bridge.toolReqCh, send, state)
	if dispatchErr != nil {
		cancel()
	}
	scanErr := <-scanErrCh
	if scanErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	<-stderrDone
	if dispatchErr != nil {
		return nil, dispatchErr
	}
	if scanErr != nil {
		return nil, fmt.Errorf("read Cursor output: %w", scanErr)
	}
	if waitErr != nil {
		return nil, p.cursorCommandError(waitErr, snapshotCLITail(&stderrMu, stderrTail), snapshotCLITail(&stdoutMu, stdoutTail))
	}
	if state.providerErr != nil {
		return nil, state.providerErr
	}
	if !state.sawResult {
		return nil, fmt.Errorf("Cursor command ended without a result event")
	}
	if state.resultIsError {
		return nil, fmt.Errorf("Cursor command failed: %s", strings.TrimSpace(state.resultErrorMsg))
	}
	if !state.sawTextDelta && state.fallbackText != "" {
		if err := send.Send(Event{Type: EventTextDelta, Text: state.fallbackText}); err != nil {
			return nil, err
		}
	}
	if state.usage != nil {
		if err := send.Send(Event{Type: EventUsage, Use: state.usage}); err != nil {
			return nil, err
		}
	}
	return state, nil
}

func (p *CursorBinProvider) dispatchCursorEvents(ctx context.Context, lineCh <-chan string, toolReqCh <-chan cliToolRequest, send eventSender, state *cursorStreamState) error {
	return dispatchCLILines(ctx, lineCh, toolReqCh, send, cursorToolDrainGrace, func(line string) error {
		return handleCursorStreamLine(line, send, state)
	})
}

func handleCursorStreamLine(line string, send eventSender, state *cursorStreamState) error {
	var message cursorStreamMessage
	if err := json.Unmarshal([]byte(line), &message); err != nil {
		return fmt.Errorf("decode Cursor stream event: %w", err)
	}
	if message.SessionID != "" {
		state.sessionID = message.SessionID
	}
	switch message.Type {
	case "thinking":
		if message.Subtype == "delta" && message.Text != "" {
			return send.Send(Event{Type: EventReasoningDelta, Text: message.Text, ReasoningKind: ReasoningKindRaw, ReasoningItemID: fmt.Sprintf("cursor-thought-%d", state.reasoningItem)})
		}
	case "assistant":
		text := cursorMessageText(message)
		if text == "" {
			return nil
		}
		// Cursor repeats assistant text in buffered pre-tool and final flushes.
		// Field presence, rather than a non-empty model_call_id value, identifies
		// the pre-tool flush; some CLI versions emit the field as an empty string.
		if message.TimestampMS != nil && len(message.ModelCallID) == 0 {
			state.sawTextDelta = true
			return send.Send(Event{Type: EventTextDelta, Text: text})
		}
		if message.TimestampMS == nil {
			state.fallbackText = text
		}
	case "tool_call":
		if message.Subtype == "started" {
			state.reasoningItem++
		}
	case "result":
		state.sawResult = true
		state.resultIsError = message.IsError || message.Subtype != "success"
		state.resultErrorMsg = message.Result
		state.usage = &Usage{InputTokens: message.Usage.InputTokens, OutputTokens: message.Usage.OutputTokens, CachedInputTokens: message.Usage.CacheReadTokens, CacheWriteTokens: message.Usage.CacheWriteTokens}
	case "error":
		state.providerErr = errors.New(strings.TrimSpace(message.Result))
	}
	return nil
}

func cursorMessageText(message cursorStreamMessage) string {
	var parts []string
	for _, content := range message.Message.Content {
		if content.Type == "text" && content.Text != "" {
			parts = append(parts, content.Text)
		}
	}
	return strings.Join(parts, "")
}

func (p *CursorBinProvider) cursorCommandError(waitErr error, stderrTail, stdoutTail []string) error {
	return newCLIProcessError(waitErr, stderrTail, stdoutTail, cliCommandErrorOptions{
		name:        "Cursor",
		authTerms:   []string{"authentication", "login", "sign in"},
		authSummary: "Cursor Agent is not logged in",
		authDetail:  "Run `cursor-agent login` or set CURSOR_API_KEY.",
	})
}

func (p *CursorBinProvider) ensureMCPServer(ctx context.Context, tools []ToolSpec, debug bool) error {
	if p.mcpServer != nil {
		return nil
	}
	server := mcphttp.NewServer(p.cliToolBridgeState.wrappedExecutor(p.formatToolOutput))
	server.SetDebug(debug)
	url, token, err := server.Start(ctx, mcpToolSpecs(tools))
	if err != nil {
		return fmt.Errorf("start cursor-bin MCP server: %w", err)
	}
	p.mcpServer, p.mcpURL, p.mcpToken = server, url, token
	return nil
}

func (p *CursorBinProvider) writeCursorMCPConfig(home string, enabled bool) error {
	dir := filepath.Join(home, "cwd", ".cursor")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create cursor-bin project config: %w", err)
	}
	servers := map[string]any{}
	if enabled && p.mcpURL != "" {
		servers[cursorMCPServerName] = map[string]any{
			"url":     p.mcpURL,
			"headers": map[string]string{"Authorization": "Bearer " + p.mcpToken},
		}
	}
	data, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write cursor-bin MCP config: %w", err)
	}
	return os.Chmod(path, 0o600)
}

func (p *CursorBinProvider) CleanupMCP() {
	if p.mcpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), mcpStopTimeout)
		p.mcpServer.Stop(ctx)
		cancel()
		p.mcpServer, p.mcpURL, p.mcpToken = nil, "", ""
	}
	p.cleanupTempFiles()
}

func (p *CursorBinProvider) CleanupTurn() { p.cleanupTempFilesIfIdle() }

func (p *CursorBinProvider) buildCommandEnv(home string) []string {
	// Keep Cursor's real config dir so `cursor-agent login` credentials in
	// cli-config.json remain visible, but isolate everything Cursor discovers
	// through the OS home along with its session/project data.
	configDir := strings.TrimSpace(p.extraEnv["CURSOR_CONFIG_DIR"])
	if configDir == "" {
		configDir = cursorConfigDir()
	}
	userHome := filepath.Join(home, cursorUserHomeDir)
	forced := map[string]string{
		"CURSOR_CONFIG_DIR": configDir,
		"CURSOR_DATA_DIR":   filepath.Join(home, "data"),
		"HOME":              userHome,
		"USERPROFILE":       userHome,
	}
	out := make([]string, 0, len(os.Environ())+len(p.extraEnv)+len(forced))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, ok := forced[key]; ok {
			continue
		}
		if _, ok := p.extraEnv[key]; ok {
			continue
		}
		out = append(out, entry)
	}
	for key, value := range p.extraEnv {
		if _, forcedKey := forced[key]; !forcedKey {
			out = append(out, key+"="+value)
		}
	}
	for key, value := range forced {
		out = append(out, key+"="+value)
	}
	return out
}

func cursorConfigDir() string {
	if dir := strings.TrimSpace(os.Getenv("CURSOR_CONFIG_DIR")); dir != "" {
		return dir
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "cursor")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".cursor")
}

// CursorBinHasCredentials reports whether Cursor Agent can authenticate via
// CURSOR_API_KEY or a local `cursor-agent login` (cli-config.json authInfo).
func CursorBinHasCredentials() bool {
	if strings.TrimSpace(os.Getenv("CURSOR_API_KEY")) != "" {
		return true
	}
	dir := cursorConfigDir()
	if dir == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, "cli-config.json"))
	if err != nil {
		return false
	}
	var cfg struct {
		AuthInfo json.RawMessage `json:"authInfo"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false
	}
	return len(cfg.AuthInfo) > 0 && string(cfg.AuthInfo) != "null"
}

func (p *CursorBinProvider) cursorDiagnosticRedactor() func(string) string {
	var secrets []string
	for key, value := range p.extraEnv {
		if value != "" && redactEnvValue(key, value) == "[redacted]" {
			secrets = append(secrets, value)
		}
	}
	if key := strings.TrimSpace(os.Getenv("CURSOR_API_KEY")); key != "" {
		secrets = append(secrets, key)
	}
	if p.mcpToken != "" {
		secrets = append(secrets, p.mcpToken)
	}
	return func(text string) string {
		for _, secret := range secrets {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
		return text
	}
}

func (p *CursorBinProvider) effectiveEnv(key string) string {
	if value, ok := p.extraEnv[key]; ok {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(os.Getenv(key))
}

func (p *CursorBinProvider) prepareHomeCredentials(home string) error {
	privateHome := filepath.Join(home, cursorUserHomeDir)
	switch strings.ToLower(p.effectiveEnv("AGENT_CLI_CREDENTIAL_STORE")) {
	case "file":
		return prepareCursorFileCredentials(p.realHome, privateHome)
	case "memory":
		return nil
	default:
		return prepareCursorPlatformCredentials(p.realHome, privateHome)
	}
}

func (p *CursorBinProvider) homeForRequest(ephemeral bool) (string, func(), error) {
	if !ephemeral {
		if err := p.ensureCursorHome(); err != nil {
			return "", func() {}, err
		}
		if err := p.prepareHomeCredentials(p.cursorHome); err != nil {
			return "", func() {}, err
		}
		return p.cursorHome, func() {}, nil
	}
	home, err := os.MkdirTemp("", "term-llm-cursor-bin-")
	if err != nil {
		return "", func() {}, err
	}
	if err := ensureCursorHomeLayout(home); err != nil {
		_ = os.RemoveAll(home)
		return "", func() {}, err
	}
	if err := p.prepareHomeCredentials(home); err != nil {
		_ = os.RemoveAll(home)
		return "", func() {}, err
	}
	return home, func() { _ = os.RemoveAll(home) }, nil
}

func cursorBinCacheBase() (string, error) {
	cache := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME"))
	if cache == "" {
		var err error
		cache, err = os.UserCacheDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Abs(filepath.Join(cache, "term-llm", "cursor-bin"))
}

func (p *CursorBinProvider) ensureCursorHome() error {
	if p.cursorHome == "" {
		base, err := cursorBinCacheBase()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(base, 0o700); err != nil {
			return err
		}
		id, err := newGrokHomeID()
		if err != nil {
			return err
		}
		p.cursorHome = filepath.Join(base, id)
	} else {
		home, _, err := validateCursorHomeState(p.cursorHome)
		if err != nil {
			return err
		}
		p.cursorHome = home
	}
	if err := ensureCursorHomeLayout(p.cursorHome); err != nil {
		return err
	}
	p.touchCursorHome()
	p.gcCursorHomes()
	return nil
}

func ensureCursorHomeLayout(home string) error {
	userHome := filepath.Join(home, cursorUserHomeDir)
	for _, dir := range []string{home, filepath.Join(home, "data"), filepath.Join(home, "cwd"), userHome, filepath.Join(userHome, ".cursor")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create cursor-bin directory: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	return ensureCursorManagedSkillsPath(userHome, cursorBuiltinSkillsAllowed())
}

func cursorBuiltinSkillsAllowed() bool {
	// Compatibility escape hatch for Cursor versions that make a failed managed
	// skill sync fatal. Isolation remains the default so Cursor cannot add a
	// second, provider-owned instruction layer to term-llm conversations.
	switch strings.ToLower(strings.TrimSpace(os.Getenv(cursorAllowBuiltinSkillsEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func ensureCursorManagedSkillsPath(userHome string, allow bool) error {
	// Cursor synchronizes its built-in skills into this path before discovery.
	// An empty regular file makes that sync and the subsequent directory walk
	// fail harmlessly, which keeps Cursor's managed instructions off the wire.
	path := filepath.Join(userHome, ".cursor", cursorManagedSkillsDir)
	info, err := os.Lstat(path)
	if allow {
		switch {
		case os.IsNotExist(err):
			return nil
		case err != nil:
			return fmt.Errorf("inspect cursor-bin managed skills path: %w", err)
		case info.Mode().IsRegular():
			if err := os.Chmod(path, 0o600); err != nil {
				return fmt.Errorf("prepare Cursor managed skills sentinel removal: %w", err)
			}
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("enable Cursor managed skills: %w", err)
			}
			return nil
		case info.IsDir():
			return nil
		default:
			return fmt.Errorf("cursor-bin managed skills path is not a safe file or directory")
		}
	}

	if err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("cursor-bin managed skills path is not a safe regular file")
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("prepare cursor-bin managed skills sentinel: %w", err)
		}
		if err := os.WriteFile(path, nil, 0o400); err != nil {
			return fmt.Errorf("reset cursor-bin managed skills sentinel: %w", err)
		}
		return os.Chmod(path, 0o400)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("inspect cursor-bin managed skills path: %w", err)
	}
	if err := os.WriteFile(path, nil, 0o400); err != nil {
		return fmt.Errorf("create cursor-bin managed skills sentinel: %w", err)
	}
	return os.Chmod(path, 0o400)
}

func validateCursorHomeState(home string) (string, bool, error) {
	base, err := cursorBinCacheBase()
	if err != nil {
		return "", false, err
	}
	candidate, err := filepath.Abs(strings.TrimSpace(home))
	if err != nil {
		return "", false, err
	}
	candidate = filepath.Clean(candidate)
	if filepath.Dir(candidate) != filepath.Clean(base) || !isGrokHomeID(filepath.Base(candidate)) {
		return "", false, fmt.Errorf("cursor_home must be directly under %s", base)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", false, err
	}
	existed := true
	info, err := os.Lstat(candidate)
	if os.IsNotExist(err) {
		existed = false
		err = os.Mkdir(candidate, 0o700)
		info, _ = os.Lstat(candidate)
	}
	if err != nil {
		return "", false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", false, fmt.Errorf("cursor_home is not a safe directory")
	}
	return candidate, existed, nil
}

func (p *CursorBinProvider) touchCursorHome() {
	if p.cursorHome != "" {
		_ = os.WriteFile(filepath.Join(p.cursorHome, ".last_used"), []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o600)
	}
}

func (p *CursorBinProvider) gcCursorHomes() {
	base, err := cursorBinCacheBase()
	if err != nil {
		return
	}
	gcStaleCLIHomes(base, p.cursorHome, cursorHomeMaxAge, isGrokHomeID, "cursor-bin")
}

func defaultCursorModelsCommandOutput(ctx context.Context, p *CursorBinProvider, home string) ([]byte, error) {
	cmd, err := newCLICommand(ctx, "cursor-agent", []string{"models"}, filepath.Join(home, "cwd"))
	if err != nil {
		return nil, err
	}
	cmd.Env = p.buildCommandEnv(home)
	auditedEnv, wireAudit, err := startCLIWireAudit("cursor-bin-models", cmd.Env)
	if err != nil {
		return nil, err
	}
	cmd.Env = auditedEnv
	defer stopCLIWireAudit(wireAudit)
	return cmd.Output()
}

var cursorModelsCommandOutput = defaultCursorModelsCommandOutput

// ListModels parses the account-specific model list exposed by Cursor Agent.
func (p *CursorBinProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	home, err := os.MkdirTemp("", "term-llm-cursor-models-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	if err := ensureCursorHomeLayout(home); err != nil {
		return nil, err
	}
	if err := p.prepareHomeCredentials(home); err != nil {
		return nil, err
	}
	output, err := cursorModelsCommandOutput(ctx, p, home)
	if err != nil {
		return nil, fmt.Errorf("list Cursor models: %w", err)
	}
	var models []ModelInfo
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(output), "\n") {
		id, name, ok := strings.Cut(strings.TrimSpace(line), " - ")
		if !ok || id == "" || name == "" {
			continue
		}
		id = normalizeCursorListedModelID(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, ModelInfo{ID: id, DisplayName: name, OwnedBy: "cursor", InputPrice: -1, OutputPrice: -1})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("list Cursor models: no models found")
	}
	RefreshCursorBinCacheSync(models)
	return models, nil
}

var _ Provider = (*CursorBinProvider)(nil)
var _ ToolExecutorSetter = (*CursorBinProvider)(nil)
var _ ProviderCleaner = (*CursorBinProvider)(nil)
var _ ProviderTurnCleaner = (*CursorBinProvider)(nil)
var _ ProviderStateExporter = (*CursorBinProvider)(nil)
var _ ProviderStateImporter = (*CursorBinProvider)(nil)

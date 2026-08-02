package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/mcphttp"
	"github.com/samsaffron/term-llm/internal/procutil"
)

const (
	agyMCPServerName      = "term-llm"
	agyStderrTailMaxLines = 40
	agyStdoutTailMaxLines = 40
	agyToolLineGrace      = 75 * time.Millisecond
)

var agyToolDrainGrace = loadCLIToolLineDrainGrace("TERM_LLM_AGY_TOOL_LINE_GRACE_MS", agyToolLineGrace)

type AgyBinProvider struct {
	model                  string
	extraEnv               map[string]string
	realHome               string
	agyHome                string
	agyBinary              string
	toolExecutorConfigured bool
	mcpServer              *mcphttp.Server
	mcpURL, mcpToken       string
	isolation              agyToolIsolation
	cliToolBridgeState
	tempFileTracker
	active atomic.Bool
}

type agyStreamState struct {
	conversationID string
	sawResult      bool
	sawText        bool
	fallbackText   string
	usage          *Usage
	providerErr    error
	pathReplacer   *strings.Replacer
}

type agyStreamEvent struct {
	Event          string `json:"event"`
	ConversationID string `json:"conversation_id"`
	StepUpdate     struct {
		ConversationID string `json:"conversation_id"`
		State          string `json:"state"`
		StepType       string `json:"step_type"`
		TextDelta      string `json:"text_delta"`
	} `json:"step_update"`
	Result struct {
		ConversationID string   `json:"conversation_id"`
		Status         string   `json:"status"`
		Response       string   `json:"response"`
		Error          string   `json:"error"`
		Usage          agyUsage `json:"usage"`
	} `json:"result"`
}

type agyUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ThinkingTokens  int `json:"thinking_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
}

func NewAgyBinProvider(model string, env map[string]string) *AgyBinProvider {
	home, _ := os.UserHomeDir()
	p := &AgyBinProvider{model: strings.TrimSpace(model), realHome: home, isolation: newAgyToolIsolation()}
	p.tempFileTracker.logName = "agy-bin"
	p.SetEnv(env)
	return p
}

func (p *AgyBinProvider) resolveBinary() error {
	path, err := exec.LookPath("agy")
	if err != nil {
		return fmt.Errorf("locate agy binary: %w", err)
	}
	p.agyBinary = path
	return nil
}

func ValidateAgyBinModel(model string) error {
	if strings.ContainsAny(model, "\r\n") {
		return fmt.Errorf("agy-bin model contains a newline")
	}
	return nil
}

func (p *AgyBinProvider) Name() string {
	if p.model == "" {
		return "agy CLI"
	}
	return "agy CLI (" + p.model + ")"
}
func (p *AgyBinProvider) Credential() string { return "agy-bin" }
func (p *AgyBinProvider) Capabilities() Capabilities {
	return Capabilities{ToolCalls: true, InlineToolLoop: true, OrderedInlineToolEvents: true}
}
func (p *AgyBinProvider) SetEnv(env map[string]string) {
	p.extraEnv = nil
	if len(env) == 0 {
		return
	}
	p.extraEnv = make(map[string]string, len(env))
	for k, v := range env {
		p.extraEnv[k] = v
	}
}
func (p *AgyBinProvider) SetToolExecutor(executor func(context.Context, string, json.RawMessage) (ToolOutput, error)) {
	p.toolExecutorConfigured = executor != nil
}
func (p *AgyBinProvider) ResetConversation() {}

func (p *AgyBinProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		p.activeRuns.Add(1)
		defer p.finishStreamCleanup()
		if !p.active.CompareAndSwap(false, true) {
			return errors.New("agy-bin provider already has an active stream")
		}
		defer p.active.Store(false)
		// agy-bin does not resume vendor conversations; keep the temporary home,
		// MCP listener, and compatibility proxy scoped to this single Stream.
		defer p.cleanupRuntime()
		if err := p.resolveBinary(); err != nil {
			return err
		}
		if err := p.ensureHome(); err != nil {
			return err
		}
		exposeBridge := false
		if len(req.Tools) > 0 {
			if !p.toolExecutorConfigured {
				slog.Warn("agy-bin tools requested but no tool executor configured", "tool_count", len(req.Tools))
			} else if err := p.ensureMCPServer(ctx, req.Tools, req.Debug || req.DebugRaw); err != nil {
				return err
			} else {
				exposeBridge = true
			}
		}
		if err := p.writeMCPConfigs(exposeBridge); err != nil {
			return err
		}
		if err := p.isolation.EnsureStarted(); err != nil {
			return fmt.Errorf("start agy native-tool isolation: %w", err)
		}
		p.isolation.BeginTurn(exposeBridge)
		workspaceDir := agyWorkspaceDir(req.WorkingDir)
		prompt := buildAgyPrompt(req.Messages, workspaceDir)
		if strings.TrimSpace(prompt) == "" {
			return errors.New("build agy-bin prompt: no user-visible content")
		}
		state, err := p.runCommand(ctx, p.buildArgs(req), prompt, workspaceDir, req.Debug || req.DebugRaw, send, exposeBridge)
		if err != nil {
			return err
		}
		if p.isolation.FilteredGenerations() == 0 {
			return errors.New("agy did not route generation through the term-llm tool-filter proxy; refusing unverified turn")
		}
		_ = state
		return send.Send(Event{Type: EventDone})
	}), nil
}

func agyWorkspaceDir(configured string) string {
	dir := strings.TrimSpace(configured)
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if dir == "" {
		return ""
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	return filepath.Clean(absolute)
}

func agyPathReplacer(home, workspace string) *strings.Replacer {
	if home == "" || workspace == "" {
		return nil
	}
	var pairs []string
	for _, temporary := range []string{
		filepath.Join(home, ".gemini", "antigravity-cli", "scratch"),
		filepath.Join(home, "cwd"),
	} {
		temporary = filepath.Clean(temporary)
		temporarySlash := filepath.ToSlash(temporary)
		workspaceSlash := filepath.ToSlash(workspace)
		pairs = append(pairs,
			"file://"+temporarySlash+"/", "file://"+workspaceSlash+"/",
			temporary+string(filepath.Separator), workspace+string(filepath.Separator),
		)
	}
	return strings.NewReplacer(pairs...)
}

func (s *agyStreamState) rewritePaths(text string) string {
	if s.pathReplacer == nil || text == "" {
		return text
	}
	return s.pathReplacer.Replace(text)
}

func buildAgyPrompt(messages []Message, workspaceDir string) string {
	var parts []string
	if workspaceDir != "" {
		parts = append(parts, "<workspace>\nThe user's actual workspace is "+workspaceDir+". Resolve relative file paths against this workspace. Never present paths or file links from the temporary Antigravity scratch or home directories; links to workspace files must use the actual workspace path.\n</workspace>")
	}
	if system := extractSystemPrompt(messages); system != "" {
		parts = append(parts, "<system>\n"+system+"\n</system>")
	}
	if conversation := buildCLIConversationPrompt(messages, renderGrokConversationParts); conversation != "" {
		parts = append(parts, conversation)
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func (p *AgyBinProvider) buildArgs(req Request) []string {
	args := []string{"--dangerously-skip-permissions", "--disable-slash-commands", "--output-format", "stream-json", "--print-timeout", "10m"}
	model := strings.TrimSpace(chooseModel(req.Model, p.model))
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}

func (p *AgyBinProvider) runCommand(ctx context.Context, args []string, prompt, workspaceDir string, debug bool, send eventSender, exposeBridge bool) (*agyStreamState, error) {
	// Keep agy out of the real workspace so it cannot auto-load workspace-local
	// agents or MCP configuration. Antigravity consequently describes its own
	// disposable scratch directory in some generated file links; output path
	// rewriting maps those links back to workspaceDir before term-llm emits them.
	neutralCWD := filepath.Join(p.agyHome, "cwd")
	fullArgs := append(append(append([]string{}, args...), "--print"), prompt)
	if debug {
		fmt.Fprintf(os.Stderr, "[agy-bin] starting agy with model %q\n", chooseModel("", p.model))
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := newCLICommand(runCtx, p.agyBinary, fullArgs, neutralCWD)
	if err != nil {
		return nil, fmt.Errorf("prepare agy command: %w", err)
	}
	cmd.Env = p.buildCommandEnv()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("get agy stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("get agy stderr: %w", err)
	}
	cleanup, err := procutil.PrepareCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("prepare agy process: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("start agy command: %w", err)
	}
	defer cleanup()
	lineCh := make(chan string, 64)
	scanErrCh := make(chan error, 1)
	stderrDone := make(chan struct{})
	var stderrMu, stdoutMu sync.Mutex
	var stderrTail, stdoutTail []string
	redact := p.diagnosticRedactor(prompt)
	go func() {
		defer close(lineCh)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			line := sc.Text()
			recordCLITailLine(&stdoutMu, &stdoutTail, redact(line), agyStdoutTailMaxLines)
			select {
			case lineCh <- line:
			case <-runCtx.Done():
				scanErrCh <- runCtx.Err()
				return
			}
		}
		scanErrCh <- sc.Err()
	}()
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := redact(sc.Text())
			if debug {
				fmt.Fprintf(os.Stderr, "[agy stderr] %s\n", line)
			}
			recordCLITailLine(&stderrMu, &stderrTail, line, agyStderrTailMaxLines)
		}
	}()
	bridge := &cliTurnBridge{toolReqCh: make(chan cliToolRequest, 64), done: make(chan struct{})}
	if exposeBridge {
		p.cliToolBridgeState.activate(bridge, send.ch)
		defer p.cliToolBridgeState.deactivate(bridge)
	}
	defer close(bridge.done)
	state := &agyStreamState{pathReplacer: agyPathReplacer(p.agyHome, workspaceDir)}
	dispatchErr := p.dispatchEvents(ctx, lineCh, bridge.toolReqCh, send, state)
	if dispatchErr != nil {
		cancel()
	}
	scanErr := <-scanErrCh
	<-stderrDone
	waitErr := cmd.Wait()
	if dispatchErr != nil {
		return nil, dispatchErr
	}
	if scanErr != nil {
		return nil, fmt.Errorf("read agy output: %w", scanErr)
	}
	if waitErr != nil {
		return nil, p.commandError(waitErr, snapshotCLITail(&stderrMu, stderrTail), snapshotCLITail(&stdoutMu, stdoutTail))
	}
	if state.providerErr != nil {
		return nil, state.providerErr
	}
	if !state.sawResult {
		return nil, errors.New("agy command ended without a result event")
	}
	if !state.sawText && state.fallbackText != "" {
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

func (p *AgyBinProvider) dispatchEvents(ctx context.Context, lineCh <-chan string, toolReqCh <-chan cliToolRequest, send eventSender, state *agyStreamState) error {
	linesOpen := true
	handle := func(line string) error { return handleAgyStreamLine(line, send, state) }
	for linesOpen {
		had := false
		for linesOpen {
			select {
			case line, ok := <-lineCh:
				if !ok {
					linesOpen = false
					break
				}
				had = true
				if err := handle(line); err != nil {
					return err
				}
			default:
				goto drained
			}
		}
	drained:
		if had {
			continue
		}
		select {
		case line, ok := <-lineCh:
			if !ok {
				linesOpen = false
				continue
			}
			if err := handle(line); err != nil {
				return err
			}
		case request := <-toolReqCh:
			if err := drainCLILinesWithGrace(ctx, lineCh, agyToolDrainGrace, handle); err != nil {
				return err
			}
			handleCLIToolRequest(request, send)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for {
		select {
		case request := <-toolReqCh:
			handleCLIToolRequest(request, send)
		default:
			return nil
		}
	}
}

func handleAgyStreamLine(line string, send eventSender, state *agyStreamState) error {
	if !strings.HasPrefix(strings.TrimSpace(line), "{") {
		return nil
	}
	var message agyStreamEvent
	if err := json.Unmarshal([]byte(line), &message); err != nil {
		return fmt.Errorf("decode agy stream event: %w", err)
	}
	switch message.Event {
	case "init":
		if message.ConversationID != "" {
			state.conversationID = message.ConversationID
		}
	case "step_update":
		if message.StepUpdate.ConversationID != "" {
			state.conversationID = message.StepUpdate.ConversationID
		}
		if message.StepUpdate.StepType == "agent_response" && message.StepUpdate.TextDelta != "" {
			state.sawText = true
			return send.Send(Event{Type: EventTextDelta, Text: state.rewritePaths(message.StepUpdate.TextDelta)})
		}
	case "result":
		state.sawResult = true
		if message.Result.ConversationID != "" {
			state.conversationID = message.Result.ConversationID
		}
		state.fallbackText = state.rewritePaths(message.Result.Response)
		state.usage = &Usage{InputTokens: message.Result.Usage.InputTokens, OutputTokens: message.Result.Usage.OutputTokens, CachedInputTokens: message.Result.Usage.CacheReadTokens, ReasoningTokens: message.Result.Usage.ThinkingTokens}
		if message.Result.Status != "SUCCESS" {
			detail := strings.TrimSpace(message.Result.Error)
			if detail == "" {
				detail = "agy returned status " + message.Result.Status
			}
			state.providerErr = errors.New(detail)
		}
	}
	return nil
}

func (p *AgyBinProvider) commandError(waitErr error, stderrTail, stdoutTail []string) error {
	diagnostic := firstUsefulCLIDiagnosticLine(strings.Join(stderrTail, "\n"))
	if diagnostic == "" {
		diagnostic = firstUsefulCLIDiagnosticLine(strings.Join(stdoutTail, "\n"))
	}
	lower := strings.ToLower(diagnostic)
	if strings.Contains(lower, "authentication") || strings.Contains(lower, "not logged") || strings.Contains(lower, "log in") {
		return &UserFacingProviderError{Summary: "agy is not logged in", Detail: "Run `agy` and complete Antigravity authentication.", Cause: waitErr}
	}
	if diagnostic != "" {
		return fmt.Errorf("agy command failed: %w: %s", waitErr, diagnostic)
	}
	return fmt.Errorf("agy command failed: %w", waitErr)
}

func (p *AgyBinProvider) ensureMCPServer(ctx context.Context, tools []ToolSpec, debug bool) error {
	if p.mcpServer != nil {
		return nil
	}
	server := mcphttp.NewServer(p.cliToolBridgeState.wrappedExecutor(formatToolOutputForGrok))
	server.SetDebug(debug)
	url, token, err := server.Start(ctx, mcpToolSpecs(tools))
	if err != nil {
		return fmt.Errorf("start agy-bin MCP server: %w", err)
	}
	p.mcpServer, p.mcpURL, p.mcpToken = server, url, token
	return nil
}

func (p *AgyBinProvider) ensureHome() error {
	if p.agyHome == "" {
		home, err := os.MkdirTemp("", "term-llm-agy-home-")
		if err != nil {
			return fmt.Errorf("create agy private home: %w", err)
		}
		p.agyHome = home
	}
	for _, dir := range []string{filepath.Join(p.agyHome, "cwd"), filepath.Join(p.agyHome, ".gemini", "config"), filepath.Join(p.agyHome, ".gemini", "antigravity"), filepath.Join(p.agyHome, ".gemini", "antigravity-cli")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create agy home layout: %w", err)
		}
	}
	return p.copyCredentials()
}

func (p *AgyBinProvider) copyCredentials() error {
	if p.realHome == "" {
		return errors.New("locate agy credentials: user home unavailable")
	}
	src := filepath.Join(p.realHome, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	data, fileErr := os.ReadFile(src)
	if fileErr == nil && len(data) == 0 {
		fileErr = errors.New("agy OAuth token file is empty")
	}
	if fileErr == nil {
		dst := filepath.Join(p.agyHome, ".gemini", "antigravity-cli", "antigravity-oauth-token")
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return fmt.Errorf("copy agy credentials: %w", err)
		}
		return os.Chmod(dst, 0o600)
	}

	prepared, err := prepareAgyPlatformCredentials(p.realHome, p.agyHome)
	if err != nil {
		return err
	}
	if prepared {
		return nil
	}
	return &UserFacingProviderError{Summary: "agy is not logged in", Detail: "Run `agy` and complete Antigravity authentication.", Cause: fileErr}
}

func (p *AgyBinProvider) writeMCPConfigs(enabled bool) error {
	servers := map[string]any{}
	if enabled {
		servers[agyMCPServerName] = map[string]any{"serverUrl": p.mcpURL, "headers": map[string]string{"Authorization": "Bearer " + p.mcpToken}}
	}
	data, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return err
	}
	for _, dir := range []string{"config", "antigravity", "antigravity-cli"} {
		path := filepath.Join(p.agyHome, ".gemini", dir, "mcp_config.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return fmt.Errorf("write agy MCP config: %w", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (p *AgyBinProvider) buildCommandEnv() []string {
	forced := map[string]string{"HOME": p.agyHome}
	for k, v := range p.isolation.Environment() {
		forced[k] = v
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
	for k, v := range p.extraEnv {
		if _, forcedKey := forced[k]; !forcedKey {
			out = append(out, k+"="+v)
		}
	}
	for k, v := range forced {
		out = append(out, k+"="+v)
	}
	return out
}

func (p *AgyBinProvider) diagnosticRedactor(prompt string) func(string) string {
	secrets := []string{prompt, p.mcpToken}
	for k, v := range p.extraEnv {
		if v != "" && redactEnvValue(k, v) == "[redacted]" {
			secrets = append(secrets, v)
		}
	}
	return func(text string) string {
		for _, secret := range secrets {
			if secret != "" {
				text = strings.ReplaceAll(text, secret, "[redacted]")
			}
		}
		return text
	}
}

func (p *AgyBinProvider) cleanupRuntime() {
	if p.mcpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), mcpStopTimeout)
		_ = p.mcpServer.Stop(ctx)
		cancel()
		p.mcpServer, p.mcpURL, p.mcpToken = nil, "", ""
	}
	if p.isolation != nil {
		ctx, cancel := context.WithTimeout(context.Background(), agyIsolationStopTimeout)
		_ = p.isolation.Stop(ctx)
		cancel()
	}
	if p.agyHome != "" {
		_ = os.RemoveAll(p.agyHome)
		p.agyHome = ""
	}
}

func (p *AgyBinProvider) CleanupMCP() {
	if p.active.Load() {
		return
	}
	p.cleanupRuntime()
	p.cleanupTempFiles()
}
func (p *AgyBinProvider) CleanupTurn() { p.cleanupTempFilesIfIdle() }

func AgyBinHasCredentials() bool {
	if _, err := exec.LookPath("agy"); err != nil {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	info, fileErr := os.Stat(filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"))
	if fileErr == nil && info.Size() > 0 {
		return true
	}
	return agyPlatformHasCredentials(home)
}

func (p *AgyBinProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	cmd, err := newCLICommand(ctx, "agy", []string{"models"}, "")
	if err != nil {
		return nil, err
	}
	cmd.Env = p.extraEnvList()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list agy models: %w", err)
	}
	var models []ModelInfo
	for _, line := range strings.Split(string(out), "\n") {
		id := strings.TrimSpace(line)
		if id != "" {
			models = append(models, ModelInfo{ID: id, DisplayName: id})
		}
	}
	return models, nil
}
func (p *AgyBinProvider) extraEnvList() []string {
	out := append([]string{}, os.Environ()...)
	for k, v := range p.extraEnv {
		out = append(out, k+"="+v)
	}
	return out
}

var _ Provider = (*AgyBinProvider)(nil)
var _ ToolExecutorSetter = (*AgyBinProvider)(nil)
var _ ProviderCleaner = (*AgyBinProvider)(nil)
var _ ProviderTurnCleaner = (*AgyBinProvider)(nil)

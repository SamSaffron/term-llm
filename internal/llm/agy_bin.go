package llm

import (
	"bufio"
	"context"
	"crypto/sha256"
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

	"github.com/samsaffron/term-llm/internal/cliwire"
	"github.com/samsaffron/term-llm/internal/mcphttp"
	"github.com/samsaffron/term-llm/internal/procutil"
)

const (
	agyMCPServerName      = "term-llm"
	agyStderrTailMaxLines = 40
	agyStdoutTailMaxLines = 40
	agyToolLineGrace      = 75 * time.Millisecond
	agyHomeMaxAge         = 30 * 24 * time.Hour
)

var agyToolDrainGrace = loadCLIToolLineDrainGrace("TERM_LLM_AGY_TOOL_LINE_GRACE_MS", agyToolLineGrace)

var errAgyConversationMismatch = errors.New("agy conversation continuation mismatch")

type AgyBinProvider struct {
	model                  string
	extraEnv               map[string]string
	realHome               string
	agyHome                string
	conversationID         string
	messagesSent           int
	transcriptHash         string
	stateMu                sync.Mutex
	agyBinary              string
	toolExecutorConfigured bool
	mcpServer              *mcphttp.Server
	mcpURL, mcpToken       string
	isolation              agyToolIsolation
	wireAudit              *cliwire.Server
	cliToolBridgeState
	tempFileTracker
	active atomic.Bool
}

type agyBinProviderState struct {
	AgyHome        string `json:"agy_home"`
	ConversationID string `json:"conversation_id"`
	MessagesSent   int    `json:"messages_sent"`
	TranscriptHash string `json:"transcript_hash"`
}

type agyStreamState struct {
	conversationID         string
	expectedConversationID string
	sawResult              bool
	sawText                bool
	fallbackText           string
	usage                  *Usage
	providerErr            error
	pathReplacer           *strings.Replacer
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

func validAgyConversationID(id string) bool {
	if id == "" || len(id) > 128 || id == "." || id == ".." || filepath.IsAbs(id) || filepath.Base(id) != id || strings.ContainsAny(id, "/\\\x00") {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (p *AgyBinProvider) Name() string {
	if p.model == "" {
		return "agy CLI"
	}
	return "agy CLI (" + p.model + ")"
}
func (p *AgyBinProvider) Credential() string { return "agy-bin" }
func (p *AgyBinProvider) Capabilities() Capabilities {
	return Capabilities{ToolCalls: true, ManagesOwnContext: true, InlineToolLoop: true, OrderedInlineToolEvents: true}
}

// RequestInlineFlush marks the next tool result so agy ends its current prompt.
// The engine then starts a new Stream that delivers queued steering.
func (p *AgyBinProvider) RequestInlineFlush() {
	p.requestInlineFlush()
}

// SupportsInlineFlush reports that agy-bin can stop its inline tool loop at a
// tool-result boundary.
func (p *AgyBinProvider) SupportsInlineFlush() bool { return true }

func (p *AgyBinProvider) formatToolOutput(output ToolOutput) string {
	return p.appendInlineFlushNotice(formatToolOutputForGrok(output))
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
func (p *AgyBinProvider) ResetConversation() {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	p.resetConversationLocked()
}

func (p *AgyBinProvider) resetConversationLocked() {
	p.conversationID = ""
	p.messagesSent = 0
	p.transcriptHash = ""
}

func (p *AgyBinProvider) ExportProviderState() ([]byte, bool) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if strings.TrimSpace(p.agyHome) == "" || strings.TrimSpace(p.conversationID) == "" || p.transcriptHash == "" {
		return nil, false
	}
	data, err := json.Marshal(agyBinProviderState{
		AgyHome:        p.agyHome,
		ConversationID: p.conversationID,
		MessagesSent:   p.messagesSent,
		TranscriptHash: p.transcriptHash,
	})
	return data, err == nil
}

func (p *AgyBinProvider) ImportProviderState(data []byte) error {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()

	var state agyBinProviderState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode agy-bin provider state: %w", err)
	}
	state.AgyHome = strings.TrimSpace(state.AgyHome)
	state.ConversationID = strings.TrimSpace(state.ConversationID)
	state.TranscriptHash = strings.TrimSpace(state.TranscriptHash)
	if state.AgyHome == "" || !validAgyConversationID(state.ConversationID) || state.MessagesSent < 0 || state.TranscriptHash == "" {
		return errors.New("decode agy-bin provider state: invalid session state")
	}

	home, existed, err := validateAgyHomeState(state.AgyHome)
	if err != nil {
		return fmt.Errorf("decode agy-bin provider state: %w", err)
	}
	if err := ensureAgyHomeLayout(home); err != nil {
		return fmt.Errorf("decode agy-bin provider state: restore home: %w", err)
	}
	conversationDB := filepath.Join(home, ".gemini", "antigravity-cli", "conversations", state.ConversationID+".db")
	if _, err := os.Stat(conversationDB); !existed || err != nil {
		state.ConversationID = ""
		state.MessagesSent = 0
		state.TranscriptHash = ""
	}

	p.agyHome = home
	p.conversationID = state.ConversationID
	p.messagesSent = state.MessagesSent
	p.transcriptHash = state.TranscriptHash
	p.touchAgyHome()
	p.gcAgyHomes()
	return nil
}

func (p *AgyBinProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		p.activeRuns.Add(1)
		defer p.finishStreamCleanup()
		if !p.active.CompareAndSwap(false, true) {
			return errors.New("agy-bin provider already has an active stream")
		}
		defer p.active.Store(false)
		p.stateMu.Lock()
		defer p.stateMu.Unlock()

		previousHome := p.agyHome
		if err := p.prepareHome(req.Ephemeral); err != nil {
			if req.Ephemeral {
				p.agyHome = previousHome
			}
			return err
		}
		defer func() {
			p.cleanupRuntime()
			if req.Ephemeral {
				ephemeralHome := p.agyHome
				p.agyHome = previousHome
				_ = os.RemoveAll(ephemeralHome)
			}
		}()
		if err := p.resolveBinary(); err != nil {
			return err
		}
		messages, err := p.messagesForRequest(req)
		if err != nil {
			return err
		}
		transcriptHash, err := agyTranscriptHash(req.Messages)
		if err != nil {
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
		wireAudit, err := startCLIWireServer("agy-bin", p.buildCommandEnv())
		if err != nil {
			return err
		}
		p.wireAudit = wireAudit
		if wireAudit != nil {
			p.isolation.SetUpstream(wireAudit.ProxyURL(), wireAudit.CAPath())
		} else {
			p.isolation.SetUpstream("", "")
		}
		if err := p.isolation.EnsureStarted(p.agyHome); err != nil {
			return fmt.Errorf("start agy native-tool isolation: %w", err)
		}
		p.isolation.BeginTurn(exposeBridge)
		workspaceDir := agyWorkspaceDir(req.WorkingDir)
		prompt := buildAgyPrompt(messages, workspaceDir)
		if strings.TrimSpace(prompt) == "" {
			return errors.New("build agy-bin prompt: no user-visible content")
		}
		resumeID := ""
		if !req.Ephemeral {
			resumeID = p.conversationID
		}
		state, err := p.runCommand(ctx, p.buildArgs(req, resumeID), prompt, workspaceDir, resumeID, req.Debug || req.DebugRaw, send, exposeBridge)
		if err != nil {
			if resumeID != "" && errors.Is(err, errAgyConversationMismatch) {
				p.resetConversationLocked()
			}
			return err
		}
		if err := p.requireFilteredGeneration(resumeID); err != nil {
			return err
		}
		if !req.Ephemeral && state.conversationID != "" {
			p.conversationID = state.conversationID
			p.messagesSent = len(req.Messages)
			p.transcriptHash = transcriptHash
			p.touchAgyHome()
		}
		return send.Send(Event{Type: EventDone})
	}), nil
}

func (p *AgyBinProvider) requireFilteredGeneration(resumeID string) error {
	if p.isolation.FilteredGenerations() > 0 {
		return nil
	}
	if strings.TrimSpace(resumeID) != "" {
		// agy may already have committed the submitted delta to its durable DB.
		// Reset the local boundary so a retry starts a fresh conversation with the
		// complete term-llm transcript instead of duplicating that delta on resume.
		p.resetConversationLocked()
	}
	return errors.New("agy did not route generation through the term-llm tool-filter proxy; refusing unverified turn")
}

func (p *AgyBinProvider) messagesForRequest(req Request) ([]Message, error) {
	if req.Ephemeral || p.conversationID == "" {
		return req.Messages, nil
	}
	if p.messagesSent > len(req.Messages) {
		slog.Warn("agy-bin resume message boundary exceeded request transcript; resetting conversation state",
			"messages_sent", p.messagesSent, "request_messages", len(req.Messages))
		p.resetConversationLocked()
		return req.Messages, nil
	}
	prefixHash, err := agyTranscriptHash(req.Messages[:p.messagesSent])
	if err != nil {
		return nil, err
	}
	if p.transcriptHash == "" || prefixHash != p.transcriptHash {
		slog.Warn("agy-bin request transcript diverged from resumed conversation; resetting conversation state",
			"messages_sent", p.messagesSent)
		p.resetConversationLocked()
		return req.Messages, nil
	}
	if p.messagesSent == len(req.Messages) {
		return nil, errors.New("agy-bin resumed session has no new messages to send")
	}
	if p.messagesSent > 0 {
		messages := grokResumeMessages(req.Messages[p.messagesSent:])
		if len(messages) == 0 {
			return nil, errors.New("agy-bin resumed session has no new messages to send")
		}
		return messages, nil
	}
	return req.Messages, nil
}

func agyTranscriptHash(messages []Message) (string, error) {
	data, err := json.Marshal(messages)
	if err != nil {
		return "", fmt.Errorf("hash agy-bin transcript: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
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

func (p *AgyBinProvider) buildArgs(req Request, resumeID string) []string {
	args := []string{"--dangerously-skip-permissions", "--disable-slash-commands", "--output-format", "stream-json", "--print-timeout", "10m"}
	model := strings.TrimSpace(chooseModel(req.Model, p.model))
	if model != "" {
		args = append(args, "--model", model)
	}
	if resumeID = strings.TrimSpace(resumeID); resumeID != "" && !req.Ephemeral {
		args = append(args, "--conversation", resumeID)
	}
	return args
}

func (p *AgyBinProvider) runCommand(ctx context.Context, args []string, prompt, workspaceDir, expectedConversationID string, debug bool, send eventSender, exposeBridge bool) (*agyStreamState, error) {
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
	cmd.Env = forceCLIWireEnvironment(p.buildCommandEnv(), p.isolation.Environment())
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
		scanErr := sc.Err()
		if scanErr != nil {
			cancel()
		}
		scanErrCh <- scanErr
	}()
	go func() {
		defer close(stderrDone)
		_ = drainCLIDiagnosticLines(stderr, func(rawLine string) {
			line := redact(rawLine)
			if debug {
				fmt.Fprintf(os.Stderr, "[agy stderr] %s\n", line)
			}
			recordCLITailLine(&stderrMu, &stderrTail, line, agyStderrTailMaxLines)
		})
	}()
	bridge := &cliTurnBridge{toolReqCh: make(chan cliToolRequest, 64), done: make(chan struct{})}
	if exposeBridge {
		p.cliToolBridgeState.activate(bridge, send.ch)
		defer p.cliToolBridgeState.deactivate(bridge)
	}
	defer close(bridge.done)
	expectedConversationID = strings.TrimSpace(expectedConversationID)
	state := &agyStreamState{
		conversationID:         expectedConversationID,
		expectedConversationID: expectedConversationID,
		pathReplacer:           agyPathReplacer(p.agyHome, workspaceDir),
	}
	dispatchErr := p.dispatchEvents(runCtx, lineCh, bridge.toolReqCh, send, state)
	if dispatchErr != nil {
		cancel()
	}
	scanErr := <-scanErrCh
	if scanErr != nil {
		cancel()
	}
	<-stderrDone
	waitErr := cmd.Wait()
	if scanErr != nil && !errors.Is(scanErr, context.Canceled) {
		return nil, fmt.Errorf("read agy output: %w", scanErr)
	}
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
	return dispatchCLILines(ctx, lineCh, toolReqCh, send, agyToolDrainGrace, func(line string) error {
		return handleAgyStreamLine(line, send, state)
	})
}

func (s *agyStreamState) observeConversationID(event, conversationID string) error {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID != "" && !validAgyConversationID(conversationID) {
		return fmt.Errorf("%w: %s event returned invalid conversation ID %q", errAgyConversationMismatch, event, conversationID)
	}
	expected := s.expectedConversationID
	if expected == "" {
		expected = s.conversationID
	}
	if conversationID == "" {
		// Once init or --conversation establishes the ID, later agy event variants
		// may omit it. Retain the established ID; non-empty conflicts are still
		// rejected before any text is emitted.
		return nil
	}
	if expected != "" && conversationID != expected {
		return fmt.Errorf("%w: %s event changed conversation ID from %q to %q", errAgyConversationMismatch, event, expected, conversationID)
	}
	s.conversationID = conversationID
	return nil
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
		if err := state.observeConversationID("init", message.ConversationID); err != nil {
			return err
		}
	case "step_update":
		if err := state.observeConversationID("step_update", message.StepUpdate.ConversationID); err != nil {
			return err
		}
		if message.StepUpdate.StepType == "agent_response" && message.StepUpdate.TextDelta != "" {
			state.sawText = true
			return send.Send(Event{Type: EventTextDelta, Text: state.rewritePaths(message.StepUpdate.TextDelta)})
		}
	case "result":
		if err := state.observeConversationID("result", message.Result.ConversationID); err != nil {
			return err
		}
		state.sawResult = true
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
	return newCLIProcessError(waitErr, stderrTail, stdoutTail, cliCommandErrorOptions{
		name:        "agy",
		authTerms:   []string{"authentication", "not logged", "log in"},
		authSummary: "agy is not logged in",
		authDetail:  "Run `agy` and complete Antigravity authentication.",
	})
}

func (p *AgyBinProvider) ensureMCPServer(ctx context.Context, tools []ToolSpec, debug bool) error {
	if p.mcpServer != nil {
		return nil
	}
	server := mcphttp.NewServer(p.cliToolBridgeState.wrappedExecutor(p.formatToolOutput))
	server.SetDebug(debug)
	url, token, err := server.Start(ctx, mcpToolSpecs(tools))
	if err != nil {
		return fmt.Errorf("start agy-bin MCP server: %w", err)
	}
	p.mcpServer, p.mcpURL, p.mcpToken = server, url, token
	return nil
}

func (p *AgyBinProvider) prepareHome(ephemeral bool) error {
	if !ephemeral {
		return p.ensureAgyHome()
	}
	home, err := os.MkdirTemp("", "term-llm-agy-home-")
	if err != nil {
		return fmt.Errorf("create agy private home: %w", err)
	}
	p.agyHome = home
	if err := ensureAgyHomeLayout(home); err != nil {
		_ = os.RemoveAll(home)
		p.agyHome = ""
		return err
	}
	if err := p.copyCredentials(); err != nil {
		_ = os.RemoveAll(home)
		p.agyHome = ""
		return err
	}
	return nil
}

func (p *AgyBinProvider) ensureAgyHome() error {
	if p.agyHome == "" {
		base, err := agyBinCacheBase()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(base, 0o700); err != nil {
			return fmt.Errorf("create agy-bin cache: %w", err)
		}
		id, err := newGrokHomeID()
		if err != nil {
			return err
		}
		p.agyHome = filepath.Join(base, id)
	} else {
		home, _, err := validateAgyHomeState(p.agyHome)
		if err != nil {
			return err
		}
		p.agyHome = home
	}
	if err := ensureAgyHomeLayout(p.agyHome); err != nil {
		return err
	}
	if err := p.copyCredentials(); err != nil {
		return err
	}
	p.touchAgyHome()
	p.gcAgyHomes()
	return nil
}

func ensureAgyHomeLayout(home string) error {
	for _, dir := range []string{
		home,
		filepath.Join(home, "cwd"),
		filepath.Join(home, ".gemini", "config"),
		filepath.Join(home, ".gemini", "antigravity"),
		filepath.Join(home, ".gemini", "antigravity-cli"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create agy home layout: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure agy home layout: %w", err)
		}
	}
	return nil
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

func agyBinCacheBase() (string, error) {
	cache := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME"))
	if cache == "" {
		var err error
		cache, err = os.UserCacheDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Abs(filepath.Join(cache, "term-llm", "agy-bin"))
}

func isAgyHomeID(id string) bool { return isGrokHomeID(id) }

func validateAgyHomeState(home string) (string, bool, error) {
	base, err := agyBinCacheBase()
	if err != nil {
		return "", false, err
	}
	candidate, err := filepath.Abs(strings.TrimSpace(home))
	if err != nil {
		return "", false, err
	}
	candidate = filepath.Clean(candidate)
	if filepath.Dir(candidate) != filepath.Clean(base) || !isAgyHomeID(filepath.Base(candidate)) {
		return "", false, fmt.Errorf("agy_home must be directly under %s", base)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", false, err
	}
	existed := true
	info, err := os.Lstat(candidate)
	if os.IsNotExist(err) {
		existed = false
		err = os.Mkdir(candidate, 0o700)
		if err == nil {
			info, err = os.Lstat(candidate)
		}
	}
	if err != nil {
		return "", false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", false, errors.New("agy_home is not a safe directory")
	}
	return candidate, existed, nil
}

func (p *AgyBinProvider) touchAgyHome() {
	if p.agyHome != "" {
		_ = os.WriteFile(filepath.Join(p.agyHome, ".last_used"), []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o600)
	}
}

func (p *AgyBinProvider) gcAgyHomes() {
	base, err := agyBinCacheBase()
	if err != nil {
		return
	}
	gcStaleCLIHomes(base, p.agyHome, agyHomeMaxAge, isAgyHomeID, "agy-bin")
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
	stopCLIWireAudit(p.wireAudit)
	p.wireAudit = nil
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
var _ ProviderStateExporter = (*AgyBinProvider)(nil)
var _ ProviderStateImporter = (*AgyBinProvider)(nil)

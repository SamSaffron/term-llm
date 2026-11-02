package clibin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/samsaffron/term-llm/internal/acp"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/mcphttp"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

const (
	vibeACPBinary = "vibe-acp"
	// vibeMCPServerName is deliberately a plain identifier: Vibe filters MCP
	// tools by "<server>_<tool>" display names and exposes them to the model as
	// "mcp_<server>.<tool>", normalizing any other characters to "_".
	vibeMCPServerName    = "termllm"
	vibeMCPToolPrefix    = "mcp_" + vibeMCPServerName + "."
	vibeEnabledToolsGlob = vibeMCPServerName + "_*"
	vibeStderrTailLines  = 40
	// vibeMaxTurns bounds model steps per prompt, matching grok-bin's budget.
	// Without it a model can loop on tool discovery indefinitely.
	vibeMaxTurns = 30
)

// vibeForcedEnv restricts the Vibe agent to term-llm's MCP tools. Vibe reads
// VIBE_* variables as a per-process config layer above the user's config.toml,
// so none of this is persisted to ~/.vibe. Built-in tools outside
// enabled_tools get Vibe's "never" approval policy; the ACP permission handler
// additionally denies anything that is not a term-llm MCP tool.
var vibeForcedEnv = map[string]string{
	"VIBE_ENABLED_TOOLS":               `["` + vibeEnabledToolsGlob + `"]`,
	"VIBE_ENABLE_MCP":                  "true",
	"VIBE_MCP_SERVERS":                 "[]",
	"VIBE_ENABLE_SUBAGENTS":            "false",
	"VIBE_ENABLE_BACKGROUND_PROCESSES": "false",
	"VIBE_ENABLE_CONNECTORS":           "false",
	"VIBE_INCLUDE_PROJECT_CONTEXT":     "false",
	"VIBE_ENABLE_UPDATE_CHECKS":        "false",
	"VIBE_ENABLE_AUTO_UPDATE":          "false",
	"VIBE_ENABLE_NOTIFICATIONS":        "false",
}

// VibeBinProvider uses the locally installed Mistral Vibe CLI (vibe-acp) as an
// authenticated model transport, so requests draw on the Vibe subscription the
// CLI is signed in with rather than a Mistral API key. term-llm never reads the
// Vibe credential: vibe-acp loads it itself, and an inherited MISTRAL_API_KEY is
// removed so it cannot take precedence over the Vibe sign-in.
//
// term-llm remains the sole owner of executable tools, exposed through an
// isolated in-process HTTP MCP server. Vibe's configuration and sessions live
// in a dedicated persistent VIBE_HOME, not the interactive CLI's ~/.vibe. Its
// subscription sign-in remains in the OS keyring; no credentials are copied.
//
// It is not safe for concurrent conversation streams; create one provider per conversation.
type VibeBinProvider struct {
	model                  string
	sessionID              string
	messagesSent           int
	extraEnv               map[string]string
	toolExecutorConfigured bool

	mcpMu                 sync.Mutex
	mcpServer             *mcphttp.Server
	mcpURL                string
	mcpToken              string
	mcpToolFingerprint    [sha256.Size]byte
	mcpToolFingerprintSet bool

	acpMu      sync.Mutex
	acpProcess *vibeACPProcess
	// acpPromptActive is true only while session/prompt is in flight.
	acpPromptActive atomic.Bool
	// expectedCancel marks a cancelled stop reason as a term-llm initiated
	// steering boundary rather than an agent-side cancellation.
	expectedCancel atomic.Bool

	cliToolBridgeState
	tempFileTracker

	activeStream atomic.Bool
}

type vibeBinProviderState struct {
	SessionID    string `json:"session_id"`
	MessagesSent int    `json:"messages_sent"`
}

// ValidateVibeBinModel rejects values that are clearly not Vibe model aliases.
// Aliases are otherwise open-ended because the catalog depends on the account.
func ValidateVibeBinModel(model string) error {
	model = strings.TrimSpace(model)
	if strings.ContainsAny(model, " \t\n\"'") {
		return fmt.Errorf("vibe-bin model %q is not a valid Vibe model alias", model)
	}
	return nil
}

func NewVibeBinProvider(model string, env map[string]string) *VibeBinProvider {
	p := &VibeBinProvider{model: strings.TrimSpace(model)}
	p.tempFileTracker.logName = "vibe-bin"
	p.SetEnv(env)
	return p
}

func (p *VibeBinProvider) Name() string {
	if p.model == "" {
		return "Mistral Vibe CLI"
	}
	return fmt.Sprintf("Mistral Vibe CLI (%s)", p.model)
}

func (p *VibeBinProvider) Credential() string { return "vibe-bin" }

func (p *VibeBinProvider) Capabilities() core.Capabilities {
	return core.Capabilities{
		ToolCalls:               true,
		SupportsToolChoice:      false,
		ManagesOwnContext:       true,
		InlineToolLoop:          true,
		OrderedInlineToolEvents: true,
	}
}

func (p *VibeBinProvider) SetEnv(env map[string]string) {
	if len(env) == 0 {
		p.extraEnv = nil
		return
	}
	p.extraEnv = make(map[string]string, len(env))
	for key, value := range env {
		p.extraEnv[key] = value
	}
}

func (p *VibeBinProvider) SetToolExecutor(executor func(context.Context, string, json.RawMessage) (core.ToolOutput, error)) {
	// Tool calls route through the engine via cliToolBridgeState; only record
	// whether engine wiring exists.
	p.toolExecutorConfigured = executor != nil
}

func (p *VibeBinProvider) ResetConversation() {
	p.acpMu.Lock()
	if p.acpProcess != nil {
		p.acpProcess.stop()
		p.acpProcess = nil
	}
	p.acpMu.Unlock()
	p.acpPromptActive.Store(false)
	p.expectedCancel.Store(false)
	p.sessionID = ""
	p.messagesSent = 0
}

func (p *VibeBinProvider) ExportProviderState() ([]byte, bool) {
	if strings.TrimSpace(p.sessionID) == "" {
		return nil, false
	}
	data, err := json.Marshal(vibeBinProviderState{SessionID: p.sessionID, MessagesSent: p.messagesSent})
	if err != nil {
		return nil, false
	}
	return data, true
}

func (p *VibeBinProvider) ImportProviderState(data []byte) error {
	var state vibeBinProviderState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode vibe-bin provider state: %w", err)
	}
	state.SessionID = strings.TrimSpace(state.SessionID)
	if state.SessionID == "" {
		return fmt.Errorf("decode vibe-bin provider state: missing session_id")
	}
	if state.MessagesSent < 0 {
		return fmt.Errorf("decode vibe-bin provider state: negative messages_sent")
	}
	p.sessionID = state.SessionID
	p.messagesSent = state.MessagesSent
	return nil
}

func (p *VibeBinProvider) RequestInlineFlush() {
	p.requestInlineFlush()
	p.sendNativeInterrupt()
}

func (p *VibeBinProvider) SupportsInlineFlush() bool { return true }

func (p *VibeBinProvider) SupportsImmediateInterruption() bool { return true }

// PublishDynamicTools asks for a provider boundary: Vibe binds MCP servers when
// a session is created or loaded, so the next Stream restarts vibe-acp and
// reloads the session against the expanded tool surface.
func (p *VibeBinProvider) PublishDynamicTools(tools []core.ToolSpec) error {
	if len(tools) == 0 {
		return nil
	}
	p.mcpMu.Lock()
	running := p.mcpServer != nil
	p.mcpMu.Unlock()
	if running {
		p.requestInlineFlush()
	}
	return nil
}

func (p *VibeBinProvider) Stream(ctx context.Context, req core.Request) (core.Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		p.activeRuns.Add(1)
		defer p.finishStreamCleanup()

		if !p.activeStream.CompareAndSwap(false, true) {
			if req.Ephemeral {
				return fmt.Errorf("vibe-bin ephemeral request cannot run while another stream is active on the same provider")
			}
			return fmt.Errorf("vibe-bin provider already has an active stream; create a dedicated provider instance per conversation")
		}
		defer p.activeStream.Store(false)

		messages, err := p.messagesForRequest(req)
		if err != nil {
			return err
		}
		debug := req.Debug || req.DebugRaw
		exposeToolBridge := false
		if len(req.Tools) > 0 {
			if !p.toolExecutorConfigured {
				runtimeoutput.Warn("vibe-bin tools requested but no tool executor configured", "tool_count", len(req.Tools))
			} else if err := p.ensureMCPServer(ctx, req.Tools, debug); err != nil {
				return err
			} else {
				exposeToolBridge = true
			}
		}

		result, err := p.runVibeACP(ctx, req, messages, debug, send, exposeToolBridge)
		if err != nil {
			return err
		}
		if !req.Ephemeral && result.sessionID != "" {
			p.sessionID = result.sessionID
			p.messagesSent = len(req.Messages)
		}
		return send.Send(core.Event{Type: core.EventDone})
	}), nil
}

func (p *VibeBinProvider) messagesForRequest(req core.Request) ([]core.Message, error) {
	if req.Ephemeral || p.sessionID == "" {
		return req.Messages, nil
	}
	switch {
	case p.messagesSent > len(req.Messages):
		runtimeoutput.Warn("vibe-bin resume message boundary exceeded request transcript; resetting conversation state",
			"messages_sent", p.messagesSent, "request_messages", len(req.Messages))
		p.ResetConversation()
		return req.Messages, nil
	case p.messagesSent == len(req.Messages):
		return nil, fmt.Errorf("vibe-bin resumed session has no new messages to send")
	case p.messagesSent > 0:
		// Vibe's session already holds the generated assistant and tool output.
		messages := grokResumeMessages(req.Messages[p.messagesSent:])
		if len(messages) == 0 {
			return nil, fmt.Errorf("vibe-bin resumed session has no new messages to send")
		}
		return messages, nil
	default:
		return req.Messages, nil
	}
}

// vibeToolPolicy steers the model away from Vibe's built-in tools. Vibe's
// runtime always lists them (bash, read_file, write_file, edit, grep, ...);
// configuration can only deny them, and their names collide with term-llm's.
func vibeToolPolicy(tools []core.ToolSpec) string {
	if len(tools) == 0 {
		return "Your built-in tools (bash, read_file, write_file, edit, grep, process, and agent tools) are disabled in this session and every call to them is denied. Answer without tools."
	}
	names := make([]string, 0, len(tools))
	functions := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, "`"+tool.Name+"`")
		functions = append(functions, `"`+vibeMCPToolPrefix+tool.Name+`"`)
	}
	return "Your built-in tools (bash, read_file, write_file, edit, grep, process, and agent tools) are disabled in this session and every call to them is denied. " +
		"For all actions use only the term-llm tools in the `" + strings.TrimSuffix(vibeMCPToolPrefix, ".") + "` group: " + strings.Join(names, ", ") + ". " +
		"When a term-llm tool has the same name as a built-in tool, always call the term-llm one (`" + vibeMCPToolPrefix + "<name>`). " +
		"Do not search for tools: before first use, load their arguments once with search_tool_functions mode \"details\" and functions [" + strings.Join(functions, ", ") + "], then call them."
}

// vibePromptBlocks renders messages as ACP content blocks. A fresh session
// also carries the tool policy and term-llm's system prompt, because Vibe
// keeps its own agent system prompt and has no ACP override for it.
func vibePromptBlocks(messages []core.Message, toolPolicy, systemPrompt string) ([]acp.ContentBlock, error) {
	data, err := buildGrokACPPrompt(messages)
	if err != nil {
		return nil, fmt.Errorf("build vibe-bin prompt: %w", err)
	}
	var prompt grokACPPrompt
	if err := json.Unmarshal(data, &prompt); err != nil {
		return nil, fmt.Errorf("decode vibe-bin prompt: %w", err)
	}
	blocks := make([]acp.ContentBlock, 0, len(prompt.Content)+1)
	var instructions []string
	for _, text := range []string{toolPolicy, systemPrompt} {
		if text = strings.TrimSpace(text); text != "" {
			instructions = append(instructions, text)
		}
	}
	if len(instructions) > 0 {
		blocks = append(blocks, acp.ContentBlock{Type: "text", Text: "<system_instructions>\n" + strings.Join(instructions, "\n\n") + "\n</system_instructions>\n\n"})
	}
	for _, block := range prompt.Content {
		blocks = append(blocks, acp.ContentBlock{Type: block.Type, Text: block.Text, Data: block.Data, MimeType: block.MimeType})
	}
	return blocks, nil
}

// buildVibeCommandEnv returns the vibe-acp environment. Model selection uses
// VIBE_ACTIVE_MODEL rather than ACP session/set_config_option, which Vibe
// persists as the user's default model. home is the dedicated runtime home:
// user hooks, plugins, tools, config.toml and .env must not be inherited.
func (p *VibeBinProvider) buildVibeCommandEnv(model, home string) []string {
	forced := make(map[string]string, len(vibeForcedEnv)+2)
	for key, value := range vibeForcedEnv {
		forced[key] = value
	}
	forced["VIBE_HOME"] = home
	if model != "" {
		forced["VIBE_ACTIVE_MODEL"] = model
	}
	env := make([]string, 0, len(os.Environ())+len(p.extraEnv)+len(forced))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, ok := forced[key]; ok {
			continue
		}
		if _, ok := p.extraEnv[key]; ok {
			continue
		}
		// An inherited API key would win over the Vibe sign-in and bill the
		// API allowance instead of the Vibe subscription.
		if key == "MISTRAL_API_KEY" {
			continue
		}
		env = append(env, entry)
	}
	for key, value := range p.extraEnv {
		if _, ok := forced[key]; ok {
			continue
		}
		env = append(env, key+"="+value)
	}
	for key, value := range forced {
		env = append(env, key+"="+value)
	}
	return env
}

// vibeNeutralCWD is the stable working directory given to Vibe. It keeps
// Vibe's project discovery (AGENTS.md, .vibe/) away from user checkouts and
// stays constant so session/load finds earlier sessions.
func vibeNeutralCWD() (string, error) {
	cacheDir := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME"))
	if cacheDir == "" {
		var err error
		cacheDir, err = os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve user cache directory: %w", err)
		}
	}
	dir, err := filepath.Abs(filepath.Join(cacheDir, "term-llm", "vibe-bin", "cwd"))
	if err != nil {
		return "", fmt.Errorf("resolve vibe-bin working directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create vibe-bin working directory: %w", err)
	}
	return dir, nil
}

// vibeRuntimeHome isolates executable Vibe configuration, including user and
// plugin hooks, which are independent of ACP tool approval. OIDC credentials
// are held in the OS keyring, outside VIBE_HOME. Keep this directory across
// provider cleanup so session/load works after restarting term-llm.
func vibeRuntimeHome(cwd string) (string, error) {
	home := filepath.Join(filepath.Dir(cwd), "home")
	if err := os.Mkdir(home, 0o700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("create vibe-bin runtime home: %w", err)
	}
	info, err := os.Lstat(home)
	if err != nil {
		return "", fmt.Errorf("inspect vibe-bin runtime home: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("vibe-bin runtime home must be a directory, not a symlink: %s", home)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return "", fmt.Errorf("secure vibe-bin runtime home: %w", err)
	}
	return home, nil
}

func (p *VibeBinProvider) ensureMCPServer(ctx context.Context, tools []core.ToolSpec, debug bool) error {
	specs := mcpToolSpecs(tools)
	encoded, err := json.Marshal(specs)
	if err != nil {
		return fmt.Errorf("encode vibe-bin MCP tool surface: %w", err)
	}
	fingerprint := sha256.Sum256(encoded)

	p.mcpMu.Lock()
	if p.mcpServer != nil && p.mcpToolFingerprintSet && p.mcpToolFingerprint == fingerprint {
		p.mcpMu.Unlock()
		return nil
	}
	oldServer := p.mcpServer
	p.mcpServer = nil
	p.mcpURL = ""
	p.mcpToken = ""
	p.mcpToolFingerprintSet = false
	p.mcpMu.Unlock()
	if oldServer != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), mcpStopTimeout)
		oldServer.Stop(stopCtx)
		cancel()
	}

	if debug {
		runtimeoutput.Printf("[vibe-bin] starting HTTP MCP server for %d tools\n", len(tools))
	}
	server := mcphttp.NewServer(p.cliToolBridgeState.wrappedExecutor(p.formatToolOutput))
	server.SetDebug(debug)
	url, token, err := server.Start(ctx, specs)
	if err != nil {
		return fmt.Errorf("start vibe-bin MCP server: %w", err)
	}
	p.mcpMu.Lock()
	p.mcpServer = server
	p.mcpURL = url
	p.mcpToken = token
	p.mcpToolFingerprint = fingerprint
	p.mcpToolFingerprintSet = true
	p.mcpMu.Unlock()
	return nil
}

func (p *VibeBinProvider) formatToolOutput(output core.ToolOutput) string {
	return p.appendInlineFlushNotice(formatToolOutputForGrok(output))
}

// CleanupMCP stops the conversation-scoped vibe-acp process and HTTP tool
// bridge. Vibe's own session history stays in its home for later session/load.
func (p *VibeBinProvider) CleanupMCP() {
	p.acpMu.Lock()
	if p.acpProcess != nil {
		p.acpProcess.stop()
		p.acpProcess = nil
	}
	p.acpMu.Unlock()
	p.mcpMu.Lock()
	server := p.mcpServer
	p.mcpServer = nil
	p.mcpURL = ""
	p.mcpToken = ""
	p.mcpToolFingerprint = [sha256.Size]byte{}
	p.mcpToolFingerprintSet = false
	p.clearInlineFlush()
	p.mcpMu.Unlock()
	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), mcpStopTimeout)
		server.Stop(ctx)
		cancel()
	}
	p.cleanupTempFiles()
}

func (p *VibeBinProvider) CleanupTurn() { p.cleanupTempFilesIfIdle() }

var _ core.Provider = (*VibeBinProvider)(nil)
var _ toolExecutorSetter = (*VibeBinProvider)(nil)
var _ providerCleaner = (*VibeBinProvider)(nil)
var _ providerTurnCleaner = (*VibeBinProvider)(nil)
var _ core.ProviderStateExporter = (*VibeBinProvider)(nil)
var _ core.ProviderStateImporter = (*VibeBinProvider)(nil)

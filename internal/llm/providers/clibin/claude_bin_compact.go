package clibin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// claudeNativeCompactRunner executes one Claude Code compaction process and
// returns its stream-json stdout. Overridable in tests.
type claudeNativeCompactRunner func(ctx context.Context, args []string, effort, workingDir, stdin string) ([]byte, error)

var _ core.NativeCompactor = (*ClaudeBinProvider)(nil)

// NativeCompact runs Claude Code's own /compact against the resumed CLI
// session. Claude Code keeps the session ID and replaces its context with a
// summary, so later turns keep resuming the same session and term-llm's
// transcript (and resume boundary) stay untouched.
//
// Normal turns pass --disable-slash-commands, which also disables /compact, so
// compaction runs as a separate one-shot process without that flag. Its only
// input is the /compact command; no tools or MCP bridge are exposed.
func (p *ClaudeBinProvider) NativeCompact(ctx context.Context, req core.NativeCompactRequest) (*core.NativeCompactResult, error) {
	// Take the stream gate first: resume state is only stable while held.
	if !p.activeStream.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("claude-bin cannot compact while a stream is active")
	}
	defer p.activeStream.Store(false)
	// A pending fork would compact the parent session in place.
	if strings.TrimSpace(p.sessionID) == "" || p.messagesSent <= 0 || p.forkSession {
		return nil, core.ErrNativeCompactUnavailable
	}

	args, effort := p.buildArgs(ctx, core.Request{Model: req.Model, MaxTurns: 1}, eventSender{})
	filtered := args[:0]
	for _, arg := range args {
		// Slash commands must be enabled for /compact; partial deltas are noise.
		if arg != "--disable-slash-commands" && arg != "--include-partial-messages" {
			filtered = append(filtered, arg)
		}
	}
	// Re-send the session's instructions: without them Claude Code summarises
	// under its default prompt, which produced unusable summaries in testing.
	systemPrompt := p.lastSystemPrompt
	if strings.TrimSpace(systemPrompt) == "" {
		systemPrompt = req.SystemPrompt
	}
	if strings.TrimSpace(systemPrompt) != "" {
		filtered = append(filtered, "--system-prompt", systemPrompt)
	}
	filtered = append(filtered, "--input-format", "stream-json")
	input := p.buildStreamJsonInput([]core.Message{core.UserText("/compact")}, p.sessionID)

	runner := p.nativeCompactRunner
	if runner == nil {
		runner = p.runNativeCompact
	}
	// Claude Code resolves --resume within the project of its working directory.
	out, err := runner(ctx, filtered, effort, p.sessionWorkingDir, input)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("claude-bin native compact: %w", err)
	}
	return parseNativeCompactOutput(out)
}

func (p *ClaudeBinProvider) runNativeCompact(ctx context.Context, args []string, effort, workingDir, stdin string) ([]byte, error) {
	cmd, stdinPipe, cleanup, err := p.prepareClaudeCommand(ctx, args, effort, workingDir)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		_ = stdinPipe.Close()
		return nil, fmt.Errorf("start claude: %w", err)
	}
	_, writeErr := io.WriteString(stdinPipe, stdin+"\n")
	_ = stdinPipe.Close()
	if err := cmd.Wait(); err != nil {
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
		}
		return nil, fmt.Errorf("%w: %s", err, tail)
	}
	if writeErr != nil {
		return nil, fmt.Errorf("write /compact input: %w", writeErr)
	}
	return stdout.Bytes(), nil
}

func parseNativeCompactOutput(out []byte) (*core.NativeCompactResult, error) {
	var (
		result     *core.NativeCompactResult
		resultText string
		isError    bool
	)
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var msg struct {
			Type            string `json:"type"`
			Subtype         string `json:"subtype"`
			Result          string `json:"result"`
			IsError         bool   `json:"is_error"`
			CompactMetadata *struct {
				PreTokens  int `json:"pre_tokens"`
				PostTokens int `json:"post_tokens"`
			} `json:"compact_metadata"`
		}
		if json.Unmarshal(scanner.Bytes(), &msg) != nil {
			continue
		}
		switch {
		case msg.Type == "system" && msg.Subtype == "compact_boundary" && msg.CompactMetadata != nil:
			result = &core.NativeCompactResult{PreTokens: msg.CompactMetadata.PreTokens, PostTokens: msg.CompactMetadata.PostTokens}
		case msg.Type == "result":
			resultText, isError = strings.TrimSpace(msg.Result), msg.IsError
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("claude-bin native compact: read output: %w", err)
	}
	if result != nil && !isError {
		return result, nil
	}
	if resultText == "" {
		resultText = "no compact_boundary event"
	}
	// Claude Code reports refusals ("Not enough messages to compact.",
	// "Error during compaction: ...") as ordinary result text.
	return nil, fmt.Errorf("claude-bin native compact: %s", resultText)
}

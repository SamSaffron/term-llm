package clibin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/term-llm/term-llm/internal/agyproxy"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/procutil"
	"github.com/term-llm/term-llm/internal/runtimeoutput"
)

// agyStdinWaitDelay bounds Wait after agy exits or the turn is cancelled. exec
// copies stdin on its own goroutine; a descendant that inherited the pipe must
// not stall the turn once the direct process is gone.
const agyStdinWaitDelay = 5 * time.Second

// agyPromptStagingThreshold is the largest prompt sent verbatim in the NDJSON
// user event. agy truncates a user text part at roughly 192000 bytes, so larger
// prompts are persisted as a private artifact and sent as a hash-anchored
// envelope that still embeds the full original. When agy truncates the stored
// envelope, agyproxy restores the exact original from the artifact before every
// model generation, including generations that replay it from agy's durable
// conversation.
const agyPromptStagingThreshold = 128 << 10

// stageAgyPrompt replaces an oversized prompt with the hash-addressed wire
// envelope. The artifact lives under the provider's private home, so historical
// native resumes remain rehydratable after the turn and after a process restart.
// A staging failure happens before exec and must preserve a good cursor.
func (p *AgyBinProvider) stageAgyPrompt(prompt string) (string, bool, error) {
	if len(prompt) <= agyPromptStagingThreshold {
		return prompt, false, nil
	}
	wire, err := agyproxy.StagePrompt(p.agyHome, prompt)
	if err != nil {
		return "", false, fmt.Errorf("stage agy-bin prompt: %w", err)
	}
	if strings.TrimSpace(wire) == "" {
		return "", false, errors.New("stage agy-bin prompt: empty wire prompt")
	}
	return wire, true, nil
}

// agyStreamInput is the NDJSON prompt event agy accepts with
// --input-format stream-json. agy validates an "event" discriminator, not
// Claude's "type" field. Exactly one user event followed by EOF runs one turn.
type agyStreamInput struct {
	Event   string             `json:"event"`
	Message agyStreamInputBody `json:"message"`
}

type agyStreamInputBody struct {
	Content string `json:"content"`
}

// encodeAgyStreamInput serializes the complete assembled prompt as one
// newline-terminated NDJSON user event. encoding/json owns escaping, so
// unicode, NUL, newlines and flag-looking text stay inside the JSON string.
func encodeAgyStreamInput(prompt string) ([]byte, error) {
	payload, err := json.Marshal(agyStreamInput{Event: "user", Message: agyStreamInputBody{Content: prompt}})
	if err != nil {
		return nil, fmt.Errorf("encode agy stream input: %w", err)
	}
	return append(payload, '\n'), nil
}

// agyProcessError marks a failure after the agy process started. The CLI may
// already have accepted the submitted delta before failing, so a restored
// cursor is no longer verifiably committed.
type agyProcessError struct{ err error }

func (e *agyProcessError) Error() string { return e.err.Error() }
func (e *agyProcessError) Unwrap() error { return e.err }

// agyFailureAfterStart reports whether err happened after process creation.
// A pre-exec failure accepted no input and must preserve a good cursor.
func agyFailureAfterStart(err error) bool {
	var processErr *agyProcessError
	return errors.As(err, &processErr)
}

// runCommand executes one agy process for one engine stream. The whole prompt
// travels as a single NDJSON user event on stdin; argv carries flags only, so a
// large transcript can no longer hit the per-argument E2BIG limit. A fresh
// process per stream preserves procutil ownership, cancellation, concurrent
// drains, neutral cwd and proxy environment semantics.
func (p *AgyBinProvider) runCommand(ctx context.Context, args []string, prompt, workspaceDir, expectedConversationID string, debug bool, send eventSender, exposeBridge bool) (*agyStreamState, error) {
	wirePrompt, staged, err := p.stageAgyPrompt(prompt)
	if err != nil {
		return nil, err
	}
	input, err := encodeAgyStreamInput(wirePrompt)
	if err != nil {
		return nil, err
	}
	if debug {
		// Original prompt bytes describe the conversation input; stdin bytes
		// describe the transport. Neither contains prompt text.
		runtimeoutput.Printf("[agy-bin] starting agy with model %q (prompt_bytes=%d stdin_bytes=%d staged=%t)\n",
			core.ChooseModel("", p.model), len(prompt), len(input), staged)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	owned, stdout, stderr, err := p.startAgyCommand(runCtx, args, input)
	if err != nil {
		return nil, err
	}
	defer owned.Close()
	state, err := p.collectAgyStream(runCtx, cancel, owned, stdout, stderr, prompt, expectedConversationID, debug, send, exposeBridge)
	if err != nil {
		return nil, &agyProcessError{err: err}
	}
	return state, nil
}

// startAgyCommand prepares and starts the subprocess. Every error it returns is
// pre-exec: no prompt bytes were accepted, so callers must not invalidate a
// resumable conversation because of it.
func (p *AgyBinProvider) startAgyCommand(ctx context.Context, args []string, input []byte) (*procutil.Command, io.ReadCloser, io.ReadCloser, error) {
	// Keep agy out of the real workspace so it cannot auto-load workspace-local
	// agents, rules, or MCP configuration. The term-llm agent has no default
	// prompt sections, so the model never sees this neutral directory; the
	// prompt names workspaceDir instead.
	neutralCWD := filepath.Join(p.agyHome, "cwd")
	cmd, err := newCLICommand(ctx, p.agyBinary, args, neutralCWD)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("prepare agy command: %w", err)
	}
	cmd.Env = forceCLIWireEnvironment(p.buildCommandEnv(), p.isolation.Environment())
	// exec copies a non-file Stdin on its own goroutine, so agy can write a
	// result before draining a multi-megabyte payload without deadlocking the
	// stdout drain. EPIPE from an early exit is ignored by os/exec.
	cmd.Stdin = bytes.NewReader(input)
	cmd.WaitDelay = agyStdinWaitDelay
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get agy stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get agy stderr: %w", err)
	}
	owned, err := procutil.PrepareCommand(cmd)
	if err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, nil, nil, fmt.Errorf("prepare agy process: %w", err)
	}
	if err := owned.Start(); err != nil {
		owned.Close()
		return nil, nil, nil, fmt.Errorf("start agy command: %w", err)
	}
	return owned, stdout, stderr, nil
}

// collectAgyStream drains stdout/stderr, dispatches bridge tools, and validates
// the terminal result. Every error here is post-start.
func (p *AgyBinProvider) collectAgyStream(runCtx context.Context, cancel context.CancelFunc, owned *procutil.Command, stdout, stderr io.Reader, prompt, expectedConversationID string, debug bool, send eventSender, exposeBridge bool) (*agyStreamState, error) {
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
				runtimeoutput.Printf("[agy stderr] %s\n", line)
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
	waitErr := owned.Wait()
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
		if err := send.Send(core.Event{Type: core.EventTextDelta, Text: state.fallbackText}); err != nil {
			return nil, err
		}
	}
	if state.usage != nil {
		if err := send.Send(core.Event{Type: core.EventUsage, Use: state.usage}); err != nil {
			return nil, err
		}
	}
	return state, nil
}

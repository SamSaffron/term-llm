package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	toolpkg "github.com/samsaffron/term-llm/internal/tools"
)

type progressiveStopWhen string

const (
	progressiveStopWhenDone    progressiveStopWhen = "done"
	progressiveStopWhenTimeout progressiveStopWhen = "timeout"

	progressiveDefaultFinalizeGrace = 5 * time.Minute
	progressiveMaxFinalizeBudget    = 5 * time.Minute
	progressiveMinFinalizeBudget    = 5 * time.Second
)

type askProgressiveOptions struct {
	Enabled      bool
	Timeout      time.Duration
	StopWhen     progressiveStopWhen
	ContinueWith string
}

type progressiveRunOptions struct {
	StopWhen               progressiveStopWhen
	ContinueWith           string
	SessionID              string
	ForceNamedFinalization bool
	OnEvent                func(llm.Event) error
	OnSyntheticUserMessage func(context.Context, llm.Message) error
	OnResponseCompleted    llm.ResponseCompletedCallback
	OnTurnCompleted        llm.TurnCompletedCallback
}

type progressiveRunResult struct {
	ExitReason    string         `json:"exit_reason"`
	Finalized     bool           `json:"finalized"`
	SessionID     string         `json:"session_id,omitempty"`
	Sequence      int            `json:"sequence,omitempty"`
	Reason        string         `json:"reason,omitempty"`
	Message       string         `json:"message,omitempty"`
	Progress      map[string]any `json:"progress,omitempty"`
	FinalResponse string         `json:"final_response,omitempty"`
	FallbackText  string         `json:"fallback_text,omitempty"`
}

type progressivePassResult struct {
	produced           []llm.Message
	lastText           string
	newCommitCount     int
	hadNonProgressTool bool
}

type progressCandidate struct {
	State   map[string]any
	Reason  string
	Message string
	Final   bool
}

type progressCommit struct {
	Sequence int
	Reason   string
	Message  string
	Final    bool
	State    map[string]any
}

type progressTracker struct {
	pending map[string]progressCandidate
	latest  *progressCommit
}

func newProgressTracker() *progressTracker {
	return &progressTracker{
		pending: make(map[string]progressCandidate),
	}
}

func (t *progressTracker) latestSequence() int {
	if t.latest == nil {
		return 0
	}
	return t.latest.Sequence
}

func (t *progressTracker) observeToolCall(callID, name string, args json.RawMessage) {
	if !isProgressToolName(name) || strings.TrimSpace(callID) == "" {
		return
	}
	candidate, err := parseProgressCandidate(args)
	if err != nil {
		return
	}
	if strings.TrimSpace(name) == toolpkg.FinalizeProgressToolName {
		candidate.Final = true
		if candidate.Reason == "" {
			candidate.Reason = "finalize"
		}
	}
	t.pending[callID] = candidate
}

func (t *progressTracker) commitToolCall(callID, name string, success bool) *progressCommit {
	if !success || !isProgressToolName(name) || strings.TrimSpace(callID) == "" {
		delete(t.pending, callID)
		return nil
	}
	candidate, ok := t.pending[callID]
	delete(t.pending, callID)
	if !ok {
		return nil
	}
	commit := &progressCommit{
		Sequence: t.latestSequence() + 1,
		Reason:   candidate.Reason,
		Message:  candidate.Message,
		Final:    candidate.Final,
		State:    cloneProgressState(candidate.State),
	}
	t.latest = commit
	return commit
}

func (t *progressTracker) latestState() map[string]any {
	if t.latest == nil {
		return nil
	}
	return cloneProgressState(t.latest.State)
}

func cloneProgressState(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	data, err := json.Marshal(in)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func isProgressToolName(name string) bool {
	switch strings.TrimSpace(name) {
	case toolpkg.UpdateProgressToolName, toolpkg.FinalizeProgressToolName:
		return true
	default:
		return false
	}
}

func parseProgressCandidate(raw json.RawMessage) (progressCandidate, error) {
	var parsed struct {
		State   map[string]any `json:"state"`
		Reason  string         `json:"reason,omitempty"`
		Message string         `json:"message,omitempty"`
		Final   bool           `json:"final,omitempty"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return progressCandidate{}, err
	}
	if parsed.State == nil {
		return progressCandidate{}, fmt.Errorf("state missing")
	}
	return progressCandidate{
		State:   parsed.State,
		Reason:  strings.TrimSpace(parsed.Reason),
		Message: strings.TrimSpace(parsed.Message),
		Final:   parsed.Final,
	}, nil
}

func validateAskProgressiveOptions(opts *askProgressiveOptions) error {
	stopWhenSet := opts.StopWhen != ""
	continueWithSet := strings.TrimSpace(opts.ContinueWith) != ""

	if !opts.Enabled {
		if stopWhenSet {
			return fmt.Errorf("--stop-when requires --progressive")
		}
		if continueWithSet {
			return fmt.Errorf("--continue-with requires --progressive")
		}
		return nil
	}

	if opts.StopWhen == "" {
		// When a timeout is configured, default to "timeout" so the agent
		// actually uses its budget instead of exiting after the first pass.
		if opts.Timeout > 0 {
			opts.StopWhen = progressiveStopWhenTimeout
		} else {
			opts.StopWhen = progressiveStopWhenDone
		}
	}
	switch opts.StopWhen {
	case progressiveStopWhenDone, progressiveStopWhenTimeout:
	default:
		return fmt.Errorf("invalid --stop-when %q", opts.StopWhen)
	}

	if continueWithSet && opts.StopWhen != progressiveStopWhenTimeout {
		return fmt.Errorf("--continue-with requires --stop-when timeout")
	}
	if opts.StopWhen == progressiveStopWhenTimeout && opts.Timeout <= 0 {
		return fmt.Errorf("--stop-when timeout requires --timeout")
	}
	return nil
}

func runProgressiveSession(ctx context.Context, engine *llm.Engine, req llm.Request, opts progressiveRunOptions) (progressiveRunResult, error) {
	if opts.StopWhen == "" {
		opts.StopWhen = progressiveStopWhenDone
	}
	if strings.TrimSpace(opts.ContinueWith) == "" {
		opts.ContinueWith = defaultProgressiveContinuePrompt()
	}

	updateTool := toolpkg.NewUpdateProgressTool()
	finalizeTool := toolpkg.NewFinalizeProgressTool()
	engine.RegisterTool(updateTool)
	defer engine.UnregisterTool(updateTool.Spec().Name)

	history := append([]llm.Message(nil), req.Messages...)
	tracker := newProgressTrackerFromMessages(history)
	lastText := ""

	mainCtx, reserve, hasTimeoutBudget := progressiveWorkContext(ctx)

	for {
		passReq := req
		passReq.Messages = append([]llm.Message(nil), history...)
		passReq.Tools = append([]llm.ToolSpec(nil), req.Tools...)
		passReq.Tools = append(passReq.Tools, updateTool.Spec())

		passResult, err := runProgressivePass(mainCtx, engine, passReq, opts, tracker)
		if passResult.lastText != "" {
			lastText = passResult.lastText
		}
		history = append(history, passResult.produced...)
		if err != nil {
			exitReason := progressiveExitReason(err)
			result := buildProgressiveRunResult(opts.SessionID, exitReason, false, tracker.latest, lastText)
			if exitReason == exitReasonTimeout || exitReason == exitReasonCancelled {
				finalized, finalText := attemptProgressiveFinalization(ctx, engine, finalizeTool, req, history, opts, tracker, reserve, exitReason)
				if finalText != "" {
					lastText = finalText
				}
				result = buildProgressiveRunResult(opts.SessionID, exitReason, finalized, tracker.latest, lastText)
				return result, nil
			}
			if tracker.latest != nil {
				return result, err
			}
			return result, err
		}

		exitReason := exitReasonNatural
		if opts.StopWhen == progressiveStopWhenTimeout && hasTimeoutBudget && !progressiveHasRemainingBudget(ctx, reserve) {
			exitReason = exitReasonTimeout
		}
		if opts.StopWhen != progressiveStopWhenTimeout || !hasTimeoutBudget || exitReason == exitReasonTimeout {
			finalized, finalText := attemptProgressiveFinalization(ctx, engine, finalizeTool, req, history, opts, tracker, reserve, exitReason)
			if finalText != "" {
				lastText = finalText
			}
			return buildProgressiveRunResult(opts.SessionID, exitReason, finalized, tracker.latest, lastText), nil
		}

		if passResult.newCommitCount == 0 && !passResult.hadNonProgressTool {
			finalized, finalText := attemptProgressiveFinalization(ctx, engine, finalizeTool, req, history, opts, tracker, reserve, exitReasonNatural)
			if finalText != "" {
				lastText = finalText
			}
			return buildProgressiveRunResult(opts.SessionID, exitReasonNatural, finalized, tracker.latest, lastText), nil
		}

		if opts.OnEvent != nil {
			if err := opts.OnEvent(llm.Event{Type: llm.EventPhase, Text: "Continuing with remaining budget..."}); err != nil {
				return buildProgressiveRunResult(opts.SessionID, exitReasonNatural, false, tracker.latest, lastText), err
			}
		}

		continueMsg := llm.UserText(expandProgressiveTemplate(opts.ContinueWith, mainCtx))
		history = append(history, continueMsg)
		if opts.OnSyntheticUserMessage != nil {
			if err := opts.OnSyntheticUserMessage(context.Background(), continueMsg); err != nil {
				return buildProgressiveRunResult(opts.SessionID, exitReasonNatural, false, tracker.latest, lastText), err
			}
		}
	}
}

func newProgressTrackerFromMessages(messages []llm.Message) *progressTracker {
	tracker := newProgressTracker()
	for _, msg := range messages {
		for _, part := range msg.Parts {
			switch part.Type {
			case llm.PartToolCall:
				if part.ToolCall == nil {
					continue
				}
				tracker.observeToolCall(strings.TrimSpace(part.ToolCall.ID), strings.TrimSpace(part.ToolCall.Name), part.ToolCall.Arguments)
			case llm.PartToolResult:
				if part.ToolResult == nil {
					continue
				}
				tracker.commitToolCall(strings.TrimSpace(part.ToolResult.ID), strings.TrimSpace(part.ToolResult.Name), !part.ToolResult.IsError)
			}
		}
	}
	tracker.pending = make(map[string]progressCandidate)
	return tracker
}

func runProgressivePass(ctx context.Context, engine *llm.Engine, req llm.Request, opts progressiveRunOptions, tracker *progressTracker) (progressivePassResult, error) {
	var produced []llm.Message
	var text strings.Builder
	var hadNonProgressTool bool
	startSeq := tracker.latestSequence()

	engine.SetResponseCompletedCallback(func(cbCtx context.Context, turnIndex int, assistantMsg llm.Message, metrics llm.TurnMetrics) error {
		produced = append(produced, assistantMsg)
		if opts.OnResponseCompleted != nil {
			return opts.OnResponseCompleted(cbCtx, turnIndex, assistantMsg, metrics)
		}
		return nil
	})
	defer engine.SetResponseCompletedCallback(nil)

	engine.SetTurnCompletedCallback(func(cbCtx context.Context, turnIndex int, messages []llm.Message, metrics llm.TurnMetrics) error {
		produced = append(produced, messages...)
		if opts.OnTurnCompleted != nil {
			return opts.OnTurnCompleted(cbCtx, turnIndex, messages, metrics)
		}
		return nil
	})
	defer engine.SetTurnCompletedCallback(nil)

	stream, err := engine.Stream(ctx, req)
	if err != nil {
		return progressivePassResult{}, err
	}
	defer stream.Close()

	for {
		ev, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return progressivePassResult{
				produced:           produced,
				lastText:           text.String(),
				newCommitCount:     tracker.latestSequence() - startSeq,
				hadNonProgressTool: hadNonProgressTool,
			}, recvErr
		}

		switch ev.Type {
		case llm.EventTextDelta:
			text.WriteString(ev.Text)
		case llm.EventToolCall:
			call := ev.Tool
			if call != nil {
				name := strings.TrimSpace(call.Name)
				callID := strings.TrimSpace(call.ID)
				if callID == "" {
					callID = strings.TrimSpace(ev.ToolCallID)
				}
				if isProgressToolName(name) {
					tracker.observeToolCall(callID, name, call.Arguments)
				} else {
					hadNonProgressTool = true
				}
			}
		case llm.EventToolExecEnd:
			if commit := tracker.commitToolCall(strings.TrimSpace(ev.ToolCallID), strings.TrimSpace(ev.ToolName), ev.ToolSuccess); commit != nil {
				if opts.OnEvent != nil {
					if err := opts.OnEvent(ev); err != nil {
						return progressivePassResult{}, err
					}
					continue
				}
			}
		}

		if opts.OnEvent != nil {
			if err := opts.OnEvent(ev); err != nil {
				return progressivePassResult{}, err
			}
		}
	}

	return progressivePassResult{
		produced:           produced,
		lastText:           text.String(),
		newCommitCount:     tracker.latestSequence() - startSeq,
		hadNonProgressTool: hadNonProgressTool,
	}, nil
}

func attemptProgressiveFinalization(parentCtx context.Context, engine *llm.Engine, finalizeTool llm.Tool, baseReq llm.Request, history []llm.Message, opts progressiveRunOptions, tracker *progressTracker, reserve time.Duration, exitReason string) (bool, string) {
	finalizeCtx := progressiveFinalizationContext(parentCtx, reserve, exitReason)
	if finalizeCtx == nil {
		return false, ""
	}

	finalPrompt := buildProgressiveFinalizePrompt(tracker.latest)
	finalizeMsg := llm.UserText(finalPrompt)
	if opts.OnSyntheticUserMessage != nil {
		if err := opts.OnSyntheticUserMessage(context.Background(), finalizeMsg); err != nil {
			return false, ""
		}
	}

	finalReq := baseReq
	finalReq.Messages = append(append([]llm.Message(nil), history...), finalizeMsg)
	finalReq.Search = false
	finalReq.ForceExternalSearch = false
	finalReq.ParallelToolCalls = false

	if tracker.latest != nil {
		// State has been accumulated — ask model to write prose then call finalize_progress.
		engine.RegisterTool(finalizeTool)
		defer engine.UnregisterTool(finalizeTool.Spec().Name)
		finalReq.Tools = []llm.ToolSpec{finalizeTool.Spec()}
		finalReq.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceAuto}
		if opts.ForceNamedFinalization {
			finalReq.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceName, Name: finalizeTool.Spec().Name}
		}
	} else {
		// No accumulated state — suppress tools so the model writes plain text directly.
		finalReq.Tools = []llm.ToolSpec{}
		finalReq.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceNone}
	}

	passResult, err := runProgressivePass(finalizeCtx, engine, finalReq, opts, tracker)
	if passResult.lastText != "" {
		if commit := tracker.latest; commit == nil {
			return false, passResult.lastText
		}
	}
	if err != nil {
		return false, passResult.lastText
	}
	return tracker.latest != nil && tracker.latest.Final, passResult.lastText
}

func buildProgressiveRunResult(sessionID, exitReason string, finalized bool, commit *progressCommit, fallbackText string) progressiveRunResult {
	result := progressiveRunResult{
		ExitReason: exitReason,
		Finalized:  finalized,
		SessionID:  strings.TrimSpace(sessionID),
	}
	if commit != nil {
		result.Sequence = commit.Sequence
		result.Reason = commit.Reason
		result.Message = commit.Message
		result.Progress = cloneProgressState(commit.State)
	}
	if strings.TrimSpace(fallbackText) != "" {
		if result.Progress == nil {
			result.FallbackText = fallbackText
		} else if finalized {
			// The finalization pass produced prose — surface it as the readable answer.
			result.FinalResponse = fallbackText
		}
	}
	return result
}

func progressiveWorkContext(parent context.Context) (context.Context, time.Duration, bool) {
	deadline, ok := parent.Deadline()
	if !ok {
		return parent, 0, false
	}
	total := time.Until(deadline)
	reserve := progressiveFinalizeReserve(total)
	if reserve <= 0 || total <= reserve {
		return parent, reserve, true
	}
	ctx, _ := context.WithDeadline(parent, deadline.Add(-reserve))
	return ctx, reserve, true
}

func progressiveHasRemainingBudget(parent context.Context, reserve time.Duration) bool {
	deadline, ok := parent.Deadline()
	if !ok {
		return false
	}
	return time.Until(deadline) > reserve
}

func progressiveFinalizeReserve(total time.Duration) time.Duration {
	if total <= 0 {
		return 0
	}
	reserve := total / 10
	if reserve < progressiveMinFinalizeBudget {
		reserve = progressiveMinFinalizeBudget
	}
	if reserve > progressiveMaxFinalizeBudget {
		reserve = progressiveMaxFinalizeBudget
	}
	return reserve
}

func progressiveFinalizationContext(parent context.Context, reserve time.Duration, exitReason string) context.Context {
	grace := reserve
	if grace < progressiveDefaultFinalizeGrace {
		grace = progressiveDefaultFinalizeGrace
	}
	if exitReason == exitReasonNatural {
		if grace < progressiveMinFinalizeBudget {
			grace = progressiveMinFinalizeBudget
		}
	}
	if grace <= 0 {
		return nil
	}
	ctx, _ := context.WithTimeout(context.WithoutCancel(parent), grace)
	return ctx
}

func buildProgressiveFinalizePrompt(latest *progressCommit) string {
	var b strings.Builder
	b.WriteString("Time budget is ending. Stop all tool calls immediately.\n")
	if latest != nil && latest.State != nil {
		b.WriteString("Write a complete, well-formatted human-readable response to the original question using your best-so-far state.\n")
		b.WriteString("After writing your response, call finalize_progress with final=true and reason=finalize to save the structured state.\n")
		if data, err := json.Marshal(latest.State); err == nil && len(data) > 0 {
			b.WriteString("Current best state:\n")
			b.Write(data)
			b.WriteString("\n")
		}
	} else {
		b.WriteString("Write a complete, well-formatted human-readable response to the original question based on everything you have found so far.\n")
		b.WriteString("Do not call any tools. Write your response as plain text now.\n")
	}
	return b.String()
}

func defaultProgressiveContinuePrompt() string {
	return "{{remaining}}Continue working on the same task. Do not stop because you already have a plausible answer. Use the remaining budget to verify risky claims, explore credible alternatives, find counterevidence, strengthen the current best answer, and identify failure modes. Call update_progress if the best-so-far state materially improves. Only stop early if the task is genuinely exhausted or blocked."
}

// expandProgressiveTemplate replaces {{remaining}} with a human-readable
// representation of how much time is left on ctx's deadline.
// If ctx has no deadline, {{remaining}} is replaced with an empty string.
func expandProgressiveTemplate(text string, ctx context.Context) string {
	if !strings.Contains(text, "{{remaining}}") {
		return text
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return strings.ReplaceAll(text, "{{remaining}}", "")
	}
	remaining := time.Until(deadline)
	if remaining < 0 {
		remaining = 0
	}
	return strings.ReplaceAll(text, "{{remaining}}", formatProgressiveDuration(remaining)+" remaining. ")
}

func formatProgressiveDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if m == 0 {
		return fmt.Sprintf("%ds", s)
	}
	if s == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm%ds", m, s)
}

func progressiveExitReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return exitReasonTimeout
	case errors.Is(err, context.Canceled):
		return exitReasonCancelled
	case strings.Contains(err.Error(), "agentic loop exceeded max turns"):
		return exitReasonMaxTurns
	default:
		return exitReasonException
	}
}

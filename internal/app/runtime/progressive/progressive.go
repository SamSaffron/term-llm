package progressive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/jobs"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	toolpkg "github.com/samsaffron/term-llm/internal/tools"
)

type StopWhen string

const (
	StopWhenDone    StopWhen = "done"
	StopWhenTimeout StopWhen = "timeout"

	DefaultFinalizeGrace    = 5 * time.Minute
	MaxFinalizeBudget       = 5 * time.Minute
	MinFinalizeBudget       = 5 * time.Second
	SyntheticMessageTimeout = 5 * time.Second
)

type AskOptions struct {
	Enabled      bool
	Timeout      time.Duration
	StopWhen     StopWhen
	ContinueWith string
}

type RunOptions struct {
	StopWhen               StopWhen
	ContinueWith           string
	SessionID              string
	ForceNamedFinalization bool
	OnEvent                func(core.Event) error
	OnSyntheticUserMessage func(context.Context, core.Message) error
	OnResponseCompleted    engine.ResponseCompletedCallback
	OnTurnCompleted        engine.TurnCompletedCallback
}

type RunResult struct {
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
	produced           []core.Message
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

type Commit struct {
	Sequence int
	Reason   string
	Message  string
	Final    bool
	State    map[string]any
}

type Tracker struct {
	pending map[string]progressCandidate
	latest  *Commit
}

func NewTracker() *Tracker {
	return &Tracker{
		pending: make(map[string]progressCandidate),
	}
}

func (t *Tracker) latestSequence() int {
	if t.latest == nil {
		return 0
	}
	return t.latest.Sequence
}

func (t *Tracker) ObserveToolCall(callID, name string, args json.RawMessage) {
	if !IsProgressTool(name) || strings.TrimSpace(callID) == "" {
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

func (t *Tracker) CommitToolCall(callID, name string, success bool) *Commit {
	if !success || !IsProgressTool(name) || strings.TrimSpace(callID) == "" {
		delete(t.pending, callID)
		return nil
	}
	candidate, ok := t.pending[callID]
	delete(t.pending, callID)
	if !ok {
		return nil
	}
	commit := &Commit{
		Sequence: t.latestSequence() + 1,
		Reason:   candidate.Reason,
		Message:  candidate.Message,
		Final:    candidate.Final,
		State:    cloneProgressState(candidate.State),
	}
	t.latest = commit
	return commit
}

func (t *Tracker) latestState() map[string]any {
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

func IsProgressTool(name string) bool {
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

func ValidateAskOptions(opts *AskOptions) error {
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
			opts.StopWhen = StopWhenTimeout
		} else {
			opts.StopWhen = StopWhenDone
		}
	}
	switch opts.StopWhen {
	case StopWhenDone, StopWhenTimeout:
	default:
		return fmt.Errorf("invalid --stop-when %q", opts.StopWhen)
	}

	if continueWithSet && opts.StopWhen != StopWhenTimeout {
		return fmt.Errorf("--continue-with requires --stop-when timeout")
	}
	if opts.StopWhen == StopWhenTimeout && opts.Timeout <= 0 {
		return fmt.Errorf("--stop-when timeout requires --timeout")
	}
	return nil
}

func RunSession(ctx context.Context, engine *engine.Engine, req core.Request, opts RunOptions) (RunResult, error) {
	if opts.StopWhen == "" {
		opts.StopWhen = StopWhenDone
	}
	if strings.TrimSpace(opts.ContinueWith) == "" {
		opts.ContinueWith = defaultProgressiveContinuePrompt()
	}

	updateTool := toolpkg.NewUpdateProgressTool()
	finalizeTool := toolpkg.NewFinalizeProgressTool()
	engine.RegisterTool(updateTool)
	defer engine.UnregisterTool(updateTool.Spec().Name)

	history := append([]core.Message(nil), req.Messages...)
	tracker := newProgressTrackerFromMessages(history)
	lastText := ""

	mainCtx, cancelMainCtx, reserve, hasTimeoutBudget := progressiveWorkContext(ctx)
	defer cancelMainCtx()

	for {
		passReq := req
		passReq.Messages = append([]core.Message(nil), history...)
		passReq.Tools = append([]core.ToolSpec(nil), req.Tools...)
		passReq.Tools = append(passReq.Tools, updateTool.Spec())

		passResult, err := runProgressivePass(mainCtx, engine, passReq, opts, tracker)
		if passResult.lastText != "" {
			lastText = passResult.lastText
		}
		history = append(history, passResult.produced...)
		if err != nil {
			exitReason := progressiveExitReason(err)
			result := BuildRunResult(opts.SessionID, exitReason, false, tracker.latest, lastText)
			if exitReason == jobs.ExitReasonTimeout || exitReason == jobs.ExitReasonCancelled {
				finalized, finalText := attemptProgressiveFinalization(ctx, engine, finalizeTool, req, history, opts, tracker, reserve, exitReason)
				if finalText != "" {
					lastText = finalText
				}
				result = BuildRunResult(opts.SessionID, exitReason, finalized, tracker.latest, lastText)
				return result, nil
			}
			return result, err
		}

		exitReason := jobs.ExitReasonNatural
		if opts.StopWhen == StopWhenTimeout && hasTimeoutBudget && !progressiveHasRemainingBudget(ctx, reserve) {
			exitReason = jobs.ExitReasonTimeout
		}
		if opts.StopWhen != StopWhenTimeout || !hasTimeoutBudget || exitReason == jobs.ExitReasonTimeout {
			finalized, finalText := attemptProgressiveFinalization(ctx, engine, finalizeTool, req, history, opts, tracker, reserve, exitReason)
			if finalText != "" {
				lastText = finalText
			}
			return BuildRunResult(opts.SessionID, exitReason, finalized, tracker.latest, lastText), nil
		}

		if passResult.newCommitCount == 0 && !passResult.hadNonProgressTool {
			finalized, finalText := attemptProgressiveFinalization(ctx, engine, finalizeTool, req, history, opts, tracker, reserve, jobs.ExitReasonNatural)
			if finalText != "" {
				lastText = finalText
			}
			return BuildRunResult(opts.SessionID, jobs.ExitReasonNatural, finalized, tracker.latest, lastText), nil
		}

		if opts.OnEvent != nil {
			if err := opts.OnEvent(core.Event{Type: core.EventPhase, Text: "Continuing with remaining budget..."}); err != nil {
				return BuildRunResult(opts.SessionID, jobs.ExitReasonNatural, false, tracker.latest, lastText), err
			}
		}

		continueMsg := core.UserText(expandProgressiveTemplate(opts.ContinueWith, mainCtx))
		history = append(history, continueMsg)
		if opts.OnSyntheticUserMessage != nil {
			msgCtx, cancel := SyntheticUserMessageContext(mainCtx)
			err := opts.OnSyntheticUserMessage(msgCtx, continueMsg)
			cancel()
			if err != nil {
				return BuildRunResult(opts.SessionID, jobs.ExitReasonNatural, false, tracker.latest, lastText), err
			}
		}
	}
}

func newProgressTrackerFromMessages(messages []core.Message) *Tracker {
	tracker := NewTracker()
	for _, msg := range messages {
		for _, part := range msg.Parts {
			switch part.Type {
			case core.PartToolCall:
				if part.ToolCall == nil {
					continue
				}
				tracker.ObserveToolCall(strings.TrimSpace(part.ToolCall.ID), strings.TrimSpace(part.ToolCall.Name), part.ToolCall.Arguments)
			case core.PartToolResult:
				if part.ToolResult == nil {
					continue
				}
				tracker.CommitToolCall(strings.TrimSpace(part.ToolResult.ID), strings.TrimSpace(part.ToolResult.Name), !part.ToolResult.IsError)
			}
		}
	}
	tracker.pending = make(map[string]progressCandidate)
	return tracker
}

func runProgressivePass(ctx context.Context, engine *engine.Engine, req core.Request, opts RunOptions, tracker *Tracker) (progressivePassResult, error) {
	var produced []core.Message
	var text strings.Builder
	var hadNonProgressTool bool
	startSeq := tracker.latestSequence()

	engine.SetResponseCompletedCallback(func(cbCtx context.Context, turnIndex int, assistantMsg core.Message, metrics core.TurnMetrics) error {
		var err error
		if opts.OnResponseCompleted != nil {
			err = opts.OnResponseCompleted(cbCtx, turnIndex, assistantMsg, metrics)
		}
		if err == nil {
			produced = append(produced, assistantMsg)
		}
		return err
	})
	defer engine.SetResponseCompletedCallback(nil)

	engine.SetTurnCompletedCallback(func(cbCtx context.Context, turnIndex int, messages []core.Message, metrics core.TurnMetrics) error {
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
		case core.EventTextDelta:
			text.WriteString(ev.Text)
		case core.EventToolCall:
			call := ev.Tool
			if call != nil {
				name := strings.TrimSpace(call.Name)
				callID := strings.TrimSpace(call.ID)
				if callID == "" {
					callID = strings.TrimSpace(ev.ToolCallID)
				}
				if IsProgressTool(name) {
					tracker.ObserveToolCall(callID, name, call.Arguments)
				} else {
					hadNonProgressTool = true
				}
			}
		case core.EventToolExecEnd:
			if commit := tracker.CommitToolCall(strings.TrimSpace(ev.ToolCallID), strings.TrimSpace(ev.ToolName), ev.ToolSuccess); commit != nil {
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

func attemptProgressiveFinalization(parentCtx context.Context, engine *engine.Engine, finalizeTool core.Tool, baseReq core.Request, history []core.Message, opts RunOptions, tracker *Tracker, reserve time.Duration, exitReason string) (bool, string) {
	finalizeCtx, finalizeCancel := progressiveFinalizationContext(parentCtx, reserve, exitReason)
	if finalizeCtx == nil {
		return false, ""
	}
	defer finalizeCancel()

	finalPrompt := buildProgressiveFinalizePrompt(tracker.latest)
	finalizeMsg := core.UserText(finalPrompt)
	if opts.OnSyntheticUserMessage != nil {
		msgCtx, cancel := SyntheticUserMessageContext(finalizeCtx)
		err := opts.OnSyntheticUserMessage(msgCtx, finalizeMsg)
		cancel()
		if err != nil {
			return false, ""
		}
	}

	finalReq := baseReq
	finalReq.Messages = append(append([]core.Message(nil), history...), finalizeMsg)
	finalReq.Search = false
	finalReq.ForceExternalSearch = false
	finalReq.ParallelToolCalls = false

	if tracker.latest != nil {
		// State has been accumulated — ask model to write prose then call finalize_progress.
		engine.RegisterTool(finalizeTool)
		defer engine.UnregisterTool(finalizeTool.Spec().Name)
		finalReq.Tools = []core.ToolSpec{finalizeTool.Spec()}
		finalReq.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceAuto}
		if opts.ForceNamedFinalization {
			finalReq.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceName, Name: finalizeTool.Spec().Name}
		}
	} else {
		// No accumulated state — suppress tools so the model writes plain text directly.
		finalReq.Tools = []core.ToolSpec{}
		finalReq.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceNone}
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

func BuildRunResult(sessionID, exitReason string, finalized bool, commit *Commit, fallbackText string) RunResult {
	result := RunResult{
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

// progressiveWorkContext derives the working context that reserves a slice of the
// parent deadline for finalization. It returns a CancelFunc that the caller must
// invoke (typically via defer) to release the derived context's resources; the
// no-derivation branches return a no-op CancelFunc so the caller can always defer
// unconditionally.
func progressiveWorkContext(parent context.Context) (context.Context, context.CancelFunc, time.Duration, bool) {
	deadline, ok := parent.Deadline()
	if !ok {
		return parent, func() {}, 0, false
	}
	total := time.Until(deadline)
	reserve := progressiveFinalizeReserve(total)
	if reserve <= 0 || total <= reserve {
		return parent, func() {}, reserve, true
	}
	ctx, cancel := context.WithDeadline(parent, deadline.Add(-reserve))
	return ctx, cancel, reserve, true
}

func progressiveHasRemainingBudget(parent context.Context, reserve time.Duration) bool {
	deadline, ok := parent.Deadline()
	if !ok {
		return false
	}
	return time.Until(deadline) > reserve
}

// SyntheticUserMessageContext caps synthetic user-message persistence
// to a short window while still honoring the caller's cancellation/deadline.
func SyntheticUserMessageContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, SyntheticMessageTimeout)
}

func progressiveFinalizeReserve(total time.Duration) time.Duration {
	if total <= 0 {
		return 0
	}
	reserve := total / 10
	if reserve < MinFinalizeBudget {
		reserve = MinFinalizeBudget
	}
	if reserve > MaxFinalizeBudget {
		reserve = MaxFinalizeBudget
	}
	return reserve
}

// progressiveFinalizationContext returns a time-boxed context for the finalization
// pass plus its CancelFunc, which the caller must invoke (via defer) to release
// resources. Natural completion gets a detached grace window so the model can add
// final polish; timeout/cancel exits stay attached to the parent context and are
// capped to the reserved budget. It returns (nil, nil) when finalization should
// be skipped.
func progressiveFinalizationContext(parent context.Context, reserve time.Duration, exitReason string) (context.Context, context.CancelFunc) {
	if exitReason == jobs.ExitReasonNatural {
		grace := reserve
		if grace < DefaultFinalizeGrace {
			grace = DefaultFinalizeGrace
		}
		if grace < MinFinalizeBudget {
			grace = MinFinalizeBudget
		}
		if grace <= 0 {
			return nil, nil
		}
		return context.WithTimeout(context.WithoutCancel(parent), grace)
	}

	if reserve <= 0 || parent.Err() != nil {
		return nil, nil
	}
	return context.WithTimeout(parent, reserve)
}

func buildProgressiveFinalizePrompt(latest *Commit) string {
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
		return jobs.ExitReasonTimeout
	case errors.Is(err, context.Canceled):
		return jobs.ExitReasonCancelled
	case engine.IsMaxTurnsExceeded(err):
		return jobs.ExitReasonMaxTurns
	default:
		return jobs.ExitReasonException
	}
}

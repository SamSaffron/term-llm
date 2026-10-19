package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"io"
	"strings"
	"sync"

	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/restart"
)

// Stream returns a stream, applying external tools when needed.
func (e *Engine) Stream(ctx context.Context, req core.Request) (core.Stream, error) {
	if resume := req.Resume; resume != nil {
		req = resume.Request
		req.Resume = resume
		if len(resume.ProviderState) > 0 {
			if importer, ok := e.provider.(core.ProviderStateImporter); ok {
				if err := importer.ImportProviderState(resume.ProviderState); err != nil {
					return nil, fmt.Errorf("restore provider continuation: %w", err)
				}
			}
		}
	}
	ctx = env.withDebugDiagnosticSink(ctx, e.debugLogger)
	// Until this request is proven agentic, it has no boundary at which it can
	// consume steering. This also clears accepting state left by a prior run.
	e.markSteeringRunNonConsuming()
	req.Messages = core.FilterConversationMessages(req.Messages)

	caps := e.provider.Capabilities()

	// 1. Handle external search/fetch tool injection
	// If Search is enabled, add web_search and read_url tools to the tool list.
	// The LLM will use them naturally during conversation like any other tool.
	if req.Search {
		needsExternalSearch := !caps.NativeWebSearch || req.ForceExternalSearch
		needsExternalFetch := (!caps.NativeWebFetch || req.ForceExternalSearch) && !req.DisableExternalWebFetch

		// Callers commonly pre-populate req.Tools from the full registry. Remove
		// external search tools when the provider owns that capability so native
		// search is exclusive rather than silently competing with (for example)
		// an Exa-backed MCP tool. Forced external search keeps the external tools.
		req.Tools = filterExternalSearchTools(req.Tools, needsExternalSearch, needsExternalFetch)

		if needsExternalSearch {
			if t, ok := e.tools.Get(core.WebSearchToolName); ok {
				if !hasToolNamed(req.Tools, core.WebSearchToolName) {
					req.Tools = append(req.Tools, t.Spec())
				}
			}
		}
		if needsExternalFetch {
			if t, ok := e.tools.Get(core.ReadURLToolName); ok {
				if !hasToolNamed(req.Tools, core.ReadURLToolName) {
					req.Tools = append(req.Tools, t.Spec())
				}
			}
		}
	}

	// Force external search means "do not use provider-native search".
	// Keep req.Search=true only for providers that must handle native search.
	if req.ForceExternalSearch && caps.NativeWebSearch {
		req.Search = false
	}

	// Keep the provider-visible tool surface aligned with execution policy. This
	// matters for explicit-empty skill filters and for restrictions activated
	// between agentic turns.
	req.Tools = e.FilterAllowedToolSpecs(req.Tools)
	var planner core.ToolSurfacePlanner
	if req.EnableToolDiscovery {
		planner = e.currentToolPlanner()
	}
	validateNamedChoice := func(choice core.ToolChoice) error {
		if choice.Mode != core.ToolChoiceName || hasToolNamed(req.Tools, choice.Name) {
			return nil
		}
		// The only missing forced name a planner may defer validation for is the
		// exact authorised MCP wrapper it can make visible at BeginRun.
		if planner != nil && planner.CanActivateDeferredTool(choice.Name) {
			return nil
		}
		return fmt.Errorf("selected tool %q is not allowed by the active tool filter", choice.Name)
	}
	if err := validateNamedChoice(req.ToolChoice); err != nil {
		return nil, err
	}
	if req.LastTurnToolChoice != nil {
		if err := validateNamedChoice(*req.LastTurnToolChoice); err != nil {
			return nil, err
		}
	}
	if len(req.Tools) == 0 && planner == nil {
		req.ToolChoice = core.ToolChoice{}
		req.LastTurnToolChoice = nil
	}

	// Restorable session context is capability-gated by the final filtered specs
	// and provider support. Requests without such a configured tool never touch
	// its controller or store.
	if caps.ToolCalls {
		e.prepareRequestContext(ctx, &req)
	}

	if req.DebugRaw {
		debugReq := e.prepareProviderRequest(req)
		env.debugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), debugReq, "Request")
	}

	// 2. Decide if we use the agentic loop
	// We use it if request has tools, or this request opted into a discovery
	// planner that may add authorised MCP tools, and the provider supports calls.
	// Providers with a true mid-generation interrupt also need the loop when no
	// tools are exposed: the next provider turn is where the queued steer is
	// committed and delivered after the interrupted turn.
	useLoop := restart.CurrentTask(ctx) != nil || req.Resume != nil || (caps.ToolCalls && (len(req.Tools) > 0 || planner != nil || e.providerSupportsImmediateInterruption()))

	if useLoop {
		e.beginSteeringRun(getMaxTurns(req) > 1)
		if req.SessionID != "" {
			// Tools read this for session-scoped concerns like file-change tracking.
			ctx = core.ContextWithSessionID(ctx, req.SessionID)
		}
		stream := corestream.New(ctx, func(ctx context.Context, send corestream.Sender) error {
			return e.runLoop(ctx, req, send)
		})
		stream = wrapLoggingStream(stream, e.provider.Name(), req.Model)
		stream = e.wrapDebugLoggingStream(stream)

		// Wrap with per-turn cleanup for providers that materialize temporary
		// prompt/image files. Conversation-scoped CleanupMCP is not invoked here;
		// it runs on runtime eviction or server shutdown.
		if cleaner, ok := e.provider.(ProviderTurnCleaner); ok {
			stream = &cleanupStream{inner: stream, cleanup: cleaner.CleanupTurn}
		}

		return stream, nil
	}

	// 3. Simple stream (no tools or no provider support for tools). Model output is
	// staged in an attempt-local scratchpad until the stream completes; if the
	// transport fails first, we can discard the scratchpad and replay safely.
	runID := e.beginToolRun()
	if req.SessionID != "" {
		ctx = core.ContextWithSessionID(ctx, req.SessionID)
	}
	ctx = core.ContextWithToolRunID(ctx, runID)
	if e.debugLogger != nil {
		debugReq := e.prepareProviderRequest(req)
		e.debugLogger.LogRequest(e.provider.Name(), req.Model, debugReq)
	}
	stream := corestream.New(ctx, func(ctx context.Context, send corestream.Sender) error {
		return e.runSimpleScratchpad(ctx, req, send)
	})
	stream = wrapLoggingStream(stream, e.provider.Name(), req.Model)
	stream = e.wrapDebugLoggingStream(stream)
	stream = &fileTrackingRunStream{inner: stream, start: func() {
		e.recordFileTrackingRunStart(ctx, req.SessionID, runID)
	}, complete: func() {
		e.recordFileTrackingRunComplete(ctx, req.SessionID, runID)
		e.endToolRun(runID)
	}}
	return stream, nil
}

type fileTrackingRunStream struct {
	inner     core.Stream
	start     func()
	complete  func()
	startOnce sync.Once
	once      sync.Once
}

func (s *fileTrackingRunStream) begin() {
	s.startOnce.Do(func() {
		if s.start != nil {
			s.start()
		}
	})
}
func (s *fileTrackingRunStream) finish() {
	s.once.Do(s.complete)
}
func (s *fileTrackingRunStream) Recv() (core.Event, error) {
	s.begin()
	event, err := s.inner.Recv()
	if err != nil || event.Type == core.EventDone || event.Type == core.EventError {
		s.finish()
	}
	return event, err
}
func (s *fileTrackingRunStream) Close() error {
	s.begin()
	err := s.inner.Close()
	s.finish()
	return err
}

// wrapCallbackStream wraps a stream to call the turn callback on completion.
// Used for simple (non-agentic) streams to enable incremental session saving.
func wrapCallbackStream(ctx context.Context, inner core.Stream, cb TurnCompletedCallback) core.Stream {
	return &callbackStream{
		inner:              inner,
		ctx:                ctx,
		text:               &strings.Builder{},
		reasoning:          &strings.Builder{},
		metrics:            core.TurnMetrics{},
		callback:           cb,
		reasoningItemID:    "",
		reasoningEncrypted: "",
		reasoningKind:      "",
	}
}

// callbackStream wraps a stream to accumulate text/usage and call callback on EOF.
type callbackStream struct {
	inner     core.Stream
	ctx       context.Context
	mu        sync.Mutex
	text      *strings.Builder
	reasoning *strings.Builder
	// reasoningTextItemID tracks only text-bearing items for display boundaries;
	// reasoningItemID also tracks metadata-only events for provider replay.
	reasoningTextItemID   string
	reasoningItemID       string
	reasoningEncrypted    string
	reasoningKind         core.ReasoningKind
	reasoningSummaryParts []string
	metrics               core.TurnMetrics
	callback              TurnCompletedCallback
	done                  bool
}

func (s *callbackStream) Recv() (core.Event, error) {
	event, err := s.inner.Recv()
	if err == io.EOF {
		// Call callback with accumulated content on normal completion
		s.fireCallback()
		return event, err
	}
	if err != nil {
		// Call callback on error too (best-effort save of partial output)
		s.fireCallback()
		return event, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Accumulate text and usage
	if event.Type == core.EventAttemptDiscard {
		s.text.Reset()
		s.reasoning.Reset()
		s.reasoningTextItemID = ""
		s.reasoningItemID = ""
		s.reasoningEncrypted = ""
		s.reasoningKind = ""
		s.reasoningSummaryParts = nil
		s.metrics = core.TurnMetrics{}
		return event, nil
	}
	if event.Type == core.EventTextDelta && event.Text != "" {
		s.text.WriteString(event.Text)
	}
	if event.Type == core.EventUsage && event.Use != nil {
		s.metrics.InputTokens += event.Use.InputTokens
		s.metrics.OutputTokens += event.Use.OutputTokens
		s.metrics.CachedInputTokens += event.Use.CachedInputTokens
		s.metrics.CacheWriteTokens += event.Use.CacheWriteTokens
	}
	if event.Type == core.EventReasoningDelta {
		internalreasoning.AppendStreamItemText(s.reasoning, &s.reasoningTextItemID, event.Text, event.ReasoningItemID)
		if event.Text != "" {
			s.reasoningKind = core.MergeReasoningKind(s.reasoningKind, event.ReasoningKind)
		}
		if len(event.ReasoningSummaryParts) > 0 {
			// Last-write-wins: Responses emits the full summary parts array on each delta.
			s.reasoningSummaryParts = append([]string(nil), event.ReasoningSummaryParts...)
			s.reasoningKind = core.MergeReasoningKind(s.reasoningKind, core.ReasoningKindSummary)
		}
		if event.ReasoningItemID != "" {
			s.reasoningItemID = event.ReasoningItemID
		}
		if event.ReasoningEncryptedContent != "" {
			s.reasoningEncrypted = event.ReasoningEncryptedContent
			s.reasoningKind = core.MergeReasoningKind(s.reasoningKind, event.ReasoningKind)
		}
	}

	return event, nil
}

// fireCallback invokes the callback once if there's accumulated content.
func (s *callbackStream) fireCallback() {
	var (
		cb      TurnCompletedCallback
		msg     core.Message
		metrics core.TurnMetrics
	)

	s.mu.Lock()
	if s.callback != nil && !s.done && (s.text.Len() > 0 || s.reasoning.Len() > 0 || len(s.reasoningSummaryParts) > 0 || s.reasoningItemID != "" || s.reasoningEncrypted != "") {
		reasoningText := s.reasoning.String()
		reasoningKind := core.ReasoningKind("")
		if reasoningText != "" || len(s.reasoningSummaryParts) > 0 || s.reasoningItemID != "" || s.reasoningEncrypted != "" {
			reasoningKind = core.NormalizeReasoningKind(s.reasoningKind)
		}
		if reasoningText == "" && len(s.reasoningSummaryParts) > 0 {
			reasoningText = strings.Join(s.reasoningSummaryParts, "\n\n")
		}
		reasoningTitle := ""
		if reasoningKind == core.ReasoningKindSummary {
			reasoningTitle = internalreasoning.ParseReasoningSummary(reasoningText).Title
		}
		s.done = true
		cb = s.callback
		msg = core.Message{
			Role: core.RoleAssistant,
			Parts: []core.Part{{
				Type:                      core.PartText,
				Text:                      s.text.String(),
				ReasoningContent:          reasoningText,
				ReasoningSummaryParts:     append([]string(nil), s.reasoningSummaryParts...),
				ReasoningItemID:           s.reasoningItemID,
				ReasoningEncryptedContent: s.reasoningEncrypted,
				ReasoningKind:             reasoningKind,
				ReasoningSummaryTitle:     reasoningTitle,
			}},
		}
		metrics = s.metrics
	}
	s.mu.Unlock()

	if cb != nil {
		cbCtx, cancel := callbackContext(s.ctx)
		defer cancel()
		_ = cb(cbCtx, 0, []core.Message{msg}, metrics)
	}
}

func (s *callbackStream) Close() error {
	// Best-effort: fire callback if stream closed without EOF/error
	s.fireCallback()
	return s.inner.Close()
}

func filterExternalSearchTools(tools []core.ToolSpec, keepSearch, keepFetch bool) []core.ToolSpec {
	filtered := make([]core.ToolSpec, 0, len(tools))
	for _, tool := range tools {
		switch tool.Name {
		case core.WebSearchToolName:
			if !keepSearch {
				continue
			}
		case core.ReadURLToolName:
			if !keepFetch {
				continue
			}
		}
		filtered = append(filtered, tool)
	}
	return filtered
}

// hasToolNamed checks if a tool with the given name exists in the tool list.
func hasToolNamed(tools []core.ToolSpec, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

func isUncommittedReplayableStreamError(err error) bool {
	if err == nil {
		return false
	}
	var incomplete *corestream.StreamIncompleteError
	if errors.As(err, &incomplete) {
		return true
	}
	return env.isNonRecoverableStreamError(err)
}

func isCommittedStreamRecoveryError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var incomplete *corestream.StreamIncompleteError
	if errors.As(err, &incomplete) {
		return true
	}
	return env.isNonRecoverableStreamError(err)
}

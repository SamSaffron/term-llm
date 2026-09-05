package ui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/diff"
	"github.com/samsaffron/term-llm/internal/llm"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/tools"
)

// DefaultStreamBufferSize is the default buffer size for the event channel.
// Large enough to handle bursts while still providing backpressure.
const DefaultStreamBufferSize = 100

// StreamAdapter bridges an llm.Stream to a channel of StreamEvents.
// It handles event conversion and provides proper buffering with blocking sends
// to ensure no events are dropped.
type StreamAdapter struct {
	events chan StreamEvent
	stats  *SessionStats

	sendMu    sync.Mutex
	closed    bool
	stopSend  chan struct{}
	closeDone chan struct{}
	senders   sync.WaitGroup
	statsMu   sync.Mutex

	seenToolStarts map[string]struct{}
	seenToolEnds   map[string]struct{}

	attemptInput          int
	attemptOutput         int
	attemptCached         int
	attemptCacheWrite     int
	attemptUsageCalls     int
	attemptUsageCommitted bool
}

// NewStreamAdapter creates a new StreamAdapter with the specified buffer size.
// If bufSize <= 0, DefaultStreamBufferSize is used.
func NewStreamAdapter(bufSize int) *StreamAdapter {
	if bufSize <= 0 {
		bufSize = DefaultStreamBufferSize
	}
	return &StreamAdapter{
		events:         make(chan StreamEvent, bufSize),
		stats:          NewSessionStats(),
		stopSend:       make(chan struct{}),
		closeDone:      make(chan struct{}),
		seenToolStarts: make(map[string]struct{}),
		seenToolEnds:   make(map[string]struct{}),
	}
}

func (a *StreamAdapter) markAttemptCommitted() {
	a.attemptInput, a.attemptOutput, a.attemptCached, a.attemptCacheWrite, a.attemptUsageCalls = 0, 0, 0, 0, 0
	a.attemptUsageCommitted = true
}

func (a *StreamAdapter) resetAttemptUsage() {
	a.attemptInput, a.attemptOutput, a.attemptCached, a.attemptCacheWrite, a.attemptUsageCalls = 0, 0, 0, 0, 0
	a.attemptUsageCommitted = false
}

// Events returns the channel to read events from.
func (a *StreamAdapter) Events() <-chan StreamEvent {
	return a.events
}

// EmitErrorAndClose sends an error event and closes the channel.
// Use this when stream creation fails before ProcessStream can be called.
func (a *StreamAdapter) EmitErrorAndClose(err error) {
	a.emit(context.Background(), ErrorEvent(err))
	a.closeEvents()
}

// EmitGuardian inserts a guardian review into the same ordered event stream as
// tool start/end events. It returns false after the adapter closes or when the
// supplied context is canceled.
func (a *StreamAdapter) EmitGuardian(ctx context.Context, event tools.GuardianEvent) bool {
	if !event.Usage.BillableCountersZero() {
		u := event.Usage
		a.updateStats(func(stats *SessionStats) {
			stats.AddGuardianUsageForModel(event.Model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
		})
	}
	return a.emit(ctx, GuardianReviewEvent(event))
}

func (a *StreamAdapter) updateStats(update func(*SessionStats)) {
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	update(a.stats)
}

func (a *StreamAdapter) emit(ctx context.Context, event StreamEvent) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	a.sendMu.Lock()
	if a.closed {
		a.sendMu.Unlock()
		return false
	}
	a.senders.Add(1)
	stopSend := a.stopSend
	a.sendMu.Unlock()
	defer a.senders.Done()

	select {
	case a.events <- event:
		return true
	case <-stopSend:
		return false
	case <-ctx.Done():
		return false
	}
}

func (a *StreamAdapter) closeEvents() {
	a.sendMu.Lock()
	if a.closed {
		closeDone := a.closeDone
		a.sendMu.Unlock()
		<-closeDone
		return
	}
	a.closed = true
	close(a.stopSend)
	a.sendMu.Unlock()

	a.senders.Wait()
	close(a.events)
	close(a.closeDone)
}

// Stats returns the session stats being tracked.
func (a *StreamAdapter) Stats() *SessionStats {
	return a.stats
}

// ProcessStream reads events from the llm.Stream and sends them to the events channel.
// This method blocks until the stream is exhausted or an error occurs.
// The events channel is closed when this method returns.
//
// Call this in a goroutine:
//
//	go adapter.ProcessStream(ctx, stream)
func (a *StreamAdapter) ProcessStream(ctx context.Context, stream llm.Stream) {
	if ctx == nil {
		ctx = context.Background()
	}
	defer a.closeEvents()
	a.updateStats(func(stats *SessionStats) { stats.RequestStart() })

	emit := func(event StreamEvent) bool {
		return a.emit(ctx, event)
	}

	var totalTokens int
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		event, err := stream.Recv()
		if err == io.EOF {
			emit(DoneEvent(totalTokens))
			return
		}
		if err != nil {
			// Check if context was cancelled
			if ctx.Err() != nil {
				return
			}
			emit(ErrorEvent(err))
			return
		}

		switch event.Type {
		case llm.EventError:
			if event.Err != nil {
				emit(ErrorEvent(event.Err))
				return
			}

		case llm.EventTextDelta:
			a.attemptUsageCommitted = false
			if event.Text != "" {
				a.updateStats(func(stats *SessionStats) { stats.ObserveOutput() })
				if !emit(TextEvent(event.Text)) {
					return
				}
			}

		case llm.EventReasoningDelta:
			a.attemptUsageCommitted = false
			kind := llm.NormalizeReasoningKind(event.ReasoningKind)
			if llm.IsEncryptedReasoningDelta(event) {
				a.updateStats(func(stats *SessionStats) { stats.ObserveOutput() })
				// Preserve generation timing without exposing encrypted replay data.
				if !emit(GenerationActivityEvent()) {
					return
				}
				continue
			}
			if event.Text == "" && event.ReasoningItemID == "" && !event.ReasoningFinal {
				continue
			}
			if event.Text != "" {
				a.updateStats(func(stats *SessionStats) { stats.ObserveOutput() })
			}
			title := ""
			displayable := kind == llm.ReasoningKindSummary
			if kind == llm.ReasoningKindSummary && event.Text != "" {
				title = internalreasoning.ParseReasoningSummary(event.Text).Title
			}
			if !emit(ReasoningEvent(kind, event.Text, title, event.ReasoningItemID, event.ReasoningFinal, displayable)) {
				return
			}

		case llm.EventDiscoveryCall:
			if event.DiscoveryCall == nil {
				continue
			}
			a.markAttemptCommitted()
			callID := event.DiscoveryCall.ID
			if callID != "" {
				a.seenToolStarts[callID] = struct{}{}
			}
			if !emit(ToolStartEvent(callID, "tool_search", "(native client discovery)", event.DiscoveryCall.Arguments)) {
				return
			}
			a.updateStats(func(stats *SessionStats) { stats.ToolStart() })

		case llm.EventDiscoveryOutput:
			if event.DiscoveryOutput == nil {
				continue
			}
			callID := event.DiscoveryOutput.CallID
			if callID != "" {
				a.seenToolEnds[callID] = struct{}{}
			}
			names := make([]string, 0, len(event.DiscoveryOutput.Tools))
			for _, selected := range event.DiscoveryOutput.Tools {
				names = append(names, selected.Spec.Name)
			}
			infoRaw, _ := json.Marshal(map[string]any{
				"execution":            "client",
				"catalogue_generation": event.DiscoveryOutput.CatalogueGen,
				"catalogue_hash":       event.DiscoveryOutput.CatalogueHash,
				"tools":                names,
			})
			if !emit(ToolEndEvent(callID, "tool_search", string(infoRaw), true)) {
				return
			}
			a.updateStats(func(stats *SessionStats) { stats.ToolEnd() })
			a.resetAttemptUsage()

		case llm.EventToolCall:
			// A tool call is a durable boundary. Usage from the provider attempt that
			// produced it must not be rolled back by a later provisional retry discard.
			a.markAttemptCommitted()
			// Tool call announced during streaming - preserves interleaving order
			if event.Tool != nil {
				toolCallID := event.ToolCallID
				if toolCallID == "" {
					toolCallID = event.Tool.ID
				}
				// Skip if already seen (prevents double-counting with EventToolExecStart).
				// If toolCallID is empty, don't dedupe - treat each call as unique.
				if toolCallID != "" {
					if _, ok := a.seenToolStarts[toolCallID]; ok {
						continue
					}
					a.seenToolStarts[toolCallID] = struct{}{}
				}
				toolInfo := event.ToolInfo
				if toolInfo == "" {
					toolInfo = llm.ExtractToolInfo(*event.Tool)
				}
				toolArgs := event.ToolArgs
				if len(toolArgs) == 0 {
					toolArgs = event.Tool.Arguments
				}
				uiEvent := ToolStartEvent(toolCallID, event.Tool.Name, toolInfo, toolArgs)
				if !emit(uiEvent) {
					return
				}
				a.updateStats(func(stats *SessionStats) { stats.ToolStart() })
			}

		case llm.EventToolExecStart:
			// Tool execution is also durable/replayed from the engine journal.
			a.markAttemptCommitted()
			// Skip if already seen. If toolCallID is empty, don't dedupe - treat as unique.
			if event.ToolCallID != "" {
				if _, ok := a.seenToolStarts[event.ToolCallID]; ok {
					continue
				}
				a.seenToolStarts[event.ToolCallID] = struct{}{}
			}
			uiEvent := ToolStartEvent(event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolArgs)
			if !emit(uiEvent) {
				return
			}
			a.updateStats(func(stats *SessionStats) { stats.ToolStart() })

		case llm.EventToolExecEnd:
			// Skip if already seen. If toolCallID is empty, don't dedupe - treat as unique.
			if event.ToolCallID != "" {
				if _, ok := a.seenToolEnds[event.ToolCallID]; ok {
					continue
				}
				a.seenToolEnds[event.ToolCallID] = struct{}{}
			}
			uiEvent := ToolEndEvent(event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolSuccess)
			if !emit(uiEvent) {
				return
			}
			a.updateStats(func(stats *SessionStats) { stats.ToolEnd() })
			a.resetAttemptUsage()

			// Emit image events from structured data
			for _, imagePath := range event.ToolImages {
				if !emit(ImageEvent(imagePath)) {
					return
				}
			}
			for _, media := range event.ToolMedia {
				if !emit(MediaEvent(media)) {
					return
				}
			}
			// Emit diff events from structured data
			for _, d := range event.ToolDiffs {
				if !emit(DiffEventWithOperation(d.File, d.Old, d.New, d.Line, d.Operation)) {
					return
				}
			}
			// Emit file-change metadata events (file tracking)
			for _, fc := range event.ToolFileChanges {
				if !emit(FileChangeEvent(fc)) {
					return
				}
			}
			for _, observation := range event.ToolFilesystemObservations {
				if !emit(FilesystemObservationEvent(observation)) {
					return
				}
			}
			for _, diagnostic := range event.ToolOutputClaimDiagnostics {
				if !emit(OutputClaimDiagnosticEvent(diagnostic)) {
					return
				}
			}

		case llm.EventRetry:
			a.updateStats(func(stats *SessionStats) { stats.ScheduleRetryStart(event.RetryWaitSecs) })
			if !emit(RetryEvent(event.RetryAttempt, event.RetryMaxAttempts, event.RetryWaitSecs)) {
				return
			}

		case llm.EventAttemptDiscard:
			a.updateStats(func(stats *SessionStats) {
				stats.DiscardUsage(a.attemptInput, a.attemptOutput, a.attemptCached, a.attemptCacheWrite, a.attemptUsageCalls)
			})
			a.resetAttemptUsage()
			if !emit(AttemptDiscardEvent()) {
				return
			}

		case llm.EventUsage:
			a.updateStats(func(stats *SessionStats) { stats.GenerationEnd() })
			if event.Use != nil {
				totalTokens = event.Use.OutputTokens
				usageEvent := UsageEvent(event.Use.InputTokens, event.Use.OutputTokens, event.Use.CachedInputTokens, event.Use.CacheWriteTokens)
				if !emit(usageEvent) {
					return
				}
				a.updateStats(func(stats *SessionStats) {
					stats.AddUsage(event.Use.InputTokens, event.Use.OutputTokens, event.Use.CachedInputTokens, event.Use.CacheWriteTokens)
				})
				if !event.Use.BillableCountersZero() && !a.attemptUsageCommitted {
					a.attemptInput += event.Use.InputTokens
					a.attemptOutput += event.Use.OutputTokens
					a.attemptCached += event.Use.CachedInputTokens
					a.attemptCacheWrite += event.Use.CacheWriteTokens
					a.attemptUsageCalls++
				}
			}

		case llm.EventPhase:
			if event.Text != "" {
				if !emit(PhaseEvent(event.Text)) {
					return
				}
			}

		case llm.EventModelSwitch:
			model := event.Model
			if model == "" {
				model = event.Text
			}
			if model != "" {
				a.updateStats(func(stats *SessionStats) { stats.SetModel(model) })
				if !emit(ModelSwitchEvent(model, event.ReasoningEffort)) {
					return
				}
			}

		case llm.EventSteering:
			if event.Text != "" {
				if !emit(SteeringEventWithMessage(event.Text, event.SteeringID, event.Message)) {
					return
				}
			}
		}
	}
}

// DiffData is an alias for llm.DiffData for backward compatibility.
type DiffData = llm.DiffData

// ParseDiffMarkers extracts diff data from tool output that contain __DIFF__: markers.
// Used for backward compatibility when rendering old sessions that have markers in Display/Content.
// Format: __DIFF__:<base64-encoded JSON>
func ParseDiffMarkers(output string) []DiffData {
	var diffs []DiffData
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "__DIFF__:") {
			encoded := strings.TrimPrefix(line, "__DIFF__:")
			encoded = strings.TrimSpace(encoded)
			if encoded == "" {
				continue
			}
			// Check decoded size before allocating (prevent large buffer allocation)
			decodedLen := base64.StdEncoding.DecodedLen(len(encoded))
			if decodedLen > diff.MaxDiffSize {
				continue
			}
			// Decode base64
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				continue
			}
			// Parse JSON
			var d DiffData
			if err := json.Unmarshal(decoded, &d); err != nil {
				continue
			}
			if d.File != "" {
				diffs = append(diffs, d)
			}
		}
	}
	return diffs
}

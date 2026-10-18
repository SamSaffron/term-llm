package responses

import (
	"encoding/json"
	"fmt"
	"strings"
)

type responsesToolState struct {
	calls map[int]*responsesToolCallState // keyed by output_index (stable across events)
	order []int                           // order of output_index values
}

type responsesToolCallState struct {
	outputIndex int    // Output index - stable across added/delta/done events
	callID      string // Actual call ID (call_xxx) - used in tool results
	name        string
	namespace   string
	caller      string
	args        strings.Builder
	finished    bool
}

func newResponsesToolState() *responsesToolState {
	return &responsesToolState{calls: make(map[int]*responsesToolCallState)}
}

// StartCall starts tracking a new tool call.
// outputIndex is the stable index across events, callID is the actual call ID (call_xxx).
func (s *responsesToolState) StartCall(outputIndex int, callID, namespace, name string) {
	if _, exists := s.calls[outputIndex]; exists {
		return
	}
	s.calls[outputIndex] = &responsesToolCallState{outputIndex: outputIndex, callID: callID, name: name, namespace: namespace}
	s.order = append(s.order, outputIndex)
}

func (s *responsesToolState) AppendArguments(outputIndex int, args string) {
	if state, ok := s.calls[outputIndex]; ok && !state.finished {
		state.args.WriteString(args)
	}
}

func (s *responsesToolState) FinishCall(outputIndex int, callID, namespace, name, finalArgs string) {
	state, ok := s.calls[outputIndex]
	if !ok {
		// Tool call not found by output_index - create it now (handles edge cases)
		s.calls[outputIndex] = &responsesToolCallState{outputIndex: outputIndex, callID: callID, name: name, namespace: namespace}
		s.order = append(s.order, outputIndex)
		state = s.calls[outputIndex]
	}
	if finalArgs != "" {
		// Use final args if provided (overwrite streamed)
		state.args.Reset()
		state.args.WriteString(finalArgs)
	}
	// Update callID if provided (in case it wasn't set initially)
	if callID != "" {
		state.callID = callID
	}
	// Update name and namespace if provided and current values are empty.
	if name != "" && state.name == "" {
		state.name = name
	}
	if namespace != "" && state.namespace == "" {
		state.namespace = namespace
	}
	state.finished = true
}

func (s *responsesToolState) SetCaller(outputIndex int, caller string) {
	if state, ok := s.calls[outputIndex]; ok {
		state.caller = caller
	}
}

func (s *responsesToolState) Validate() error {
	for _, outputIndex := range s.order {
		state := s.calls[outputIndex]
		if state == nil {
			continue
		}
		if !state.finished {
			return fmt.Errorf("Responses API stream ended before tool call %d completed", outputIndex)
		}
		if strings.TrimSpace(state.callID) == "" {
			return fmt.Errorf("Responses API stream missing call_id for tool call %d", outputIndex)
		}
		args := strings.TrimSpace(state.args.String())
		if args == "" {
			return fmt.Errorf("Responses API stream missing arguments for tool call %d", outputIndex)
		}
		if !json.Valid([]byte(args)) {
			return fmt.Errorf("Responses API stream invalid arguments for tool call %d", outputIndex)
		}
	}
	return nil
}

func (s *responsesToolState) Calls() []ToolCall {
	if len(s.order) == 0 {
		return nil
	}
	calls := make([]ToolCall, 0, len(s.order))
	for _, outputIndex := range s.order {
		state := s.calls[outputIndex]
		if state == nil {
			continue
		}
		args := state.args.String()
		if args == "" {
			args = "{}"
		}
		// Use callID for the tool call ID (this is what needs to be sent back in tool results)
		// callID should always be set from the done event if not from added
		id := state.callID
		if id == "" {
			// Defensive fallback for malformed/incomplete streams. It remains the
			// correlation ID echoed in the subsequent tool result.
			id = newSyntheticToolCallID()
			state.callID = id
		}
		call := ToolCall{
			ID:        id,
			Name:      state.name,
			Arguments: json.RawMessage(args),
			Caller:    state.caller,
		}
		if strings.TrimSpace(state.namespace) != "" {
			call.Namespace = state.namespace
			call.ChildName = state.name
			call.Name = ""
		}
		calls = append(calls, call)
	}
	return calls
}

type responsesReasoningState struct {
	items map[int]*responsesReasoningItemState
	// The ChatGPT backend may repeat the complete summary accumulated so far in
	// each new reasoning output item. Track displayed summary content across the
	// whole response so later cumulative snapshots emit only their new suffix.
	emittedSummaryContent string
}

type responsesReasoningItemState struct {
	part                    Part
	summaryParts            []string
	currentSummaryPart      int
	emittedEncrypted        string
	pendingSummarySeparator bool
}

func newResponsesReasoningState() *responsesReasoningState {
	return &responsesReasoningState{
		items: make(map[int]*responsesReasoningItemState),
	}
}

func (s *responsesReasoningState) Ensure(outputIndex int) {
	if _, ok := s.items[outputIndex]; ok {
		return
	}
	s.items[outputIndex] = &responsesReasoningItemState{
		part: Part{Type: PartText, ReasoningKind: ReasoningKindUnknown},
	}
}

func (s *responsesReasoningState) Start(outputIndex int, itemID, encrypted string, summary []responsesReasoningSummaryPart) {
	s.Ensure(outputIndex)
	state := s.items[outputIndex]
	if itemID != "" {
		state.part.ReasoningItemID = itemID
	}
	if encrypted != "" {
		state.part.ReasoningEncryptedContent = encrypted
		if strings.TrimSpace(state.part.ReasoningContent) == "" && len(state.part.ReasoningSummaryParts) == 0 {
			state.part.ReasoningKind = ReasoningKindEncrypted
		}
	}
	if summaryTexts := extractReasoningSummaryTexts(summary); len(summaryTexts) > 0 {
		// Start is called for both output_item.added and output_item.done. If
		// summary_text deltas already populated the state, keep the streamed
		// aggregate instead of replacing it with the final item snapshot.
		if strings.TrimSpace(state.part.ReasoningContent) == "" && len(state.summaryParts) == 0 {
			state.summaryParts = summaryTexts
			state.rebuildReasoningSummaryContent()
		}
		state.part.ReasoningKind = ReasoningKindSummary
	}
}

func (s *responsesReasoningState) SummaryPartAdded(outputIndex int, summaryIndex ...int) {
	s.Ensure(outputIndex)
	state := s.items[outputIndex]
	if strings.TrimSpace(state.part.ReasoningContent) != "" && !strings.HasSuffix(state.part.ReasoningContent, "\n\n") {
		state.pendingSummarySeparator = true
	}
	if len(summaryIndex) > 0 && summaryIndex[0] >= 0 {
		state.currentSummaryPart = summaryIndex[0]
		return
	}
	if len(state.summaryParts) == 0 && strings.TrimSpace(state.part.ReasoningContent) == "" {
		state.currentSummaryPart = 0
		return
	}
	state.currentSummaryPart = len(state.summaryParts)
}

// AppendSummary appends a streaming summary_text delta to the accumulated state
// and returns an event-shaped Part for that delta only. The returned Part's
// ReasoningContent is intentionally just the delta text (with a separator prefix
// when a new summary part starts), and ReasoningSummaryParts is nil; callers can
// use Part() when they need a snapshot of the accumulated content/parts.
func (s *responsesReasoningState) AppendSummary(outputIndex int, delta string) *Part {
	return s.AppendSummaryAt(outputIndex, -1, delta)
}

func (s *responsesReasoningState) AppendSummaryAt(outputIndex int, summaryIndex int, delta string) *Part {
	if delta == "" {
		return nil
	}
	s.Ensure(outputIndex)
	state := s.items[outputIndex]
	if summaryIndex >= 0 {
		// Some Responses-compatible backends omit summary_part.added. Treat an
		// explicit summary index transition as the same section boundary so two
		// complete bold headings cannot be concatenated as `****`.
		if summaryIndex != state.currentSummaryPart && strings.TrimSpace(state.part.ReasoningContent) != "" && !strings.HasSuffix(state.part.ReasoningContent, "\n\n") {
			state.pendingSummarySeparator = true
		}
		state.currentSummaryPart = summaryIndex
	}
	state.ensureSummaryPart(state.currentSummaryPart)
	state.summaryParts[state.currentSummaryPart] += delta
	state.part.ReasoningSummaryParts = append([]string(nil), state.summaryParts...)
	if state.pendingSummarySeparator {
		delta = "\n\n" + strings.TrimLeft(delta, "\r\n")
		state.pendingSummarySeparator = false
	}
	state.part.ReasoningContent += delta
	state.part.ReasoningKind = ReasoningKindSummary
	part := state.part
	part.ReasoningContent = delta
	part.ReasoningSummaryParts = nil
	return &part
}

func (s *responsesReasoningState) SummaryDone(outputIndex int, summaryIndex int, text string) *Part {
	s.Ensure(outputIndex)
	state := s.items[outputIndex]
	if summaryIndex < 0 {
		summaryIndex = state.currentSummaryPart
	}
	state.currentSummaryPart = summaryIndex
	state.ensureSummaryPart(summaryIndex)
	state.summaryParts[summaryIndex] = cleanReasoningSummaryPartText(text)
	state.rebuildReasoningSummaryContent()
	state.part.ReasoningKind = ReasoningKindSummary

	missing := missingReasoningSummarySuffix(s.emittedSummaryContent, state.part.ReasoningContent)
	if missing == "" {
		return nil
	}
	part := state.part
	part.ReasoningContent = missing
	part.ReasoningSummaryParts = nil
	return &part
}

func (s *responsesReasoningState) MarkEmitted(outputIndex int) {
	state, ok := s.items[outputIndex]
	if !ok {
		return
	}
	if state.part.ReasoningKind == ReasoningKindSummary {
		full := state.cleanReasoningSummaryContent()
		if s.emittedSummaryContent == "" || strings.HasPrefix(full, s.emittedSummaryContent) {
			s.emittedSummaryContent = full
		}
	}
	state.emittedEncrypted = state.part.ReasoningEncryptedContent
}

func (s *responsesReasoningState) NeedsFinalEvent(outputIndex int) bool {
	state, ok := s.items[outputIndex]
	if !ok {
		return false
	}
	if state.part.ReasoningContent != "" && s.emittedSummaryContent == "" {
		return true
	}
	if state.part.ReasoningContent != "" && missingReasoningSummarySuffix(s.emittedSummaryContent, state.part.ReasoningContent) != "" {
		return true
	}
	if len(state.part.ReasoningSummaryParts) > 1 {
		return true
	}
	return state.part.ReasoningEncryptedContent != "" && state.part.ReasoningEncryptedContent != state.emittedEncrypted
}

func (s *responsesReasoningState) FinalEventText(outputIndex int) string {
	state, ok := s.items[outputIndex]
	if !ok {
		return ""
	}
	return missingReasoningSummarySuffix(s.emittedSummaryContent, state.part.ReasoningContent)
}

func (s *responsesReasoningState) Finish(outputIndex int, itemID, encrypted string, summary []responsesReasoningSummaryPart) {
	s.Start(outputIndex, itemID, encrypted, summary)
	if state, ok := s.items[outputIndex]; ok {
		state.sanitizeSummaryParts()
	}
}

func (s *responsesReasoningState) MatchesItem(outputIndex int, itemID string) bool {
	if itemID == "" {
		return true
	}
	state, ok := s.items[outputIndex]
	return !ok || state.part.ReasoningItemID == "" || state.part.ReasoningItemID == itemID
}

// Part returns a snapshot of the accumulated reasoning item. The returned Part
// will not be updated by later Start/AppendSummary calls; callers that need the
// latest state should ask again after mutating the state machine.
func (s *responsesReasoningState) Part(outputIndex int) *Part {
	state, ok := s.items[outputIndex]
	if !ok {
		return nil
	}
	if state.part.ReasoningItemID == "" && state.part.ReasoningEncryptedContent == "" && state.part.ReasoningContent == "" {
		return nil
	}
	part := state.part
	return &part
}

func (state *responsesReasoningItemState) ensureSummaryPart(summaryIndex int) {
	for len(state.summaryParts) <= summaryIndex {
		state.summaryParts = append(state.summaryParts, "")
	}
}

func (state *responsesReasoningItemState) sanitizeSummaryParts() {
	if len(state.summaryParts) == 0 {
		return
	}
	for i := range state.summaryParts {
		state.summaryParts[i] = cleanReasoningSummaryPartText(state.summaryParts[i])
	}
	state.rebuildReasoningSummaryContent()
}

func (state *responsesReasoningItemState) rebuildReasoningSummaryContent() {
	parts := displayReasoningSummaryParts(state.summaryParts)
	state.part.ReasoningSummaryParts = parts
	state.part.ReasoningContent = strings.Join(parts, "\n\n")
	if state.part.ReasoningContent != "" {
		state.part.ReasoningKind = ReasoningKindSummary
	}
}

func (state *responsesReasoningItemState) cleanReasoningSummaryContent() string {
	if len(state.summaryParts) == 0 {
		return cleanReasoningSummaryPartText(state.part.ReasoningContent)
	}
	return strings.Join(displayReasoningSummaryParts(state.summaryParts), "\n\n")
}

func displayReasoningSummaryParts(parts []string) []string {
	if len(parts) == 0 {
		return nil
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if cleaned := cleanReasoningSummaryPartText(part); cleaned != "" {
			out = append(out, cleaned)
		}
	}
	return out
}

func missingReasoningSummarySuffix(emitted, full string) string {
	if full == "" {
		return ""
	}
	if emitted == "" {
		return full
	}
	if strings.HasPrefix(full, emitted) {
		return full[len(emitted):]
	}
	return ""
}

func stripReasoningSummaryHTMLComments(text string) string {
	var out strings.Builder
	for {
		start := strings.Index(text, "<!--")
		if start < 0 {
			out.WriteString(text)
			return out.String()
		}
		out.WriteString(text[:start])
		text = text[start+len("<!--"):]
		end := strings.Index(text, "-->")
		if end < 0 {
			return out.String()
		}
		text = text[end+len("-->"):]
	}
}

func cleanReasoningSummaryPartText(text string) string {
	return strings.TrimSpace(stripReasoningSummaryHTMLComments(text))
}

func extractReasoningSummaryText(summary []responsesReasoningSummaryPart) string {
	return strings.Join(extractReasoningSummaryTexts(summary), "\n\n")
}

func extractReasoningSummaryTexts(summary []responsesReasoningSummaryPart) []string {
	if len(summary) == 0 {
		return nil
	}
	var texts []string
	for _, part := range summary {
		if part.Type != "summary_text" {
			continue
		}
		if text := cleanReasoningSummaryPartText(part.Text); text != "" {
			texts = append(texts, text)
		}
	}
	return texts
}

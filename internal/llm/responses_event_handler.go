package llm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

type responsesStreamEventHandler struct {
	client                         *ResponsesClient
	responseStateGeneration        uint64
	debugRaw                       bool
	debugPrefix                    string
	toolState                      *responsesToolState
	reasoningState                 *responsesReasoningState
	lastUsage                      *Usage
	outputItems                    []ResponsesInputItem
	replayItems                    []ProviderReplayItem
	visibleMessageByOutputIndex    map[int]bool
	visibleMessageByItemID         map[string]bool
	textByOutputIndex              map[int]*strings.Builder
	textByItemID                   map[string]*strings.Builder
	textItemIDByOutputIndex        map[int]string
	doneMessageItemIDs             map[string]struct{}
	webSearchStarted               map[string]struct{}
	allowResponseState             bool
	stateSessionID                 string
	suppressReasoningSummaryDeltas bool
	emitted                        bool
}

func newResponsesStreamEventHandler(client *ResponsesClient, responseStateGeneration uint64, debugRaw bool, debugPrefix string, allowResponseState bool, stateSessionID string, suppressReasoningSummaryDeltas bool) *responsesStreamEventHandler {
	return &responsesStreamEventHandler{
		client:                         client,
		responseStateGeneration:        responseStateGeneration,
		debugRaw:                       debugRaw,
		debugPrefix:                    debugPrefix,
		allowResponseState:             allowResponseState,
		stateSessionID:                 stateSessionID,
		suppressReasoningSummaryDeltas: suppressReasoningSummaryDeltas,
		toolState:                      newResponsesToolState(),
		reasoningState:                 newResponsesReasoningState(),
		visibleMessageByOutputIndex:    make(map[int]bool),
		visibleMessageByItemID:         make(map[string]bool),
		textByOutputIndex:              make(map[int]*strings.Builder),
		textByItemID:                   make(map[string]*strings.Builder),
		textItemIDByOutputIndex:        make(map[int]string),
		doneMessageItemIDs:             make(map[string]struct{}),
		webSearchStarted:               make(map[string]struct{}),
	}
}

func (h *responsesStreamEventHandler) Emitted() bool { return h.emitted }

func (h *responsesStreamEventHandler) OutputItems() []ResponsesInputItem {
	return append([]ResponsesInputItem(nil), h.outputItems...)
}

func (h *responsesStreamEventHandler) FunctionCallIDs() []string {
	ids := make([]string, 0)
	for _, item := range h.outputItems {
		if item.Type == "function_call" && strings.TrimSpace(item.CallID) != "" {
			ids = append(ids, item.CallID)
		}
	}
	return ids
}

// responsesJSONEventType reads the top-level Responses event type without
// unmarshalling the full event envelope on the common WebSocket path where a
// known "type" is the first object field. Frames still get decoded by
// HandleJSONEvent below; this avoids an extra json.Unmarshal on hot events while
// falling back to json.Unmarshal for uncommon field orderings, unknown event
// types, and malformed JSON.
func responsesJSONEventType(data []byte) (string, error) {
	if typ, ok := fastResponsesJSONEventType(data); ok && knownResponsesEventType(typ) {
		return typ, nil
	}

	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", err
	}
	return envelope.Type, nil
}

func knownResponsesEventType(typ string) bool {
	switch typ {
	case "response.output_text.delta",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.output_item.done",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.completed",
		"response.incomplete",
		"response.failed",
		"error":
		return true
	default:
		return false
	}
}

func fastResponsesJSONEventType(data []byte) (string, bool) {
	i := skipJSONWhitespace(data, 0)
	if i >= len(data) || data[i] != '{' {
		return "", false
	}
	i = skipJSONWhitespace(data, i+1)
	if i >= len(data) || data[i] == '}' {
		return "", false
	}

	keyEnd, err := skipJSONString(data, i)
	if err != nil {
		return "", false
	}
	if !bytes.Equal(data[i:keyEnd], []byte(`"type"`)) {
		return "", false
	}
	i = skipJSONWhitespace(data, keyEnd)
	if i >= len(data) || data[i] != ':' {
		return "", false
	}
	i = skipJSONWhitespace(data, i+1)

	valueEnd, err := skipJSONString(data, i)
	if err != nil {
		return "", false
	}
	raw := data[i:valueEnd]
	if len(raw) >= 2 && bytes.IndexByte(raw[1:len(raw)-1], '\\') < 0 {
		return string(raw[1 : len(raw)-1]), true
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func skipJSONWhitespace(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\n', '\r', '\t':
			i++
		default:
			return i
		}
	}
	return i
}

func skipJSONString(data []byte, i int) (int, error) {
	if i >= len(data) || data[i] != '"' {
		return 0, fmt.Errorf("expected JSON string")
	}
	for i++; i < len(data); i++ {
		switch data[i] {
		case '\\':
			i++
			if i >= len(data) {
				return 0, fmt.Errorf("unterminated JSON escape")
			}
		case '"':
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("unterminated JSON string")
}

type responsesEventContext struct {
	data  []byte
	label string
	send  eventSender
}

func (c responsesEventContext) decode(dst any) error {
	if err := json.Unmarshal(c.data, dst); err != nil {
		return fmt.Errorf("decode Responses API %s event: %w", c.label, err)
	}
	return nil
}

func (h *responsesStreamEventHandler) emit(c responsesEventContext, event Event) error {
	h.emitted = true
	return c.send.Send(event)
}

func (h *responsesStreamEventHandler) HandleJSONEvent(data []byte, eventType string, send eventSender) (bool, error) {
	if bytes.Equal(data, sseDoneData) {
		return true, nil
	}
	if eventType == "" {
		if typ, err := responsesJSONEventType(data); err == nil && typ != "" {
			eventType = typ
		}
	}
	eventLabel := eventType
	if eventLabel == "" {
		eventLabel = "unknown"
	}
	if h.debugRaw {
		DebugRawSection(h.debugRaw, h.debugPrefix+" Event (event="+eventLabel+")", string(data))
	}
	ctx := responsesEventContext{data: data, label: eventLabel, send: send}

	switch eventType {
	case "response.output_text.delta":
		return false, h.handleTextDelta(ctx)
	case "response.output_item.added":
		return false, h.handleOutputItemAdded(ctx)
	case "response.function_call_arguments.delta":
		return false, h.handleFunctionArgumentsDelta(ctx)
	case "response.output_item.done":
		return false, h.handleOutputItemDone(ctx)
	case "response.reasoning_summary_part.added":
		return false, h.handleReasoningPartAdded(ctx)
	case "response.reasoning_summary_part.done":
		// text.done and the final reasoning item carry the authoritative section
		// text; part.done is only needed as a section boundary here.
		return false, nil
	case "response.reasoning_summary_text.delta":
		return false, h.handleReasoningSummaryDelta(ctx)
	case "response.reasoning_summary_text.done":
		return false, h.handleReasoningSummaryDone(ctx)
	case "response.completed", "response.incomplete":
		return h.handleResponseTerminal(ctx, eventType)
	case "response.failed", "error":
		return false, h.handleResponseError(ctx)
	default:
		return false, nil
	}
}

func (h *responsesStreamEventHandler) handleTextDelta(ctx responsesEventContext) error {
	var event struct {
		Delta       string `json:"delta"`
		ItemID      string `json:"item_id"`
		OutputIndex int    `json:"output_index"`
	}
	if err := ctx.decode(&event); err != nil {
		return err
	}
	if !h.messageVisible(event.OutputIndex, event.ItemID) {
		return nil
	}
	if event.Delta == "" {
		return nil
	}
	var streamed *strings.Builder
	if event.ItemID != "" {
		streamed = h.textByItemID[event.ItemID]
	}
	knownItem := streamed != nil
	owner := h.textItemIDByOutputIndex[event.OutputIndex]
	useIndexBuilder := event.ItemID == "" || owner == "" || owner == event.ItemID
	claimIndex := useIndexBuilder || !knownItem
	if streamed == nil && useIndexBuilder {
		streamed = h.textByOutputIndex[event.OutputIndex]
	}
	if streamed == nil {
		streamed = &strings.Builder{}
	}
	if claimIndex {
		h.textByOutputIndex[event.OutputIndex] = streamed
		if event.ItemID != "" {
			h.textItemIDByOutputIndex[event.OutputIndex] = event.ItemID
		}
	}
	if event.ItemID != "" {
		h.textByItemID[event.ItemID] = streamed
	}
	streamed.WriteString(event.Delta)
	return h.emit(ctx, Event{Type: EventTextDelta, Text: event.Delta})
}

func (h *responsesStreamEventHandler) messageVisible(outputIndex int, itemID string) bool {
	if itemID != "" {
		if visible, known := h.visibleMessageByItemID[itemID]; known {
			return visible
		}
	}
	visible, known := h.visibleMessageByOutputIndex[outputIndex]
	return !known || visible
}

func (h *responsesStreamEventHandler) handleOutputItemAdded(ctx responsesEventContext) error {
	var envelope struct {
		Item        json.RawMessage `json:"item"`
		OutputIndex int             `json:"output_index"`
	}
	if err := ctx.decode(&envelope); err != nil {
		return err
	}
	var itemType struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(envelope.Item, &itemType); err != nil {
		return fmt.Errorf("decode Responses API added output item type: %w", err)
	}
	var item responsesOutputItem
	if itemType.Type != "tool_search_call" {
		if err := json.Unmarshal(envelope.Item, &item); err != nil {
			return fmt.Errorf("decode Responses API added output item: %w", err)
		}
	}
	return h.recordAddedOutputItem(ctx, envelope.OutputIndex, itemType.Type, item)
}

func (h *responsesStreamEventHandler) recordAddedOutputItem(ctx responsesEventContext, outputIndex int, itemType string, item responsesOutputItem) error {
	switch itemType {
	case "function_call":
		h.toolState.StartCall(outputIndex, item.CallID, item.Namespace, item.Name)
	case "web_search_call":
		callID := responsesWebSearchCallID(item.ID, outputIndex)
		if _, started := h.webSearchStarted[callID]; started {
			return nil
		}
		if err := h.emit(ctx, Event{Type: EventToolExecStart, ToolCallID: callID, ToolName: WebSearchToolName}); err != nil {
			return err
		}
		h.webSearchStarted[callID] = struct{}{}
	case "reasoning":
		h.reasoningState.Start(outputIndex, item.ID, item.EncryptedContent, item.Summary)
	case "message":
		agent := strings.TrimSpace(item.Agent)
		visible := agent == "" || agent == "/root"
		h.visibleMessageByOutputIndex[outputIndex] = visible
		if item.ID != "" {
			h.visibleMessageByItemID[item.ID] = visible
		}
	}
	return nil
}

func (h *responsesStreamEventHandler) handleFunctionArgumentsDelta(ctx responsesEventContext) error {
	var event struct {
		OutputIndex int    `json:"output_index"`
		Delta       string `json:"delta"`
	}
	if err := ctx.decode(&event); err != nil {
		return err
	}
	h.toolState.AppendArguments(event.OutputIndex, event.Delta)
	return nil
}

func (h *responsesStreamEventHandler) handleOutputItemDone(ctx responsesEventContext) error {
	var envelope struct {
		Item        json.RawMessage `json:"item"`
		OutputIndex int             `json:"output_index"`
	}
	if err := ctx.decode(&envelope); err != nil {
		return err
	}
	var itemType struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(envelope.Item, &itemType); err != nil {
		return fmt.Errorf("decode Responses API output item type: %w", err)
	}
	if itemType.Type == "tool_search_call" {
		return h.handleToolDiscoveryDone(ctx, envelope.Item)
	}
	var item responsesOutputItem
	if err := json.Unmarshal(envelope.Item, &item); err != nil {
		return fmt.Errorf("decode Responses API output item: %w", err)
	}
	if item.Type == "message" && item.ID != "" {
		if _, done := h.doneMessageItemIDs[item.ID]; done {
			return nil
		}
	}
	if len(envelope.Item) > 0 {
		h.replayItems = append(h.replayItems, ProviderReplayItem{Raw: append(json.RawMessage(nil), envelope.Item...)})
	}
	return h.recordDoneOutputItem(ctx, envelope.OutputIndex, item)
}

func (h *responsesStreamEventHandler) handleToolDiscoveryDone(ctx responsesEventContext, raw json.RawMessage) error {
	var discovery struct {
		Execution string          `json:"execution"`
		CallID    string          `json:"call_id"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &discovery); err != nil {
		return fmt.Errorf("decode tool_search_call: %w", err)
	}
	if discovery.Execution != "client" || strings.TrimSpace(discovery.CallID) == "" {
		return fmt.Errorf("invalid client tool_search_call execution=%q call_id=%q", discovery.Execution, discovery.CallID)
	}
	arguments := discovery.Arguments
	if len(arguments) > 0 && arguments[0] == '"' {
		var encoded string
		if err := json.Unmarshal(arguments, &encoded); err != nil {
			return fmt.Errorf("decode tool_search_call arguments string: %w", err)
		}
		arguments = json.RawMessage(encoded)
	}
	if !json.Valid(arguments) {
		return fmt.Errorf("tool_search_call returned invalid arguments")
	}
	call := &ToolDiscoveryCall{ID: discovery.CallID, Arguments: append(json.RawMessage(nil), arguments...)}
	return h.emit(ctx, Event{Type: EventDiscoveryCall, DiscoveryCall: call})
}

func (h *responsesStreamEventHandler) recordDoneOutputItem(ctx responsesEventContext, outputIndex int, item responsesOutputItem) error {
	switch item.Type {
	case "function_call":
		h.outputItems = append(h.outputItems, responsesOutputItemToInputItem(item)...)
		h.toolState.FinishCall(outputIndex, item.CallID, item.Namespace, item.Name, item.Arguments)
		h.toolState.SetCaller(outputIndex, item.Caller)
		return nil
	case "web_search_call":
		return h.handleWebSearchDone(ctx, outputIndex, item)
	case "reasoning":
		return h.handleReasoningDone(ctx, outputIndex, item)
	case "message":
		return h.handleMessageDone(ctx, outputIndex, item)
	case "image_generation_call":
		return h.handleImageDone(ctx, item)
	default:
		return nil
	}
}

func (h *responsesStreamEventHandler) handleWebSearchDone(ctx responsesEventContext, outputIndex int, item responsesOutputItem) error {
	action, err := decodeResponsesWebSearchAction(item.Action)
	if err != nil {
		return fmt.Errorf("decode web_search_call action: %w", err)
	}
	callID := responsesWebSearchCallID(item.ID, outputIndex)
	toolInfo := responsesWebSearchToolInfo(action)
	toolArgs := responsesWebSearchToolArguments(action)
	succeeded := strings.EqualFold(item.Status, "completed")
	status := ToolActivityFailed
	if succeeded {
		status = ToolActivityCompleted
	}
	if _, started := h.webSearchStarted[callID]; !started {
		if err := h.emit(ctx, Event{Type: EventToolExecStart, ToolCallID: callID, ToolName: WebSearchToolName, ToolInfo: toolInfo}); err != nil {
			return err
		}
		h.webSearchStarted[callID] = struct{}{}
	}
	if err := h.emit(ctx, Event{Type: EventToolExecEnd, ToolCallID: callID, ToolName: WebSearchToolName, ToolInfo: toolInfo, ToolArgs: toolArgs, ToolSuccess: succeeded}); err != nil {
		return err
	}
	activity := &ToolActivity{ID: callID, Name: WebSearchToolName, Info: toolInfo, Arguments: toolArgs, Status: status}
	return h.emit(ctx, Event{Type: EventToolActivity, ToolActivity: activity})
}

func (h *responsesStreamEventHandler) handleReasoningDone(ctx responsesEventContext, outputIndex int, item responsesOutputItem) error {
	h.outputItems = append(h.outputItems, responsesOutputItemToInputItem(item)...)
	h.reasoningState.Finish(outputIndex, item.ID, item.EncryptedContent, item.Summary)
	part := h.reasoningState.Part(outputIndex)
	if part == nil || !h.reasoningState.NeedsFinalEvent(outputIndex) {
		return nil
	}
	event := Event{
		Type:                      EventReasoningDelta,
		Text:                      h.reasoningState.FinalEventText(outputIndex),
		ReasoningKind:             part.ReasoningKind,
		ReasoningSummaryParts:     append([]string(nil), part.ReasoningSummaryParts...),
		ReasoningIndex:            outputIndex,
		ReasoningFinal:            true,
		ReasoningItemID:           part.ReasoningItemID,
		ReasoningEncryptedContent: part.ReasoningEncryptedContent,
	}
	if err := h.emit(ctx, event); err != nil {
		return err
	}
	h.reasoningState.MarkEmitted(outputIndex)
	return nil
}

func (h *responsesStreamEventHandler) markMessageDone(itemID string) {
	if itemID != "" {
		h.doneMessageItemIDs[itemID] = struct{}{}
	}
}

func (h *responsesStreamEventHandler) handleMessageDone(ctx responsesEventContext, outputIndex int, item responsesOutputItem) error {
	h.outputItems = append(h.outputItems, responsesOutputItemToInputItem(item)...)
	if !h.messageVisible(outputIndex, item.ID) {
		h.markMessageDone(item.ID)
		return nil
	}
	agent := strings.TrimSpace(item.Agent)
	if agent != "" && agent != "/root" {
		h.markMessageDone(item.ID)
		return nil
	}
	streamedText := ""
	var streamed *strings.Builder
	if item.ID != "" {
		streamed = h.textByItemID[item.ID]
	}
	if streamed == nil {
		owner := h.textItemIDByOutputIndex[outputIndex]
		if item.ID == "" || owner == "" || owner == item.ID {
			streamed = h.textByOutputIndex[outputIndex]
		}
	}
	if streamed != nil {
		streamedText = streamed.String()
	}
	for _, content := range item.Content {
		var text string
		if content.Type == "output_text" && content.Text != "" {
			text, streamedText = responsesDoneTextSuffix(streamedText, content.Text)
		} else if content.Type == "refusal" && content.Refusal != "" {
			text = content.Refusal
		}
		if text != "" {
			if err := h.emit(ctx, Event{Type: EventTextDelta, Text: text}); err != nil {
				return err
			}
		}
	}
	h.markMessageDone(item.ID)
	return nil
}

func responsesDoneTextSuffix(streamed, completed string) (suffix, remainingStreamed string) {
	if streamed == "" {
		return completed, ""
	}
	if strings.HasPrefix(completed, streamed) {
		return strings.TrimPrefix(completed, streamed), ""
	}
	if strings.HasPrefix(streamed, completed) {
		return "", strings.TrimPrefix(streamed, completed)
	}
	// A non-prefix mismatch cannot be reconciled without duplicating text the
	// consumer has already displayed. Preserve the streamed representation for
	// this part, but consume it so later unstreamed content parts remain visible.
	return "", ""
}

func (h *responsesStreamEventHandler) handleImageDone(ctx responsesEventContext, item responsesOutputItem) error {
	if item.Result == "" {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(item.Result)
	if err != nil {
		return fmt.Errorf("decode image_generation_call result: %w", err)
	}
	return h.emit(ctx, Event{Type: EventImageGenerated, ImageData: decoded, ImageMimeType: "image/png", RevisedPrompt: item.RevisedPrompt})
}

func (h *responsesStreamEventHandler) handleReasoningPartAdded(ctx responsesEventContext) error {
	var event struct {
		OutputIndex  int  `json:"output_index"`
		SummaryIndex *int `json:"summary_index"`
	}
	if err := ctx.decode(&event); err != nil {
		return err
	}
	if event.SummaryIndex != nil {
		h.reasoningState.SummaryPartAdded(event.OutputIndex, *event.SummaryIndex)
	} else {
		h.reasoningState.SummaryPartAdded(event.OutputIndex)
	}
	return nil
}

func (h *responsesStreamEventHandler) handleReasoningSummaryDelta(ctx responsesEventContext) error {
	var event struct {
		OutputIndex  int    `json:"output_index"`
		SummaryIndex *int   `json:"summary_index"`
		Delta        string `json:"delta"`
	}
	if err := ctx.decode(&event); err != nil {
		return err
	}
	summaryIndex := -1
	if event.SummaryIndex != nil {
		summaryIndex = *event.SummaryIndex
	}
	part := h.reasoningState.AppendSummaryAt(event.OutputIndex, summaryIndex, event.Delta)
	if part == nil || h.suppressReasoningSummaryDeltas {
		return nil
	}
	reasoningEvent := Event{Type: EventReasoningDelta, Text: part.ReasoningContent, ReasoningKind: ReasoningKindSummary, ReasoningIndex: event.OutputIndex, ReasoningItemID: part.ReasoningItemID, ReasoningEncryptedContent: part.ReasoningEncryptedContent}
	if err := h.emit(ctx, reasoningEvent); err != nil {
		return err
	}
	h.reasoningState.MarkEmitted(event.OutputIndex)
	return nil
}

func (h *responsesStreamEventHandler) handleReasoningSummaryDone(ctx responsesEventContext) error {
	var event struct {
		OutputIndex  int    `json:"output_index"`
		ItemID       string `json:"item_id"`
		SummaryIndex *int   `json:"summary_index"`
		Text         string `json:"text"`
	}
	if err := ctx.decode(&event); err != nil {
		return err
	}
	if !h.reasoningState.MatchesItem(event.OutputIndex, event.ItemID) {
		return nil
	}
	summaryIndex := -1
	if event.SummaryIndex != nil {
		summaryIndex = *event.SummaryIndex
	}
	part := h.reasoningState.SummaryDone(event.OutputIndex, summaryIndex, event.Text)
	if part == nil {
		return nil
	}
	reasoningEvent := Event{Type: EventReasoningDelta, Text: part.ReasoningContent, ReasoningKind: ReasoningKindSummary, ReasoningIndex: event.OutputIndex, ReasoningItemID: part.ReasoningItemID, ReasoningEncryptedContent: part.ReasoningEncryptedContent}
	if err := h.emit(ctx, reasoningEvent); err != nil {
		return err
	}
	h.reasoningState.MarkEmitted(event.OutputIndex)
	return nil
}

func (h *responsesStreamEventHandler) handleResponseTerminal(ctx responsesEventContext, eventType string) (bool, error) {
	var event struct {
		Response struct {
			ID                string          `json:"id"`
			Usage             *responsesUsage `json:"usage,omitempty"`
			IncompleteDetails struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details,omitempty"`
		} `json:"response"`
	}
	if err := ctx.decode(&event); err != nil {
		return false, err
	}
	if eventType == "response.completed" && h.allowResponseState && event.Response.ID != "" {
		h.client.setLastResponseIDIfGeneration(h.responseStateGeneration, event.Response.ID, h.stateSessionID)
	}
	h.recordUsage(event.Response.Usage)
	if eventType != "response.incomplete" {
		return true, nil
	}
	if err := h.FinishIncomplete(ctx.send); err != nil {
		return false, err
	}
	return false, &ResponsesIncompleteError{Reason: strings.TrimSpace(event.Response.IncompleteDetails.Reason)}
}

func (h *responsesStreamEventHandler) recordUsage(usage *responsesUsage) {
	if usage == nil {
		return
	}
	cached := usage.InputTokensDetails.CachedTokens
	cacheWrite := usage.InputTokensDetails.CacheWriteTokens
	uncached := usage.InputTokens - cached - cacheWrite
	if uncached < 0 {
		uncached = 0
	}
	h.lastUsage = &Usage{
		InputTokens:            uncached,
		OutputTokens:           usage.OutputTokens,
		CachedInputTokens:      cached,
		CacheWriteTokens:       cacheWrite,
		ProviderRawInputTokens: usage.InputTokens,
		ProviderTotalTokens:    usage.TotalTokens,
		ReasoningTokens:        usage.OutputTokensDetails.ReasoningTokens,
	}
}

func (h *responsesStreamEventHandler) handleResponseError(ctx responsesEventContext) error {
	var event struct {
		Status   int             `json:"status,omitempty"`
		Error    *responsesError `json:"error"`
		Response struct {
			Error *responsesError `json:"error"`
		} `json:"response"`
	}
	if err := ctx.decode(&event); err != nil {
		return err
	}
	apiErr := event.Error
	if apiErr == nil {
		apiErr = event.Response.Error
	}
	if apiErr == nil {
		return fmt.Errorf("Responses API error: unknown error")
	}
	return &responsesAPIEventError{Status: event.Status, APIError: apiErr}
}

func responsesWebSearchCallID(itemID string, outputIndex int) string {
	if itemID != "" {
		return itemID
	}
	return fmt.Sprintf("web_search:%d", outputIndex)
}

func decodeResponsesWebSearchAction(raw json.RawMessage) (responsesWebSearchAction, error) {
	var action responsesWebSearchAction
	if len(raw) == 0 {
		return action, nil
	}
	if err := json.Unmarshal(raw, &action); err != nil {
		return action, err
	}
	return action, nil
}

func responsesWebSearchToolArguments(action responsesWebSearchAction) json.RawMessage {
	args := make(map[string]string)
	switch action.Type {
	case "search":
		if action.Query != "" {
			args["query"] = action.Query
		}
	case "open_page":
		if action.URL != "" {
			args["url"] = action.URL
		}
	case "find":
		if action.URL != "" {
			args["url"] = action.URL
		}
		if action.Pattern != "" {
			args["pattern"] = action.Pattern
		}
	default:
		if action.Query != "" {
			args["query"] = action.Query
		}
		if action.URL != "" {
			args["url"] = action.URL
		}
		if action.Pattern != "" {
			args["pattern"] = action.Pattern
		}
	}
	if len(args) == 0 {
		return nil
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil
	}
	return raw
}

func responsesWebSearchToolInfo(action responsesWebSearchAction) string {
	var value string
	switch action.Type {
	case "search":
		value = action.Query
	case "open_page":
		value = action.URL
	case "find":
		value = action.Pattern
		if value == "" {
			value = action.URL
		}
	}
	if value == "" {
		switch {
		case action.Query != "":
			value = action.Query
		case action.URL != "":
			value = action.URL
		case action.Pattern != "":
			value = action.Pattern
		}
	}
	if value == "" {
		return ""
	}
	return "(" + value + ")"
}

func responsesOutputItemToInputItem(item responsesOutputItem) []ResponsesInputItem {
	switch item.Type {
	case "function_call":
		callID := strings.TrimSpace(item.CallID)
		if callID == "" {
			return nil
		}
		args := strings.TrimSpace(item.Arguments)
		if args == "" {
			args = "{}"
		}
		return []ResponsesInputItem{{Type: "function_call", CallID: callID, Name: item.Name, Namespace: item.Namespace, Arguments: args, Caller: item.Caller}}
	case "reasoning":
		summary := responsesReasoningSummary(item.Summary)
		return []ResponsesInputItem{{Type: "reasoning", ID: item.ID, EncryptedContent: item.EncryptedContent, Summary: &summary}}
	case "message":
		var text strings.Builder
		for _, content := range item.Content {
			if content.Type == "output_text" && content.Text != "" {
				text.WriteString(content.Text)
			} else if content.Type == "refusal" && content.Refusal != "" {
				text.WriteString(content.Refusal)
			}
		}
		if text.Len() == 0 {
			return nil
		}
		return []ResponsesInputItem{{Type: "message", Role: "assistant", Content: text.String()}}
	default:
		return nil
	}
}

func (h *responsesStreamEventHandler) emitFinalItems(send eventSender) error {
	if err := h.toolState.Validate(); err != nil {
		return err
	}
	for _, call := range h.toolState.Calls() {
		if err := send.Send(Event{Type: EventToolCall, Tool: &call}); err != nil {
			return err
		}
	}
	for i := range h.replayItems {
		replay := h.replayItems[i]
		if err := send.Send(Event{Type: EventProviderReplay, ProviderReplay: &replay}); err != nil {
			return err
		}
	}
	if h.lastUsage != nil {
		if err := send.Send(Event{Type: EventUsage, Use: h.lastUsage}); err != nil {
			return err
		}
	}
	return nil
}

func (h *responsesStreamEventHandler) FinishIncomplete(send eventSender) error {
	// Do not interpose hidden replay events before the transport's incomplete
	// error. Committed function calls are already emitted first and the engine's
	// recovery journal preserves them.
	replay := h.replayItems
	h.replayItems = nil
	err := h.emitFinalItems(send)
	h.replayItems = replay
	return err
}

func (h *responsesStreamEventHandler) Finish(send eventSender) error {
	if err := h.emitFinalItems(send); err != nil {
		return err
	}
	return send.Send(Event{Type: EventDone})
}

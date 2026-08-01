package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/gateway/protocol"
	"github.com/samsaffron/term-llm/internal/llm"
)

type responsesOutputContent struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type responsesSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesOutputItem struct {
	ID               string
	Type             string
	Status           string
	Role             string
	Content          []responsesOutputContent
	EncryptedContent string
	Summary          []responsesSummaryPart
	CallID           string
	Name             string
	Arguments        string
}

func (item responsesOutputItem) MarshalJSON() ([]byte, error) {
	wire := map[string]any{"id": item.ID, "type": item.Type}
	if item.Status != "" {
		wire["status"] = item.Status
	}
	switch item.Type {
	case "reasoning":
		wire["encrypted_content"] = item.EncryptedContent
		wire["summary"] = item.Summary
	case "message":
		wire["role"] = item.Role
		wire["content"] = item.Content
	case "function_call":
		wire["call_id"] = item.CallID
		wire["name"] = item.Name
		wire["arguments"] = item.Arguments
	}
	return json.Marshal(wire)
}

type responsesUsageDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type responsesOutputUsageDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type responsesUsage struct {
	InputTokens         int                         `json:"input_tokens"`
	InputTokensDetails  responsesUsageDetails       `json:"input_tokens_details"`
	OutputTokens        int                         `json:"output_tokens"`
	OutputTokensDetails responsesOutputUsageDetails `json:"output_tokens_details"`
	TotalTokens         int                         `json:"total_tokens"`
}

type responsesDocument struct {
	ID                string                 `json:"id"`
	Object            string                 `json:"object"`
	CreatedAt         int64                  `json:"created_at"`
	Status            string                 `json:"status"`
	Error             any                    `json:"error"`
	IncompleteDetails any                    `json:"incomplete_details"`
	Instructions      any                    `json:"instructions"`
	Metadata          map[string]string      `json:"metadata"`
	Model             string                 `json:"model"`
	Output            []*responsesOutputItem `json:"output"`
	ParallelToolCalls bool                   `json:"parallel_tool_calls"`
	Temperature       float64                `json:"temperature"`
	ToolChoice        any                    `json:"tool_choice"`
	Tools             []json.RawMessage      `json:"tools"`
	TopP              float64                `json:"top_p"`
	ServiceTier       string                 `json:"service_tier,omitempty"`
	Usage             *responsesUsage        `json:"usage"`
}

type responsesReasoningOutput struct {
	item         *responsesOutputItem
	outputIndex  int
	summary      string
	summaryParts []string
	partAdded    bool
	done         bool
}

type responsesMessageOutput struct {
	item        *responsesOutputItem
	outputIndex int
	text        string
	done        bool
}

type responsesAccumulator struct {
	document  responsesDocument
	emit      func(string, map[string]any) error
	sequence  int
	reasoning *responsesReasoningOutput
	message   *responsesMessageOutput
	usage     llm.Usage
}

func newResponsesAccumulator(responseID string, request responsesRequest, parallel bool, emit func(string, map[string]any) error) *responsesAccumulator {
	var instructions any
	if request.Instructions != nil {
		instructions = *request.Instructions
	}
	metadata := request.Metadata
	if metadata == nil {
		metadata = make(map[string]string)
	}
	temperature := 1.0
	if request.Temperature != nil {
		temperature = *request.Temperature
	}
	topP := 1.0
	if request.TopP != nil {
		topP = *request.TopP
	}
	toolChoice := any("auto")
	if raw := bytes.TrimSpace(request.ToolChoice); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		toolChoice = json.RawMessage(append([]byte(nil), raw...))
	}
	tools := request.Tools
	if tools == nil {
		tools = make([]json.RawMessage, 0)
	}
	serviceTier := ""
	if request.ServiceTier != nil {
		serviceTier = strings.TrimSpace(*request.ServiceTier)
	}
	return &responsesAccumulator{
		document: responsesDocument{
			ID: responseID, Object: "response", CreatedAt: time.Now().Unix(), Status: "in_progress",
			Error: nil, IncompleteDetails: nil, Instructions: instructions, Metadata: metadata,
			Model: request.Model, Output: make([]*responsesOutputItem, 0), ParallelToolCalls: parallel,
			Temperature: temperature, ToolChoice: toolChoice, Tools: tools, TopP: topP, ServiceTier: serviceTier,
		},
		emit: emit,
	}
}

func (a *responsesAccumulator) event(eventType string, payload map[string]any) error {
	if a.emit == nil {
		return nil
	}
	if payload == nil {
		payload = make(map[string]any)
	}
	payload["type"] = eventType
	payload["sequence_number"] = a.sequence
	a.sequence++
	return a.emit(eventType, payload)
}

func (a *responsesAccumulator) created() error {
	response := a.document
	response.Output = []*responsesOutputItem{}
	response.Usage = nil
	if err := a.event("response.created", map[string]any{"response": response}); err != nil {
		return err
	}
	return a.event("response.in_progress", map[string]any{"response": response})
}

func (a *responsesAccumulator) consume(event llm.Event) error {
	switch event.Type {
	case llm.EventTextDelta:
		return a.addText(event.Text)
	case llm.EventReasoningDelta:
		return a.addReasoning(event)
	case llm.EventToolCall:
		if event.Tool == nil {
			return nil
		}
		if event.ToolResponse != nil {
			return &responsesWireError{Status: http.StatusBadRequest, Code: "incompatible_tool_request", Message: "provider requested gateway-side tool execution, which the Responses edge never permits"}
		}
		return a.addFunctionCall(*event.Tool)
	case llm.EventUsage:
		if event.Use != nil {
			a.usage.Add(*event.Use)
		}
	case llm.EventError:
		if event.Err != nil {
			return event.Err
		}
		return fmt.Errorf("provider returned an error event")
	case llm.EventAttemptDiscard:
		if a.message != nil || a.reasoning != nil || len(a.document.Output) > 0 {
			return fmt.Errorf("provider retry attempted after Responses output was committed")
		}
	}
	return nil
}

func (a *responsesAccumulator) addReasoning(event llm.Event) error {
	displaySummary := llm.NormalizeReasoningKind(event.ReasoningKind) == llm.ReasoningKindSummary || len(event.ReasoningSummaryParts) > 0
	if !displaySummary && event.ReasoningEncryptedContent == "" {
		return nil
	}
	if err := a.finishMessage(); err != nil {
		return err
	}
	itemID := strings.TrimSpace(event.ReasoningItemID)
	if a.reasoning != nil && itemID != "" && a.reasoning.item.ID != itemID {
		if err := a.finishReasoning(); err != nil {
			return err
		}
	}
	if a.reasoning == nil {
		if itemID == "" {
			var err error
			itemID, err = randomSecret("rs", 16)
			if err != nil {
				return err
			}
		}
		item := &responsesOutputItem{ID: itemID, Type: "reasoning", Status: "in_progress", EncryptedContent: event.ReasoningEncryptedContent, Summary: []responsesSummaryPart{}}
		index := len(a.document.Output)
		a.document.Output = append(a.document.Output, item)
		a.reasoning = &responsesReasoningOutput{item: item, outputIndex: index}
		if err := a.event("response.output_item.added", map[string]any{"output_index": index, "item": item}); err != nil {
			return err
		}
	}
	state := a.reasoning
	if event.ReasoningEncryptedContent != "" {
		state.item.EncryptedContent = event.ReasoningEncryptedContent
	}
	if len(event.ReasoningSummaryParts) > 0 {
		state.summaryParts = append([]string(nil), event.ReasoningSummaryParts...)
	}
	if displaySummary && event.Text != "" {
		if !state.partAdded {
			state.partAdded = true
			if err := a.event("response.reasoning_summary_part.added", map[string]any{
				"item_id": state.item.ID, "output_index": state.outputIndex, "summary_index": 0,
				"part": responsesSummaryPart{Type: "summary_text", Text: ""},
			}); err != nil {
				return err
			}
		}
		state.summary += event.Text
		if err := a.event("response.reasoning_summary_text.delta", map[string]any{
			"item_id": state.item.ID, "output_index": state.outputIndex, "summary_index": 0, "delta": event.Text,
		}); err != nil {
			return err
		}
	}
	if event.ReasoningFinal {
		return a.finishReasoning()
	}
	return nil
}

func (a *responsesAccumulator) finishReasoning() error {
	state := a.reasoning
	if state == nil || state.done {
		return nil
	}
	state.done = true
	if len(state.summaryParts) > 0 {
		state.item.Summary = make([]responsesSummaryPart, 0, len(state.summaryParts))
		for _, text := range state.summaryParts {
			state.item.Summary = append(state.item.Summary, responsesSummaryPart{Type: "summary_text", Text: text})
		}
	} else if state.summary != "" {
		state.item.Summary = []responsesSummaryPart{{Type: "summary_text", Text: state.summary}}
	}
	if state.partAdded {
		if err := a.event("response.reasoning_summary_text.done", map[string]any{
			"item_id": state.item.ID, "output_index": state.outputIndex, "summary_index": 0, "text": state.summary,
		}); err != nil {
			return err
		}
		if err := a.event("response.reasoning_summary_part.done", map[string]any{
			"item_id": state.item.ID, "output_index": state.outputIndex, "summary_index": 0,
			"part": responsesSummaryPart{Type: "summary_text", Text: state.summary},
		}); err != nil {
			return err
		}
	}
	state.item.Status = "completed"
	if err := a.event("response.output_item.done", map[string]any{"output_index": state.outputIndex, "item": state.item}); err != nil {
		return err
	}
	a.reasoning = nil
	return nil
}

func (a *responsesAccumulator) addText(delta string) error {
	if delta == "" {
		return nil
	}
	if err := a.finishReasoning(); err != nil {
		return err
	}
	if a.message == nil {
		itemID, err := randomSecret("msg", 16)
		if err != nil {
			return err
		}
		item := &responsesOutputItem{ID: itemID, Type: "message", Status: "in_progress", Role: "assistant", Content: []responsesOutputContent{}}
		index := len(a.document.Output)
		a.document.Output = append(a.document.Output, item)
		a.message = &responsesMessageOutput{item: item, outputIndex: index}
		if err := a.event("response.output_item.added", map[string]any{"output_index": index, "item": item}); err != nil {
			return err
		}
		if err := a.event("response.content_part.added", map[string]any{
			"item_id": item.ID, "output_index": index, "content_index": 0,
			"part": responsesOutputContent{Type: "output_text", Text: "", Annotations: []any{}},
		}); err != nil {
			return err
		}
	}
	a.message.text += delta
	return a.event("response.output_text.delta", map[string]any{
		"item_id": a.message.item.ID, "output_index": a.message.outputIndex, "content_index": 0,
		"delta": delta, "logprobs": []any{},
	})
}

func (a *responsesAccumulator) finishMessage() error {
	state := a.message
	if state == nil || state.done {
		return nil
	}
	state.done = true
	content := responsesOutputContent{Type: "output_text", Text: state.text, Annotations: []any{}}
	state.item.Content = []responsesOutputContent{content}
	if err := a.event("response.output_text.done", map[string]any{
		"item_id": state.item.ID, "output_index": state.outputIndex, "content_index": 0,
		"text": state.text, "logprobs": []any{},
	}); err != nil {
		return err
	}
	if err := a.event("response.content_part.done", map[string]any{
		"item_id": state.item.ID, "output_index": state.outputIndex, "content_index": 0, "part": content,
	}); err != nil {
		return err
	}
	state.item.Status = "completed"
	if err := a.event("response.output_item.done", map[string]any{"output_index": state.outputIndex, "item": state.item}); err != nil {
		return err
	}
	a.message = nil
	return nil
}

func (a *responsesAccumulator) addFunctionCall(call llm.ToolCall) error {
	if err := a.finishReasoning(); err != nil {
		return err
	}
	if err := a.finishMessage(); err != nil {
		return err
	}
	callID := strings.TrimSpace(call.ID)
	if callID == "" {
		var err error
		callID, err = randomSecret("call", 16)
		if err != nil {
			return err
		}
	}
	itemID, err := randomSecret("fc", 16)
	if err != nil {
		return err
	}
	arguments := strings.TrimSpace(string(call.Arguments))
	if arguments == "" {
		arguments = "{}"
	}
	item := &responsesOutputItem{ID: itemID, Type: "function_call", Status: "in_progress", CallID: callID, Name: call.Name, Arguments: ""}
	index := len(a.document.Output)
	a.document.Output = append(a.document.Output, item)
	if err := a.event("response.output_item.added", map[string]any{"output_index": index, "item": item}); err != nil {
		return err
	}
	if err := a.event("response.function_call_arguments.delta", map[string]any{
		"item_id": itemID, "output_index": index, "delta": arguments,
	}); err != nil {
		return err
	}
	if err := a.event("response.function_call_arguments.done", map[string]any{
		"item_id": itemID, "output_index": index, "arguments": arguments,
	}); err != nil {
		return err
	}
	item.Status = "completed"
	item.Arguments = arguments
	return a.event("response.output_item.done", map[string]any{"output_index": index, "item": item})
}

func (a *responsesAccumulator) complete() error {
	if err := a.finishReasoning(); err != nil {
		return err
	}
	if err := a.finishMessage(); err != nil {
		return err
	}
	inputTokens := a.usage.InputTokens + a.usage.CachedInputTokens + a.usage.CacheWriteTokens
	totalTokens := a.usage.ProviderTotalTokens
	if totalTokens <= 0 {
		totalTokens = inputTokens + a.usage.OutputTokens
	}
	a.document.Status = "completed"
	a.document.Usage = &responsesUsage{
		InputTokens: inputTokens, InputTokensDetails: responsesUsageDetails{CachedTokens: a.usage.CachedInputTokens},
		OutputTokens: a.usage.OutputTokens, OutputTokensDetails: responsesOutputUsageDetails{ReasoningTokens: a.usage.ReasoningTokens},
		TotalTokens: totalTokens,
	}
	return a.event("response.completed", map[string]any{"response": &a.document})
}

func (a *responsesAccumulator) fail(code, message, param string) error {
	if err := a.event("error", map[string]any{
		"code": code, "message": message, "param": nullableString(param),
	}); err != nil {
		return err
	}
	a.document.Status = "failed"
	a.document.Error = map[string]any{"code": code, "message": message}
	return a.event("response.failed", map[string]any{"response": &a.document})
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request, client Client) {
	request, decodeErr := s.decodeResponsesRequest(r)
	if decodeErr != nil {
		s.writeResponsesError(w, decodeErr.Status, decodeErr.Code, decodeErr.Message, decodeErr.Param)
		return
	}
	provider, model, namespaceErr := splitResponsesModel(request.Model)
	if namespaceErr != nil {
		s.writeResponsesError(w, namespaceErr.Status, namespaceErr.Code, namespaceErr.Message, namespaceErr.Param)
		return
	}
	providerReq, translateErr := translateResponsesRequest(request)
	if translateErr != nil {
		s.writeResponsesError(w, translateErr.Status, translateErr.Code, translateErr.Message, translateErr.Param)
		return
	}
	providerReq.Model = model
	requestID, err := randomSecret("req", 16)
	if err != nil {
		s.writeResponsesError(w, http.StatusInternalServerError, "internal", "could not create gateway request", "")
		return
	}
	envelope := protocol.InferenceRequest{Version: protocol.Version, RequestID: requestID, Provider: provider}
	execution, requestErr := s.startInference(r.Context(), client, envelope, providerReq, true)
	if requestErr != nil {
		s.writeResponsesError(w, requestErr.Status, requestErr.Code, requestErr.Message, responsesErrorParam(requestErr.Code))
		return
	}
	errorCode := ""
	defer func() { execution.close(errorCode) }()

	responseID, err := randomSecret("resp", 16)
	if err != nil {
		errorCode = "internal"
		s.writeResponsesError(w, http.StatusInternalServerError, "internal", "could not create response", "")
		return
	}
	var flusher http.Flusher
	if request.Stream {
		var ok bool
		flusher, ok = w.(http.Flusher)
		if !ok {
			errorCode = "streaming_unsupported"
			s.writeResponsesError(w, http.StatusInternalServerError, errorCode, "streaming is unavailable", "stream")
			return
		}
	}
	accumulator := newResponsesAccumulator(responseID, request, providerReq.ParallelToolCalls, nil)
	var prefetched *llm.Event
	streamEOF := false
	if request.Stream {
		event, recvErr := prefetchResponsesStream(execution.stream)
		if recvErr == io.EOF {
			streamEOF = true
		} else if recvErr != nil {
			status, code := s.logProviderStreamError(execution, recvErr)
			errorCode = code
			s.writeResponsesError(w, status, code, safeProviderErrorMessage(code, provider), responsesErrorParam(code))
			return
		} else {
			prefetched = &event
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		accumulator.emit = func(eventType string, payload map[string]any) error {
			if err := writeResponsesSSE(w, payload); err != nil {
				return context.Canceled
			}
			flusher.Flush()
			return nil
		}
		if err := accumulator.created(); err != nil {
			errorCode = "canceled"
			return
		}
	}

	for !streamEOF {
		var event llm.Event
		var recvErr error
		if prefetched != nil {
			event = *prefetched
			prefetched = nil
		} else {
			event, recvErr = execution.stream.Recv()
		}
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			status, code := s.logProviderStreamError(execution, recvErr)
			errorCode = code
			if request.Stream {
				_ = accumulator.fail(code, safeProviderErrorMessage(code, provider), responsesErrorParam(code))
				return
			}
			s.writeResponsesError(w, status, code, safeProviderErrorMessage(code, provider), responsesErrorParam(code))
			return
		}
		if event.Type == llm.EventUsage && event.Use != nil {
			execution.addUsage(*event.Use)
		}
		if err := accumulator.consume(event); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
				errorCode = "canceled"
				return
			}
			var wireErr *responsesWireError
			status, code := classifyProviderError(err, execution.entry.Type)
			if errors.As(err, &wireErr) {
				status, code = wireErr.Status, wireErr.Code
			} else {
				status, code = s.logProviderStreamError(execution, err)
			}
			errorCode = code
			message := safeProviderErrorMessage(code, provider)
			param := responsesErrorParam(code)
			if wireErr != nil {
				message = wireErr.Message
				param = wireErr.Param
			}
			if request.Stream {
				_ = accumulator.fail(code, message, param)
				return
			}
			s.writeResponsesError(w, status, code, message, param)
			return
		}
	}
	if err := accumulator.complete(); err != nil {
		errorCode = "canceled"
		return
	}
	if !request.Stream {
		s.writeResponsesJSON(w, http.StatusOK, &accumulator.document)
	}
}

func responsesErrorParam(code string) string {
	switch code {
	case "unknown_provider", "unknown_model", "policy_denied":
		return "model"
	default:
		return ""
	}
}

func (s *Server) handleResponsesModels(w http.ResponseWriter, r *http.Request, client Client) {
	catalog, err := s.currentCatalog(r.Context())
	if err != nil {
		s.writeResponsesError(w, http.StatusServiceUnavailable, "catalog_unavailable", "gateway model catalog is temporarily unavailable; retry or contact the gateway operator", "")
		return
	}
	catalog = filterCatalog(catalog, s.cfg.Policy, client.Policy)
	data := make([]map[string]any, 0)
	for _, entry := range catalog.Providers {
		for _, model := range entry.Models {
			item := map[string]any{
				"id": entry.Key + "/" + model.ID, "object": "model", "created": model.Created,
				"owned_by": model.OwnedBy, "provider": entry.Key,
			}
			if item["owned_by"] == "" {
				item["owned_by"] = entry.Key
			}
			if model.DisplayName != "" {
				item["display_name"] = model.DisplayName
			}
			if model.InputLimit > 0 {
				item["input_limit"] = model.InputLimit
			}
			if model.OutputLimit > 0 {
				item["output_limit"] = model.OutputLimit
			}
			item["input_price"] = model.InputPrice
			item["output_price"] = model.OutputPrice
			if len(model.ReasoningEfforts) > 0 {
				item["reasoning_efforts"] = model.ReasoningEfforts
			}
			if model.DefaultReasoningEffort != "" {
				item["default_reasoning_effort"] = model.DefaultReasoningEffort
			}
			if len(model.ReasoningModes) > 0 {
				item["reasoning_modes"] = model.ReasoningModes
			}
			data = append(data, item)
		}
	}
	s.writeResponsesJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) writeResponsesError(w http.ResponseWriter, status int, code, message, param string) {
	errorType := "gateway_error"
	if status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusForbidden {
		errorType = "invalid_request_error"
	} else if status == http.StatusUnauthorized {
		errorType = "authentication_error"
	} else if status == http.StatusTooManyRequests {
		errorType = "rate_limit_error"
	}
	s.writeResponsesJSON(w, status, map[string]any{"error": map[string]any{
		"message": message, "type": errorType, "param": nullableString(param), "code": code,
	}})
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Server) writeResponsesJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeResponsesSSE(w io.Writer, payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

func prefetchResponsesStream(stream llm.Stream) (llm.Event, error) {
	for {
		event, err := stream.Recv()
		if err != nil {
			return llm.Event{}, err
		}
		if event.Type == llm.EventRetry {
			// Retry notifications are not Responses output and do not prove that an
			// upstream attempt started. Keep waiting without buffering output.
			continue
		}
		if event.Type == llm.EventError {
			if event.Err != nil {
				return llm.Event{}, event.Err
			}
			return llm.Event{}, fmt.Errorf("provider returned an error event")
		}
		return event, nil
	}
}

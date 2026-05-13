package llm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

type responsesStreamEventHandler struct {
	client                  *ResponsesClient
	responseStateGeneration uint64
	debugRaw                bool
	debugPrefix             string
	toolState               *responsesToolState
	reasoningState          *responsesReasoningState
	lastUsage               *Usage
	outputItems             []ResponsesInputItem
	sawTextDelta            bool
	allowResponseState      bool
	emitted                 bool
}

func newResponsesStreamEventHandler(client *ResponsesClient, responseStateGeneration uint64, debugRaw bool, debugPrefix string, allowResponseState bool) *responsesStreamEventHandler {
	return &responsesStreamEventHandler{
		client:                  client,
		responseStateGeneration: responseStateGeneration,
		debugRaw:                debugRaw,
		debugPrefix:             debugPrefix,
		allowResponseState:      allowResponseState,
		toolState:               newResponsesToolState(),
		reasoningState:          newResponsesReasoningState(),
	}
}

func (h *responsesStreamEventHandler) Emitted() bool { return h.emitted }

func (h *responsesStreamEventHandler) OutputItems() []ResponsesInputItem {
	return append([]ResponsesInputItem(nil), h.outputItems...)
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
		"response.reasoning_summary_text.delta",
		"response.completed",
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

	unmarshalEvent := func(dst any) error {
		if err := json.Unmarshal(data, dst); err != nil {
			return fmt.Errorf("decode Responses API %s event: %w", eventLabel, err)
		}
		return nil
	}

	sendEvent := func(event Event) error {
		h.emitted = true
		return send.Send(event)
	}

	switch eventType {
	case "response.output_text.delta":
		var deltaEvent struct {
			Delta string `json:"delta"`
		}
		if err := unmarshalEvent(&deltaEvent); err != nil {
			return false, err
		}
		if deltaEvent.Delta != "" {
			h.sawTextDelta = true
			if err := sendEvent(Event{Type: EventTextDelta, Text: deltaEvent.Delta}); err != nil {
				return false, err
			}
		}

	case "response.output_item.added":
		var itemEvent struct {
			Item        responsesOutputItem `json:"item"`
			OutputIndex int                 `json:"output_index"`
		}
		if err := unmarshalEvent(&itemEvent); err != nil {
			return false, err
		}
		if itemEvent.Item.Type == "function_call" {
			h.toolState.StartCall(itemEvent.OutputIndex, itemEvent.Item.CallID, itemEvent.Item.Name)
		} else if itemEvent.Item.Type == "reasoning" {
			h.reasoningState.Start(itemEvent.OutputIndex, itemEvent.Item.ID, itemEvent.Item.EncryptedContent, itemEvent.Item.Summary)
		}

	case "response.function_call_arguments.delta":
		var argEvent struct {
			OutputIndex int    `json:"output_index"`
			Delta       string `json:"delta"`
		}
		if err := unmarshalEvent(&argEvent); err != nil {
			return false, err
		}
		h.toolState.AppendArguments(argEvent.OutputIndex, argEvent.Delta)

	case "response.output_item.done":
		var doneEvent struct {
			Item        responsesOutputItem `json:"item"`
			OutputIndex int                 `json:"output_index"`
		}
		if err := unmarshalEvent(&doneEvent); err != nil {
			return false, err
		}
		if doneEvent.Item.Type == "function_call" {
			h.outputItems = append(h.outputItems, responsesOutputItemToInputItem(doneEvent.Item)...)
			h.toolState.FinishCall(doneEvent.OutputIndex, doneEvent.Item.CallID, doneEvent.Item.Name, doneEvent.Item.Arguments)
		} else if doneEvent.Item.Type == "reasoning" {
			h.outputItems = append(h.outputItems, responsesOutputItemToInputItem(doneEvent.Item)...)
			h.reasoningState.Finish(doneEvent.OutputIndex, doneEvent.Item.ID, doneEvent.Item.EncryptedContent, doneEvent.Item.Summary)
			if part := h.reasoningState.Part(doneEvent.OutputIndex); part != nil {
				if err := sendEvent(Event{
					Type:                      EventReasoningDelta,
					Text:                      part.ReasoningContent,
					ReasoningItemID:           part.ReasoningItemID,
					ReasoningEncryptedContent: part.ReasoningEncryptedContent,
				}); err != nil {
					return false, err
				}
			}
		} else if doneEvent.Item.Type == "message" {
			h.outputItems = append(h.outputItems, responsesOutputItemToInputItem(doneEvent.Item)...)
			for _, content := range doneEvent.Item.Content {
				if content.Type == "output_text" && content.Text != "" && !h.sawTextDelta {
					if err := sendEvent(Event{Type: EventTextDelta, Text: content.Text}); err != nil {
						return false, err
					}
				} else if content.Type == "refusal" && content.Refusal != "" {
					if err := sendEvent(Event{Type: EventTextDelta, Text: content.Refusal}); err != nil {
						return false, err
					}
				}
			}
		} else if doneEvent.Item.Type == "image_generation_call" {
			if doneEvent.Item.Result != "" {
				decoded, err := base64.StdEncoding.DecodeString(doneEvent.Item.Result)
				if err != nil {
					return false, fmt.Errorf("decode image_generation_call result: %w", err)
				}
				if err := sendEvent(Event{Type: EventImageGenerated, ImageData: decoded, ImageMimeType: "image/png", RevisedPrompt: doneEvent.Item.RevisedPrompt}); err != nil {
					return false, err
				}
			}
		}

	case "response.reasoning_summary_part.added":
		var partEvent struct {
			OutputIndex int `json:"output_index"`
		}
		if err := unmarshalEvent(&partEvent); err != nil {
			return false, err
		}
		h.reasoningState.Ensure(partEvent.OutputIndex)

	case "response.reasoning_summary_text.delta":
		var summaryDeltaEvent struct {
			OutputIndex int    `json:"output_index"`
			Delta       string `json:"delta"`
		}
		if err := unmarshalEvent(&summaryDeltaEvent); err != nil {
			return false, err
		}
		h.reasoningState.AppendSummary(summaryDeltaEvent.OutputIndex, summaryDeltaEvent.Delta)

	case "response.completed":
		var completedEvent struct {
			Response struct {
				ID    string          `json:"id"`
				Usage *responsesUsage `json:"usage,omitempty"`
			} `json:"response"`
		}
		if err := unmarshalEvent(&completedEvent); err != nil {
			return false, err
		}
		if h.allowResponseState && completedEvent.Response.ID != "" {
			h.client.setLastResponseIDIfGeneration(h.responseStateGeneration, completedEvent.Response.ID)
		}
		if completedEvent.Response.Usage != nil {
			cached := completedEvent.Response.Usage.InputTokensDetails.CachedTokens
			h.lastUsage = &Usage{
				InputTokens:            completedEvent.Response.Usage.InputTokens - cached,
				OutputTokens:           completedEvent.Response.Usage.OutputTokens,
				CachedInputTokens:      cached,
				ProviderRawInputTokens: completedEvent.Response.Usage.InputTokens,
				ProviderTotalTokens:    completedEvent.Response.Usage.TotalTokens,
				ReasoningTokens:        completedEvent.Response.Usage.OutputTokensDetails.ReasoningTokens,
			}
		}
		return true, nil

	case "response.failed", "error":
		var errorEvent struct {
			Status   int             `json:"status,omitempty"`
			Error    *responsesError `json:"error"`
			Response struct {
				Error *responsesError `json:"error"`
			} `json:"response"`
		}
		if err := unmarshalEvent(&errorEvent); err != nil {
			return false, err
		}
		apiErr := errorEvent.Error
		if apiErr == nil {
			apiErr = errorEvent.Response.Error
		}
		if apiErr != nil {
			return false, &responsesAPIEventError{Status: errorEvent.Status, APIError: apiErr}
		}
		return false, fmt.Errorf("Responses API error: unknown error")
	}
	return false, nil
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
		return []ResponsesInputItem{{Type: "function_call", CallID: callID, Name: item.Name, Arguments: args}}
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

func (h *responsesStreamEventHandler) Finish(send eventSender) error {
	if err := h.toolState.Validate(); err != nil {
		return err
	}
	for _, call := range h.toolState.Calls() {
		if err := send.Send(Event{Type: EventToolCall, Tool: &call}); err != nil {
			return err
		}
	}
	if h.lastUsage != nil {
		if err := send.Send(Event{Type: EventUsage, Use: h.lastUsage}); err != nil {
			return err
		}
	}
	return send.Send(Event{Type: EventDone})
}

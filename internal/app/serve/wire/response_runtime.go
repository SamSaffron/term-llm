package wire

import "slices"

// ResponseRuntimeMetadata projects authoritative runtime identity from a response
// event, preserving known fields when older frames omit them.
func ResponseRuntimeMetadata(event string, payload map[string]any, provider, model, effort string, effortSet bool) (string, string, string, bool) {
	var source map[string]any
	switch event {
	case "response.created", "response.completed", "response.cancelled":
		source = responseMap(payload["response"])
	case "response.failed":
		// Older failure frames name the attempted model, not the retained runtime.
		// Only frames with explicit provider identity are authoritative.
		source = responseMap(payload["response"])
		if responseString(source["provider"]) == "" {
			return provider, model, effort, effortSet
		}
	case "response.model_switch":
		source = payload
	case "response.model_swap.progress":
		var prefix string
		switch responseString(payload["stage"]) {
		case "complete":
			prefix = "target_"
		case "failed":
			prefix = "previous_"
		default:
			return provider, model, effort, effortSet
		}
		source = map[string]any{"provider": payload[prefix+"provider"], "model": payload[prefix+"model"]}
		if effort, ok := payload[prefix+"effort"]; ok {
			source["reasoning_effort"] = effort
		}
	default:
		return provider, model, effort, effortSet
	}
	if len(source) == 0 {
		return provider, model, effort, effortSet
	}
	nextModel := responseString(source["model"])
	nextProvider := responseString(source["provider"])
	nextEffort, nextEffortSet := source["reasoning_effort"]
	if event == "response.model_switch" {
		if toModel := responseString(source["to_model"]); toModel != "" {
			nextModel = toModel
		}
		if toProvider := responseString(source["to_provider"]); toProvider != "" {
			nextProvider = toProvider
		}
		if toEffort, ok := source["to_reasoning_effort"]; ok && toEffort != nil {
			nextEffort, nextEffortSet = toEffort, true
		}
	}
	if nextModel != "" {
		model = nextModel
	}
	if nextProvider != "" {
		provider = nextProvider
	}
	if nextEffortSet {
		effort = responseString(nextEffort)
		effortSet = true
	}
	return provider, model, effort, effortSet
}

func responseString(v any) string {
	s, _ := v.(string)
	return s
}

func responseMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// ClearsModelSwapProgress reports events that replace transient handover status
// with output or a terminal result, matching the browser's response reducer.
func ClearsModelSwapProgress(event string) bool {
	switch event {
	case "response.output_text.new_segment", "response.output_text.delta", "response.reasoning.delta",
		"response.output_item.added", "response.output_item.done", "response.tool_exec.start", "response.tool_exec.end",
		"response.attempt.discard", "response.model_switch", "response.completed", "response.cancelled", "response.failed":
		return true
	}
	return false
}

// RecoveryEvent is a retained control event in a response recovery snapshot.
type RecoveryEvent struct {
	Event   string
	Payload map[string]any
}

// ClearModelSwapProgress removes transient progress without disturbing prompts.
func ClearModelSwapProgress(events []RecoveryEvent) []RecoveryEvent {
	return slices.DeleteFunc(events, func(event RecoveryEvent) bool {
		return event.Event == "response.model_swap.progress"
	})
}

// WithModelSwapProgress retains only the latest nonterminal handover stage.
func WithModelSwapProgress(events []RecoveryEvent, event string, payload map[string]any) []RecoveryEvent {
	events = ClearModelSwapProgress(events)
	switch responseString(payload["stage"]) {
	case "complete", "failed":
		return events
	}
	return append(events, RecoveryEvent{Event: event, Payload: CloneJSONMap(payload)})
}

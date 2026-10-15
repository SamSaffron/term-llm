package llm

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

// responsesHistoryFacts summarises the transcript properties that change how a
// Responses request must be shaped. Providers compute it once per request with
// scanResponsesHistory instead of re-walking the history for each question.
type responsesHistoryFacts struct {
	// HasDiscoveryParts reports provider-neutral native tool discovery parts.
	HasDiscoveryParts bool
	// HasReplayedWebSearch reports a replayed hosted web_search_call item.
	HasReplayedWebSearch bool
}

var responsesWebSearchCallMarker = []byte(`"web_search_call"`)

// scanResponsesHistory walks the transcript once. Replay items are only decoded
// when a cheap byte search shows they could be a web_search_call, so large
// opaque items (encrypted reasoning, messages) cost a single memchr-style pass.
func scanResponsesHistory(messages []Message) responsesHistoryFacts {
	var facts responsesHistoryFacts
	for _, msg := range messages {
		for i := range msg.Parts {
			part := &msg.Parts[i]
			switch part.Type {
			case PartDiscoveryCall, PartDiscoveryOutput:
				facts.HasDiscoveryParts = true
			case PartProviderReplay:
				if !facts.HasReplayedWebSearch && part.ProviderReplay != nil && isResponsesWebSearchCall(part.ProviderReplay.Raw) {
					facts.HasReplayedWebSearch = true
				}
			}
			if facts.HasDiscoveryParts && facts.HasReplayedWebSearch {
				return facts
			}
		}
	}
	return facts
}

func isResponsesWebSearchCall(raw json.RawMessage) bool {
	if !bytes.Contains(raw, responsesWebSearchCallMarker) {
		return false
	}
	var item struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &item) == nil && item.Type == "web_search_call"
}

// guardReplayedWebSearch keeps a request valid when its history replays a
// hosted web_search_call but the caller did not declare web search, for example
// after search was switched off. OpenAI's ChatGPT backend rejects that shape
// with "response protection is unavailable" (the public API currently accepts
// it, but a session can move between the two).
//
// Preferred: declare a cached-only web_search tool purely to satisfy the check
// and narrow tool_choice so the model cannot call it. When tool_choice cannot
// exclude it (see responsesToolChoiceExcludingReplayWebSearch), the replayed
// web_search_call items are dropped from the request instead; the backend
// accepts that history and the assistant text that used the results remains.
// Either way search stays uncallable. Requests that already declare web search,
// or whose history has no replayed search, are left untouched.
func guardReplayedWebSearch(req *ResponsesRequest, facts responsesHistoryFacts) {
	if !facts.HasReplayedWebSearch || responsesToolsDeclareWebSearch(req.Tools) {
		return
	}
	choice, ok := responsesToolChoiceExcludingReplayWebSearch(req.ToolChoice, req.Tools)
	if !ok {
		req.Messages = withoutReplayedWebSearchCalls(req.Messages)
		req.Input = withoutReplayedWebSearchInputItems(req.Input)
		return
	}
	tools := make([]any, 0, len(req.Tools)+1)
	tools = append(tools, ResponsesWebSearchTool{Type: "web_search", ExternalWebAccess: boolPtr(false)})
	req.Tools = append(tools, req.Tools...)
	req.ToolChoice = choice
}

// responsesToolChoiceExcludingReplayWebSearch returns a tool_choice under which
// a replay-only web_search tool cannot be called, and whether one exists.
//   - no caller tools: "none" (including "required", which has nothing to
//     require once web_search is excluded)
//   - nil/"auto"/"required": allowed_tools over the caller's own tools
//   - "none" and named choices already exclude web_search: unchanged
//
// allowed_tools only accepts some tool types; notably tool_search is rejected.
// If any caller tool cannot be listed, ok is false.
func responsesToolChoiceExcludingReplayWebSearch(choice any, callerTools []any) (any, bool) {
	if len(callerTools) == 0 {
		return "none", true
	}
	mode := "auto"
	switch choice {
	case nil, "auto":
	case "required":
		mode = "required"
	default:
		return choice, true
	}
	allowed := make([]map[string]any, 0, len(callerTools))
	for _, tool := range callerTools {
		toolType, name := responsesToolIdentity(tool)
		if !responsesAllowedToolsAcceptsType[toolType] {
			return nil, false
		}
		ref := map[string]any{"type": toolType}
		if name != "" {
			ref["name"] = name
		}
		allowed = append(allowed, ref)
	}
	return map[string]any{"type": "allowed_tools", "mode": mode, "tools": allowed}, true
}

// responsesAllowedToolsAcceptsType lists caller tool types that may appear in an
// allowed_tools tool_choice. Source: the ChatGPT backend's validation error for
// a tool_search entry, verbatim: "Invalid value: 'tool_search'. Supported values
// are: 'function', 'custom', 'image_generation', 'programmatic_tool_calling',
// and 'web_search'." web_search is never a caller tool here.
var responsesAllowedToolsAcceptsType = map[string]bool{
	"function":                  true,
	"custom":                    true,
	"image_generation":          true,
	"programmatic_tool_calling": true,
}

// withoutReplayedWebSearchCalls returns messages without replayed
// web_search_call parts, copying only the messages that change.
func withoutReplayedWebSearchCalls(messages []Message) []Message {
	out := messages
	copied := false
	for i, msg := range messages {
		if !slices.ContainsFunc(msg.Parts, isReplayedWebSearchPart) {
			continue
		}
		if !copied {
			out = slices.Clone(messages)
			copied = true
		}
		out[i].Parts = slices.DeleteFunc(slices.Clone(msg.Parts), isReplayedWebSearchPart)
	}
	return out
}

func isReplayedWebSearchPart(part Part) bool {
	return part.Type == PartProviderReplay && part.ProviderReplay != nil && isResponsesWebSearchCall(part.ProviderReplay.Raw)
}

func withoutReplayedWebSearchInputItems(items []ResponsesInputItem) []ResponsesInputItem {
	if items == nil {
		return nil
	}
	out := make([]ResponsesInputItem, 0, len(items))
	for _, item := range items {
		if len(item.Raw) > 0 && isResponsesWebSearchCall(item.Raw) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func responsesToolsDeclareWebSearch(tools []any) bool {
	for _, tool := range tools {
		if toolType, _ := responsesToolIdentity(tool); strings.HasPrefix(toolType, "web_search") {
			return true
		}
	}
	return false
}

// responsesToolIdentity returns the wire type and, for named tools, the name of
// a Responses tool declaration.
func responsesToolIdentity(tool any) (toolType, name string) {
	switch t := tool.(type) {
	case ResponsesTool:
		return t.Type, t.Name
	case ResponsesWebSearchTool:
		return t.Type, ""
	case ResponsesToolSearchTool:
		return t.Type, ""
	case ResponsesImageGenerationTool:
		return t.Type, ""
	case map[string]any:
		toolType, _ = t["type"].(string)
		name, _ = t["name"].(string)
		return toolType, name
	}
	raw, err := json.Marshal(tool)
	if err != nil {
		return "", ""
	}
	var desc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &desc) != nil {
		return "", ""
	}
	return desc.Type, desc.Name
}

package opencompat

import (
	"encoding/json"
	"strings"
)

// marshalCompatChatRequest preserves empty assistant reasoning for DeepSeek's
// thinking-mode replay contract. DeepSeek can return empty reasoning during a
// tool chain, then reject that same history if reasoning_content is omitted.
// OpenCode applies the same workaround: anomalyco/opencode#24146.
func marshalCompatChatRequest(req oaiChatRequest, vllm bool) ([]byte, error) {
	if vllm || !strings.Contains(strings.ToLower(req.Model), "deepseek") {
		return json.Marshal(req)
	}

	type replayMessage struct {
		oaiMessage
		ReasoningContent *string `json:"reasoning_content,omitempty"`
	}
	messages := make([]replayMessage, len(req.Messages))
	for i, msg := range req.Messages {
		messages[i].oaiMessage = msg
		if msg.Role == "assistant" || msg.ReasoningContent != "" {
			reasoning := msg.ReasoningContent
			messages[i].ReasoningContent = &reasoning
		}
	}
	return json.Marshal(struct {
		oaiChatRequest
		Messages []replayMessage `json:"messages"`
	}{oaiChatRequest: req, Messages: messages})
}

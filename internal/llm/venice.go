package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const veniceBaseURL = "https://api.venice.ai/api/v1"

type VeniceProvider struct {
	*OpenAICompatProvider
}

func NewVeniceProvider(apiKey, model string) *VeniceProvider {
	if model == "" {
		model = "venice-uncensored"
	}
	return &VeniceProvider{OpenAICompatProvider: NewOpenAICompatProvider(veniceBaseURL, apiKey, model, "Venice")}
}

func (p *VeniceProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch:    true,
		NativeWebFetch:     false,
		ToolCalls:          true,
		SupportsToolChoice: true,
	}
}

func (p *VeniceProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	req.MaxOutputTokens = ClampOutputTokens(req.MaxOutputTokens, chooseModel(req.Model, p.model))
	messages := buildCompatMessages(req.Messages)
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages provided")
	}

	tools, err := buildCompatTools(req.Tools)
	if err != nil {
		return nil, err
	}

	model, veniceParams := buildVeniceModelAndParams(chooseModel(req.Model, p.model), req.Search)
	chatReq := oaiChatRequest{
		Model:            model,
		Messages:         messages,
		Tools:            tools,
		Stream:           true,
		VeniceParameters: veniceParams,
	}

	if req.ToolChoice.Mode != "" {
		chatReq.ToolChoice = buildCompatToolChoice(req.ToolChoice)
	}
	if req.Temperature > 0 {
		v := float64(req.Temperature)
		chatReq.Temperature = &v
	}
	if req.TopP > 0 {
		v := float64(req.TopP)
		chatReq.TopP = &v
	}
	if req.MaxOutputTokens > 0 {
		v := req.MaxOutputTokens
		chatReq.MaxTokens = &v
	}

	if req.Debug {
		fmt.Fprintf(os.Stderr, "=== DEBUG: %s Stream Request ===\n", p.name)
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "URL: %s/chat/completions\n", p.baseURL)
		fmt.Fprintf(os.Stderr, "Model: %s\n", model)
		fmt.Fprintf(os.Stderr, "Messages: %d\n", len(messages))
		fmt.Fprintf(os.Stderr, "Tools: %d\n", len(tools))
		fmt.Fprintf(os.Stderr, "Venice params: %v\n", veniceParams)
		fmt.Fprintln(os.Stderr, "===================================")
	}

	resp, err := p.makeChatRequest(ctx, chatReq)
	if err != nil {
		return nil, fmt.Errorf("%s API request failed: %w", p.name, err)
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("%s API error (status %d): %s", p.name, resp.StatusCode, string(body))
	}

	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		toolState := newCompatToolState()
		var lastUsage *Usage
		var lastEventType string
		var reasoningBuilder strings.Builder

		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				lastEventType = strings.TrimPrefix(line, "event: ")
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}

			var chatResp oaiChatResponse
			if err := json.Unmarshal([]byte(data), &chatResp); err != nil {
				continue
			}
			if lastEventType == "error" || chatResp.Error != nil {
				errMsg := "unknown error"
				if chatResp.Error != nil {
					errMsg = chatResp.Error.Message
				}
				return fmt.Errorf("%s API error: %s", p.name, errMsg)
			}
			if chatResp.Usage != nil {
				cached := chatResp.Usage.PromptTokensDetails.CachedTokens
				lastUsage = &Usage{InputTokens: chatResp.Usage.PromptTokens - cached, OutputTokens: chatResp.Usage.CompletionTokens, CachedInputTokens: cached}
			}
			for _, choice := range chatResp.Choices {
				if choice.Delta != nil {
					if content, ok := choice.Delta.Content.(string); ok && content != "" {
						events <- Event{Type: EventTextDelta, Text: content}
					}
					if choice.Delta.Reasoning != "" {
						reasoningBuilder.WriteString(choice.Delta.Reasoning)
						events <- Event{Type: EventReasoningDelta, Text: choice.Delta.Reasoning}
					}
					if len(choice.Delta.ToolCalls) > 0 {
						toolState.Add(choice.Delta.ToolCalls)
					}
				}
			}
			lastEventType = ""
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("%s streaming error: %w", p.name, err)
		}
		for _, call := range toolState.Calls() {
			events <- Event{Type: EventToolCall, Tool: &call}
		}
		if lastUsage != nil {
			events <- Event{Type: EventUsage, Use: lastUsage}
		}
		events <- Event{Type: EventDone}
		return nil
	}), nil
}

func buildVeniceModelAndParams(model string, search bool) (string, map[string]interface{}) {
	baseModel, suffixParams := parseVeniceModelSuffix(model)
	params := make(map[string]interface{}, len(suffixParams)+1)
	for k, v := range suffixParams {
		params[k] = v
	}
	if search {
		if _, hasX := params["enable_x_search"]; !hasX {
			if _, hasWeb := params["enable_web_search"]; !hasWeb {
				params["enable_web_search"] = "on"
			}
		}
	}
	if len(params) == 0 {
		return baseModel, nil
	}
	return baseModel, params
}

func parseVeniceModelSuffix(model string) (string, map[string]interface{}) {
	parts := strings.SplitN(model, ":", 2)
	if len(parts) < 2 {
		return model, nil
	}
	params := map[string]interface{}{}
	for _, raw := range strings.Split(parts[1], "&") {
		if raw == "" {
			continue
		}
		kv := strings.SplitN(raw, "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			continue
		}
		params[kv[0]] = parseVeniceParamValue(kv[1])
	}
	if len(params) == 0 {
		return model, nil
	}
	return parts[0], params
}

func parseVeniceParamValue(v string) interface{} {
	switch strings.ToLower(v) {
	case "true":
		return true
	case "false":
		return false
	}
	if i, err := strconv.Atoi(v); err == nil {
		return i
	}
	return v
}

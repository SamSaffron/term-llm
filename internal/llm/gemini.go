package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

// GeminiProvider implements Provider using the Google Gemini API.
type GeminiProvider struct {
	apiKey         string
	model          string
	thinkingLevel  geminiThinkingLevel // for Gemini 3: MINIMAL, LOW, HIGH
	thinkingBudget *int32              // for Gemini 2.5: 0, 8192, etc.
	baseURL        string
	client         *http.Client
}

// geminiThinkingConfig holds thinking configuration for a Gemini model
type geminiThinkingConfig struct {
	level  geminiThinkingLevel // for Gemini 3
	budget *int32              // for Gemini 2.5 (nil = no config)
}

// parseGeminiModelThinking extracts the base model name and determines thinking config.
// Gemini 3 models use thinkingLevel (MINIMAL/LOW/HIGH).
// Gemini 2.5 models use thinkingBudget (0 = disabled).
func parseGeminiModelThinking(model string) (string, geminiThinkingConfig) {
	hasThinkingSuffix := strings.HasSuffix(model, "-thinking")
	baseModel := strings.TrimSuffix(model, "-thinking")

	switch {
	// Gemini 3 Flash - use minimal thinking by default and high with -thinking.
	case strings.HasPrefix(baseModel, "gemini-3-flash"):
		if hasThinkingSuffix {
			return baseModel, geminiThinkingConfig{level: geminiThinkingLevelHigh}
		}
		return baseModel, geminiThinkingConfig{level: geminiThinkingLevelMinimal}

	// Gemini 3 Pro - only supports LOW and HIGH (not MINIMAL)
	case strings.HasPrefix(baseModel, "gemini-3-pro"):
		if hasThinkingSuffix {
			return baseModel, geminiThinkingConfig{level: geminiThinkingLevelHigh}
		}
		return baseModel, geminiThinkingConfig{level: geminiThinkingLevelLow}

	// Gemini 2.5 models - disable thinking with thinkingBudget=0
	case strings.HasPrefix(baseModel, "gemini-2.5"):
		zero := int32(0)
		return baseModel, geminiThinkingConfig{budget: &zero}

	// Unknown model - no thinking config
	default:
		return model, geminiThinkingConfig{}
	}
}

func NewGeminiProvider(apiKey, model string) *GeminiProvider {
	if model == "" {
		model = config.DefaultProviderModel("gemini")
	}
	baseModel, thinkingCfg := parseGeminiModelThinking(model)
	return &GeminiProvider{
		apiKey:         apiKey,
		model:          baseModel,
		thinkingLevel:  thinkingCfg.level,
		thinkingBudget: thinkingCfg.budget,
		baseURL:        geminiAPIBaseURL,
		client:         defaultHTTPClient,
	}
}

func (p *GeminiProvider) Name() string {
	if p.thinkingLevel != "" {
		return fmt.Sprintf("Gemini (%s, thinking=%s)", p.model, strings.ToLower(string(p.thinkingLevel)))
	}
	if p.thinkingBudget != nil {
		return fmt.Sprintf("Gemini (%s, thinkingBudget=%d)", p.model, *p.thinkingBudget)
	}
	return fmt.Sprintf("Gemini (%s)", p.model)
}

func (p *GeminiProvider) Credential() string {
	return "api_key"
}

func (p *GeminiProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch:    true,
		NativeWebFetch:     false, // No native URL fetch
		ToolCalls:          true,
		SupportsToolChoice: true,
	}
}

func (p *GeminiProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	effectiveModel := chooseModel(req.Model, p.model)
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		system, contents := buildGeminiContents(req.Messages)
		if len(contents) == 0 {
			return fmt.Errorf("no user content provided")
		}

		apiReq := geminiGenerateContentRequest{Contents: contents}
		if system != "" {
			apiReq.SystemInstruction = &geminiContent{Role: geminiRoleUser, Parts: []*geminiPart{{Text: system}}}
		}

		generation := &geminiGenerationConfig{}
		// Thinking is not supported together with search or function tools.
		if !req.Search && len(req.Tools) == 0 {
			if p.thinkingLevel != "" {
				generation.ThinkingConfig = &geminiThinkingAPIConfig{ThinkingLevel: p.thinkingLevel}
			} else if p.thinkingBudget != nil {
				generation.ThinkingConfig = &geminiThinkingAPIConfig{ThinkingBudget: p.thinkingBudget}
			}
		}
		if req.TemperatureSet || req.Temperature != 0 {
			temperature := req.Temperature
			generation.Temperature = &temperature
		}
		if req.TopPSet || req.TopP != 0 {
			topP := req.TopP
			generation.TopP = &topP
		}
		generation.MaxOutputTokens = ClampOutputTokens(req.MaxOutputTokens, effectiveModel)
		if generation.ThinkingConfig != nil || generation.Temperature != nil || generation.TopP != nil || generation.MaxOutputTokens > 0 {
			apiReq.GenerationConfig = generation
		}

		if req.Search {
			apiReq.Tools = append(apiReq.Tools, &geminiTool{GoogleSearch: &geminiGoogleSearch{}})
		}

		if len(req.Tools) > 0 {
			apiReq.Tools = append(apiReq.Tools, buildGeminiTools(req.Tools)...)
			apiReq.ToolConfig = buildGeminiToolConfig(req.ToolChoice)
		}

		if req.Debug {
			userPreview := collectGeminiUserPreview(contents)
			fmt.Fprintln(os.Stderr, "=== DEBUG: Gemini Stream Request ===")
			fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
			fmt.Fprintf(os.Stderr, "System: %s\n", truncate(system, 200))
			fmt.Fprintf(os.Stderr, "User: %s\n", truncate(userPreview, 200))
			fmt.Fprintf(os.Stderr, "Input Items: %d\n", len(contents))
			fmt.Fprintf(os.Stderr, "Tools: %d\n", len(req.Tools))
			fmt.Fprintln(os.Stderr, "====================================")
		}

		var lastThoughtSig []byte
		var sources []string
		var usageResp *geminiGenerateContentResponse
		sawCandidateContent := false
		err := streamGeminiResponses(ctx, p.client, p.baseURL, p.apiKey, effectiveModel, apiReq, func(resp *geminiGenerateContentResponse) error {
			if resp.UsageMetadata != nil {
				usageResp = resp
			}
			if len(resp.Candidates) > 0 && resp.Candidates[0] != nil && resp.Candidates[0].Content != nil {
				if len(resp.Candidates[0].Content.Parts) > 0 {
					sawCandidateContent = true
				}
				if err := emitGeminiParts(send, resp.Candidates[0].Content.Parts, &lastThoughtSig); err != nil {
					return err
				}
			}
			if req.Search {
				for _, cand := range resp.Candidates {
					if cand != nil && cand.GroundingMetadata != nil {
						for _, chunk := range cand.GroundingMetadata.GroundingChunks {
							if chunk != nil && chunk.Web != nil && chunk.Web.URI != "" {
								title := chunk.Web.Title
								if title == "" {
									title = "Source"
								}
								source := fmt.Sprintf("[%s](%s)", title, chunk.Web.URI)
								if !containsString(sources, source) {
									sources = append(sources, source)
								}
							}
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("gemini streaming error: %w", err)
		}
		if !sawCandidateContent {
			return fmt.Errorf("gemini streaming error: response contained no candidate content")
		}

		if len(sources) > 0 {
			if err := send.Send(Event{Type: EventTextDelta, Text: "\n\n**Sources:**\n"}); err != nil {
				return err
			}
			for _, source := range sources {
				if err := send.Send(Event{Type: EventTextDelta, Text: "- " + source + "\n"}); err != nil {
					return err
				}
			}
		}
		if err := emitGeminiUsage(send, usageResp); err != nil {
			return err
		}
		if err := send.Send(Event{Type: EventDone}); err != nil {
			return err
		}
		return nil
	}), nil
}

func emitGeminiParts(send eventSender, parts []*geminiPart, lastThoughtSig *[]byte) error {
	var text strings.Builder
	flushText := func() error {
		if text.Len() == 0 {
			return nil
		}
		if err := send.Send(Event{Type: EventTextDelta, Text: text.String()}); err != nil {
			return err
		}
		text.Reset()
		return nil
	}

	for _, part := range parts {
		if part == nil {
			continue
		}
		if part.Thought && len(part.ThoughtSignature) > 0 {
			*lastThoughtSig = append((*lastThoughtSig)[:0], part.ThoughtSignature...)
		}
		if part.Text != "" && !part.Thought {
			text.WriteString(part.Text)
			continue
		}
		if part.FunctionCall == nil {
			continue
		}
		if err := flushText(); err != nil {
			return err
		}
		argsJSON, _ := jsonMarshal(part.FunctionCall.Args)
		thoughtSig := part.ThoughtSignature
		if len(thoughtSig) == 0 {
			thoughtSig = *lastThoughtSig
		}
		if len(thoughtSig) > 0 {
			thoughtSig = append([]byte(nil), thoughtSig...)
		}
		if err := send.Send(Event{Type: EventToolCall, Tool: &ToolCall{
			ID:         part.FunctionCall.ID,
			Name:       part.FunctionCall.Name,
			Arguments:  argsJSON,
			ThoughtSig: thoughtSig,
		}}); err != nil {
			return err
		}
	}

	return flushText()
}

func emitGeminiUsage(send eventSender, resp *geminiGenerateContentResponse) error {
	if resp == nil || resp.UsageMetadata == nil {
		return nil
	}
	if resp.UsageMetadata.TotalTokenCount > 0 {
		return send.Send(Event{Type: EventUsage, Use: &Usage{
			InputTokens:  int(resp.UsageMetadata.PromptTokenCount),
			OutputTokens: int(resp.UsageMetadata.CandidatesTokenCount + resp.UsageMetadata.ThoughtsTokenCount),
		}})
	}
	return nil
}

func buildGeminiTools(specs []ToolSpec) []*geminiTool {
	if len(specs) == 0 {
		return nil
	}
	tools := make([]*geminiTool, 0, len(specs))
	for _, spec := range specs {
		// Normalize schema for Gemini's requirements (similar to OpenAI normalization)
		schema := normalizeSchemaForGemini(spec.Schema)
		tools = append(tools, &geminiTool{
			FunctionDeclarations: []*geminiFunctionDeclaration{
				{
					Name:        spec.Name,
					Description: spec.Description,
					Parameters:  schemaToGemini(schema),
				},
			},
		})
	}
	return tools
}

func containsString(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

func jsonMarshal(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	return json.RawMessage(b), err
}

func buildGeminiContents(messages []Message) (string, []*geminiContent) {
	messages = sanitizeToolHistory(messages)

	var systemParts []string
	contents := make([]*geminiContent, 0, len(messages))

	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			if text := collectTextParts(msg.Parts); text != "" {
				systemParts = append(systemParts, text)
			}
		case RoleUser:
			content := buildGeminiContent(geminiRoleUser, msg.Parts)
			if content != nil {
				contents = append(contents, content)
			}
		case RoleAssistant:
			content := buildGeminiContent(geminiRoleModel, msg.Parts)
			if content != nil {
				contents = append(contents, content)
			}
		case RoleTool:
			content := buildGeminiToolResultContent(msg.Parts)
			if content != nil {
				contents = append(contents, content)
			}
		}
	}

	return strings.Join(systemParts, "\n\n"), contents
}

func buildGeminiContent(role string, parts []Part) *geminiContent {
	content := &geminiContent{Role: role}
	for _, part := range parts {
		switch part.Type {
		case PartText, PartFile:
			if part.Text != "" {
				content.Parts = append(content.Parts, &geminiPart{Text: part.Text})
			}
		case PartImage:
			if part.ImageData != nil && strings.TrimSpace(part.ImageData.Base64) != "" {
				imageData, err := base64.StdEncoding.DecodeString(part.ImageData.Base64)
				if err == nil {
					content.Parts = append(content.Parts, &geminiPart{
						InlineData: &geminiBlob{
							MIMEType: part.ImageData.MediaType,
							Data:     imageData,
						},
					})
					if part.ImagePath != "" {
						content.Parts = append(content.Parts, &geminiPart{Text: "[image saved at: " + part.ImagePath + "]"})
					}
				}
			}
		case PartToolCall:
			if part.ToolCall == nil {
				continue
			}
			args := toolArgsToMap(part.ToolCall.Arguments)
			content.Parts = append(content.Parts, &geminiPart{
				FunctionCall: &geminiFunctionCall{
					ID:   part.ToolCall.ID,
					Name: part.ToolCall.Name,
					Args: args,
				},
				ThoughtSignature: part.ToolCall.ThoughtSig, // Required for Gemini 3 thinking models
			})
		}
	}
	if len(content.Parts) == 0 {
		return nil
	}
	return content
}

func buildGeminiToolResultContent(parts []Part) *geminiContent {
	content := &geminiContent{Role: geminiRoleUser}
	for _, part := range parts {
		switch part.Type {
		case PartText, PartFile:
			if part.Text != "" {
				content.Parts = append(content.Parts, &geminiPart{Text: part.Text})
			}
		case PartToolResult:
			if part.ToolResult == nil {
				continue
			}

			textContent := toolResultTextContent(part.ToolResult)

			// Add the function response with text content
			// Include ThoughtSignature if present (required for Gemini 3 thinking models)
			content.Parts = append(content.Parts, &geminiPart{
				FunctionResponse: &geminiFunctionResponse{
					ID:       part.ToolResult.ID,
					Name:     part.ToolResult.Name,
					Response: map[string]any{"output": textContent},
				},
				ThoughtSignature: part.ToolResult.ThoughtSig,
			})

			for _, contentPart := range toolResultContentParts(part.ToolResult) {
				mimeType, base64Data, ok := toolResultImageData(contentPart)
				if !ok {
					continue
				}
				imageData, err := base64.StdEncoding.DecodeString(base64Data)
				if err != nil {
					continue
				}
				content.Parts = append(content.Parts, &geminiPart{
					InlineData: &geminiBlob{
						MIMEType: mimeType,
						Data:     imageData,
					},
				})
			}
		}
	}
	if len(content.Parts) == 0 {
		return nil
	}
	return content
}

func toolArgsToMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err == nil {
		return args
	}
	return map[string]any{"_raw": string(raw)}
}

func buildGeminiToolConfig(choice ToolChoice) *geminiToolConfig {
	mode := geminiFunctionCallingConfigModeAuto
	var allowed []string

	switch choice.Mode {
	case ToolChoiceNone:
		mode = geminiFunctionCallingConfigModeNone
	case ToolChoiceRequired:
		mode = geminiFunctionCallingConfigModeAny
	case ToolChoiceName:
		if strings.TrimSpace(choice.Name) != "" {
			mode = geminiFunctionCallingConfigModeAny
			allowed = []string{choice.Name}
		}
	case ToolChoiceAuto:
		mode = geminiFunctionCallingConfigModeAuto
	}

	cfg := &geminiToolConfig{
		FunctionCallingConfig: &geminiFunctionCallingConfig{
			Mode:                 mode,
			AllowedFunctionNames: allowed,
		},
	}

	return cfg
}

func collectGeminiUserPreview(contents []*geminiContent) string {
	var parts []string
	for _, content := range contents {
		if content.Role != geminiRoleUser {
			continue
		}
		for _, part := range content.Parts {
			if part.Text != "" {
				parts = append(parts, part.Text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

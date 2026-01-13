package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"google.golang.org/genai"
)

// GeminiProvider implements Provider using the Google Gemini API.
type GeminiProvider struct {
	apiKey         string
	model          string
	thinkingLevel  genai.ThinkingLevel // for Gemini 3: MINIMAL, LOW, HIGH
	thinkingBudget *int32              // for Gemini 2.5: 0, 8192, etc.
}

// geminiThinkingConfig holds thinking configuration for a Gemini model
type geminiThinkingConfig struct {
	level  genai.ThinkingLevel // for Gemini 3
	budget *int32              // for Gemini 2.5 (nil = no config)
}

// parseGeminiModelThinking extracts the base model name and determines thinking config.
// Gemini 3 models use thinkingLevel (MINIMAL/LOW/HIGH).
// Gemini 2.5 models use thinkingBudget (0 = disabled).
func parseGeminiModelThinking(model string) (string, geminiThinkingConfig) {
	hasThinkingSuffix := strings.HasSuffix(model, "-thinking")
	baseModel := strings.TrimSuffix(model, "-thinking")

	switch {
	// Gemini 3 Flash - supports MINIMAL, LOW, MEDIUM, HIGH
	case strings.HasPrefix(baseModel, "gemini-3-flash"):
		if hasThinkingSuffix {
			return baseModel, geminiThinkingConfig{level: genai.ThinkingLevelHigh}
		}
		return baseModel, geminiThinkingConfig{level: genai.ThinkingLevelMinimal}

	// Gemini 3 Pro - only supports LOW and HIGH (not MINIMAL)
	case strings.HasPrefix(baseModel, "gemini-3-pro"):
		if hasThinkingSuffix {
			return baseModel, geminiThinkingConfig{level: genai.ThinkingLevelHigh}
		}
		return baseModel, geminiThinkingConfig{level: genai.ThinkingLevelLow}

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
	baseModel, thinkingCfg := parseGeminiModelThinking(model)
	return &GeminiProvider{
		apiKey:         apiKey,
		model:          baseModel,
		thinkingLevel:  thinkingCfg.level,
		thinkingBudget: thinkingCfg.budget,
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
		NativeWebSearch: true,
		NativeWebFetch:  false, // No native URL fetch
		ToolCalls:       true,
	}
}

func (p *GeminiProvider) newClient(ctx context.Context) (*genai.Client, error) {
	return genai.NewClient(ctx, &genai.ClientConfig{APIKey: p.apiKey})
}

func (p *GeminiProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		client, err := p.newClient(ctx)
		if err != nil {
			return fmt.Errorf("failed to create gemini client: %w", err)
		}

		system, contents := buildGeminiContents(req.Messages)
		if len(contents) == 0 {
			return fmt.Errorf("no user content provided")
		}

		config := &genai.GenerateContentConfig{}
		if system != "" {
			config.SystemInstruction = genai.NewContentFromText(system, genai.RoleUser)
		}

		// Apply thinking config based on model generation
		// Note: Skip thinking config when search or tools are enabled (not supported together)
		if !req.Search && len(req.Tools) == 0 {
			if p.thinkingLevel != "" {
				config.ThinkingConfig = &genai.ThinkingConfig{
					ThinkingLevel: p.thinkingLevel,
				}
			} else if p.thinkingBudget != nil {
				config.ThinkingConfig = &genai.ThinkingConfig{
					ThinkingBudget: p.thinkingBudget,
				}
			}
		}

		if req.Search {
			config.Tools = append(config.Tools, &genai.Tool{GoogleSearch: &genai.GoogleSearch{}})
		}

		if len(req.Tools) > 0 {
			config.Tools = append(config.Tools, buildGeminiTools(req.Tools)...)
			config.ToolConfig = buildGeminiToolConfig(req.ToolChoice)
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

		if len(req.Tools) > 0 {
			resp, err := client.Models.GenerateContent(ctx, chooseModel(req.Model, p.model), contents, config)
			if err != nil {
				return fmt.Errorf("gemini API error: %w", err)
			}
			for _, fc := range resp.FunctionCalls() {
				argsJSON, _ := jsonMarshal(fc.Args)
				events <- Event{Type: EventToolCall, Tool: &ToolCall{ID: "", Name: fc.Name, Arguments: argsJSON}}
			}
			emitGeminiUsage(events, resp)
			events <- Event{Type: EventDone}
			return nil
		}

		var sources []string
		var lastResp *genai.GenerateContentResponse
		for resp, err := range client.Models.GenerateContentStream(ctx, chooseModel(req.Model, p.model), contents, config) {
			if err != nil {
				return fmt.Errorf("gemini streaming error: %w", err)
			}
			lastResp = resp
			if text := resp.Text(); text != "" {
				events <- Event{Type: EventTextDelta, Text: text}
			}
			if req.Search {
				for _, cand := range resp.Candidates {
					if cand.GroundingMetadata != nil && cand.GroundingMetadata.GroundingChunks != nil {
						for _, chunk := range cand.GroundingMetadata.GroundingChunks {
							if chunk.Web != nil && chunk.Web.URI != "" {
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
		}

		if len(sources) > 0 {
			events <- Event{Type: EventTextDelta, Text: "\n\n**Sources:**\n"}
			for _, source := range sources {
				events <- Event{Type: EventTextDelta, Text: "- " + source + "\n"}
			}
		}
		emitGeminiUsage(events, lastResp)
		events <- Event{Type: EventDone}
		return nil
	}), nil
}

func emitGeminiUsage(events chan<- Event, resp *genai.GenerateContentResponse) {
	if resp == nil || resp.UsageMetadata == nil {
		return
	}
	if resp.UsageMetadata.TotalTokenCount > 0 {
		events <- Event{Type: EventUsage, Use: &Usage{
			InputTokens:  int(resp.UsageMetadata.PromptTokenCount),
			OutputTokens: int(resp.UsageMetadata.CandidatesTokenCount),
		}}
	}
}

func buildGeminiTools(specs []ToolSpec) []*genai.Tool {
	if len(specs) == 0 {
		return nil
	}
	tools := make([]*genai.Tool, 0, len(specs))
	for _, spec := range specs {
		// Normalize schema for Gemini's requirements (similar to OpenAI normalization)
		schema := normalizeSchemaForGemini(spec.Schema)
		tools = append(tools, &genai.Tool{
			FunctionDeclarations: []*genai.FunctionDeclaration{
				{
					Name:        spec.Name,
					Description: spec.Description,
					Parameters:  schemaToGenai(schema),
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

func buildGeminiContents(messages []Message) (string, []*genai.Content) {
	var systemParts []string
	contents := make([]*genai.Content, 0, len(messages))

	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			if text := collectTextParts(msg.Parts); text != "" {
				systemParts = append(systemParts, text)
			}
		case RoleUser:
			content := buildGeminiContent(genai.RoleUser, msg.Parts)
			if content != nil {
				contents = append(contents, content)
			}
		case RoleAssistant:
			content := buildGeminiContent(genai.RoleModel, msg.Parts)
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

func buildGeminiContent(role string, parts []Part) *genai.Content {
	content := &genai.Content{Role: role}
	for _, part := range parts {
		switch part.Type {
		case PartText:
			if part.Text != "" {
				content.Parts = append(content.Parts, &genai.Part{Text: part.Text})
			}
		case PartToolCall:
			if part.ToolCall == nil {
				continue
			}
			args := toolArgsToMap(part.ToolCall.Arguments)
			content.Parts = append(content.Parts, &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   part.ToolCall.ID,
					Name: part.ToolCall.Name,
					Args: args,
				},
			})
		}
	}
	if len(content.Parts) == 0 {
		return nil
	}
	return content
}

func buildGeminiToolResultContent(parts []Part) *genai.Content {
	content := &genai.Content{Role: genai.RoleUser}
	for _, part := range parts {
		switch part.Type {
		case PartText:
			if part.Text != "" {
				content.Parts = append(content.Parts, &genai.Part{Text: part.Text})
			}
		case PartToolResult:
			if part.ToolResult == nil {
				continue
			}
			// Check for embedded image data in tool result
			mimeType, base64Data, textContent := parseToolResultImageData(part.ToolResult.Content)

			// Add the function response with text content only
			content.Parts = append(content.Parts, &genai.Part{
				FunctionResponse: &genai.FunctionResponse{
					ID:       part.ToolResult.ID,
					Name:     part.ToolResult.Name,
					Response: map[string]any{"output": textContent},
				},
			})

			// If image data was found, add it as inline data
			if base64Data != "" {
				imageData, err := base64.StdEncoding.DecodeString(base64Data)
				if err == nil {
					content.Parts = append(content.Parts, &genai.Part{
						InlineData: &genai.Blob{
							MIMEType: mimeType,
							Data:     imageData,
						},
					})
				}
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

func buildGeminiToolConfig(choice ToolChoice) *genai.ToolConfig {
	mode := genai.FunctionCallingConfigModeAuto
	var allowed []string

	switch choice.Mode {
	case ToolChoiceNone:
		mode = genai.FunctionCallingConfigModeNone
	case ToolChoiceRequired:
		mode = genai.FunctionCallingConfigModeAny
	case ToolChoiceName:
		if strings.TrimSpace(choice.Name) != "" {
			mode = genai.FunctionCallingConfigModeAny
			allowed = []string{choice.Name}
		}
	case ToolChoiceAuto:
		mode = genai.FunctionCallingConfigModeAuto
	}

	cfg := &genai.ToolConfig{
		FunctionCallingConfig: &genai.FunctionCallingConfig{
			Mode:                 mode,
			AllowedFunctionNames: allowed,
		},
	}

	return cfg
}

func collectGeminiUserPreview(contents []*genai.Content) string {
	var parts []string
	for _, content := range contents {
		if content.Role != genai.RoleUser {
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

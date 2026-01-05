package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"google.golang.org/genai"
)

// GeminiProvider implements Provider using the Google Gemini API.
type GeminiProvider struct {
	apiKey string
	model  string
}

func NewGeminiProvider(apiKey, model string, _ bool) *GeminiProvider {
	return &GeminiProvider{
		apiKey: apiKey,
		model:  model,
	}
}

func (p *GeminiProvider) Name() string {
	return fmt.Sprintf("Gemini (%s)", p.model)
}

func (p *GeminiProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeSearch: true,
		ToolCalls:    true,
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

		system, user := flattenSystemUser(req.Messages)
		if user == "" {
			return fmt.Errorf("no user content provided")
		}

		config := &genai.GenerateContentConfig{}
		if system != "" {
			config.SystemInstruction = genai.NewContentFromText(system, genai.RoleUser)
		}

		if req.Search {
			config.Tools = append(config.Tools, &genai.Tool{GoogleSearch: &genai.GoogleSearch{}})
		}

		if len(req.Tools) > 0 {
			config.Tools = append(config.Tools, buildGeminiTools(req.Tools)...)
			config.ToolConfig = &genai.ToolConfig{
				FunctionCallingConfig: &genai.FunctionCallingConfig{
					Mode: genai.FunctionCallingConfigModeAny,
				},
			}
		}

		if req.Debug {
			fmt.Fprintln(os.Stderr, "=== DEBUG: Gemini Stream Request ===")
			fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
			fmt.Fprintf(os.Stderr, "System: %s\n", truncate(system, 200))
			fmt.Fprintf(os.Stderr, "User: %s\n", truncate(user, 200))
			fmt.Fprintf(os.Stderr, "Tools: %d\n", len(req.Tools))
			fmt.Fprintln(os.Stderr, "====================================")
		}

		if len(req.Tools) > 0 {
			resp, err := client.Models.GenerateContent(ctx, chooseModel(req.Model, p.model), genai.Text(user), config)
			if err != nil {
				return fmt.Errorf("gemini API error: %w", err)
			}
			for _, fc := range resp.FunctionCalls() {
				argsJSON, _ := jsonMarshal(fc.Args)
				events <- Event{Type: EventToolCall, Tool: &ToolCall{ID: "", Name: fc.Name, Arguments: argsJSON}}
			}
			events <- Event{Type: EventDone}
			return nil
		}

		var sources []string
		for resp, err := range client.Models.GenerateContentStream(ctx, chooseModel(req.Model, p.model), genai.Text(user), config) {
			if err != nil {
				return fmt.Errorf("gemini streaming error: %w", err)
			}
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
		events <- Event{Type: EventDone}
		return nil
	}), nil
}

func buildGeminiTools(specs []ToolSpec) []*genai.Tool {
	if len(specs) == 0 {
		return nil
	}
	tools := make([]*genai.Tool, 0, len(specs))
	for _, spec := range specs {
		tools = append(tools, &genai.Tool{
			FunctionDeclarations: []*genai.FunctionDeclaration{
				{
					Name:        spec.Name,
					Description: spec.Description,
					Parameters:  schemaToGenai(spec.Schema),
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

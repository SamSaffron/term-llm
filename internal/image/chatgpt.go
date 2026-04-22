package image

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/llm"
)

const chatGPTImageDefaultModel = "gpt-5.4-mini"

// ChatGPTProvider implements ImageProvider using the chatgpt.com backend's
// built-in image_generation tool, authenticated via the user's existing
// ChatGPT OAuth login (same as the chatgpt LLM provider).
type ChatGPTProvider struct {
	creds  *credentials.ChatGPTCredentials
	client *llm.ResponsesClient
	model  string
}

// NewChatGPTProvider builds a ChatGPT image provider. The user must already
// be logged in via `term-llm ask --provider chatgpt`; this constructor does
// not prompt for interactive auth because the image command runs without a
// full TTY lifecycle.
func NewChatGPTProvider(model string) (*ChatGPTProvider, error) {
	actualModel, _ := llm.ParseModelEffort(strings.TrimSpace(model))
	if actualModel == "" {
		actualModel = chatGPTImageDefaultModel
	}

	creds, err := credentials.GetChatGPTCredentials()
	if err != nil {
		return nil, fmt.Errorf("chatgpt image provider requires ChatGPT login — run 'term-llm ask --provider chatgpt \"hi\"' first: %w", err)
	}
	if creds.IsExpired() {
		if err := credentials.RefreshChatGPTCredentials(creds); err != nil {
			return nil, fmt.Errorf("ChatGPT token refresh failed — re-run 'term-llm ask --provider chatgpt' to re-authenticate: %w", err)
		}
	}

	return &ChatGPTProvider{
		creds:  creds,
		client: llm.NewChatGPTResponsesClient(creds),
		model:  actualModel,
	}, nil
}

func (p *ChatGPTProvider) Name() string             { return "ChatGPT (" + p.model + ")" }
func (p *ChatGPTProvider) SupportsEdit() bool       { return true }
func (p *ChatGPTProvider) SupportsMultiImage() bool { return false }

func (p *ChatGPTProvider) Generate(ctx context.Context, req GenerateRequest) (*ImageResult, error) {
	return p.run(ctx, decorateChatGPTPrompt(req.Prompt, req.Size, req.AspectRatio), nil, req.DebugRaw)
}

func (p *ChatGPTProvider) Edit(ctx context.Context, req EditRequest) (*ImageResult, error) {
	if len(req.InputImages) == 0 {
		return nil, fmt.Errorf("no input image provided")
	}
	if len(req.InputImages) > 1 {
		return nil, fmt.Errorf("ChatGPT image provider only supports single image editing, got %d", len(req.InputImages))
	}
	return p.run(ctx, decorateChatGPTPrompt(req.Prompt, req.Size, req.AspectRatio), &req.InputImages[0], req.DebugRaw)
}

// decorateChatGPTPrompt appends plain-English hints for --size and --aspect-ratio
// to the prompt. The chatgpt.com backend's image_generation tool does not expose
// a dedicated size/ratio parameter we can rely on, so we let the model interpret
// the request via prompt engineering rather than silently dropping the flags.
func decorateChatGPTPrompt(prompt, size, aspectRatio string) string {
	var hints []string
	if s := strings.TrimSpace(size); s != "" {
		if h := chatGPTSizeHint(s); h != "" {
			hints = append(hints, h)
		}
	}
	if ar := strings.TrimSpace(aspectRatio); ar != "" {
		hints = append(hints, fmt.Sprintf("Aspect ratio: %s.", ar))
	}
	if len(hints) == 0 {
		return prompt
	}
	return prompt + "\n\n" + strings.Join(hints, " ")
}

// chatGPTSizeHint maps a normalized size (1K/2K/4K) to a human-readable
// resolution target the model can interpret.
func chatGPTSizeHint(size string) string {
	switch strings.ToUpper(size) {
	case "1K":
		return "Target resolution: approximately 1024×1024 pixels (1K)."
	case "2K":
		return "Target resolution: approximately 2048×2048 pixels (2K)."
	case "4K":
		return "Target resolution: approximately 4096×4096 pixels (4K)."
	default:
		return ""
	}
}

func (p *ChatGPTProvider) run(ctx context.Context, prompt string, src *InputImage, debugRaw bool) (*ImageResult, error) {
	content := []llm.ResponsesContentPart{{Type: "input_text", Text: prompt}}
	instructions := "You are an image generation assistant. Generate exactly one image matching the user's prompt by calling the image_generation tool."
	if src != nil {
		mime := getMimeType(src.Path)
		dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(src.Data)
		content = append(content, llm.ResponsesContentPart{Type: "input_image", ImageURL: dataURL})
		instructions = "You are an image editing assistant. The user provides an input image and instructions to modify it. Call the image_generation tool to produce exactly one edited image."
	}

	responsesReq := llm.ResponsesRequest{
		Model:        p.model,
		Instructions: instructions,
		Input: []llm.ResponsesInputItem{{
			Type:    "message",
			Role:    "user",
			Content: content,
		}},
		Tools:      []any{llm.ResponsesImageGenerationTool{Type: "image_generation", OutputFormat: "png"}},
		ToolChoice: map[string]any{"type": "image_generation"},
		Store:      boolPtr(false),
		Stream:     true,
	}

	stream, err := p.client.Stream(ctx, responsesReq, debugRaw)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	for {
		ev, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return nil, fmt.Errorf("stream ended without image")
			}
			return nil, err
		}
		switch ev.Type {
		case llm.EventImageGenerated:
			return &ImageResult{Data: ev.ImageData, MimeType: ev.ImageMimeType}, nil
		case llm.EventError:
			if ev.Err != nil {
				return nil, fmt.Errorf("chatgpt image generation error: %w", ev.Err)
			}
			return nil, fmt.Errorf("chatgpt image generation error: %s", ev.Text)
		case llm.EventDone:
			return nil, fmt.Errorf("stream completed without image")
		}
	}
}

func boolPtr(b bool) *bool { return &b }

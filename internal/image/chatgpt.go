package image

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/buildinfo"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

const (
	chatGPTImageGenerateEndpoint = "https://chatgpt.com/backend-api/codex/images/generations"
	chatGPTImageEditEndpoint     = "https://chatgpt.com/backend-api/codex/images/edits"
	chatGPTImageHTTPTimeout      = 10 * time.Minute
	chatGPTImageQualityAuto      = "auto"
	chatGPTImageBackgroundAuto   = "auto"
)

var (
	chatGPTImageDefaultModel = config.DefaultImageChatGPTModel
	chatGPTImageHTTPClient   = &http.Client{Timeout: chatGPTImageHTTPTimeout}
)

// ChatGPTProvider implements ImageProvider using the Codex Images API,
// authenticated via the user's existing ChatGPT OAuth login.
type ChatGPTProvider struct {
	creds            *credentials.ChatGPTCredentials
	client           *http.Client
	model            string
	generateEndpoint string
	editEndpoint     string
}

// NewChatGPTProvider builds a ChatGPT image provider. The user must already be
// logged in with ChatGPT OAuth; image generation never starts an interactive
// authentication flow.
func NewChatGPTProvider(model string) (*ChatGPTProvider, error) {
	model = normalizeChatGPTImageModel(model)

	creds, err := credentials.GetChatGPTCredentials()
	if err != nil {
		return nil, fmt.Errorf("chatgpt image provider requires ChatGPT login — run 'term-llm auth login chatgpt' first: %w", err)
	}
	if creds.IsExpired() {
		if err := credentials.RefreshChatGPTCredentials(creds); err != nil {
			return nil, fmt.Errorf("ChatGPT token refresh failed — run 'term-llm auth login chatgpt' to re-authenticate: %w", err)
		}
	}

	return &ChatGPTProvider{
		creds:            creds,
		client:           chatGPTImageHTTPClient,
		model:            model,
		generateEndpoint: chatGPTImageGenerateEndpoint,
		editEndpoint:     chatGPTImageEditEndpoint,
	}, nil
}

func (p *ChatGPTProvider) Name() string             { return "ChatGPT (" + p.model + ")" }
func (p *ChatGPTProvider) SupportsEdit() bool       { return true }
func (p *ChatGPTProvider) SupportsMultiImage() bool { return false }

func (p *ChatGPTProvider) Generate(ctx context.Context, req GenerateRequest) (*ImageResult, error) {
	payload := chatGPTImageGenerationRequest{
		Model:      p.model,
		Prompt:     req.Prompt,
		Background: imageOption(req.Background, chatGPTImageBackgroundAuto),
		Quality:    imageOption(req.Quality, chatGPTImageQualityAuto),
		Size:       chatGPTImageSize(req.Size, req.AspectRatio),
	}
	debug := req.Debug || req.DebugRaw
	debugRawImageLog(debug, "ChatGPT Image Request", "POST %s\nmodel=%s prompt=%q quality=%s size=%s",
		p.generateEndpoint, payload.Model, payload.Prompt, payload.Quality, payload.Size)
	return p.doRequest(ctx, p.generateEndpoint, payload, debug)
}

func (p *ChatGPTProvider) Edit(ctx context.Context, req EditRequest) (*ImageResult, error) {
	if len(req.InputImages) == 0 {
		return nil, fmt.Errorf("no input image provided")
	}
	if len(req.InputImages) > 1 {
		return nil, fmt.Errorf("ChatGPT image provider only supports single image editing, got %d", len(req.InputImages))
	}

	input := req.InputImages[0]
	payload := chatGPTImageEditRequest{
		Images: []chatGPTImageURL{{
			ImageURL: "data:" + getMimeType(input.Path) + ";base64," + base64.StdEncoding.EncodeToString(input.Data),
		}},
		Model:      p.model,
		Prompt:     req.Prompt,
		Background: imageOption(req.Background, chatGPTImageBackgroundAuto),
		Quality:    imageOption(req.Quality, chatGPTImageQualityAuto),
		Size:       chatGPTImageSize(req.Size, req.AspectRatio),
	}
	debug := req.Debug || req.DebugRaw
	debugRawImageLog(debug, "ChatGPT Image Request", "POST %s\nmodel=%s prompt=%q quality=%s size=%s images=1 image_bytes=%d",
		p.editEndpoint, payload.Model, payload.Prompt, payload.Quality, payload.Size, len(input.Data))
	return p.doRequest(ctx, p.editEndpoint, payload, debug)
}

func (p *ChatGPTProvider) doRequest(ctx context.Context, endpoint string, payload any, debug bool) (*ImageResult, error) {
	headers := make(http.Header)
	if p.creds.AccountID != "" {
		headers.Set("ChatGPT-Account-ID", p.creds.AccountID)
	}
	headers.Set("originator", "term-llm")
	headers.Set("User-Agent", buildinfo.UserAgent())
	headers.Set("Accept", "application/json")

	body, _, err := providerhttp.DoJSONRequest(ctx, providerhttp.JSONRequestOptions{
		Client:         p.client,
		Method:         http.MethodPost,
		URL:            endpoint,
		APIKey:         p.creds.AccessToken,
		Payload:        payload,
		Provider:       "ChatGPT image",
		Headers:        headers,
		ExpectedStatus: http.StatusOK,
		OnResponse: func(status int, contentType string, body []byte) {
			debugRawImageLog(debug, "ChatGPT Image Response", "status=%d content-type=%s body_len=%d", status, contentType, len(body))
		},
	})
	if err != nil {
		return nil, err
	}

	var response chatGPTImageResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("failed to parse ChatGPT image response: %w", err)
	}
	if len(response.Data) == 0 || response.Data[0].B64JSON == "" {
		return nil, fmt.Errorf("no image data in ChatGPT image response")
	}
	imageData, err := base64.StdEncoding.DecodeString(response.Data[0].B64JSON)
	if err != nil {
		return nil, fmt.Errorf("failed to decode ChatGPT image: %w", err)
	}
	return &ImageResult{Data: imageData, MimeType: "image/png"}, nil
}

func normalizeChatGPTImageModel(model string) string {
	model = strings.TrimSpace(model)
	switch model {
	case "", "gpt-5.4-mini", "gpt-5.4":
		// These language models selected the old Responses API hop. Preserve old
		// configs while moving them to the dedicated Images API default.
		return chatGPTImageDefaultModel
	default:
		return model
	}
}

func chatGPTImageSize(size, aspectRatio string) string {
	if strings.TrimSpace(size) == "" && strings.TrimSpace(aspectRatio) == "" {
		return "auto"
	}
	return openaiSizeFromRequest("gpt-image-2", size, aspectRatio)
}

type chatGPTImageGenerationRequest struct {
	Model      string `json:"model"`
	Prompt     string `json:"prompt"`
	Background string `json:"background"`
	Quality    string `json:"quality"`
	Size       string `json:"size"`
}

type chatGPTImageEditRequest struct {
	Images     []chatGPTImageURL `json:"images"`
	Model      string            `json:"model"`
	Prompt     string            `json:"prompt"`
	Background string            `json:"background"`
	Quality    string            `json:"quality"`
	Size       string            `json:"size"`
}

type chatGPTImageURL struct {
	ImageURL string `json:"image_url"`
}

type chatGPTImageResponse struct {
	Data []struct {
		B64JSON string `json:"b64_json"`
	} `json:"data"`
}

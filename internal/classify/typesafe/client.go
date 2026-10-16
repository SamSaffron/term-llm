// Package typesafe implements the classify.Backend for TypeSafe System One.
package typesafe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/classify"
	"github.com/samsaffron/term-llm/internal/classify/transport"
)

const (
	DefaultBaseURL = "https://api.typesafe.ai"
	DefaultTimeout = 10 * time.Second

	classifyPath = "/v1/systemone"
	modelsPath   = "/v1/models"
)

// Options configures a TypeSafe API client.
type Options struct {
	APIKey  string
	BaseURL string
	Timeout time.Duration
	Headers map[string]string // extra request headers
	// ImageFormat selects how request images are sent: ImageFormatAssets
	// (the default when empty) or ImageFormatInline.
	ImageFormat string
}

// Client is a native HTTP client for the TypeSafe System One API. It
// implements classify.Backend.
type Client struct {
	http *transport.Client
	// imageHTTP sends requests that carry images; it adds the images opt-in
	// header for ImageFormatAssets.
	imageHTTP   *transport.Client
	imageFormat string
}

// wireRequest is the System One request plus the optional image encodings.
type wireRequest struct {
	State     json.RawMessage              `json:"state"`
	Model     string                       `json:"model"`
	Questions map[string]classify.Question `json:"questions"`
	Assets    map[string]asset             `json:"assets,omitempty"`
	Images    []string                     `json:"images,omitempty"`
}

// APIError reports a non-successful TypeSafe API response.
type APIError = transport.APIError

// NewClient creates a TypeSafe client.
func NewClient(opts Options) (*Client, error) {
	switch opts.ImageFormat {
	case "":
		opts.ImageFormat = ImageFormatAssets
	case ImageFormatAssets, ImageFormatInline:
	default:
		return nil, fmt.Errorf("typesafe: unsupported image format %q; use %q or %q", opts.ImageFormat, ImageFormatAssets, ImageFormatInline)
	}
	cfg := transport.Config{
		Provider:       "typesafe",
		APIKey:         opts.APIKey,
		Headers:        opts.Headers,
		BaseURL:        opts.BaseURL,
		Timeout:        opts.Timeout,
		DefaultBaseURL: DefaultBaseURL,
		DefaultTimeout: DefaultTimeout,
		MissingKeyHint: "set TYPESAFE_API_KEY (used only with the default endpoint) or classify.providers.<name>.api_key",
		ErrorMessage:   errorMessage,
	}
	http, err := transport.New(cfg)
	if err != nil {
		return nil, err
	}
	imageHTTP := http
	if opts.ImageFormat == ImageFormatAssets {
		cfg.Headers = make(map[string]string, len(opts.Headers)+1)
		for k, v := range opts.Headers {
			cfg.Headers[k] = v
		}
		cfg.Headers[imagesHeader] = "true"
		if imageHTTP, err = transport.New(cfg); err != nil {
			return nil, err
		}
	}
	return &Client{http: http, imageHTTP: imageHTTP, imageFormat: opts.ImageFormat}, nil
}

// Classify evaluates a state against typed questions.
func (c *Client) Classify(ctx context.Context, req classify.Request) (*classify.Response, error) {
	if c == nil {
		return nil, errors.New("typesafe: nil client")
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	wire, err := c.wireRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("typesafe: encode request: %w", err)
	}
	client := c.http
	if len(req.Images) > 0 {
		client = c.imageHTTP
	}
	respBody, err := client.Do(ctx, http.MethodPost, classifyPath, body, req.State)
	if err != nil {
		return nil, err
	}
	var resp classify.Response
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("typesafe: decode classify response: %w", err)
	}
	resp.Raw = append(json.RawMessage(nil), respBody...)
	if err := resp.Validate(req); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListModels lists models available to the account.
func (c *Client) ListModels(ctx context.Context) (*classify.ModelsResponse, error) {
	if c == nil {
		return nil, errors.New("typesafe: nil client")
	}
	respBody, err := c.http.Do(ctx, http.MethodGet, modelsPath, nil, nil)
	if err != nil {
		return nil, err
	}
	models, err := decodeModels(respBody)
	if err != nil {
		return nil, err
	}
	models.Raw = append(json.RawMessage(nil), respBody...)
	return models, nil
}

func decodeModels(body []byte) (*classify.ModelsResponse, error) {
	var object classify.ModelsResponse
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, fmt.Errorf("typesafe: decode models response: %w", err)
	}
	if len(object.Models) == 0 {
		return nil, errors.New("typesafe: models response missing models")
	}
	for i, model := range object.Models {
		if strings.TrimSpace(model.Name) == "" {
			return nil, fmt.Errorf("typesafe: model %d missing name", i)
		}
	}
	return &object, nil
}

// errorMessage extracts TypeSafe's message, error, or detail field.
func errorMessage(body []byte) string {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	for _, key := range []string{"message", "error", "detail"} {
		value, ok := obj[key]
		if !ok {
			continue
		}
		if s, ok := value.(string); ok {
			return s
		}
		if encoded, err := json.Marshal(value); err == nil {
			return string(encoded)
		}
	}
	return ""
}

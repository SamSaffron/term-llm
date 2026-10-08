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
}

// Client is a native HTTP client for the TypeSafe System One API. It
// implements classify.Backend.
type Client struct {
	http *transport.Client
}

// APIError reports a non-successful TypeSafe API response.
type APIError = transport.APIError

// NewClient creates a TypeSafe client.
func NewClient(opts Options) (*Client, error) {
	http, err := transport.New(transport.Config{
		Provider:       "typesafe",
		APIKey:         opts.APIKey,
		BaseURL:        opts.BaseURL,
		Timeout:        opts.Timeout,
		DefaultBaseURL: DefaultBaseURL,
		DefaultTimeout: DefaultTimeout,
		MissingKeyHint: "set TYPESAFE_API_KEY (used only with the default endpoint) or classify.providers.<name>.api_key",
		ErrorMessage:   errorMessage,
	})
	if err != nil {
		return nil, err
	}
	return &Client{http: http}, nil
}

// Classify evaluates a state against typed questions.
func (c *Client) Classify(ctx context.Context, req classify.Request) (*classify.Response, error) {
	if c == nil {
		return nil, errors.New("typesafe: nil client")
	}
	if len(req.Images) > 0 {
		return nil, classify.ErrImagesUnsupported
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("typesafe: encode request: %w", err)
	}
	respBody, err := c.http.Do(ctx, http.MethodPost, classifyPath, body, req.State)
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

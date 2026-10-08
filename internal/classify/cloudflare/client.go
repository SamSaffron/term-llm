// Package cloudflare implements the classify.Backend for Clef decision models
// on Cloudflare Workers AI (POST {base}/@cf/cloudflare/{model}). Clef speaks
// the System One request and response shapes natively, extended with inline
// images, and wraps responses in Cloudflare's {result, success, errors}
// envelope.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/classify"
	"github.com/samsaffron/term-llm/internal/classify/transport"
)

const (
	// DefaultBaseURL is the Workers AI REST endpoint; {account_id} is
	// replaced with Options.AccountID.
	DefaultBaseURL = "https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/run"
	DefaultModel   = "clef"
	DefaultTimeout = 10 * time.Second

	accountIDPlaceholder = "{account_id}"
	modelPathPrefix      = "/@cf/cloudflare/"

	// Clef request limits, checked locally so violations fail before a
	// round trip with a clear message.
	maxQuestions     = 64
	minChoiceOptions = 2
	maxChoiceOptions = 255
	maxScoreLevels   = 10
	maxImages        = 4
	maxImageBytes    = 4 << 20
	maxImagesBytes   = 8 << 20
)

var (
	accountIDPattern = regexp.MustCompile(`^[A-Za-z0-9]+$`)
	modelPattern     = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	imageTypes       = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true}
)

// Options configures a Workers AI client.
type Options struct {
	APIKey string
	// BaseURL is the Workers AI run endpoint, or an AI Gateway
	// .../workers-ai endpoint. Empty uses DefaultBaseURL.
	BaseURL string
	// AccountID replaces {account_id} in BaseURL.
	AccountID string
	Timeout   time.Duration
}

// Client calls Clef on Workers AI. It implements classify.Backend.
type Client struct {
	http *transport.Client
}

// APIError reports a non-successful Workers AI response.
type APIError = transport.APIError

// NewClient creates a Workers AI client.
func NewClient(opts Options) (*Client, error) {
	base := strings.TrimSpace(opts.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	if strings.Contains(base, accountIDPlaceholder) {
		account := strings.TrimSpace(opts.AccountID)
		if account == "" {
			return nil, errors.New("cloudflare: account ID is required; set CLOUDFLARE_ACCOUNT_ID or put the account ID in classify.providers.<name>.base_url")
		}
		if !accountIDPattern.MatchString(account) {
			return nil, errors.New("cloudflare: account ID must contain only letters and digits")
		}
		base = strings.ReplaceAll(base, accountIDPlaceholder, account)
	}
	http, err := transport.New(transport.Config{
		Provider:       "cloudflare",
		APIKey:         opts.APIKey,
		BaseURL:        base,
		Timeout:        opts.Timeout,
		DefaultBaseURL: base,
		DefaultTimeout: DefaultTimeout,
		MissingKeyHint: "set CLOUDFLARE_API_TOKEN (used only with the default endpoint) or classify.providers.<name>.api_key, for example $(wrangler auth token)",
		ErrorMessage:   errorMessage,
	})
	if err != nil {
		return nil, err
	}
	return &Client{http: http}, nil
}

// ListModels reports the Clef models. Workers AI has no decision-model
// listing, so this is answered locally.
func (c *Client) ListModels(context.Context) (*classify.ModelsResponse, error) {
	return &classify.ModelsResponse{Models: []classify.Model{
		{Name: "clef", Description: "Clef 27B multimodal decision model (highest precision)", ReleaseDate: "2026-10-01"},
		{Name: "clef-flash", Description: "Clef-flash 9B multimodal decision model (lowest latency)", ReleaseDate: "2026-10-01"},
	}}, nil
}

type wireRequest struct {
	Model     string                       `json:"model"`
	State     json.RawMessage              `json:"state"`
	Questions map[string]classify.Question `json:"questions"`
	Images    []string                     `json:"images,omitempty"`
}

type envelope struct {
	Result  json.RawMessage `json:"result"`
	Success *bool           `json:"success"`
	Errors  []envelopeError `json:"errors"`
}

type envelopeError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Classify evaluates the state and images against typed questions.
func (c *Client) Classify(ctx context.Context, req classify.Request) (*classify.Response, error) {
	if c == nil {
		return nil, errors.New("cloudflare: nil client")
	}
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	model := strings.TrimSpace(req.Model)
	state := req.State
	if len(bytes.TrimSpace(state)) == 0 {
		state = json.RawMessage(`""`) // Image-only request; Clef requires state.
	}
	body, err := json.Marshal(wireRequest{Model: model, State: state, Questions: req.Questions, Images: req.Images})
	if err != nil {
		return nil, fmt.Errorf("cloudflare: encode request: %w", err)
	}
	respBody, err := c.http.Do(ctx, http.MethodPost, modelPathPrefix+model, body, req.State)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, fmt.Errorf("cloudflare: decode response: %w", err)
	}
	if env.Success != nil && !*env.Success {
		return nil, fmt.Errorf("cloudflare: request failed: %s", c.http.Redact(envelopeMessage(env.Errors), req.State))
	}
	result := bytes.TrimSpace(env.Result)
	if len(result) == 0 || classify.IsJSONNull(result) {
		return nil, errors.New("cloudflare: response missing result")
	}
	var resp classify.Response
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("cloudflare: decode result: %w", err)
	}
	resp.Raw = append(json.RawMessage(nil), result...)
	if err := resp.Validate(req); err != nil {
		return nil, err
	}
	return &resp, nil
}

func validateRequest(req classify.Request) error {
	if err := req.Validate(); err != nil {
		return err
	}
	if !modelPattern.MatchString(strings.TrimSpace(req.Model)) {
		return fmt.Errorf("cloudflare: model %q must contain only letters, digits, '.', '_', or '-'", req.Model)
	}
	if len(req.Questions) > maxQuestions {
		return fmt.Errorf("cloudflare: at most %d questions are allowed", maxQuestions)
	}
	for id, q := range req.Questions {
		if err := validateQuestion(id, q); err != nil {
			return err
		}
	}
	return validateImages(req.Images)
}

func validateQuestion(id string, q classify.Question) error {
	switch q.Type {
	case classify.TypeChoice:
		var criteria map[string]json.RawMessage
		_ = json.Unmarshal(q.Criteria, &criteria)
		if len(criteria) < minChoiceOptions || len(criteria) > maxChoiceOptions {
			return fmt.Errorf("cloudflare: question %q needs %d to %d choice options", id, minChoiceOptions, maxChoiceOptions)
		}
	case classify.TypeScore:
		var levels []json.RawMessage
		_ = json.Unmarshal(q.Criteria, &levels)
		if len(levels) > maxScoreLevels {
			return fmt.Errorf("cloudflare: question %q allows at most %d score levels", id, maxScoreLevels)
		}
	}
	return nil
}

func validateImages(images []string) error {
	if len(images) > maxImages {
		return fmt.Errorf("cloudflare: at most %d images are allowed", maxImages)
	}
	total := 0
	for i, image := range images {
		header, payload, ok := strings.Cut(image, ",")
		mime, isBase64 := strings.CutSuffix(strings.TrimPrefix(strings.ToLower(header), "data:"), ";base64")
		if !ok || !isBase64 || !imageTypes[mime] {
			return fmt.Errorf("cloudflare: image %d must be a PNG, JPEG, or WebP base64 data URL", i+1)
		}
		// Decoded size from the padded base64 length, without decoding.
		size := len(payload)*3/4 - (len(payload) - len(strings.TrimRight(payload, "=")))
		if size > maxImageBytes {
			return fmt.Errorf("cloudflare: image %d exceeds %d bytes", i+1, maxImageBytes)
		}
		total += size
	}
	if total > maxImagesBytes {
		return fmt.Errorf("cloudflare: images exceed %d bytes in total", maxImagesBytes)
	}
	return nil
}

// errorMessage extracts messages from Workers AI and AI Gateway error bodies.
func errorMessage(body []byte) string {
	var obj struct {
		Errors  []envelopeError `json:"errors"`
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	if msg := envelopeMessage(obj.Errors); msg != "" {
		return msg
	}
	var gatewayErrors []envelopeError
	if json.Unmarshal(obj.Error, &gatewayErrors) == nil {
		if msg := envelopeMessage(gatewayErrors); msg != "" {
			return msg
		}
	}
	return obj.Message
}

func envelopeMessage(errs []envelopeError) string {
	var parts []string
	for _, e := range errs {
		msg := strings.TrimSpace(e.Message)
		// Workers AI nests the model error as "AiError: AiError: {...}".
		for strings.HasPrefix(msg, "AiError: ") {
			msg = strings.TrimPrefix(msg, "AiError: ")
		}
		if msg == "" {
			continue
		}
		if e.Code != 0 {
			msg = fmt.Sprintf("%s (code %d)", msg, e.Code)
		}
		parts = append(parts, msg)
	}
	return strings.Join(parts, "; ")
}

package bedrock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/awsauth"
	"github.com/samsaffron/term-llm/internal/buildinfo"
	"github.com/samsaffron/term-llm/internal/llm/providers/anthropic"
)

// bedrockAnthropicVersion is the anthropic_version Bedrock requires in the body.
const bedrockAnthropicVersion = "bedrock-2023-05-31"

// bedrockTransport adapts Anthropic Messages requests to Bedrock's
// InvokeModelWithResponseStream API: the model and stream flags move into the
// URL, betas move into the body, requests are SigV4-signed (or use a Bedrock
// API key bearer token), and the binary event stream is unwrapped back into
// Anthropic stream events.
type bedrockTransport struct {
	region      string
	credentials awsauth.CredentialsProvider
	bearerToken string // AWS_BEARER_TOKEN_BEDROCK
	now         func() time.Time
}

func (t *bedrockTransport) StreamMessages(ctx context.Context, tr anthropic.TransportRequest) (anthropic.EventSource, error) {
	extra := http.Header{}
	anthropic.ApplyCallHeaders(extra, nil, tr.ClientHeaders)
	anthropic.ApplyCallHeaders(extra, nil, tr.CallHeaders)
	betas := bedrockBetas(tr.Betas, extra)
	body, err := bedrockMessagesBody(tr.Body, betas)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.invokeURL(tr.Model, tr.Beta), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Anthropic-Version", anthropic.APIVersion)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	anthropic.ApplyCallHeaders(req.Header, nil, extra)
	// Signing runs last so configured headers are covered by SigV4 and can
	// never replace Authorization / X-Amz-* signing headers.
	if err := t.authorize(ctx, req, body); err != nil {
		return nil, err
	}
	resp, err := tr.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && mediaType == "application/vnd.amazon.eventstream" {
		return &bedrockEventSource{body: resp.Body, decoder: awsauth.NewEventStreamDecoder(resp.Body)}, nil
	}
	return anthropic.SSEResponse(resp, "bedrock")
}

func (t *bedrockTransport) invokeURL(model string, beta bool) string {
	host := "bedrock-runtime." + t.region + ".amazonaws.com"
	if strings.HasPrefix(t.region, "cn-") {
		host += ".cn" // AWS China partition
	}
	u := url.URL{
		Scheme:  "https",
		Host:    host,
		Path:    "/model/" + model + "/invoke-with-response-stream",
		RawPath: "/model/" + url.QueryEscape(model) + "/invoke-with-response-stream",
	}
	if beta {
		u.RawQuery = "beta=true"
	}
	return u.String()
}

func (t *bedrockTransport) authorize(ctx context.Context, req *http.Request, body []byte) error {
	if t.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+t.bearerToken)
		return nil
	}
	if t.credentials == nil {
		return errors.New("expected AWS credentials to be set")
	}
	creds, err := t.credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("bedrock credentials: %w", err)
	}
	hash := sha256.Sum256(body)
	now := time.Now
	if t.now != nil {
		now = t.now
	}
	return awsauth.SignV4(req, hex.EncodeToString(hash[:]), creds, "bedrock", t.region, now())
}

// bedrockBetas merges request betas with configured anthropic-beta headers.
// Bedrock only accepts betas in the body (anthropic_beta), so the header is
// removed from extra; values may be comma-separated, as the SDK accepted.
func bedrockBetas(betas []string, extra http.Header) []string {
	out := append([]string(nil), betas...)
	for _, value := range extra.Values("Anthropic-Beta") {
		for _, beta := range strings.Split(value, ",") {
			if beta = strings.TrimSpace(beta); beta != "" && !slices.Contains(out, beta) {
				out = append(out, beta)
			}
		}
	}
	extra.Del("Anthropic-Beta")
	return out
}

// bedrockMessagesBody rewrites a Messages request body for Bedrock.
func bedrockMessagesBody(body []byte, betas []string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("rewrite Bedrock request: %w", err)
	}
	delete(fields, "model")
	delete(fields, "stream")
	fields["anthropic_version"] = json.RawMessage(`"` + bedrockAnthropicVersion + `"`)
	if len(betas) > 0 {
		encoded, err := json.Marshal(betas)
		if err != nil {
			return nil, err
		}
		fields["anthropic_beta"] = encoded
	}
	return json.Marshal(fields)
}

// bedrockEventSource unwraps Bedrock event-stream frames into Anthropic events.
type bedrockEventSource struct {
	body    io.ReadCloser
	decoder *awsauth.EventStreamDecoder
}

func (s *bedrockEventSource) Close() error { return s.body.Close() }

func (s *bedrockEventSource) Next() (string, []byte, error) {
	for {
		msg, err := s.decoder.Next()
		if err != nil {
			return "", nil, err
		}
		name, data, ok, err := translateBedrockMessage(msg)
		if err != nil {
			return "", nil, err
		}
		if ok {
			return name, data, nil
		}
	}
}

// translateBedrockMessage maps one frame to an Anthropic event. ok is false
// for frames that carry no Anthropic event.
func translateBedrockMessage(msg awsauth.EventStreamMessage) (name string, data []byte, ok bool, err error) {
	messageType, found := msg.Header(":message-type")
	if !found {
		return "", nil, false, errors.New(":message-type event header not present")
	}
	switch messageType {
	case "event":
		return translateBedrockEvent(msg)
	case "exception":
		return translateBedrockException(msg)
	case "error":
		code, _ := msg.Header(":error-code")
		message, _ := msg.Header(":error-message")
		data, err := bedrockErrorEvent(code, message)
		return "error", data, err == nil, err
	}
	return "", nil, false, nil
}

func translateBedrockEvent(msg awsauth.EventStreamMessage) (string, []byte, bool, error) {
	eventType, found := msg.Header(":event-type")
	if !found {
		return "", nil, false, errors.New(":event-type event header not present")
	}
	if eventType != "chunk" {
		return "", nil, false, nil
	}
	var chunk struct {
		Bytes string `json:"bytes"`
	}
	if err := json.Unmarshal(msg.Payload, &chunk); err != nil {
		return "", nil, false, fmt.Errorf("decode Bedrock chunk: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(chunk.Bytes)
	if err != nil {
		return "", nil, false, fmt.Errorf("decode Bedrock chunk bytes: %w", err)
	}
	var typed struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(decoded, &typed)
	return typed.Type, decoded, true, nil
}

func translateBedrockException(msg awsauth.EventStreamMessage) (string, []byte, bool, error) {
	exceptionType, found := msg.Header(":exception-type")
	if !found {
		return "", nil, false, errors.New(":exception-type event header not present")
	}
	var info struct {
		Code    string
		Type    string `json:"__type"`
		Message string
	}
	if err := json.Unmarshal(msg.Payload, &info); err != nil && len(bytes.TrimSpace(msg.Payload)) > 0 {
		return "", nil, false, fmt.Errorf("received exception %s: parsing exception payload failed: %w", exceptionType, err)
	}
	code := exceptionType
	if code == "" {
		code = info.Code
	}
	if code == "" {
		code = info.Type
	}
	data, err := bedrockErrorEvent(code, info.Message)
	return "error", data, err == nil, err
}

func bedrockErrorEvent(code, message string) ([]byte, error) {
	if code == "" {
		code = "UnknownError"
	}
	if message == "" {
		message = "UnknownError"
	}
	return json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": code, "message": message}})
}

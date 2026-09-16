package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

const (
	openAIDelegationTool = "delegate_to_controller"
	openAIErrorBodyLimit = 4 << 10
)

type openAIFunctionTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type openAIRealtimeSessionPayload struct {
	Type             string               `json:"type"`
	Model            string               `json:"model"`
	Instructions     string               `json:"instructions,omitempty"`
	OutputModalities []string             `json:"output_modalities"`
	Audio            openAISessionAudio   `json:"audio"`
	Tools            []openAIFunctionTool `json:"tools"`
	ToolChoice       string               `json:"tool_choice"`
}

type openAISessionAudio struct {
	Input  openAIInputAudio  `json:"input"`
	Output openAIOutputAudio `json:"output"`
}

type openAIInputAudio struct {
	Transcription openAITranscription `json:"transcription"`
}

type openAITranscription struct {
	Model string `json:"model"`
}

type openAIOutputAudio struct {
	Voice string `json:"voice"`
}

// openAIRealtimeSessionJSON builds the GA Realtime session supplied alongside
// the SDP offer. This path is retained for explicitly configured Realtime
// models; GPT-Live uses its native client-delegation protocol instead.
func openAIRealtimeSessionJSON(cfg config.LiveConfig, opts SessionOptions) (json.RawMessage, error) {
	if err := ValidateClientTools(cfg, opts.ClientTools); err != nil {
		return nil, err
	}
	if err := cfg.OpenAI.ValidateVoice(); err != nil {
		return nil, err
	}
	model := strings.TrimSpace(cfg.OpenAI.Model)
	if model == "" {
		model = config.DefaultLiveOpenAIModel
	}
	payload := openAIRealtimeSessionPayload{
		Type:             "realtime",
		Model:            model,
		Instructions:     resolvedInstructions(cfg.Instructions, opts),
		OutputModalities: []string{"audio"},
		Audio: openAISessionAudio{
			Input:  openAIInputAudio{Transcription: openAITranscription{Model: "gpt-4o-mini-transcribe"}},
			Output: openAIOutputAudio{Voice: cfg.OpenAI.ResolvedVoice()},
		},
		Tools: []openAIFunctionTool{{
			Type:        "function",
			Name:        openAIDelegationTool,
			Description: "Delegate work to term-llm's execution controller. Call this for requests involving code, files, the shell, tools, project-specific investigation, or changes to work already running. Pass the complete requested work in input.",
			Parameters: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"input": map[string]any{
						"type":        "string",
						"description": "The complete work request for the execution controller.",
					},
				},
				"required": []string{"input"},
			},
		}},
		ToolChoice: "auto",
	}
	for _, tool := range opts.ClientTools {
		payload.Tools = append(payload.Tools, openAIFunctionTool{Type: "function", Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters})
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI Realtime session: %w", err)
	}
	return encoded, nil
}

type openAILiveSessionPayload struct {
	Model        string                  `json:"model"`
	Instructions string                  `json:"instructions,omitempty"`
	Input        []openAILiveInitialItem `json:"input,omitempty"`
	Audio        openAILiveSessionAudio  `json:"audio"`
	Delegation   openAILiveDelegation    `json:"delegation"`
}

type openAILiveSessionAudio struct {
	Output openAIOutputAudio `json:"output"`
}

type openAILiveDelegation struct {
	Type string `json:"type"`
}

type openAILiveInitialItem struct {
	Type    string                     `json:"type"`
	Role    string                     `json:"role"`
	Content []openAILiveInitialContent `json:"content"`
}

type openAILiveInitialContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type openAILiveCreateRequest struct {
	Session   openAILiveSessionPayload `json:"session"`
	Transport openAILiveTransport      `json:"transport"`
}

type openAILiveTransport struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

func openAILiveSessionPayloadFor(cfg config.LiveConfig, opts SessionOptions) (openAILiveSessionPayload, error) {
	if err := cfg.OpenAI.ValidateVoice(); err != nil {
		return openAILiveSessionPayload{}, err
	}
	model := strings.TrimSpace(cfg.OpenAI.Model)
	if model == "" {
		model = config.DefaultLiveOpenAIModel
	}
	payload := openAILiveSessionPayload{
		Model:        model,
		Instructions: resolvedInstructions(cfg.Instructions, opts),
		Audio:        openAILiveSessionAudio{Output: openAIOutputAudio{Voice: cfg.OpenAI.ResolvedVoice()}},
		Delegation:   openAILiveDelegation{Type: "client"},
	}
	for _, initial := range opts.InitialItems {
		text := strings.TrimSpace(initial.Text)
		if text == "" {
			continue
		}
		role := strings.TrimSpace(initial.Role)
		switch role {
		case RoleAssistant, RoleUser, "developer":
		case "system":
			role = "developer"
		default:
			role = RoleUser
		}
		contentType := "input_text"
		if role == RoleAssistant {
			contentType = "output_text"
		}
		payload.Input = append(payload.Input, openAILiveInitialItem{
			Type: "message",
			Role: role,
			Content: []openAILiveInitialContent{{
				Type: contentType,
				Text: text,
			}},
		})
	}
	if len(payload.Input) > 128 {
		return openAILiveSessionPayload{}, fmt.Errorf("live: OpenAI GPT-Live history has %d messages; maximum is 128", len(payload.Input))
	}
	return payload, nil
}

type openAICallResponse struct {
	AnswerSDP string
	CallID    string
}

// createOpenAICall uses the GA Realtime unified WebRTC interface.
func createOpenAICall(ctx context.Context, client *http.Client, baseURL, apiKey, offerSDP string, session json.RawMessage) (openAICallResponse, error) {
	endpoint, err := openAIURL(baseURL, "/realtime/calls")
	if err != nil {
		return openAICallResponse{}, err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writeMultipartField(writer, "sdp", offerSDP); err != nil {
		return openAICallResponse{}, err
	}
	if err := writeMultipartField(writer, "session", string(session)); err != nil {
		return openAICallResponse{}, err
	}
	if err := writer.Close(); err != nil {
		return openAICallResponse{}, fmt.Errorf("encode OpenAI Realtime call: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return openAICallResponse{}, fmt.Errorf("build OpenAI Realtime call request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return openAICallResponse{}, fmt.Errorf("create OpenAI Realtime call: %w", err)
	}
	defer resp.Body.Close()

	if err := classifyOpenAIHTTPError(resp, "create OpenAI Realtime call"); err != nil {
		return openAICallResponse{}, err
	}
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return openAICallResponse{}, fmt.Errorf("read OpenAI Realtime answer: %w", err)
	}
	if strings.TrimSpace(string(answer)) == "" {
		return openAICallResponse{}, errors.New("create OpenAI Realtime call: empty answer sdp")
	}
	callID := ParseCallID(resp.Header.Get("Location"))
	if callID == "" {
		return openAICallResponse{}, errors.New("create OpenAI Realtime call: response is missing a call id")
	}
	return openAICallResponse{AnswerSDP: string(answer), CallID: callID}, nil
}

type openAILiveCallResponse struct {
	Session struct {
		ID string `json:"id"`
	} `json:"session"`
	Transport openAILiveTransport `json:"transport"`
}

func createOpenAILiveCall(ctx context.Context, client *http.Client, baseURL, apiKey, offerSDP string, session openAILiveSessionPayload) (openAICallResponse, error) {
	endpoint, err := openAIURL(baseURL, "/live/sessions")
	if err != nil {
		return openAICallResponse{}, err
	}
	body, err := json.Marshal(openAILiveCreateRequest{
		Session:   session,
		Transport: openAILiveTransport{Type: "webrtc", SDP: offerSDP},
	})
	if err != nil {
		return openAICallResponse{}, fmt.Errorf("encode OpenAI GPT-Live session: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return openAICallResponse{}, fmt.Errorf("build OpenAI GPT-Live session request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return openAICallResponse{}, fmt.Errorf("create OpenAI GPT-Live session: %w", err)
	}
	defer resp.Body.Close()
	if err := classifyOpenAIHTTPError(resp, "create OpenAI GPT-Live session"); err != nil {
		return openAICallResponse{}, err
	}
	var result openAILiveCallResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return openAICallResponse{}, fmt.Errorf("decode OpenAI GPT-Live session response: %w", err)
	}
	result.Session.ID = strings.TrimSpace(result.Session.ID)
	if result.Session.ID == "" {
		return openAICallResponse{}, errors.New("create OpenAI GPT-Live session: response is missing session.id")
	}
	if result.Transport.Type != "webrtc" {
		return openAICallResponse{}, fmt.Errorf("create OpenAI GPT-Live session: unexpected transport type %q", result.Transport.Type)
	}
	if strings.TrimSpace(result.Transport.SDP) == "" {
		return openAICallResponse{}, errors.New("create OpenAI GPT-Live session: empty transport.sdp")
	}
	return openAICallResponse{AnswerSDP: result.Transport.SDP, CallID: result.Session.ID}, nil
}

func classifyOpenAIHTTPError(resp *http.Response, operation string) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %s", ErrUnauthorized, openAIHTTPError(resp))
	case http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrForbidden, openAIHTTPError(resp))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: http %d: %s", operation, resp.StatusCode, openAIHTTPError(resp))
	}
	return nil
}

func writeMultipartField(writer *multipart.Writer, name, value string) error {
	part, err := writer.CreateFormField(name)
	if err != nil {
		return fmt.Errorf("encode OpenAI Realtime %s field: %w", name, err)
	}
	if _, err := io.WriteString(part, value); err != nil {
		return fmt.Errorf("encode OpenAI Realtime %s field: %w", name, err)
	}
	return nil
}

func openAIURL(baseURL, suffix string) (string, error) {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return "", errors.New("live: OpenAI base url is empty")
	}
	parsed, err := url.Parse(strings.TrimRight(trimmed, "/"))
	if err != nil {
		return "", fmt.Errorf("live: invalid OpenAI base url %q: %w", baseURL, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("live: invalid OpenAI base url %q", baseURL)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + suffix
	return parsed.String(), nil
}

func openAIHTTPError(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, openAIErrorBodyLimit))
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return http.StatusText(resp.StatusCode)
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		if message := strings.TrimSpace(envelope.Error.Message); message != "" {
			return message
		}
		if code := strings.TrimSpace(envelope.Error.Code); code != "" {
			return code
		}
	}
	return trimmed
}

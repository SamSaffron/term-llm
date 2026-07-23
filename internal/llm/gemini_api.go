package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/providerhttp"
)

const (
	geminiAPIBaseURL = "https://generativelanguage.googleapis.com/v1beta/models"
	geminiRoleUser   = "user"
	geminiRoleModel  = "model"

	geminiThinkingLevelMinimal geminiThinkingLevel = "MINIMAL"
	geminiThinkingLevelLow     geminiThinkingLevel = "LOW"
	geminiThinkingLevelHigh    geminiThinkingLevel = "HIGH"

	geminiFunctionCallingConfigModeAuto geminiFunctionCallingMode = "AUTO"
	geminiFunctionCallingConfigModeNone geminiFunctionCallingMode = "NONE"
	geminiFunctionCallingConfigModeAny  geminiFunctionCallingMode = "ANY"
)

type geminiThinkingLevel string

type geminiGenerateContentRequest struct {
	Contents          []*geminiContent        `json:"contents"`
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
	Tools             []*geminiTool           `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig       `json:"toolConfig,omitempty"`
}

type geminiGenerationConfig struct {
	ThinkingConfig  *geminiThinkingAPIConfig `json:"thinkingConfig,omitempty"`
	Temperature     *float32                 `json:"temperature,omitempty"`
	TopP            *float32                 `json:"topP,omitempty"`
	MaxOutputTokens int                      `json:"maxOutputTokens,omitempty"`
}

type geminiThinkingAPIConfig struct {
	ThinkingLevel  geminiThinkingLevel `json:"thinkingLevel,omitempty"`
	ThinkingBudget *int32              `json:"thinkingBudget,omitempty"`
}

type geminiContent struct {
	Role  string        `json:"role,omitempty"`
	Parts []*geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	InlineData       *geminiBlob             `json:"inlineData,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	Thought          bool                    `json:"thought,omitempty"`
	ThoughtSignature []byte                  `json:"thoughtSignature,omitempty"`
}

type geminiBlob struct {
	MIMEType string `json:"mimeType,omitempty"`
	Data     []byte `json:"data,omitempty"`
}

type geminiFunctionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiTool struct {
	FunctionDeclarations []*geminiFunctionDeclaration `json:"functionDeclarations,omitempty"`
	GoogleSearch         *geminiGoogleSearch          `json:"googleSearch,omitempty"`
}

type geminiGoogleSearch struct{}

type geminiFunctionDeclaration struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Parameters  *geminiSchema `json:"parameters,omitempty"`
}

type geminiToolConfig struct {
	FunctionCallingConfig *geminiFunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

type geminiFunctionCallingMode string

type geminiFunctionCallingConfig struct {
	Mode                 geminiFunctionCallingMode `json:"mode"`
	AllowedFunctionNames []string                  `json:"allowedFunctionNames,omitempty"`
}

type geminiSchemaType string

const (
	geminiSchemaTypeString  geminiSchemaType = "STRING"
	geminiSchemaTypeInteger geminiSchemaType = "INTEGER"
	geminiSchemaTypeNumber  geminiSchemaType = "NUMBER"
	geminiSchemaTypeBoolean geminiSchemaType = "BOOLEAN"
	geminiSchemaTypeArray   geminiSchemaType = "ARRAY"
	geminiSchemaTypeObject  geminiSchemaType = "OBJECT"
)

type geminiSchema struct {
	Type        geminiSchemaType         `json:"type"`
	Description string                   `json:"description,omitempty"`
	Properties  map[string]*geminiSchema `json:"properties,omitempty"`
	Items       *geminiSchema            `json:"items,omitempty"`
	Required    []string                 `json:"required,omitempty"`
}

type geminiGenerateContentResponse struct {
	Candidates     []*geminiCandidate    `json:"candidates"`
	UsageMetadata  *geminiUsageMetadata  `json:"usageMetadata,omitempty"`
	PromptFeedback *geminiPromptFeedback `json:"promptFeedback,omitempty"`
	Error          *geminiAPIError       `json:"error,omitempty"`
}

type geminiAPIError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Status  string `json:"status,omitempty"`
}

type geminiPromptFeedback struct {
	BlockReason        string `json:"blockReason,omitempty"`
	BlockReasonMessage string `json:"blockReasonMessage,omitempty"`
}

type geminiCandidate struct {
	Content           *geminiContent           `json:"content,omitempty"`
	GroundingMetadata *geminiGroundingMetadata `json:"groundingMetadata,omitempty"`
	FinishReason      string                   `json:"finishReason,omitempty"`
	FinishMessage     string                   `json:"finishMessage,omitempty"`
}

type geminiGroundingMetadata struct {
	GroundingChunks []*geminiGroundingChunk `json:"groundingChunks"`
}

type geminiGroundingChunk struct {
	Web *geminiGroundingWeb `json:"web,omitempty"`
}

type geminiGroundingWeb struct {
	URI   string `json:"uri,omitempty"`
	Title string `json:"title,omitempty"`
}

type geminiUsageMetadata struct {
	PromptTokenCount     int32 `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount int32 `json:"candidatesTokenCount,omitempty"`
	ThoughtsTokenCount   int32 `json:"thoughtsTokenCount,omitempty"`
	TotalTokenCount      int32 `json:"totalTokenCount,omitempty"`
}

func streamGeminiResponses(ctx context.Context, client *http.Client, baseURL, apiKey, model string, request geminiGenerateContentRequest, handle func(*geminiGenerateContentResponse) error) error {
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	model = strings.TrimPrefix(model, "models/")
	endpoint := fmt.Sprintf("%s/%s:streamGenerateContent?alt=sse", strings.TrimRight(baseURL, "/"), model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("x-goog-api-key", apiKey)

	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return providerhttp.NewStatusErrorFromResponse("Gemini", resp)
	}
	defer resp.Body.Close()

	return readGeminiSSE(resp.Body, handle)
}

func readGeminiSSE(r io.Reader, handle func(*geminiGenerateContentResponse) error) error {
	decoder := newSSEDecoder(r, sseDecoderOptions{Transport: "Gemini SSE"})
	sawTerminal := false
	for {
		_, data, err := decoder.Next()
		if err == io.EOF {
			if !sawTerminal {
				return &StreamIncompleteError{Transport: "Gemini SSE", Terminal: "candidate finishReason"}
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("read stream response: %w", err)
		}

		var response geminiGenerateContentResponse
		if err := json.Unmarshal(data, &response); err != nil {
			return fmt.Errorf("decode stream response: %w", err)
		}
		if err := geminiResponseError(&response, false); err != nil {
			return err
		}
		if err := handle(&response); err != nil {
			return err
		}
		if err := geminiResponseError(&response, true); err != nil {
			return err
		}
		if len(response.Candidates) > 0 && response.Candidates[0] != nil {
			switch response.Candidates[0].FinishReason {
			case "STOP", "MAX_TOKENS":
				sawTerminal = true
			}
		}
	}
}

func geminiResponseError(response *geminiGenerateContentResponse, checkFinishReason bool) error {
	if response == nil {
		return nil
	}
	if response.Error != nil {
		detail := geminiFirstNonEmpty(response.Error.Message, response.Error.Status)
		if detail == "" {
			detail = fmt.Sprintf("code %d", response.Error.Code)
		}
		if response.Error.Code >= 100 && response.Error.Code <= 599 {
			return providerhttp.NewStatusErrorString("Gemini", response.Error.Code, response.Error.Status, nil, detail)
		}
		return fmt.Errorf("Gemini API error: %s", detail)
	}
	if feedback := response.PromptFeedback; feedback != nil && feedback.BlockReason != "" && feedback.BlockReason != "BLOCK_REASON_UNSPECIFIED" {
		return fmt.Errorf("Gemini prompt blocked: %s", geminiReasonDetail(feedback.BlockReason, feedback.BlockReasonMessage))
	}
	if !checkFinishReason {
		return nil
	}
	if len(response.Candidates) == 0 || response.Candidates[0] == nil {
		return nil
	}
	candidate := response.Candidates[0]
	switch candidate.FinishReason {
	case "", "STOP", "MAX_TOKENS", "FINISH_REASON_UNSPECIFIED":
		return nil
	default:
		return fmt.Errorf("Gemini response stopped: %s", geminiReasonDetail(candidate.FinishReason, candidate.FinishMessage))
	}
}

func geminiReasonDetail(reason, message string) string {
	if message == "" {
		return reason
	}
	return reason + ": " + message
}

func geminiFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

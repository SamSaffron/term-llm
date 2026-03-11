package cmd

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type sessionInterruptRequest struct {
	Message string `json:"message"`
}

func writeChatStreamChunk(w io.Writer, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

func writeSSEEvent(w io.Writer, event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

type responsesCreateRequest struct {
	Model              string            `json:"model"`
	Input              json.RawMessage   `json:"input"`
	Tools              []json.RawMessage `json:"tools,omitempty"`
	ToolChoice         json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool             `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens    int               `json:"max_output_tokens,omitempty"`
	Temperature        *float32          `json:"temperature,omitempty"`
	TopP               *float32          `json:"top_p,omitempty"`
	Stream             bool              `json:"stream,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
}

type chatCompletionsRequest struct {
	Model             string             `json:"model"`
	Messages          []chatMessage      `json:"messages"`
	Tools             []chatTool         `json:"tools,omitempty"`
	ToolChoice        json.RawMessage    `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool              `json:"parallel_tool_calls,omitempty"`
	Temperature       *float32           `json:"temperature,omitempty"`
	TopP              *float32           `json:"top_p,omitempty"`
	MaxTokens         int                `json:"max_tokens,omitempty"`
	Stream            bool               `json:"stream,omitempty"`
	StreamOptions     *chatStreamOptions `json:"stream_options,omitempty"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type chatTool struct {
	Type     string           `json:"type"`
	Name     string           `json:"name,omitempty"`
	Function *chatToolFuncDef `json:"function,omitempty"`
}

type chatToolFuncDef struct {
	Name string `json:"name"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func parseChatMessages(msgs []chatMessage) ([]llm.Message, bool, error) {
	callNameByID := make(map[string]string)
	result := make([]llm.Message, 0, len(msgs))
	replaceHistory := len(msgs) > 1

	for _, msg := range msgs {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "system", "developer":
			result = append(result, llm.SystemText(extractMessageText(msg.Content)))
			replaceHistory = true
		case "user":
			result = append(result, llm.UserText(extractMessageText(msg.Content)))
		case "assistant":
			parts := []llm.Part{}
			text := extractMessageText(msg.Content)
			if text != "" {
				parts = append(parts, llm.Part{Type: llm.PartText, Text: text})
			}
			for _, tc := range msg.ToolCalls {
				args := tc.Function.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				callNameByID[tc.ID] = tc.Function.Name
				parts = append(parts, llm.Part{
					Type: llm.PartToolCall,
					ToolCall: &llm.ToolCall{
						ID:        tc.ID,
						Name:      tc.Function.Name,
						Arguments: json.RawMessage(args),
					},
				})
			}
			if len(parts) == 0 {
				continue
			}
			result = append(result, llm.Message{Role: llm.RoleAssistant, Parts: parts})
			replaceHistory = true
		case "tool":
			callID := strings.TrimSpace(msg.ToolCallID)
			if callID == "" {
				return nil, false, fmt.Errorf("tool message missing tool_call_id")
			}
			name := callNameByID[callID]
			result = append(result, llm.ToolResultMessage(callID, name, extractMessageText(msg.Content), nil))
			replaceHistory = true
		default:
			return nil, false, fmt.Errorf("unsupported message role: %s", msg.Role)
		}
	}

	return result, replaceHistory, nil
}

func parseResponsesInput(input json.RawMessage) ([]llm.Message, bool, error) {
	trimmed := strings.TrimSpace(string(input))
	if trimmed == "" || trimmed == "null" {
		return nil, false, fmt.Errorf("input is required")
	}

	// string shorthand
	var inputText string
	if err := json.Unmarshal(input, &inputText); err == nil {
		if strings.TrimSpace(inputText) == "" {
			return nil, false, fmt.Errorf("input is empty")
		}
		return []llm.Message{llm.UserText(inputText)}, false, nil
	}

	var items []map[string]json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, false, fmt.Errorf("invalid input format")
	}

	var messages []llm.Message
	callNameByID := map[string]string{}
	replaceHistory := false
	userCount := 0

	for _, item := range items {
		itemType := jsonString(item["type"])
		switch itemType {
		case "message":
			role := strings.ToLower(strings.TrimSpace(jsonString(item["role"])))
			switch role {
			case "developer", "system":
				messages = append(messages, llm.SystemText(extractItemContent(item["content"])))
				replaceHistory = true
			case "assistant":
				messages = append(messages, llm.AssistantText(extractItemContent(item["content"])))
				replaceHistory = true
			default:
				msg, err := parseUserMessageContent(item["content"])
				if err != nil {
					return nil, false, fmt.Errorf("user message: %w", err)
				}
				messages = append(messages, msg)
				userCount++
			}
		case "function_call":
			id := jsonString(item["call_id"])
			name := jsonString(item["name"])
			args := jsonString(item["arguments"])
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			callNameByID[id] = name
			messages = append(messages, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{
				Type:     llm.PartToolCall,
				ToolCall: &llm.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)},
			}}})
			replaceHistory = true
		case "function_call_output":
			id := jsonString(item["call_id"])
			out := jsonString(item["output"])
			messages = append(messages, llm.ToolResultMessage(id, callNameByID[id], out, nil))
			replaceHistory = true
		}
	}

	if userCount > 1 {
		replaceHistory = true
	}
	return messages, replaceHistory, nil
}

func parseRequestedTools(raw []json.RawMessage) (bool, map[string]bool) {
	search := false
	toolNames := map[string]bool{}

	for _, item := range raw {
		var generic map[string]json.RawMessage
		if err := json.Unmarshal(item, &generic); err != nil {
			continue
		}
		typeName := strings.ToLower(strings.TrimSpace(jsonString(generic["type"])))
		switch typeName {
		case "web_search_preview", "web_search":
			search = true
		case "function":
			name := strings.TrimSpace(jsonString(generic["name"]))
			if name == "" {
				var fn chatToolFuncDef
				if rawFunc := generic["function"]; len(rawFunc) > 0 {
					_ = json.Unmarshal(rawFunc, &fn)
					name = strings.TrimSpace(fn.Name)
				}
			}
			if name != "" {
				toolNames[name] = true
			}
		}
	}

	return search, toolNames
}

func parseChatRequestedToolNames(tools []chatTool) map[string]bool {
	selected := map[string]bool{}
	for _, t := range tools {
		if strings.ToLower(t.Type) != "function" {
			continue
		}
		name := strings.TrimSpace(t.Name)
		if name == "" && t.Function != nil {
			name = strings.TrimSpace(t.Function.Name)
		}
		if name != "" {
			selected[name] = true
		}
	}
	return selected
}

func parseToolChoice(raw json.RawMessage) llm.ToolChoice {
	if len(raw) == 0 {
		return llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	}
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "none":
			return llm.ToolChoice{Mode: llm.ToolChoiceNone}
		case "required":
			return llm.ToolChoice{Mode: llm.ToolChoiceRequired}
		default:
			return llm.ToolChoice{Mode: llm.ToolChoiceAuto}
		}
	}

	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if strings.ToLower(strings.TrimSpace(obj.Type)) == "function" {
			name := strings.TrimSpace(obj.Name)
			if name == "" {
				name = strings.TrimSpace(obj.Function.Name)
			}
			if name != "" {
				return llm.ToolChoice{Mode: llm.ToolChoiceName, Name: name}
			}
		}
	}
	return llm.ToolChoice{Mode: llm.ToolChoiceAuto}
}

func extractMessageText(content json.RawMessage) string {
	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(content, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			pType := strings.ToLower(strings.TrimSpace(jsonString(p["type"])))
			switch pType {
			case "text", "input_text", "output_text":
				b.WriteString(jsonString(p["text"]))
			}
		}
		return b.String()
	}
	return ""
}

func extractItemContent(content json.RawMessage) string {
	return extractMessageText(content)
}

// parseDataURL splits a data URL into its media type and base64 payload.
// Format: data:image/png;base64,iVBORw0KGgo...
func parseDataURL(dataURL string) (mediaType, base64Data string) {
	if !strings.HasPrefix(dataURL, "data:") {
		return "", ""
	}
	rest := dataURL[5:]
	idx := strings.Index(rest, ";base64,")
	if idx < 0 {
		return "", ""
	}
	return rest[:idx], rest[idx+8:]
}

// isLLMImageType returns true for image media types that LLM providers handle natively.
func isLLMImageType(mediaType string) bool {
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

const (
	maxAttachments     = 10
	maxAttachmentBytes = 20 << 20 // 20 MB per file (decoded)
)

// saveUploadedFile decodes base64 data and writes it to the uploads directory,
// returning the full filesystem path. Uses O_CREATE|O_EXCL for atomic uniqueness.
func saveUploadedFile(filename, b64Data string) (string, error) {
	dataDir, err := session.GetDataDir()
	if err != nil {
		return "", fmt.Errorf("get data dir: %w", err)
	}
	uploadsDir := filepath.Join(dataDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0o700); err != nil {
		return "", fmt.Errorf("create uploads dir: %w", err)
	}

	raw, err := base64.StdEncoding.DecodeString(b64Data)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	if len(raw) > maxAttachmentBytes {
		return "", fmt.Errorf("file %q exceeds %d MB limit", filename, maxAttachmentBytes>>20)
	}

	safeName := filepath.Base(filename)
	if safeName == "." || safeName == "/" {
		safeName = "upload"
	}
	ext := filepath.Ext(safeName)
	prefix := strings.TrimSuffix(safeName, ext) + "_"

	f, err := os.CreateTemp(uploadsDir, prefix+"*"+ext)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	dest := f.Name()

	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(dest)
		return "", fmt.Errorf("chmod: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(dest)
		return "", fmt.Errorf("write file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(dest)
		return "", fmt.Errorf("close file: %w", err)
	}
	return dest, nil
}

// abbreviatePath replaces the user's home directory prefix with ~ for privacy.
func abbreviatePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

// parseUserMessageContent builds a user llm.Message from a content field
// that may be a plain string or an array of content parts (input_text, input_image, input_file).
// Supported image types are sent inline to the LLM; all other files are saved to disk
// and referenced by path in a text part.
//
// All uploaded images are saved to disk unconditionally as originals before any
// processing. Images exceeding 1 MB are resized/compressed before being sent to
// the LLM to avoid provider errors.
func parseUserMessageContent(content json.RawMessage) (llm.Message, error) {
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(content, &parts); err == nil && len(parts) > 0 {
		var llmParts []llm.Part
		fileCount := 0
		for _, part := range parts {
			partType := jsonString(part["type"])
			switch partType {
			case "input_text":
				text := jsonString(part["text"])
				if text != "" {
					llmParts = append(llmParts, llm.Part{Type: llm.PartText, Text: text})
				}
			case "input_image":
				imageURL := jsonString(part["image_url"])
				filename := jsonString(part["filename"])
				if !strings.HasPrefix(imageURL, "data:") {
					continue
				}
				mt, b64 := parseDataURL(imageURL)
				if mt == "" || b64 == "" {
					continue
				}
				if isLLMImageType(mt) {
					// Always save the original to disk first.
					if filename == "" {
						filename = "image"
					}
					savedPath := ""
					if path, err := saveUploadedFile(filename, b64); err != nil {
						log.Printf("[web] warning: could not save uploaded image %q: %v", filename, err)
					} else {
						savedPath = path
					}

					// Decode raw bytes for possible resize.
					raw, err := base64.StdEncoding.DecodeString(b64)
					if err != nil {
						// Shouldn't happen — b64 was just parsed — but fall back gracefully.
						log.Printf("[web] warning: base64 re-decode failed for %q: %v", filename, err)
						llmParts = append(llmParts, llm.Part{
							Type:      llm.PartImage,
							ImageData: &llm.ToolImageData{MediaType: mt, Base64: b64},
							ImagePath: savedPath,
						})
						continue
					}

					// Resize if over 1 MB before sending to the model.
					resized, resMT := resizeImageForLLM(raw, mt)
					sendB64 := b64
					if len(resized) != len(raw) || resMT != mt {
						sendB64 = base64.StdEncoding.EncodeToString(resized)
					}
					llmParts = append(llmParts, llm.Part{
						Type:      llm.PartImage,
						ImageData: &llm.ToolImageData{MediaType: resMT, Base64: sendB64},
						ImagePath: savedPath,
					})
				} else {
					fileCount++
					if fileCount > maxAttachments {
						return llm.Message{}, fmt.Errorf("too many attachments (max %d)", maxAttachments)
					}
					if filename == "" {
						filename = "image"
					}
					path, err := saveUploadedFile(filename, b64)
					if err != nil {
						return llm.Message{}, fmt.Errorf("save attachment %q: %w", filename, err)
					}
					llmParts = append(llmParts, llm.Part{
						Type: llm.PartText,
						Text: fmt.Sprintf("[User uploaded file: %s — saved to %s]", filename, abbreviatePath(path)),
					})
				}
			case "input_file":
				fileData := jsonString(part["file_data"])
				filename := jsonString(part["filename"])
				if filename == "" {
					filename = "upload"
				}
				if !strings.HasPrefix(fileData, "data:") {
					continue
				}
				_, b64 := parseDataURL(fileData)
				if b64 == "" {
					continue
				}
				fileCount++
				if fileCount > maxAttachments {
					return llm.Message{}, fmt.Errorf("too many attachments (max %d)", maxAttachments)
				}
				path, err := saveUploadedFile(filename, b64)
				if err != nil {
					return llm.Message{}, fmt.Errorf("save attachment %q: %w", filename, err)
				}
				llmParts = append(llmParts, llm.Part{
					Type: llm.PartText,
					Text: fmt.Sprintf("[User uploaded file: %s — saved to %s]", filename, abbreviatePath(path)),
				})
			}
		}
		if len(llmParts) > 0 {
			return llm.Message{Role: llm.RoleUser, Parts: llmParts}, nil
		}
	}
	return llm.UserText(extractItemContent(content)), nil
}

func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

func writeOpenAIError(w http.ResponseWriter, status int, errorType, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func decodeJSONBody(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 50<<20))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request body must contain a single JSON object")
	}
	return nil
}

func resolveRequestSessionID(r *http.Request) string {
	sessionID := strings.TrimSpace(r.Header.Get("session_id"))
	if sessionID != "" {
		return sessionID
	}
	return ""
}

func ensureSessionID(w http.ResponseWriter) string {
	sessionID := session.NewID()
	w.Header().Set("x-session-id", sessionID)
	return sessionID
}

func requireJSONContentType(r *http.Request) error {
	contentType := r.Header.Get("Content-Type")
	if strings.TrimSpace(contentType) == "" {
		return fmt.Errorf("Content-Type must be application/json")
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return fmt.Errorf("invalid Content-Type header")
	}
	if mediaType != "application/json" {
		return fmt.Errorf("Content-Type must be application/json")
	}
	return nil
}

func responsesFinalResponse(result serveRunResult, model string, respID string) map[string]any {
	output := []map[string]any{}
	if result.Text.Len() > 0 {
		output = append(output, map[string]any{
			"id":   "msg_" + randomSuffix(),
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{{
				"type": "output_text",
				"text": result.Text.String(),
			}},
		})
	}
	for _, call := range result.ToolCalls {
		output = append(output, map[string]any{
			"id":        "fc_" + call.ID,
			"type":      "function_call",
			"call_id":   call.ID,
			"name":      call.Name,
			"arguments": string(call.Arguments),
		})
	}

	return map[string]any{
		"id":      respID,
		"object":  "response",
		"created": time.Now().Unix(),
		"model":   model,
		"output":  output,
		"usage": map[string]any{
			"input_tokens":  result.Usage.InputTokens,
			"output_tokens": result.Usage.OutputTokens,
			"total_tokens":  result.Usage.InputTokens + result.Usage.CachedInputTokens + result.Usage.CacheWriteTokens + result.Usage.OutputTokens,
			"input_tokens_details": map[string]any{
				"cached_tokens":      result.Usage.CachedInputTokens,
				"cache_write_tokens": result.Usage.CacheWriteTokens,
			},
		},
		"session_usage": map[string]any{
			"input_tokens":  result.SessionUsage.InputTokens,
			"output_tokens": result.SessionUsage.OutputTokens,
			"total_tokens":  result.SessionUsage.InputTokens + result.SessionUsage.CachedInputTokens + result.SessionUsage.CacheWriteTokens + result.SessionUsage.OutputTokens,
			"input_tokens_details": map[string]any{
				"cached_tokens":      result.SessionUsage.CachedInputTokens,
				"cache_write_tokens": result.SessionUsage.CacheWriteTokens,
			},
		},
	}
}

func chatCompletionFinalResponse(result serveRunResult, model string) map[string]any {
	message := map[string]any{
		"role":    "assistant",
		"content": result.Text.String(),
	}
	finishReason := "stop"
	if len(result.ToolCalls) > 0 {
		finishReason = "tool_calls"
		toolCalls := make([]map[string]any, 0, len(result.ToolCalls))
		for _, call := range result.ToolCalls {
			toolCalls = append(toolCalls, map[string]any{
				"id":   call.ID,
				"type": "function",
				"function": map[string]any{
					"name":      call.Name,
					"arguments": string(call.Arguments),
				},
			})
		}
		message["tool_calls"] = toolCalls
	}

	return map[string]any{
		"id":      "chatcmpl_" + randomSuffix(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": map[string]any{
			"prompt_tokens":     result.Usage.InputTokens,
			"completion_tokens": result.Usage.OutputTokens,
			"total_tokens":      result.Usage.InputTokens + result.Usage.CachedInputTokens + result.Usage.CacheWriteTokens + result.Usage.OutputTokens,
			"prompt_tokens_details": map[string]any{
				"cached_tokens":      result.Usage.CachedInputTokens,
				"cache_write_tokens": result.Usage.CacheWriteTokens,
			},
		},
	}
}

func sessionOrRandomID(sessionID string) string {
	if sessionID != "" {
		return sanitizeID(sessionID)
	}
	return randomSuffix()
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return randomSuffix()
	}
	return b.String()
}

func randomSuffix() string {
	buf := make([]byte, 9)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

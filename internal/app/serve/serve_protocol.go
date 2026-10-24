package serve

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/app/serve/wire"
	"github.com/samsaffron/term-llm/internal/app/uploads"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

type sessionInterruptRequest struct {
	Message            string          `json:"message"`
	Content            json.RawMessage `json:"content"`
	SteeringID         string          `json:"steering_id"`
	ClientMessageID    string          `json:"client_message_id,omitempty"`
	Delivery           string          `json:"delivery,omitempty"`
	ExpectedResponseID string          `json:"expected_response_id,omitempty"`
	ExpectedRunEpoch   int64           `json:"expected_run_epoch,omitempty"`
}

type sessionRuntimeEffortRequest struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type sessionRuntimeGoalRequest struct {
	Action      string `json:"action"`
	Objective   string `json:"objective"`
	TokenBudget *int   `json:"token_budget"`
}

func writeChatStreamChunk(w io.Writer, payload any) error {
	return wire.WriteChatStreamChunk(w, payload)
}

func setSSEHeaders(w http.ResponseWriter) { wire.SetSSEHeaders(w) }

func writeSSEEvent(w io.Writer, event string, payload any) error {
	return wire.WriteSSEEvent(w, event, payload)
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

const (
	diffCommentMaxPathBytes             = 1024
	diffCommentMaxLineBytes             = 8 * 1024
	diffCommentMaxInstructionBytes      = 8 * 1024
	diffCommentMaxPayloadBytes          = 32 * 1024
	diffCommentMaxPartsPerMessage       = 25
	diffCommentMaxAggregatePayloadBytes = 256 * 1024
)

func parseDiffCommentPart(raw json.RawMessage) (*core.DiffComment, error) {
	var comment core.DiffComment
	if err := json.Unmarshal(raw, &comment); err != nil {
		return nil, fmt.Errorf("invalid diff_comment metadata: %w", err)
	}
	comment.ID = strings.TrimSpace(comment.ID)
	comment.ParentID = strings.TrimSpace(comment.ParentID)
	comment.Path = strings.TrimSpace(comment.Path)
	var scopeOK bool
	comment.Scope, scopeOK = normalizeFileChangeScope(comment.Scope)
	comment.Side = strings.ToLower(strings.TrimSpace(comment.Side))
	comment.Instruction = strings.TrimSpace(comment.Instruction)
	if comment.ID == "" || len(comment.ID) > 200 {
		return nil, fmt.Errorf("diff_comment.id is required and must be at most 200 characters")
	}
	if len(comment.ParentID) > 200 {
		return nil, fmt.Errorf("diff_comment.parent_id must be at most 200 characters")
	}
	if comment.Path == "" || len(comment.Path) > diffCommentMaxPathBytes {
		return nil, fmt.Errorf("diff_comment.path is required and must be at most %d bytes", diffCommentMaxPathBytes)
	}
	if !scopeOK {
		return nil, fmt.Errorf("diff_comment.scope must be one of %s", fileChangeScopeNames())
	}
	if comment.Side != "old" && comment.Side != "new" {
		return nil, fmt.Errorf("diff_comment.side must be old or new")
	}
	if comment.Line <= 0 {
		return nil, fmt.Errorf("diff_comment.line must be positive")
	}
	_, turnScope := fileChangeScopeRunWindow(comment.Scope)
	if turnScope && comment.FileChangeSeq <= 0 {
		return nil, fmt.Errorf("diff_comment.file_change_seq must be positive for %s", comment.Scope)
	}
	if !turnScope && comment.FileChangeSeq != 0 {
		return nil, fmt.Errorf("diff_comment.file_change_seq must be zero for Git diff scopes")
	}
	if len(comment.LineText) > diffCommentMaxLineBytes {
		return nil, fmt.Errorf("diff_comment.line_text must be at most %d bytes", diffCommentMaxLineBytes)
	}
	if comment.Instruction == "" || len(comment.Instruction) > diffCommentMaxInstructionBytes {
		return nil, fmt.Errorf("diff_comment.instruction is required and must be at most %d bytes", diffCommentMaxInstructionBytes)
	}
	if len(comment.ContextBefore) > 4 || len(comment.ContextAfter) > 4 {
		return nil, fmt.Errorf("diff_comment context is limited to four lines on each side")
	}
	totalBytes := len(comment.ID) + len(comment.ParentID) + len(comment.Path) + len(comment.Scope) + len(comment.Side) + len(comment.LineText) + len(comment.Instruction)
	for _, contextLines := range [][]core.DiffCommentContextLine{comment.ContextBefore, comment.ContextAfter} {
		for i := range contextLines {
			contextLines[i].Side = strings.ToLower(strings.TrimSpace(contextLines[i].Side))
			if contextLines[i].Side != "old" && contextLines[i].Side != "new" || contextLines[i].Line <= 0 {
				return nil, fmt.Errorf("diff_comment context lines require an old/new side and positive line")
			}
			if len(contextLines[i].Text) > diffCommentMaxLineBytes {
				return nil, fmt.Errorf("diff_comment context line text must be at most %d bytes", diffCommentMaxLineBytes)
			}
			totalBytes += len(contextLines[i].Side) + len(contextLines[i].Text)
		}
	}
	if totalBytes > diffCommentMaxPayloadBytes {
		return nil, fmt.Errorf("diff_comment text payload must be at most %d bytes", diffCommentMaxPayloadBytes)
	}
	return &comment, nil
}

// parseUserMessageContent builds a user core.Message from a content field
// that may be a plain string or an array of content parts (input_text, input_image, input_file, input_upload).
// Chat Completions-style text/image_url parts are also accepted.
// Supported image types are sent inline to the LLM and also saved to disk
// so tools can reopen the original upload later. Text-like files also get a
// text fallback so providers without native file support can still read them.
// Other files are saved to disk and referenced by a marker.
//
// Images exceeding 1 MB are resized/compressed only for the inline LLM payload
// to avoid provider errors; the saved ImagePath always points at the original
// uploaded bytes.
func parseUserMessageContent(content json.RawMessage) (core.Message, error) {
	if err := uploads.ValidateAttachmentCount(content); err != nil {
		return core.Message{}, err
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(content, &parts); err == nil && len(parts) > 0 {
		var llmParts []core.Part
		diffCommentCount := 0
		diffCommentAggregateBytes := 0
		for _, part := range parts {
			partType := strings.ToLower(strings.TrimSpace(jsonString(part["type"])))
			switch partType {
			case "diff_comment":
				diffCommentCount++
				if diffCommentCount > diffCommentMaxPartsPerMessage {
					return core.Message{}, fmt.Errorf("too many diff_comment parts (max %d)", diffCommentMaxPartsPerMessage)
				}
				diffCommentAggregateBytes += len(part["diff_comment"])
				if diffCommentAggregateBytes > diffCommentMaxAggregatePayloadBytes {
					return core.Message{}, fmt.Errorf("diff_comment parts exceed aggregate payload limit (%d bytes)", diffCommentMaxAggregatePayloadBytes)
				}
				comment, err := parseDiffCommentPart(part["diff_comment"])
				if err != nil {
					return core.Message{}, err
				}
				llmParts = append(llmParts, core.Part{Type: core.PartDiffComment, DiffComment: comment})
			case "input_text", "text", "output_text":
				text := jsonString(part["text"])
				if text != "" {
					llmParts = append(llmParts, core.Part{Type: core.PartText, Text: text})
				}
			case "input_upload":
				refPart, err := uploads.ParseReferencePart(part)
				if err != nil {
					return core.Message{}, err
				}
				llmParts = append(llmParts, refPart)
			case "input_image", "image_url":
				if len(part["upload_id"]) != 0 {
					return core.Message{}, fmt.Errorf("%w: use input_upload for references", uploads.ErrInvalidUpload)
				}
				imageURL := jsonImageURL(part["image_url"])
				filename := jsonString(part["filename"])
				if !strings.HasPrefix(imageURL, "data:") {
					return core.Message{}, fmt.Errorf("attachment %q must use an inline data URL", filename)
				}
				mt, b64 := uploads.ParseDataURL(imageURL)
				mt = uploads.NormalizeMediaType(filename, mt, nil)
				if mt == "" || b64 == "" {
					return core.Message{}, fmt.Errorf("attachment %q has an empty or malformed data URL", filename)
				}
				if uploads.IsLLMImageType(mt) {
					if filename == "" {
						filename = "image"
					}

					b64 = uploads.StripBase64Newlines(b64)
					raw, err := uploads.DecodeFile(filename, b64)
					if err != nil {
						return core.Message{}, fmt.Errorf("decode attachment %q: %w", filename, err)
					}
					path, err := uploads.SaveBytes(filename, raw)
					if err != nil {
						return core.Message{}, fmt.Errorf("save attachment %q: %w", filename, err)
					}

					llmParts = append(llmParts, uploads.ImagePart(raw, mt, path))
				} else {
					filePart, err := parseUploadedFilePart(filename, mt, b64)
					if err != nil {
						return core.Message{}, err
					}
					llmParts = append(llmParts, filePart)
				}
			case "input_file":
				if len(part["upload_id"]) != 0 {
					return core.Message{}, fmt.Errorf("%w: use input_upload for references", uploads.ErrInvalidUpload)
				}
				fileData := jsonString(part["file_data"])
				filename := jsonString(part["filename"])
				if filename == "" {
					filename = "upload"
				}
				displayFilename := llm.EmbeddedFileDisplayName(filename)
				if !strings.HasPrefix(fileData, "data:") {
					return core.Message{}, fmt.Errorf("attachment %q must use an inline data URL", displayFilename)
				}
				mt, b64 := uploads.ParseDataURL(fileData)
				mt = uploads.NormalizeMediaType(displayFilename, mt, nil)
				if mt == "" || b64 == "" {
					return core.Message{}, fmt.Errorf("attachment %q has an empty or malformed data URL", displayFilename)
				}
				filePart, err := parseUploadedFilePart(displayFilename, mt, b64)
				if err != nil {
					return core.Message{}, err
				}
				llmParts = append(llmParts, filePart)
			}
		}
		if len(llmParts) > 0 {
			hasDiffComment := false
			hasProviderContent := false
			for _, part := range llmParts {
				hasDiffComment = hasDiffComment || part.Type == core.PartDiffComment
				hasProviderContent = hasProviderContent || ((part.Type == core.PartText || part.Type == core.PartFile) && strings.TrimSpace(part.Text) != "") || part.Type == core.PartImage
			}
			if hasDiffComment && !hasProviderContent {
				return core.Message{}, fmt.Errorf("diff_comment requires adjacent provider-facing content")
			}
			return core.Message{Role: core.RoleUser, Parts: llmParts}, nil
		}
	}
	return core.UserText(extractItemContent(content)), nil
}

func jsonImageURL(raw json.RawMessage) string {
	if s := jsonString(raw); s != "" {
		return s
	}
	var value struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &value); err == nil {
		return strings.TrimSpace(value.URL)
	}
	return ""
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
	wire.WriteOpenAIError(w, status, errorType, message)
}

func writeJSON(w http.ResponseWriter, status int, payload any) { wire.WriteJSON(w, status, payload) }

const jsonGzipMinBytes = wire.JSONGzipMinBytes

func writeJSONGzip(w http.ResponseWriter, r *http.Request, status int, payload any) {
	wire.WriteJSONGzip(w, r, status, payload)
}

func writeJSONGzipBody(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	wire.WriteJSONGzipBody(w, r, status, body)
}

func jsonPayloadETag(body []byte) string { return wire.JSONPayloadETag(body) }

func writeJSONConditional(w http.ResponseWriter, r *http.Request, status int, payload any) {
	wire.WriteJSONConditional(w, r, status, payload)
}

func decodeJSONBody(r *http.Request, dst any) error { return wire.DecodeJSONBody(r, dst) }

const (
	requestSessionIDHeader        = "X-Term-LLM-Session-ID"
	legacyRequestSessionIDHeader  = "session_id"
	requestDraftIDHeader          = "X-Term-LLM-Draft-ID"
	requestPushSubscriptionHeader = "X-Term-LLM-Push-Subscription-ID"
)

func resolveRequestSessionID(r *http.Request) string {
	if sessionID := strings.TrimSpace(r.Header.Get(requestSessionIDHeader)); sessionID != "" {
		return sessionID
	}
	return strings.TrimSpace(r.Header.Get(legacyRequestSessionIDHeader))
}

func ensureSessionID(w http.ResponseWriter) string {
	sessionID := session.NewID()
	w.Header().Set("x-session-id", sessionID)
	return sessionID
}

func setSessionNumberHeader(w http.ResponseWriter, rt *serveRuntime) {
	if number := rt.SessionNumber(); number > 0 {
		w.Header().Set("x-session-number", strconv.FormatInt(number, 10))
	}
}

func requireJSONContentType(r *http.Request) error { return wire.RequireJSONContentType(r) }

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

func randomSuffix() string { return wire.RandomSuffix() }

// parseUploadedFilePart stores any file independently of provider comprehension.
// Providers decide whether FileData can travel natively; Text is the safe fallback.
func parseUploadedFilePart(filename, mediaType, b64 string) (core.Part, error) {
	filename = llm.EmbeddedFileDisplayName(filename)
	b64 = uploads.StripBase64Newlines(b64)
	raw, err := uploads.DecodeFile(filename, b64)
	if err != nil {
		return core.Part{}, fmt.Errorf("decode attachment %q: %w", filename, err)
	}
	mediaType = uploads.NormalizeMediaType(filename, mediaType, raw)
	path, err := uploads.SaveBytes(filename, raw)
	if err != nil {
		return core.Part{}, fmt.Errorf("save attachment %q: %w", filename, err)
	}
	return uploads.FilePart(filename, mediaType, path, raw, b64), nil
}

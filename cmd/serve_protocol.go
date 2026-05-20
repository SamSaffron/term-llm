package cmd

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

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

func stripBase64Newlines(b64Data string) string {
	if !strings.ContainsAny(b64Data, "\r\n") {
		return b64Data
	}
	return strings.NewReplacer("\r", "", "\n", "").Replace(b64Data)
}

func decodedBase64Len(b64Data string) (int, error) {
	b64Data = stripBase64Newlines(b64Data)
	if b64Data == "" {
		return 0, nil
	}
	if len(b64Data)%4 != 0 {
		return 0, fmt.Errorf("decode base64: invalid length %d", len(b64Data))
	}
	decodedLen := base64.StdEncoding.DecodedLen(len(b64Data))
	if strings.HasSuffix(b64Data, "=") {
		decodedLen--
	}
	if strings.HasSuffix(b64Data, "==") {
		decodedLen--
	}
	return decodedLen, nil
}

func decodeUploadedFile(filename, b64Data string) ([]byte, error) {
	b64Data = stripBase64Newlines(b64Data)
	decodedLen, err := decodedBase64Len(b64Data)
	if err != nil {
		return nil, err
	}
	if decodedLen > maxAttachmentBytes {
		return nil, fmt.Errorf("file %q exceeds %d MB limit", filename, maxAttachmentBytes>>20)
	}
	raw := make([]byte, decodedLen)
	n, err := base64.StdEncoding.Decode(raw, []byte(b64Data))
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	return raw[:n], nil
}

// saveUploadedFile decodes base64 data and writes it to the uploads directory,
// returning the full filesystem path. Uses O_CREATE|O_EXCL for atomic uniqueness.
func saveUploadedFile(filename, b64Data string) (string, error) {
	raw, err := decodeUploadedFile(filename, b64Data)
	if err != nil {
		return "", err
	}
	return saveUploadedBytes(filename, raw)
}

func saveUploadedBytes(filename string, raw []byte) (string, error) {
	dataDir, err := session.GetDataDir()
	if err != nil {
		return "", fmt.Errorf("get data dir: %w", err)
	}
	uploadsDir := filepath.Join(dataDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0o700); err != nil {
		return "", fmt.Errorf("create uploads dir: %w", err)
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
// Chat Completions-style text/image_url parts are also accepted.
// Supported image types are sent inline to the LLM and also saved to disk
// so tools can reopen the original upload later. Other files are saved to disk
// and referenced by path in a text part.
//
// Images exceeding 1 MB are resized/compressed only for the inline LLM payload
// to avoid provider errors; the saved ImagePath always points at the original
// uploaded bytes.
func parseUserMessageContent(content json.RawMessage) (llm.Message, error) {
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(content, &parts); err == nil && len(parts) > 0 {
		var llmParts []llm.Part
		fileCount := 0
		for _, part := range parts {
			partType := strings.ToLower(strings.TrimSpace(jsonString(part["type"])))
			switch partType {
			case "input_text", "text", "output_text":
				text := jsonString(part["text"])
				if text != "" {
					textParts, attachments, err := buildPartsFromTextWithLocalImages(text, maxAttachments-fileCount)
					if err != nil {
						return llm.Message{}, err
					}
					fileCount += attachments
					llmParts = append(llmParts, textParts...)
				}
			case "input_image", "image_url":
				imageURL := jsonImageURL(part["image_url"])
				filename := jsonString(part["filename"])
				if !strings.HasPrefix(imageURL, "data:") {
					continue
				}
				mt, b64 := parseDataURL(imageURL)
				if mt == "" || b64 == "" {
					continue
				}
				if isLLMImageType(mt) {
					fileCount++
					if fileCount > maxAttachments {
						return llm.Message{}, fmt.Errorf("too many attachments (max %d)", maxAttachments)
					}
					if filename == "" {
						filename = "image"
					}

					b64 = stripBase64Newlines(b64)
					raw, err := decodeUploadedFile(filename, b64)
					if err != nil {
						return llm.Message{}, fmt.Errorf("decode attachment %q: %w", filename, err)
					}
					part, err := imagePartFromBytes(filename, mt, b64, raw)
					if err != nil {
						return llm.Message{}, fmt.Errorf("save attachment %q: %w", filename, err)
					}
					llmParts = append(llmParts, part)
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
	text := extractItemContent(content)
	if parts, _, err := buildPartsFromTextWithLocalImages(text, maxAttachments); err != nil {
		return llm.Message{}, err
	} else if len(parts) > 0 {
		return llm.Message{Role: llm.RoleUser, Parts: parts}, nil
	}
	return llm.UserText(text), nil
}

func imagePartFromBytes(filename, mediaType, b64 string, raw []byte) (llm.Part, error) {
	path, err := saveUploadedBytes(filename, raw)
	if err != nil {
		return llm.Part{}, err
	}

	sendB64 := stripBase64Newlines(b64)
	sendMT := mediaType
	if len(raw) > maxLLMImageBytes {
		// Resize only the inline payload sent to the model. Keep ImagePath
		// pointing at the original upload so tools can inspect high-res data.
		resized, resMT := resizeImageForLLM(raw, mediaType)
		if len(resized) != len(raw) || resMT != mediaType {
			sendB64 = base64.StdEncoding.EncodeToString(resized)
			sendMT = resMT
		}
	}

	return llm.Part{
		Type:      llm.PartImage,
		ImageData: &llm.ToolImageData{MediaType: sendMT, Base64: sendB64},
		ImagePath: path,
	}, nil
}

func buildPartsFromTextWithLocalImages(text string, remainingAttachments int) ([]llm.Part, int, error) {
	if text == "" {
		return nil, 0, nil
	}
	candidates := extractLocalImagePathCandidates(text)
	if len(candidates) == 0 {
		return []llm.Part{{Type: llm.PartText, Text: text}}, 0, nil
	}

	parts := []llm.Part{{Type: llm.PartText, Text: text}}
	attached := 0
	for _, path := range candidates {
		part, ok, err := localImagePathPart(path)
		if err != nil {
			return nil, 0, err
		}
		if !ok {
			continue
		}
		attached++
		if attached > remainingAttachments {
			return nil, 0, fmt.Errorf("too many attachments (max %d)", maxAttachments)
		}
		parts = append(parts, part)
	}
	return parts, attached, nil
}

func localImagePathPart(path string) (llm.Part, bool, error) {
	path = expandHomePath(strings.TrimSpace(path))
	if path == "" || !filepath.IsAbs(path) || !looksLikeImagePath(path) {
		return llm.Part{}, false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if looksLikeMacScreenshotTempPath(path) {
			return llm.Part{}, false, fmt.Errorf("local image path %q is no longer available or cannot be read; macOS TemporaryItems screenshots are short-lived, so paste/drop the image or save it somewhere stable", path)
		}
		return llm.Part{}, false, nil
	}
	if info.IsDir() {
		return llm.Part{}, false, nil
	}
	if info.Size() > maxAttachmentBytes {
		return llm.Part{}, false, fmt.Errorf("file %q exceeds %d MB limit", filepath.Base(path), maxAttachmentBytes>>20)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return llm.Part{}, false, fmt.Errorf("read local image %q: %w", path, err)
	}
	mediaType := detectImageMediaType(path, raw)
	if !isLLMImageType(mediaType) {
		return llm.Part{}, false, nil
	}
	part, err := imagePartFromBytes(filepath.Base(path), mediaType, base64.StdEncoding.EncodeToString(raw), raw)
	if err != nil {
		return llm.Part{}, false, fmt.Errorf("save local image %q: %w", path, err)
	}
	return part, true, nil
}

func detectImageMediaType(path string, raw []byte) string {
	if len(raw) > 0 {
		if mt := http.DetectContentType(raw); isLLMImageType(mt) {
			return mt
		}
	}
	return strings.TrimSpace(strings.Split(mime.TypeByExtension(filepath.Ext(path)), ";")[0])
}

func looksLikeImagePath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return true
	default:
		return false
	}
}

func looksLikeMacScreenshotTempPath(path string) bool {
	return (strings.HasPrefix(path, "/var/folders/") || strings.HasPrefix(path, "/private/var/folders/")) &&
		strings.Contains(path, "/TemporaryItems/NSIRD_screencaptureui_") &&
		looksLikeImagePath(path)
}

func expandHomePath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func extractLocalImagePathCandidates(text string) []string {
	seen := map[string]bool{}
	var paths []string
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.Trim(candidate, "<>.,;:)]}")
		if candidate == "" || seen[candidate] {
			return
		}
		seen[candidate] = true
		paths = append(paths, candidate)
	}

	for _, candidate := range quotedStrings(text) {
		add(candidate)
	}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "~/") {
			add(trimmed)
		}
	}
	for _, field := range strings.FieldsFunc(text, unicode.IsSpace) {
		if strings.HasPrefix(field, "/") || strings.HasPrefix(field, "~/") {
			add(field)
		}
	}
	return paths
}

func quotedStrings(text string) []string {
	var out []string
	for i := 0; i < len(text); i++ {
		quote := text[i]
		if quote != '\'' && quote != '"' && quote != '`' {
			continue
		}
		start := i + 1
		var b strings.Builder
		for j := start; j < len(text); j++ {
			if text[j] == '\\' && quote != '`' && j+1 < len(text) {
				j++
				b.WriteByte(text[j])
				continue
			}
			if text[j] == quote {
				out = append(out, b.String())
				i = j
				break
			}
			b.WriteByte(text[j])
		}
	}
	return out
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

const jsonGzipMinBytes = 512

// writeJSONGzip writes payload as JSON and gzip-compresses the response when
// the client advertises gzip support and the marshaled payload is larger than
// jsonGzipMinBytes. Small responses stay uncompressed to avoid gzip overhead.
func writeJSONGzip(w http.ResponseWriter, r *http.Request, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSONGzipBody(w, r, status, body)
}

func writeJSONGzipBody(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	uiAddVary(h, "Accept-Encoding")

	if len(body) > jsonGzipMinBytes && uiAcceptsGzip(r.Header.Get("Accept-Encoding")) {
		var buf bytes.Buffer
		gz, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
		if err == nil {
			_, err = gz.Write(body)
			if closeErr := gz.Close(); err == nil {
				err = closeErr
			}
		}
		if err == nil {
			body = buf.Bytes()
			h.Set("Content-Encoding", "gzip")
		}
	}

	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeJSONConditional marshals payload, sets an ETag, and returns 304 Not
// Modified when the client's If-None-Match header already holds the current
// ETag. Cache-Control: no-cache tells browsers to always revalidate, so they
// issue a conditional GET rather than skipping the request entirely.
func jsonPayloadETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func writeJSONConditional(w http.ResponseWriter, r *http.Request, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-cache")
	etag := jsonPayloadETag(body)
	h.Set("ETag", etag)
	if uiETagMatches(r.Header.Get("If-None-Match"), etag) {
		h.Set("Content-Type", "application/json")
		uiAddVary(h, "Accept-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSONGzipBody(w, r, status, body)
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

func setSessionNumberHeader(w http.ResponseWriter, rt *serveRuntime) {
	if rt != nil && rt.sessionMeta != nil && rt.sessionMeta.Number > 0 {
		w.Header().Set("x-session-number", strconv.FormatInt(rt.sessionMeta.Number, 10))
	}
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

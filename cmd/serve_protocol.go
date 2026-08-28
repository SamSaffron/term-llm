package cmd

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type sessionInterruptRequest struct {
	Message            string          `json:"message"`
	Content            json.RawMessage `json:"content"`
	InterjectionID     string          `json:"interjection_id"`
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
	switch llm.NormalizeMediaType(mediaType) {
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

var supportedAttachmentExtensions = map[string]struct{}{
	".jpg": {}, ".jpeg": {}, ".png": {}, ".gif": {}, ".webp": {}, ".pdf": {},
	".txt": {}, ".md": {}, ".markdown": {}, ".json": {}, ".csv": {}, ".yaml": {},
	".yml": {}, ".xml": {}, ".go": {}, ".js": {}, ".jsx": {}, ".ts": {}, ".tsx": {},
	".py": {}, ".rb": {}, ".rs": {}, ".java": {}, ".c": {}, ".h": {}, ".cpp": {},
	".hpp": {}, ".mp3": {}, ".wav": {}, ".ogg": {}, ".mp4": {}, ".webm": {},
}

var supportedAttachmentMediaTypes = map[string]struct{}{
	"image/jpeg": {}, "image/png": {}, "image/gif": {}, "image/webp": {},
	"application/pdf": {}, "text/plain": {}, "text/markdown": {}, "application/json": {},
	"text/csv": {}, "audio/mpeg": {}, "audio/wav": {}, "audio/ogg": {},
	"video/mp4": {}, "video/webm": {},
}

func attachmentMediaTypes() []string {
	values := make([]string, 0, len(supportedAttachmentMediaTypes))
	for value := range supportedAttachmentMediaTypes {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func attachmentExtensions() []string {
	values := make([]string, 0, len(supportedAttachmentExtensions))
	for value := range supportedAttachmentExtensions {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func supportedAttachment(filename, mediaType string) bool {
	if _, ok := supportedAttachmentExtensions[strings.ToLower(filepath.Ext(filename))]; ok {
		return true
	}
	_, ok := supportedAttachmentMediaTypes[llm.NormalizeMediaType(mediaType)]
	return ok
}

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
// returning the full filesystem path. The final filename includes a random suffix
// created atomically by os.CreateTemp.
func saveUploadedFile(filename, b64Data string) (string, error) {
	raw, err := decodeUploadedFile(filename, b64Data)
	if err != nil {
		return "", err
	}
	return saveUploadedBytes(filename, raw)
}

func uploadFilenameForMediaType(prefix, mediaType string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "upload"
	}
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/jpeg":
		return prefix + ".jpg"
	case "image/png":
		return prefix + ".png"
	case "image/gif":
		return prefix + ".gif"
	case "image/webp":
		return prefix + ".webp"
	default:
		return prefix
	}
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

func normalizeUploadMediaType(filename, mediaType string, raw []byte) string {
	mediaType = llm.NormalizeMediaType(mediaType)
	if mediaType == "" || mediaType == "application/octet-stream" {
		if extType := llm.NormalizeMediaType(mime.TypeByExtension(strings.ToLower(filepath.Ext(filename)))); extType != "" {
			mediaType = extType
		}
	}
	if (mediaType == "" || mediaType == "application/octet-stream") && len(raw) > 0 {
		if detected := llm.NormalizeMediaType(http.DetectContentType(raw)); detected != "" {
			mediaType = detected
		}
	}
	if mediaType == "application/octet-stream" && isTextUploadExtension(filename) && !bytes.Contains(raw, []byte{0}) && utf8.Valid(raw) {
		mediaType = "text/plain"
	}
	return mediaType
}

func uploadFallbackText(filename, mediaType string, raw []byte) string {
	if text, ok := textUploadContent(filename, mediaType, raw); ok {
		return llm.FormatEmbeddedFileText(filename, mediaType, text)
	}
	return fmt.Sprintf("[User uploaded file: %s — saved locally]\n\n", llm.EmbeddedFileDisplayName(filename))
}

func textUploadContent(filename, mediaType string, raw []byte) (string, bool) {
	if len(raw) == 0 {
		return "", true
	}
	if bytes.Contains(raw, []byte{0}) || !utf8.Valid(raw) {
		return "", false
	}
	mediaType = llm.NormalizeMediaType(mediaType)
	genericMediaType := mediaType == "" || mediaType == "application/octet-stream"
	if isTextUploadMIME(mediaType) || isTextUploadExtension(filename) || (genericMediaType && looksLikePlainText(raw)) {
		return string(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))), true
	}
	return "", false
}

func isTextUploadMIME(mediaType string) bool {
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/json", "application/x-ndjson", "application/xml", "application/yaml", "application/x-yaml":
		return true
	default:
		return false
	}
}

func isTextUploadExtension(filename string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".txt", ".text", ".md", ".markdown", ".csv", ".tsv", ".json", ".jsonl", ".yaml", ".yml", ".xml", ".html", ".htm",
		".go", ".js", ".jsx", ".ts", ".tsx", ".py", ".rb", ".rs", ".java", ".c", ".cc", ".cpp", ".h", ".hpp", ".cs", ".sh", ".bash", ".zsh", ".fish", ".sql", ".css", ".scss", ".toml", ".ini", ".conf", ".log":
		return true
	default:
		return false
	}
}

func looksLikePlainText(raw []byte) bool {
	checkLen := len(raw)
	if checkLen > 8192 {
		checkLen = 8192
	}
	if checkLen == 0 {
		return true
	}
	printable := 0
	for _, r := range string(raw[:checkLen]) {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			printable++
		case r >= 0x20 && r != utf8.RuneError:
			printable++
		}
	}
	return printable*100/checkLen >= 95
}

const maxImageMetadataBytes = 1 << 20

func imageDisplayDimensions(raw []byte) (width, height int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0
	}
	width, height = cfg.Width, cfg.Height
	if orientation := jpegEXIFOrientation(raw); orientation >= 5 && orientation <= 8 {
		width, height = height, width
	}
	return width, height
}

func uploadedImageDimensions(raw []byte, clientWidth, clientHeight int) (width, height int) {
	width, height = imageDisplayDimensions(raw)
	if width <= 0 || height <= 0 {
		return 0, 0
	}
	// Browser dimensions account for EXIF orientation. Only accept an exact match
	// for the server-derived display dimensions, so callers cannot reserve
	// arbitrary or incorrectly rotated geometry.
	if clientWidth == width && clientHeight == height {
		return clientWidth, clientHeight
	}
	return width, height
}

func jsonImageDimension(raw json.RawMessage) int {
	var value int64
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value <= 0 || value > int64(^uint(0)>>1) {
		return 0
	}
	return int(value)
}

func jpegEXIFOrientation(raw []byte) int {
	if len(raw) < 4 || raw[0] != 0xff || raw[1] != 0xd8 {
		return 0
	}
	for offset := 2; offset+4 <= len(raw); {
		if raw[offset] != 0xff {
			offset++
			continue
		}
		for offset < len(raw) && raw[offset] == 0xff {
			offset++
		}
		if offset >= len(raw) {
			return 0
		}
		marker := raw[offset]
		offset++
		if marker == 0xd9 || marker == 0xda {
			return 0
		}
		if marker == 0x00 || marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) {
			continue
		}
		if offset+2 > len(raw) {
			return 0
		}
		segmentLength := int(binary.BigEndian.Uint16(raw[offset : offset+2]))
		if segmentLength < 2 || offset+segmentLength > len(raw) {
			return 0
		}
		payload := raw[offset+2 : offset+segmentLength]
		if marker == 0xe1 && len(payload) >= 6 && bytes.Equal(payload[:6], []byte("Exif\x00\x00")) {
			if orientation := tiffOrientation(payload[6:]); orientation != 0 {
				return orientation
			}
		}
		offset += segmentLength
	}
	return 0
}

func tiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 0
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 0
	}
	ifdOffset := uint64(order.Uint32(tiff[4:8]))
	if ifdOffset+2 > uint64(len(tiff)) {
		return 0
	}
	entryCount := uint64(order.Uint16(tiff[ifdOffset : ifdOffset+2]))
	entriesStart := ifdOffset + 2
	if entryCount > (uint64(len(tiff))-entriesStart)/12 {
		return 0
	}
	for index := uint64(0); index < entryCount; index++ {
		entryOffset := entriesStart + index*12
		entry := tiff[entryOffset : entryOffset+12]
		if order.Uint16(entry[0:2]) != 0x0112 || order.Uint16(entry[2:4]) != 3 || order.Uint32(entry[4:8]) != 1 {
			continue
		}
		orientation := int(order.Uint16(entry[8:10]))
		if orientation >= 1 && orientation <= 8 {
			return orientation
		}
		return 0
	}
	return 0
}

const (
	diffCommentMaxPathBytes             = 1024
	diffCommentMaxLineBytes             = 8 * 1024
	diffCommentMaxInstructionBytes      = 8 * 1024
	diffCommentMaxPayloadBytes          = 32 * 1024
	diffCommentMaxPartsPerMessage       = 25
	diffCommentMaxAggregatePayloadBytes = 256 * 1024
)

func parseDiffCommentPart(raw json.RawMessage) (*llm.DiffComment, error) {
	var comment llm.DiffComment
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
	for _, contextLines := range [][]llm.DiffCommentContextLine{comment.ContextBefore, comment.ContextAfter} {
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

// parseUserMessageContent builds a user llm.Message from a content field
// that may be a plain string or an array of content parts (input_text, input_image, input_file).
// Chat Completions-style text/image_url parts are also accepted.
// Supported image types are sent inline to the LLM and also saved to disk
// so tools can reopen the original upload later. Text-like files also get a
// text fallback so providers without native file support can still read them.
// Other files are saved to disk and referenced by a marker.
//
// Images exceeding 1 MB are resized/compressed only for the inline LLM payload
// to avoid provider errors; the saved ImagePath always points at the original
// uploaded bytes.
func parseUserMessageContent(content json.RawMessage) (llm.Message, error) {
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(content, &parts); err == nil && len(parts) > 0 {
		var llmParts []llm.Part
		fileCount := 0
		diffCommentCount := 0
		diffCommentAggregateBytes := 0
		for _, part := range parts {
			partType := strings.ToLower(strings.TrimSpace(jsonString(part["type"])))
			switch partType {
			case "diff_comment":
				diffCommentCount++
				if diffCommentCount > diffCommentMaxPartsPerMessage {
					return llm.Message{}, fmt.Errorf("too many diff_comment parts (max %d)", diffCommentMaxPartsPerMessage)
				}
				diffCommentAggregateBytes += len(part["diff_comment"])
				if diffCommentAggregateBytes > diffCommentMaxAggregatePayloadBytes {
					return llm.Message{}, fmt.Errorf("diff_comment parts exceed aggregate payload limit (%d bytes)", diffCommentMaxAggregatePayloadBytes)
				}
				comment, err := parseDiffCommentPart(part["diff_comment"])
				if err != nil {
					return llm.Message{}, err
				}
				llmParts = append(llmParts, llm.Part{Type: llm.PartDiffComment, DiffComment: comment})
			case "input_text", "text", "output_text":
				text := jsonString(part["text"])
				if text != "" {
					llmParts = append(llmParts, llm.Part{Type: llm.PartText, Text: text})
				}
			case "input_image", "image_url":
				imageURL := jsonImageURL(part["image_url"])
				filename := jsonString(part["filename"])
				if !strings.HasPrefix(imageURL, "data:") {
					return llm.Message{}, fmt.Errorf("attachment %q must use an inline data URL", filename)
				}
				mt, b64 := parseDataURL(imageURL)
				mt = normalizeUploadMediaType(filename, mt, nil)
				if mt == "" || b64 == "" {
					return llm.Message{}, fmt.Errorf("attachment %q has an empty or malformed data URL", filename)
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
					path, err := saveUploadedBytes(filename, raw)
					if err != nil {
						return llm.Message{}, fmt.Errorf("save attachment %q: %w", filename, err)
					}

					sendB64 := b64
					sendMT := mt
					if len(raw) > maxLLMImageBytes {
						// Resize only the inline payload sent to the model. Keep ImagePath
						// pointing at the original upload so tools can inspect high-res data.
						resized, resMT := resizeImageForLLM(raw, mt)
						if len(resized) != len(raw) || resMT != mt {
							sendB64 = base64.StdEncoding.EncodeToString(resized)
							sendMT = resMT
						}
					}

					clientWidth := jsonImageDimension(part["width"])
					clientHeight := jsonImageDimension(part["height"])
					width, height := uploadedImageDimensions(raw, clientWidth, clientHeight)
					llmParts = append(llmParts, llm.Part{
						Type: llm.PartImage,
						ImageData: &llm.ToolImageData{
							MediaType: sendMT,
							Base64:    sendB64,
							Width:     width,
							Height:    height,
						},
						ImagePath: path,
					})
				} else {
					if !supportedAttachment(filename, mt) {
						return llm.Message{}, fmt.Errorf("unsupported attachment type %q for %q", mt, filename)
					}
					fileCount++
					if fileCount > maxAttachments {
						return llm.Message{}, fmt.Errorf("too many attachments (max %d)", maxAttachments)
					}
					if filename == "" {
						filename = "image"
					}
					if _, err := saveUploadedFile(filename, b64); err != nil {
						return llm.Message{}, fmt.Errorf("save attachment %q: %w", filename, err)
					}
					llmParts = append(llmParts, llm.Part{
						Type: llm.PartText,
						Text: fmt.Sprintf("[User uploaded file: %s — saved locally]\n\n", llm.EmbeddedFileDisplayName(filename)),
					})
				}
			case "input_file":
				fileData := jsonString(part["file_data"])
				filename := jsonString(part["filename"])
				if filename == "" {
					filename = "upload"
				}
				displayFilename := llm.EmbeddedFileDisplayName(filename)
				if !strings.HasPrefix(fileData, "data:") {
					return llm.Message{}, fmt.Errorf("attachment %q must use an inline data URL", displayFilename)
				}
				mt, b64 := parseDataURL(fileData)
				mt = normalizeUploadMediaType(displayFilename, mt, nil)
				if mt == "" || b64 == "" {
					return llm.Message{}, fmt.Errorf("attachment %q has an empty or malformed data URL", displayFilename)
				}
				fileCount++
				if fileCount > maxAttachments {
					return llm.Message{}, fmt.Errorf("too many attachments (max %d)", maxAttachments)
				}
				cleanB64 := stripBase64Newlines(b64)
				raw, err := decodeUploadedFile(displayFilename, cleanB64)
				if err != nil {
					return llm.Message{}, fmt.Errorf("decode attachment %q: %w", displayFilename, err)
				}
				mt = normalizeUploadMediaType(displayFilename, mt, raw)
				if !supportedAttachment(displayFilename, mt) {
					return llm.Message{}, fmt.Errorf("unsupported attachment type %q for %q", mt, displayFilename)
				}
				path, err := saveUploadedBytes(displayFilename, raw)
				if err != nil {
					return llm.Message{}, fmt.Errorf("save attachment %q: %w", displayFilename, err)
				}
				llmParts = append(llmParts, llm.Part{
					Type: llm.PartFile,
					Text: uploadFallbackText(displayFilename, mt, raw),
					FileData: &llm.ToolFileData{
						MediaType: mt,
						Base64:    cleanB64,
						Filename:  displayFilename,
						SizeBytes: int64(len(raw)),
					},
					FilePath: path,
				})
			}
		}
		if len(llmParts) > 0 {
			hasDiffComment := false
			hasProviderContent := false
			for _, part := range llmParts {
				hasDiffComment = hasDiffComment || part.Type == llm.PartDiffComment
				hasProviderContent = hasProviderContent || ((part.Type == llm.PartText || part.Type == llm.PartFile) && strings.TrimSpace(part.Text) != "") || part.Type == llm.PartImage
			}
			if hasDiffComment && !hasProviderContent {
				return llm.Message{}, fmt.Errorf("diff_comment requires adjacent provider-facing content")
			}
			return llm.Message{Role: llm.RoleUser, Parts: llmParts}, nil
		}
	}
	return llm.UserText(extractItemContent(content)), nil
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

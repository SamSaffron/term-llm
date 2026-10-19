package uploads

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/filemime"
	"github.com/samsaffron/term-llm/internal/session"
)

const (
	MaxAttachments     = 10
	MaxAttachmentBytes = 20 << 20 // 20 MB per file (decoded)
)

// Dir returns the first-party uploads directory, or an empty string when the
// application data directory is unavailable.
func Dir() string {
	dataDir, err := session.GetDataDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dataDir, "uploads")
}

// ReadDir creates the uploads directory and returns its canonical path when
// possible. It returns an empty string when the directory is unavailable.
func ReadDir() string {
	dir := Dir()
	if dir == "" {
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

// ParseDataURL splits a base64 data URL into its media type and payload.
func ParseDataURL(dataURL string) (mediaType, base64Data string) {
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

// IsLLMImageType reports whether providers handle the media type natively.
func IsLLMImageType(mediaType string) bool {
	switch filemime.NormalizeMediaType(mediaType) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

// StripBase64Newlines accepts wrapped base64 payloads from clients.
func StripBase64Newlines(data string) string {
	if !strings.ContainsAny(data, "\r\n") {
		return data
	}
	return strings.NewReplacer("\r", "", "\n", "").Replace(data)
}

func decodedBase64Len(data string) (int, error) {
	data = StripBase64Newlines(data)
	if data == "" {
		return 0, nil
	}
	if len(data)%4 != 0 {
		return 0, fmt.Errorf("decode base64: invalid length %d", len(data))
	}
	decodedLen := base64.StdEncoding.DecodedLen(len(data))
	if strings.HasSuffix(data, "=") {
		decodedLen--
	}
	if strings.HasSuffix(data, "==") {
		decodedLen--
	}
	return decodedLen, nil
}

// DecodeFile decodes one uploaded base64 payload after enforcing its size limit.
func DecodeFile(filename, data string) ([]byte, error) {
	data = StripBase64Newlines(data)
	decodedLen, err := decodedBase64Len(data)
	if err != nil {
		return nil, err
	}
	if decodedLen > MaxAttachmentBytes {
		return nil, fmt.Errorf("file %q exceeds %d MB limit", filename, MaxAttachmentBytes>>20)
	}
	raw := make([]byte, decodedLen)
	n, err := base64.StdEncoding.Decode(raw, []byte(data))
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	return raw[:n], nil
}

// FilenameForMediaType adds the conventional extension for a supported image.
func FilenameForMediaType(prefix, mediaType string) string {
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

// SaveBytes stores an upload using the standard attachment size limit.
func SaveBytes(filename string, raw []byte) (string, error) {
	return SaveBytesWithLimit(filename, raw, MaxAttachmentBytes)
}

// SaveBytesWithLimit stores an upload with private permissions.
func SaveBytesWithLimit(filename string, raw []byte, maxBytes int64) (string, error) {
	dataDir, err := session.GetDataDir()
	if err != nil {
		return "", fmt.Errorf("get data dir: %w", err)
	}
	uploadsDir := filepath.Join(dataDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0o700); err != nil {
		return "", fmt.Errorf("create uploads dir: %w", err)
	}
	if maxBytes <= 0 {
		return "", fmt.Errorf("invalid upload size limit %d", maxBytes)
	}
	if int64(len(raw)) > maxBytes {
		if maxBytes == MaxAttachmentBytes {
			return "", fmt.Errorf("file %q exceeds %d MB limit", filename, maxBytes>>20)
		}
		return "", fmt.Errorf("file %q exceeds %d byte limit", filename, maxBytes)
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

// NormalizeMediaType returns a canonical, provider-recognized upload label.
func NormalizeMediaType(filename, mediaType string, raw []byte) string {
	mediaType = filemime.NormalizeMediaType(mediaType)
	if mediaType == "" || mediaType == "application/octet-stream" {
		if extType := filemime.NormalizeMediaType(mime.TypeByExtension(strings.ToLower(filepath.Ext(filename)))); extType != "" {
			mediaType = extType
		}
	}
	if (mediaType == "" || mediaType == "application/octet-stream") && len(raw) > 0 {
		if detected := filemime.NormalizeMediaType(http.DetectContentType(raw)); detected != "" {
			mediaType = detected
		}
	}
	if mediaType == "application/octet-stream" && isTextExtension(filename) && rawLooksLikeText(raw) {
		mediaType = "text/plain"
	}
	if !IsLLMImageType(mediaType) {
		if rawLooksLikeText(raw) {
			mediaType = filemime.CanonicalTextUploadMediaType(filename, mediaType)
		} else {
			mediaType = filemime.CanonicalUploadMediaType(filename, mediaType)
		}
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return mediaType
}

// FallbackText returns provider-safe text for a stored upload.
func FallbackText(filename, mediaType, path string, raw []byte) string {
	if text, ok := TextContent(filename, mediaType, raw); ok {
		return llm.FormatEmbeddedFileText(filename, mediaType, text)
	}
	return llm.FormatUploadedFileNotice(filename, mediaType, path, int64(len(raw)))
}

// TextContent returns safe textual upload content when it can be embedded.
func TextContent(filename, mediaType string, raw []byte) (string, bool) {
	if len(raw) == 0 {
		return "", true
	}
	if !rawLooksLikeText(raw) {
		return "", false
	}
	mediaType = filemime.NormalizeMediaType(mediaType)
	genericMediaType := mediaType == "" || mediaType == "application/octet-stream"
	if filemime.IsTextLikeMediaType(mediaType) || isTextExtension(filename) || (genericMediaType && looksLikePlainText(raw)) {
		return string(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))), true
	}
	return "", false
}

func rawLooksLikeText(raw []byte) bool {
	return len(raw) > 0 && !bytes.Contains(raw, []byte{0}) && utf8.Valid(raw)
}

func isTextExtension(filename string) bool {
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

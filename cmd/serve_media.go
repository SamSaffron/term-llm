package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

const (
	serveMediaMaxImageBytes int64 = 25 << 20
	serveMediaMaxVideoBytes int64 = 256 << 20
)

type webMediaEntry struct {
	Reference string `json:"reference,omitempty"`
	URL       string `json:"url"`
	MediaType string `json:"type"`
	Name      string `json:"name,omitempty"`
	Caption   string `json:"caption,omitempty"`
}

func (s *serveServer) toolMediaEntries(media []llm.MediaArtifact) []webMediaEntry {
	entries := make([]webMediaEntry, 0, len(media))
	for _, item := range media {
		mediaType := strings.ToLower(strings.TrimSpace(item.MediaType))
		if !strings.HasPrefix(mediaType, "image/") && !strings.HasPrefix(mediaType, "video/") {
			continue
		}
		mediaURL := s.mediaURL(item.StoredPath)
		if mediaURL == "" {
			continue
		}
		entries = append(entries, webMediaEntry{Reference: item.Reference, URL: mediaURL, MediaType: mediaType, Name: item.Name, Caption: item.Caption})
	}
	return entries
}

func mediaEntriesFromValue(value any) []webMediaEntry {
	if typed, ok := value.([]webMediaEntry); ok {
		return append([]webMediaEntry(nil), typed...)
	}
	raw, ok := value.([]any)
	if !ok {
		return nil
	}
	entries := make([]webMediaEntry, 0, len(raw))
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		urlValue := strings.TrimSpace(stringValue(entry["url"]))
		mediaType := strings.ToLower(strings.TrimSpace(stringValue(entry["type"])))
		if urlValue == "" || (!strings.HasPrefix(mediaType, "image/") && !strings.HasPrefix(mediaType, "video/")) {
			continue
		}
		entries = append(entries, webMediaEntry{
			Reference: strings.TrimSpace(stringValue(entry["reference"])),
			URL:       urlValue, MediaType: mediaType,
			Name: strings.TrimSpace(stringValue(entry["name"])), Caption: strings.TrimSpace(stringValue(entry["caption"])),
		})
	}
	return entries
}

type serveMediaPublisher struct {
	dir string
}

func newServeMediaPublisher() (*serveMediaPublisher, error) {
	dataDir, err := session.GetDataDir()
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	return &serveMediaPublisher{dir: filepath.Join(dataDir, "media")}, nil
}

func (p *serveMediaPublisher) PublishMedia(ctx context.Context, sourcePath, mediaType string) (string, error) {
	if p == nil || strings.TrimSpace(p.dir) == "" {
		return "", fmt.Errorf("media publisher is unavailable")
	}
	ext, limit, ok := serveMediaExtension(mediaType)
	if !ok {
		return "", fmt.Errorf("unsupported media type %q", mediaType)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", fmt.Errorf("open source: %w", err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return "", fmt.Errorf("stat source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("source is not a regular file")
	}
	if info.Size() > limit {
		return "", fmt.Errorf("media exceeds %d MiB limit", limit>>20)
	}
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return "", fmt.Errorf("create media directory: %w", err)
	}

	tmp, err := os.CreateTemp(p.dir, ".publish-*")
	if err != nil {
		return "", fmt.Errorf("create temporary media: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", fmt.Errorf("secure temporary media: %w", err)
	}
	hash := sha256.New()
	limited := &io.LimitedReader{R: &contextReader{ctx: ctx, reader: source}, N: limit + 1}
	written, err := io.Copy(io.MultiWriter(tmp, hash), limited)
	if err != nil {
		return "", fmt.Errorf("copy media: %w", err)
	}
	if written > limit {
		return "", fmt.Errorf("media exceeds %d MiB limit", limit>>20)
	}
	if info.Size() != written {
		return "", fmt.Errorf("source changed while publishing")
	}
	header := make([]byte, 512)
	n, readErr := tmp.ReadAt(header, 0)
	if readErr != nil && readErr != io.EOF {
		return "", fmt.Errorf("verify published media: %w", readErr)
	}
	if detected := tools.DetectShowMediaType(header[:n]); detected != strings.ToLower(strings.TrimSpace(mediaType)) {
		return "", fmt.Errorf("media type changed while publishing")
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("sync media: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close media: %w", err)
	}
	name := hex.EncodeToString(hash.Sum(nil)[:16]) + ext
	destination := filepath.Join(p.dir, name)
	if existing, statErr := os.Lstat(destination); statErr == nil && existing.Mode().IsRegular() {
		return destination, nil
	}
	if err := os.Rename(tmpPath, destination); err != nil {
		if existing, statErr := os.Lstat(destination); statErr == nil && existing.Mode().IsRegular() {
			return destination, nil
		}
		return "", fmt.Errorf("publish media: %w", err)
	}
	keep = true
	return destination, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(p)
	}
}

func serveMediaExtension(mediaType string) (extension string, limit int64, ok bool) {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/png":
		return ".png", serveMediaMaxImageBytes, true
	case "image/jpeg":
		return ".jpg", serveMediaMaxImageBytes, true
	case "image/gif":
		return ".gif", serveMediaMaxImageBytes, true
	case "image/webp":
		return ".webp", serveMediaMaxImageBytes, true
	case "image/bmp":
		return ".bmp", serveMediaMaxImageBytes, true
	case "video/mp4":
		return ".mp4", serveMediaMaxVideoBytes, true
	case "video/webm":
		return ".webm", serveMediaMaxVideoBytes, true
	default:
		return "", 0, false
	}
}

func serveMediaTypeForName(name string) (string, bool) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png", true
	case ".jpg":
		return "image/jpeg", true
	case ".gif":
		return "image/gif", true
	case ".webp":
		return "image/webp", true
	case ".bmp":
		return "image/bmp", true
	case ".mp4":
		return "video/mp4", true
	case ".webm":
		return "video/webm", true
	default:
		return "", false
	}
}

func (s *serveServer) handleMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if s.mediaPublisher == nil {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/media/")
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\\`) {
		http.NotFound(w, r)
		return
	}
	mediaType, ok := serveMediaTypeForName(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	absFile := filepath.Join(s.mediaPublisher.dir, name)
	info, err := os.Lstat(absFile)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", `inline; filename="media`+filepath.Ext(name)+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Add("Vary", "Authorization, Cookie")
	serveResolvedFile(w, r, absFile)
}

func (s *serveServer) mediaURL(storedPath string) string {
	if s == nil || s.mediaPublisher == nil || strings.TrimSpace(storedPath) == "" {
		return ""
	}
	canonicalDir, err := canonicalizeServeDirForWrite(s.mediaPublisher.dir)
	if err != nil {
		return ""
	}
	canonicalPath, err := canonicalizeServeExistingPath(storedPath)
	if err != nil || !pathWithinDir(canonicalPath, canonicalDir) || filepath.Dir(canonicalPath) != canonicalDir {
		return ""
	}
	if _, ok := serveMediaTypeForName(filepath.Base(canonicalPath)); !ok {
		return ""
	}
	return s.cfg.mediaRoute() + filepath.Base(canonicalPath)
}

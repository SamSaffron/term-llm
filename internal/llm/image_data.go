package llm

import (
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func partImageData(part Part) (mediaType, base64Data string, ok bool) {
	if part.Type != PartImage {
		return "", "", false
	}

	if part.ImageData != nil {
		mediaType = strings.TrimSpace(part.ImageData.MediaType)
		base64Data = strings.TrimSpace(part.ImageData.Base64)
	}
	if base64Data != "" {
		return mediaType, base64Data, true
	}
	if strings.TrimSpace(part.ImagePath) == "" {
		return "", "", false
	}
	return imageFileData(part.ImagePath, mediaType)
}

func imageFileData(path, mediaType string) (string, string, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", false
	}

	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return "", "", false
	}

	mediaType = strings.TrimSpace(mediaType)
	if mediaType == "" || mediaType == "application/octet-stream" {
		mediaType = detectImageMediaType(data, path)
	}
	if mediaType == "" {
		return "", "", false
	}

	return mediaType, base64.StdEncoding.EncodeToString(data), true
}

func detectImageMediaType(data []byte, path string) string {
	if detected := strings.TrimSpace(http.DetectContentType(data)); strings.HasPrefix(detected, "image/") {
		return detected
	}

	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".tif", ".tiff":
		return "image/tiff"
	default:
		return ""
	}
}

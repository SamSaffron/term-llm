package cmd

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// classifyImageTypes are the image formats the OpenAI Decisions API accepts.
var classifyImageTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// loadClassifyImages converts --image values into base64 data URLs. Values
// may be file paths or existing data:image URLs; hosted URLs are rejected
// because the Decisions API only accepts inline images.
func loadClassifyImages(values []string) ([]string, error) {
	images := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, errors.New("--image must not be empty")
		}
		image, err := loadClassifyImage(value)
		if err != nil {
			return nil, err
		}
		images = append(images, image)
	}
	return images, nil
}

func loadClassifyImage(value string) (string, error) {
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(lower, "data:"):
		return classifyImageDataURL(value)
	case strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://"):
		return "", fmt.Errorf("--image %q: hosted image URLs are not supported; download the image and pass the file path", value)
	case value == "-":
		return "", errors.New("--image does not read stdin; pass a file path")
	}
	data, err := readClassifyFile(value)
	if err != nil {
		return "", fmt.Errorf("read --image %q: %w", value, err)
	}
	mime := http.DetectContentType(data)
	if !classifyImageTypes[mime] {
		return "", fmt.Errorf("--image %q: unsupported image type %q (want PNG, JPEG, GIF, or WebP)", value, mime)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// classifyImageDataURL validates a data:image URL and normalizes it: the
// header is lowercased and whitespace (such as base64 line wrapping) is
// removed from the payload, since the API needs a contiguous URL.
func classifyImageDataURL(value string) (string, error) {
	header, payload, ok := strings.Cut(value, ",")
	header = strings.ToLower(strings.TrimSpace(header))
	mime, isBase64 := strings.CutSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	if !ok || !isBase64 || !classifyImageTypes[mime] {
		return "", errors.New("--image data URL must be data:image/{png,jpeg,gif,webp};base64,...")
	}
	payload = strings.Join(strings.Fields(payload), "")
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Errorf("--image data URL has invalid base64: %w", err)
	}
	if len(decoded) == 0 {
		return "", errors.New("--image data URL is empty")
	}
	if len(decoded) > maxClassifyInputBytes {
		return "", fmt.Errorf("--image data URL exceeds %d bytes", maxClassifyInputBytes)
	}
	return header + "," + payload, nil
}

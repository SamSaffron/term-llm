package cmd

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/samsaffron/term-llm/internal/typesafe"
)

// Only transport framing belongs here. The endpoint validates dimensions,
// decodes/preprocesses images and owns all model-specific behavior.
func classifyImages(paths []string) ([]typesafe.Image, error) {
	const maxImageBytes = 4 << 20
	if len(paths) > 4 {
		return nil, fmt.Errorf("at most four --image files are supported; the endpoint may impose a lower limit")
	}
	var images []typesafe.Image
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open image: %w", err)
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			return nil, fmt.Errorf("image must be a regular local file")
		}
		data, err := io.ReadAll(io.LimitReader(f, maxImageBytes+1))
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("read image: %w", err)
		}
		if len(data) == 0 || len(data) > maxImageBytes {
			return nil, fmt.Errorf("image must contain 1 byte to 4 MiB")
		}
		mime := http.DetectContentType(data)
		switch mime {
		case "image/png", "image/jpeg", "image/webp":
		default:
			return nil, fmt.Errorf("image must be PNG, JPEG, or WebP")
		}
		images = append(images, typesafe.Image{ContentType: mime, Base64: base64.StdEncoding.EncodeToString(data)})
	}
	return images, nil
}

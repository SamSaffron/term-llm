package ask

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

// The real upload helpers live in package cmd; these minimal equivalents let
// the ask package exercise input preparation in isolation.
func init() {
	Uploads = UploadHooks{
		Dir: func() string {
			dataDir, err := session.GetDataDir()
			if err != nil {
				return ""
			}
			return filepath.Join(dataDir, "uploads")
		},
		SaveBytes: func(filename string, raw []byte, maxBytes int64) (string, error) {
			dir := Uploads.Dir()
			if dir == "" {
				return "", fmt.Errorf("no data dir")
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return "", err
			}
			if int64(len(raw)) > maxBytes {
				return "", fmt.Errorf("file %q exceeds %d byte limit", filename, maxBytes)
			}
			safe := filepath.Base(filename)
			ext := filepath.Ext(safe)
			f, err := os.CreateTemp(dir, strings.TrimSuffix(safe, ext)+"_*"+ext)
			if err != nil {
				return "", err
			}
			defer f.Close()
			if err := f.Chmod(0o600); err != nil {
				return "", err
			}
			if _, err := f.Write(raw); err != nil {
				return "", err
			}
			return f.Name(), nil
		},
		ResizeImage: func(data []byte, mediaType string) ([]byte, string) {
			if len(data) <= 1<<20 {
				return data, mediaType
			}
			src, _, err := image.Decode(bytes.NewReader(data))
			if err != nil {
				return data, mediaType
			}
			dst := image.NewRGBA(image.Rect(0, 0, 256, 256))
			for y := 0; y < 256; y++ {
				for x := 0; x < 256; x++ {
					dst.Set(x, y, src.At(x*src.Bounds().Dx()/256, y*src.Bounds().Dy()/256))
				}
			}
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, dst, nil); err != nil {
				return data, mediaType
			}
			return buf.Bytes(), "image/jpeg"
		},
		ImageDimensions: func(raw []byte) (int, int) {
			cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
			if err != nil {
				return 0, 0
			}
			return cfg.Width, cfg.Height
		},
		NormalizeMediaType: func(filename, mediaType string, raw []byte) string {
			mediaType = llm.NormalizeMediaType(mediaType)
			if mediaType == "" || mediaType == "application/octet-stream" {
				mediaType = llm.NormalizeMediaType(http.DetectContentType(raw))
			}
			if !strings.HasPrefix(mediaType, "image/") {
				mediaType = llm.CanonicalUploadMediaType(filename, mediaType)
			}
			return mediaType
		},
		MaxImagePixels: 50_000_000,
	}
}

type progressiveTestTool struct{}

func (t *progressiveTestTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "progressive_test_tool", Description: "test tool", Schema: map[string]any{"type": "object"}}
}

func (t *progressiveTestTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	return llm.TextOutput("tool ok"), nil
}

func (t *progressiveTestTool) Preview(args json.RawMessage) string { return "" }

func (t *progressiveTestTool) IsFinishingTool() bool { return true }

package uploads

import (
	"bytes"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"log"
	"math"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	MaxLLMImageBytes = 1 << 20 // 1 MB
	MaxImagePixels   = 50_000_000
)

type imageResizeLogFunc func(format string, args ...any)

// ResizeImageForLLM returns bounded image bytes suitable for an LLM and logs
// web diagnostics. Failures preserve the original bytes and media type.
func ResizeImageForLLM(data []byte, mediaType string) ([]byte, string) {
	return resizeImageForLLMWithLogger(data, mediaType, log.Printf)
}

// ResizeImageForLLMQuiet performs the same preprocessing without diagnostics.
func ResizeImageForLLMQuiet(data []byte, mediaType string) ([]byte, string) {
	return resizeImageForLLMWithLogger(data, mediaType, nil)
}

func resizeImageForLLMWithLogger(data []byte, mediaType string, logf imageResizeLogFunc) ([]byte, string) {
	if len(data) <= MaxLLMImageBytes {
		return data, mediaType
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		if logf != nil {
			logf("[web] resizeImageForLLM: decode config failed (%v) — sending original (%d bytes)", err, len(data))
		}
		return data, mediaType
	}
	width, height := int64(cfg.Width), int64(cfg.Height)
	tooManyPixels := width <= 0 || height <= 0 || width > MaxImagePixels || height > MaxImagePixels/width
	if tooManyPixels {
		if logf != nil {
			logf("[web] resizeImageForLLM: refusing to decode %dx%d image — sending original (%d bytes)", cfg.Width, cfg.Height, len(data))
		}
		return data, mediaType
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		if logf != nil {
			logf("[web] resizeImageForLLM: decode failed (%v) — sending original (%d bytes)", err, len(data))
		}
		return data, mediaType
	}
	bounds := img.Bounds()
	origW := bounds.Dx()
	origH := bounds.Dy()
	newW, newH := llmImageDimensions(origW, origH)
	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	draw.BiLinear.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)
	for _, quality := range []int{85, 70, 55, 40} {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
			if logf != nil {
				logf("[web] resizeImageForLLM: jpeg encode q=%d failed: %v", quality, err)
			}
			continue
		}
		if buf.Len() <= MaxLLMImageBytes {
			if logf != nil {
				logf("[web] resizeImageForLLM: resized %dx%d→%dx%d q=%d (%d→%d bytes)", origW, origH, newW, newH, quality, len(data), buf.Len())
			}
			return buf.Bytes(), "image/jpeg"
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 40}); err == nil {
		if logf != nil {
			logf("[web] resizeImageForLLM: could not reach ≤1MB, sending best effort (%d bytes)", buf.Len())
		}
		return buf.Bytes(), "image/jpeg"
	}
	if logf != nil {
		logf("[web] resizeImageForLLM: all attempts failed — sending original (%d bytes)", len(data))
	}
	return data, mediaType
}

func llmImageDimensions(origW, origH int) (int, int) {
	targetPixels := float64(MaxLLMImageBytes) / 4.0
	currentPixels := float64(origW * origH)
	scale := 1.0
	if currentPixels > targetPixels {
		scale = math.Sqrt(targetPixels / currentPixels)
	}
	newW := int(math.Round(float64(origW) * scale))
	newH := int(math.Round(float64(origH) * scale))
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}
	return newW, newH
}

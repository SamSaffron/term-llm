package cmd

import (
	"bytes"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"log"
	"math"
	"os"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const maxLLMImageBytes = 1 << 20 // 1 MB

// resizeImageForLLM returns image bytes (and updated media type) suitable for
// sending to an LLM. If the input is already ≤1 MB it is returned unchanged.
// Otherwise the image is downscaled and re-encoded as JPEG at decreasing quality
// levels until it fits. On any error the original bytes + media type are returned
// with a warning logged — we never fail a user message just because we couldn't
// compress a preview.
func resizeImageForLLM(data []byte, mediaType string) ([]byte, string) {
	if len(data) <= maxLLMImageBytes {
		return data, mediaType
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		log.Printf("[web] resizeImageForLLM: decode failed (%v) — sending original (%d bytes)", err, len(data))
		return data, mediaType
	}

	resized, ok := encodeResizedImageForLLM(img, len(data))
	if !ok {
		log.Printf("[web] resizeImageForLLM: all attempts failed — sending original (%d bytes)", len(data))
		return data, mediaType
	}
	return resized, "image/jpeg"
}

func resizeImageFileForLLM(path string, originalSize int, mediaType string) ([]byte, string, bool) {
	if originalSize <= maxLLMImageBytes {
		return nil, mediaType, false
	}

	f, err := os.Open(path)
	if err != nil {
		log.Printf("[web] resizeImageForLLM: open failed (%v) — sending original (%d bytes)", err, originalSize)
		return nil, mediaType, false
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		log.Printf("[web] resizeImageForLLM: decode failed (%v) — sending original (%d bytes)", err, originalSize)
		return nil, mediaType, false
	}

	resized, ok := encodeResizedImageForLLM(img, originalSize)
	if !ok {
		log.Printf("[web] resizeImageForLLM: all attempts failed — sending original (%d bytes)", originalSize)
		return nil, mediaType, false
	}
	return resized, "image/jpeg", true
}

func encodeResizedImageForLLM(img image.Image, originalSize int) ([]byte, bool) {
	// Compute scale factor: we want pixel area such that a rough 3-bytes-per-pixel
	// JPEG estimate fits within the target. Use a conservative 4 bpp for safety.
	bounds := img.Bounds()
	origW := bounds.Dx()
	origH := bounds.Dy()
	targetPixels := float64(maxLLMImageBytes) / 4.0
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

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	draw.BiLinear.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)

	// Try JPEG quality levels until we're under the limit.
	for _, quality := range []int{85, 70, 55, 40} {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
			log.Printf("[web] resizeImageForLLM: jpeg encode q=%d failed: %v", quality, err)
			continue
		}
		if buf.Len() <= maxLLMImageBytes {
			log.Printf("[web] resizeImageForLLM: resized %dx%d→%dx%d q=%d (%d→%d bytes)",
				origW, origH, newW, newH, quality, originalSize, buf.Len())
			return buf.Bytes(), true
		}
	}

	// Last resort: send smallest attempt anyway (better than erroring).
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 40}); err == nil {
		log.Printf("[web] resizeImageForLLM: could not reach ≤1MB, sending best effort (%d bytes)", buf.Len())
		return buf.Bytes(), true
	}

	return nil, false
}

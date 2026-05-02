package image

import (
	"bytes"
	"context"
	"hash/fnv"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"time"
)

// DebugProvider implements ImageProvider for local development without API costs
type DebugProvider struct {
	delay time.Duration
}

// NewDebugProvider creates a debug provider with an optional delay (in seconds)
func NewDebugProvider(delaySeconds float64) *DebugProvider {
	return &DebugProvider{
		delay: time.Duration(delaySeconds * float64(time.Second)),
	}
}

func (p *DebugProvider) Name() string {
	return "Debug"
}

func (p *DebugProvider) SupportsEdit() bool {
	return true
}

func (p *DebugProvider) SupportsMultiImage() bool {
	return false
}

func (p *DebugProvider) Generate(ctx context.Context, req GenerateRequest) (*ImageResult, error) {
	return p.generateImage(ctx, req.Prompt)
}

func (p *DebugProvider) Edit(ctx context.Context, req EditRequest) (*ImageResult, error) {
	return p.generateImage(ctx, req.Prompt)
}

func (p *DebugProvider) generateImage(ctx context.Context, prompt string) (*ImageResult, error) {
	// Apply configured delay
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Create a 512x512 image
	width, height := 512, 512
	img := image.NewRGBA(image.Rect(0, 0, width, height))

	// Fill with deterministic pseudo-random colored rectangles derived from the prompt.
	h := fnv.New64a()
	_, _ = h.Write([]byte(prompt))
	rng := rand.New(rand.NewSource(int64(h.Sum64())))

	// Background color
	bgColor := color.RGBA{
		R: uint8(rng.Intn(256)),
		G: uint8(rng.Intn(256)),
		B: uint8(rng.Intn(256)),
		A: 255,
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, bgColor)
		}
	}

	// Draw 5-15 random rectangles
	numRects := 5 + rng.Intn(11)
	for i := 0; i < numRects; i++ {
		rectColor := color.RGBA{
			R: uint8(rng.Intn(256)),
			G: uint8(rng.Intn(256)),
			B: uint8(rng.Intn(256)),
			A: 255,
		}

		// Random rectangle bounds
		x1 := rng.Intn(width)
		y1 := rng.Intn(height)
		x2 := x1 + 20 + rng.Intn(200)
		y2 := y1 + 20 + rng.Intn(200)

		// Clamp to image bounds
		if x2 > width {
			x2 = width
		}
		if y2 > height {
			y2 = height
		}

		// Fill rectangle
		for y := y1; y < y2; y++ {
			for x := x1; x < x2; x++ {
				img.Set(x, y, rectColor)
			}
		}
	}

	// Add some noise
	for i := 0; i < 1000; i++ {
		x := rng.Intn(width)
		y := rng.Intn(height)
		noiseColor := color.RGBA{
			R: uint8(rng.Intn(256)),
			G: uint8(rng.Intn(256)),
			B: uint8(rng.Intn(256)),
			A: 255,
		}
		img.Set(x, y, noiseColor)
	}

	// Encode to PNG
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}

	return &ImageResult{
		Data:     buf.Bytes(),
		MimeType: "image/png",
	}, nil
}

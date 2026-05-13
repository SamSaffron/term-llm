package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	imagedraw "image/draw"
	_ "image/gif" // GIF decode support
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // WebP decode support
)

// ViewImageTool implements the view_image tool.
type ViewImageTool struct {
	approval *ApprovalManager
}

// NewViewImageTool creates a new ViewImageTool.
func NewViewImageTool(approval *ApprovalManager) *ViewImageTool {
	return &ViewImageTool{
		approval: approval,
	}
}

// ViewImageArgs are the arguments for view_image.
type ViewImageArgs struct {
	FilePath string     `json:"file_path"`
	Detail   string     `json:"detail,omitempty"` // "low", "high", or "auto"
	Region   string     `json:"region,omitempty"` // optional: full, left_half, right_half, top_half, bottom_half
	Crop     *ImageCrop `json:"crop,omitempty"`   // optional pixel crop before sending
	Scale    int        `json:"scale,omitempty"`  // optional 1-4x upscale after crop, useful for tiny text
}

type ImageCrop struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

const (
	maxImageSize    = 5 * 1024 * 1024 // 5MB - Anthropic API limit
	maxDimension    = 1568            // Anthropic recommended max for optimal performance
	maxAbsDimension = 8000            // Anthropic absolute max
	jpegQuality     = 85              // JPEG quality for re-encoding
)

var supportedImageFormats = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

var supportedImageMimes = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
	"image/gif":  {},
	"image/webp": {},
}

func (t *ViewImageTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        ViewImageToolName,
		Description: "View and analyze an image file. Returns base64-encoded image for multimodal analysis. Supports PNG, JPEG, GIF, WebP.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"file_path": map[string]interface{}{
					"type":        "string",
					"description": "Path to the image file",
				},
				"detail": map[string]interface{}{
					"type":        "string",
					"description": "Vision detail level: 'low', 'high', or 'auto' (default: 'auto'). Use 'high' for handwriting, small text, charts, and OCR/transcription.",
					"enum":        []string{"low", "high", "auto"},
					"default":     "auto",
				},
				"region": map[string]interface{}{
					"type":        "string",
					"description": "Optional common crop before viewing. Useful for two-page notebook photos: view left_half and right_half separately.",
					"enum":        []string{"full", "left_half", "right_half", "top_half", "bottom_half"},
					"default":     "full",
				},
				"crop": map[string]interface{}{
					"type":        "object",
					"description": "Optional pixel crop before viewing: x, y, width, height.",
					"properties": map[string]interface{}{
						"x":      map[string]interface{}{"type": "integer", "minimum": 0},
						"y":      map[string]interface{}{"type": "integer", "minimum": 0},
						"width":  map[string]interface{}{"type": "integer", "minimum": 1},
						"height": map[string]interface{}{"type": "integer", "minimum": 1},
					},
					"required":             []string{"x", "y", "width", "height"},
					"additionalProperties": false,
				},
				"scale": map[string]interface{}{
					"type":        "integer",
					"description": "Optional 1-4x upscale after crop before sending to the model. Use 2 or 3 for tiny handwriting/text.",
					"minimum":     1,
					"maximum":     4,
					"default":     1,
				},
			},
			"required":             []string{"file_path"},
			"additionalProperties": false,
		},
	}
}

func (t *ViewImageTool) Preview(args json.RawMessage) string {
	var a ViewImageArgs
	if err := json.Unmarshal(args, &a); err != nil || a.FilePath == "" {
		return ""
	}
	return a.FilePath
}

func (t *ViewImageTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a ViewImageArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, err.Error()))), nil
	}

	if a.FilePath == "" {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, "file_path is required"))), nil
	}

	// Check permissions via approval manager
	if t.approval != nil {
		outcome, err := t.approval.CheckPathApproval(ViewImageToolName, a.FilePath, a.FilePath, false)
		if err != nil {
			if toolErr, ok := err.(*ToolError); ok {
				return llm.TextOutput(formatToolError(toolErr)), nil
			}
			return llm.TextOutput(formatToolError(NewToolError(ErrPermissionDenied, err.Error()))), nil
		}
		if outcome == Cancel {
			return llm.TextOutput(formatToolError(NewToolErrorf(ErrPermissionDenied, "access denied: %s", a.FilePath))), nil
		}
	}

	resolvedPath, err := resolveToolPath(a.FilePath, false)
	if err != nil {
		if toolErr, ok := err.(*ToolError); ok {
			return llm.TextOutput(formatToolError(toolErr)), nil
		}
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrInvalidParams, "cannot resolve path: %v", err))), nil
	}

	// Check file exists
	if _, err := os.Stat(resolvedPath); err != nil {
		if os.IsNotExist(err) {
			return llm.TextOutput(formatToolError(NewToolError(ErrFileNotFound, a.FilePath))), nil
		}
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot stat file: %v", err))), nil
	}

	// Read file
	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to read image: %v", err))), nil
	}

	// Detect format from file content
	sniffSize := 512
	if len(data) < sniffSize {
		sniffSize = len(data)
	}
	mimeType := http.DetectContentType(data[:sniffSize])
	if mimeType == "application/octet-stream" {
		ext := strings.ToLower(filepath.Ext(resolvedPath))
		var ok bool
		mimeType, ok = supportedImageFormats[ext]
		if !ok {
			return llm.TextOutput(formatToolError(NewToolErrorf(ErrUnsupportedFormat, "unsupported format: %s (supported: PNG, JPEG, GIF, WebP)", ext))), nil
		}
	} else if !isSupportedImageMime(mimeType) {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrUnsupportedFormat, "unsupported format: %s (supported: PNG, JPEG, GIF, WebP)", mimeType))), nil
	}

	// Process image: crop/upscale if requested, then resize if needed and ensure under size limit
	processedData, processedMime, resized, processedDesc, err := processImage(data, mimeType, a.Region, a.Crop, a.Scale)
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to process image: %v", err))), nil
	}

	// Encode as base64
	encoded := base64.StdEncoding.EncodeToString(processedData)

	// Build result message
	var sizeInfo string
	if resized {
		sizeInfo = fmt.Sprintf("Size: %d bytes (resized from %d bytes)", len(processedData), len(data))
	} else {
		sizeInfo = fmt.Sprintf("Size: %d bytes", len(processedData))
	}

	textResult := fmt.Sprintf(`Image loaded: %s
Format: %s
%s
Dimensions: %s
Detail: %s`,
		a.FilePath,
		processedMime,
		sizeInfo,
		processedDesc,
		getDetail(a.Detail),
	)

	return llm.ToolOutput{
		Content: textResult,
		ContentParts: []llm.ToolContentPart{
			{Type: llm.ToolContentPartText, Text: textResult},
			{
				Type: llm.ToolContentPartImageData,
				ImageData: &llm.ToolImageData{
					MediaType: processedMime,
					Base64:    encoded,
					Detail:    getDetail(a.Detail),
				},
			},
		},
	}, nil
}

// processImage checks if an image needs resizing and processes it accordingly.
// Returns the (possibly resized) image data, mime type, whether it was resized, a dimension summary, and any error.
func processImage(data []byte, originalMime, region string, crop *ImageCrop, scale int) ([]byte, string, bool, string, error) {
	// Decode image to check dimensions
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", false, "", fmt.Errorf("failed to decode image: %w", err)
	}

	detectedMime := mimeTypeFromDecodedFormat(format)
	if detectedMime == "" {
		detectedMime = originalMime
	}

	origBounds := img.Bounds()
	origWidth := origBounds.Dx()
	origHeight := origBounds.Dy()
	processed := img
	processedBounds := origBounds
	changed := false

	if rect, ok, err := requestedCropRect(origBounds, region, crop); err != nil {
		return nil, "", false, "", err
	} else if ok && rect.Dx() > 0 && rect.Dy() > 0 && rect != origBounds {
		processed = cropImage(processed, rect)
		processedBounds = processed.Bounds()
		changed = true
	}

	if scale < 1 {
		scale = 1
	}
	if scale > 4 {
		scale = 4
	}
	if scale > 1 {
		processed = resizeImage(processed, processedBounds.Dx()*scale, processedBounds.Dy()*scale)
		processedBounds = processed.Bounds()
		changed = true
	}

	width := processedBounds.Dx()
	height := processedBounds.Dy()

	// Check if resizing is needed
	needsResize := width > maxDimension || height > maxDimension || len(data) > maxImageSize

	if !needsResize && !changed {
		desc := fmt.Sprintf("%dx%d", origWidth, origHeight)
		return data, detectedMime, false, desc, nil
	}

	newWidth, newHeight := width, height
	if width > maxDimension || height > maxDimension {
		if width > height {
			newWidth = maxDimension
			newHeight = int(float64(height) * float64(maxDimension) / float64(width))
		} else {
			newHeight = maxDimension
			newWidth = int(float64(width) * float64(maxDimension) / float64(height))
		}
		processed = resizeImage(processed, newWidth, newHeight)
		processedBounds = processed.Bounds()
		changed = true
	}

	result, outputMime, err := encodeProcessedImage(processed, format, jpegQuality)
	if err != nil {
		return nil, "", false, "", err
	}

	// If still too large after resizing, try more aggressive compression
	if len(result) > maxImageSize {
		result, outputMime, err = encodeProcessedImage(processed, "jpeg", 70)
		if err != nil {
			return nil, "", false, "", err
		}

		// If still too large, reduce dimensions further
		if len(result) > maxImageSize {
			smallerWidth := processedBounds.Dx() * 3 / 4
			smallerHeight := processedBounds.Dy() * 3 / 4
			processed = resizeImage(processed, smallerWidth, smallerHeight)
			processedBounds = processed.Bounds()
			result, outputMime, err = encodeProcessedImage(processed, "jpeg", 70)
			if err != nil {
				return nil, "", false, "", err
			}
		}
	}

	if len(result) > maxImageSize {
		return nil, "", false, "", fmt.Errorf("image still exceeds 5MB after resizing (%d bytes)", len(result))
	}

	desc := fmt.Sprintf("%dx%d → %dx%d", origWidth, origHeight, processedBounds.Dx(), processedBounds.Dy())
	return result, outputMime, true, desc, nil
}

func requestedCropRect(bounds image.Rectangle, region string, crop *ImageCrop) (image.Rectangle, bool, error) {
	if crop != nil {
		rect := image.Rect(bounds.Min.X+crop.X, bounds.Min.Y+crop.Y, bounds.Min.X+crop.X+crop.Width, bounds.Min.Y+crop.Y+crop.Height)
		rect = rect.Intersect(bounds)
		if rect.Empty() {
			return image.Rectangle{}, false, fmt.Errorf("crop is outside image bounds")
		}
		return rect, true, nil
	}

	w, h := bounds.Dx(), bounds.Dy()
	switch region {
	case "", "full":
		return bounds, false, nil
	case "left_half":
		return image.Rect(bounds.Min.X, bounds.Min.Y, bounds.Min.X+w/2, bounds.Min.Y+h), true, nil
	case "right_half":
		return image.Rect(bounds.Min.X+w/2, bounds.Min.Y, bounds.Max.X, bounds.Min.Y+h), true, nil
	case "top_half":
		return image.Rect(bounds.Min.X, bounds.Min.Y, bounds.Min.X+w, bounds.Min.Y+h/2), true, nil
	case "bottom_half":
		return image.Rect(bounds.Min.X, bounds.Min.Y+h/2, bounds.Min.X+w, bounds.Max.Y), true, nil
	default:
		return image.Rectangle{}, false, fmt.Errorf("unsupported region %q", region)
	}
}

func cropImage(src image.Image, rect image.Rectangle) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	imagedraw.Draw(dst, dst.Bounds(), src, rect.Min, imagedraw.Src)
	return dst
}

func encodeProcessedImage(img image.Image, format string, quality int) ([]byte, string, error) {
	var buf bytes.Buffer
	switch format {
	case "png", "gif":
		if err := png.Encode(&buf, img); err != nil {
			return nil, "", fmt.Errorf("failed to encode PNG: %w", err)
		}
		return buf.Bytes(), "image/png", nil
	default:
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, "", fmt.Errorf("failed to encode JPEG: %w", err)
		}
		return buf.Bytes(), "image/jpeg", nil
	}
}

func mimeTypeFromDecodedFormat(format string) string {
	switch strings.ToLower(format) {
	case "jpeg", "jpg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	default:
		return ""
	}
}

// resizeImage resizes an image to the specified dimensions using high-quality interpolation.
func resizeImage(src image.Image, width, height int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

func isSupportedImageMime(mimeType string) bool {
	_, ok := supportedImageMimes[mimeType]
	return ok
}

func getDetail(detail string) string {
	switch detail {
	case "low", "high":
		return detail
	default:
		return "auto"
	}
}

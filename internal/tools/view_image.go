package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif" // GIF decode support
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // WebP decode support
)

// ViewImageTool implements the view_image tool.
type ViewImageTool struct {
	approval       *ApprovalManager
	config         *ToolConfig
	visionProvider llm.Provider
	visionModel    string
}

// NewViewImageTool creates a new ViewImageTool.
func NewViewImageTool(approval *ApprovalManager, configs ...*ToolConfig) *ViewImageTool {
	return &ViewImageTool{
		approval: approval,
		config:   optionalToolConfig(configs),
	}
}

// NewViewImageToolWithVision creates a view_image tool that analyzes images by
// calling a separate vision-capable LLM and returning text only.
func NewViewImageToolWithVision(approval *ApprovalManager, provider llm.Provider, model string, configs ...*ToolConfig) *ViewImageTool {
	return &ViewImageTool{
		approval:       approval,
		config:         optionalToolConfig(configs),
		visionProvider: provider,
		visionModel:    strings.TrimSpace(model),
	}
}

// ViewImageArgs are the arguments for view_image.
type ViewImageArgs struct {
	FilePath string  `json:"file_path"`
	Question string  `json:"question,omitempty"` // Optional focused question for routed-vision mode
	Prompt   string  `json:"prompt,omitempty"`   // Alias for question
	Detail   string  `json:"detail,omitempty"`   // "low", "high", or "auto"
	Region   string  `json:"region,omitempty"`   // "full", "left_half", "right_half", "top_half", "bottom_half"
	Scale    float64 `json:"scale,omitempty"`    // 1-4x upscale after region
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
		Description: "View and analyze an image file. Supports PNG, JPEG, GIF, WebP. In routed-vision mode this returns a textual analysis from a vision-capable model. For transcription, dense handwriting, or two-page spreads, use a preset region and high detail.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"file_path": map[string]interface{}{
					"type":        "string",
					"description": "Path to the image file",
				},
				"question": map[string]interface{}{
					"type":        "string",
					"description": "Optional focused question or task for image analysis (e.g. transcribe text, identify objects, compare layout)",
				},
				"prompt": map[string]interface{}{
					"type":        "string",
					"description": "Alias for question; use only if question is not provided",
				},
				"detail": map[string]interface{}{
					"type":        "string",
					"description": "Detail level: 'low', 'high', or 'auto' (default: 'auto')",
					"enum":        []string{"low", "high", "auto"},
					"default":     "auto",
				},
				"region": map[string]interface{}{
					"type":        "string",
					"description": "Optional preset region before analysis. Use left_half/right_half for two-page notebook spreads.",
					"enum":        []string{"full", "left_half", "right_half", "top_half", "bottom_half"},
					"default":     "full",
				},
				"scale": map[string]interface{}{
					"type":        "number",
					"description": "Optional 1-4x upscale after region selection. Useful for small text regions; defaults to 1.",
					"minimum":     1,
					"maximum":     4,
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

	resolvedPath, err := resolveToolPathWithConfig(a.FilePath, false, t.config)
	if err != nil {
		if toolErr, ok := err.(*ToolError); ok {
			return llm.TextOutput(formatToolError(toolErr)), nil
		}
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrInvalidParams, "cannot resolve path: %v", err))), nil
	}

	// Check permissions via approval manager
	if t.approval != nil {
		outcome, err := t.approval.CheckPathApproval(ViewImageToolName, resolvedPath, a.FilePath, false)
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

	// Apply optional region/scale before normal provider-size processing.
	transformDesc := ""
	data, mimeType, transformDesc, err = transformImageForView(data, mimeType, a)
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrInvalidParams, "%v", err))), nil
	}

	// Process image: resize if needed, ensure under size limit
	processedData, processedMime, resized, err := processImage(data, mimeType)
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
	if transformDesc != "" {
		sizeInfo += "; " + transformDesc
	}

	textResult := fmt.Sprintf(`Image loaded: %s
Format: %s
%s
Detail: %s`,
		a.FilePath,
		processedMime,
		sizeInfo,
		getDetail(a.Detail),
	)

	if t.visionProvider != nil {
		analysis, err := t.analyzeWithVisionProvider(ctx, processedMime, encoded, textResult, a)
		if err != nil {
			return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "vision analysis failed: %v", err))), nil
		}
		return llm.TextOutput(analysis), nil
	}

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

func (t *ViewImageTool) analyzeWithVisionProvider(ctx context.Context, mimeType, base64Data, metadata string, args ViewImageArgs) (string, error) {
	question := strings.TrimSpace(args.Question)
	if question == "" {
		question = strings.TrimSpace(args.Prompt)
	}
	if question == "" {
		question = "Describe the image, transcribe any visible text, and call out details relevant to the user's likely request. If uncertain, say so."
	}

	prompt := fmt.Sprintf(`You are the vision analysis backend for a text-only LLM's view_image tool.
Return a concise but useful textual interpretation of the supplied image.
If the request asks for transcription, preserve line breaks and uncertain characters where practical.
If details are ambiguous or low confidence, explicitly mention uncertainty.

Tool request metadata:
%s

Question/task from caller:
%s`, metadata, question)

	req := llm.Request{
		Model:     strings.TrimSpace(t.visionModel),
		Ephemeral: true,
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			Parts: []llm.Part{
				{Type: llm.PartText, Text: prompt},
				{Type: llm.PartImage, ImageData: &llm.ToolImageData{MediaType: mimeType, Base64: base64Data, Detail: getDetail(args.Detail)}},
			},
		}},
		MaxTurns: 1,
	}

	stream, err := t.visionProvider.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var b strings.Builder
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch event.Type {
		case llm.EventTextDelta:
			b.WriteString(event.Text)
		case llm.EventError:
			if event.Err != nil {
				return "", event.Err
			}
		}
	}

	analysis := strings.TrimSpace(b.String())
	if analysis == "" {
		analysis = "Vision analysis completed, but the vision model returned no text."
	}
	return metadata + "\n\nVision analysis:\n" + analysis, nil
}

// transformImageForView applies region/scale requested by the caller before
// final provider-size processing. It is intentionally separate from processImage:
// region selection first preserves useful detail for dense handwriting and two-page spreads.
func transformImageForView(data []byte, mimeType string, args ViewImageArgs) ([]byte, string, string, error) {
	region := strings.TrimSpace(strings.ToLower(args.Region))
	if region == "" {
		region = "full"
	}
	scale := args.Scale
	if scale == 0 {
		scale = 1
	}
	if scale < 1 || scale > 4 {
		return nil, "", "", fmt.Errorf("scale must be between 1 and 4")
	}
	if region == "full" && scale == 1 {
		return data, mimeType, "", nil
	}

	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to decode image: %w", err)
	}
	bounds := img.Bounds()
	cropRect, desc, err := cropRectForView(bounds, region)
	if err != nil {
		return nil, "", "", err
	}

	cropped := cropImage(img, cropRect)
	out := image.Image(cropped)
	if scale > 1 {
		w := int(float64(cropRect.Dx()) * scale)
		h := int(float64(cropRect.Dy()) * scale)
		if w > maxAbsDimension || h > maxAbsDimension {
			return nil, "", "", fmt.Errorf("scaled image would be %dx%d, exceeding max dimension %d", w, h, maxAbsDimension)
		}
		out = resizeImage(cropped, w, h)
		desc = strings.TrimSpace(desc + fmt.Sprintf("; scaled %.2gx", scale))
	}

	encoded, outMime, err := encodeViewedImage(out, format, mimeType)
	if err != nil {
		return nil, "", "", err
	}
	return encoded, outMime, desc, nil
}

func cropRectForView(bounds image.Rectangle, region string) (image.Rectangle, string, error) {
	midX := bounds.Min.X + bounds.Dx()/2
	midY := bounds.Min.Y + bounds.Dy()/2
	switch region {
	case "", "full":
		return bounds, "", nil
	case "left_half":
		return image.Rect(bounds.Min.X, bounds.Min.Y, midX, bounds.Max.Y), "region left_half", nil
	case "right_half":
		return image.Rect(midX, bounds.Min.Y, bounds.Max.X, bounds.Max.Y), "region right_half", nil
	case "top_half":
		return image.Rect(bounds.Min.X, bounds.Min.Y, bounds.Max.X, midY), "region top_half", nil
	case "bottom_half":
		return image.Rect(bounds.Min.X, midY, bounds.Max.X, bounds.Max.Y), "region bottom_half", nil
	default:
		return image.Rectangle{}, "", fmt.Errorf("unsupported region %q", region)
	}
}

func cropImage(src image.Image, rect image.Rectangle) image.Image {
	if sub, ok := src.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		return sub.SubImage(rect)
	}
	dst := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(dst, dst.Bounds(), src, rect.Min, draw.Src)
	return dst
}

func encodeViewedImage(img image.Image, format, fallbackMime string) ([]byte, string, error) {
	var buf bytes.Buffer
	switch format {
	case "png", "gif":
		if err := png.Encode(&buf, img); err != nil {
			return nil, "", fmt.Errorf("failed to encode PNG: %w", err)
		}
		return buf.Bytes(), "image/png", nil
	default:
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return nil, "", fmt.Errorf("failed to encode JPEG: %w", err)
		}
		outMime := mimeTypeFromDecodedFormat(format)
		if outMime == "" {
			outMime = fallbackMime
		}
		if outMime != "image/jpeg" {
			outMime = "image/jpeg"
		}
		return buf.Bytes(), outMime, nil
	}
}

// processImage checks if an image needs resizing and processes it accordingly.
// Returns the (possibly resized) image data, mime type, whether it was resized, and any error.
func processImage(data []byte, originalMime string) ([]byte, string, bool, error) {
	// Decode image to check dimensions
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", false, fmt.Errorf("failed to decode image: %w", err)
	}

	detectedMime := mimeTypeFromDecodedFormat(format)
	if detectedMime == "" {
		detectedMime = originalMime
	}

	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	// Check if resizing is needed
	needsResize := width > maxDimension || height > maxDimension || len(data) > maxImageSize

	if !needsResize {
		return data, detectedMime, false, nil
	}

	// Calculate new dimensions maintaining aspect ratio
	newWidth, newHeight := width, height
	if width > maxDimension || height > maxDimension {
		if width > height {
			newWidth = maxDimension
			newHeight = int(float64(height) * float64(maxDimension) / float64(width))
		} else {
			newHeight = maxDimension
			newWidth = int(float64(width) * float64(maxDimension) / float64(height))
		}
	}

	// Resize the image
	resized := resizeImage(img, newWidth, newHeight)

	// Encode to appropriate format
	// Use JPEG for most cases (better compression), PNG if original was PNG/GIF (transparency)
	var buf bytes.Buffer
	var outputMime string

	switch format {
	case "png", "gif":
		// Keep PNG for formats that might have transparency
		if err := png.Encode(&buf, resized); err != nil {
			return nil, "", false, fmt.Errorf("failed to encode PNG: %w", err)
		}
		outputMime = "image/png"
	default:
		// Use JPEG for better compression
		if err := jpeg.Encode(&buf, resized, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return nil, "", false, fmt.Errorf("failed to encode JPEG: %w", err)
		}
		outputMime = "image/jpeg"
	}

	result := buf.Bytes()

	// If still too large after resizing, try more aggressive compression
	if len(result) > maxImageSize {
		// Try JPEG with lower quality
		buf.Reset()
		if err := jpeg.Encode(&buf, resized, &jpeg.Options{Quality: 70}); err != nil {
			return nil, "", false, fmt.Errorf("failed to encode JPEG: %w", err)
		}
		result = buf.Bytes()
		outputMime = "image/jpeg"

		// If still too large, reduce dimensions further
		if len(result) > maxImageSize {
			smallerWidth := newWidth * 3 / 4
			smallerHeight := newHeight * 3 / 4
			resized = resizeImage(img, smallerWidth, smallerHeight)
			buf.Reset()
			if err := jpeg.Encode(&buf, resized, &jpeg.Options{Quality: 70}); err != nil {
				return nil, "", false, fmt.Errorf("failed to encode JPEG: %w", err)
			}
			result = buf.Bytes()
		}
	}

	if len(result) > maxImageSize {
		return nil, "", false, fmt.Errorf("image still exceeds 5MB after resizing (%d bytes)", len(result))
	}

	return result, outputMime, true, nil
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
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
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

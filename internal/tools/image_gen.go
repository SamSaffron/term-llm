package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/image"
	"github.com/samsaffron/term-llm/internal/llm"
	memorystore "github.com/samsaffron/term-llm/internal/memory"
)

// ImageRecorder is a minimal interface for recording generated images.
// Using an interface keeps the tools package decoupled from memory internals.
type ImageRecorder interface {
	RecordImage(ctx context.Context, r *memorystore.ImageRecord) error
}

// ImageGenerateTool implements the image_generate tool.
type ImageGenerateTool struct {
	approval      *ApprovalManager
	config        *config.Config
	providerName  string // Override provider name
	imageRecorder ImageRecorder
	agent         string
	sessionID     string
}

// NewImageGenerateTool creates a new ImageGenerateTool.
func NewImageGenerateTool(approval *ApprovalManager, cfg *config.Config, providerOverride string, recorder ImageRecorder, agent, sessionID string) *ImageGenerateTool {
	return &ImageGenerateTool{
		approval:      approval,
		config:        cfg,
		providerName:  providerOverride,
		imageRecorder: recorder,
		agent:         agent,
		sessionID:     sessionID,
	}
}

// ImageGenerateArgs are the arguments for image_generate.
type ImageGenerateArgs struct {
	Prompt          string   `json:"prompt"`
	InputImage      string   `json:"input_image,omitempty"`       // Single path for editing/variation (backward compat)
	InputImages     []string `json:"input_images,omitempty"`      // Multiple paths for multi-image editing
	AspectRatio     string   `json:"aspect_ratio,omitempty"`      // e.g., "16:9", "4:3"
	OutputPath      string   `json:"output_path,omitempty"`       // Save location
	ShowImage       *bool    `json:"show_image,omitempty"`        // Display via icat (default: true)
	CopyToClipboard *bool    `json:"copy_to_clipboard,omitempty"` // Copy to clipboard (default: true)
}

func (t *ImageGenerateTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        ImageGenerateToolName,
		Description: "Generate an image from a text prompt. Optionally provide an input image for editing/variation.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"prompt": map[string]interface{}{
					"type":        "string",
					"description": "Description of the image to generate",
				},
				"input_image": map[string]interface{}{
					"type":        "string",
					"description": "Path to input image for editing/variation (optional, for single image)",
				},
				"input_images": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Paths to multiple input images for multi-image editing (optional, supported by Gemini and OpenRouter)",
				},
				"aspect_ratio": map[string]interface{}{
					"type":        "string",
					"description": "Aspect ratio, e.g., '1:1', '16:9', '4:3' (default: '1:1')",
					"default":     "1:1",
				},
				"output_path": map[string]interface{}{
					"type":        "string",
					"description": "Path to save the generated image (defaults to temp file)",
				},
				"show_image": map[string]interface{}{
					"type":        "boolean",
					"description": "Display generated image via terminal (icat) (default: true)",
					"default":     true,
				},
				"copy_to_clipboard": map[string]interface{}{
					"type":        "boolean",
					"description": "Copy generated image to system clipboard (default: true)",
					"default":     true,
				},
			},
			"required":             []string{"prompt"},
			"additionalProperties": false,
		},
	}
}

func (t *ImageGenerateTool) Preview(args json.RawMessage) string {
	var a ImageGenerateArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "Generating image..."
	}
	prompt := a.Prompt
	if len(prompt) > 50 {
		prompt = prompt[:47] + "..."
	}
	if a.InputImage != "" {
		return fmt.Sprintf("Editing image: %s", prompt)
	}
	return fmt.Sprintf("Generating image: %s", prompt)
}

func (t *ImageGenerateTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a ImageGenerateArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, err.Error()))), nil
	}

	if a.Prompt == "" {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, "prompt is required"))), nil
	}

	// Check output path permissions if specified
	if a.OutputPath != "" && t.approval != nil {
		outcome, err := t.approval.CheckPathApproval(ImageGenerateToolName, a.OutputPath, a.OutputPath, true)
		if err != nil {
			if toolErr, ok := err.(*ToolError); ok {
				return llm.TextOutput(formatToolError(toolErr)), nil
			}
			return llm.TextOutput(formatToolError(NewToolError(ErrPermissionDenied, err.Error()))), nil
		}
		if outcome == Cancel {
			return llm.TextOutput(formatToolError(NewToolErrorf(ErrPermissionDenied, "access denied: %s", a.OutputPath))), nil
		}
	}

	// Check if config is available
	if t.config == nil {
		return llm.TextOutput(formatToolError(NewToolError(ErrImageGenFailed, "image provider not configured"))), nil
	}

	// Create image provider
	provider, err := image.NewImageProvider(t.config, t.providerName)
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrImageGenFailed, "failed to create image provider: %v", err))), nil
	}

	var result *image.ImageResult

	// Consolidate input_image and input_images into a single slice
	var inputPaths []string
	if a.InputImage != "" {
		inputPaths = append(inputPaths, a.InputImage)
	}
	inputPaths = append(inputPaths, a.InputImages...)

	// Check if this is an edit or generation
	if len(inputPaths) > 0 {
		// Check read permissions for all input images via approval manager
		if t.approval != nil {
			for _, inputPath := range inputPaths {
				outcome, err := t.approval.CheckPathApproval(ImageGenerateToolName, inputPath, inputPath, false)
				if err != nil {
					if toolErr, ok := err.(*ToolError); ok {
						return llm.TextOutput(formatToolError(toolErr)), nil
					}
					return llm.TextOutput(formatToolError(NewToolError(ErrPermissionDenied, err.Error()))), nil
				}
				if outcome == Cancel {
					return llm.TextOutput(formatToolError(NewToolErrorf(ErrPermissionDenied, "access denied: %s", inputPath))), nil
				}
			}
		}

		// Check if provider supports editing
		if !provider.SupportsEdit() {
			return llm.TextOutput(formatToolError(NewToolErrorf(ErrImageGenFailed, "provider %s does not support image editing", provider.Name()))), nil
		}

		// Check if multi-image is supported when multiple images provided
		if len(inputPaths) > 1 && !provider.SupportsMultiImage() {
			return llm.TextOutput(formatToolError(NewToolErrorf(ErrImageGenFailed, "provider %s does not support multiple input images", provider.Name()))), nil
		}

		// Read all input images
		var inputImages []image.InputImage
		for _, inputPath := range inputPaths {
			inputData, err := os.ReadFile(inputPath)
			if err != nil {
				if os.IsNotExist(err) {
					return llm.TextOutput(formatToolError(NewToolError(ErrFileNotFound, inputPath))), nil
				}
				return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to read input image: %v", err))), nil
			}
			inputImages = append(inputImages, image.InputImage{
				Data: inputData,
				Path: inputPath,
			})
		}

		// Edit image
		result, err = provider.Edit(ctx, image.EditRequest{
			Prompt:      a.Prompt,
			InputImages: inputImages,
		})
		if err != nil {
			return llm.TextOutput(formatToolError(NewToolErrorf(ErrImageGenFailed, "image edit failed: %v", err))), nil
		}
	} else {
		// Generate new image
		result, err = provider.Generate(ctx, image.GenerateRequest{
			Prompt: a.Prompt,
		})
		if err != nil {
			return llm.TextOutput(formatToolError(NewToolErrorf(ErrImageGenFailed, "image generation failed: %v", err))), nil
		}
	}

	// Determine output path
	outputPath := a.OutputPath
	if outputPath == "" {
		// Use default output directory from config
		outputDir := t.config.Image.OutputDir
		if outputDir == "" {
			outputDir = "~/Pictures/term-llm"
		}
		outputPath, err = image.SaveImage(result.Data, outputDir, a.Prompt)
		if err != nil {
			return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to save image: %v", err))), nil
		}
	} else {
		// Ensure parent directory exists
		dir := filepath.Dir(outputPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to create directory: %v", err))), nil
		}

		// Write to specified path
		if err := os.WriteFile(outputPath, result.Data, 0644); err != nil {
			return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to write image: %v", err))), nil
		}
	}

	// Get image dimensions (approximate from data size)
	width, height := estimateImageDimensions(result.Data)

	if t.imageRecorder != nil {
		rec := &memorystore.ImageRecord{
			Agent:      t.agent,
			SessionID:  t.sessionID,
			Prompt:     a.Prompt,
			OutputPath: outputPath,
			MimeType:   result.MimeType,
			Provider:   provider.Name(),
			Width:      width,
			Height:     height,
			FileSize:   len(result.Data),
		}
		_ = t.imageRecorder.RecordImage(ctx, rec)
	}

	// Emit image marker for deferred display (default: true)
	showImage := a.ShowImage == nil || *a.ShowImage

	// Copy to clipboard if requested (default: true)
	copyClipboard := a.CopyToClipboard == nil || *a.CopyToClipboard
	if copyClipboard {
		image.CopyToClipboard(outputPath, result.Data)
	}

	// Build result
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Generated image saved to: %s\n", outputPath))
	sb.WriteString(fmt.Sprintf("Prompt: %s\n", a.Prompt))
	sb.WriteString(fmt.Sprintf("Format: %s\n", result.MimeType))
	sb.WriteString(fmt.Sprintf("Size: %d bytes\n", len(result.Data)))
	if width > 0 && height > 0 {
		sb.WriteString(fmt.Sprintf("Dimensions: ~%dx%d\n", width, height))
	}
	sb.WriteString(fmt.Sprintf("Provider: %s\n", provider.Name()))
	if copyClipboard {
		sb.WriteString("Copied to clipboard: yes\n")
	}

	output := llm.ToolOutput{Content: sb.String()}
	if showImage {
		output.Images = []string{outputPath}
	}

	return output, nil
}

// estimateImageDimensions provides rough estimates based on file size.
// Returns 0,0 if cannot estimate.
func estimateImageDimensions(data []byte) (int, int) {
	// PNG header check for dimensions
	if len(data) > 24 && string(data[1:4]) == "PNG" {
		// PNG dimensions are at bytes 16-23
		width := int(data[16])<<24 | int(data[17])<<16 | int(data[18])<<8 | int(data[19])
		height := int(data[20])<<24 | int(data[21])<<16 | int(data[22])<<8 | int(data[23])
		if width > 0 && width < 10000 && height > 0 && height < 10000 {
			return width, height
		}
	}

	// JPEG header check
	if len(data) > 2 && data[0] == 0xFF && data[1] == 0xD8 {
		// Would need to parse JPEG segments for dimensions
		// Return 0,0 for now
		return 0, 0
	}

	return 0, 0
}

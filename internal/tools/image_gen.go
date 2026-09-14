package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	approval          *ApprovalManager
	config            *config.Config
	toolConfig        *ToolConfig
	providerName      string // Override provider name
	imageRecorder     ImageRecorder
	agent             string
	sessionID         string
	serveMode         bool   // When true, strip terminal-only params from spec/execution
	serveImageBaseURL string // Deprecated compatibility knob; stream/session layers serve output.Images
}

// NewImageGenerateTool creates a new ImageGenerateTool.
func NewImageGenerateTool(approval *ApprovalManager, cfg *config.Config, providerOverride string, recorder ImageRecorder, agent, sessionID string, toolConfigs ...*ToolConfig) *ImageGenerateTool {
	return &ImageGenerateTool{
		approval:      approval,
		config:        cfg,
		toolConfig:    optionalToolConfig(toolConfigs),
		providerName:  providerOverride,
		imageRecorder: recorder,
		agent:         agent,
		sessionID:     sessionID,
	}
}

// ImageGenerateArgs are the arguments for image_generate.
type ImageGenerateArgs struct {
	Prompt      string   `json:"prompt"`
	InputImage  string   `json:"input_image,omitempty"`  // Single path for editing/variation (backward compat)
	InputImages []string `json:"input_images,omitempty"` // Multiple paths for multi-image editing
	Size        string   `json:"size,omitempty"`         // Resolution: "1K", "2K", "4K"
	AspectRatio string   `json:"aspect_ratio,omitempty"` // e.g., "16:9", "4:3"
	Quality     string   `json:"quality,omitempty"`      // auto, low, medium, high, xhigh, max
	Background  string   `json:"background,omitempty"`   // auto, opaque, transparent
	OutputPath  string   `json:"output_path,omitempty"`  // Save location
	ShowImage   *bool    `json:"show_image,omitempty"`   // Display via icat (default: true)
}

func (t *ImageGenerateTool) Spec() llm.ToolSpec {
	props := map[string]interface{}{
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
		"size": map[string]interface{}{
			"type":        "string",
			"enum":        []string{"1K", "2K", "4K"},
			"description": "Image resolution: 1K (default, ~1024px), 2K (~2048px), 4K (~4096px)",
		},
		"aspect_ratio": map[string]interface{}{
			"type":        "string",
			"description": "Aspect ratio, e.g. '1:1', '16:9', '4:3' (default: provider choice)",
		},
		"quality": map[string]interface{}{
			"type":        "string",
			"enum":        image.ValidQualities,
			"description": "Image quality; provider/model support varies",
		},
		"background": map[string]interface{}{
			"type":        "string",
			"enum":        image.ValidBackgrounds,
			"description": "Image background mode; transparent requires provider/model support",
		},
		"output_path": map[string]interface{}{
			"type":        "string",
			"description": "Optional: path to save the image. Omit this — images are automatically saved and displayed. Only specify if the user explicitly requests a specific file path.",
		},
	}
	// Terminal-only params: not useful in serve/web/telegram mode
	if !t.serveMode {
		props["show_image"] = map[string]interface{}{
			"type":        "boolean",
			"description": "Display generated image via terminal (icat) (default: true)",
			"default":     true,
		}
	}
	return llm.ToolSpec{
		Name:        ImageGenerateToolName,
		Description: "Generate an image from a text prompt. Optionally provide an input image for editing/variation.",
		Schema: map[string]interface{}{
			"type":                 "object",
			"properties":           props,
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
	if a.InputImage != "" || len(a.InputImages) > 0 {
		return fmt.Sprintf("Editing image: %s", prompt)
	}
	return fmt.Sprintf("Generating image: %s", prompt)
}

func (t *ImageGenerateTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a ImageGenerateArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, err.Error()))), nil
	}

	if toolErr := validateImageGenerateArgs(a); toolErr != nil {
		return llm.TextOutput(formatToolError(toolErr)), nil
	}

	if a.OutputPath != "" {
		resolvedOutputPath, errOut := t.approveOutputPath(ctx, a)
		if errOut != nil {
			return *errOut, nil
		}
		// Keep the resolved path for downstream comparisons/writes.
		a.OutputPath = resolvedOutputPath
	}

	// Check if config is available
	if t.config == nil {
		return llm.TextOutput(formatToolError(NewToolError(ErrImageGenFailed, "image provider not configured"))), nil
	}

	outputDir := t.config.Image.OutputDir
	if outputDir == "" {
		outputDir = "~/Pictures/term-llm"
	}
	resolvedOutputDir, err := resolveToolPathWithConfig(outputDir, true, t.toolConfig)
	if err != nil {
		return toolPathErrorOutput(err, "failed to resolve output directory"), nil
	}

	if errOut := t.approveOutputDir(ctx, resolvedOutputDir, a.OutputPath); errOut != nil {
		return *errOut, nil
	}

	// Create image provider
	provider, err := image.NewImageProvider(t.config, t.providerName)
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrImageGenFailed, "failed to create image provider: %v", err))), nil
	}

	// Consolidate input_image and input_images into a single slice
	var inputPaths []string
	if a.InputImage != "" {
		inputPaths = append(inputPaths, a.InputImage)
	}
	inputPaths = append(inputPaths, a.InputImages...)

	var result *image.ImageResult
	var opErr *llm.ToolOutput

	// Check if this is an edit or generation
	if len(inputPaths) > 0 {
		resolvedInputPaths, errOut := t.resolveInputImages(ctx, inputPaths, resolvedOutputDir)
		if errOut != nil {
			return *errOut, nil
		}
		result, opErr = t.editImage(ctx, provider, a, resolvedInputPaths)
	} else {
		result, opErr = t.generateImage(ctx, provider, a)
	}
	if opErr != nil {
		return *opErr, nil
	}

	outputPath, servedPath, saveErrOut := t.saveImageResult(a, result, resolvedOutputDir)
	if saveErrOut != nil {
		return *saveErrOut, nil
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

	// Emit image marker for deferred display. In serve mode the web/telegram
	// client owns image rendering and show_image is intentionally hidden from the
	// tool schema, so ignore stale/legacy show_image:false arguments there.
	showImage := t.serveMode || a.ShowImage == nil || *a.ShowImage

	// Build result. Keep model-facing text semantic: the UI receives
	// output.Images out-of-band and owns rendering the image artifact. Avoid
	// returning a public web image URL here, otherwise models tend to embed the
	// same image again in markdown after the client already displayed it.
	output := llm.ToolOutput{Content: t.describeImageResult(a, result, provider.Name(), outputPath, servedPath, width, height)}
	if showImage {
		output.Images = []string{servedPath}
	}

	return output, nil
}

// toolPathErrorOutput renders a path resolution failure for the model. Path
// resolvers return structured ToolErrors, which are kept verbatim; any other
// error keeps the caller's context prefix.
func toolPathErrorOutput(err error, context string) llm.ToolOutput {
	if toolErr, ok := err.(*ToolError); ok {
		return llm.TextOutput(formatToolError(toolErr))
	}
	return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "%s: %v", context, err)))
}

// validateImageGenerateArgs applies the prompt and option checks in their
// original order and returns the first failure.
func validateImageGenerateArgs(a ImageGenerateArgs) *ToolError {
	if a.Prompt == "" {
		return NewToolError(ErrInvalidParams, "prompt is required")
	}
	if err := image.ValidateSize(a.Size); err != nil {
		return NewToolError(ErrInvalidParams, err.Error())
	}
	if err := image.ValidateAspectRatio(a.AspectRatio); err != nil {
		return NewToolError(ErrInvalidParams, err.Error())
	}
	if err := image.ValidateQuality(a.Quality); err != nil {
		return NewToolError(ErrInvalidParams, err.Error())
	}
	if err := image.ValidateBackground(a.Background); err != nil {
		return NewToolError(ErrInvalidParams, err.Error())
	}
	return nil
}

// approveOutputPath resolves a requested output path and requires read/write
// approval for it. The resolved path is returned for downstream comparisons and
// writes; a non-nil output means the caller must return it to the model.
func (t *ImageGenerateTool) approveOutputPath(ctx context.Context, a ImageGenerateArgs) (string, *llm.ToolOutput) {
	resolvedOutputPath, err := resolveToolPathWithConfig(a.OutputPath, true, t.toolConfig)
	if err != nil {
		out := toolPathErrorOutput(err, "failed to resolve output path")
		return "", &out
	}
	if t.approval != nil {
		outcome, err := t.approval.CheckPathApprovalWithContext(ctx, ImageGenerateToolName, resolvedOutputPath, a.OutputPath, true)
		if err != nil {
			out := pathApprovalErrorOutput("", err)
			return "", &out
		}
		if outcome == Cancel {
			out := pathApprovalErrorOutput("", NewToolErrorf(ErrPermissionDenied, "access denied: %s", a.OutputPath))
			return "", &out
		}
	}
	return resolvedOutputPath, nil
}

// approveOutputDir requires approval for the configured output directory when
// the resolved output path does not already live in it.
func (t *ImageGenerateTool) approveOutputDir(ctx context.Context, resolvedOutputDir, outputPath string) *llm.ToolOutput {
	if t.approval == nil {
		return nil
	}
	needOutputDirApproval := outputPath == "" || filepath.Clean(filepath.Dir(outputPath)) != filepath.Clean(resolvedOutputDir)
	if !needOutputDirApproval {
		return nil
	}
	outcome, err := t.approval.CheckPathApprovalWithContext(ctx, ImageGenerateToolName, resolvedOutputDir, resolvedOutputDir, true)
	if err != nil {
		out := pathApprovalErrorOutput("", err)
		return &out
	}
	if outcome == Cancel {
		out := pathApprovalErrorOutput("", NewToolErrorf(ErrPermissionDenied, "access denied: %s", resolvedOutputDir))
		return &out
	}
	return nil
}

// resolveInputImages resolves each input image path, consulting the approval
// manager when one is configured. Paths inside the output directory are
// auto-approved; everything else is re-resolved only after approval succeeds.
func (t *ImageGenerateTool) resolveInputImages(ctx context.Context, inputPaths []string, resolvedOutputDir string) ([]string, *llm.ToolOutput) {
	resolvedInputPaths := make([]string, 0, len(inputPaths))
	if t.approval == nil {
		for _, inputPath := range inputPaths {
			resolvedInput, err := resolveToolPathWithConfig(inputPath, false, t.toolConfig)
			if err != nil {
				out := toolPathErrorOutput(err, "failed to resolve input image")
				return nil, &out
			}
			resolvedInputPaths = append(resolvedInputPaths, resolvedInput)
		}
		return resolvedInputPaths, nil
	}

	debug := t.approval.DebugApproval
	for _, inputPath := range inputPaths {
		resolvedInput, inputErr := resolveToolPathWithConfig(inputPath, false, t.toolConfig)
		if inputErr == nil && strings.HasPrefix(resolvedInput, resolvedOutputDir+string(filepath.Separator)) {
			if debug {
				log.Printf("[image_generate] auto-approved input %q (inside output dir %q)", inputPath, resolvedOutputDir)
			}
			resolvedInputPaths = append(resolvedInputPaths, resolvedInput)
			continue
		}
		if debug && inputErr != nil {
			log.Printf("[image_generate] resolveToolPath input=%v — falling through to approval check", inputErr)
		}

		outcome, err := t.approval.CheckPathApprovalWithContext(ctx, ImageGenerateToolName, inputPath, inputPath, false)
		if debug {
			log.Printf("[image_generate] CheckPathApproval input=%q → outcome=%v err=%v", inputPath, outcome, err)
		}
		if err != nil {
			out := pathApprovalErrorOutput("", err)
			return nil, &out
		}
		if outcome == Cancel {
			out := pathApprovalErrorOutput("", NewToolErrorf(ErrPermissionDenied, "access denied: %s", inputPath))
			return nil, &out
		}

		resolvedInput, err = resolveToolPathWithConfig(inputPath, false, t.toolConfig)
		if err != nil {
			out := toolPathErrorOutput(err, "failed to resolve input image")
			return nil, &out
		}
		resolvedInputPaths = append(resolvedInputPaths, resolvedInput)
	}
	return resolvedInputPaths, nil
}

// editImage reads the approved input images and asks the provider to edit them.
func (t *ImageGenerateTool) editImage(ctx context.Context, provider image.ImageProvider, a ImageGenerateArgs, inputPaths []string) (*image.ImageResult, *llm.ToolOutput) {
	// Check if provider supports editing
	if !provider.SupportsEdit() {
		out := llm.TextOutput(formatToolError(NewToolErrorf(ErrImageGenFailed, "provider %s does not support image editing", provider.Name())))
		return nil, &out
	}

	// Check if multi-image is supported when multiple images provided
	if len(inputPaths) > 1 && !provider.SupportsMultiImage() {
		out := llm.TextOutput(formatToolError(NewToolErrorf(ErrImageGenFailed, "provider %s does not support multiple input images", provider.Name())))
		return nil, &out
	}

	// Read all input images
	var inputImages []image.InputImage
	for _, inputPath := range inputPaths {
		inputData, err := os.ReadFile(inputPath)
		if err != nil {
			if os.IsNotExist(err) {
				out := llm.TextOutput(formatToolError(NewToolError(ErrFileNotFound, inputPath)))
				return nil, &out
			}
			out := llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to read input image: %v", err)))
			return nil, &out
		}
		inputImages = append(inputImages, image.InputImage{
			Data: inputData,
			Path: inputPath,
		})
	}

	// Edit image
	result, err := provider.Edit(ctx, image.EditRequest{
		Prompt:      a.Prompt,
		InputImages: inputImages,
		Size:        a.Size,
		AspectRatio: a.AspectRatio,
		Quality:     a.Quality,
		Background:  a.Background,
	})
	if err != nil {
		out := llm.TextOutput(formatToolError(NewToolErrorf(ErrImageGenFailed, "image edit failed: %v", err)))
		return nil, &out
	}
	return result, nil
}

// generateImage asks the provider for a new image.
func (t *ImageGenerateTool) generateImage(ctx context.Context, provider image.ImageProvider, a ImageGenerateArgs) (*image.ImageResult, *llm.ToolOutput) {
	result, err := provider.Generate(ctx, image.GenerateRequest{
		Prompt:      a.Prompt,
		Size:        a.Size,
		AspectRatio: a.AspectRatio,
		Quality:     a.Quality,
		Background:  a.Background,
	})
	if err != nil {
		out := llm.TextOutput(formatToolError(NewToolErrorf(ErrImageGenFailed, "image generation failed: %v", err)))
		return nil, &out
	}
	return result, nil
}

// saveImageResult writes the image to the requested path, or into the output
// directory when none was requested, and returns the saved path plus the path
// served to clients.
func (t *ImageGenerateTool) saveImageResult(a ImageGenerateArgs, result *image.ImageResult, resolvedOutputDir string) (outputPath, servedPath string, errOut *llm.ToolOutput) {
	outputPath = a.OutputPath

	if outputPath == "" {
		saved, err := image.SaveImage(result.Data, resolvedOutputDir, a.Prompt)
		if err != nil {
			out := llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to save image: %v", err)))
			return "", "", &out
		}
		return saved, saved, nil
	}

	resolvedOutputPath, err := resolveToolPathWithConfig(outputPath, true, t.toolConfig)
	if err != nil {
		out := toolPathErrorOutput(err, "failed to resolve output path")
		return "", "", &out
	}
	outputPath = resolvedOutputPath

	// Write to requested location
	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		out := llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to create directory: %v", err)))
		return "", "", &out
	}
	if err := os.WriteFile(outputPath, result.Data, 0644); err != nil {
		out := llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to write image: %v", err)))
		return "", "", &out
	}

	// Also copy into outputDir so the web UI can serve it
	servedPath, saveErr := image.SaveImage(result.Data, resolvedOutputDir, a.Prompt)
	if saveErr != nil {
		// Non-fatal: fall back to outputPath (web UI may not work but file is saved)
		servedPath = outputPath
	}
	return outputPath, servedPath, nil
}

// describeImageResult builds the model-facing result text for a saved image.
func (t *ImageGenerateTool) describeImageResult(a ImageGenerateArgs, result *image.ImageResult, providerName, outputPath, servedPath string, width, height int) string {
	var sb strings.Builder
	if t.serveMode {
		sb.WriteString("Generated image successfully.\n")
		sb.WriteString("The image has already been displayed to the user by the client UI.\n")
	} else {
		sb.WriteString(fmt.Sprintf("Generated image saved to: %s\n", outputPath))
	}
	if servedPath != "" {
		sb.WriteString(fmt.Sprintf("For follow-up edits, use this path as input_image: %s\n", servedPath))
	}
	if a.OutputPath != "" && outputPath != servedPath {
		sb.WriteString(fmt.Sprintf("Requested save path: %s\n", outputPath))
	}
	sb.WriteString("Do not embed or repeat the image path in markdown unless the user explicitly asks for the file location.\n")
	sb.WriteString(fmt.Sprintf("Prompt: %s\n", a.Prompt))
	sb.WriteString(fmt.Sprintf("Format: %s\n", result.MimeType))
	sb.WriteString(fmt.Sprintf("Size: %d bytes\n", len(result.Data)))
	if width > 0 && height > 0 {
		sb.WriteString(fmt.Sprintf("Dimensions: ~%dx%d\n", width, height))
	}
	sb.WriteString(fmt.Sprintf("Provider: %s\n", providerName))
	return sb.String()
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

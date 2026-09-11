package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

func TestImageGenAutoApprove_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require special privileges on Windows")
	}

	// Set up directory structure:
	//   outputDir/           - the configured image output directory
	//   outputDir/real.png   - a real image file inside outputDir
	//   outsideDir/          - a directory outside the output directory
	//   outsideDir/secret.png - a sensitive file outside the output directory
	//   outputDir/link.png   - a symlink inside outputDir pointing to secret.png

	tmpDir := t.TempDir()
	// Resolve symlinks on tmpDir itself (macOS /var -> /private/var)
	tmpDir, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("failed to resolve tmpDir symlinks: %v", err)
	}

	outputDir := filepath.Join(tmpDir, "outputDir")
	outsideDir := filepath.Join(tmpDir, "outsideDir")
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		t.Fatalf("mkdir outputDir: %v", err)
	}
	if err := os.MkdirAll(outsideDir, 0755); err != nil {
		t.Fatalf("mkdir outsideDir: %v", err)
	}

	// Create a real image in the output dir
	realImage := filepath.Join(outputDir, "real.png")
	if err := os.WriteFile(realImage, []byte("fake png data"), 0644); err != nil {
		t.Fatalf("write real.png: %v", err)
	}

	// Create a sensitive file outside the output dir
	secretFile := filepath.Join(outsideDir, "secret.png")
	if err := os.WriteFile(secretFile, []byte("sensitive data"), 0644); err != nil {
		t.Fatalf("write secret.png: %v", err)
	}

	// Create a symlink inside the output dir pointing to the secret file
	symlinkPath := filepath.Join(outputDir, "link.png")
	if err := os.Symlink(secretFile, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	// Set up an approval manager that denies everything not auto-approved.
	// If the symlink escape works, the auto-approve logic will skip the
	// approval check and the test will NOT see a denial.
	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)
	mgr.dirCache.Set(outputDir, ProceedAlways, true)

	var approvalRequested []string
	mgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, workDir string) (ApprovalResult, error) {
		approvalRequested = append(approvalRequested, path)
		return ApprovalResult{Choice: ApprovalChoiceDeny}, nil
	}

	cfg := &config.Config{
		Image: config.ImageConfig{
			Provider:  "debug",
			OutputDir: outputDir,
		},
	}

	tool := NewImageGenerateTool(mgr, cfg, "debug", nil, "", "")

	// Test 1: A symlink inside outputDir pointing outside should NOT be auto-approved.
	// The approval manager should be consulted and deny access.
	args, _ := json.Marshal(ImageGenerateArgs{
		Prompt:     "test prompt",
		InputImage: symlinkPath,
	})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !strings.Contains(out.Content, "SYMLINK_ESCAPE") && !strings.Contains(out.Content, "access denied") {
		t.Errorf("expected symlink escape denial, got: %s", out.Content)
	}
	if len(approvalRequested) != 0 {
		t.Errorf("expected symlink escape to fail before prompting, got prompt(s) for: %v", approvalRequested)
	}

	// Test 2: A real file inside outputDir should be auto-approved (no prompt).
	approvalRequested = nil
	args2, _ := json.Marshal(ImageGenerateArgs{
		Prompt:     "test prompt",
		InputImage: realImage,
	})

	out2, err := tool.Execute(context.Background(), args2)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(approvalRequested) > 0 {
		t.Errorf("expected real file inside output dir to be auto-approved, but approval was requested for: %v", approvalRequested)
	}
	// The debug provider should succeed (it generates a random image)
	if strings.Contains(out2.Content, "permission_denied") || strings.Contains(out2.Content, "access denied") {
		t.Errorf("expected success for real file in output dir, got: %s", out2.Content)
	}
}

func TestImageGenerateToolRejectsInvalidImageOptions(t *testing.T) {
	cfg := &config.Config{Image: config.ImageConfig{Provider: "debug", OutputDir: t.TempDir()}}
	tool := NewImageGenerateTool(nil, cfg, "debug", nil, "", "")
	for _, args := range []ImageGenerateArgs{
		{Prompt: "test", Quality: "ultra"},
		{Prompt: "test", Background: "checkerboard"},
		{Prompt: "test", AspectRatio: "16:0"},
	} {
		raw, _ := json.Marshal(args)
		out, err := tool.Execute(context.Background(), raw)
		if err != nil {
			t.Fatalf("Execute returned error: %v", err)
		}
		if !strings.Contains(out.Content, "INVALID_PARAMS") {
			t.Fatalf("expected invalid params for %+v, got %q", args, out.Content)
		}
	}
}

func TestImageGenerateTool_RequiresApprovalForDefaultOutputDir(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{
		Image: config.ImageConfig{
			Provider:  "debug",
			OutputDir: filepath.Join(tmpDir, "generated"),
		},
	}

	mgr := NewApprovalManager(NewToolPermissions())
	mgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, workDir string) (ApprovalResult, error) {
		return ApprovalResult{Choice: ApprovalChoiceDeny}, nil
	}

	tool := NewImageGenerateTool(mgr, cfg, "debug", nil, "", "")
	args, _ := json.Marshal(ImageGenerateArgs{Prompt: "denied"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !strings.Contains(strings.ToLower(out.Content), "access denied") {
		t.Fatalf("expected output-dir permission denial, got: %s", out.Content)
	}
}

func TestImageGenerateTool_ServeModeReturnsArtifactInstructions(t *testing.T) {
	tmpDir := t.TempDir()
	tmpDir, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("resolve tmpDir: %v", err)
	}

	cfg := &config.Config{
		Image: config.ImageConfig{
			Provider:  "debug",
			OutputDir: tmpDir,
		},
	}
	tool := NewImageGenerateTool(nil, cfg, "debug", nil, "", "")
	tool.serveMode = true
	tool.serveImageBaseURL = "/ui/images/"

	args, _ := json.Marshal(ImageGenerateArgs{Prompt: "a cat"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	// Web platform: model-facing text should describe the already-rendered
	// artifact and keep a local path for follow-up edits, not invite markdown
	// re-embedding with a web URL.
	if strings.Contains(out.Content, "Generated image URL:") || strings.Contains(out.Content, "/ui/images/") {
		t.Errorf("unexpected web image URL in model-facing content, got:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "already been displayed") {
		t.Errorf("expected displayed-to-user instruction, got:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "use this path as input_image: "+tmpDir) {
		t.Errorf("expected local input_image path for follow-up edits, got:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "Do not embed or repeat") {
		t.Errorf("expected no-reembed instruction, got:\n%s", out.Content)
	}
}

func TestImageGenerateTool_ServeModeIgnoresShowImageFalse(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{
		Image: config.ImageConfig{
			Provider:  "debug",
			OutputDir: tmpDir,
		},
	}
	tool := NewImageGenerateTool(nil, cfg, "debug", nil, "", "")
	tool.serveMode = true

	showImage := false
	args, _ := json.Marshal(ImageGenerateArgs{Prompt: "a cat", ShowImage: &showImage})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if len(out.Images) != 1 || out.Images[0] == "" {
		t.Fatalf("serve mode should always emit an image artifact, got %#v", out.Images)
	}
	if !strings.Contains(out.Content, "already been displayed") {
		t.Errorf("expected serve-mode displayed-to-user instruction, got:\n%s", out.Content)
	}
}

func TestImageGenerateTool_ServeModeExplicitOutputPath(t *testing.T) {
	tmpDir := t.TempDir()
	tmpDir, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("resolve tmpDir: %v", err)
	}

	explicitPath := filepath.Join(tmpDir, "custom-output.png")

	cfg := &config.Config{
		Image: config.ImageConfig{
			Provider:  "debug",
			OutputDir: tmpDir,
		},
	}
	tool := NewImageGenerateTool(nil, cfg, "debug", nil, "", "")
	tool.serveMode = true
	tool.serveImageBaseURL = "/ui/images/"

	args, _ := json.Marshal(ImageGenerateArgs{Prompt: "a cat", OutputPath: explicitPath})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	// Should have no web URL in model-facing content. The explicit save path is
	// still reported, while a generated copy under the output directory is exposed
	// as the preferred input_image path for follow-up edits.
	if strings.Contains(out.Content, "Generated image URL:") || strings.Contains(out.Content, "/ui/images/") {
		t.Errorf("unexpected web image URL in content, got:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "Requested save path: "+explicitPath) {
		t.Errorf("expected explicit output path in content, got:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "use this path as input_image: "+tmpDir) {
		t.Errorf("expected generated image path for follow-up edits, got:\n%s", out.Content)
	}
}

func TestImageGenerateTool_TelegramServeModeNoWebURL(t *testing.T) {
	tmpDir := t.TempDir()
	tmpDir, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("resolve tmpDir: %v", err)
	}

	cfg := &config.Config{
		Image: config.ImageConfig{
			Provider:  "debug",
			OutputDir: tmpDir,
		},
	}
	// Telegram: serveMode=true but no imageBaseURL
	tool := NewImageGenerateTool(nil, cfg, "debug", nil, "", "")
	tool.serveMode = true

	args, _ := json.Marshal(ImageGenerateArgs{Prompt: "a dog"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	// Telegram: no web URL, but the model still gets a local path for follow-up edits.
	if strings.Contains(out.Content, "Generated image URL:") {
		t.Errorf("unexpected image URL in telegram serve mode, got:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "use this path as input_image: "+tmpDir) {
		t.Errorf("expected local input_image path in content, got:\n%s", out.Content)
	}
}

func TestImageGenerateTool_NonServeModeNoDiskPathOnly(t *testing.T) {
	tmpDir := t.TempDir()
	tmpDir, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("resolve tmpDir: %v", err)
	}

	cfg := &config.Config{
		Image: config.ImageConfig{
			Provider:  "debug",
			OutputDir: tmpDir,
		},
	}
	tool := NewImageGenerateTool(nil, cfg, "debug", nil, "", "")

	args, _ := json.Marshal(ImageGenerateArgs{Prompt: "a dog"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if !strings.Contains(out.Content, "Generated image saved to:") {
		t.Errorf("expected 'saved to' with disk path in content, got:\n%s", out.Content)
	}
	if strings.Contains(out.Content, "Generated image URL:") {
		t.Errorf("unexpected image URL in non-serve mode, got:\n%s", out.Content)
	}
}

func TestSetServeMode_SetsImageBaseURL(t *testing.T) {
	cfg := &config.Config{
		Image: config.ImageConfig{
			Provider: "debug",
		},
	}
	toolConfig := &ToolConfig{
		Enabled: []string{ImageGenerateToolName},
	}
	registry, err := NewLocalToolRegistry(toolConfig, cfg, nil)
	if err != nil {
		t.Fatalf("NewLocalToolRegistry: %v", err)
	}

	// Web platform: imageBaseURL set
	registry.SetServeMode(true, "/ui/images/")

	tool, ok := registry.Get(ImageGenerateToolName)
	if !ok {
		t.Fatal("image_generate tool not found")
	}
	ig := tool.(*ImageGenerateTool)
	if !ig.serveMode {
		t.Error("expected serveMode to be true")
	}
	if ig.serveImageBaseURL != "/ui/images/" {
		t.Errorf("expected serveImageBaseURL = %q, got %q", "/ui/images/", ig.serveImageBaseURL)
	}

	// Telegram platform: serveMode on but no imageBaseURL
	registry.SetServeMode(true, "")
	if !ig.serveMode {
		t.Error("expected serveMode to remain true for telegram")
	}
	if ig.serveImageBaseURL != "" {
		t.Errorf("expected serveImageBaseURL = %q for telegram, got %q", "", ig.serveImageBaseURL)
	}

	// Disable serve mode entirely
	registry.SetServeMode(false, "")
	if ig.serveMode {
		t.Error("expected serveMode to be false after disabling")
	}
}

func TestImageGenerateTool_ServeModeStripsTerminalParams(t *testing.T) {
	cfg := &config.Config{}
	tool := NewImageGenerateTool(nil, cfg, "", nil, "", "")

	// Default: exposes display control, but never clipboard access.
	spec := tool.Spec()
	props := spec.Schema["properties"].(map[string]interface{})
	if _, ok := props["copy_to_clipboard"]; ok {
		t.Error("clipboard access must not be exposed in tool specs")
	}
	if _, ok := props["show_image"]; !ok {
		t.Error("expected show_image in default spec")
	}

	// Serve mode: stripped
	tool.serveMode = true
	spec = tool.Spec()
	props = spec.Schema["properties"].(map[string]interface{})
	if _, ok := props["copy_to_clipboard"]; ok {
		t.Error("copy_to_clipboard should be stripped in serve mode")
	}
	if _, ok := props["show_image"]; ok {
		t.Error("show_image should be stripped in serve mode")
	}
	// Core params still present
	if _, ok := props["prompt"]; !ok {
		t.Error("prompt should still be present in serve mode")
	}
	if _, ok := props["input_image"]; !ok {
		t.Error("input_image should still be present in serve mode")
	}
}

func TestShowImageTool_NeverExposesClipboard(t *testing.T) {
	tool := NewShowImageTool(nil)

	// Terminal mode must not expose clipboard access.
	spec := tool.Spec()
	props := spec.Schema["properties"].(map[string]any)
	if _, ok := props["copy_to_clipboard"]; ok {
		t.Error("clipboard access must not be exposed in tool specs")
	}
	if strings.Contains(spec.Description, "clipboard") {
		t.Error("clipboard should not be mentioned in default description")
	}

	// Serve mode: stripped
	tool.serveMode = true
	spec = tool.Spec()
	props = spec.Schema["properties"].(map[string]any)
	if _, ok := props["copy_to_clipboard"]; ok {
		t.Error("copy_to_clipboard should be stripped in serve mode")
	}
	if strings.Contains(spec.Description, "clipboard") {
		t.Error("clipboard should not be mentioned in serve mode description")
	}
	// Core params still present
	if _, ok := props["file_path"]; !ok {
		t.Error("file_path should still be present in serve mode")
	}
}

func TestImageGenAutoApprove_EvalSymlinksFailure(t *testing.T) {
	// When EvalSymlinks fails (e.g., file doesn't exist), the code should
	// NOT auto-approve. The path falls through to CheckPathApproval which
	// will error because the file doesn't exist (FILE_NOT_FOUND from
	// canonicalizePath). The key point is that a non-existent path inside
	// the output dir is never silently auto-approved.
	tmpDir := t.TempDir()
	tmpDir, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("resolve tmpDir: %v", err)
	}

	outputDir := filepath.Join(tmpDir, "outputDir")
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Reference a non-existent file inside the output dir
	nonexistent := filepath.Join(outputDir, "does-not-exist.png")

	perms := NewToolPermissions()
	mgr := NewApprovalManager(perms)

	cfg := &config.Config{
		Image: config.ImageConfig{
			Provider:  "debug",
			OutputDir: outputDir,
		},
	}

	tool := NewImageGenerateTool(mgr, cfg, "debug", nil, "", "")

	args, _ := json.Marshal(ImageGenerateArgs{
		Prompt:     "test prompt",
		InputImage: nonexistent,
	})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	// Should NOT succeed - the file doesn't exist and should not be auto-approved
	if !strings.Contains(out.Content, "FILE_NOT_FOUND") && !strings.Contains(out.Content, "PERMISSION_DENIED") {
		t.Errorf("expected error for non-existent file (not auto-approved), got: %s", out.Content)
	}
}

// Tool calls must not mutate the clipboard, even when an old conversation
// supplies the removed copy_to_clipboard argument.
func TestImageToolsDoNotCopyToClipboard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("clipboard command sentinels use shell scripts")
	}
	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "clipboard-called")
	t.Setenv("CLIPBOARD_TEST_MARKER", marker)
	for _, name := range []string{"wl-copy", "xclip", "osascript", "pbcopy"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nprintf called > \"$CLIPBOARD_TEST_MARKER\"\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)

	for _, serveMode := range []bool{false, true} {
		for _, clipboardArg := range []string{"omitted", "false", "true"} {
			t.Run(fmt.Sprintf("serve=%t/clipboard=%s", serveMode, clipboardArg), func(t *testing.T) {
				outputDir := t.TempDir()
				cfg := &config.Config{Image: config.ImageConfig{Provider: "debug", OutputDir: outputDir}}
				generate := NewImageGenerateTool(nil, cfg, "debug", nil, "", "")
				generate.serveMode = serveMode
				show := NewShowImageTool(nil)
				show.serveMode = serveMode
				inputPath := filepath.Join(outputDir, "input.png")
				if err := os.WriteFile(inputPath, []byte("image data"), 0600); err != nil {
					t.Fatal(err)
				}
				for _, tool := range []llm.Tool{generate, show} {
					args := map[string]any{"prompt": "a cat", "file_path": inputPath}
					if clipboardArg != "omitted" {
						args["copy_to_clipboard"] = clipboardArg == "true"
					}
					raw, err := json.Marshal(args)
					if err != nil {
						t.Fatal(err)
					}
					out, err := tool.Execute(context.Background(), raw)
					if err != nil {
						t.Fatal(err)
					}
					if len(out.Images) == 0 {
						t.Fatalf("%s did not return an image: %s", tool.Spec().Name, out.Content)
					}
					if strings.Contains(out.Content, "Copied to clipboard:") {
						t.Errorf("unexpected clipboard claim: %s", out.Content)
					}
					if _, err := os.Stat(marker); !os.IsNotExist(err) {
						t.Fatalf("%s invoked a clipboard command (stat error: %v)", tool.Spec().Name, err)
					}
				}
			})
		}
	}
}

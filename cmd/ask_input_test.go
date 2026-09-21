package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/spf13/cobra"

	"github.com/samsaffron/term-llm/internal/config"
	inputpkg "github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/prompt"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func testAskConfig() config.AskConfig {
	return config.AskConfig{StdinInlineMaxBytes: 10, StdinMaxBytes: 1024}
}

func TestPrepareAskInputUsesCobraInputAndKeepsSmallTextInline(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader("small text"))
	reader, hasStdin := askCommandInput(cmd)
	if !hasStdin {
		t.Fatal("SetIn reader was not recognized as piped stdin")
	}

	got, err := prepareAskInput("question", nil, reader, hasStdin, testAskConfig())
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	if len(got.stagedPaths) != 0 {
		t.Fatalf("staged paths = %v, want none", got.stagedPaths)
	}
	want := prompt.AskUserPrompt("question", nil, "small text")
	if len(got.message.Parts) != 1 || got.message.Parts[0].Type != llm.PartText || got.message.Parts[0].Text != want {
		t.Fatalf("message = %#v, want exact legacy prompt %q", got.message, want)
	}
}

func TestPrepareAskInputAllowsEmptyQuestionWithPipedText(t *testing.T) {
	got, err := prepareAskInput("", nil, strings.NewReader("piped only"), true, testAskConfig())
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	want := prompt.AskUserPrompt("", nil, "piped only")
	if len(got.message.Parts) != 1 || got.message.Parts[0].Text != want {
		t.Fatalf("message = %#v, want %q", got.message, want)
	}
}

func TestPrepareAskInputRejectsEmptyPipeWithoutQuestion(t *testing.T) {
	_, err := prepareAskInput("", nil, strings.NewReader(""), true, testAskConfig())
	if err == nil || !strings.Contains(err.Error(), "stdin was empty") {
		t.Fatalf("error = %v, want empty stdin validation", err)
	}
}

func TestPrepareAskInputUsesAgentDefaultForEmptyPipe(t *testing.T) {
	got, err := prepareAskInput("", nil, strings.NewReader(""), true, testAskConfig(), "inspect the project")
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	if got.question != "inspect the project" || len(got.message.Parts) != 1 || got.message.Parts[0].Text != "inspect the project" {
		t.Fatalf("prepared input = %#v", got)
	}
}

func TestPrepareAskInputUsesAgentDefaultWithPipedContent(t *testing.T) {
	got, err := prepareAskInput("", nil, strings.NewReader("pipe"), true, testAskConfig(), "inspect the input")
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	if got.question != "inspect the input" || !strings.HasSuffix(got.message.Parts[0].Text, "\n\ninspect the input") {
		t.Fatalf("prepared input = %#v", got)
	}
}

func TestPrepareAskInputStagesLargeTextAfterFileContext(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	contextPath := filepath.Join(t.TempDir(), "context.txt")
	if err := os.WriteFile(contextPath, []byte("ctx"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := "large text body"
	got, err := prepareAskInput("question", []string{contextPath}, strings.NewReader(input), true, testAskConfig())
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	t.Cleanup(func() { cleanupPreparedAskInput(got) })
	if len(got.message.Parts) != 3 {
		t.Fatalf("parts = %#v, want context, file, question", got.message.Parts)
	}
	if got.message.Parts[0].Type != llm.PartText || !strings.Contains(got.message.Parts[0].Text, "ctx") {
		t.Fatalf("first part = %#v, want embedded -f context", got.message.Parts[0])
	}
	part := got.message.Parts[1]
	if part.Type != llm.PartFile || part.FileData == nil {
		t.Fatalf("attachment = %#v, want file part", part)
	}
	if part.FileData.Filename != "stdin.txt" || part.FileData.MediaType != "text/plain" || part.FileData.SizeBytes != int64(len(input)) {
		t.Fatalf("file metadata = %#v", part.FileData)
	}
	if part.FileData.Base64 != "" {
		t.Fatalf("large stdin was duplicated into base64: %q", part.FileData.Base64)
	}
	if part.Text != llm.FormatUploadedFileNotice("stdin.txt", "text/plain", part.FilePath, int64(len(input))) {
		t.Fatalf("file notice = %q", part.Text)
	}
	if got.message.Parts[2].Type != llm.PartText || got.message.Parts[2].Text != "question" {
		t.Fatalf("question part = %#v", got.message.Parts[2])
	}
	raw, err := os.ReadFile(part.FilePath)
	if err != nil || string(raw) != input {
		t.Fatalf("staged content = %q, err = %v", raw, err)
	}
	info, err := os.Stat(part.FilePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("staged mode = %v, err = %v", info.Mode().Perm(), err)
	}
	if !pathWithinDir(part.FilePath, serveUploadsDir()) {
		t.Fatalf("staged path %q is outside uploads", part.FilePath)
	}
}

func TestPrepareAskInputClassifiesTinyPNGAndPreservesOriginal(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	raw := tinyAskPNG(t)
	got, err := prepareAskInput("describe", nil, bytes.NewReader(raw), true, testAskConfig())
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	t.Cleanup(func() { cleanupPreparedAskInput(got) })
	if len(got.message.Parts) != 2 || got.message.Parts[0].Type != llm.PartImage {
		t.Fatalf("message parts = %#v", got.message.Parts)
	}
	part := got.message.Parts[0]
	if filepath.Ext(part.ImagePath) != ".png" {
		t.Fatalf("image path = %q, want .png", part.ImagePath)
	}
	if part.ImageData == nil || part.ImageData.MediaType != "image/png" || part.ImageData.Width != 2 || part.ImageData.Height != 1 {
		t.Fatalf("image metadata = %#v", part.ImageData)
	}
	decoded, err := base64.StdEncoding.DecodeString(part.ImageData.Base64)
	if err != nil || !bytes.Equal(decoded, raw) {
		t.Fatalf("inline image differs from original: err=%v", err)
	}
	saved, err := os.ReadFile(part.ImagePath)
	if err != nil || !bytes.Equal(saved, raw) {
		t.Fatalf("saved original differs: err=%v", err)
	}
	settings := SessionSettings{}
	addAskAutoTools(&settings, []llm.Message{got.message})
	if strings.Join(settings.AutoTools, ",") != tools.ReadFileToolName+","+tools.ViewImageToolName {
		t.Fatalf("AutoTools = %v", settings.AutoTools)
	}
}

func TestPrepareAskInputResizesLargeImageWithProviderDimensions(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	img := image.NewNRGBA(image.Rect(0, 0, 1024, 1024))
	var state uint32 = 1
	for i := range img.Pix {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		img.Pix[i] = byte(state)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	if encoded.Len() <= maxLLMImageBytes {
		t.Fatalf("test PNG is only %d bytes", encoded.Len())
	}
	cfg := testAskConfig()
	cfg.StdinMaxBytes = int64(encoded.Len() + 1)

	got, err := prepareAskInput("describe", nil, bytes.NewReader(encoded.Bytes()), true, cfg)
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	t.Cleanup(func() { cleanupPreparedAskInput(got) })
	part := got.message.Parts[0]
	if part.ImageData == nil || part.ImageData.MediaType != "image/jpeg" {
		t.Fatalf("image metadata = %#v, want resized JPEG", part.ImageData)
	}
	providerBytes, err := base64.StdEncoding.DecodeString(part.ImageData.Base64)
	if err != nil {
		t.Fatal(err)
	}
	width, height := imageDisplayDimensions(providerBytes)
	if part.ImageData.Width != width || part.ImageData.Height != height || width >= 1024 || height >= 1024 {
		t.Fatalf("metadata dimensions = %dx%d, provider bytes = %dx%d", part.ImageData.Width, part.ImageData.Height, width, height)
	}
}

func TestPrepareAskInputDetectsBinaryMIMEAndExtension(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	raw := []byte("%PDF-1.7\n%\x00binary")
	got, err := prepareAskInput("inspect", nil, bytes.NewReader(raw), true, testAskConfig())
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	t.Cleanup(func() { cleanupPreparedAskInput(got) })
	part := got.message.Parts[0]
	if part.Type != llm.PartFile || part.FileData == nil {
		t.Fatalf("part = %#v", part)
	}
	if part.FileData.MediaType != "application/pdf" || part.FileData.Filename != "stdin.pdf" || filepath.Ext(part.FilePath) != ".pdf" {
		t.Fatalf("metadata/path = %#v %q", part.FileData, part.FilePath)
	}
}

func TestPrepareAskInputRejectsMaxPlusOneWithoutStaging(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	cfg := testAskConfig()
	cfg.StdinMaxBytes = 5
	cfg.StdinInlineMaxBytes = 5
	_, err := prepareAskInput("question", nil, strings.NewReader("123456"), true, cfg)
	if err == nil || !strings.Contains(err.Error(), "5 bytes") {
		t.Fatalf("error = %v, want size limit", err)
	}
	entries, readErr := os.ReadDir(filepath.Join(dataHome, "term-llm", "uploads"))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("oversize input left staged files: %v", entries)
	}
}

func TestPrepareAskInputKeepsSmallFilesLegacyCompatible(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.txt")
	second := filepath.Join(dir, "second.txt")
	if err := os.WriteFile(first, []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("beta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testAskConfig()
	cfg.StdinInlineMaxBytes = 100

	got, err := prepareAskInput("question", []string{first, second}, nil, false, cfg)
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	if len(got.stagedPaths) != 0 {
		t.Fatalf("staged paths = %v, want none", got.stagedPaths)
	}
	want := prompt.AskUserPrompt("question", []inputpkg.FileContent{
		{Path: first, Content: "alpha"},
		{Path: second, Content: "beta\n"},
	}, "")
	if len(got.message.Parts) != 1 || got.message.Parts[0].Type != llm.PartText || got.message.Parts[0].Text != want {
		t.Fatalf("message = %#v, want exact legacy prompt %q", got.message, want)
	}
}

func TestPrepareAskInputStagesLargeFileSource(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	source := filepath.Join(t.TempDir(), "large.txt")
	raw := []byte("large text body")
	if err := os.WriteFile(source, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := prepareAskInput("question", []string{source}, nil, false, testAskConfig())
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	t.Cleanup(func() { cleanupPreparedAskInput(got) })
	if len(got.message.Parts) != 2 || got.message.Parts[0].Type != llm.PartFile || got.message.Parts[1].Text != "question" {
		t.Fatalf("parts = %#v, want staged file then question", got.message.Parts)
	}
	part := got.message.Parts[0]
	if part.FileData == nil || part.FileData.Filename != "large.txt" || part.FileData.MediaType != "text/plain" || part.FileData.SizeBytes != int64(len(raw)) {
		t.Fatalf("file metadata = %#v", part.FileData)
	}
	if len(got.stagedPaths) != 1 || got.stagedPaths[0] != part.FilePath {
		t.Fatalf("staged paths = %v, want structured file path %q", got.stagedPaths, part.FilePath)
	}
	if part.FilePath == source || !pathWithinDir(part.FilePath, serveUploadsDir()) {
		t.Fatalf("staged path = %q, source = %q", part.FilePath, source)
	}
	saved, err := os.ReadFile(part.FilePath)
	if err != nil || !bytes.Equal(saved, raw) {
		t.Fatalf("staged content = %q, err = %v", saved, err)
	}
}

func TestPrepareAskInputClassifiesPNGFileAndStagesOriginal(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	source := filepath.Join(t.TempDir(), "source.png")
	raw := tinyAskPNG(t)
	if err := os.WriteFile(source, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := prepareAskInput("describe", []string{source}, nil, false, testAskConfig())
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	t.Cleanup(func() { cleanupPreparedAskInput(got) })
	if len(got.message.Parts) != 2 || got.message.Parts[0].Type != llm.PartImage {
		t.Fatalf("parts = %#v, want image then question", got.message.Parts)
	}
	part := got.message.Parts[0]
	if part.ImagePath == source || !pathWithinDir(part.ImagePath, serveUploadsDir()) {
		t.Fatalf("image path = %q, source = %q", part.ImagePath, source)
	}
	if len(got.stagedPaths) != 1 || got.stagedPaths[0] != part.ImagePath {
		t.Fatalf("staged paths = %v, want structured image path %q", got.stagedPaths, part.ImagePath)
	}
	if part.ImageData == nil || part.ImageData.MediaType != "image/png" {
		t.Fatalf("image metadata = %#v", part.ImageData)
	}
	providerBytes, err := base64.StdEncoding.DecodeString(part.ImageData.Base64)
	if err != nil || len(providerBytes) > maxLLMImageBytes {
		t.Fatalf("provider image bytes = %d, err = %v", len(providerBytes), err)
	}
	saved, err := os.ReadFile(part.ImagePath)
	if err != nil || !bytes.Equal(saved, raw) {
		t.Fatalf("persisted original differs: err=%v", err)
	}
}

func TestPrepareAskInputStagesBinaryFileSource(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	source := filepath.Join(t.TempDir(), "misleading.txt")
	raw := []byte("%PDF-1.7\n%\x00binary")
	if err := os.WriteFile(source, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := prepareAskInput("inspect", []string{source}, nil, false, testAskConfig())
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	t.Cleanup(func() { cleanupPreparedAskInput(got) })
	if len(got.message.Parts) != 2 || got.message.Parts[0].Type != llm.PartFile {
		t.Fatalf("parts = %#v, want binary file then question", got.message.Parts)
	}
	part := got.message.Parts[0]
	if part.FileData == nil || part.FileData.MediaType != "application/pdf" || part.FileData.Filename != "misleading.pdf" {
		t.Fatalf("file metadata = %#v", part.FileData)
	}
	if len(got.stagedPaths) != 1 || got.stagedPaths[0] != part.FilePath || part.FilePath == source || !pathWithinDir(part.FilePath, serveUploadsDir()) {
		t.Fatalf("staged paths/file path = %v / %q", got.stagedPaths, part.FilePath)
	}
	saved, err := os.ReadFile(part.FilePath)
	if err != nil || !bytes.Equal(saved, raw) {
		t.Fatalf("staged content = %q, err = %v", saved, err)
	}
}

func TestPrepareAskInputPreservesMixedFileStdinQuestionOrder(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := t.TempDir()
	smallA := filepath.Join(dir, "a.txt")
	large := filepath.Join(dir, "large.txt")
	smallC := filepath.Join(dir, "c.txt")
	for path, body := range map[string]string{smallA: "a", large: "large text body", smallC: "c"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := prepareAskInput("question", []string{smallA, large, smallC}, strings.NewReader("pipe"), true, testAskConfig())
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	t.Cleanup(func() { cleanupPreparedAskInput(got) })
	if len(got.message.Parts) != 3 {
		t.Fatalf("parts = %#v, want text, file, text", got.message.Parts)
	}
	wantFirst := prompt.AskUserPrompt("", []inputpkg.FileContent{{Path: smallA, Content: "a"}}, "")
	wantLast := prompt.AskUserPrompt("question", []inputpkg.FileContent{{Path: smallC, Content: "c"}}, "pipe")
	if got.message.Parts[0].Type != llm.PartText || got.message.Parts[0].Text != wantFirst {
		t.Fatalf("first part = %#v, want %q", got.message.Parts[0], wantFirst)
	}
	if got.message.Parts[1].Type != llm.PartFile || got.message.Parts[1].FileData == nil || got.message.Parts[1].FileData.Filename != "large.txt" {
		t.Fatalf("second part = %#v, want large file", got.message.Parts[1])
	}
	if got.message.Parts[2].Type != llm.PartText || got.message.Parts[2].Text != wantLast {
		t.Fatalf("last part = %#v, want %q", got.message.Parts[2], wantLast)
	}
}

func TestPrepareAskInputRejectsOversizeFileWithoutStagedLeftovers(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	dir := t.TempDir()
	first := filepath.Join(dir, "first.txt")
	oversize := filepath.Join(dir, "oversize.txt")
	if err := os.WriteFile(first, []byte("123"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oversize, []byte("123456"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testAskConfig()
	cfg.StdinInlineMaxBytes = 2
	cfg.StdinMaxBytes = 5

	_, err := prepareAskInput("question", []string{first, oversize}, nil, false, cfg)
	if err == nil || !strings.Contains(err.Error(), oversize) || !strings.Contains(err.Error(), "5 bytes") {
		t.Fatalf("error = %v, want per-file size rejection", err)
	}
	entries, readErr := os.ReadDir(filepath.Join(dataHome, "term-llm", "uploads"))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("oversize -f left staged files: %v", entries)
	}
}

func TestPrepareAskInputPreservesGlobExpansionOrder(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "01.txt")
	second := filepath.Join(dir, "02.txt")
	last := filepath.Join(dir, "last.md")
	for path, body := range map[string]string{first: "first", second: "second", last: "last"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := testAskConfig()
	cfg.StdinInlineMaxBytes = 100

	got, err := prepareAskInput("question", []string{filepath.Join(dir, "*.txt"), last}, nil, false, cfg)
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	want := prompt.AskUserPrompt("question", []inputpkg.FileContent{
		{Path: first, Content: "first"},
		{Path: second, Content: "second"},
		{Path: last, Content: "last"},
	}, "")
	if len(got.message.Parts) != 1 || got.message.Parts[0].Text != want {
		t.Fatalf("message = %#v, want glob then explicit order %q", got.message, want)
	}
}

func TestPrepareAskInputLineRangesUseSelectedTextPolicy(t *testing.T) {
	t.Run("limit applies to selected bytes", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "lines.txt")
		if err := os.WriteFile(source, []byte("keep\n"+strings.Repeat("x", 100)), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := testAskConfig()
		cfg.StdinInlineMaxBytes = 4
		cfg.StdinMaxBytes = 4
		got, err := prepareAskInput("question", []string{source + ":1-1"}, nil, false, cfg)
		if err != nil {
			t.Fatalf("prepareAskInput: %v", err)
		}
		want := prompt.AskUserPrompt("question", []inputpkg.FileContent{{Path: source + ":1-1", Content: "keep"}}, "")
		if len(got.message.Parts) != 1 || got.message.Parts[0].Text != want {
			t.Fatalf("message = %#v, want selected line %q", got.message, want)
		}
	})

	t.Run("open range keeps readable display syntax", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "lines.txt")
		if err := os.WriteFile(source, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := testAskConfig()
		cfg.StdinInlineMaxBytes = 100
		got, err := prepareAskInput("question", []string{source + ":2-"}, nil, false, cfg)
		if err != nil {
			t.Fatalf("prepareAskInput: %v", err)
		}
		want := prompt.AskUserPrompt("question", []inputpkg.FileContent{{Path: source + ":2-", Content: "two\nthree\n"}}, "")
		if len(got.message.Parts) != 1 || got.message.Parts[0].Text != want {
			t.Fatalf("message = %#v, want %q", got.message, want)
		}
	})

	t.Run("whole binary range uses normal image classification", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		source := filepath.Join(t.TempDir(), "image.png")
		raw := tinyAskPNG(t)
		if err := os.WriteFile(source, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := prepareAskInput("question", []string{source + ":1-"}, nil, false, testAskConfig())
		if err != nil {
			t.Fatalf("prepareAskInput: %v", err)
		}
		t.Cleanup(func() { cleanupPreparedAskInput(got) })
		if len(got.message.Parts) != 2 || got.message.Parts[0].Type != llm.PartImage || got.message.Parts[0].ImageData == nil || got.message.Parts[0].ImageData.MediaType != "image/png" {
			t.Fatalf("parts = %#v, want image part", got.message.Parts)
		}
		part := got.message.Parts[0]
		if len(got.stagedPaths) != 1 || got.stagedPaths[0] != part.ImagePath || !strings.Contains(filepath.Base(part.ImagePath), "image_lines_1-end_") {
			t.Fatalf("staged range = %v / %q", got.stagedPaths, part.ImagePath)
		}
		saved, err := os.ReadFile(part.ImagePath)
		if err != nil || !bytes.Equal(saved, raw) {
			t.Fatalf("staged range differs from extracted bytes: err=%v", err)
		}
	})
}

func TestReadBoundedAskLineRangePreservesExtractLinesSemantics(t *testing.T) {
	content := "one\ntwo\nthree\n"
	for _, tc := range []struct {
		name       string
		start, end int
	}{
		{name: "closed", start: 1, end: 2},
		{name: "open start", start: 0, end: 2},
		{name: "open end", start: 2, end: 0},
		{name: "past end", start: 8, end: 0},
		{name: "reversed", start: 3, end: 2},
		{name: "trailing empty line", start: 4, end: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readBoundedAskLineRange(strings.NewReader(content), tc.start, tc.end, 1024)
			if err != nil {
				t.Fatalf("readBoundedAskLineRange: %v", err)
			}
			want := inputpkg.ExtractLines(content, tc.start, tc.end)
			if string(got) != want {
				t.Fatalf("range %d-%d = %q, want %q", tc.start, tc.end, got, want)
			}
		})
	}
}

func TestReadBoundedAskLineRangeBoundariesAndReadErrors(t *testing.T) {
	longLine := strings.Repeat("x", 64*1024)
	for _, tc := range []struct {
		name       string
		reader     io.Reader
		start, end int
		maxBytes   int64
		want       string
		wantErr    error
	}{
		{name: "closed range at byte limit", reader: iotest.OneByteReader(strings.NewReader("one\ntwo")), start: 1, end: 1, maxBytes: 3, want: "one"},
		{name: "open range includes overflowing newline", reader: iotest.OneByteReader(strings.NewReader("one\ntwo")), start: 1, maxBytes: 3, want: "one\n"},
		{name: "skipped line exceeds buffer", reader: strings.NewReader(longLine + "\nchosen\nignored"), start: 2, end: 2, maxBytes: 6, want: "chosen"},
		{name: "selected line exceeds buffer", reader: strings.NewReader(longLine + "\nignored"), start: 1, end: 1, maxBytes: int64(len(longLine)), want: longLine},
		{name: "data and EOF together", reader: iotest.DataErrReader(strings.NewReader("one\ntwo")), start: 2, maxBytes: 10, want: "two"},
		{name: "error discards partial range", reader: io.MultiReader(strings.NewReader("one"), iotest.ErrReader(io.ErrUnexpectedEOF)), start: 1, maxBytes: 10, wantErr: io.ErrUnexpectedEOF},
		{name: "completed range ignores later error", reader: io.MultiReader(strings.NewReader("one\n"), iotest.ErrReader(io.ErrUnexpectedEOF)), start: 1, end: 1, maxBytes: 10, want: "one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readBoundedAskLineRange(tc.reader, tc.start, tc.end, tc.maxBytes)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("readBoundedAskLineRange error = %v, want %v", err, tc.wantErr)
			}
			if string(got) != tc.want {
				t.Fatalf("range content mismatch: got %d bytes, want %d", len(got), len(tc.want))
			}
		})
	}
}

func TestSetupToolManagerKeepsAutoToolsScopedAndMergesExplicitTools(t *testing.T) {
	cfg := &config.Config{Tools: config.ToolsConfig{Enabled: []string{tools.GrepToolName}}}
	defaultsEngine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	defaults := SessionSettings{AutoTools: []string{tools.ReadFileToolName}}
	defaultsMgr, err := defaults.SetupToolManager(cfg, defaultsEngine)
	if err != nil {
		t.Fatalf("SetupToolManager defaults: %v", err)
	}
	defer defaultsMgr.ApprovalMgr.Close()
	if _, ok := defaultsMgr.Registry.Get(tools.ReadFileToolName); !ok {
		t.Fatalf("auto tool %q was not registered", tools.ReadFileToolName)
	}
	if _, ok := defaultsMgr.Registry.Get(tools.GrepToolName); ok {
		t.Fatal("auto tools unexpectedly activated the global default tool set")
	}

	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	settings := SessionSettings{
		Tools:     tools.ShellToolName,
		AutoTools: []string{tools.ReadFileToolName, tools.ViewImageToolName},
	}
	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		t.Fatalf("SetupToolManager: %v", err)
	}
	defer toolMgr.ApprovalMgr.Close()
	for _, name := range []string{tools.ShellToolName, tools.ReadFileToolName, tools.ViewImageToolName} {
		if _, ok := toolMgr.Registry.Get(name); !ok {
			t.Fatalf("tool %q was not registered", name)
		}
	}
	if _, ok := toolMgr.Registry.Get(tools.GrepToolName); ok {
		t.Fatal("CLI tool selection should still replace config defaults")
	}
	if settings.Tools != tools.ShellToolName {
		t.Fatalf("raw configured Tools changed to %q", settings.Tools)
	}
}

func TestAskAttachmentGrantsAreExactAndStructured(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := uploadsReadDir()
	attached := filepath.Join(root, "attached.bin")
	sibling := filepath.Join(root, "sibling.bin")
	for _, path := range []string{attached, sibling} {
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside.bin")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escaped.bin")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	cfg := &config.Config{}
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	settings := SessionSettings{AutoTools: []string{tools.ReadFileToolName, tools.ViewImageToolName}}
	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		t.Fatalf("SetupToolManager: %v", err)
	}
	defer toolMgr.ApprovalMgr.Close()
	messages := []llm.Message{
		{Role: llm.RoleUser, Parts: []llm.Part{
			{Type: llm.PartImage, ImagePath: attached},
			{Type: llm.PartFile, FilePath: link},
			{Type: llm.PartText, Text: sibling},
		}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartFile, FilePath: sibling}}},
	}
	grantAskUploadedFileReads(toolMgr, messages)

	if outcome, err := toolMgr.ApprovalMgr.CheckPathApproval(tools.ReadFileToolName, attached, attached, false); err != nil || outcome != tools.ProceedOnce {
		t.Fatalf("attached read approval = %v, %v", outcome, err)
	}
	if outcome, err := toolMgr.ApprovalMgr.CheckPathApproval(tools.ViewImageToolName, attached, attached, false); err != nil || outcome != tools.ProceedOnce {
		t.Fatalf("attached image approval = %v, %v", outcome, err)
	}
	for _, path := range []string{sibling, outside, link} {
		if outcome, err := toolMgr.ApprovalMgr.CheckPathApproval(tools.ReadFileToolName, path, path, false); err == nil || outcome != tools.Cancel {
			t.Fatalf("unexpected approval for %q = %v, %v", path, outcome, err)
		}
	}
}

func TestDeriveAskResumeAutoToolsFromMissingStructuredAttachment(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sess := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock", Mode: session.ModeAsk}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	message := llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{
		Type: llm.PartImage, ImagePath: filepath.Join(t.TempDir(), "missing.png"), ImageData: &llm.ToolImageData{MediaType: "image/png"},
	}}}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, message, -1)); err != nil {
		t.Fatal(err)
	}
	settings := SessionSettings{Tools: tools.ShellToolName}
	deriveAskResumeAutoTools(ctx, store, sess, &settings)
	if settings.Tools != tools.ShellToolName {
		t.Fatalf("configured Tools changed to %q", settings.Tools)
	}
	if strings.Join(settings.AutoTools, ",") != tools.ReadFileToolName+","+tools.ViewImageToolName {
		t.Fatalf("AutoTools = %v", settings.AutoTools)
	}
}

func TestInitializeAskPersistencePreservesStructuredUserMessage(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sess := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock", Mode: session.ModeAsk}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	user := llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
		{Type: llm.PartFile, Text: "notice", FilePath: "/tmp/stdin.txt", FileData: &llm.ToolFileData{Filename: "stdin.txt", MediaType: "text/plain", SizeBytes: 12}},
		{Type: llm.PartText, Text: "question"},
	}}
	persistence := initializeAskPersistence(ctx, nil, store, sess, "", false, false, nil, sess.CreatedAt, user, "question")
	if persistence == nil || !persistence.initialUserPersisted {
		t.Fatal("structured user message was not persisted")
	}
	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || len(messages[0].Parts) != 2 || messages[0].Parts[0].Type != llm.PartFile || messages[0].Parts[0].FilePath != "/tmp/stdin.txt" {
		t.Fatalf("persisted messages = %#v", messages)
	}
}

func tinyAskPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 0, color.RGBA{B: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

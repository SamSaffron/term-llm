package ask

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/app/uploads"
	"github.com/samsaffron/term-llm/internal/config"
	inputpkg "github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/prompt"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/cobra"
)

func testAskConfig() config.AskConfig {
	return config.AskConfig{StdinInlineMaxBytes: 10, StdinMaxBytes: 1024}
}

func TestPrepareAskInputUsesCobraInputAndKeepsSmallTextInline(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader("small text"))
	reader, hasStdin := CommandInput(cmd)
	if !hasStdin {
		t.Fatal("SetIn reader was not recognized as piped stdin")
	}

	got, err := PrepareInput("question", nil, reader, hasStdin, testAskConfig())
	if err != nil {
		t.Fatalf("PrepareInput: %v", err)
	}
	if len(got.StagedPaths) != 0 {
		t.Fatalf("staged paths = %v, want none", got.StagedPaths)
	}
	want := prompt.AskUserPrompt("question", nil, "small text")
	if len(got.Message.Parts) != 1 || got.Message.Parts[0].Type != core.PartText || got.Message.Parts[0].Text != want {
		t.Fatalf("message = %#v, want exact legacy prompt %q", got.Message, want)
	}
}

func TestPrepareAskInputAllowsEmptyQuestionWithPipedText(t *testing.T) {
	got, err := PrepareInput("", nil, strings.NewReader("piped only"), true, testAskConfig())
	if err != nil {
		t.Fatalf("PrepareInput: %v", err)
	}
	want := prompt.AskUserPrompt("", nil, "piped only")
	if len(got.Message.Parts) != 1 || got.Message.Parts[0].Text != want {
		t.Fatalf("message = %#v, want %q", got.Message, want)
	}
}

func TestPrepareAskInputRejectsEmptyPipeWithoutQuestion(t *testing.T) {
	_, err := PrepareInput("", nil, strings.NewReader(""), true, testAskConfig())
	if err == nil || !strings.Contains(err.Error(), "stdin was empty") {
		t.Fatalf("error = %v, want empty stdin validation", err)
	}
}

func TestPrepareAskInputUsesAgentDefaultForEmptyPipe(t *testing.T) {
	got, err := PrepareInput("", nil, strings.NewReader(""), true, testAskConfig(), "inspect the project")
	if err != nil {
		t.Fatalf("PrepareInput: %v", err)
	}
	if got.Question != "inspect the project" || len(got.Message.Parts) != 1 || got.Message.Parts[0].Text != "inspect the project" {
		t.Fatalf("prepared input = %#v", got)
	}
}

func TestPrepareAskInputUsesAgentDefaultWithPipedContent(t *testing.T) {
	got, err := PrepareInput("", nil, strings.NewReader("pipe"), true, testAskConfig(), "inspect the input")
	if err != nil {
		t.Fatalf("PrepareInput: %v", err)
	}
	if got.Question != "inspect the input" || !strings.HasSuffix(got.Message.Parts[0].Text, "\n\ninspect the input") {
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
	got, err := PrepareInput("question", []string{contextPath}, strings.NewReader(input), true, testAskConfig())
	if err != nil {
		t.Fatalf("PrepareInput: %v", err)
	}
	t.Cleanup(func() { CleanupPreparedInput(got) })
	if len(got.Message.Parts) != 3 {
		t.Fatalf("parts = %#v, want context, file, question", got.Message.Parts)
	}
	if got.Message.Parts[0].Type != core.PartText || !strings.Contains(got.Message.Parts[0].Text, "ctx") {
		t.Fatalf("first part = %#v, want embedded -f context", got.Message.Parts[0])
	}
	part := got.Message.Parts[1]
	if part.Type != core.PartFile || part.FileData == nil {
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
	if got.Message.Parts[2].Type != core.PartText || got.Message.Parts[2].Text != "question" {
		t.Fatalf("question part = %#v", got.Message.Parts[2])
	}
	raw, err := os.ReadFile(part.FilePath)
	if err != nil || string(raw) != input {
		t.Fatalf("staged content = %q, err = %v", raw, err)
	}
	info, err := os.Stat(part.FilePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("staged mode = %v, err = %v", info.Mode().Perm(), err)
	}
	if !pathWithinDir(part.FilePath, uploads.Dir()) {
		t.Fatalf("staged path %q is outside uploads", part.FilePath)
	}
}

func TestPrepareAskInputClassifiesTinyPNGAndPreservesOriginal(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	raw := tinyAskPNG(t)
	got, err := PrepareInput("describe", nil, bytes.NewReader(raw), true, testAskConfig())
	if err != nil {
		t.Fatalf("PrepareInput: %v", err)
	}
	t.Cleanup(func() { CleanupPreparedInput(got) })
	if len(got.Message.Parts) != 2 || got.Message.Parts[0].Type != core.PartImage {
		t.Fatalf("message parts = %#v", got.Message.Parts)
	}
	part := got.Message.Parts[0]
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
	autoTools := AddAutoTools(nil, []core.Message{got.Message})
	if strings.Join(autoTools, ",") != tools.ReadFileToolName+","+tools.ViewImageToolName {
		t.Fatalf("AutoTools = %v", autoTools)
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
	if encoded.Len() <= (1 << 20) {
		t.Fatalf("test PNG is only %d bytes", encoded.Len())
	}
	cfg := testAskConfig()
	cfg.StdinMaxBytes = int64(encoded.Len() + 1)

	got, err := PrepareInput("describe", nil, bytes.NewReader(encoded.Bytes()), true, cfg)
	if err != nil {
		t.Fatalf("PrepareInput: %v", err)
	}
	t.Cleanup(func() { CleanupPreparedInput(got) })
	part := got.Message.Parts[0]
	if part.ImageData == nil {
		t.Fatal("missing provider image metadata")
	}
	providerBytes, err := base64.StdEncoding.DecodeString(part.ImageData.Base64)
	if err != nil {
		t.Fatal(err)
	}
	width, height := uploads.ImageDisplayDimensions(providerBytes)
	if width <= 0 || height <= 0 || part.ImageData.Width != width || part.ImageData.Height != height || width >= 1024 || height >= 1024 {
		t.Fatalf("metadata dimensions = %dx%d, provider bytes = %dx%d", part.ImageData.Width, part.ImageData.Height, width, height)
	}
	if len(providerBytes) > uploads.MaxLLMImageBytes {
		t.Fatalf("provider image is %d bytes, want at most %d", len(providerBytes), uploads.MaxLLMImageBytes)
	}
	if part.ImageData.MediaType != "image/jpeg" {
		t.Fatalf("provider image MIME = %q, want resized JPEG", part.ImageData.MediaType)
	}
	saved, err := os.ReadFile(part.ImagePath)
	if err != nil || !bytes.Equal(saved, encoded.Bytes()) {
		t.Fatalf("saved original differs: err=%v", err)
	}
}

func TestPrepareAskInputDetectsBinaryMIMEAndExtension(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	raw := []byte("%PDF-1.7\n%\x00binary")
	got, err := PrepareInput("inspect", nil, bytes.NewReader(raw), true, testAskConfig())
	if err != nil {
		t.Fatalf("PrepareInput: %v", err)
	}
	t.Cleanup(func() { CleanupPreparedInput(got) })
	part := got.Message.Parts[0]
	if part.Type != core.PartFile || part.FileData == nil {
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
	_, err := PrepareInput("question", nil, strings.NewReader("123456"), true, cfg)
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

	got, err := PrepareInput("question", []string{first, second}, nil, false, cfg)
	if err != nil {
		t.Fatalf("PrepareInput: %v", err)
	}
	if len(got.StagedPaths) != 0 {
		t.Fatalf("staged paths = %v, want none", got.StagedPaths)
	}
	want := prompt.AskUserPrompt("question", []inputpkg.FileContent{
		{Path: first, Content: "alpha"},
		{Path: second, Content: "beta\n"},
	}, "")
	if len(got.Message.Parts) != 1 || got.Message.Parts[0].Type != core.PartText || got.Message.Parts[0].Text != want {
		t.Fatalf("message = %#v, want exact legacy prompt %q", got.Message, want)
	}
}

func TestPrepareAskInputStagesLargeFileSource(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	source := filepath.Join(t.TempDir(), "large.txt")
	raw := []byte("large text body")
	if err := os.WriteFile(source, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := PrepareInput("question", []string{source}, nil, false, testAskConfig())
	if err != nil {
		t.Fatalf("PrepareInput: %v", err)
	}
	t.Cleanup(func() { CleanupPreparedInput(got) })
	if len(got.Message.Parts) != 2 || got.Message.Parts[0].Type != core.PartFile || got.Message.Parts[1].Text != "question" {
		t.Fatalf("parts = %#v, want staged file then question", got.Message.Parts)
	}
	part := got.Message.Parts[0]
	if part.FileData == nil || part.FileData.Filename != "large.txt" || part.FileData.MediaType != "text/plain" || part.FileData.SizeBytes != int64(len(raw)) {
		t.Fatalf("file metadata = %#v", part.FileData)
	}
	if len(got.StagedPaths) != 1 || got.StagedPaths[0] != part.FilePath {
		t.Fatalf("staged paths = %v, want structured file path %q", got.StagedPaths, part.FilePath)
	}
	if part.FilePath == source || !pathWithinDir(part.FilePath, uploads.Dir()) {
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

	got, err := PrepareInput("describe", []string{source}, nil, false, testAskConfig())
	if err != nil {
		t.Fatalf("PrepareInput: %v", err)
	}
	t.Cleanup(func() { CleanupPreparedInput(got) })
	if len(got.Message.Parts) != 2 || got.Message.Parts[0].Type != core.PartImage {
		t.Fatalf("parts = %#v, want image then question", got.Message.Parts)
	}
	part := got.Message.Parts[0]
	if part.ImagePath == source || !pathWithinDir(part.ImagePath, uploads.Dir()) {
		t.Fatalf("image path = %q, source = %q", part.ImagePath, source)
	}
	if len(got.StagedPaths) != 1 || got.StagedPaths[0] != part.ImagePath {
		t.Fatalf("staged paths = %v, want structured image path %q", got.StagedPaths, part.ImagePath)
	}
	if part.ImageData == nil || part.ImageData.MediaType != "image/png" {
		t.Fatalf("image metadata = %#v", part.ImageData)
	}
	providerBytes, err := base64.StdEncoding.DecodeString(part.ImageData.Base64)
	if err != nil || len(providerBytes) > (1<<20) {
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

	got, err := PrepareInput("inspect", []string{source}, nil, false, testAskConfig())
	if err != nil {
		t.Fatalf("PrepareInput: %v", err)
	}
	t.Cleanup(func() { CleanupPreparedInput(got) })
	if len(got.Message.Parts) != 2 || got.Message.Parts[0].Type != core.PartFile {
		t.Fatalf("parts = %#v, want binary file then question", got.Message.Parts)
	}
	part := got.Message.Parts[0]
	if part.FileData == nil || part.FileData.MediaType != "application/pdf" || part.FileData.Filename != "misleading.pdf" {
		t.Fatalf("file metadata = %#v", part.FileData)
	}
	if len(got.StagedPaths) != 1 || got.StagedPaths[0] != part.FilePath || part.FilePath == source || !pathWithinDir(part.FilePath, uploads.Dir()) {
		t.Fatalf("staged paths/file path = %v / %q", got.StagedPaths, part.FilePath)
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

	got, err := PrepareInput("question", []string{smallA, large, smallC}, strings.NewReader("pipe"), true, testAskConfig())
	if err != nil {
		t.Fatalf("PrepareInput: %v", err)
	}
	t.Cleanup(func() { CleanupPreparedInput(got) })
	if len(got.Message.Parts) != 3 {
		t.Fatalf("parts = %#v, want text, file, text", got.Message.Parts)
	}
	wantFirst := prompt.AskUserPrompt("", []inputpkg.FileContent{{Path: smallA, Content: "a"}}, "")
	wantLast := prompt.AskUserPrompt("question", []inputpkg.FileContent{{Path: smallC, Content: "c"}}, "pipe")
	if got.Message.Parts[0].Type != core.PartText || got.Message.Parts[0].Text != wantFirst {
		t.Fatalf("first part = %#v, want %q", got.Message.Parts[0], wantFirst)
	}
	if got.Message.Parts[1].Type != core.PartFile || got.Message.Parts[1].FileData == nil || got.Message.Parts[1].FileData.Filename != "large.txt" {
		t.Fatalf("second part = %#v, want large file", got.Message.Parts[1])
	}
	if got.Message.Parts[2].Type != core.PartText || got.Message.Parts[2].Text != wantLast {
		t.Fatalf("last part = %#v, want %q", got.Message.Parts[2], wantLast)
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

	_, err := PrepareInput("question", []string{first, oversize}, nil, false, cfg)
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

	got, err := PrepareInput("question", []string{filepath.Join(dir, "*.txt"), last}, nil, false, cfg)
	if err != nil {
		t.Fatalf("PrepareInput: %v", err)
	}
	want := prompt.AskUserPrompt("question", []inputpkg.FileContent{
		{Path: first, Content: "first"},
		{Path: second, Content: "second"},
		{Path: last, Content: "last"},
	}, "")
	if len(got.Message.Parts) != 1 || got.Message.Parts[0].Text != want {
		t.Fatalf("message = %#v, want glob then explicit order %q", got.Message, want)
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
		got, err := PrepareInput("question", []string{source + ":1-1"}, nil, false, cfg)
		if err != nil {
			t.Fatalf("PrepareInput: %v", err)
		}
		want := prompt.AskUserPrompt("question", []inputpkg.FileContent{{Path: source + ":1-1", Content: "keep"}}, "")
		if len(got.Message.Parts) != 1 || got.Message.Parts[0].Text != want {
			t.Fatalf("message = %#v, want selected line %q", got.Message, want)
		}
	})

	t.Run("open range keeps readable display syntax", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "lines.txt")
		if err := os.WriteFile(source, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := testAskConfig()
		cfg.StdinInlineMaxBytes = 100
		got, err := PrepareInput("question", []string{source + ":2-"}, nil, false, cfg)
		if err != nil {
			t.Fatalf("PrepareInput: %v", err)
		}
		want := prompt.AskUserPrompt("question", []inputpkg.FileContent{{Path: source + ":2-", Content: "two\nthree\n"}}, "")
		if len(got.Message.Parts) != 1 || got.Message.Parts[0].Text != want {
			t.Fatalf("message = %#v, want %q", got.Message, want)
		}
	})

	t.Run("whole binary range uses normal image classification", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		source := filepath.Join(t.TempDir(), "image.png")
		raw := tinyAskPNG(t)
		if err := os.WriteFile(source, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := PrepareInput("question", []string{source + ":1-"}, nil, false, testAskConfig())
		if err != nil {
			t.Fatalf("PrepareInput: %v", err)
		}
		t.Cleanup(func() { CleanupPreparedInput(got) })
		if len(got.Message.Parts) != 2 || got.Message.Parts[0].Type != core.PartImage || got.Message.Parts[0].ImageData == nil || got.Message.Parts[0].ImageData.MediaType != "image/png" {
			t.Fatalf("parts = %#v, want image part", got.Message.Parts)
		}
		part := got.Message.Parts[0]
		if len(got.StagedPaths) != 1 || got.StagedPaths[0] != part.ImagePath || !strings.Contains(filepath.Base(part.ImagePath), "image_lines_1-end_") {
			t.Fatalf("staged range = %v / %q", got.StagedPaths, part.ImagePath)
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

func TestApplyAskInlineMaxBytesOverride(t *testing.T) {
	base := config.AskConfig{StdinInlineMaxBytes: 10 * 1024, StdinMaxBytes: 20 * 1024 * 1024}
	cases := []struct {
		raw     string
		want    int64
		wantErr string
	}{
		{raw: "", want: 10 * 1024},
		{raw: "  ", want: 10 * 1024},
		{raw: "4096", want: 4096},
		{raw: "512K", want: 512000},
		{raw: "2MiB", want: 2 * 1024 * 1024},
		{raw: "20MiB", want: 20 * 1024 * 1024},
		{raw: "0", wantErr: "positive"},
		{raw: "banana", wantErr: "invalid --inline-max-bytes"},
		{raw: "21MiB", wantErr: "must not exceed ask.stdin_max_bytes"},
	}
	for _, tc := range cases {
		got, err := ApplyInlineMaxBytesOverride(base, tc.raw)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("%q: err = %v, want containing %q", tc.raw, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", tc.raw, err)
		}
		if got.StdinInlineMaxBytes != tc.want {
			t.Fatalf("%q: inline = %d, want %d", tc.raw, got.StdinInlineMaxBytes, tc.want)
		}
		if got.StdinMaxBytes != base.StdinMaxBytes {
			t.Fatalf("%q: stdin max changed to %d", tc.raw, got.StdinMaxBytes)
		}
	}
}

func TestAskInlineMaxBytesOverrideKeepsLargeTextInline(t *testing.T) {
	cfg := config.AskConfig{StdinInlineMaxBytes: 10 * 1024, StdinMaxBytes: 20 * 1024 * 1024}
	raw := []byte(strings.Repeat("briefing line\n", 12000)) // ~168 KB, like a large generated prompt
	staged, err := prepareAskSource("prompt.txt", "prompt.txt", raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if staged.inline != nil {
		t.Fatal("expected default limit to stage large text")
	}
	if staged.stagedPath != "" {
		_ = os.Remove(staged.stagedPath)
	}
	cfg, err = ApplyInlineMaxBytesOverride(cfg, "1MiB")
	if err != nil {
		t.Fatal(err)
	}
	inline, err := prepareAskSource("prompt.txt", "prompt.txt", raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if inline.inline == nil || *inline.inline != string(raw) {
		t.Fatal("expected override to keep large text inline")
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
	user := core.Message{Role: core.RoleUser, Parts: []core.Part{
		{Type: core.PartFile, Text: "notice", FilePath: "/tmp/stdin.txt", FileData: &core.ToolFileData{Filename: "stdin.txt", MediaType: "text/plain", SizeBytes: 12}},
		{Type: core.PartText, Text: "question"},
	}}
	persistence := initializeAskPersistence(ctx, nil, store, sess, "", false, false, nil, sess.CreatedAt, user, "question")
	if persistence == nil || !persistence.InitialUserPersisted {
		t.Fatal("structured user message was not persisted")
	}
	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || len(messages[0].Parts) != 2 || messages[0].Parts[0].Type != core.PartFile || messages[0].Parts[0].FilePath != "/tmp/stdin.txt" {
		t.Fatalf("persisted messages = %#v", messages)
	}
}

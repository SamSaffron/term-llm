package cmd

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/samsaffron/term-llm/internal/clipboard"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/prompt"
	"github.com/samsaffron/term-llm/internal/tools"
	_ "golang.org/x/image/webp"
)

type preparedAskInput struct {
	message     llm.Message
	stagedPaths []string
	question    string
}

// askCommandInput returns Cobra's configured input exactly once. Tests and
// embedders commonly replace it with Command.SetIn, while normal CLI use still
// receives os.Stdin.
func askCommandInput(cmd *cobra.Command) (io.Reader, bool) {
	reader := cmd.InOrStdin()
	return reader, askReaderHasStdin(reader)
}

func askReaderHasStdin(reader io.Reader) bool {
	file, ok := reader.(*os.File)
	if !ok {
		return reader != nil
	}
	info, err := file.Stat()
	if err != nil || term.IsTerminal(int(file.Fd())) {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0 || info.Size() > 0
}

type askPreparedFile struct {
	inline *input.FileContent
	part   *llm.Part
}

type askFileSource struct {
	displayPath string
	filename    string
	raw         []byte
}

type askPreparedSource struct {
	inline     *string
	part       *llm.Part
	stagedPath string
}

func prepareAskInput(question string, paths []string, reader io.Reader, hasStdin bool, askCfg config.AskConfig, defaultQuestions ...string) (prepared preparedAskInput, err error) {
	files, stagedPaths, err := prepareAskFiles(paths, askCfg)
	if err != nil {
		return preparedAskInput{}, fmt.Errorf("failed to read files: %w", err)
	}
	prepared.stagedPaths = append(prepared.stagedPaths, stagedPaths...)
	defer func() {
		if err != nil {
			cleanupPreparedAskInput(prepared)
			prepared = preparedAskInput{}
		}
	}()
	if strings.TrimSpace(question) == "" && len(defaultQuestions) > 0 && strings.TrimSpace(defaultQuestions[0]) != "" {
		question = defaultQuestions[0]
	}

	var stdinInline *string
	var stdinPart *llm.Part
	if hasStdin {
		raw, readErr := readBoundedAskStdin(reader, askCfg.StdinMaxBytes)
		if readErr != nil {
			return prepared, readErr
		}
		if len(raw) == 0 && strings.TrimSpace(question) == "" && len(files) == 0 {
			return prepared, fmt.Errorf("question required (stdin was empty)")
		}

		source, sourceErr := prepareAskSource("stdin", "stdin", raw, askCfg)
		if sourceErr != nil {
			return prepared, sourceErr
		}
		stdinInline = source.inline
		stdinPart = source.part
		if source.stagedPath != "" {
			prepared.stagedPaths = append(prepared.stagedPaths, source.stagedPath)
		}
	}

	prepared.message = buildAskInputMessage(question, files, stdinInline, stdinPart)
	prepared.question = question
	return prepared, nil
}

func prepareAskFiles(paths []string, askCfg config.AskConfig) (prepared []askPreparedFile, stagedPaths []string, err error) {
	defer func() {
		if err != nil {
			for _, path := range stagedPaths {
				_ = os.Remove(path)
			}
			prepared = nil
			stagedPaths = nil
		}
	}()

	for _, path := range paths {
		if strings.EqualFold(path, "clipboard") {
			content, readErr := clipboard.ReadText()
			if readErr != nil {
				return nil, stagedPaths, fmt.Errorf("failed to read clipboard: %w", readErr)
			}
			item, stagedPath, itemErr := prepareAskFileSource(askFileSource{
				displayPath: "clipboard",
				filename:    "clipboard.txt",
				raw:         []byte(content),
			}, askCfg)
			if itemErr != nil {
				return nil, stagedPaths, itemErr
			}
			prepared = append(prepared, item)
			if stagedPath != "" {
				stagedPaths = append(stagedPaths, stagedPath)
			}
			continue
		}

		spec, parseErr := input.ParseFileSpec(path)
		if parseErr != nil {
			return nil, stagedPaths, fmt.Errorf("invalid file spec %q: %w", path, parseErr)
		}
		expandedPath := expandAskPath(spec.Path)
		matches, globErr := filepath.Glob(expandedPath)
		if globErr != nil {
			return nil, stagedPaths, fmt.Errorf("invalid glob pattern %q: %w", spec.Path, globErr)
		}
		if len(matches) == 0 {
			if strings.ContainsAny(spec.Path, "*?[") {
				continue
			}
			matches = []string{expandedPath}
		}

		for _, match := range matches {
			info, statErr := os.Stat(match)
			if statErr != nil {
				return nil, stagedPaths, fmt.Errorf("failed to stat %q: %w", match, statErr)
			}
			if info.IsDir() {
				continue
			}
			raw, readErr := readBoundedAskFile(match, spec, askCfg.StdinMaxBytes)
			if readErr != nil {
				return nil, stagedPaths, readErr
			}
			displayPath := formatAskFileRegion(match, spec)
			item, stagedPath, itemErr := prepareAskFileSource(askFileSource{
				displayPath: displayPath,
				filename:    askFileSourceFilename(match, spec),
				raw:         raw,
			}, askCfg)
			if itemErr != nil {
				return nil, stagedPaths, itemErr
			}
			prepared = append(prepared, item)
			if stagedPath != "" {
				stagedPaths = append(stagedPaths, stagedPath)
			}
		}
	}
	return prepared, stagedPaths, nil
}

func prepareAskFileSource(source askFileSource, askCfg config.AskConfig) (askPreparedFile, string, error) {
	prepared, err := prepareAskSource(source.filename, source.displayPath, source.raw, askCfg)
	if err != nil {
		return askPreparedFile{}, "", err
	}
	if prepared.inline != nil {
		content := input.FileContent{Path: source.displayPath, Content: *prepared.inline}
		return askPreparedFile{inline: &content}, "", nil
	}
	return askPreparedFile{part: prepared.part}, prepared.stagedPath, nil
}

// prepareAskSource applies the same classification to every ask byte source:
// supported valid images are structured image parts, small safe text stays
// inline, and all remaining content is staged as a structured file part.
func prepareAskSource(filename, sourceName string, raw []byte, askCfg config.AskConfig) (askPreparedSource, error) {
	if askCfg.StdinMaxBytes <= 0 {
		return askPreparedSource{}, fmt.Errorf("ask.stdin_max_bytes must be positive")
	}
	if askCfg.StdinInlineMaxBytes <= 0 {
		return askPreparedSource{}, fmt.Errorf("ask.stdin_inline_max_bytes must be positive")
	}
	if int64(len(raw)) > askCfg.StdinMaxBytes {
		return askPreparedSource{}, askSourceTooLargeError(sourceName, askCfg.StdinMaxBytes)
	}
	if isAskInlineSourceText(raw, askCfg.StdinInlineMaxBytes) {
		text := string(raw)
		return askPreparedSource{inline: &text}, nil
	}
	part, stagedPath, err := prepareAskStructuredBytes(filename, sourceName, raw, askCfg.StdinMaxBytes)
	if err != nil {
		return askPreparedSource{}, err
	}
	return askPreparedSource{part: &part, stagedPath: stagedPath}, nil
}

func prepareAskStructuredBytes(filename, sourceName string, raw []byte, maxBytes int64) (llm.Part, string, error) {
	filename = sanitizeAskSourceFilename(filename)
	if mediaType, extension, width, height, ok := classifyAskImage(raw); ok {
		filename = askFilenameForMediaType(filename, mediaType, extension)
		path, err := saveUploadedBytesWithLimit(filename, raw, maxBytes)
		if err != nil {
			return llm.Part{}, "", fmt.Errorf("stage %s image: %w", sourceName, err)
		}
		sendBytes, sendMediaType := resizeImageForLLMQuiet(raw, mediaType)
		if sendWidth, sendHeight := imageDisplayDimensions(sendBytes); sendWidth > 0 && sendHeight > 0 {
			width, height = sendWidth, sendHeight
		}
		return llm.Part{
			Type: llm.PartImage,
			ImageData: &llm.ToolImageData{
				MediaType: sendMediaType,
				Base64:    base64.StdEncoding.EncodeToString(sendBytes),
				Width:     width,
				Height:    height,
			},
			ImagePath: path,
		}, path, nil
	}

	isText := utf8.Valid(raw) && !bytes.Contains(raw, []byte{0})
	mediaType := "text/plain"
	if isText {
		if filename == "stdin" {
			filename = "stdin.txt"
		}
		mediaType = normalizeUploadMediaType(filename, http.DetectContentType(raw), raw)
	} else {
		mediaType = normalizeUploadMediaType(filename, http.DetectContentType(raw), raw)
		filename = askFilenameForMediaType(filename, mediaType, askUploadExtension(mediaType))
	}
	path, err := saveUploadedBytesWithLimit(filename, raw, maxBytes)
	if err != nil {
		return llm.Part{}, "", fmt.Errorf("stage %s file: %w", sourceName, err)
	}
	return llm.Part{
		Type: llm.PartFile,
		Text: llm.FormatUploadedFileNotice(filename, mediaType, path, int64(len(raw))),
		FileData: &llm.ToolFileData{
			Filename:  filename,
			MediaType: mediaType,
			SizeBytes: int64(len(raw)),
		},
		FilePath: path,
	}, path, nil
}

func buildAskInputMessage(question string, files []askPreparedFile, stdinInline *string, stdinPart *llm.Part) llm.Message {
	parts := make([]llm.Part, 0, len(files)+2)
	pending := make([]input.FileContent, 0, len(files))
	flushPending := func() {
		if len(pending) == 0 {
			return
		}
		parts = append(parts, llm.Part{Type: llm.PartText, Text: prompt.AskUserPrompt("", pending, "")})
		pending = pending[:0]
	}

	for _, file := range files {
		if file.inline != nil {
			pending = append(pending, *file.inline)
			continue
		}
		flushPending()
		if file.part != nil {
			parts = append(parts, *file.part)
		}
	}

	if stdinPart != nil {
		flushPending()
		parts = append(parts, *stdinPart)
		if question != "" {
			parts = append(parts, llm.Part{Type: llm.PartText, Text: question})
		}
	} else {
		stdin := ""
		if stdinInline != nil {
			stdin = *stdinInline
		}
		text := prompt.AskUserPrompt(question, pending, stdin)
		if text != "" || len(parts) == 0 {
			parts = append(parts, llm.Part{Type: llm.PartText, Text: text})
		}
	}
	return llm.Message{Role: llm.RoleUser, Parts: parts}
}

func isAskInlineSourceText(raw []byte, inlineMaxBytes int64) bool {
	if _, _, _, _, ok := classifyAskImage(raw); ok {
		return false
	}
	return isAskInlineText(raw, inlineMaxBytes)
}

func isAskInlineText(raw []byte, inlineMaxBytes int64) bool {
	return utf8.Valid(raw) && !bytes.Contains(raw, []byte{0}) && int64(len(raw)) <= inlineMaxBytes
}

func readBoundedAskFile(path string, spec input.FileSpec, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("ask.stdin_max_bytes must be positive")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %q: %w", path, err)
	}
	defer file.Close()

	var raw []byte
	if spec.HasRegion {
		raw, err = readBoundedAskLineRange(file, spec.StartLine, spec.EndLine, maxBytes)
	} else {
		raw, err = io.ReadAll(io.LimitReader(file, maxBytes+1))
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read %q: %w", path, err)
	}
	if int64(len(raw)) > maxBytes {
		return nil, askSourceTooLargeError(formatAskFileRegion(path, spec), maxBytes)
	}
	return raw, nil
}

func readBoundedAskLineRange(reader io.Reader, startLine, endLine int, maxBytes int64) ([]byte, error) {
	start := startLine
	if start <= 0 {
		start = 1
	}
	if endLine > 0 && start > endLine {
		return nil, nil
	}

	line := 1
	raw := make([]byte, 0, min(int(maxBytes), 32*1024))
	buf := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buf)
		for _, b := range buf[:n] {
			selected := line >= start && (endLine == 0 || line <= endLine)
			if b == '\n' {
				if selected && (endLine == 0 || line < endLine) {
					raw = append(raw, b)
				}
				line++
				if endLine > 0 && line > endLine {
					return raw, nil
				}
			} else if selected {
				raw = append(raw, b)
			}
			if int64(len(raw)) > maxBytes {
				return raw, nil
			}
		}
		if readErr == io.EOF {
			return raw, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func askSourceTooLargeError(source string, maxBytes int64) error {
	return fmt.Errorf("%s exceeds ask.stdin_max_bytes limit of %d bytes", source, maxBytes)
}

func formatAskFileRegion(path string, spec input.FileSpec) string {
	if !spec.HasRegion {
		return path
	}
	start := spec.StartLine
	if start <= 0 {
		start = 1
	}
	if spec.EndLine <= 0 {
		return fmt.Sprintf("%s:%d-", path, start)
	}
	return fmt.Sprintf("%s:%d-%d", path, start, spec.EndLine)
}

func askFileSourceFilename(path string, spec input.FileSpec) string {
	filename := filepath.Base(path)
	if !spec.HasRegion {
		return filename
	}
	extension := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, extension)
	if stem == "" {
		stem = "selection"
	}
	startLine := spec.StartLine
	if startLine <= 0 {
		startLine = 1
	}
	endLine := "end"
	if spec.EndLine > 0 {
		endLine = fmt.Sprintf("%d", spec.EndLine)
	}
	return fmt.Sprintf("%s_lines_%d-%s%s", stem, startLine, endLine, extension)
}

func sanitizeAskSourceFilename(filename string) string {
	filename = filepath.Base(strings.TrimSpace(filename))
	if filename == "" || filename == "." || filename == string(filepath.Separator) {
		return "upload"
	}
	return filename
}

func askFilenameForMediaType(filename, mediaType, extension string) string {
	mediaType = llm.NormalizeMediaType(mediaType)
	currentExtension := filepath.Ext(filename)
	if currentExtension != "" && llm.NormalizeMediaType(mime.TypeByExtension(strings.ToLower(currentExtension))) == mediaType {
		return filename
	}
	if extension == "" {
		extension = askUploadExtension(mediaType)
	}
	stem := strings.TrimSuffix(filename, currentExtension)
	if stem == "" {
		stem = strings.Trim(filename, ".")
	}
	if stem == "" {
		stem = "upload"
	}
	return stem + extension
}

func expandAskPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func readBoundedAskStdin(reader io.Reader, maxBytes int64) ([]byte, error) {
	if reader == nil {
		return nil, nil
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("ask.stdin_max_bytes must be positive")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read stdin: %w", err)
	}
	if int64(len(raw)) > maxBytes {
		return nil, askSourceTooLargeError("stdin", maxBytes)
	}
	return raw, nil
}

func classifyAskImage(raw []byte) (mediaType, extension string, width, height int, ok bool) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return "", "", 0, 0, false
	}
	w, h := int64(cfg.Width), int64(cfg.Height)
	if w <= 0 || h <= 0 || w > maxLLMImagePixels || h > maxLLMImagePixels/w {
		return "", "", 0, 0, false
	}
	switch strings.ToLower(format) {
	case "png":
		return "image/png", ".png", cfg.Width, cfg.Height, true
	case "jpeg":
		return "image/jpeg", ".jpg", cfg.Width, cfg.Height, true
	case "gif":
		return "image/gif", ".gif", cfg.Width, cfg.Height, true
	case "webp":
		return "image/webp", ".webp", cfg.Width, cfg.Height, true
	default:
		return "", "", 0, 0, false
	}
}

func askUploadExtension(mediaType string) string {
	mediaType = llm.NormalizeMediaType(mediaType)
	switch mediaType {
	case "application/pdf":
		return ".pdf"
	case "application/zip":
		return ".zip"
	case "application/gzip", "application/x-gzip":
		return ".gz"
	case "application/octet-stream":
		return ".bin"
	}
	extensions, _ := mime.ExtensionsByType(mediaType)
	sort.Slice(extensions, func(i, j int) bool {
		if len(extensions[i]) == len(extensions[j]) {
			return extensions[i] < extensions[j]
		}
		return len(extensions[i]) < len(extensions[j])
	})
	for _, extension := range extensions {
		if strings.HasPrefix(extension, ".") && len(extension) <= 12 {
			return strings.ToLower(extension)
		}
	}
	return ".bin"
}

func askInputSummary(question string, message llm.Message) string {
	if question = strings.TrimSpace(question); question != "" {
		return question
	}
	for _, part := range message.Parts {
		switch part.Type {
		case llm.PartImage:
			return "stdin image"
		case llm.PartFile:
			if part.FileData != nil && strings.TrimSpace(part.FileData.Filename) != "" {
				return "stdin: " + part.FileData.Filename
			}
			return "stdin file"
		}
	}
	return "stdin input"
}

func cleanupPreparedAskInput(prepared preparedAskInput) {
	for _, path := range prepared.stagedPaths {
		_ = os.Remove(path)
	}
}

func addAskAutoTools(settings *SessionSettings, messages []llm.Message) {
	if settings == nil {
		return
	}
	for _, message := range messages {
		if message.Role != llm.RoleUser {
			continue
		}
		for _, part := range message.Parts {
			switch part.Type {
			case llm.PartFile:
				settings.AutoTools = appendUniqueString(settings.AutoTools, tools.ReadFileToolName)
			case llm.PartImage:
				settings.AutoTools = appendUniqueString(settings.AutoTools, tools.ReadFileToolName)
				settings.AutoTools = appendUniqueString(settings.AutoTools, tools.ViewImageToolName)
			}
		}
	}
}

// grantAskUploadedFileReads grants only existing regular files referenced by
// structured user parts and canonically contained by the app-owned uploads dir.
func grantAskUploadedFileReads(toolMgr *tools.ToolManager, messages []llm.Message) {
	if toolMgr == nil || toolMgr.ApprovalMgr == nil {
		return
	}
	root := serveUploadsDir()
	if root == "" {
		return
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return
	}
	for _, message := range messages {
		if message.Role != llm.RoleUser {
			continue
		}
		for _, part := range message.Parts {
			path := ""
			switch part.Type {
			case llm.PartFile:
				path = part.FilePath
			case llm.PartImage:
				path = part.ImagePath
			}
			if path == "" {
				continue
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil || !pathWithinDir(resolved, root) {
				continue
			}
			info, err := os.Stat(resolved)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			_ = toolMgr.ApprovalMgr.AddReadFile(resolved)
		}
	}
}

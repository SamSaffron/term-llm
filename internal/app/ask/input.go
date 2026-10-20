package ask

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	humanize "github.com/dustin/go-humanize"
	"github.com/samsaffron/term-llm/internal/app/uploads"
	"github.com/samsaffron/term-llm/internal/clipboard"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/filemime"
	"github.com/samsaffron/term-llm/internal/prompt"
	"github.com/samsaffron/term-llm/internal/tools"
	_ "golang.org/x/image/webp"
	"golang.org/x/term"
)

type PreparedInput struct {
	Message     core.Message
	StagedPaths []string
	Question    string
}

// ReaderHasStdin reports whether a captured input contains piped data.
func ReaderHasStdin(reader io.Reader) bool {
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
	part   *core.Part
}

type askFileSource struct {
	displayPath string
	filename    string
	raw         []byte
}

type askPreparedSource struct {
	inline     *string
	part       *core.Part
	stagedPath string
}

func PrepareInput(question string, paths []string, reader io.Reader, hasStdin bool, askCfg config.AskConfig, defaultQuestions ...string) (prepared PreparedInput, err error) {
	files, stagedPaths, err := prepareAskFiles(paths, askCfg)
	if err != nil {
		return PreparedInput{}, fmt.Errorf("failed to read files: %w", err)
	}
	prepared.StagedPaths = append(prepared.StagedPaths, stagedPaths...)
	defer func() {
		if err != nil {
			CleanupPreparedInput(prepared)
			prepared = PreparedInput{}
		}
	}()
	if strings.TrimSpace(question) == "" && len(defaultQuestions) > 0 && strings.TrimSpace(defaultQuestions[0]) != "" {
		question = defaultQuestions[0]
	}

	var stdinInline *string
	var stdinPart *core.Part
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
			prepared.StagedPaths = append(prepared.StagedPaths, source.stagedPath)
		}
	}

	prepared.Message = buildAskInputMessage(question, files, stdinInline, stdinPart)
	prepared.Question = question
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
		items, staged, fileErr := prepareAskFilePath(path, askCfg)
		stagedPaths = append(stagedPaths, staged...)
		if fileErr != nil {
			return nil, stagedPaths, fileErr
		}
		prepared = append(prepared, items...)
	}
	return prepared, stagedPaths, nil
}

func prepareAskFilePath(path string, askCfg config.AskConfig) (prepared []askPreparedFile, stagedPaths []string, err error) {
	if strings.EqualFold(path, "clipboard") {
		content, readErr := clipboard.ReadText()
		if readErr != nil {
			return nil, nil, fmt.Errorf("failed to read clipboard: %w", readErr)
		}
		item, stagedPath, itemErr := prepareAskFileSource(askFileSource{
			displayPath: "clipboard",
			filename:    "clipboard.txt",
			raw:         []byte(content),
		}, askCfg)
		if itemErr != nil {
			return nil, nil, itemErr
		}
		if stagedPath != "" {
			stagedPaths = append(stagedPaths, stagedPath)
		}
		return []askPreparedFile{item}, stagedPaths, nil
	}

	spec, parseErr := input.ParseFileSpec(path)
	if parseErr != nil {
		return nil, nil, fmt.Errorf("invalid file spec %q: %w", path, parseErr)
	}
	expandedPath := expandAskPath(spec.Path)
	matches, globErr := filepath.Glob(expandedPath)
	if globErr != nil {
		return nil, nil, fmt.Errorf("invalid glob pattern %q: %w", spec.Path, globErr)
	}
	if len(matches) == 0 {
		if strings.ContainsAny(spec.Path, "*?[") {
			return nil, nil, nil
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

func prepareAskStructuredBytes(filename, sourceName string, raw []byte, maxBytes int64) (core.Part, string, error) {
	filename = sanitizeAskSourceFilename(filename)
	if mediaType, extension, width, height, ok := classifyAskImage(raw); ok {
		filename = askFilenameForMediaType(filename, mediaType, extension)
		path, err := uploads.SaveBytesWithLimit(filename, raw, maxBytes)
		if err != nil {
			return core.Part{}, "", fmt.Errorf("stage %s image: %w", sourceName, err)
		}
		sendBytes, sendMediaType := uploads.ResizeImageForLLMQuiet(raw, mediaType)
		if sendWidth, sendHeight := uploads.ImageDisplayDimensions(sendBytes); sendWidth > 0 && sendHeight > 0 {
			width, height = sendWidth, sendHeight
		}
		return core.Part{
			Type: core.PartImage,
			ImageData: &core.ToolImageData{
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
		mediaType = uploads.NormalizeMediaType(filename, http.DetectContentType(raw), raw)
	} else {
		mediaType = uploads.NormalizeMediaType(filename, http.DetectContentType(raw), raw)
		filename = askFilenameForMediaType(filename, mediaType, askUploadExtension(mediaType))
	}
	path, err := uploads.SaveBytesWithLimit(filename, raw, maxBytes)
	if err != nil {
		return core.Part{}, "", fmt.Errorf("stage %s file: %w", sourceName, err)
	}
	return core.Part{
		Type: core.PartFile,
		Text: llm.FormatUploadedFileNotice(filename, mediaType, path, int64(len(raw))),
		FileData: &core.ToolFileData{
			Filename:  filename,
			MediaType: mediaType,
			SizeBytes: int64(len(raw)),
		},
		FilePath: path,
	}, path, nil
}

func buildAskInputMessage(question string, files []askPreparedFile, stdinInline *string, stdinPart *core.Part) core.Message {
	parts := make([]core.Part, 0, len(files)+2)
	pending := make([]input.FileContent, 0, len(files))
	flushPending := func() {
		if len(pending) == 0 {
			return
		}
		parts = append(parts, core.Part{Type: core.PartText, Text: prompt.AskUserPrompt("", pending, "")})
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
			parts = append(parts, core.Part{Type: core.PartText, Text: question})
		}
	} else {
		stdin := ""
		if stdinInline != nil {
			stdin = *stdinInline
		}
		text := prompt.AskUserPrompt(question, pending, stdin)
		if text != "" || len(parts) == 0 {
			parts = append(parts, core.Part{Type: core.PartText, Text: text})
		}
	}
	return core.Message{Role: core.RoleUser, Parts: parts}
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
		var done bool
		raw, line, done = appendBoundedAskLines(raw, buf[:n], line, start, endLine, maxBytes)
		if done {
			return raw, nil
		}
		if readErr == io.EOF {
			return raw, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func appendBoundedAskLines(raw, chunk []byte, line, start, end int, maxBytes int64) ([]byte, int, bool) {
	for _, b := range chunk {
		selected := line >= start && (end == 0 || line <= end)
		if b == '\n' {
			if selected && (end == 0 || line < end) {
				raw = append(raw, b)
			}
			line++
			if end > 0 && line > end {
				return raw, line, true
			}
		} else if selected {
			raw = append(raw, b)
		}
		if int64(len(raw)) > maxBytes {
			return raw, line, true
		}
	}
	return raw, line, false
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
	mediaType = filemime.NormalizeMediaType(mediaType)
	currentExtension := filepath.Ext(filename)
	if currentExtension != "" && filemime.NormalizeMediaType(mime.TypeByExtension(strings.ToLower(currentExtension))) == mediaType {
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
	if w <= 0 || h <= 0 || w > uploads.MaxImagePixels || h > uploads.MaxImagePixels/w {
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
	mediaType = filemime.NormalizeMediaType(mediaType)
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

func askInputSummary(question string, message core.Message) string {
	if question = strings.TrimSpace(question); question != "" {
		return question
	}
	for _, part := range message.Parts {
		switch part.Type {
		case core.PartImage:
			return "stdin image"
		case core.PartFile:
			if part.FileData != nil && strings.TrimSpace(part.FileData.Filename) != "" {
				return "stdin: " + part.FileData.Filename
			}
			return "stdin file"
		}
	}
	return "stdin input"
}

func CleanupPreparedInput(prepared PreparedInput) {
	for _, path := range prepared.StagedPaths {
		_ = os.Remove(path)
	}
}

// AddAutoTools returns autoTools extended with the transient tools required to
// read the structured file and image parts of user messages.
func AddAutoTools(autoTools []string, messages []core.Message) []string {
	for _, message := range messages {
		if message.Role != core.RoleUser {
			continue
		}
		for _, part := range message.Parts {
			switch part.Type {
			case core.PartFile:
				autoTools = appendUniqueString(autoTools, tools.ReadFileToolName)
			case core.PartImage:
				autoTools = appendUniqueString(autoTools, tools.ReadFileToolName)
				autoTools = appendUniqueString(autoTools, tools.ViewImageToolName)
			}
		}
	}
	return autoTools
}

func appendUniqueString(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" || slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func pathWithinDir(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	if runtime.GOOS == "windows" {
		path, dir = strings.ToLower(path), strings.ToLower(dir)
	}
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// GrantUploadedFileReads grants only existing regular files referenced by
// structured user parts and canonically contained by the app-owned uploads dir.
func GrantUploadedFileReads(toolMgr *tools.ToolManager, messages []core.Message) {
	if toolMgr == nil || toolMgr.ApprovalMgr == nil {
		return
	}
	root := uploads.Dir()
	if root == "" {
		return
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return
	}
	for _, message := range messages {
		if message.Role != core.RoleUser {
			continue
		}
		for _, part := range message.Parts {
			path := ""
			switch part.Type {
			case core.PartFile:
				path = part.FilePath
			case core.PartImage:
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

// ApplyInlineMaxBytesOverride returns askCfg with the per-invocation
// --inline-max-bytes override applied. Values accept plain byte counts or
// human sizes (512K, 2M, 1MiB). The override may not exceed ask.stdin_max_bytes.
func ApplyInlineMaxBytesOverride(askCfg config.AskConfig, raw string) (config.AskConfig, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return askCfg, nil
	}
	parsed, err := humanize.ParseBytes(raw)
	if err != nil {
		return askCfg, fmt.Errorf("invalid --inline-max-bytes %q: %w", raw, err)
	}
	if parsed == 0 || parsed > uint64(math.MaxInt64) {
		return askCfg, fmt.Errorf("invalid --inline-max-bytes %q: expected a positive size", raw)
	}
	value := int64(parsed)
	if value > askCfg.StdinMaxBytes {
		return askCfg, fmt.Errorf("invalid --inline-max-bytes %q (%d bytes): must not exceed ask.stdin_max_bytes (%d)", raw, value, askCfg.StdinMaxBytes)
	}
	askCfg.StdinInlineMaxBytes = value
	return askCfg, nil
}

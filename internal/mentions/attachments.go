package mentions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
)

const (
	// MaxEagerFileBytes matches Claude Code's total-file-size gate.
	MaxEagerFileBytes int64 = 256 << 10
	// MaxEagerContentTokens is applied with term-llm's local four-bytes-per-token estimate.
	MaxEagerContentTokens = 25_000
	// MaxEagerFallbackLines is retried from the requested line when content exceeds the token ceiling.
	MaxEagerFallbackLines = 2_000
	// MaxEagerDirectoryEntries bounds the names retained from a non-recursive directory listing.
	MaxEagerDirectoryEntries = 1_000
)

var imageExtensions = map[string]struct{}{
	".avif": {}, ".bmp": {}, ".gif": {}, ".heic": {}, ".ico": {}, ".jfif": {},
	".jpeg": {}, ".jpg": {}, ".png": {}, ".svg": {}, ".tif": {}, ".tiff": {}, ".webp": {},
}

type eagerLoadOptions struct {
	openFile   func(secureMentionRoot, string) (*os.File, error)
	beforeOpen func(string)
}

// LoadEagerAttachments resolves ordinary textual mentions under root at submit
// time. Omitted, denied, oversized, binary, and failed resources are silently
// skipped so the unchanged textual references still reach the model. allowed,
// when non-nil, must make a non-interactive read-policy decision and must not
// grant permissions.
func LoadEagerAttachments(ctx context.Context, root, text string, allowed func(string) bool) []EagerAttachment {
	return loadEagerAttachments(ctx, root, text, allowed, eagerLoadOptions{})
}

func loadEagerAttachments(ctx context.Context, root, text string, allowed func(string) bool, opts eagerLoadOptions) []EagerAttachment {
	root, err := canonicalRoot(root)
	if err != nil {
		return nil
	}
	secureRoot, err := openSecureMentionRoot(root)
	if err != nil {
		return nil
	}
	defer secureRoot.Close()
	if opts.openFile == nil {
		opts.openFile = func(root secureMentionRoot, name string) (*os.File, error) {
			return root.Open(name)
		}
	}

	var attachments []EagerAttachment
	for _, mention := range ParseSubmitted(text) {
		if err := ctx.Err(); err != nil {
			break
		}
		resolved, display, err := resolveMentionPath(root, mention.Path)
		if err != nil {
			continue
		}
		relative := filepath.FromSlash(display)

		// Stat through the retained root descriptor before policy evaluation,
		// then require the opened descriptor to name that same object. This binds
		// approval and reading across final- and parent-component replacement.
		expected, err := secureRoot.Stat(relative)
		if err != nil {
			continue
		}
		if !expected.IsDir() && (!expected.Mode().IsRegular() || expected.Size() > MaxEagerFileBytes || isInlineMediaPath(resolved)) {
			continue
		}
		if allowed != nil && !allowed(resolved) {
			continue
		}
		if opts.beforeOpen != nil {
			opts.beforeOpen(resolved)
		}
		file, err := opts.openFile(secureRoot, relative)
		if err != nil {
			continue
		}
		actual, err := file.Stat()
		if err != nil || !os.SameFile(expected, actual) {
			file.Close()
			continue
		}
		if actual.IsDir() {
			attachment, err := loadDirectory(ctx, file, display)
			if err == nil {
				attachments = append(attachments, attachment)
			}
			continue
		}
		if !actual.Mode().IsRegular() || actual.Size() > MaxEagerFileBytes {
			file.Close()
			continue
		}
		attachment, err := loadTextFile(ctx, file, display, mention)
		if err == nil {
			attachments = append(attachments, attachment)
		}
	}
	return attachments
}

func canonicalRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("empty mention root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errors.New("mention root is not a directory")
	}
	return filepath.Clean(resolved), nil
}

func resolveMentionPath(root, mentioned string) (resolved, display string, err error) {
	mentioned = strings.TrimSuffix(mentioned, "/")
	if mentioned == "" || !utf8.ValidString(mentioned) || hasUnsafePathRune(mentioned) {
		return "", "", errors.New("invalid mention path")
	}
	candidate := filepath.FromSlash(mentioned)
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil || !pathWithinRoot(root, candidate) {
		return "", "", errors.New("mention path escapes root")
	}
	resolved, err = filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", "", err
	}
	resolved = filepath.Clean(resolved)
	if !pathWithinRoot(root, resolved) {
		return "", "", errors.New("mention symlink escapes root")
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", errors.New("invalid project-relative mention path")
	}
	display = filepath.ToSlash(relative)
	return resolved, display, nil
}

func hasUnsafePathRune(value string) bool {
	for _, r := range value {
		if r == 0 || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func pathWithinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func isInlineMediaPath(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".pdf" {
		return true
	}
	_, image := imageExtensions[ext]
	return image
}

func loadTextFile(ctx context.Context, file *os.File, display string, mention SubmittedMention) (EagerAttachment, error) {
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxEagerFileBytes {
		return EagerAttachment{}, errors.New("file changed before eager read")
	}
	content, err := io.ReadAll(io.LimitReader(file, MaxEagerFileBytes+1))
	if err != nil {
		return EagerAttachment{}, err
	}
	if int64(len(content)) > MaxEagerFileBytes || !utf8.Valid(content) || binarySample(content) {
		return EagerAttachment{}, errors.New("file is not eligible text")
	}
	if err := ctx.Err(); err != nil {
		return EagerAttachment{}, err
	}

	selected := content
	lineStart, lineEnd := 0, 0
	if mention.LineStart > 0 {
		selected, lineStart, lineEnd = selectLines(content, mention.LineStart, mention.LineEnd)
		if len(selected) == 0 {
			return EagerAttachment{}, errors.New("requested line range is empty")
		}
	}
	truncated := false
	// The local reader has no separate selected-range exception or successful
	// token-truncation result. Every full-file and ranged selection reaches this
	// common post-selection check, so either local equivalent receives Claude's
	// first-2,000-lines fallback.
	if llm.EstimateTokens(string(selected)) > MaxEagerContentTokens {
		fallbackStart := mention.LineStart
		if fallbackStart == 0 {
			fallbackStart = 1
		}
		selected, lineStart, lineEnd = selectLines(content, fallbackStart, fallbackStart+MaxEagerFallbackLines-1)
		if len(selected) == 0 || llm.EstimateTokens(string(selected)) > MaxEagerContentTokens {
			return EagerAttachment{}, errors.New("file exceeds eager token limit")
		}
		truncated = true
	}

	return EagerAttachment{
		Path: display, Kind: KindFile, Content: string(selected),
		LineStart: lineStart, LineEnd: lineEnd, Truncated: truncated,
	}, nil
}

func binarySample(content []byte) bool {
	if len(content) > 8<<10 {
		content = content[:8<<10]
	}
	return bytes.IndexByte(content, 0) >= 0
}

func selectLines(content []byte, start, end int) ([]byte, int, int) {
	if start < 1 || end < start {
		return nil, 0, 0
	}
	lines := bytes.SplitAfter(content, []byte{'\n'})
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if start > len(lines) {
		return nil, 0, 0
	}
	last := min(end, len(lines))
	return bytes.Join(lines[start-1:last], nil), start, last
}

func loadDirectory(ctx context.Context, file *os.File, display string) (EagerAttachment, error) {
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.IsDir() {
		return EagerAttachment{}, errors.New("directory changed before eager read")
	}

	names := make([]string, 0, MaxEagerDirectoryEntries+1)
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return EagerAttachment{}, err
		}
		batch, readErr := file.Readdirnames(256)
		for _, name := range batch {
			total++
			if len(names) < MaxEagerDirectoryEntries {
				names = append(names, name)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return EagerAttachment{}, readErr
		}
	}
	truncated := total > MaxEagerDirectoryEntries
	if truncated {
		names = append(names, fmt.Sprintf("… and %d more entries", total-MaxEagerDirectoryEntries))
	}
	if !strings.HasSuffix(display, "/") {
		display += "/"
	}
	return EagerAttachment{
		Path: display, Kind: KindDirectory, Content: strings.Join(names, "\n"),
		Truncated: truncated, EntryCount: total,
	}, nil
}

// FormatEagerAttachments appends submit-time context without changing the
// visible user text. It uses the existing generic embedded-file format rather
// than adding mention-specific persistence or provenance types.
func FormatEagerAttachments(attachments []EagerAttachment) string {
	if len(attachments) == 0 {
		return ""
	}
	var output strings.Builder
	output.WriteString("\n\n")
	for _, attachment := range attachments {
		name := attachment.Path
		if attachment.Kind == KindFile && attachment.LineStart > 0 {
			name += fmt.Sprintf("#L%d-%d", attachment.LineStart, attachment.LineEnd)
		}
		output.WriteString(llm.FormatEmbeddedFileText(name, "text/plain", attachment.Content))
	}
	return output.String()
}

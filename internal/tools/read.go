package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

// ReadFileTool implements the read_file tool.
type ReadFileTool struct {
	approval *ApprovalManager
	limits   OutputLimits
}

// NewReadFileTool creates a new ReadFileTool.
func NewReadFileTool(approval *ApprovalManager, limits OutputLimits) *ReadFileTool {
	return &ReadFileTool{
		approval: approval,
		limits:   limits,
	}
}

// ReadFileArgs are the arguments for read_file.
type ReadFileArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}

func (t *ReadFileTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        ReadFileToolName,
		Description: "Read file contents. Returns line-numbered output. Use start_line/end_line for pagination.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Absolute or relative path to the file to read",
				},
				"start_line": map[string]interface{}{
					"type":        "integer",
					"description": "1-indexed start line (default: 1)",
				},
				"end_line": map[string]interface{}{
					"type":        "integer",
					"description": "1-indexed end line (default: EOF)",
				},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		},
	}
}

func (t *ReadFileTool) Preview(args json.RawMessage) string {
	var a ReadFileArgs
	if err := json.Unmarshal(args, &a); err != nil || a.Path == "" {
		return ""
	}
	if a.StartLine > 0 && a.EndLine > 0 {
		return fmt.Sprintf("%s:%d-%d", a.Path, a.StartLine, a.EndLine)
	} else if a.StartLine > 0 {
		return fmt.Sprintf("%s:%d-", a.Path, a.StartLine)
	} else if a.EndLine > 0 {
		return fmt.Sprintf("%s:1-%d", a.Path, a.EndLine)
	}
	return a.Path
}

func (t *ReadFileTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	warning := WarnUnknownParams(args, []string{"path", "start_line", "end_line"})
	textOutput := func(message string) llm.ToolOutput {
		return llm.TextOutput(warning + message)
	}

	var a ReadFileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, err.Error()))), nil
	}

	if a.Path == "" {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, "path is required"))), nil
	}

	// Check permissions via approval manager
	if t.approval != nil {
		outcome, err := t.approval.CheckPathApproval(ReadFileToolName, a.Path, a.Path, false)
		if err != nil {
			if toolErr, ok := err.(*ToolError); ok {
				return textOutput(formatToolError(toolErr)), nil
			}
			return textOutput(formatToolError(NewToolError(ErrPermissionDenied, err.Error()))), nil
		}
		if outcome == Cancel {
			return textOutput(formatToolError(NewToolErrorf(ErrPermissionDenied, "access denied: %s", a.Path))), nil
		}
	}

	resolvedPath, err := resolveToolPath(a.Path, false)
	if err != nil {
		if toolErr, ok := err.(*ToolError); ok {
			return textOutput(formatToolError(toolErr)), nil
		}
		return textOutput(formatToolError(NewToolErrorf(ErrInvalidParams, "cannot resolve path: %v", err))), nil
	}

	output, err := readLineNumberedFile(ctx, resolvedPath, a.Path, a.StartLine, a.EndLine, t.limits)
	if err != nil {
		if toolErr, ok := err.(*ToolError); ok {
			return textOutput(formatToolError(toolErr)), nil
		}
		return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "read error: %v", err))), nil
	}

	return textOutput(output), nil
}

func readLineNumberedFile(ctx context.Context, resolvedPath, displayPath string, startLine, endLine int, limits OutputLimits) (string, error) {
	file, err := os.Open(resolvedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", NewToolError(ErrFileNotFound, displayPath)
		}
		return "", fmt.Errorf("open: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)

	// Check for binary file using only the sniffing prefix.  Peek leaves the
	// bytes buffered so paged reads can continue without a second read or seek,
	// and without requiring the path to be seekable.
	sample, err := reader.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return "", fmt.Errorf("read sample: %w", err)
	}
	if isBinaryContent(sample) {
		return "", NewToolErrorf(ErrBinaryFile, "%s appears to be a binary file", displayPath)
	}

	return streamLineNumberedRange(ctx, reader, startLine, endLine, limits)
}

func streamLineNumberedRange(ctx context.Context, reader *bufio.Reader, startLine, endLine int, limits OutputLimits) (string, error) {
	requestedStart := startLine
	if startLine <= 0 {
		startLine = 1
	}

	var sb strings.Builder
	if limits.MaxBytes > 0 {
		grow := limits.MaxBytes
		if grow > 4*1024 {
			grow = 4 * 1024
		}
		sb.Grow(int(grow))
	}

	totalLines := 0
	selectedLines := 0
	writtenLines := 0
	truncated := false
	byteCapped := false
	emittedAny := false
	lastEndedNewline := false

	appendOutput := func(s string) {
		if byteCapped {
			return
		}
		if limits.MaxBytes <= 0 {
			if len(s) > 0 {
				truncated = true
				byteCapped = true
			}
			return
		}
		remaining := int(limits.MaxBytes) - sb.Len()
		if remaining <= 0 {
			if len(s) > 0 {
				truncated = true
				byteCapped = true
			}
			return
		}
		if len(s) > remaining {
			sb.WriteString(s[:remaining])
			truncated = true
			byteCapped = true
			return
		}
		sb.WriteString(s)
	}

	processLine := func(line string) bool {
		totalLines++
		lineNum := totalLines

		inRange := lineNum >= startLine && (endLine <= 0 || lineNum <= endLine)
		if inRange {
			selectedLines++
			if selectedLines > limits.MaxLines {
				truncated = true
			} else {
				if writtenLines > 0 {
					appendOutput("\n")
				}
				appendOutput(strconv.Itoa(lineNum))
				appendOutput(": ")
				appendOutput(line)
				writtenLines++
			}
		}

		if truncated {
			return true // Keep scanning so the truncation note can report total lines.
		}
		if endLine > 0 {
			if startLine <= endLine && lineNum >= endLine {
				return false
			}
			if startLine > endLine && lineNum >= startLine {
				return false
			}
		}
		return true
	}

	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			emittedAny = true
			if strings.HasSuffix(line, "\n") {
				line = strings.TrimSuffix(line, "\n")
				lastEndedNewline = true
			} else {
				lastEndedNewline = false
			}
			if !processLine(line) {
				break
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if !emittedAny || lastEndedNewline {
					processLine("")
				}
				break
			}
			return "", readErr
		}
	}

	if requestedStart > 0 && startLine > totalLines {
		return "", NewToolErrorf(ErrInvalidParams, "start_line %d exceeds file length %d", requestedStart, totalLines)
	}

	if selectedLines == 0 {
		return "No content in requested range.", nil
	}

	output := sb.String()
	if truncated {
		output += fmt.Sprintf("\n\n[Output truncated. Total lines: %d. Use start_line/end_line for pagination.]", totalLines)
	}
	return output, nil
}

// isBinaryContent detects if content is binary using http.DetectContentType.
func isBinaryContent(data []byte) bool {
	if len(data) == 0 {
		return false
	}

	// Check first 512 bytes
	sample := data
	if len(sample) > 512 {
		sample = sample[:512]
	}

	contentType := http.DetectContentType(sample)

	// Text types are not binary
	if strings.HasPrefix(contentType, "text/") {
		return false
	}

	// application/json, application/xml, etc. are text-like
	if strings.Contains(contentType, "json") || strings.Contains(contentType, "xml") {
		return false
	}

	// Check for null bytes (common in binary)
	for _, b := range sample {
		if b == 0 {
			return true
		}
	}

	return false
}

// formatToolError formats a ToolError for LLM consumption.
func formatToolError(err *ToolError) string {
	return fmt.Sprintf("Error [%s]: %s", err.Type, err.Message)
}

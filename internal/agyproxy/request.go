package agyproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// BridgeServerName is the MCP server name term-llm registers with agy. agy
// exposes its always-loaded tools as native functions named
// mcp_<server>_<tool>; only those declarations survive filtering.
const BridgeServerName = "term-llm"

const (
	bridgeToolPrefix = "mcp_" + BridgeServerName + "_"

	// agySpillMarker precedes the spill path when agy replaces a large MCP
	// result with a file reference and no preview.
	agySpillMarker = "The output was large and was saved to: file://"
	// agyTruncationMarker precedes the spill path when agy keeps a preview with
	// the middle of a large tool result elided. The preview runs to the end of
	// the tool output.
	agyTruncationMarker   = "The tool's output was truncated due to being too long. You can find the full output in a file at file://"
	agyTruncationPreview  = "\n\nHere is a preview with the middle of the output truncated:\n"
	agyArtifactDelimiters = ".,;:)]}\"'`>"
	maxAgyArtifactSize    = 1 << 20
)

var agyTruncationElision = regexp.MustCompile(`\n<truncated ([0-9]+) of ([0-9]+) bytes from the middle of the output>\n`)

// ExpandGenerationArtifacts replaces agy's private spill and truncation
// notices in the model request with the original tool output. Only regular
// output.txt files in <artifactRoot>/<conversation>/.system_generated/steps/<n>/
// are eligible; missing, malformed, oversized, symlinked, or external paths,
// and previews that do not match the file, are left intact.
func ExpandGenerationArtifacts(body []byte, artifactRoot string) ([]byte, error) {
	artifactRoot = strings.TrimSpace(artifactRoot)
	text := string(body)
	if artifactRoot == "" || (!strings.Contains(text, agySpillMarker) && !strings.Contains(text, agyTruncationMarker)) {
		return body, nil
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("decode generation request for artifact expansion: %w", err)
	}
	request, ok := root["request"].(map[string]any)
	if !ok {
		return body, nil
	}
	if !expandAgyArtifactValue(request, artifactRoot) {
		return body, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode generation request after artifact expansion: %w", err)
	}
	return out, nil
}

func expandAgyArtifactValue(value any, artifactRoot string) bool {
	changed := false
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if text, ok := child.(string); ok {
				if expanded, replaced := expandAgyArtifactText(text, artifactRoot); replaced {
					value[key] = expanded
					changed = true
				}
				continue
			}
			if expandAgyArtifactValue(child, artifactRoot) {
				changed = true
			}
		}
	case []any:
		for index, child := range value {
			if text, ok := child.(string); ok {
				if expanded, replaced := expandAgyArtifactText(text, artifactRoot); replaced {
					value[index] = expanded
					changed = true
				}
				continue
			}
			if expandAgyArtifactValue(child, artifactRoot) {
				changed = true
			}
		}
	}
	return changed
}

// expandAgyArtifactText restores at most one notice format. Restored output
// is never rescanned, so a notice quoted inside a tool result stays literal.
func expandAgyArtifactText(text, artifactRoot string) (string, bool) {
	if expanded, ok := expandAgyTruncationNotice(text, artifactRoot); ok {
		return expanded, true
	}
	return expandAgySpillNotices(text, artifactRoot)
}

// expandAgyTruncationNotice restores a truncated tool result. The preview's
// head, elision count, and tail must all match the artifact, which guarantees
// the replaced span is exactly agy's notice plus preview.
func expandAgyTruncationNotice(text, artifactRoot string) (string, bool) {
	marker := strings.Index(text, agyTruncationMarker)
	if marker < 0 {
		return text, false
	}
	pathStart := marker + len(agyTruncationMarker)
	pathLen := strings.Index(text[pathStart:], agyTruncationPreview)
	if pathLen < 0 {
		return text, false
	}
	path := strings.TrimSuffix(text[pathStart:pathStart+pathLen], ".")
	preview := text[pathStart+pathLen+len(agyTruncationPreview):]
	content, ok := readAgyArtifact("file://"+path, artifactRoot)
	if !ok || !agyPreviewMatches(preview, content) {
		return text, false
	}
	return text[:marker] + content, true
}

// agyPreviewMatches tries every elision-shaped line because the tool output
// itself may quote one (for example, when reading a proxy trace).
func agyPreviewMatches(preview, content string) bool {
	for _, loc := range agyTruncationElision.FindAllStringSubmatchIndex(preview, -1) {
		elided, err := strconv.Atoi(preview[loc[2]:loc[3]])
		if err != nil {
			continue
		}
		total, err := strconv.Atoi(preview[loc[4]:loc[5]])
		if err != nil || total != len(content) {
			continue
		}
		head, tail := preview[:loc[0]], preview[loc[1]:]
		if len(head)+elided+len(tail) == total && strings.HasPrefix(content, head) && strings.HasSuffix(content, tail) {
			return true
		}
	}
	return false
}

func expandAgySpillNotices(text, artifactRoot string) (string, bool) {
	if !strings.Contains(text, agySpillMarker) {
		return text, false
	}
	var out strings.Builder
	changed := false
	for {
		marker := strings.Index(text, agySpillMarker)
		if marker < 0 {
			out.WriteString(text)
			return out.String(), changed
		}
		pathStart := marker + len(agySpillMarker)
		tokenEnd := pathStart
		for tokenEnd < len(text) && text[tokenEnd] > ' ' {
			tokenEnd++
		}
		pathEnd := tokenEnd
		for pathEnd > pathStart && strings.ContainsRune(agyArtifactDelimiters, rune(text[pathEnd-1])) {
			pathEnd--
		}
		out.WriteString(text[:marker])
		if content, ok := readAgyArtifact("file://"+text[pathStart:pathEnd], artifactRoot); ok {
			out.WriteString(content)
			out.WriteString(text[pathEnd:tokenEnd])
			changed = true
		} else {
			out.WriteString(text[marker:tokenEnd])
		}
		text = text[tokenEnd:]
	}
}

func readAgyArtifact(rawURL, artifactRoot string) (string, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "file" || (parsed.Host != "" && parsed.Host != "localhost") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	root, err := filepath.EvalSymlinks(artifactRoot)
	if err != nil {
		return "", false
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", false
	}
	candidate, err := filepath.Abs(filepath.Clean(parsed.Path))
	if err != nil {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", false
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) != 5 || parts[0] == "" || parts[1] != ".system_generated" || parts[2] != "steps" || parts[4] != "output.txt" {
		return "", false
	}
	if step, err := strconv.Atoi(parts[3]); err != nil || step < 0 {
		return "", false
	}
	linkInfo, err := os.Lstat(candidate)
	if err != nil || linkInfo.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	file, err := os.Open(candidate)
	if err != nil {
		return "", false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxAgyArtifactSize {
		return "", false
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil || !os.SameFile(info, resolvedInfo) {
		return "", false
	}
	content, err := io.ReadAll(io.LimitReader(file, maxAgyArtifactSize+1))
	if err != nil || len(content) > maxAgyArtifactSize {
		return "", false
	}
	return string(content), true
}

// FilterGenerationRequest keeps only term-llm bridge function declarations
// and drops every other tool agy adds. Unknown request fields are preserved.
// Requests without a tools field are auxiliary generations (titles,
// summaries) and are forwarded unchanged.
func FilterGenerationRequest(body []byte, requireMCP bool) ([]byte, error) {
	out, _, err := filterGenerationRequest(body, requireMCP)
	return out, err
}

// filterGenerationRequest also reports whether the request exposed bridge
// tools. Callers use that evidence to distinguish an agent generation from
// agy's tool-less auxiliary generations.
func filterGenerationRequest(body []byte, requireMCP bool) ([]byte, bool, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, false, fmt.Errorf("decode generation request: %w", err)
	}
	request, ok := root["request"].(map[string]any)
	if !ok {
		return nil, false, errors.New("generation request missing request object")
	}
	rawTools, exists := request["tools"]
	if !exists {
		return body, false, nil
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return nil, false, errors.New("generation request has invalid tools array")
	}
	kept, err := bridgeDeclarations(tools)
	if err != nil {
		return nil, false, err
	}
	if requireMCP && len(kept) == 0 {
		return nil, false, errors.New("generation request missing term-llm bridge tools")
	}
	if len(kept) == 0 {
		delete(request, "tools")
		delete(request, "toolConfig")
	} else {
		request["tools"] = []any{map[string]any{"functionDeclarations": kept}}
		filterAllowedFunctionNames(request, kept)
	}
	cleanAgyContents(request)
	out, err := json.Marshal(root)
	if err != nil {
		return nil, false, fmt.Errorf("encode generation request: %w", err)
	}
	return out, len(kept) > 0, nil
}

func bridgeDeclarations(tools []any) ([]any, error) {
	seen := make(map[string]bool)
	var kept []any
	for _, raw := range tools {
		group, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		decls, ok := group["functionDeclarations"].([]any)
		if !ok {
			continue
		}
		for _, d := range decls {
			decl, ok := d.(map[string]any)
			if !ok {
				continue
			}
			name, _ := decl["name"].(string)
			if !strings.HasPrefix(name, bridgeToolPrefix) || len(name) == len(bridgeToolPrefix) {
				continue
			}
			if seen[name] {
				return nil, fmt.Errorf("generation request contains duplicate %s declarations", name)
			}
			seen[name] = true
			cleanBridgeDeclaration(decl)
			kept = append(kept, decl)
		}
	}
	return kept, nil
}

// filterAllowedFunctionNames drops forced-choice references to removed tools
// so the upstream request stays valid. A forced choice that only named removed
// tools is cleared rather than redirected to a bridge tool.
func filterAllowedFunctionNames(request map[string]any, kept []any) {
	toolConfig, ok := request["toolConfig"].(map[string]any)
	if !ok {
		return
	}
	calling, ok := toolConfig["functionCallingConfig"].(map[string]any)
	if !ok {
		return
	}
	allowed, ok := calling["allowedFunctionNames"].([]any)
	if !ok {
		return
	}
	names := make(map[string]bool, len(kept))
	for _, decl := range kept {
		name, _ := decl.(map[string]any)["name"].(string)
		names[name] = true
	}
	filtered := make([]any, 0, len(allowed))
	for _, raw := range allowed {
		if name, _ := raw.(string); names[name] {
			filtered = append(filtered, raw)
		}
	}
	if len(filtered) == 0 {
		delete(request, "toolConfig")
		return
	}
	calling["allowedFunctionNames"] = filtered
}

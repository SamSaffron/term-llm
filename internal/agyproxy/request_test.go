package agyproxy

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func generationBody(names ...string) []byte {
	decls := make([]any, 0, len(names))
	for _, name := range names {
		decls = append(decls, map[string]any{"name": name, "description": "test"})
	}
	body, _ := json.Marshal(map[string]any{"model": "test", "request": map[string]any{"tools": []any{map[string]any{"functionDeclarations": decls}}, "contents": []any{"preserved"}}})
	return body
}

func generationBodyWithText(text string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model": "test",
		"request": map[string]any{
			"contents": []any{map[string]any{
				"parts": []any{map[string]any{"text": text}},
			}},
		},
	})
	return body
}

func firstPartText(t *testing.T, body []byte) string {
	t.Helper()
	var request struct {
		Request struct {
			Contents []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	return request.Request.Contents[0].Parts[0].Text
}

func fileURL(path string) string { return (&url.URL{Scheme: "file", Path: path}).String() }

func agySpillNotice(path string) string {
	return "The output was large and was saved to: " + fileURL(path)
}

// agyTruncationNotice reproduces agy 1.3's notice for always-loaded MCP tools.
// Like agy, it keeps about keep bytes at each end, cut on rune boundaries,
// and reports byte counts.
func agyTruncationNotice(path, content string, keep int) string {
	headEnd, tailStart := keep, len(content)-keep
	for headEnd > 0 && !utf8.RuneStart(content[headEnd]) {
		headEnd--
	}
	for tailStart < len(content) && !utf8.RuneStart(content[tailStart]) {
		tailStart++
	}
	head, tail := content[:headEnd], content[tailStart:]
	return "The tool's output was truncated due to being too long. You can find the full output in a file at " + fileURL(path) + ".\n\n" +
		"Here is a preview with the middle of the output truncated:\n" + head +
		fmt.Sprintf("\n<truncated %d of %d bytes from the middle of the output>\n", len(content)-len(head)-len(tail), len(content)) + tail
}

func writeArtifact(t *testing.T, root, step, content string) string {
	t.Helper()
	path := filepath.Join(root, "conversation-1", ".system_generated", "steps", step, "output.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExpandGenerationArtifactsRehydratesPrivateSpill(t *testing.T) {
	root := filepath.Join(t.TempDir(), "brain")
	content := "full tool result\nwith details"
	artifact := writeArtifact(t, root, "3", content)
	body := generationBodyWithText("metadata\n(" + agySpillNotice(artifact) + ").\nafter")

	expanded, err := ExpandGenerationArtifacts(body, root)
	if err != nil {
		t.Fatal(err)
	}
	want := "metadata\n(" + content + ").\nafter"
	if got := firstPartText(t, expanded); got != want {
		t.Fatalf("expanded text = %q, want %q", got, want)
	}
}

func TestExpandGenerationArtifactsRestoresTruncatedPreview(t *testing.T) {
	root := filepath.Join(t.TempDir(), "brain")
	// agy (a Go binary) reports UTF-8 byte counts; multibyte text must validate.
	content := "1: package main ├── 日本語 🚀\n" + strings.Repeat("middle line ünïcødé\n", 50) + "99: } ✓\n"
	artifact := writeArtifact(t, root, "2", content)
	prefix := "Created At: now\nCompleted At: now\n"
	body := generationBodyWithText(prefix + agyTruncationNotice(artifact, content, 20))

	expanded, err := ExpandGenerationArtifacts(body, root)
	if err != nil {
		t.Fatal(err)
	}
	if got := firstPartText(t, expanded); got != prefix+content {
		t.Fatalf("expanded text = %q, want %q", got, prefix+content)
	}
}

func TestExpandGenerationArtifactsRestoresPreviewQuotingElision(t *testing.T) {
	root := filepath.Join(t.TempDir(), "brain")
	quoted := "\n<truncated 5 of 10 bytes from the middle of the output>\n"
	content := "trace:" + quoted + strings.Repeat("body\n", 40) + "end\n"
	artifact := writeArtifact(t, root, "2", content)
	body := generationBodyWithText(agyTruncationNotice(artifact, content, len("trace:"+quoted)+4))

	expanded, err := ExpandGenerationArtifacts(body, root)
	if err != nil {
		t.Fatal(err)
	}
	if got := firstPartText(t, expanded); got != content {
		t.Fatalf("expanded text = %q, want %q", got, content)
	}
}

func TestExpandGenerationArtifactsDoesNotRescanRestoredOutput(t *testing.T) {
	root := filepath.Join(t.TempDir(), "brain")
	other := writeArtifact(t, root, "1", "other artifact")
	content := "quoted: " + agySpillNotice(other) + "\n" + strings.Repeat("padding\n", 30)
	artifact := writeArtifact(t, root, "2", content)
	body := generationBodyWithText(agyTruncationNotice(artifact, content, 12))

	expanded, err := ExpandGenerationArtifacts(body, root)
	if err != nil {
		t.Fatal(err)
	}
	if got := firstPartText(t, expanded); got != content {
		t.Fatalf("restored output = %q, want it byte-for-byte", got)
	}
}

func TestExpandGenerationArtifactsKeepsMismatchedPreview(t *testing.T) {
	root := filepath.Join(t.TempDir(), "brain")
	content := "head of output\n" + strings.Repeat("x", 200) + "\ntail of output\n"
	artifact := writeArtifact(t, root, "2", content)
	for name, text := range map[string]string{
		"edited head":     strings.Replace(agyTruncationNotice(artifact, content, 20), "head of", "HEAD of", 1),
		"trailing text":   agyTruncationNotice(artifact, content, 20) + "\nappended by agy",
		"wrong total":     strings.Replace(agyTruncationNotice(artifact, content, 20), fmt.Sprintf("of %d bytes", len(content)), "of 1 bytes", 1),
		"missing elision": strings.Replace(agyTruncationNotice(artifact, content, 20), "<truncated", "<cut", 1),
	} {
		t.Run(name, func(t *testing.T) {
			body := generationBodyWithText(text)
			expanded, err := ExpandGenerationArtifacts(body, root)
			if err != nil {
				t.Fatal(err)
			}
			if string(expanded) != string(body) {
				t.Fatalf("mismatched preview was expanded: %s", expanded)
			}
		})
	}
}

func TestExpandGenerationArtifactsRejectsUnsafeSpills(t *testing.T) {
	root := filepath.Join(t.TempDir(), "brain")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "output.txt")
	if err := os.WriteFile(external, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "conversation-1", ".system_generated", "steps", "4", "output.txt")
	wrongShape := filepath.Join(root, "conversation-1", "output.txt")
	oversized := writeArtifact(t, root, "6", string(make([]byte, maxAgyArtifactSize+1)))
	for _, path := range []string{external, missing, wrongShape, oversized} {
		for _, notice := range []string{agySpillNotice(path), agyTruncationNotice(path, "secret-secret-secret", 3)} {
			body := generationBodyWithText(notice)
			expanded, err := ExpandGenerationArtifacts(body, root)
			if err != nil {
				t.Fatalf("path %q: %v", path, err)
			}
			if string(expanded) != string(body) {
				t.Fatalf("unsafe path %q was expanded: %s", path, expanded)
			}
		}
	}

	symlink := filepath.Join(root, "conversation-1", ".system_generated", "steps", "5", "output.txt")
	if err := os.MkdirAll(filepath.Dir(symlink), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, symlink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	body := generationBodyWithText(agySpillNotice(symlink))
	expanded, err := ExpandGenerationArtifacts(body, root)
	if err != nil {
		t.Fatal(err)
	}
	if string(expanded) != string(body) {
		t.Fatalf("symlinked artifact was expanded: %s", expanded)
	}
}

func declarationNames(t *testing.T, body []byte) []string {
	t.Helper()
	var root struct {
		Request struct {
			Tools []struct {
				FunctionDeclarations []struct {
					Name string `json:"name"`
				} `json:"functionDeclarations"`
			} `json:"tools"`
		} `json:"request"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, group := range root.Request.Tools {
		for _, decl := range group.FunctionDeclarations {
			names = append(names, decl.Name)
		}
	}
	return names
}

func TestFilterGenerationRequestKeepsOnlyBridgeTools(t *testing.T) {
	body := generationBody("view_file", "mcp_term-llm_read_file", "list_resources", "read_resource", "manage_task", "call_mcp_tool", "mcp_other_tool", "mcp_term-llm_grep")
	out, verified, err := filterGenerationRequest(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if !verified {
		t.Fatal("bridge generation was not verified")
	}
	if got := strings.Join(declarationNames(t, out), ","); got != "mcp_term-llm_read_file,mcp_term-llm_grep" {
		t.Fatalf("declarations = %s", got)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["request"].(map[string]any)["contents"]; !ok {
		t.Fatal("unrelated request field was removed")
	}
}

func TestFilterGenerationRequestDropsNonFunctionToolGroups(t *testing.T) {
	body := []byte(`{"request":{"tools":[{"googleSearch":{}},{"functionDeclarations":[{"name":"mcp_term-llm_glob"}]}]}}`)
	out, err := FilterGenerationRequest(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "googleSearch") {
		t.Fatalf("non-bridge tool group survived: %s", out)
	}
}

func TestFilterGenerationRequestPrunesForcedChoice(t *testing.T) {
	body := []byte(`{"request":{"tools":[{"functionDeclarations":[{"name":"mcp_term-llm_glob"},{"name":"run_command"}]}],` +
		`"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["run_command","mcp_term-llm_glob"]}}}}`)
	out, err := FilterGenerationRequest(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "run_command") || !strings.Contains(string(out), `"allowedFunctionNames":["mcp_term-llm_glob"]`) {
		t.Fatalf("forced choice not pruned: %s", out)
	}

	body = []byte(`{"request":{"tools":[{"functionDeclarations":[{"name":"mcp_term-llm_glob"},{"name":"run_command"}]}],` +
		`"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["run_command"]}}}}`)
	out, err = FilterGenerationRequest(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "toolConfig") {
		t.Fatalf("forced choice of a removed tool survived: %s", out)
	}
}

func TestFilterGenerationRequestFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"malformed", []byte("{")},
		{"missing request", []byte(`{"requestId":"x"}`)},
		{"invalid tools", []byte(`{"request":{"tools":{}}}`)},
		{"missing bridge tools", generationBody("run_command", "call_mcp_tool")},
		{"bare bridge prefix", generationBody("mcp_term-llm_")},
		{"duplicate bridge tool", generationBody("mcp_term-llm_grep", "mcp_term-llm_grep")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := FilterGenerationRequest(tc.body, true); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestFilterGenerationRequestRemovesAllToolsWhenBridgeNotRequired(t *testing.T) {
	body := []byte(`{"request":{"tools":[{"functionDeclarations":[{"name":"manage_task"}]}],"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}}}}`)
	out, verified, err := filterGenerationRequest(body, false)
	if err != nil {
		t.Fatal(err)
	}
	if verified || strings.Contains(string(out), "tools") || strings.Contains(string(out), "toolConfig") {
		t.Fatalf("verified=%v request=%s", verified, out)
	}
}

func TestFilterGenerationRequestForwardsToollessAuxiliaryUnchanged(t *testing.T) {
	body := []byte(`{"request":{"contents":["preserved"]}}`)
	for _, required := range []bool{true, false} {
		out, verified, err := filterGenerationRequest(body, required)
		if err != nil {
			t.Fatal(err)
		}
		if verified || string(out) != string(body) {
			t.Fatalf("required=%v verified=%v out=%s", required, verified, out)
		}
	}
}

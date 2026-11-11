package agyproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BenchmarkGenerationArtifactPipeline compares the actual bounded production
// path against composing the exported wrappers, including the old expanded-body
// cap check. Both read the same local artifact on each iteration: no models,
// network, persistent cache, or audit files are needed.
func BenchmarkGenerationArtifactPipeline(b *testing.B) {
	for _, tc := range []struct {
		name                      string
		historyBytes, resultBytes int
		lazy                      bool
	}{
		{"32KiB_artifact_256KiB_history", 256 << 10, 32 << 10, false},
		{"150KiB_artifact_2MiB_history", 2 << 20, 150 << 10, false},
		{"32KiB_lazy_artifact_256KiB_history", 256 << 10, 32 << 10, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			root := b.TempDir()
			path := filepath.Join(root, "conversation-1", ".system_generated", "steps", "2", "output.txt")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				b.Fatal(err)
			}
			// Eager truncation stores timestamps IN the artifact; lazy spills
			// put them before the file notice instead.
			const timestamps = "Created At: 2026-11-12T08:50:47+11:00\nCompleted At: 2026-11-12T08:50:47+11:00\n"
			content := strings.Repeat("tool output line\n", tc.resultBytes/17)
			if !tc.lazy {
				content = timestamps + content
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				b.Fatal(err)
			}
			contents := make([]any, 0, 65)
			for i := 0; i < 64; i++ {
				role := "user"
				if i%2 != 0 {
					role = "model"
				}
				contents = append(contents, map[string]any{"role": role, "parts": []any{
					map[string]any{"text": strings.Repeat("conversation history line\n", tc.historyBytes/(64*26))},
					map[string]any{"functionCall": map[string]any{"name": "mcp_term-llm_shell", "args": map[string]any{"id": json.Number("9007199254740993"), "command": "echo hi"}}},
				}})
			}
			name, output := "mcp_term-llm_shell", agyTruncationNotice(path, content, 4000)
			if tc.lazy {
				name, output = "call_mcp_tool", timestamps+agySpillNotice(path)
				contents = append(contents, map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{
					"id": "legacy-call", "name": name, "args": map[string]any{"ServerName": BridgeServerName, "ToolName": "shell", "Arguments": map[string]any{}},
				}}}})
			}
			contents = append(contents, map[string]any{"role": "model", "parts": []any{map[string]any{"functionResponse": map[string]any{
				"id": "legacy-call", "name": name, "response": map[string]any{"output": output},
			}}}})
			body, err := json.Marshal(map[string]any{"model": "test", "request": map[string]any{
				"contents": contents,
				"tools": []any{map[string]any{"functionDeclarations": []any{
					map[string]any{"name": "run_command"},
					map[string]any{"name": "mcp_term-llm_shell", "description": agyToolDescriptionPrefix + "Execute a shell command."},
				}}},
			}})
			if err != nil {
				b.Fatal(err)
			}
			unified := func() ([]byte, error) {
				out, _, err := processGenerationRequest(body, root, true, maxRequestBody)
				return out, err
			}
			composed := func() ([]byte, error) {
				expanded, err := ExpandGenerationArtifacts(body, root)
				if err != nil {
					return nil, err
				}
				if len(expanded) > maxRequestBody {
					return nil, fmt.Errorf("oversized expanded request")
				}
				return FilterGenerationRequest(expanded, true)
			}
			one, err := unified()
			if err != nil {
				b.Fatal(err)
			}
			two, err := composed()
			if err != nil || !bytes.Equal(one, two) {
				b.Fatalf("pipeline mismatch: %v", err)
			}
			for _, pipeline := range []struct {
				name string
				run  func() ([]byte, error)
			}{{"unified", unified}, {"composed", composed}} {
				b.Run(pipeline.name, func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(len(body)))
					for b.Loop() {
						if _, err := pipeline.run(); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}

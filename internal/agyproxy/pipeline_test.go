package agyproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestExpandGenerationArtifactsOnlyBridgeResultSlots(t *testing.T) {
	root := t.TempDir()
	artifact := writeArtifact(t, root, "2", "restored tool output")
	for _, notice := range []string{agySpillNotice(artifact), agyTruncationNotice(artifact, "restored tool output", 3)} {
		// Include a genuine result so preservation is tested across re-encoding,
		// not just the unchanged-body fast path.
		body := []byte(`{"request":{
			"systemInstruction":{"parts":[{"text":` + mustJSON(notice) + `}]},
			"tools":[{"functionDeclarations":[{"name":"mcp_term-llm_shell","description":` + mustJSON(notice) + `,
				"parameters":{"type":"OBJECT","properties":{"example":{"const":` + mustJSON(notice) + `}}}}]}],
			"contents":[
				{"role":"user","parts":[{"text":` + mustJSON(notice) + `}]},
				{"role":"model","parts":[{"text":` + mustJSON(notice) + `},
					{"functionCall":{"name":"mcp_term-llm_shell","args":{"command":` + mustJSON(notice) + `}}}]},
				{"role":"model","parts":[{"functionResponse":{"name":"mcp_term-llm_shell","response":{"output":` + mustJSON(notice) + `,"other":` + mustJSON(notice) + `}}}]},
				{"role":"model","parts":[{"functionResponse":{"name":"mcp_other_shell","response":{"output":` + mustJSON(notice) + `}}}]},
				{"role":"model","parts":[{"functionResponse":{"name":"mcp_term-llm_","response":{"output":` + mustJSON(notice) + `}}}]}
			]}}`)
		for _, pipeline := range []struct {
			name string
			run  func([]byte) ([]byte, error)
		}{
			{"expansion", func(body []byte) ([]byte, error) { return ExpandGenerationArtifacts(body, root) }},
			{"production", func(body []byte) ([]byte, error) {
				out, _, err := processGenerationRequest(body, root, true, maxRequestBody)
				return out, err
			}},
		} {
			t.Run(pipeline.name, func(t *testing.T) {
				out, err := pipeline.run(body)
				if err != nil {
					t.Fatal(err)
				}
				if got := bytes.Count(out, []byte(mustJSON(notice))); got != 9 {
					t.Fatalf("preserved notice slots = %d, want 9: %s", got, out)
				}
				if got := bytes.Count(out, []byte(`"restored tool output"`)); got != 1 {
					t.Fatalf("restored slots = %d, want 1: %s", got, out)
				}
			})
		}
	}
}

func TestExpandGenerationArtifactsRequiresNoticeEnvelope(t *testing.T) {
	root := t.TempDir()
	content := "head" + strings.Repeat("middle", 30) + "tail"
	path := writeArtifact(t, root, "2", content)
	for _, notice := range []string{agySpillNotice(path), agyTruncationNotice(path, content, 4)} {
		for _, text := range []string{
			"Quoted notice: " + notice,
			"before\n" + notice,
			"```\n" + notice + "\n```",
			`"` + notice + `"`,
			notice + "\nafter",
			"Created At: now\nCompleted At: now\nLog: " + notice,
		} {
			body := generationBodyWithOutput(text)
			out, err := ExpandGenerationArtifacts(body, root)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out, body) {
				t.Fatalf("quoted notice was rewritten: %s", out)
			}
		}
	}
}

func TestExpandGenerationArtifactsLegacyRequiresBridgeCall(t *testing.T) {
	root := t.TempDir()
	path := writeArtifact(t, root, "2", "legacy result")
	notice := "Created At: now\nCompleted At: now\n" + agySpillNotice(path)
	for _, tc := range []struct {
		name, call, responseID string
		restored               bool
	}{
		{"matching bridge", `{"id":"call_123","name":"call_mcp_tool","args":{"Arguments":{},"ServerName":"term-llm","ToolName":"shell"}}`, "call_123", true},
		{"another server", `{"id":"call_123","name":"call_mcp_tool","args":{"ServerName":"other","ToolName":"shell"}}`, "call_123", false},
		{"wrong ID", `{"id":"call_123","name":"call_mcp_tool","args":{"ServerName":"term-llm","ToolName":"shell"}}`, "call_456", false},
		{"no ID", `{"name":"call_mcp_tool","args":{"ServerName":"term-llm","ToolName":"shell"}}`, "", false},
		{"no tool", `{"id":"call_123","name":"call_mcp_tool","args":{"ServerName":"term-llm"}}`, "call_123", false},
		{"no call", `{}`, "call_123", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"request":{"contents":[
				{"role":"model","parts":[{"functionCall":` + tc.call + `}]},
				{"role":"model","parts":[{"functionResponse":{"id":` + mustJSON(tc.responseID) + `,"name":"call_mcp_tool","response":{"output":` + mustJSON(notice) + `}}}]}
			]}}`)
			out, err := ExpandGenerationArtifacts(body, root)
			if err != nil {
				t.Fatal(err)
			}
			if changed := !bytes.Equal(body, out); changed != tc.restored {
				t.Fatalf("restored=%v, want %v: %s", changed, tc.restored, out)
			}
			if tc.restored && !bytes.Contains(out, []byte(mustJSON("Created At: now\nCompleted At: now\nlegacy result"))) {
				t.Fatalf("legacy output not restored: %s", out)
			}
		})
	}
}

func TestGenerationTransformsPreserveLargeIntegers(t *testing.T) {
	root := t.TempDir()
	path := writeArtifact(t, root, "2", "full tool output")
	// Exercise every decode/re-encode entry point, with integers in unknown
	// fields, schemas, generation config, historical args, and tool results.
	const number = "9007199254740993"
	body := []byte(`{"counter":` + number + `,"request":{
		"generationConfig":{"seed":` + number + `},
		"tools":[{"functionDeclarations":[{"name":"mcp_term-llm_shell","parameters":{"maximum":` + number + `}}]}],
		"contents":[{"role":"model","parts":[
			{"functionCall":{"name":"mcp_term-llm_shell","args":{"id":` + number + `}}},
			{"functionResponse":{"name":"mcp_term-llm_shell","response":{"count":` + number + `,"output":` + mustJSON(agySpillNotice(path)) + `}}}
		]}]}}`)
	for name, transform := range map[string]func([]byte) ([]byte, error){
		"expand": func(body []byte) ([]byte, error) { return ExpandGenerationArtifacts(body, root) },
		"filter": func(body []byte) ([]byte, error) { return FilterGenerationRequest(body, true) },
		"unified": func(body []byte) ([]byte, error) {
			out, _, err := processGenerationRequest(body, root, true, maxRequestBody)
			return out, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := transform(body)
			if err != nil {
				t.Fatal(err)
			}
			if got := bytes.Count(out, []byte(number)); got != 5 {
				t.Fatalf("exact large integers = %d, want 5: %s", got, out)
			}
		})
	}
}

func TestGenerationTransformsRejectTrailingJSON(t *testing.T) {
	root := t.TempDir()
	path := writeArtifact(t, root, "2", "restored")
	body := generationBodyWithOutput(agySpillNotice(path))
	for name, transform := range map[string]func([]byte) ([]byte, error){
		"expand":          func(body []byte) ([]byte, error) { return ExpandGenerationArtifacts(body, root) },
		"expand disabled": func(body []byte) ([]byte, error) { return ExpandGenerationArtifacts(body, "") },
		"filter":          func(body []byte) ([]byte, error) { return FilterGenerationRequest(body, true) },
		"unified": func(body []byte) ([]byte, error) {
			out, _, err := processGenerationRequest(body, root, true, maxRequestBody)
			return out, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, suffix := range []string{" {}", " true", " null", " junk"} {
				if _, err := transform(append(bytes.Clone(body), suffix...)); err == nil {
					t.Fatalf("accepted trailing %q", suffix)
				}
			}
			if _, err := transform(append(bytes.Clone(body), " \n\t"...)); err != nil {
				t.Fatalf("rejected trailing whitespace: %v", err)
			}
		})
	}
}

func TestProcessGenerationRequestMatchesComposedWrappers(t *testing.T) {
	root := t.TempDir()
	timestamps := "Created At: 2026-11-12T08:50:47+11:00\nCompleted At: 2026-11-12T08:50:47+11:00\n"
	for _, inside := range []bool{false, true} {
		content, prefix := strings.Repeat("full result 日本語\n", 100), timestamps
		if inside {
			content, prefix = timestamps+content, ""
		}
		path := writeArtifact(t, root, "2", content)
		for _, output := range []string{prefix + agySpillNotice(path), prefix + agyTruncationNotice(path, content, 100)} {
			for _, auxiliary := range []bool{false, true} {
				t.Run(fmt.Sprintf("timestampsInside=%v/auxiliary=%v/truncation=%v", inside, auxiliary, strings.Contains(output, agyTruncationMarker)), func(t *testing.T) {
					body := generationBodyWithOutput(output)
					if !auxiliary {
						body = bytes.Replace(body, []byte(`"request":{`), []byte(`"request":{"tools":[{"functionDeclarations":[{"name":"run_command"},{"name":"mcp_term-llm_shell"}]}],`), 1)
					}
					expanded, err := ExpandGenerationArtifacts(body, root)
					if err != nil {
						t.Fatal(err)
					}
					composed, err := FilterGenerationRequest(expanded, true)
					if err != nil {
						t.Fatal(err)
					}
					unified, verified, err := processGenerationRequest(body, root, true, maxRequestBody)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(unified, composed) || verified == auxiliary {
						t.Fatalf("verified=%v unified=%s composed=%s", verified, unified, composed)
					}
					want := strings.TrimPrefix(content, timestamps)
					if auxiliary {
						want = prefix + content // auxiliary history is restored but not cleaned
					}
					if got := firstOutput(t, unified); got != want {
						t.Fatalf("output=%q, want %q", got, want)
					}
				})
			}
		}
	}
}

func TestProcessGenerationRequestSizeLimits(t *testing.T) {
	root := t.TempDir()
	path := writeArtifact(t, root, "2", strings.Repeat("<\x00\xff", 300))
	body := generationBodyWithOutput(agySpillNotice(path))
	// Filtering discards this declaration, but the expanded cap must still
	// count it (and JSON escaping), just as the old pre-filter Marshal did.
	body = bytes.Replace(body, []byte(`"request":{`), []byte(`"request":{"tools":[{"functionDeclarations":[{"name":"run_command","description":`+mustJSON(strings.Repeat("x", 1000))+`},{"name":"mcp_term-llm_shell"}]}],`), 1)
	expanded, err := ExpandGenerationArtifacts(body, root)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := FilterGenerationRequest(expanded, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) >= len(expanded) || len(body) >= len(filtered) {
		t.Fatal("fixture must shrink during filtering and grow during expansion")
	}
	for _, tc := range []struct {
		name  string
		limit int
		err   string
	}{
		{"input", len(body) - 1, "oversized generation request"},
		{"expanded", len(filtered), "oversized expanded generation request"},
		{"one byte over", len(expanded) - 1, "oversized expanded generation request"},
		{"exact expanded limit", len(expanded), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := processGenerationRequest(body, root, true, tc.limit)
			if tc.err == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("error=%v, want %s", err, tc.err)
			}
		})
	}
}

func TestGenerationJSONSizeMatchesMarshal(t *testing.T) {
	stringsToCount := []string{"", "ascii", "\"\\\b\f\n\r\t", "<>&", "日本語 🚀 \u2028\u2029", "\xff\xfe\ufffd"}
	for i := 0; i < 256; i++ {
		stringsToCount = append(stringsToCount, string([]byte{byte(i)}))
	}
	for _, value := range stringsToCount {
		object := map[string]any{value: []any{value, nil, true, false, json.Number("9007199254740993"), json.Number("-1.2e+30"), map[string]any{}, []any{}}}
		encoded, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		if got := generationJSONSize(object); got != len(encoded) {
			t.Fatalf("value=%q encoded size=%d, want %d (%s)", value, got, len(encoded), encoded)
		}
	}
}

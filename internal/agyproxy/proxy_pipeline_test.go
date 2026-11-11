package agyproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForwardGenerationPipelineAndAuxiliaryEvidence(t *testing.T) {
	root := t.TempDir()
	timestamps := "Created At: 2026-11-12T08:50:59+11:00\nCompleted At: 2026-11-12T08:50:59+11:00\n"
	content := timestamps + strings.Repeat("full artifact line\n", 100)
	path := writeArtifact(t, root, "2", content)
	artifactBody := generationBodyWithOutput(agyTruncationNotice(path, content, 100))
	agentBody := bytes.Replace(artifactBody, []byte(`"request":{`), []byte(`"request":{"seed":9007199254740993,"tools":[{"functionDeclarations":[{"name":"run_command"},{"name":"mcp_term-llm_shell"}]}],`), 1)
	legacyContent := strings.Repeat("legacy artifact line\n", 100)
	legacyPath := writeArtifact(t, root, "3", legacyContent)
	legacyBody := []byte(`{"request":{"tools":[{"functionDeclarations":[{"name":"mcp_term-llm_shell"}]}],"contents":[
		{"role":"model","parts":[{"functionCall":{"id":"legacy-call","name":"call_mcp_tool","args":{"Arguments":{"size":32000},"ServerName":"term-llm","ToolName":"shell"}}}]},
		{"role":"model","parts":[{"functionResponse":{"id":"legacy-call","name":"call_mcp_tool","response":{"output":` + mustJSON(timestamps+agySpillNotice(legacyPath)) + `}}}]}
	]}}`)
	auxiliary := []byte(`{ "request": {"contents":[{"role":"user","parts":[{"text":` + mustJSON(agySpillNotice(path)) + `}]}],"seed":9007199254740993}}`)
	for _, tc := range []struct {
		name       string
		body       []byte
		requireMCP bool
		count      int64
	}{
		{"agent", agentBody, true, 1},
		{"agent with lazy history", legacyBody, true, 1},
		{"title", auxiliary, true, 0},
		{"summary with historical artifact", artifactBody, true, 0},
		{"auxiliary without enforcement", auxiliary, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
			t.Setenv(GenerationTraceFileEnv, tracePath)
			expanded, err := ExpandGenerationArtifacts(tc.body, root)
			if err != nil {
				t.Fatal(err)
			}
			want, err := FilterGenerationRequest(expanded, tc.requireMCP)
			if err != nil {
				t.Fatal(err)
			}
			transport := &http.Transport{}
			t.Cleanup(transport.CloseIdleConnections)
			called := false
			transport.RegisterProtocol("https", roundTripFunc(func(req *http.Request) (*http.Response, error) {
				called = true
				got, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) || req.ContentLength != int64(len(want)) {
					t.Fatalf("forwarded body=%s length=%d, want %s", got, req.ContentLength, want)
				}
				if req.Header.Get("Content-Encoding") != "" || req.Header.Get("Transfer-Encoding") != "" || len(req.TransferEncoding) != 0 {
					t.Fatalf("stale encoding headers: %+v transfer=%v", req.Header, req.TransferEncoding)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("streamed"))}, nil
			}))
			server := &Server{transport: transport, artifactRoot: root, requireMCP: tc.requireMCP}
			req := httptest.NewRequest(http.MethodPost, "https://"+CloudCodeHost+generationPath+"?alt=sse", bytes.NewReader(tc.body))
			req.Header.Set("Content-Encoding", "identity")
			req.Header.Set("Transfer-Encoding", "chunked")
			req.TransferEncoding = []string{"chunked"}
			recorder := httptest.NewRecorder()
			server.forward(recorder, req)
			if !called || recorder.Code != http.StatusOK || recorder.Body.String() != "streamed" || server.FilteredGenerations() != tc.count {
				t.Fatalf("called=%v code=%d body=%s evidence=%d", called, recorder.Code, recorder.Body.String(), server.FilteredGenerations())
			}
			trace, err := os.ReadFile(tracePath)
			if err != nil {
				t.Fatal(err)
			}
			var record generationTraceRecord
			if err := json.Unmarshal(trace, &record); err != nil {
				t.Fatal(err)
			}
			var original, forwarded bytes.Buffer
			if err := json.Compact(&original, tc.body); err != nil {
				t.Fatal(err)
			}
			if err := json.Compact(&forwarded, want); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(record.OriginalRequest, original.Bytes()) || !bytes.Equal(record.ForwardedRequest, forwarded.Bytes()) || record.Path != generationPath+"?alt=sse" {
				t.Fatalf("trace lost original/forwarded distinction: %s", trace)
			}
		})
	}
}

func TestForwardGenerationPipelineFailsClosed(t *testing.T) {
	t.Setenv(GenerationTraceFileEnv, "")
	for _, body := range [][]byte{
		generationBody("call_mcp_tool"),
		generationBody("run_command"),
		generationBody("mcp_term-llm_"),
		generationBody("mcp_term-llm_shell", "mcp_term-llm_shell"),
		[]byte(`{"request":{"tools":{}}}`),
		[]byte(`{"request":{}} {}`),
		[]byte(`{"request":{}} junk`),
	} {
		transport := &http.Transport{}
		transport.RegisterProtocol("https", roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("rejected generation reached upstream")
			return nil, nil
		}))
		server := &Server{transport: transport, requireMCP: true}
		req := httptest.NewRequest(http.MethodPost, "https://"+CloudCodeHost+generationPath, bytes.NewReader(body))
		recorder := httptest.NewRecorder()
		server.forward(recorder, req)
		transport.CloseIdleConnections()
		if recorder.Code != http.StatusBadGateway || server.FilteredGenerations() != 0 {
			t.Fatalf("code=%d evidence=%d for %s", recorder.Code, server.FilteredGenerations(), body)
		}
	}
}

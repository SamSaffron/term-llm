package agyproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/cliwire"
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
			"tools": []any{},
			"contents": []any{map[string]any{
				"parts": []any{map[string]any{"text": text}},
			}},
		},
	})
	return body
}

func agyArtifactNotice(path string) string {
	return "The output was large and was saved to: " + (&url.URL{Scheme: "file", Path: path}).String()
}

func TestExpandGenerationArtifactsRehydratesPrivateSpill(t *testing.T) {
	root := filepath.Join(t.TempDir(), "brain")
	artifact := filepath.Join(root, "conversation-1", ".system_generated", "steps", "3", "output.txt")
	if err := os.MkdirAll(filepath.Dir(artifact), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "full tool result\nwith details"
	if err := os.WriteFile(artifact, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	body := generationBodyWithText("metadata\n(" + agyArtifactNotice(artifact) + ").\nafter")

	expanded, err := ExpandGenerationArtifacts(body, root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(expanded), "output.txt") || !strings.Contains(string(expanded), "full tool result") {
		t.Fatalf("expanded request = %s", expanded)
	}
	var request struct {
		Request struct {
			Contents []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(expanded, &request); err != nil {
		t.Fatal(err)
	}
	want := "metadata\n(" + content + ").\nafter"
	if got := request.Request.Contents[0].Parts[0].Text; got != want {
		t.Fatalf("expanded text = %q, want %q", got, want)
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
	oversized := filepath.Join(root, "conversation-1", ".system_generated", "steps", "6", "output.txt")
	if err := os.MkdirAll(filepath.Dir(oversized), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oversized, make([]byte, maxAgyArtifactSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{external, missing, wrongShape, oversized} {
		body := generationBodyWithText(agyArtifactNotice(path))
		expanded, err := ExpandGenerationArtifacts(body, root)
		if err != nil {
			t.Fatalf("path %q: %v", path, err)
		}
		if string(expanded) != string(body) {
			t.Fatalf("unsafe path %q was expanded: %s", path, expanded)
		}
	}

	symlink := filepath.Join(root, "conversation-1", ".system_generated", "steps", "5", "output.txt")
	if err := os.MkdirAll(filepath.Dir(symlink), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, symlink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	body := generationBodyWithText(agyArtifactNotice(symlink))
	expanded, err := ExpandGenerationArtifacts(body, root)
	if err != nil {
		t.Fatal(err)
	}
	if string(expanded) != string(body) {
		t.Fatalf("symlinked artifact was expanded: %s", expanded)
	}
}

func TestFilterGenerationRequestKeepsOnlyMCPDispatcher(t *testing.T) {
	out, err := FilterGenerationRequest(generationBody("run_command", "call_mcp_tool", "write_to_file"), true)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	request := root["request"].(map[string]any)
	tools := request["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools length = %d, want 1", len(tools))
	}
	decls := tools[0].(map[string]any)["functionDeclarations"].([]any)
	if len(decls) != 1 || decls[0].(map[string]any)["name"] != "call_mcp_tool" {
		t.Fatalf("declarations = %#v", decls)
	}
	if _, ok := request["contents"]; !ok {
		t.Fatal("unrelated request field was removed")
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
		{"missing dispatcher", generationBody("run_command")},
		{"duplicate dispatcher", generationBody("call_mcp_tool", "call_mcp_tool")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := FilterGenerationRequest(tc.body, true); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestFilterGenerationRequestAllowsNoDispatcherWhenNotRequired(t *testing.T) {
	out, err := FilterGenerationRequest(generationBody("run_command"), false)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Request struct {
			Tools []any `json:"tools"`
		} `json:"request"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Request.Tools) != 0 {
		t.Fatalf("tools = %#v, want empty", root.Request.Tools)
	}
}

func TestFilterGenerationRequestAllowsToollessAuxiliaryWhenMCPRequired(t *testing.T) {
	out, verified, err := filterGenerationRequest([]byte(`{"request":{"contents":["preserved"]}}`), true)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Request struct {
			Tools    []any `json:"tools"`
			Contents []any `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if verified || len(root.Request.Tools) != 0 || len(root.Request.Contents) != 1 {
		t.Fatalf("verified = %v, request = %#v", verified, root.Request)
	}

	_, verified, err = filterGenerationRequest(generationBody("call_mcp_tool"), true)
	if err != nil {
		t.Fatal(err)
	}
	if !verified {
		t.Fatal("MCP dispatcher generation was not verified")
	}
}

func TestFilterGenerationRequestAllowsMissingToolsWhenNotRequired(t *testing.T) {
	out, err := FilterGenerationRequest([]byte(`{"request":{"contents":["preserved"]}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Request struct {
			Tools    []any `json:"tools"`
			Contents []any `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Request.Tools) != 0 || len(root.Request.Contents) != 1 {
		t.Fatalf("request = %#v", root.Request)
	}
}

func TestGenerationTraceWritesOriginalAndForwardedRequests(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "nested", "agy-requests.jsonl")
	t.Setenv(GenerationTraceFileEnv, tracePath)
	original := generationBody("run_command", "call_mcp_tool")
	forwarded, err := FilterGenerationRequest(original, true)
	if err != nil {
		t.Fatal(err)
	}
	var server Server
	if err := server.traceGenerationRequest(generationPath, original, forwarded); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	var record generationTraceRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode trace %q: %v", data, err)
	}
	if record.Path != generationPath || record.PID != os.Getpid() || record.Timestamp == "" {
		t.Fatalf("trace metadata = %+v", record)
	}
	if string(record.OriginalRequest) != string(original) || string(record.ForwardedRequest) != string(forwarded) {
		t.Fatalf("trace requests differ: original=%s forwarded=%s", record.OriginalRequest, record.ForwardedRequest)
	}
	info, err := os.Stat(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("trace mode = %o, want 600", info.Mode().Perm())
	}
}

func TestGenerationTraceRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	tracePath := filepath.Join(dir, "trace.jsonl")
	if err := os.Symlink(target, tracePath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv(GenerationTraceFileEnv, tracePath)
	if err := new(Server).traceGenerationRequest(generationPath, generationBody(), generationBody()); err == nil {
		t.Fatal("trace writer accepted symlink")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "unchanged" {
		t.Fatalf("trace target was modified: %q", data)
	}
}

func TestServerRequiresProxyAuthentication(t *testing.T) {
	var server Server
	proxyURL, _, err := server.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop(context.Background())
	u, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("CONNECT " + CloudCodeHost + ":443 HTTP/1.1\r\nHost: " + CloudCodeHost + ":443\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if status != "HTTP/1.1 407 Proxy Authentication Required\r\n" {
		t.Fatalf("status = %q", status)
	}
}

func TestServerLifecycleRemovesCA(t *testing.T) {
	var server Server
	url, caPath, err := server.Start()
	if err != nil {
		t.Fatal(err)
	}
	if url == "" {
		t.Fatal("empty proxy URL")
	}
	info, err := os.Stat(caPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("CA mode = %o, want 600", info.Mode().Perm())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(caPath); !os.IsNotExist(err) {
		t.Fatalf("CA remains after Stop: %v", err)
	}
	if err := server.Stop(ctx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

type blockingReadCloser struct {
	io.Reader
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (r *blockingReadCloser) Read(p []byte) (int, error) {
	r.once.Do(func() {
		close(r.started)
		<-r.release
	})
	return r.Reader.Read(p)
}

func (*blockingReadCloser) Close() error { return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestServerStopDoesNotBreakInFlightForward(t *testing.T) {
	t.Setenv(GenerationTraceFileEnv, "")
	var server Server
	if _, _, err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	server.mu.Lock()
	transport := server.transport
	server.mu.Unlock()
	transport.RegisterProtocol("https", roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("forwarded")),
		}, nil
	}))

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBody := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseBody)
	body := &blockingReadCloser{
		Reader:  strings.NewReader(string(generationBody("run_command"))),
		started: started,
		release: release,
	}
	req := httptest.NewRequest(http.MethodPost, "https://"+CloudCodeHost+generationPath, body)
	recorder := httptest.NewRecorder()
	panicResult := make(chan any, 1)
	go func() {
		defer func() { panicResult <- recover() }()
		server.forward(recorder, req)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not start reading request body")
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	releaseBody()

	select {
	case panicValue := <-panicResult:
		if panicValue != nil {
			t.Fatalf("forward panicked after Stop: %v", panicValue)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not finish")
	}
	if recorder.Code != http.StatusOK || recorder.Body.String() != "forwarded" {
		t.Fatalf("response = (%d, %q), want (200, %q)", recorder.Code, recorder.Body.String(), "forwarded")
	}

	stoppedReq := httptest.NewRequest(http.MethodGet, "https://"+CloudCodeHost+"/stopped", nil)
	stoppedRecorder := httptest.NewRecorder()
	server.forward(stoppedRecorder, stoppedReq)
	if stoppedRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("post-stop response status = %d, want %d", stoppedRecorder.Code, http.StatusServiceUnavailable)
	}
}

func TestServerChainsNonCloudTrafficThroughWireAudit(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("chain-ok"))
	}))
	defer upstream.Close()
	upstreamCA := filepath.Join(t.TempDir(), "upstream.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw})
	if err := os.WriteFile(upstreamCA, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	wire, err := cliwire.StartWithAdditionalCA(t.TempDir(), "agy-bin", upstreamCA)
	if err != nil {
		t.Fatal(err)
	}
	defer wire.Stop(context.Background())
	var server Server
	server.SetUpstream(wire.ProxyURL(), wire.CAPath())
	proxyText, caPath, err := server.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop(context.Background())

	proxyURL, _ := url.Parse(proxyText)
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("agy chained CA bundle contained no certificates")
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots}}
	client := &http.Client{Transport: transport}
	resp, err := client.Get(upstream.URL + "/chained")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "chain-ok" {
		t.Fatalf("response = %q", body)
	}
	transport.CloseIdleConnections()
	if err := server.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := wire.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests, _ := filepath.Glob(filepath.Join(wire.TraceDir(), "connections", "*-request.bin"))
	if len(requests) != 1 {
		t.Fatalf("wire requests = %v", requests)
	}
	captured, err := os.ReadFile(requests[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(captured), "GET /chained") {
		t.Fatalf("wire capture = %q", captured)
	}
}

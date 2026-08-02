package agyproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"testing"
	"time"
)

func generationBody(names ...string) []byte {
	decls := make([]any, 0, len(names))
	for _, name := range names {
		decls = append(decls, map[string]any{"name": name, "description": "test"})
	}
	body, _ := json.Marshal(map[string]any{"model": "test", "request": map[string]any{"tools": []any{map[string]any{"functionDeclarations": decls}}, "contents": []any{"preserved"}}})
	return body
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
		{"missing tools", []byte(`{"request":{}}`)},
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

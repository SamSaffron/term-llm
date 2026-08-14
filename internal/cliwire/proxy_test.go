package cliwire

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerCapturesDecryptedHTTPSStreams(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("response:" + string(body)))
	}))
	defer upstream.Close()

	upstreamCA := filepath.Join(t.TempDir(), "upstream.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw})
	if err := os.WriteFile(upstreamCA, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	traceRoot := filepath.Join(t.TempDir(), "wire")
	server, err := StartWithAdditionalCA(traceRoot, "test-bin", upstreamCA)
	if err != nil {
		t.Fatal(err)
	}

	proxyURL, err := url.Parse(server.ProxyURL())
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(server.CAPath())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("wire CA bundle contained no certificates")
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots}}
	client := &http.Client{Transport: transport}
	resp, err := client.Post(upstream.URL+"/v1/messages", "application/json", strings.NewReader(`{"prompt":"WIRE_MARKER"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != `response:{"prompt":"WIRE_MARKER"}` {
		t.Fatalf("response = %q", got)
	}
	transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	matches, err := filepath.Glob(filepath.Join(server.TraceDir(), "connections", "*-request.bin"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("request captures = %v, err = %v", matches, err)
	}
	request, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(request), "POST /v1/messages") || !strings.Contains(string(request), "WIRE_MARKER") {
		t.Fatalf("request capture did not contain decrypted request: %q", request)
	}
	responses, _ := filepath.Glob(filepath.Join(server.TraceDir(), "connections", "*-response.bin"))
	response, err := os.ReadFile(responses[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response), "response:") {
		t.Fatalf("response capture did not contain decrypted response: %q", response)
	}
	assertPrivateMode(t, server.TraceDir(), 0o700)
	assertPrivateMode(t, matches[0], 0o600)
	assertPrivateMode(t, filepath.Join(server.TraceDir(), "events.jsonl"), 0o600)
	if _, err := os.Stat(server.CAPath()); !os.IsNotExist(err) {
		t.Fatalf("temporary CA still exists after Stop: %v", err)
	}
}

func TestServerRequiresProxyAuthentication(t *testing.T) {
	server, err := Start(t.TempDir(), "test-bin")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop(context.Background())
	proxyURL, _ := url.Parse(server.ProxyURL())
	proxyURL.User = nil
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	resp, err := client.Get("https://example.com/")
	if err == nil {
		resp.Body.Close()
		t.Fatal("unauthenticated proxy request unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "Proxy Authentication Required") {
		t.Fatalf("error = %v, want proxy authentication rejection", err)
	}
}

func TestStartRejectsSymlinkTraceRoot(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "trace")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Start(link, "test-bin"); err == nil {
		t.Fatal("expected symlink trace root rejection")
	}
}

func assertPrivateMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

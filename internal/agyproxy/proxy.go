// Package agyproxy contains the temporary request-filter compatibility layer
// needed while agy cannot combine MCP with an empty native-tool set.
//
// Keep this package independent of internal/llm. Once agy supports that
// combination directly, the provider can replace its agyToolIsolation
// implementation and this package can be deleted.
package agyproxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	CloudCodeHost  = "daily-cloudcode-pa.googleapis.com"
	generationPath = "/v1internal:streamGenerateContent"
	maxRequestBody = 128 << 20

	// GenerationTraceFileEnv enables an opt-in JSONL trace containing the exact
	// generation request received from agy and the artifact-rehydrated, tool-
	// filtered request sent upstream. These requests contain full conversation
	// content and must be treated as sensitive.
	GenerationTraceFileEnv = "TERM_LLM_AGY_PROXY_TRACE_FILE"

	agyArtifactMarker     = "The output was large and was saved to: file://"
	agyArtifactDelimiters = ".,;:)]}\"'`>"
	maxAgyArtifactSize    = 1 << 20
)

// Server is a loopback-only HTTPS forward proxy. It intercepts only the Cloud
// Code host; CONNECT requests for every other host are tunneled without TLS
// termination.
type Server struct {
	mu           sync.Mutex
	traceMu      sync.Mutex
	server       *http.Server
	listener     net.Listener
	transport    *http.Transport
	caCert       *x509.Certificate
	caKey        *ecdsa.PrivateKey
	caPath       string
	proxyToken   string
	requireMCP   bool
	artifactRoot string
	filtered     atomic.Int64
	connections  map[net.Conn]struct{}
	running      bool
}

// Start starts the proxy and writes its public CA certificate to a private
// temporary file. The CA private key remains in memory.
func (s *Server) Start() (proxyURL, caPath string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return "", "", errors.New("agy proxy already running")
	}
	cert, key, certPEM, err := newCA()
	if err != nil {
		return "", "", err
	}
	certPEM, err = appendSystemRoots(certPEM)
	if err != nil {
		return "", "", err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", "", fmt.Errorf("generate agy proxy token: %w", err)
	}
	proxyToken := base64.RawURLEncoding.EncodeToString(tokenBytes)
	dir, err := os.MkdirTemp("", "term-llm-agy-proxy-")
	if err != nil {
		return "", "", fmt.Errorf("create agy proxy CA directory: %w", err)
	}
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, certPEM, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", "", fmt.Errorf("write agy proxy CA: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.RemoveAll(dir)
		return "", "", fmt.Errorf("listen for agy proxy: %w", err)
	}
	s.caCert, s.caKey, s.caPath, s.proxyToken = cert, key, path, proxyToken
	s.listener = ln
	s.connections = make(map[net.Conn]struct{})
	s.transport = &http.Transport{Proxy: nil}
	server := &http.Server{Handler: http.HandlerFunc(s.serveHTTP), ReadHeaderTimeout: 10 * time.Second}
	s.server = server
	s.running = true
	go func() { _ = server.Serve(ln) }()
	proxy := &url.URL{Scheme: "http", Host: ln.Addr().String(), User: url.UserPassword("term-llm", proxyToken)}
	return proxy.String(), path, nil
}

// SetRequireMCP controls whether generation requests lacking call_mcp_tool are
// rejected. Providers should enable this for turns exposing term-llm tools.
func (s *Server) SetRequireMCP(required bool) {
	s.mu.Lock()
	s.requireMCP = required
	s.mu.Unlock()
}

func (s *Server) requireMCPTool() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.requireMCP }

// SetArtifactRoot restricts agy spill-file rehydration to the provider's
// private antigravity-cli/brain directory. The directory may not exist yet when
// configured; every candidate is resolved and containment-checked at read time.
func (s *Server) SetArtifactRoot(root string) {
	s.mu.Lock()
	s.artifactRoot = strings.TrimSpace(root)
	s.mu.Unlock()
}

func (s *Server) artifactRootPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.artifactRoot
}

// BeginTurn resets interception evidence and configures dispatcher enforcement.
func (s *Server) BeginTurn(requireMCP bool) {
	s.filtered.Store(0)
	s.SetRequireMCP(requireMCP)
}

// FilteredGenerations reports successfully rewritten generation requests in the current turn.
func (s *Server) FilteredGenerations() int64 { return s.filtered.Load() }

func (s *Server) authorized(header string) bool {
	const prefix = "Basic "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	s.mu.Lock()
	token := s.proxyToken
	s.mu.Unlock()
	want := "term-llm:" + token
	return subtle.ConstantTimeCompare(decoded, []byte(want)) == 1
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r.Header.Get("Proxy-Authorization")) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="term-llm-agy"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
		return
	}
	host := r.URL.Hostname()
	if host != CloudCodeHost {
		s.tunnel(w, r)
		return
	}
	s.intercept(w, r)
}

func (s *Server) tunnel(w http.ResponseWriter, r *http.Request) {
	upstream, err := net.DialTimeout("tcp", r.Host, 15*time.Second)
	if err != nil {
		http.Error(w, "proxy connect failed", http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "hijacking unavailable", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	s.trackConnection(client)
	s.trackConnection(upstream)
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() { proxyCopy(upstream, client); s.untrackConnection(upstream); s.untrackConnection(client) }()
	proxyCopy(client, upstream)
	s.untrackConnection(client)
	s.untrackConnection(upstream)
}

func proxyCopy(dst, src net.Conn) { _, _ = io.Copy(dst, src); _ = dst.Close(); _ = src.Close() }

func (s *Server) trackConnection(conn net.Conn) {
	s.mu.Lock()
	if s.connections != nil {
		s.connections[conn] = struct{}{}
	}
	s.mu.Unlock()
}
func (s *Server) untrackConnection(conn net.Conn) {
	s.mu.Lock()
	delete(s.connections, conn)
	s.mu.Unlock()
}

func (s *Server) intercept(w http.ResponseWriter, r *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unavailable", http.StatusInternalServerError)
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	s.trackConnection(conn)
	defer s.untrackConnection(conn)
	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	leaf, err := s.leafCertificate(CloudCodeHost)
	if err != nil {
		conn.Close()
		return
	}
	tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12})
	if err := tlsConn.Handshake(); err != nil {
		tlsConn.Close()
		return
	}
	server := &http.Server{Handler: http.HandlerFunc(s.forward), ReadHeaderTimeout: 10 * time.Second}
	_ = server.Serve(&singleConnListener{conn: tlsConn})
}

type singleConnListener struct {
	mu   sync.Mutex
	conn net.Conn
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return nil, io.EOF
	}
	conn := l.conn
	l.conn = nil
	return conn, nil
}
func (l *singleConnListener) Close() error   { return nil }
func (l *singleConnListener) Addr() net.Addr { return loopbackAddr("agy-proxy") }

type loopbackAddr string

func (a loopbackAddr) Network() string { return "tcp" }
func (a loopbackAddr) String() string  { return string(a) }

func (s *Server) forward(w http.ResponseWriter, req *http.Request) {
	req.RequestURI = ""
	req.URL.Scheme = "https"
	req.URL.Host = CloudCodeHost
	req.Host = CloudCodeHost
	if isGenerationPath(req.URL.Path) {
		body, err := io.ReadAll(io.LimitReader(req.Body, maxRequestBody+1))
		req.Body.Close()
		if err != nil {
			http.Error(w, "agy tool-filter proxy rejected request: "+err.Error(), http.StatusBadGateway)
			return
		}
		if len(body) > maxRequestBody {
			http.Error(w, "agy tool-filter proxy rejected oversized request", http.StatusBadGateway)
			return
		}
		expanded, err := ExpandGenerationArtifacts(body, s.artifactRootPath())
		if err != nil {
			http.Error(w, "agy tool-filter proxy rejected artifact expansion: "+err.Error(), http.StatusBadGateway)
			return
		}
		if len(expanded) > maxRequestBody {
			http.Error(w, "agy tool-filter proxy rejected oversized expanded request", http.StatusBadGateway)
			return
		}
		filtered, err := FilterGenerationRequest(expanded, s.requireMCPTool())
		if err != nil {
			http.Error(w, "agy tool-filter proxy rejected request: "+err.Error(), http.StatusBadGateway)
			return
		}
		if err := s.traceGenerationRequest(req.URL.RequestURI(), body, filtered); err != nil {
			http.Error(w, "agy tool-filter proxy could not write request trace: "+err.Error(), http.StatusBadGateway)
			return
		}
		req.Body = io.NopCloser(strings.NewReader(string(filtered)))
		req.ContentLength = int64(len(filtered))
		req.Header.Del("Content-Encoding")
		req.Header.Del("Transfer-Encoding")
		req.TransferEncoding = nil
		s.filtered.Add(1)
	}
	resp, err := s.transport.RoundTrip(req)
	if err != nil {
		http.Error(w, "forward agy request: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

type generationTraceRecord struct {
	Timestamp        string          `json:"timestamp"`
	PID              int             `json:"pid"`
	Path             string          `json:"path"`
	OriginalRequest  json.RawMessage `json:"original_request"`
	ForwardedRequest json.RawMessage `json:"forwarded_request"`
}

func (s *Server) traceGenerationRequest(requestPath string, original, forwarded []byte) error {
	tracePath := strings.TrimSpace(os.Getenv(GenerationTraceFileEnv))
	if tracePath == "" {
		return nil
	}
	record, err := json.Marshal(generationTraceRecord{
		Timestamp:        time.Now().UTC().Format(time.RFC3339Nano),
		PID:              os.Getpid(),
		Path:             requestPath,
		OriginalRequest:  json.RawMessage(original),
		ForwardedRequest: json.RawMessage(forwarded),
	})
	if err != nil {
		return fmt.Errorf("encode trace record: %w", err)
	}
	record = append(record, '\n')

	s.traceMu.Lock()
	defer s.traceMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(tracePath), 0o700); err != nil {
		return fmt.Errorf("create trace directory: %w", err)
	}
	if info, err := os.Lstat(tracePath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("trace path is not a safe regular file")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect trace path: %w", err)
	}
	file, err := os.OpenFile(tracePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open trace file: %w", err)
	}
	closeWithError := func(err error) error {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			return fmt.Errorf("close trace file: %w", closeErr)
		}
		return err
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() {
		return closeWithError(errors.New("opened trace path is not a regular file"))
	}
	pathInfo, err := os.Lstat(tracePath)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() || !os.SameFile(openedInfo, pathInfo) {
		return closeWithError(errors.New("trace path changed while opening"))
	}
	if err := file.Chmod(0o600); err != nil {
		return closeWithError(fmt.Errorf("secure trace file: %w", err))
	}
	if _, err := file.Write(record); err != nil {
		return closeWithError(fmt.Errorf("append trace record: %w", err))
	}
	return closeWithError(nil)
}

// Stop shuts down the proxy and removes its temporary CA material.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	srv, transport, caPath := s.server, s.transport, s.caPath
	connections := make([]net.Conn, 0, len(s.connections))
	for conn := range s.connections {
		connections = append(connections, conn)
	}
	s.running = false
	s.server = nil
	s.listener = nil
	s.transport = nil
	s.caCert = nil
	s.caKey = nil
	s.caPath = ""
	s.proxyToken = ""
	s.artifactRoot = ""
	s.connections = nil
	s.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	if transport != nil {
		transport.CloseIdleConnections()
	}
	if caPath != "" {
		_ = os.RemoveAll(filepath.Dir(caPath))
	}
	return err
}

func appendSystemRoots(proxyCA []byte) ([]byte, error) {
	candidates := []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/ssl/cert.pem",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/ca-bundle.pem",
		"/etc/pki/tls/cacert.pem",
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	}
	for _, path := range candidates {
		roots, err := os.ReadFile(path)
		if err != nil || len(roots) == 0 {
			continue
		}
		bundle := make([]byte, 0, len(roots)+len(proxyCA)+1)
		bundle = append(bundle, roots...)
		if len(bundle) > 0 && bundle[len(bundle)-1] != '\n' {
			bundle = append(bundle, '\n')
		}
		bundle = append(bundle, proxyCA...)
		return bundle, nil
	}
	return nil, errors.New("locate system CA bundle for agy proxy")
}

func newCA() (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate agy proxy CA key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "term-llm agy proxy"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, err
	}
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func (s *Server) leafCertificate(host string) (tls.Certificate, error) {
	s.mu.Lock()
	ca, key := s.caCert, s.caKey
	s.mu.Unlock()
	if ca == nil || key == nil {
		return tls.Certificate{}, errors.New("agy proxy CA unavailable")
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &leafKey.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func isGenerationPath(path string) bool {
	return path == generationPath || strings.HasSuffix(path, ":generateContent") || strings.HasSuffix(path, ":streamGenerateContent")
}

// ExpandGenerationArtifacts replaces agy's private spill-file notices in the
// model request with their original contents. Only regular output.txt files in
// <artifactRoot>/<conversation>/.system_generated/steps/<number>/ are eligible;
// missing, malformed, oversized, symlinked, or external paths are left intact.
func ExpandGenerationArtifacts(body []byte, artifactRoot string) ([]byte, error) {
	artifactRoot = strings.TrimSpace(artifactRoot)
	if artifactRoot == "" || !strings.Contains(string(body), agyArtifactMarker) {
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
				expanded, replaced := expandAgyArtifactNotices(text, artifactRoot)
				if replaced {
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
				expanded, replaced := expandAgyArtifactNotices(text, artifactRoot)
				if replaced {
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

func expandAgyArtifactNotices(text, artifactRoot string) (string, bool) {
	var out strings.Builder
	changed := false
	for {
		marker := strings.Index(text, agyArtifactMarker)
		if marker < 0 {
			out.WriteString(text)
			return out.String(), changed
		}
		pathStart := marker + len(agyArtifactMarker)
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

// FilterGenerationRequest removes every function declaration except the real
// agy MCP dispatcher. Unknown fields are preserved.
func FilterGenerationRequest(body []byte, requireMCP bool) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("decode generation request: %w", err)
	}
	request, ok := root["request"].(map[string]any)
	if !ok {
		return nil, errors.New("generation request missing request object")
	}
	tools, ok := request["tools"].([]any)
	if !ok {
		if requireMCP {
			return nil, errors.New("generation request missing tools array")
		}
		request["tools"] = []any{}
		out, err := json.Marshal(root)
		if err != nil {
			return nil, fmt.Errorf("encode generation request: %w", err)
		}
		return out, nil
	}
	found := false
	filtered := make([]any, 0, 1)
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
			if name, _ := decl["name"].(string); name == "call_mcp_tool" {
				if found {
					return nil, errors.New("generation request contains duplicate call_mcp_tool declarations")
				}
				found = true
				filtered = append(filtered, map[string]any{"functionDeclarations": []any{decl}})
			}
		}
	}
	if requireMCP && !found {
		return nil, errors.New("generation request missing call_mcp_tool")
	}
	request["tools"] = filtered
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode generation request: %w", err)
	}
	return out, nil
}

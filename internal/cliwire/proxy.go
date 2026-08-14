// Package cliwire implements the opt-in wire audit proxy for authenticated
// CLI-backed providers. Captures contain decrypted HTTP streams and are
// intentionally treated as sensitive artifacts.
package cliwire

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/samsaffron/term-llm/internal/mitmca"
)

const (
	// TraceRootEnv enables CLI wire auditing and names the directory under which
	// a private run directory is created.
	TraceRootEnv = "TERM_LLM_CLI_WIRE_TRACE"
	traceVersion = 1
)

// Server is an authenticated, loopback-only HTTPS MITM proxy. It records the
// decrypted byte stream in each direction without parsing or rewriting it.
type Server struct {
	mu            sync.Mutex
	traceMu       sync.Mutex
	server        *http.Server
	listener      net.Listener
	authority     *mitmca.Authority
	upstreamRoots *x509.CertPool
	proxyURL      string
	caPath        string
	caDir         string
	traceDir      string
	provider      string
	proxyToken    string
	events        *os.File
	connections   map[net.Conn]struct{}
	leafCerts     map[string]tls.Certificate
	handlers      sync.WaitGroup
	nextID        atomic.Uint64
	running       bool
}

type traceEvent struct {
	Version       int      `json:"version"`
	Timestamp     string   `json:"timestamp"`
	Type          string   `json:"type"`
	Provider      string   `json:"provider,omitempty"`
	PID           int      `json:"pid,omitempty"`
	ConnectionID  uint64   `json:"connection_id,omitempty"`
	Target        string   `json:"target,omitempty"`
	Host          string   `json:"host,omitempty"`
	Port          int      `json:"port,omitempty"`
	ALPN          string   `json:"alpn,omitempty"`
	RequestFile   string   `json:"request_file,omitempty"`
	ResponseFile  string   `json:"response_file,omitempty"`
	RequestBytes  int64    `json:"request_bytes,omitempty"`
	ResponseBytes int64    `json:"response_bytes,omitempty"`
	DecodedFiles  []string `json:"decoded_request_files,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// Start starts a wire-audit proxy for provider and creates a private run
// directory below traceRoot.
func Start(traceRoot, provider string) (*Server, error) {
	return StartWithAdditionalCA(traceRoot, provider, "")
}

// StartWithAdditionalCA starts a wire-audit proxy and preserves certificates
// from an existing child SSL_CERT_FILE in the generated trust bundle.
func StartWithAdditionalCA(traceRoot, provider, additionalCAPath string) (*Server, error) {
	traceRoot = strings.TrimSpace(traceRoot)
	provider = sanitizeName(provider)
	if traceRoot == "" {
		return nil, errors.New("CLI wire trace root is empty")
	}
	if err := ensureTraceRoot(traceRoot); err != nil {
		return nil, err
	}
	runName := fmt.Sprintf("%s-%d-%s-%s", time.Now().UTC().Format("20060102T150405.000000000Z"), os.Getpid(), provider, randomName())
	traceDir := filepath.Join(traceRoot, runName)
	if err := os.Mkdir(traceDir, 0o700); err != nil {
		return nil, fmt.Errorf("create CLI wire trace run directory: %w", err)
	}
	cleanupTrace := true
	defer func() {
		if cleanupTrace {
			_ = os.RemoveAll(traceDir)
		}
	}()
	connectionsDir := filepath.Join(traceDir, "connections")
	if err := os.Mkdir(connectionsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create CLI wire connection directory: %w", err)
	}
	events, err := openPrivateFile(filepath.Join(traceDir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	closeEvents := true
	defer func() {
		if closeEvents {
			_ = events.Close()
		}
	}()
	authority, err := mitmca.New("term-llm CLI wire audit")
	if err != nil {
		return nil, err
	}
	var additionalCA []byte
	if path := strings.TrimSpace(additionalCAPath); path != "" {
		additionalCA, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read existing child CA bundle: %w", err)
		}
	}
	bundle, err := authority.BundleWithSystemRoots(additionalCA)
	if err != nil {
		return nil, err
	}
	upstreamRoots, err := x509.SystemCertPool()
	if err != nil || upstreamRoots == nil {
		upstreamRoots = x509.NewCertPool()
	}
	if len(additionalCA) > 0 && !upstreamRoots.AppendCertsFromPEM(additionalCA) {
		return nil, errors.New("existing child CA bundle contains no certificates")
	}
	caDir, err := os.MkdirTemp("", "term-llm-cli-wire-ca-")
	if err != nil {
		return nil, fmt.Errorf("create CLI wire CA directory: %w", err)
	}
	if err := os.Chmod(caDir, 0o700); err != nil {
		_ = os.RemoveAll(caDir)
		return nil, fmt.Errorf("secure CLI wire CA directory: %w", err)
	}
	caPath := filepath.Join(caDir, "ca.pem")
	if err := os.WriteFile(caPath, bundle, 0o600); err != nil {
		_ = os.RemoveAll(caDir)
		return nil, fmt.Errorf("write CLI wire CA bundle: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = os.RemoveAll(caDir)
		return nil, fmt.Errorf("listen for CLI wire proxy: %w", err)
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(caDir)
		return nil, fmt.Errorf("generate CLI wire proxy token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	proxy := &url.URL{Scheme: "http", Host: ln.Addr().String(), User: url.UserPassword("term-llm", token)}
	s := &Server{
		listener: ln, authority: authority, upstreamRoots: upstreamRoots, proxyURL: proxy.String(), caPath: caPath, caDir: caDir,
		traceDir: traceDir, provider: provider, proxyToken: token, events: events,
		connections: make(map[net.Conn]struct{}), leafCerts: make(map[string]tls.Certificate), running: true,
	}
	httpServer := &http.Server{Handler: http.HandlerFunc(s.serveHTTP), ReadHeaderTimeout: 10 * time.Second}
	s.server = httpServer
	if err := s.writeEvent(traceEvent{Type: "start", Provider: provider, PID: os.Getpid()}); err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(caDir)
		return nil, err
	}
	go func() { _ = httpServer.Serve(ln) }()
	cleanupTrace = false
	closeEvents = false
	return s, nil
}

// Environment returns forced child-process proxy and certificate variables.
func (s *Server) Environment() map[string]string {
	if s == nil {
		return nil
	}
	return map[string]string{
		"HTTP_PROXY": s.proxyURL, "http_proxy": s.proxyURL,
		"HTTPS_PROXY": s.proxyURL, "https_proxy": s.proxyURL,
		"ALL_PROXY": "", "all_proxy": "",
		"NO_PROXY": "127.0.0.1,localhost", "no_proxy": "127.0.0.1,localhost",
		"SSL_CERT_FILE":       s.caPath,
		"NODE_EXTRA_CA_CERTS": s.caPath,
	}
}

// ProxyURL returns the authenticated loopback proxy URL.
func (s *Server) ProxyURL() string {
	if s == nil {
		return ""
	}
	return s.proxyURL
}

// CAPath returns the combined system and audit CA bundle.
func (s *Server) CAPath() string {
	if s == nil {
		return ""
	}
	return s.caPath
}

// TraceDir returns the private run directory containing events and captures.
func (s *Server) TraceDir() string {
	if s == nil {
		return ""
	}
	return s.traceDir
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r.Header.Get("Proxy-Authorization")) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="term-llm-cli-wire"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
		return
	}
	if !s.beginHandler() {
		http.Error(w, "wire audit proxy is stopping", http.StatusServiceUnavailable)
		return
	}
	defer s.handlers.Done()
	s.intercept(w, r)
}

func (s *Server) authorized(header string) bool {
	const prefix = "Basic "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	want := "term-llm:" + s.proxyToken
	return subtle.ConstantTimeCompare(decoded, []byte(want)) == 1
}

func (s *Server) intercept(w http.ResponseWriter, r *http.Request) {
	host, portText, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "invalid CONNECT target", http.StatusBadRequest)
		return
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "invalid CONNECT port", http.StatusBadRequest)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unavailable", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	s.track(client)
	defer func() { s.untrack(client); _ = client.Close() }()
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	leaf, err := s.leafCertificate(host)
	if err != nil {
		s.recordError(0, r.Host, err)
		return
	}
	nextProtos := []string{"http/1.1"}
	if strings.HasPrefix(s.provider, "cursor-bin") {
		nextProtos = []string{"h2", "http/1.1"}
	}
	clientTLS := tls.Server(client, &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12, NextProtos: nextProtos})
	if err := clientTLS.Handshake(); err != nil {
		s.recordError(0, r.Host, fmt.Errorf("handshake with CLI: %w", err))
		return
	}
	alpn := clientTLS.ConnectionState().NegotiatedProtocol
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	upstreamRaw, err := dialer.Dial("tcp", r.Host)
	if err != nil {
		s.recordError(0, r.Host, fmt.Errorf("dial upstream: %w", err))
		return
	}
	s.track(upstreamRaw)
	defer func() { s.untrack(upstreamRaw); _ = upstreamRaw.Close() }()
	upstreamConfig := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: s.upstreamRoots}
	if alpn != "" {
		upstreamConfig.NextProtos = []string{alpn}
	}
	upstreamTLS := tls.Client(upstreamRaw, upstreamConfig)
	if err := upstreamTLS.Handshake(); err != nil {
		s.recordError(0, r.Host, fmt.Errorf("handshake with upstream: %w", err))
		return
	}
	id := s.nextID.Add(1)
	base := fmt.Sprintf("%06d-%s", id, sanitizeName(host))
	requestName := base + "-request.bin"
	responseName := base + "-response.bin"
	requestFile, err := openPrivateFile(filepath.Join(s.traceDir, "connections", requestName))
	if err != nil {
		s.recordError(id, r.Host, err)
		return
	}
	defer requestFile.Close()
	responseFile, err := openPrivateFile(filepath.Join(s.traceDir, "connections", responseName))
	if err != nil {
		s.recordError(id, r.Host, err)
		return
	}
	defer responseFile.Close()
	_ = s.writeEvent(traceEvent{Type: "connection", ConnectionID: id, Target: r.Host, Host: host, Port: port, ALPN: alpn, RequestFile: filepath.Join("connections", requestName), ResponseFile: filepath.Join("connections", responseName)})

	var requestBytes, responseBytes atomic.Int64
	done := make(chan struct{}, 2)
	copyStream := func(dst net.Conn, capture *os.File, src net.Conn, count *atomic.Int64) {
		n, _ := io.Copy(io.MultiWriter(dst, capture), src)
		count.Store(n)
		_ = dst.Close()
		done <- struct{}{}
	}
	go copyStream(upstreamTLS, requestFile, clientTLS, &requestBytes)
	go copyStream(clientTLS, responseFile, upstreamTLS, &responseBytes)
	<-done
	_ = clientTLS.Close()
	_ = upstreamTLS.Close()
	<-done
	_ = requestFile.Sync()
	decodedFiles, decodeErr := decodeConnectRequestMessages(requestFile.Name(), filepath.Join(s.traceDir, "connections"), base)
	if decodeErr != nil {
		s.recordError(id, r.Host, fmt.Errorf("decode HTTP/2 request messages: %w", decodeErr))
	}
	_ = s.writeEvent(traceEvent{Type: "connection_end", ConnectionID: id, Target: r.Host, RequestBytes: requestBytes.Load(), ResponseBytes: responseBytes.Load(), DecodedFiles: decodedFiles})
}

func (s *Server) leafCertificate(host string) (tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cert, ok := s.leafCerts[host]; ok {
		return cert, nil
	}
	cert, err := s.authority.Leaf(host)
	if err != nil {
		return tls.Certificate{}, err
	}
	s.leafCerts[host] = cert
	return cert, nil
}

func (s *Server) beginHandler() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return false
	}
	s.handlers.Add(1)
	return true
}

func (s *Server) track(conn net.Conn) {
	s.mu.Lock()
	if s.connections != nil {
		s.connections[conn] = struct{}{}
	}
	s.mu.Unlock()
}

func (s *Server) untrack(conn net.Conn) {
	s.mu.Lock()
	delete(s.connections, conn)
	s.mu.Unlock()
}

func (s *Server) recordError(id uint64, target string, err error) {
	_ = s.writeEvent(traceEvent{Type: "error", ConnectionID: id, Target: target, Error: err.Error()})
}

func (s *Server) writeEvent(event traceEvent) error {
	event.Version = traceVersion
	event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode CLI wire event: %w", err)
	}
	line = append(line, '\n')
	s.traceMu.Lock()
	defer s.traceMu.Unlock()
	if s.events == nil {
		return errors.New("CLI wire event file is closed")
	}
	if _, err := s.events.Write(line); err != nil {
		return fmt.Errorf("write CLI wire event: %w", err)
	}
	return nil
}

// Stop shuts down the proxy and removes temporary CA material. Captures remain.
func (s *Server) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	s.running = false
	srv, caDir := s.server, s.caDir
	connections := make([]net.Conn, 0, len(s.connections))
	for conn := range s.connections {
		connections = append(connections, conn)
	}
	s.server, s.listener, s.connections = nil, nil, nil
	s.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	s.handlers.Wait()
	s.traceMu.Lock()
	events := s.events
	if events != nil {
		event := traceEvent{Version: traceVersion, Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Type: "stop", Provider: s.provider, PID: os.Getpid()}
		if line, marshalErr := json.Marshal(event); marshalErr == nil {
			_, _ = events.Write(append(line, '\n'))
		}
		_ = events.Close()
		s.events = nil
	}
	s.traceMu.Unlock()
	if caDir != "" {
		_ = os.RemoveAll(caDir)
	}
	return err
}

func ensureTraceRoot(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("CLI wire trace root is not a safe directory")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect CLI wire trace root: %w", err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create CLI wire trace root: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("created CLI wire trace root is not a safe directory")
	}
	return nil
}

func openPrivateFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create private CLI wire file: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure CLI wire file: %w", err)
	}
	return file, nil
}

func sanitizeName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "cli"
	}
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	name := strings.Trim(b.String(), "._-")
	if name == "" {
		return "cli"
	}
	return name
}

func randomName() string {
	data := make([]byte, 6)
	if _, err := rand.Read(data); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

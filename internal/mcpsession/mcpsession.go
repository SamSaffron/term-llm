// Package mcpsession lets processes outside a term-llm session (most notably
// commands run by the shell tool) call tools on that session's live MCP
// servers. Each MCP manager that has servers running listens on a private unix
// socket; `term-llm mcp run --session` dials it instead of spawning a fresh
// server, so stateful servers such as Playwright keep their state.
//
// The package is deliberately dependency-light so both internal/tools (which
// exports the socket to shell commands) and internal/mcp (which serves it) can
// import it without cycles.
package mcpsession

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// EnvVar is exported to shell commands run inside a session with live MCP
// servers. `term-llm mcp run` uses it as the default --session target.
const EnvVar = "TERM_LLM_MCP_SESSION"

// DisableEnvVar turns the session socket off when set to 0/false/off.
const DisableEnvVar = "TERM_LLM_MCP_SESSION_SOCKET"

const socketPrefix = "mcp-"
const socketSuffix = ".sock"

// maxSocketPath keeps paths under the smallest common sun_path limit (104 on
// macOS/BSD, 108 on Linux).
const maxSocketPath = 100

// Disabled reports whether the session socket has been switched off.
func Disabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(DisableEnvVar))) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// Dir returns the private directory holding session sockets, creating it with
// 0700 permissions. It prefers $XDG_RUNTIME_DIR and falls back to a per-user
// directory under the system temp dir.
func Dir() (string, error) {
	var dir string
	if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
		dir = filepath.Join(runtimeDir, "term-llm")
	} else {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("term-llm-%d", os.Getuid()))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create MCP session socket dir: %w", err)
	}
	if err := checkPrivateDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

var unsafeLabel = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// SocketPath returns the socket path for a session label (usually a session ID).
func SocketPath(label string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	clean := strings.Trim(unsafeLabel.ReplaceAllString(strings.TrimSpace(label), "_"), "._")
	if clean == "" {
		return "", errors.New("empty MCP session label")
	}
	path := filepath.Join(dir, socketPrefix+clean+socketSuffix)
	if len(path) > maxSocketPath {
		sum := sha256.Sum256([]byte(label))
		path = filepath.Join(dir, socketPrefix+hex.EncodeToString(sum[:8])+socketSuffix)
	}
	return path, nil
}

// LabelFromPath extracts the session label from a socket path.
func LabelFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(strings.TrimPrefix(base, socketPrefix), socketSuffix)
}

// ResolveTarget turns a --session argument (socket path or session label) into
// a socket path.
func ResolveTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", errors.New("empty MCP session target")
	}
	if strings.ContainsRune(target, os.PathSeparator) || strings.HasSuffix(target, socketSuffix) {
		return target, nil
	}
	return SocketPath(target)
}

// ListSockets returns the session sockets currently present in Dir.
func ListSockets() ([]string, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(dir, socketPrefix+"*"+socketSuffix))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

// ---- in-process registry: session ID -> socket path ------------------------

type registration struct {
	sessionID      string
	path           string
	processDefault bool
}

var (
	registryMu sync.Mutex
	registry   = map[int]registration{}
	nextRegID  int
)

func register(reg registration) (unregister func()) {
	registryMu.Lock()
	id := nextRegID
	nextRegID++
	registry[id] = reg
	registryMu.Unlock()
	return func() {
		registryMu.Lock()
		delete(registry, id)
		registryMu.Unlock()
	}
}

// Register records that sessionID's MCP manager listens on path. Bound sockets
// are only ever handed to shell commands of that exact session.
func Register(sessionID, path string) (unregister func()) {
	return register(registration{sessionID: strings.TrimSpace(sessionID), path: path})
}

// RegisterProcessDefault records the socket of a process that owns a single
// MCP manager (one-shot CLI runs such as ask/chat). Lookup falls back to it
// only when it is the sole process default, so multi-session servers, which
// never register one, cannot leak a socket across sessions.
func RegisterProcessDefault(path string) (unregister func()) {
	return register(registration{path: path, processDefault: true})
}

// Lookup returns the socket path for a session, or "" when it has none.
func Lookup(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	registryMu.Lock()
	defer registryMu.Unlock()
	var defaults []string
	for _, reg := range registry {
		if reg.processDefault {
			defaults = append(defaults, reg.path)
			continue
		}
		if sessionID != "" && reg.sessionID == sessionID {
			return reg.path
		}
	}
	if len(defaults) == 1 {
		return defaults[0]
	}
	return ""
}

// ---- wire protocol ---------------------------------------------------------

// ToolInfo describes one tool exposed by a live session server.
type ToolInfo struct {
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	InputSchema  map[string]any `json:"input_schema,omitempty"`
	OutputSchema map[string]any `json:"output_schema,omitempty"`
}

// ServerInfo describes one MCP server in the session.
type ServerInfo struct {
	Name   string     `json:"name"`
	Status string     `json:"status"`
	Error  string     `json:"error,omitempty"`
	Tools  []ToolInfo `json:"tools,omitempty"`
}

// SessionInfo is returned by GET /v1/session.
type SessionInfo struct {
	SessionID string       `json:"session_id,omitempty"`
	PID       int          `json:"pid"`
	Servers   []ServerInfo `json:"servers"`
}

// CallRequest is the body of POST /v1/call.
type CallRequest struct {
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CallResult mirrors the parts of llm.ToolOutput an MCP call produces.
type CallResult struct {
	Content      string                `json:"content"`
	ContentParts []llm.ToolContentPart `json:"content_parts,omitempty"`
	IsError      bool                  `json:"is_error,omitempty"`
}

// ToolOutput converts the wire result back to an llm.ToolOutput.
func (r CallResult) ToolOutput() llm.ToolOutput {
	return llm.ToolOutput{Content: r.Content, ContentParts: r.ContentParts, IsError: r.IsError}
}

type errorBody struct {
	Error string `json:"error"`
}

// ---- client ----------------------------------------------------------------

// Client talks to one session socket.
type Client struct {
	Path string
	http *http.Client
}

// Dial validates the target socket and returns a client for it.
func Dial(target string) (*Client, error) {
	path, err := ResolveTarget(target)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("MCP session %q is not running (%s): %w", LabelFromPath(path), path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("MCP session target %s is not a socket", path)
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
	return &Client{Path: path, http: &http.Client{Transport: transport}}, nil
}

// Session returns the live servers and their tools.
func (c *Client) Session(ctx context.Context) (SessionInfo, error) {
	var out SessionInfo
	err := c.do(ctx, http.MethodGet, "/v1/session", nil, &out)
	return out, err
}

// Call invokes a tool on a live session server.
func (c *Client) Call(ctx context.Context, server, tool string, args json.RawMessage) (CallResult, error) {
	var out CallResult
	err := c.do(ctx, http.MethodPost, "/v1/call", CallRequest{Server: server, Tool: tool, Arguments: args}, &out)
	return out, err
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://mcp-session"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("MCP session %s: %w", LabelFromPath(c.Path), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var eb errorBody
		if json.Unmarshal(data, &eb) == nil && eb.Error != "" {
			return errors.New(eb.Error)
		}
		return fmt.Errorf("MCP session %s: HTTP %d", LabelFromPath(c.Path), resp.StatusCode)
	}
	return json.Unmarshal(data, out)
}

// ---- server helpers --------------------------------------------------------

// Listen creates the unix socket at path with 0600 permissions. A stale socket
// left by a crashed process is removed; a live one is an error.
func Listen(path string) (net.Listener, error) {
	if _, err := os.Lstat(path); err == nil {
		conn, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("MCP session socket %s is already in use", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale MCP session socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on MCP session socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod MCP session socket: %w", err)
	}
	return ln, nil
}

// WriteJSON writes a JSON response.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes a JSON error response.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, errorBody{Error: msg})
}

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
	"crypto/rand"
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

// labelSep separates the session label from the per-manager nonce. It is not
// in the sanitized label alphabet, so labels can be recovered unambiguously.
const labelSep = "@"

// maxSocketPath keeps paths under the smallest common sun_path limit (104 on
// macOS/BSD, 108 on Linux).
const maxSocketPath = 100

// Disabled reports whether the session socket has been switched off, either
// explicitly or because the platform lacks support.
func Disabled() bool {
	if !supported {
		return true
	}
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
	if !supported {
		return "", errUnsupported
	}
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

// cleanLabel maps a session label to its filename form. Long labels are
// hashed so the socket path stays within sun_path limits.
func cleanLabel(label string) (string, error) {
	clean := strings.Trim(unsafeLabel.ReplaceAllString(strings.TrimSpace(label), "_"), "._")
	if clean == "" {
		return "", errors.New("empty MCP session label")
	}
	if len(clean) > 48 {
		sum := sha256.Sum256([]byte(label))
		clean = "h" + hex.EncodeToString(sum[:8])
	}
	return clean, nil
}

// NewSocketPath returns a fresh, unique socket path for a session label.
// Every manager gets its own path, so a replacement runtime for the same
// session never collides with the one it is replacing.
func NewSocketPath(label string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	clean, err := cleanLabel(label)
	if err != nil {
		return "", err
	}
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	path := filepath.Join(dir, socketPrefix+clean+labelSep+hex.EncodeToString(nonce[:])+socketSuffix)
	if len(path) > maxSocketPath {
		return "", fmt.Errorf("MCP session socket path %q exceeds %d bytes; set XDG_RUNTIME_DIR to a shorter directory", path, maxSocketPath)
	}
	return path, nil
}

// LabelFromPath extracts the session label from a socket path.
func LabelFromPath(path string) string {
	base := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), socketPrefix), socketSuffix)
	if i := strings.LastIndex(base, labelSep); i >= 0 {
		return base[:i]
	}
	return base
}

// ResolveTarget turns a --session argument (socket path or session ID) into a
// socket path. For a session ID it picks the newest live socket bound to it.
func ResolveTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", errors.New("empty MCP session target")
	}
	if strings.ContainsRune(target, os.PathSeparator) || strings.HasSuffix(target, socketSuffix) {
		return target, nil
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	clean, err := cleanLabel(target)
	if err != nil {
		return "", err
	}
	matches, err := filepath.Glob(filepath.Join(dir, socketPrefix+clean+labelSep+"*"+socketSuffix))
	if err != nil {
		return "", err
	}
	var best string
	var bestMod time.Time
	for _, m := range matches {
		if Stale(m) {
			continue
		}
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if best == "" || info.ModTime().After(bestMod) {
			best, bestMod = m, info.ModTime()
		}
	}
	if best == "" {
		return "", fmt.Errorf("MCP session %q is not running (no live socket in %s)", target, dir)
	}
	return best, nil
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

// SweepStale removes sockets whose owner is definitively gone (connection
// refused on an existing socket file). Ambiguous failures are left alone.
func SweepStale() {
	paths, err := ListSockets()
	if err != nil {
		return
	}
	for _, path := range paths {
		if Stale(path) {
			_ = os.Remove(path)
		}
	}
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
// are only ever handed to shell commands of that exact session. When a session
// has several (a replacement runtime overlapping the one it replaces), the
// most recent registration wins; unregistering it restores the previous one.
func Register(sessionID, path string) (unregister func()) {
	return register(registration{sessionID: strings.TrimSpace(sessionID), path: path})
}

// RegisterProcessDefault records the socket of a process whose single MCP
// manager runs without a session (e.g. `ask --no-session`). It is returned
// only to callers that have no session ID themselves, never to another
// session such as an in-process subagent.
func RegisterProcessDefault(path string) (unregister func()) {
	return register(registration{path: path, processDefault: true})
}

// Lookup returns the socket path for a session, or "" when it has none.
func Lookup(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	registryMu.Lock()
	defer registryMu.Unlock()
	bestID := -1
	var best string
	var defaults []string
	for id, reg := range registry {
		switch {
		case reg.processDefault:
			defaults = append(defaults, reg.path)
		case sessionID != "" && reg.sessionID == sessionID && id > bestID:
			bestID, best = id, reg.path
		}
	}
	if best != "" {
		return best
	}
	if sessionID == "" && len(defaults) == 1 {
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

// Listen creates the unix socket at path with 0600 permissions. Paths are
// unique per manager; an existing file is only replaced when it is a socket
// whose owner is definitively gone.
func Listen(path string) (net.Listener, error) {
	if _, err := os.Lstat(path); err == nil {
		if !Stale(path) {
			return nil, fmt.Errorf("MCP session socket %s already exists", path)
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

// Package livecall owns opt-in Codex app-server WebRTC sideband processes.
package livecall

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pion/sdp/v3"
)

const (
	MaxBody      = 128 << 10
	MaxSDP       = 64 << 10
	TTL          = 15 * time.Minute
	SetupTimeout = 45 * time.Second
	MaxSessions  = 4
)

var (
	ErrUnavailable = errors.New("live calls unavailable")
	ErrCapacity    = errors.New("live call capacity reached")
)

type Item struct {
	Role string `json:"role"`
	Text string `json:"text"`
}
type Offer struct {
	SDP          string `json:"sdp"`
	Instructions string `json:"instructions,omitempty"`
	InitialItems []Item `json:"initial_items,omitempty"`
}
type Answer struct {
	Protocol             string             `json:"protocol"`
	DelegationCompletion string             `json:"delegation_completion"`
	SDP                  string             `json:"sdp"`
	ExpiresAt            time.Time          `json:"expires_at"`
	Cancel               context.CancelFunc `json:"-"`
}

func validText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' })
}
func ValidSDP(text string) bool {
	if len(text) > MaxSDP || !strings.HasPrefix(text, "v=0\r\n") || !validText(text) {
		return false
	}
	var session sdp.SessionDescription
	if err := session.Unmarshal([]byte(text)); err != nil {
		return false
	}
	for _, media := range session.MediaDescriptions {
		if media.MediaName.Media == "audio" {
			return true
		}
	}
	return false
}

func (o Offer) Validate() error {
	if !ValidSDP(o.SDP) {
		return errors.New("sdp must be an audio SDP offer (v=0, CRLF), at most 65536 bytes")
	}
	if len(o.Instructions) > 16384 || !validText(o.Instructions) {
		return errors.New("instructions must be text, at most 16384 bytes")
	}
	if len(o.InitialItems) > 128 {
		return errors.New("initial_items exceeds 128 items")
	}
	total := 0
	for _, i := range o.InitialItems {
		total += len(i.Text)
		if (i.Role != "user" && i.Role != "assistant") || strings.TrimSpace(i.Text) == "" || !validText(i.Text) {
			return errors.New("initial_items requires user/assistant roles and nonempty text")
		}
	}
	// A conservative byte bound also stays below Codex's 8192 estimated token limit.
	if total > 8192 {
		return errors.New("initial_items text exceeds 8192 bytes")
	}
	return nil
}

type Tokens struct {
	AccessToken string `json:"accessToken"`
	AccountID   string `json:"chatgptAccountId"`
}
type TokenSource func(context.Context, bool) (Tokens, error)
type Manager struct {
	Path     string
	Tokens   TokenSource
	Log      func(string)
	mu       sync.Mutex
	sessions map[*process]struct{}
	closed   bool
	ttl      time.Duration
	command  func(context.Context, string, ...string) *exec.Cmd
}

func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	var done []<-chan struct{}
	for p := range m.sessions {
		p.cancel()
		done = append(done, p.done)
	}
	m.mu.Unlock()
	for _, ch := range done {
		<-ch
	}
}
func (m *Manager) Start(ctx context.Context, o Offer) (Answer, error) {
	if err := o.Validate(); err != nil {
		return Answer{}, err
	}
	p, expires, err := m.admit()
	if err != nil {
		return Answer{}, err
	}
	return m.start(ctx, p, expires, o)
}

func (m *Manager) admit() (*process, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.Path == "" || m.Tokens == nil {
		return nil, time.Time{}, ErrUnavailable
	}
	if len(m.sessions) >= MaxSessions {
		return nil, time.Time{}, ErrCapacity
	}
	ttl := m.ttl
	if ttl == 0 {
		ttl = TTL
	}
	expires := time.Now().Add(ttl)
	life, cancel := context.WithDeadline(context.Background(), expires)
	p := &process{
		ctx: life, cancel: cancel, pending: map[int]chan message{},
		sdp: make(chan message, 1), done: make(chan struct{}), manager: m,
	}
	if m.sessions == nil {
		m.sessions = map[*process]struct{}{}
	}
	m.sessions[p] = struct{}{}
	return p, expires, nil
}

func (m *Manager) start(ctx context.Context, p *process, expires time.Time, o Offer) (answer Answer, err error) {
	success := false
	defer func() {
		if !success {
			p.log("live setup failed or canceled")
			p.cancel()
		}
	}()
	setup, stop := context.WithTimeout(ctx, SetupTimeout)
	defer stop()
	detach := context.AfterFunc(setup, p.cancel)
	defer detach()
	if err = p.launch(); err != nil {
		p.log("app-server launch unavailable")
		p.release()
		return Answer{}, ErrUnavailable
	}
	threadID, err := p.configure(setup, o)
	if err != nil {
		return Answer{}, err
	}
	answer, err = p.waitForAnswer(setup, expires, threadID)
	if err != nil {
		return Answer{}, err
	}
	detach()
	success = true
	return answer, nil
}

func (p *process) configure(ctx context.Context, o Offer) (string, error) {
	call := func(method string, params any) (json.RawMessage, error) { return p.call(ctx, method, params) }
	if _, err := call("initialize", map[string]any{"clientInfo": map[string]string{"name": "term-llm-live", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}}); err != nil {
		return "", err
	}
	if err := p.send(map[string]any{"method": "initialized"}); err != nil {
		return "", err
	}
	tokens, err := p.manager.Tokens(ctx, false)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil || tokens.AccessToken == "" || tokens.AccountID == "" {
		p.log("configured ChatGPT OAuth unavailable")
		return "", ErrUnavailable
	}
	p.mu.Lock()
	p.account = tokens.AccountID
	p.mu.Unlock()
	if _, err = call("account/login/start", map[string]any{"type": "chatgptAuthTokens", "accessToken": tokens.AccessToken, "chatgptAccountId": tokens.AccountID}); err != nil {
		return "", err
	}
	raw, err := call("thread/start", map[string]any{"ephemeral": true, "cwd": p.dir, "approvalPolicy": "never", "sandbox": "read-only", "baseInstructions": "", "developerInstructions": "", "config": map[string]any{"features.realtime_conversation": true}})
	if err != nil {
		return "", err
	}
	var thread struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &thread) != nil || thread.Thread.ID == "" {
		return "", errors.New("invalid thread/start result")
	}
	_, err = call("thread/realtime/start", map[string]any{"threadId": thread.Thread.ID, "transport": map[string]string{"type": "webrtc", "sdp": o.SDP}, "version": "v3", "outputModality": "audio", "clientManagedHandoffs": true, "includeStartupContext": false, "model": "gpt-live-1-codex", "prompt": o.Instructions, "initialItems": o.InitialItems})
	return thread.Thread.ID, err
}

func (p *process) waitForAnswer(ctx context.Context, expires time.Time, threadID string) (Answer, error) {
	select {
	case msg := <-p.sdp:
		var result struct {
			ThreadID string `json:"threadId"`
			SDP      string `json:"sdp"`
		}
		if json.Unmarshal(msg.Params, &result) != nil || result.ThreadID != threadID || !ValidSDP(result.SDP) {
			return Answer{}, errors.New("invalid realtime SDP notification")
		}
		if ctx.Err() != nil || p.ctx.Err() != nil {
			return Answer{}, errors.New("live setup canceled")
		}
		return Answer{
			Protocol:             "frameless-bidi-v3",
			DelegationCompletion: "context_append",
			SDP:                  result.SDP,
			ExpiresAt:            expires,
			Cancel:               p.cancel,
		}, nil
	case <-ctx.Done():
		return Answer{}, ctx.Err()
	case <-p.done:
		return Answer{}, errors.New("app-server exited during setup")
	}
}

type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code int `json:"code"`
	} `json:"error"`
}
type process struct {
	account string
	ctx     context.Context
	cancel  context.CancelFunc
	manager *Manager
	cmd     *exec.Cmd
	dir     string
	stdin   io.WriteCloser
	writeMu sync.Mutex
	mu      sync.Mutex
	next    int
	pending map[int]chan message
	sdp     chan message
	done    chan struct{}
}

func (p *process) release() {
	p.cancel()
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if p.dir != "" {
		if err := os.RemoveAll(p.dir); err != nil {
			p.log("temporary app-server directory cleanup failed")
		}
	}
	p.manager.mu.Lock()
	delete(p.manager.sessions, p)
	p.manager.mu.Unlock()
	close(p.done)
}
func (p *process) log(s string) {
	if p.manager.Log != nil {
		p.manager.Log(s)
	}
}
func (p *process) launch() error {
	dir, err := os.MkdirTemp("", "term-llm-live-")
	if err != nil {
		return err
	}
	p.dir = dir
	path, err := exec.LookPath(p.manager.Path)
	if err != nil {
		return err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	command := p.manager.command
	if command == nil {
		command = exec.CommandContext
	}
	p.cmd = command(p.ctx, path, "app-server", "-c", `cli_auth_credentials_store="ephemeral"`, "-c", `history.persistence="none"`, "-c", "analytics.enabled=false")
	p.cmd.Dir = dir
	// Never inherit provider credentials, Codex config, tracing, or shell startup settings.
	p.cmd.Env = []string{"HOME=" + dir, "USERPROFILE=" + dir, "CODEX_HOME=" + dir, "XDG_CONFIG_HOME=" + dir, "XDG_DATA_HOME=" + dir, "XDG_CACHE_HOME=" + dir, "PATH=" + os.Getenv("PATH"), "RUST_LOG=off"}
	p.cmd.WaitDelay = 2 * time.Second
	p.stdin, err = p.cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := p.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// Raw stderr can contain tokens, SDP or transcripts. Only structured phase/code diagnostics escape.
	p.cmd.Stderr = io.Discard
	if err = p.cmd.Start(); err != nil {
		return err
	}
	go func() { p.read(out); p.cancel() }()
	go func() {
		err := p.cmd.Wait()
		p.cancel()
		_ = p.stdin.Close()
		if err != nil {
			p.log("app-server stopped (failure or cancellation)")
		} else {
			p.log("app-server stopped")
		}
		p.release()
	}()
	return nil
}
func (p *process) send(v any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := json.NewEncoder(p.stdin).Encode(v); err != nil {
		return errors.New("app-server write failed")
	}
	return nil
}
func (p *process) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	p.mu.Lock()
	p.next++
	id := p.next
	ch := make(chan message, 1)
	p.pending[id] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }()
	if err := p.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case msg := <-ch:
		if msg.Error != nil {
			p.log(fmt.Sprintf("%s RPC error code %d", method, msg.Error.Code))
			return nil, fmt.Errorf("%s rejected (code %d)", method, msg.Error.Code)
		}
		return msg.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.done:
		return nil, fmt.Errorf("app-server exited during %s", method)
	}
}
func (p *process) read(out io.Reader) {
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var msg message
		if json.Unmarshal(scanner.Bytes(), &msg) != nil {
			p.log("invalid app-server RPC frame")
			return
		}
		if msg.Method != "" && len(msg.ID) > 0 {
			// Refuse every server-initiated operation except the external-auth refresh protocol.
			if msg.Method == "account/chatgptAuthTokens/refresh" {
				tokens, err := p.manager.Tokens(p.ctx, true)
				p.mu.Lock()
				account := p.account
				p.mu.Unlock()
				if err != nil || tokens.AccessToken == "" || tokens.AccountID != account {
					p.log("external token refresh failed")
					return
				}
				if p.send(map[string]any{"id": msg.ID, "result": tokens}) != nil {
					return
				}
			} else {
				if p.send(map[string]any{"id": msg.ID, "error": map[string]any{"code": -32601, "message": "unsupported by live bridge"}}) != nil {
					return
				}
			}
			continue
		}
		if len(msg.ID) > 0 {
			var id int
			if json.Unmarshal(msg.ID, &id) == nil {
				p.mu.Lock()
				ch := p.pending[id]
				p.mu.Unlock()
				if ch != nil {
					select {
					case ch <- msg:
					default:
					}
				}
			}
			continue
		}
		switch msg.Method {
		case "thread/realtime/sdp":
			select {
			case p.sdp <- msg:
			default:
			}
		case "thread/realtime/error", "thread/realtime/closed":
			p.log(msg.Method)
			return
		}
	}
	if scanner.Err() != nil {
		p.log("app-server RPC stream unreadable or frame exceeds 1048576 bytes")
	}
}

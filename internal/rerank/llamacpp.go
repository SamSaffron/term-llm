package rerank

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
)

// ErrWarmingUp reports that the managed llama-server is still starting (the
// first start may download the model). Callers should fall back and retry on
// a later request; the start continues in the background.
var ErrWarmingUp = errors.New("reranker is starting")

const (
	// backgroundRequestTimeout bounds a rerank request that keeps running
	// after the caller has fallen back.
	backgroundRequestTimeout = 5 * time.Minute
	// minAdaptiveDocuments is the smallest adaptive batch worth reranking.
	minAdaptiveDocuments = 4
)

// startRetryBackoff is how long a failed llama-server start is remembered
// before another start is attempted.
var startRetryBackoff = time.Minute

// managedServer is a lazily started llama-server shared by all providers
// with the same binary, model and arguments. Providers hold no reference
// counts, so creating one per query cannot leak a process: the server stops
// after the idle timeout or on Shutdown.
type managedServer struct {
	binary         string
	args           []string
	startupTimeout time.Duration
	idleTimeout    time.Duration

	mu       sync.Mutex
	cmd      *exec.Cmd
	done     chan struct{} // closed when cmd exits
	baseURL  string
	idle     *time.Timer
	active   int           // in-flight requests
	ready    chan struct{} // closed when the current start attempt finishes
	starting bool
	startErr error
	failedAt time.Time
	stderr   *tailBuffer

	busy       chan struct{} // capacity 1: at most one request on the server
	tokPerSec  float64       // measured prompt throughput (EWMA)
	tokPerChar float64       // measured prompt tokens per document character
}

var (
	serversMu sync.Mutex
	servers   = map[string]*managedServer{}
)

type llamacppProvider struct {
	srv    *managedServer
	client *http.Client
	wait   time.Duration // max time a request waits for a starting server
}

func newLlamacppProvider(cfg config.RerankConfig, binary string, timeout time.Duration) (Provider, error) {
	startup, err := parseDuration(cfg.Llamacpp.StartupTimeout, defaultStartupTimeout)
	if err != nil {
		return nil, fmt.Errorf("rerank.llamacpp.startup_timeout: %w", err)
	}
	idle, err := parseDuration(cfg.Llamacpp.IdleTimeout, defaultIdleTimeout)
	if err != nil {
		return nil, fmt.Errorf("rerank.llamacpp.idle_timeout: %w", err)
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = "ggml-org/Qwen3-Reranker-0.6B-Q8_0-GGUF"
	}
	args := []string{"--reranking", modelFlag(model), model, "-c", "32768", "-np", "4", "-b", "2048", "-ub", "2048"}
	args = append(args, cfg.Llamacpp.Args...)
	args = append(args, "--host", "127.0.0.1")

	key := binary + "\x00" + strings.Join(args, "\x00")
	serversMu.Lock()
	defer serversMu.Unlock()
	srv := servers[key]
	if srv == nil {
		srv = &managedServer{binary: binary, args: args, startupTimeout: startup, idleTimeout: idle, busy: make(chan struct{}, 1)}
		servers[key] = srv
	}
	// The HTTP request itself may outlive the caller's wait: llama-server does
	// not abandon work when a client gives up, so the request runs to
	// completion in the background (bounded here) while holding srv.busy.
	return &llamacppProvider{srv: srv, client: &http.Client{Timeout: backgroundRequestTimeout}, wait: timeout}, nil
}

// modelFlag returns -m for local files and -hf for Hugging Face repos.
func modelFlag(model string) string {
	if st, err := os.Stat(model); err == nil && !st.IsDir() {
		return "-m"
	}
	return "-hf"
}

func (p *llamacppProvider) Name() string { return "llamacpp" }

func (p *llamacppProvider) Rerank(ctx context.Context, query string, docs []string, topN int) ([]Result, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	select {
	case p.srv.busy <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	base, release, err := p.srv.acquire(ctx, p.wait)
	if err != nil {
		<-p.srv.busy
		return nil, err
	}
	chars := 0
	for _, d := range docs {
		chars += len(d)
	}
	type outcome struct {
		results []Result
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() { <-p.srv.busy }()
		defer release()
		start := time.Now()
		results, tokens, err := doRerankUsage(context.Background(), p.client, base+"/v1/rerank", "", "", query, docs, topN)
		if err == nil {
			p.srv.observe(tokens, chars, time.Since(start))
		}
		done <- outcome{results, err}
	}()
	timer := time.NewTimer(p.wait)
	defer timer.Stop()
	select {
	case o := <-done:
		return o.results, o.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("reranking %d documents exceeded %s (finishing in background; later searches use a smaller batch)", len(docs), p.wait)
	}
}

// MaxDocuments implements Sizer: the number of documents of about docChars
// characters that should finish within 75% of the request timeout.
func (p *llamacppProvider) MaxDocuments(docChars int) int {
	return p.srv.maxDocuments(docChars, p.wait)
}

func (s *managedServer) observe(tokens, chars int, elapsed time.Duration) {
	if tokens <= 0 || chars <= 0 || elapsed <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rate := float64(tokens) / elapsed.Seconds()
	perChar := float64(tokens) / float64(chars)
	if s.tokPerSec == 0 {
		s.tokPerSec, s.tokPerChar = rate, perChar
		return
	}
	s.tokPerSec = 0.5*s.tokPerSec + 0.5*rate
	s.tokPerChar = 0.5*s.tokPerChar + 0.5*perChar
}

func (s *managedServer) maxDocuments(docChars int, wait time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokPerSec == 0 || s.tokPerChar == 0 || docChars <= 0 {
		return 0
	}
	n := int(0.75 * wait.Seconds() * s.tokPerSec / (s.tokPerChar * float64(docChars)))
	return max(n, minAdaptiveDocuments)
}

// acquire ensures the server is running and healthy, returning its base URL
// and a release func that restarts the idle timer. A missing server is
// started in the background; acquire waits at most wait for it and otherwise
// returns ErrWarmingUp. A failed start is remembered for startRetryBackoff.
func (s *managedServer) acquire(ctx context.Context, wait time.Duration) (string, func(), error) {
	s.mu.Lock()
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	if s.cmd != nil && !s.starting {
		select {
		case <-s.done:
			s.cmd, s.done = nil, nil
		default:
		}
	}
	if s.cmd == nil && !s.starting {
		if s.startErr != nil && time.Since(s.failedAt) < startRetryBackoff {
			err := s.startErr
			s.mu.Unlock()
			return "", nil, err
		}
		s.starting, s.startErr = true, nil
		s.ready = make(chan struct{})
		go s.start(s.ready)
	}
	ready := s.ready
	s.mu.Unlock()

	if ready != nil {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ready:
		case <-ctx.Done():
			s.reschedule()
			return "", nil, ctx.Err()
		case <-timer.C:
			s.reschedule()
			return "", nil, ErrWarmingUp
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startErr != nil {
		return "", nil, s.startErr
	}
	if s.cmd == nil {
		return "", nil, errors.New("llama-server is not running")
	}
	s.active++
	return s.baseURL, s.release, nil
}

func (s *managedServer) reschedule() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scheduleIdleLocked()
}

// start launches llama-server and waits for health without holding s.mu.
func (s *managedServer) start(ready chan struct{}) {
	err := s.launch()
	if err == nil {
		s.mu.Lock()
		base, done := s.baseURL, s.done
		s.mu.Unlock()
		err = waitHealthy(context.Background(), base, s.startupTimeout, done)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starting = false
	if err != nil {
		s.stopLocked()
		if tail := s.stderr.Tail(); tail != "" {
			err = fmt.Errorf("%w; llama-server stderr: %s", err, tail)
		}
		s.startErr, s.failedAt = err, time.Now()
	}
	close(ready)
	s.scheduleIdleLocked()
}

func (s *managedServer) launch() error {
	port, err := freePort()
	if err != nil {
		return fmt.Errorf("allocate llama-server port: %w", err)
	}
	args := append(append([]string{}, s.args...), "--port", strconv.Itoa(port))
	cmd := exec.Command(s.binary, args...)
	stderr := &tailBuffer{max: 4096}
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	setProcAttrs(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start llama-server: %w", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	s.mu.Lock()
	s.cmd, s.done, s.stderr = cmd, done, stderr
	s.baseURL = "http://127.0.0.1:" + strconv.Itoa(port)
	s.mu.Unlock()
	return nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

// Tail returns the trimmed captured output; nil-safe.
func (b *tailBuffer) Tail() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := strings.TrimSpace(string(b.buf))
	if len(s) > 1000 {
		s = "…" + s[len(s)-1000:]
	}
	return s
}

func (s *managedServer) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	s.scheduleIdleLocked()
}

func (s *managedServer) scheduleIdleLocked() {
	if s.cmd == nil || s.active > 0 || s.starting {
		return
	}
	if s.idle != nil {
		s.idle.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(s.idleTimeout, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.idle != t || s.active > 0 {
			return
		}
		s.idle = nil
		s.stopLocked()
	})
	s.idle = t
}

func (s *managedServer) stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	s.stopLocked()
	return nil
}

func (s *managedServer) stopLocked() {
	if s.cmd == nil {
		return
	}
	killProcess(s.cmd)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
	}
	s.cmd, s.done = nil, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitHealthy polls /health until it returns 200, the process exits, the
// context ends, or the timeout elapses.
func waitHealthy(ctx context.Context, base string, timeout time.Duration, exited <-chan struct{}) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
		if err != nil {
			return err
		}
		if resp, err := client.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-exited:
			return fmt.Errorf("llama-server exited before becoming healthy")
		case <-ctx.Done():
			return fmt.Errorf("wait for llama-server health: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// Shutdown stops every managed llama-server process started by this package.
// It is safe to call multiple times and from process-exit cleanup; providers
// remain usable and will lazily restart the server on the next Rerank.
func Shutdown() {
	serversMu.Lock()
	all := make([]*managedServer, 0, len(servers))
	for _, s := range servers {
		all = append(all, s)
	}
	serversMu.Unlock()
	for _, s := range all {
		_ = s.stop()
	}
}

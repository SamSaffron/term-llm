package widgets

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	idleTimeout      = 10 * time.Minute
	startupTimeout   = 10 * time.Second
	socketRuntimeDir = "/tmp/term-llm-widgets"
)

type processState int

const (
	stateStopped processState = iota
	stateStarting
	stateRunning
	stateError
)

type widgetEntry struct {
	manifest *Manifest

	mu      sync.Mutex
	startMu sync.Mutex // serializes concurrent start attempts
	state   processState
	errMsg  string
	proc    *os.Process
	proxy   *httputil.ReverseProxy
	lastReq time.Time
	port    int
}

func (e *widgetEntry) setError(err error) error {
	e.mu.Lock()
	e.state = stateError
	e.errMsg = err.Error()
	e.mu.Unlock()
	return err
}

// Manager discovers and manages widget sub-processes.
type Manager struct {
	widgetsDir string
	basePath   string

	mu       sync.RWMutex
	entries  map[string]*widgetEntry // mount → entry
	loadErrs []error

	stopOnce sync.Once
	stopCh   chan struct{}
}

// NewManager creates a manager, scans widgetsDir, and starts the idle reaper.
func NewManager(widgetsDir, basePath string) *Manager {
	m := &Manager{
		widgetsDir: widgetsDir,
		basePath:   basePath,
		entries:    make(map[string]*widgetEntry),
		stopCh:     make(chan struct{}),
	}
	m.scan()
	go m.idleLoop()
	return m
}

func (m *Manager) scan() {
	manifests, errs := ScanDir(m.widgetsDir)

	m.mu.Lock()
	defer m.mu.Unlock()

	newMounts := make(map[string]bool, len(manifests))
	for _, mf := range manifests {
		newMounts[mf.Mount] = true
	}
	for mount, e := range m.entries {
		if !newMounts[mount] {
			go e.stopProcess()
		}
	}

	newEntries := make(map[string]*widgetEntry, len(manifests))
	for _, mf := range manifests {
		if existing, ok := m.entries[mf.Mount]; ok {
			existing.mu.Lock()
			existing.manifest = mf
			if existing.state == stateError {
				existing.state = stateStopped
				existing.errMsg = ""
			}
			existing.mu.Unlock()
			newEntries[mf.Mount] = existing
		} else {
			newEntries[mf.Mount] = &widgetEntry{
				manifest: mf,
				state:    stateStopped,
				lastReq:  time.Now(),
			}
		}
	}
	m.entries = newEntries
	m.loadErrs = errs
}

// Reload re-scans widgetsDir and returns any load errors.
func (m *Manager) Reload() []error {
	m.scan()
	m.mu.RLock()
	errs := append([]error(nil), m.loadErrs...)
	m.mu.RUnlock()
	return errs
}

// WidgetStatus describes the current state of a widget.
type WidgetStatus struct {
	ID          string `json:"id"`
	Mount       string `json:"mount"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	State       string `json:"state"`
	Error       string `json:"error,omitempty"`
	Port        int    `json:"port,omitempty"`
}

// Status returns current status for all loaded widgets.
func (m *Manager) Status() []WidgetStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]WidgetStatus, 0, len(m.entries))
	for _, e := range m.entries {
		e.mu.Lock()
		s := WidgetStatus{
			ID:          e.manifest.ID,
			Mount:       e.manifest.Mount,
			Title:       e.manifest.Title,
			Description: e.manifest.Description,
			Port:        e.port,
		}
		switch e.state {
		case stateStopped:
			s.State = "stopped"
		case stateStarting:
			s.State = "starting"
		case stateRunning:
			s.State = "running"
		case stateError:
			s.State = "error"
			s.Error = e.errMsg
		}
		e.mu.Unlock()
		out = append(out, s)
	}
	return out
}

// LoadErrors returns errors from the last scan.
func (m *Manager) LoadErrors() []error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]error(nil), m.loadErrs...)
}

// StopMount stops the process for the named mount.
func (m *Manager) StopMount(mount string) error {
	m.mu.RLock()
	e, ok := m.entries[mount]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("widget %q not found", mount)
	}
	e.stopProcess()
	return nil
}

// Proxy forwards r to the widget identified by mount.
// The path in r must already have the /widgets/<mount> prefix stripped.
func (m *Manager) Proxy(mount string, w http.ResponseWriter, r *http.Request) {
	m.mu.RLock()
	e, ok := m.entries[mount]
	m.mu.RUnlock()
	if !ok {
		http.Error(w, "widget not found: "+mount, http.StatusNotFound)
		return
	}

	if err := m.ensureRunning(e); err != nil {
		http.Error(w, fmt.Sprintf("widget %s: %v", mount, err), http.StatusBadGateway)
		return
	}

	e.mu.Lock()
	e.lastReq = time.Now()
	proxy := e.proxy
	e.mu.Unlock()

	if proxy == nil {
		http.Error(w, "widget proxy not initialized", http.StatusBadGateway)
		return
	}

	// Clone request and strip sensitive headers before forwarding.
	r2 := r.Clone(r.Context())
	r2.Header.Del("Authorization")
	r2.Header.Del("Cookie")
	r2.Header.Set("X-Forwarded-Prefix", m.basePath+"/widgets/"+mount)
	r2.Header.Set("X-Forwarded-Host", r.Host)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	r2.Header.Set("X-Forwarded-Proto", scheme)
	r2.Header.Set("X-Term-LLM-Widget", e.manifest.ID)

	proxy.ServeHTTP(w, r2)
}

func (m *Manager) ensureRunning(e *widgetEntry) error {
	e.startMu.Lock()
	defer e.startMu.Unlock()

	e.mu.Lock()
	state := e.state
	errMsg := e.errMsg
	e.mu.Unlock()

	switch state {
	case stateRunning:
		return nil
	case stateError:
		return fmt.Errorf("in error state: %s (use reload to reset)", errMsg)
	}
	return e.startProcess(m.basePath)
}

func (e *widgetEntry) startProcess(basePath string) error {
	mf := e.manifest
	mode, _ := mf.PlaceholderMode()

	e.mu.Lock()
	e.state = stateStarting
	e.mu.Unlock()

	var (
		transport  http.RoundTripper
		targetBase string
		argv       []string
	)

	env := append(os.Environ(),
		"TERM_LLM_WIDGET_ID="+mf.ID,
		"TERM_LLM_WIDGET_MOUNT="+mf.Mount,
		"TERM_LLM_WIDGET_BASE_PATH="+basePath+"/widgets/"+mf.Mount,
		"BASE_PATH="+basePath+"/widgets/"+mf.Mount,
	)

	switch mode {
	case "socket":
		if err := os.MkdirAll(socketRuntimeDir, 0700); err != nil {
			return e.setError(fmt.Errorf("create socket dir: %w", err))
		}
		sockPath := filepath.Join(socketRuntimeDir, mf.ID+".sock")
		_ = os.Remove(sockPath)

		argv = SubstArgs(mf.Command, "$SOCKET", sockPath)
		env = append(env,
			"TERM_LLM_WIDGET_SOCKET="+sockPath,
			"SOCKET="+sockPath,
		)
		transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		}
		targetBase = "http://widget"

	case "port":
		port, err := freePort()
		if err != nil {
			return e.setError(fmt.Errorf("allocate port: %w", err))
		}
		portStr := fmt.Sprintf("%d", port)
		argv = SubstArgs(mf.Command, "$PORT", portStr)
		env = append(env,
			"TERM_LLM_WIDGET_HOST=127.0.0.1",
			"TERM_LLM_WIDGET_PORT="+portStr,
			"HOST=127.0.0.1",
			"PORT="+portStr,
		)
		transport = http.DefaultTransport
		targetBase = "http://127.0.0.1:" + portStr

		e.mu.Lock()
		e.port = port
		e.mu.Unlock()
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = mf.Dir
	cmd.Env = env
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return e.setError(fmt.Errorf("start process: %w", err))
	}

	e.mu.Lock()
	e.proc = cmd.Process
	e.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		e.mu.Lock()
		if e.state == stateRunning || e.state == stateStarting {
			e.state = stateStopped
			e.proc = nil
			e.proxy = nil
		}
		e.mu.Unlock()
		log.Printf("[widget] %s process exited", mf.Mount)
	}()

	// Poll / until the widget responds or the timeout expires.
	client := &http.Client{Transport: transport, Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(startupTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		e.mu.Lock()
		alive := e.proc != nil
		e.mu.Unlock()
		if !alive {
			return e.setError(fmt.Errorf("process exited during startup"))
		}
		resp, err := client.Get(targetBase + "/")
		if err == nil {
			resp.Body.Close()
			lastErr = nil
			break
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr != nil {
		e.mu.Lock()
		p := e.proc
		e.mu.Unlock()
		if p != nil {
			_ = p.Kill()
		}
		return e.setError(fmt.Errorf("did not respond within %s: %v", startupTimeout, lastErr))
	}

	targetURL, _ := url.Parse(targetBase)
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Transport = transport

	e.mu.Lock()
	e.state = stateRunning
	e.proxy = proxy
	e.lastReq = time.Now()
	e.mu.Unlock()

	log.Printf("[widget] %s started (mode=%s)", mf.Mount, mode)
	return nil
}

func (e *widgetEntry) stopProcess() {
	e.mu.Lock()
	proc := e.proc
	e.state = stateStopped
	e.proc = nil
	e.proxy = nil
	e.port = 0
	e.mu.Unlock()

	if proc == nil {
		return
	}
	_ = proc.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		proc.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = proc.Kill()
	}
	if mode, _ := e.manifest.PlaceholderMode(); mode == "socket" {
		_ = os.Remove(filepath.Join(socketRuntimeDir, e.manifest.ID+".sock"))
	}
}

func (m *Manager) idleLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.reapIdle()
		}
	}
}

func (m *Manager) reapIdle() {
	m.mu.RLock()
	var toStop []*widgetEntry
	for _, e := range m.entries {
		e.mu.Lock()
		if e.state == stateRunning && time.Since(e.lastReq) > idleTimeout {
			toStop = append(toStop, e)
		}
		e.mu.Unlock()
	}
	m.mu.RUnlock()
	for _, e := range toStop {
		log.Printf("[widget] idle timeout, stopping %s", e.manifest.Mount)
		e.stopProcess()
	}
}

// Close stops all widget processes and the idle reaper.
func (m *Manager) Close() {
	m.stopOnce.Do(func() {
		close(m.stopCh)
		m.mu.RLock()
		defer m.mu.RUnlock()
		for _, e := range m.entries {
			e.stopProcess()
		}
	})
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

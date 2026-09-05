package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// Capture before command execution or any child launch. These lookup hints are
// never left in the environment inherited by tools.
var execRestartHint, execServiceHint = captureExecHints()

func captureExecHints() (string, string) {
	id, service := os.Getenv("TERM_LLM_RESTART_ID"), os.Getenv("TERM_LLM_RESTART_SERVICE_ID")
	_ = os.Unsetenv("TERM_LLM_RESTART_ID")
	_ = os.Unsetenv("TERM_LLM_RESTART_SERVICE_ID")
	return id, service
}

type webExecRun struct {
	boundaries int
	run        *responseRun
	request    llm.Request
	ctx        context.Context
	parked     bool
}

type webExecCoordinator struct {
	// Coalesce before mu: exec/checkpoint holds mu, so a duplicate must not
	// wait for failed exec and accidentally start a new drain afterwards.
	requested                        atomic.Bool
	ready                            bool
	safeTool                         func(llm.Tool) bool // test seam; nil uses concrete built-in types
	quarantined                      bool
	mu                               sync.Mutex
	server                           *serveServer
	ctx                              context.Context
	store                            session.ExecHandoffStore
	service, executable, unsupported string
	runs                             map[string]*webExecRun
	handlers                         int
	draining                         bool
	release                          chan struct{}
	timeout                          time.Duration
	exec                             func(string, []string, []string) error
}

func newWebExecCoordinator(ctx context.Context, s *serveServer, unsupported string) *webExecCoordinator {
	executable, err := os.Executable()
	if err != nil {
		unsupported = "cannot resolve serving executable"
	}
	// Resolve once, before an updater can replace/unlink the installed binary.
	if resolved, e := filepath.EvalSymlinks(executable); e == nil {
		executable = resolved
	}
	if s.cfgRef != nil && s.cfgRef.Serve.AutoTitle {
		unsupported = "automatic title providers are independently owned; disable serve.auto_title"
	}
	store, ok := session.AsExecHandoffStore(s.store)
	if !ok || !session.SupportsAtomicResponseRunTranscriptFencing(s.store) {
		unsupported = "durable fenced SQLite sessions required"
	}
	service := execServiceHint
	if _, err := uuid.Parse(service); err != nil {
		service = uuid.NewString()
	}
	// Bind the service UUID to this invocation, not a recycled PID or an env ID
	// from another listener/working directory. The database further scopes it.
	s.responseOwnerOnce.Do(func() { s.responseOwnerInstanceID = "owner_" + uuid.NewString() })
	sum := sha256.Sum256([]byte(strings.Join(os.Args, "\x00") + "\x00" + s.startupDir))
	service += ":" + hex.EncodeToString(sum[:])
	return &webExecCoordinator{server: s, ctx: ctx, store: store, service: service, executable: executable, unsupported: unsupported, runs: make(map[string]*webExecRun), timeout: 20 * time.Second, exec: execWebProcess}
}

func (c *webExecCoordinator) reject(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unsupported = reason
	if c.draining {
		c.abortLocked()
	}
}
func (c *webExecCoordinator) abortLocked() {
	if c.quarantined {
		return
	}
	if c.draining {
		close(c.release)
		c.draining = false
	}
}

// Unsupported owned activities taint this boot conservatively. We do not try
// to kill children or infer that a returned shell/custom tool has no descendants.
func (c *webExecCoordinator) observeTool(rt *serveRuntime, ev llm.Event) {
	if ev.Type == llm.EventToolActivity {
		c.reject("provider-managed native activity is unsupported")
		return
	}
	if ev.Type != llm.EventToolExecStart {
		return
	}
	tool, ok := rt.engine.Tools().Get(ev.ToolName)
	safe := webExecSafeTool
	if c.safeTool != nil {
		safe = c.safeTool
	}
	if !ok || !safe(tool) {
		c.reject("an unsupported tool or independently owned activity ran in this boot")
	}
}

// Names are not authority: custom/skill tools can replace a built-in name.
func webExecSafeTool(tool llm.Tool) bool {
	switch tool.(type) {
	case *tools.ReadFileTool, *tools.WriteFileTool, *tools.EditFileTool, *tools.GlobTool, *tools.GrepTool, *tools.UpdatePlanTool:
		return true
	default:
		return false
	}
}

type webExecTicketKey struct{}
type webExecTicket struct {
	once        sync.Once
	coordinator *webExecCoordinator
}

func (t *webExecTicket) release() {
	t.once.Do(func() { t.coordinator.mu.Lock(); t.coordinator.handlers--; t.coordinator.mu.Unlock() })
}
func releaseWebExecTicket(ctx context.Context) {
	if t, ok := ctx.Value(webExecTicketKey{}).(*webExecTicket); ok {
		t.release()
	}
}

// All mutation handlers are accounted for before dispatch. Only a response
// stream transfers its ticket to a tracked engine; detached mutation APIs make
// this boot ineligible. Read-only SSE streams need not finish for exec.
func (c *webExecCoordinator) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, c.server.cfg.basePath)
		if strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/admin/") || strings.HasPrefix(path, "/widgets/") {
			// Reuse the real auth policy before any admission/taint effects.
			admitted := false
			authRequest := r.Clone(r.Context())
			authRequest.URL.Path = path
			c.server.auth(func(http.ResponseWriter, *http.Request) { admitted = true })(w, authRequest)
			if !admitted {
				return
			}
		}
		unsafe := strings.Contains(path, "/shell") || (strings.Contains(path, "/widgets") && c.server.widgetsMgr != nil) || strings.HasPrefix(path, "/api/sessions/")
		mutation := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		cancel := strings.HasSuffix(path, "/cancel")
		if mutation && path != "/v1/responses" && path != "/v1/sessions" && !strings.HasSuffix(path, "/attention/seen") && !cancel {
			unsafe = true
		}
		c.mu.Lock()
		if cancel {
			c.abortLocked()
		}
		if c.draining && mutation && !cancel {
			c.mu.Unlock()
			w.Header().Set("Retry-After", "1")
			http.Error(w, "web reload draining; retry", http.StatusServiceUnavailable)
			return
		}
		if unsafe {
			c.unsupported = "unsupported owned HTTP activity in this boot"
			c.abortLocked()
		}
		readStream := r.Method == http.MethodGet && (path == "/v1/events" || (strings.HasPrefix(path, "/v1/responses/") && strings.HasSuffix(path, "/events")))
		if readStream {
			c.mu.Unlock()
			next.ServeHTTP(w, r)
			return
		}
		c.handlers++
		c.mu.Unlock()
		ticket := &webExecTicket{coordinator: c}
		defer ticket.release()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), webExecTicketKey{}, ticket)))
	})
}

// Called with c.mu held by response admission; registration happens before
// the goroutine can execute a provider or spawn a tool.
func (c *webExecCoordinator) register(run *responseRun, ctx context.Context, rt *serveRuntime, req *llm.Request, stateful bool, options startResponseRunOptions) {
	if !stateful || req.MaxTurns <= 0 || options.rush != nil || options.modelSwap != nil || options.runtimeSetup != nil || rt.provider.Capabilities().InlineToolLoop || rt.mcpManagerSnapshot() != nil {
		c.unsupported = fmt.Sprintf("unsupported response ownership (stateful=%t UI=%t rush=%t swap=%t setup=%t inline=%t MCP=%t)", stateful, options.uiSession, options.rush != nil, options.modelSwap != nil, options.runtimeSetup != nil, rt.provider.Capabilities().InlineToolLoop, rt.mcpManagerSnapshot() != nil)
		c.abortLocked()
		return
	}
	copyReq := *req
	copyReq.Messages = nil
	copyReq.Tools = make([]llm.ToolSpec, len(req.Tools))
	for i, spec := range req.Tools {
		copyReq.Tools[i] = llm.ToolSpec{Name: spec.Name}
	}
	copyReq.ApprovalTranscriptPrefix = nil
	copyReq.ModelBoundary = nil
	entry := &webExecRun{run: run, request: copyReq, ctx: ctx}
	run.webExec = c
	c.runs[run.id] = entry
	req.ModelBoundary = func(ctx context.Context) error {
		c.mu.Lock()
		entry.boundaries++
		if !c.draining {
			c.mu.Unlock()
			return ctx.Err()
		}
		// A boundary must already be durable, not wait for asynchronous writes
		// while holding global admission. The batch transaction validates leases.
		run.persistence.mu.Lock()
		durable := run.persistence.inflight == 0 && !run.persistence.failed && (len(run.persistence.outputKeys) == 0 || run.persistence.maxRev > 0)
		run.persistence.mu.Unlock()
		if !durable || !run.atomicTranscriptFencing || run.validateLifecycle == nil {
			c.abortLocked()
			c.mu.Unlock()
			return nil
		}
		entry.parked = true
		release := c.release
		c.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-release:
		}
		c.mu.Lock()
		entry.parked = false
		c.mu.Unlock()
		return ctx.Err()
	}
}
func (c *webExecCoordinator) settled(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.runs[id]; entry != nil {
		// Cancelled Execute implementations may still own side effects.
		entry.run.mu.Lock()
		complete := entry.run.status == "completed"
		entry.run.mu.Unlock()
		if !complete {
			c.unsupported = "a cancelled or failed invocation may still own work"
			c.abortLocked()
		}
	}
	delete(c.runs, id)
}

func (c *webExecCoordinator) request() {
	if !c.requested.CompareAndSwap(false, true) {
		return
	}
	started := false
	defer func() {
		if !started {
			c.requested.Store(false)
		}
	}()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.draining {
		return
	}
	if c.unsupported != "" {
		log.Printf("[reload] SIGUSR2 rejected: %s", c.unsupported)
		return
	}
	if c.ctx.Err() != nil {
		return
	}
	if !c.ready {
		log.Printf("[reload] SIGUSR2 rejected: web startup is not ready")
		return
	}
	c.draining = true
	c.release = make(chan struct{})
	started = true
	go c.drain(c.release)
}
func (c *webExecCoordinator) drain(generation chan struct{}) {
	defer c.requested.Store(false)
	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-generation:
			return
		case <-c.ctx.Done():
			c.mu.Lock()
			if c.release == generation {
				c.abortLocked()
			}
			c.mu.Unlock()
			return
		case <-timer.C:
			c.mu.Lock()
			if c.release == generation {
				c.abortLocked()
				log.Printf("[reload] drain timed out; work left running")
			}
			c.mu.Unlock()
			return
		case <-tick.C:
		}
		c.mu.Lock()
		if !c.draining || c.release != generation {
			c.mu.Unlock()
			return
		}
		ready := c.handlers == 0
		for _, r := range c.runs {
			if !r.parked {
				ready = false
			}
		}
		if !ready {
			c.mu.Unlock()
			continue
		}
		err := c.replaceLocked()
		c.abortLocked()
		c.mu.Unlock()
		if err != nil {
			log.Printf("[reload] self-exec aborted; original process retained: %v", err)
		}
		return
	}
}

func (c *webExecCoordinator) replaceLocked() error {
	if c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	if info, err := os.Stat(c.executable); err != nil || info.IsDir() || info.Mode()&0111 == 0 {
		return errors.New("installed executable is unavailable or not executable")
	}
	c.server.modelsMu.Lock()
	for _, provider := range c.server.modelsProviders {
		if provider.Capabilities().InlineToolLoop {
			c.server.modelsMu.Unlock()
			return errors.New("model listing initialized a native provider")
		}
	}
	c.server.modelsMu.Unlock()
	if c.server.sessionMgr != nil {
		c.server.sessionMgr.mu.Lock()
		for _, rt := range c.server.sessionMgr.sessions {
			if rt.provider.Capabilities().InlineToolLoop || rt.mcpManagerSnapshot() != nil {
				c.server.sessionMgr.mu.Unlock()
				return errors.New("a runtime owns an unsupported provider or MCP manager")
			}
		}
		c.server.sessionMgr.mu.Unlock()
	}
	c.server.autoTitleMu.Lock()
	titles := len(c.server.autoTitleFlights)
	c.server.autoTitleMu.Unlock()
	if titles != 0 {
		return errors.New("title generation still owns provider work")
	}
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	id := uuid.NewString()
	var entries []session.ExecHandoff
	for _, r := range c.runs {
		if webExecRunCancelled(r) {
			return context.Canceled
		}
		rev, err := c.server.transcriptRev(ctx, r.run.sessionID)
		if err != nil {
			return err
		}
		saved := r.request
		saved.MaxTurns -= r.boundaries - 1
		if saved.MaxTurns <= 0 {
			return errors.New("no model turn budget remains")
		}
		request, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		entries = append(entries, session.ExecHandoff{ID: id, ServiceID: c.service, SourceOwnerID: c.server.responseOwnerID(), SessionID: r.run.sessionID, SourceResponseID: r.run.id, SourceFence: r.run.fencingToken, CheckpointRev: rev, Request: request})
	}
	if err := c.store.PrepareExecHandoff(ctx, entries); err != nil {
		return fmt.Errorf("durable checkpoint: %w", err)
	}
	// Admission and Stop are serialized through mu. TERM and invocation deadlines
	// remain authoritative even while persistence was in progress.
	err := c.ctx.Err()
	for _, r := range c.runs {
		if webExecRunCancelled(r) {
			err = context.Canceled
		}
	}
	if err == nil {
		env := webExecEnviron(id, strings.SplitN(c.service, ":", 2)[0])
		err = c.exec(c.executable, append([]string(nil), os.Args...), env)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cleanupCancel()
	if cleanupErr := c.store.DiscardExecHandoff(cleanupCtx, id, c.server.responseOwnerID()); cleanupErr != nil {
		// Never leave an executable intent behind and silently resume work. The
		// transcript fence prevents stale adoption, but require operator attention.
		c.unsupported = "failed to discard restart intent"
		c.quarantined = true
		return fmt.Errorf("discard self-exec intent: %w", cleanupErr)
	}
	return err
}

func (c *webExecCoordinator) resume() error {
	if execRestartHint == "" {
		return nil
	}
	if c.unsupported != "" {
		return errors.New("self-exec recovery unavailable for this configuration")
	}
	entries, err := c.store.ReadExecHandoff(c.ctx, execRestartHint, c.service)
	if err != nil {
		return err
	}
	for _, h := range entries {
		if err := c.resumeSession(h); err != nil {
			// Never terminate other newly admitted engines or replay a rejected intent.
			log.Printf("[reload] a continuation was not admitted; inspect the session before continuing")
		}
	}
	return nil
}

func webExecEnviron(id, service string) []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "TERM_LLM_RESTART_ID=") && !strings.HasPrefix(entry, "TERM_LLM_RESTART_SERVICE_ID=") {
			env = append(env, entry)
		}
	}
	env = append(env, tools.HubDelegationEnviron()...)
	env = append(env, hubRegistrationEnviron()...)
	return append(env, "TERM_LLM_RESTART_ID="+id, "TERM_LLM_RESTART_SERVICE_ID="+service)
}

func (c *webExecCoordinator) resumeSession(h session.ExecHandoff) error {
	sess, err := c.server.store.Get(c.ctx, h.SessionID)
	if err != nil {
		return err
	}
	provider := sess.ProviderKey
	if provider == "" {
		provider = sess.Provider
	}
	rt, _, err := c.server.runtimeForProviderModelRequest(c.ctx, h.SessionID, provider, sess.Model)
	if err != nil {
		return err
	}
	if rt.provider.Capabilities().InlineToolLoop || rt.mcpManagerSnapshot() != nil {
		return errors.New("replacement provider does not support safe continuation")
	}
	var req llm.Request
	if err = json.Unmarshal(h.Request, &req); err != nil {
		return err
	}
	// Re-resolve selected tools from the replacement registry, retaining
	// the original request's tool selection without trusting old schemas.
	for i, spec := range req.Tools {
		tool, ok := rt.engine.Tools().Get(spec.Name)
		if !ok {
			return fmt.Errorf("replacement tool %q is unavailable", spec.Name)
		}
		req.Tools[i] = tool.Spec()
	}
	message := llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: "[Internal web hot-reload recovery]\nContinue the unfinished user task from this durable model boundary. This is not a new user message. All preceding tool results are persisted; do not replay those actions. No pending or unknown action is authorized for replay. Tools remain available for the remaining task. Do not initiate another restart."}}}
	_, err = c.server.startResponseRun(rt, true, false, []llm.Message{message}, req, h.SessionID, startResponseRunOptions{uiSession: true, previousResponseID: h.SourceResponseID, execRestartID: h.ID, execServiceID: h.ServiceID})
	if err != nil {
		return fmt.Errorf("admit self-exec continuation: %w", err)
	}
	return nil
}

func webExecRunCancelled(entry *webExecRun) bool {
	if entry.ctx.Err() != nil {
		return true
	}
	entry.run.mu.Lock()
	defer entry.run.mu.Unlock()
	return entry.run.cancelRequested || entry.run.status != "in_progress"
}

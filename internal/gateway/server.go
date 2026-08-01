package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/gateway/protocol"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/providerhttp"
	"github.com/samsaffron/term-llm/internal/search"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
)

const (
	defaultMaxBodyBytes          = 64 << 20
	DefaultUpstreamRetryAttempts = 3
	DefaultUpstreamRetryElapsed  = 20 * time.Second
)

type ProviderFactory func(*config.Config, string, string) (llm.Provider, error)
type ConfigLoader func() (*config.Config, error)

type ServerConfig struct {
	Config                  *config.Config
	ConfigLoader            ConfigLoader
	Clients                 *ClientStore
	Sealer                  *StateSealer
	Usage                   UsageRecorder
	ProviderFactory         ProviderFactory
	Searcher                search.Searcher
	FetchTool               *llm.ReadURLTool
	Policy                  Policy
	MaxBodyBytes            int64
	IdleTimeout             time.Duration
	ToolTimeout             time.Duration
	CatalogTTL              time.Duration
	ModelListTimeout        time.Duration
	UpstreamRetryAttempts   int
	UpstreamRetryMaxElapsed time.Duration
	RunTempRoot             string
}

type runState struct {
	clientID  string
	cancel    context.CancelFunc
	mu        sync.Mutex
	callbacks map[string]chan llm.ToolExecutionResponse
}

type clientLimits struct {
	inference chan struct{}
	search    chan struct{}
	fetch     chan struct{}
	searchRPS *rate.Limiter
	fetchRPS  *rate.Limiter
}

type inferenceExecution struct {
	server   *Server
	client   Client
	envelope protocol.InferenceRequest
	request  llm.Request
	entry    protocol.CatalogEntry
	provider llm.Provider
	stream   llm.Stream
	ctx      context.Context
	cancel   context.CancelFunc
	release  func()
	tempDir  string
	started  time.Time
	total    llm.Usage
	finish   sync.Once
}

type inferenceRequestError struct {
	Status  int
	Code    string
	Message string
}

func (e *inferenceExecution) addUsage(use llm.Usage) { e.total.Add(use) }

func (e *inferenceExecution) close(errorCode string) {
	if e == nil {
		return
	}
	e.finish.Do(func() {
		if errorCode == "" && errors.Is(e.ctx.Err(), context.Canceled) {
			errorCode = "canceled"
		}
		_ = e.stream.Close()
		e.cancel()
		if e.tempDir != "" {
			_ = os.RemoveAll(e.tempDir)
		}
		if e.release != nil {
			e.release()
		}
		e.server.recordUsage(e.client, e.envelope, e.request, e.total, errorCode, e.started)
	})
}

type Server struct {
	cfg ServerConfig

	configMu sync.RWMutex
	config   *config.Config

	catalogMu              sync.RWMutex
	catalog                protocol.Catalog
	catalogFetchedAt       time.Time
	catalogProviderFetched map[string]time.Time
	catalogRefresh         singleflight.Group
	configRefreshMu        sync.Mutex
	configFetchedAt        time.Time

	runsMu sync.RWMutex
	runs   map[string]*runState

	limitsMu sync.Mutex
	limits   map[string]*clientLimits
}

func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Config == nil || cfg.Clients == nil || cfg.Sealer == nil {
		return nil, fmt.Errorf("gateway server requires config, client store, and state sealer")
	}
	if cfg.ProviderFactory == nil {
		cfg.ProviderFactory = llm.NewProviderByName
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 5 * time.Minute
	}
	if cfg.ToolTimeout <= 0 {
		cfg.ToolTimeout = 10 * time.Minute
	}
	if cfg.CatalogTTL <= 0 {
		cfg.CatalogTTL = 5 * time.Minute
	}
	if cfg.ModelListTimeout <= 0 {
		cfg.ModelListTimeout = 5 * time.Second
	}
	if cfg.UpstreamRetryAttempts <= 0 {
		cfg.UpstreamRetryAttempts = DefaultUpstreamRetryAttempts
	}
	if cfg.UpstreamRetryMaxElapsed <= 0 {
		cfg.UpstreamRetryMaxElapsed = DefaultUpstreamRetryElapsed
	}
	if strings.TrimSpace(cfg.RunTempRoot) != "" {
		cfg.RunTempRoot = filepath.Clean(cfg.RunTempRoot)
		if err := prepareRunTempRoot(cfg.RunTempRoot); err != nil {
			return nil, err
		}
	}
	return &Server{
		cfg: cfg, config: cfg.Config, configFetchedAt: time.Now().UTC(),
		catalog: protocol.Catalog{Version: protocol.Version}, catalogProviderFetched: make(map[string]time.Time),
		runs: make(map[string]*runState), limits: make(map[string]*clientLimits),
	}, nil
}

func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	clean := path.Clean(r.URL.Path)
	if clean == "/v1/responses" || clean == "/v1/models" {
		client, ok := s.authenticateResponses(w, r)
		if !ok {
			return
		}
		switch {
		case clean == "/v1/responses" && r.Method == http.MethodPost:
			s.handleResponses(w, r, client)
		case clean == "/v1/models" && r.Method == http.MethodGet:
			s.handleResponsesModels(w, r, client)
		default:
			w.Header().Set("Allow", map[string]string{"/v1/responses": http.MethodPost, "/v1/models": http.MethodGet}[clean])
			s.writeResponsesError(w, http.StatusMethodNotAllowed, "method_not_allowed", "HTTP method is not supported for this endpoint", "")
		}
		return
	}
	if clean == "/g1/health" && r.Method == http.MethodGet {
		s.writeJSON(w, http.StatusOK, protocol.Health{Version: protocol.Version, Status: "ok"})
		return
	}
	if clean == "/g1/enroll" && r.Method == http.MethodPost {
		s.handleEnroll(w, r)
		return
	}
	if !s.checkVersion(w, r) {
		return
	}
	client, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	switch {
	case clean == "/g1/catalog" && r.Method == http.MethodGet:
		s.handleCatalog(w, r, client)
	case clean == "/g1/inference" && r.Method == http.MethodPost:
		s.handleInference(w, r, client)
	case clean == "/g1/search" && r.Method == http.MethodPost:
		s.handleSearch(w, r, client)
	case clean == "/g1/fetch" && r.Method == http.MethodPost:
		s.handleFetch(w, r, client)
	case strings.HasPrefix(clean, "/g1/runs/"):
		s.handleRun(w, r, client, clean)
	default:
		s.writeError(w, http.StatusNotFound, "not_found", "gateway endpoint not found", "")
	}
}

func (s *Server) checkVersion(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get(protocol.VersionHeader) != "1" {
		s.writeUnsupportedVersion(w, "")
		return false
	}
	return true
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (Client, bool) {
	client, ok := s.authenticateBearer(r)
	if !ok {
		s.writeError(w, http.StatusUnauthorized, "gateway_client_unauthorized", "gateway client credential is missing or invalid; update gateway.token/token_file on the satellite", "")
		return Client{}, false
	}
	return client, true
}

func (s *Server) authenticateResponses(w http.ResponseWriter, r *http.Request) (Client, bool) {
	client, ok := s.authenticateBearer(r)
	if !ok {
		s.writeResponsesError(w, http.StatusUnauthorized, "gateway_client_unauthorized", "gateway client bearer credential is missing, invalid, or revoked", "")
		return Client{}, false
	}
	return client, true
}

func (s *Server) authenticateBearer(r *http.Request) (Client, bool) {
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		return Client{}, false
	}
	token := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	if token == "" {
		return Client{}, false
	}
	return s.cfg.Clients.Authenticate(token)
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired gateway enrollment token", "")
		return
	}
	var req protocol.EnrollmentRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	if req.Version != protocol.Version {
		s.writeUnsupportedVersion(w, "")
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	client, clientToken, err := s.cfg.Clients.ConsumeEnrollment(token, req.Name)
	if err != nil {
		slog.Warn("gateway enrollment rejected", "reason", err)
		s.writeError(w, http.StatusUnauthorized, "enrollment_rejected", "invalid, expired, or already-used gateway enrollment token; ask the gateway operator for a new token", "")
		return
	}
	s.writeJSON(w, http.StatusCreated, protocol.EnrollmentResponse{Version: protocol.Version, ClientID: client.ID, Token: clientToken})
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request, client Client) {
	catalog, err := s.currentCatalog(r.Context())
	if err != nil {
		slog.Error("refresh gateway catalog", "error", err)
		s.writeError(w, http.StatusServiceUnavailable, "catalog_unavailable", "gateway catalog is temporarily unavailable; retry or check provider configuration on the gateway", "")
		return
	}
	catalog = filterCatalog(catalog, s.cfg.Policy, client.Policy)
	data, err := json.Marshal(catalog)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "internal", "catalog unavailable", "")
		return
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleInference(w http.ResponseWriter, r *http.Request, client Client) {
	var envelope protocol.InferenceRequest
	if !s.decodeJSON(w, r, &envelope) {
		return
	}
	if envelope.Version != protocol.Version {
		s.writeUnsupportedVersion(w, envelope.RequestID)
		return
	}
	if strings.TrimSpace(envelope.RequestID) == "" || strings.TrimSpace(envelope.Provider) == "" {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "request_id and provider are required", envelope.RequestID)
		return
	}
	providerReq, err := llm.DecodeGatewayRequest(envelope.Request)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), envelope.RequestID)
		return
	}
	execution, requestErr := s.startInference(r.Context(), client, envelope, providerReq, false)
	if requestErr != nil {
		s.writeError(w, requestErr.Status, requestErr.Code, requestErr.Message, envelope.RequestID)
		return
	}
	errorCode := ""
	defer func() { execution.close(errorCode) }()
	providerReq = execution.request
	provider := execution.provider
	stream := execution.stream
	ctx := execution.ctx
	entry := execution.entry

	runID, err := randomSecret("run", 16)
	if err != nil {
		errorCode = "internal"
		s.writeError(w, http.StatusInternalServerError, "internal", "could not create gateway run", envelope.RequestID)
		return
	}
	run := &runState{clientID: client.ID, cancel: execution.cancel, callbacks: make(map[string]chan llm.ToolExecutionResponse)}
	s.runsMu.Lock()
	s.runs[runID] = run
	s.runsMu.Unlock()
	defer func() {
		s.runsMu.Lock()
		delete(s.runs, runID)
		s.runsMu.Unlock()
	}()
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	go s.watchIdle(ctx, execution.cancel, &lastActivity)

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, "streaming_unsupported", "streaming is unavailable", envelope.RequestID)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if !writeSSE(w, protocol.StreamRecord{Version: protocol.Version, Type: "run", RequestID: envelope.RequestID, RunID: runID}) {
		return
	}
	flusher.Flush()

	for {
		event, recvErr := stream.Recv()
		lastActivity.Store(time.Now().UnixNano())
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			_, errorCode = s.logProviderStreamError(execution, recvErr)
			_ = writeSSE(w, protocol.StreamRecord{Version: protocol.Version, Type: "error", RequestID: envelope.RequestID, RunID: runID, Error: &protocol.Error{Code: errorCode, Message: safeProviderErrorMessage(errorCode, envelope.Provider), RequestID: envelope.RequestID}})
			flusher.Flush()
			break
		}
		if event.Type == llm.EventUsage && event.Use != nil {
			execution.addUsage(*event.Use)
		}
		var publicError *protocol.Error
		if event.Err != nil {
			_, classified := classifyProviderError(event.Err, entry.Type)
			publicError = &protocol.Error{Code: classified, Message: safeProviderErrorMessage(classified, envelope.Provider), RequestID: envelope.RequestID}
			slog.Error("gateway provider event failed", "request_id", envelope.RequestID, "provider", envelope.Provider, "event_type", event.Type, "error", event.Err)
			if event.Type == llm.EventError {
				errorCode = classified
				_ = writeSSE(w, protocol.StreamRecord{Version: protocol.Version, Type: "error", RequestID: envelope.RequestID, RunID: runID, Error: publicError})
				flusher.Flush()
				break
			}
		}
		wireEvent, encodeErr := llm.EncodeGatewayEvent(event, publicError)
		if encodeErr != nil {
			errorCode = "encoding_error"
			break
		}
		record := protocol.StreamRecord{Version: protocol.Version, Type: "event", RequestID: envelope.RequestID, RunID: runID, Event: wireEvent}
		if event.ToolResponse != nil {
			callbackID, idErr := randomSecret("callback", 8)
			if idErr != nil {
				errorCode = "internal"
				_ = writeSSE(w, protocol.StreamRecord{Version: protocol.Version, Type: "error", RequestID: envelope.RequestID, RunID: runID, Error: &protocol.Error{Code: errorCode, Message: "gateway callback unavailable", RequestID: envelope.RequestID}})
				flusher.Flush()
				break
			}
			callback := make(chan llm.ToolExecutionResponse, 1)
			run.mu.Lock()
			run.callbacks[callbackID] = callback
			run.mu.Unlock()
			record.Type = "tool_callback"
			record.CallbackPath = "/g1/runs/" + runID + "/tools/" + callbackID
			if !writeSSE(w, record) {
				return
			}
			flusher.Flush()
			// A pending satellite callback is active work, not provider idleness.
			lastActivity.Store(time.Now().Add(s.cfg.ToolTimeout).UnixNano())
			timer := time.NewTimer(s.cfg.ToolTimeout)
			var response llm.ToolExecutionResponse
			select {
			case response = <-callback:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
				response.Err = fmt.Errorf("satellite tool callback timed out")
			case <-ctx.Done():
				timer.Stop()
				return
			}
			select {
			case event.ToolResponse <- response:
			case <-ctx.Done():
				return
			}
			lastActivity.Store(time.Now().UnixNano())
			run.mu.Lock()
			delete(run.callbacks, callbackID)
			run.mu.Unlock()
			continue
		}
		if !writeSSE(w, record) {
			return
		}
		flusher.Flush()
	}
	if errorCode == "" && errors.Is(ctx.Err(), context.Canceled) {
		errorCode = "canceled"
	}
	if exporter, ok := provider.(llm.ProviderStateExporter); ok {
		if plain, valid := exporter.ExportProviderState(); valid {
			if sealed, sealErr := s.cfg.Sealer.Seal(client.ID, envelope.Provider, plain); sealErr == nil {
				_ = writeSSE(w, protocol.StreamRecord{Version: protocol.Version, Type: "state", RequestID: envelope.RequestID, RunID: runID, State: sealed})
				flusher.Flush()
			}
		}
	}
	if errorCode == "" {
		_ = writeSSE(w, protocol.StreamRecord{Version: protocol.Version, Type: "done", RequestID: envelope.RequestID, RunID: runID})
		flusher.Flush()
	}
}

func (s *Server) watchIdle(ctx context.Context, cancel context.CancelFunc, last *atomic.Int64) {
	interval := s.cfg.IdleTimeout / 4
	if interval > time.Second {
		interval = time.Second
	}
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, last.Load())) > s.cfg.IdleTimeout {
				cancel()
				return
			}
		}
	}
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request, client Client, clean string) {
	parts := strings.Split(strings.TrimPrefix(clean, "/g1/runs/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		s.writeError(w, http.StatusNotFound, "not_found", "run not found", "")
		return
	}
	s.runsMu.RLock()
	run := s.runs[parts[0]]
	s.runsMu.RUnlock()
	if run == nil || run.clientID != client.ID {
		s.writeError(w, http.StatusNotFound, "not_found", "run not found", "")
		return
	}
	if r.Method == http.MethodDelete && len(parts) == 1 {
		run.cancel()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodPost && len(parts) == 3 && parts[1] == "tools" {
		var req protocol.ToolResultRequest
		if !s.decodeJSON(w, r, &req) || req.Version != protocol.Version {
			return
		}
		response, err := llm.DecodeGatewayToolResponse(req.Result)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid_tool_result", err.Error(), "")
			return
		}
		run.mu.Lock()
		callback := run.callbacks[parts[2]]
		run.mu.Unlock()
		if callback == nil {
			s.writeError(w, http.StatusNotFound, "not_found", "tool callback not found", "")
			return
		}
		select {
		case callback <- response:
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
		return
	}
	s.writeError(w, http.StatusNotFound, "not_found", "run endpoint not found", "")
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request, client Client) {
	if s.cfg.Searcher == nil || !s.cfg.Policy.AllowSearch || !client.Policy.AllowSearch {
		s.writeError(w, http.StatusForbidden, "policy_denied", "gateway search is unavailable", "")
		return
	}
	release, code, ok := s.acquireTool(client, true)
	if !ok {
		s.writeError(w, http.StatusTooManyRequests, code, "gateway search limit reached for this client; wait and retry", "")
		return
	}
	defer release()
	var req protocol.SearchRequest
	if !s.decodeJSON(w, r, &req) || req.Version != protocol.Version {
		return
	}
	if req.MaxResults <= 0 || req.MaxResults > 100 {
		req.MaxResults = 20
	}
	results, err := s.cfg.Searcher.Search(r.Context(), req.Query, req.MaxResults)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "search_failed", "gateway search failed upstream; retry, then check gateway-side search configuration", "")
		return
	}
	out := make([]protocol.SearchResult, 0, len(results))
	for _, result := range results {
		out = append(out, protocol.SearchResult{Title: result.Title, URL: result.URL, Snippet: result.Snippet})
	}
	s.writeJSON(w, http.StatusOK, protocol.SearchResponse{Version: protocol.Version, Results: out})
}

func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request, client Client) {
	if s.cfg.FetchTool == nil || !s.cfg.Policy.AllowFetch || !client.Policy.AllowFetch {
		s.writeError(w, http.StatusForbidden, "policy_denied", "gateway fetch is unavailable", "")
		return
	}
	release, code, ok := s.acquireTool(client, false)
	if !ok {
		s.writeError(w, http.StatusTooManyRequests, code, "gateway fetch limit reached for this client; wait and retry", "")
		return
	}
	defer release()
	var req protocol.FetchRequest
	if !s.decodeJSON(w, r, &req) || req.Version != protocol.Version {
		return
	}
	args, _ := json.Marshal(map[string]string{"url": req.URL})
	output, err := s.cfg.FetchTool.Execute(r.Context(), args)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "fetch_failed", "gateway fetch failed upstream; retry, then check gateway-side fetch configuration", "")
		return
	}
	s.writeJSON(w, http.StatusOK, protocol.FetchResponse{Version: protocol.Version, Content: output.Content})
}

func prepareRunTempRoot(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect gateway run temp root: %w", err)
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			return fmt.Errorf("create gateway run temp root: %w", err)
		}
		info, err = os.Lstat(root)
		if err != nil {
			return fmt.Errorf("inspect created gateway run temp root: %w", err)
		}
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("gateway run temp root must be a real directory: %s", root)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("secure gateway run temp root: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("scan gateway run temp root: %w", err)
	}
	for _, entry := range entries {
		// This state-owned directory is reserved for gateway run directories. The
		// prefix and directory checks retain unrelated operator-created content.
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "run-") || strings.Contains(entry.Name(), string(os.PathSeparator)) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("remove stale gateway run directory %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func (s *Server) newRunTempDir() (string, error) {
	if s.cfg.RunTempRoot == "" {
		return os.MkdirTemp("", "term-llm-gateway-run-*")
	}
	return os.MkdirTemp(s.cfg.RunTempRoot, "run-")
}

func (s *Server) centralConfig() *config.Config {
	clone := *s.currentConfig()
	clone.Gateway = config.GatewayConfig{}
	return &clone
}

func (s *Server) currentConfig() *config.Config {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.config
}

func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	data, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxBodyBytes+1))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_json", "could not read gateway request body", "")
		return false
	}
	if int64(len(data)) > s.cfg.MaxBodyBytes {
		s.writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "gateway request body exceeds the configured limit", "")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_json", "invalid gateway request body", "")
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		s.writeError(w, http.StatusBadRequest, "invalid_json", "gateway request must contain one JSON value", "")
		return false
	}
	return true
}

func (s *Server) writeUnsupportedVersion(w http.ResponseWriter, requestID string) {
	s.writeJSON(w, http.StatusUpgradeRequired, protocol.Error{
		Code:              "unsupported_version",
		Message:           "gateway protocol version 1 is required",
		RequestID:         requestID,
		SupportedVersions: []int{protocol.Version},
	})
}

func (s *Server) writeError(w http.ResponseWriter, status int, code, message, requestID string) {
	s.writeJSON(w, status, protocol.Error{Code: code, Message: message, RequestID: requestID})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(protocol.VersionHeader, "1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeSSE(w io.Writer, record protocol.StreamRecord) bool {
	data, err := json.Marshal(record)
	if err != nil {
		return false
	}
	_, err = fmt.Fprintf(w, "event: gateway\ndata: %s\n\n", data)
	return err == nil
}

func (s *Server) recordFailure(client Client, envelope protocol.InferenceRequest, req llm.Request, code string, started time.Time) {
	s.recordUsage(client, envelope, req, llm.Usage{}, code, started)
}

func (s *Server) recordUsage(client Client, envelope protocol.InferenceRequest, req llm.Request, use llm.Usage, code string, started time.Time) {
	if s.cfg.Usage == nil {
		return
	}
	record := UsageRecord{
		StartedAt: started.UTC(), CompletedAt: time.Now().UTC(), ClientID: client.ID, ClientName: client.Name,
		ProviderKey: envelope.Provider, Model: req.Model, RequestID: envelope.RequestID, SessionID: req.SessionID,
		InputTokens: use.InputTokens, OutputTokens: use.OutputTokens, CachedInputTokens: use.CachedInputTokens,
		CacheWriteTokens: use.CacheWriteTokens, ReasoningTokens: use.ReasoningTokens, ErrorCode: code,
	}
	record.CostUSD = estimateUsageCost(envelope.Provider, req.Model, use)
	if err := s.cfg.Usage.Record(record); err != nil {
		slog.Error("record gateway usage", "error", err)
	}
}

type providerCatalogResult struct {
	entry protocol.CatalogEntry
	found bool
}

func (s *Server) refreshConfigIfStale() error {
	if s.cfg.ConfigLoader == nil {
		return nil
	}
	s.configMu.RLock()
	fetchedAt := s.configFetchedAt
	s.configMu.RUnlock()
	if !fetchedAt.IsZero() && time.Since(fetchedAt) < s.cfg.CatalogTTL {
		return nil
	}
	s.configRefreshMu.Lock()
	defer s.configRefreshMu.Unlock()
	s.configMu.RLock()
	fetchedAt = s.configFetchedAt
	s.configMu.RUnlock()
	if !fetchedAt.IsZero() && time.Since(fetchedAt) < s.cfg.CatalogTTL {
		return nil
	}
	loaded, err := s.cfg.ConfigLoader()
	if err != nil {
		// Keep the last known-good configuration and avoid coupling inference to a
		// transient local reload failure.
		s.configMu.Lock()
		s.configFetchedAt = time.Now().UTC()
		s.configMu.Unlock()
		return fmt.Errorf("reload gateway config: %w", err)
	}
	if loaded != nil {
		clone := *loaded
		clone.Gateway = config.GatewayConfig{}
		s.configMu.Lock()
		s.config = &clone
		s.configFetchedAt = time.Now().UTC()
		s.configMu.Unlock()
	}
	return nil
}

func (s *Server) cachedCatalogProvider(name string) (protocol.CatalogEntry, time.Time, bool) {
	s.catalogMu.RLock()
	defer s.catalogMu.RUnlock()
	for _, entry := range s.catalog.Providers {
		if entry.Key == name {
			return entry, s.catalogProviderFetched[name], true
		}
	}
	return protocol.CatalogEntry{}, time.Time{}, false
}

func (s *Server) storeCatalogProviderAt(entry protocol.CatalogEntry, fetchedAt time.Time) {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	replaced := false
	for i := range s.catalog.Providers {
		if s.catalog.Providers[i].Key == entry.Key {
			s.catalog.Providers[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		s.catalog.Providers = append(s.catalog.Providers, entry)
	}
	sort.Slice(s.catalog.Providers, func(i, j int) bool { return s.catalog.Providers[i].Key < s.catalog.Providers[j].Key })
	now := time.Now().UTC()
	s.catalog.Version = protocol.Version
	s.catalog.GeneratedAt = now
	s.catalog.Features = protocol.CatalogFeatures{Search: s.cfg.Searcher != nil, Fetch: s.cfg.FetchTool != nil}
	s.catalogFetchedAt = now
	s.catalogProviderFetched[entry.Key] = fetchedAt
}

func (s *Server) storeCatalogProvider(entry protocol.CatalogEntry) {
	s.storeCatalogProviderAt(entry, time.Now().UTC())
}

func (s *Server) providerConfigured(name string) bool {
	cfg := s.currentConfig()
	_, ok := cfg.Providers[name]
	return ok && cfg.IsExplicitProvider(name)
}

func configuredCatalogEntry(cfg *config.Config, name string) (protocol.CatalogEntry, bool) {
	if cfg == nil || !cfg.IsExplicitProvider(name) {
		return protocol.CatalogEntry{}, false
	}
	pc, ok := cfg.Providers[name]
	if !ok {
		return protocol.CatalogEntry{}, false
	}
	providerType := config.InferProviderType(name, pc.Type)
	entry := protocol.CatalogEntry{
		Key: name, Type: string(providerType), CLI: isCLIProvider(providerType),
		AllowUnlistedModels: allowUnlistedModels(providerType, pc),
		Capabilities:        catalogCapabilities(providerType),
	}
	if err := nonInteractiveAuthReady(entry.Type); err != nil || !providerCredentialReady(cfg, name, providerType) {
		return protocol.CatalogEntry{}, false
	}
	ids := append([]string(nil), pc.Models...)
	if pc.Model != "" {
		ids = append(ids, pc.Model)
	}
	seen := make(map[string]bool)
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		inputPrice, outputPrice := -1.0, -1.0
		if in, out, known := llm.PricingForProviderModel(name, id); known {
			inputPrice, outputPrice = in, out
		}
		entry.Models = append(entry.Models, protocol.Model{
			ID: id, InputLimit: llm.InputLimitForProviderModel(name, id), OutputLimit: llm.OutputLimitForModel(id),
			InputPrice: inputPrice, OutputPrice: outputPrice, ReasoningEfforts: llm.ReasoningEffortsForProviderModel(name, id),
		})
	}
	return entry, len(entry.Models) > 0
}

func (s *Server) refreshCatalogProvider(name string) <-chan singleflight.Result {
	return s.catalogRefresh.DoChan(name, func() (any, error) {
		if !s.providerConfigured(name) {
			return providerCatalogResult{}, nil
		}
		refreshCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ModelListTimeout)
		defer cancel()
		entry, err := buildCatalogEntry(refreshCtx, cloneConfigForCatalog(s.currentConfig()), s.cfg.ProviderFactory, name)
		if err != nil {
			slog.Error("refresh gateway provider catalog", "provider", name, "error", err)
			// A configured fallback entry remains safe to use even when live model
			// listing failed. Otherwise retain a last-known-good provider entry.
			if len(entry.Models) == 0 && !entry.AllowUnlistedModels {
				if stale, _, ok := s.cachedCatalogProvider(name); ok {
					return providerCatalogResult{entry: stale, found: true}, nil
				}
				return providerCatalogResult{}, err
			}
		}
		s.storeCatalogProvider(entry)
		return providerCatalogResult{entry: entry, found: true}, nil
	})
}

// currentCatalogProvider refreshes only the provider required for inference.
// Once an entry exists it is stale-first: the request uses it immediately while
// a bounded refresh proceeds in the background.
func (s *Server) currentCatalogProvider(ctx context.Context, name string) (protocol.CatalogEntry, bool, error) {
	if err := s.refreshConfigIfStale(); err != nil {
		slog.Error("reload gateway config", "error", err)
	}
	if !s.providerConfigured(name) {
		return protocol.CatalogEntry{}, false, nil
	}
	cached, fetchedAt, ok := s.cachedCatalogProvider(name)
	if ok && time.Since(fetchedAt) < s.cfg.CatalogTTL {
		return cached, true, nil
	}
	if !ok {
		if configured, valid := configuredCatalogEntry(s.currentConfig(), name); valid {
			cached, ok = configured, true
			// A zero fetch time makes this an immediately usable stale entry while
			// live metadata is refreshed asynchronously.
			s.storeCatalogProviderAt(configured, time.Time{})
		}
	}
	result := s.refreshCatalogProvider(name)
	if ok {
		// The channel must remain consumable so singleflight can complete, but this
		// request never waits for unrelated or stale model-list I/O.
		return cached, true, nil
	}
	select {
	case <-ctx.Done():
		return protocol.CatalogEntry{}, false, ctx.Err()
	case loaded := <-result:
		if loaded.Err != nil {
			return protocol.CatalogEntry{}, false, loaded.Err
		}
		value := loaded.Val.(providerCatalogResult)
		return value.entry, value.found, nil
	}
}

func (s *Server) currentCatalog(ctx context.Context) (protocol.Catalog, error) {
	if err := s.refreshConfigIfStale(); err != nil {
		slog.Error("reload gateway config", "error", err)
	}
	cfg := s.currentConfig()
	names := cfg.ExplicitProviderNames()
	type result struct {
		entry protocol.CatalogEntry
		found bool
		err   error
	}
	results := make(chan result, len(names))
	for _, name := range names {
		name := name
		go func() {
			entry, found, err := s.currentCatalogProvider(ctx, name)
			results <- result{entry: entry, found: found, err: err}
		}()
	}
	catalog := protocol.Catalog{Version: protocol.Version, GeneratedAt: time.Now().UTC(), Features: protocol.CatalogFeatures{Search: s.cfg.Searcher != nil, Fetch: s.cfg.FetchTool != nil}}
	var firstErr error
	for range names {
		result := <-results
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		if result.found {
			catalog.Providers = append(catalog.Providers, result.entry)
		}
	}
	sort.Slice(catalog.Providers, func(i, j int) bool { return catalog.Providers[i].Key < catalog.Providers[j].Key })
	if len(catalog.Providers) == 0 && len(names) > 0 && firstErr != nil {
		return protocol.Catalog{}, firstErr
	}
	return catalog, nil
}

func buildCatalog(ctx context.Context, cfg *config.Config, factory ProviderFactory, hasSearch, hasFetch bool) (protocol.Catalog, map[string]error) {
	catalog := protocol.Catalog{Version: protocol.Version, GeneratedAt: time.Now().UTC(), Features: protocol.CatalogFeatures{Search: hasSearch, Fetch: hasFetch}}
	failed := make(map[string]error)
	if cfg == nil {
		return catalog, failed
	}
	type result struct {
		entry protocol.CatalogEntry
		err   error
	}
	names := cfg.ExplicitProviderNames()
	results := make(chan result, len(names))
	var wg sync.WaitGroup
	for _, name := range names {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			providerConfig := cloneConfigForCatalog(cfg)
			entry, err := buildCatalogEntry(ctx, providerConfig, factory, name)
			results <- result{entry: entry, err: err}
		}()
	}
	wg.Wait()
	close(results)
	for result := range results {
		if result.err != nil {
			failed[result.entry.Key] = result.err
			if len(result.entry.Models) == 0 && !result.entry.AllowUnlistedModels {
				continue
			}
		}
		catalog.Providers = append(catalog.Providers, result.entry)
	}
	sort.Slice(catalog.Providers, func(i, j int) bool { return catalog.Providers[i].Key < catalog.Providers[j].Key })
	return catalog, failed
}

func cloneConfigForCatalog(cfg *config.Config) *config.Config {
	clone := *cfg
	clone.Providers = make(map[string]config.ProviderConfig, len(cfg.Providers))
	for name, provider := range cfg.Providers {
		clone.Providers[name] = provider
	}
	return &clone
}

func buildCatalogEntry(ctx context.Context, cfg *config.Config, factory ProviderFactory, key string) (protocol.CatalogEntry, error) {
	pc, ok := cfg.Providers[key]
	if !ok {
		return protocol.CatalogEntry{Key: key}, fmt.Errorf("provider configuration disappeared")
	}
	providerType := config.InferProviderType(key, pc.Type)
	entry := protocol.CatalogEntry{Key: key, Type: string(providerType), CLI: isCLIProvider(providerType), AllowUnlistedModels: allowUnlistedModels(providerType, pc)}
	if err := nonInteractiveAuthReady(entry.Type); err != nil {
		return entry, err
	}
	provider, err := factory(cfg, key, pc.Model)
	if err != nil {
		return entry, err
	}
	if !providerCredentialReady(cfg, key, providerType) {
		return entry, fmt.Errorf("provider credential is not configured")
	}
	entry.Capabilities = llm.CapabilitiesToGatewayProtocol(provider.Capabilities())
	if entry.Capabilities == (protocol.Capabilities{}) {
		entry.Capabilities = catalogCapabilities(providerType)
	}

	ids := append([]string(nil), pc.Models...)
	liveModels := []llm.ModelInfo(nil)
	var listErr error
	if lister, ok := provider.(interface {
		ListModels(context.Context) ([]llm.ModelInfo, error)
	}); ok {
		liveModels, err = lister.ListModels(ctx)
		if err != nil && !errors.Is(err, llm.ErrListModelsUnsupported) {
			status, code := classifyProviderError(err, entry.Type)
			if status == http.StatusUnauthorized || code == "provider_api_key_unauthenticated" || code == "provider_oauth_unauthenticated" {
				return entry, fmt.Errorf("list live models: %w", err)
			}
			listErr = fmt.Errorf("list live models; using fallback: %w", err)
			liveModels = nil
		}
	}
	if len(liveModels) > 0 {
		for _, model := range liveModels {
			if strings.TrimSpace(model.ID) == "" {
				continue
			}
			inputPrice, outputPrice := catalogLivePricing(key, providerType, model)
			entry.Models = append(entry.Models, protocol.Model{
				ID: model.ID, DisplayName: model.DisplayName, Created: model.Created, OwnedBy: model.OwnedBy,
				InputLimit: model.InputLimit, InputPrice: inputPrice, OutputPrice: outputPrice,
				ReasoningEfforts: model.ReasoningEfforts, DefaultReasoningEffort: model.DefaultReasoningEffort, ReasoningModes: model.ReasoningModes,
			})
		}
	} else {
		if len(ids) == 0 {
			ids = llm.ResolveProviderModelIDs(key)
		}
		if pc.Model != "" {
			ids = append(ids, pc.Model)
		}
		seen := make(map[string]bool)
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			inputPrice, outputPrice := -1.0, -1.0
			if in, out, known := llm.PricingForProviderModel(key, id); known {
				inputPrice, outputPrice = in, out
			}
			entry.Models = append(entry.Models, protocol.Model{ID: id, InputLimit: llm.InputLimitForProviderModel(key, id), OutputLimit: llm.OutputLimitForModel(id), InputPrice: inputPrice, OutputPrice: outputPrice, ReasoningEfforts: llm.ReasoningEffortsForProviderModel(key, id)})
		}
	}
	if len(entry.Models) == 0 && !entry.AllowUnlistedModels {
		if listErr != nil {
			return entry, listErr
		}
		return entry, fmt.Errorf("provider has no discoverable or configured models")
	}
	return entry, listErr
}

func catalogLivePricing(provider string, providerType config.ProviderType, model llm.ModelInfo) (float64, float64) {
	if model.InputPrice != 0 || model.OutputPrice != 0 {
		return model.InputPrice, model.OutputPrice
	}
	if input, output, known := llm.PricingForProviderModel(provider, model.ID); known {
		return input, output
	}
	// These catalog APIs explicitly report free models as zero prices. For APIs
	// that do not return pricing, zero is ambiguous and must not be advertised as
	// free to satellites.
	switch providerType {
	case config.ProviderTypeOpenRouter, config.ProviderTypeZen, config.ProviderTypeVenice,
		config.ProviderTypeNearAI, config.ProviderTypeSambaNova:
		return 0, 0
	default:
		return -1, -1
	}
}

func allowUnlistedModels(providerType config.ProviderType, pc config.ProviderConfig) bool {
	if pc.AllowUnlistedModels != nil {
		return *pc.AllowUnlistedModels
	}
	switch providerType {
	case config.ProviderTypeOpenRouter, config.ProviderTypeOpenAICompat, config.ProviderTypeVLLM,
		config.ProviderTypeOllama, config.ProviderTypeVenice, config.ProviderTypeNearAI,
		config.ProviderTypeSambaNova, config.ProviderTypeZen:
		return true
	default:
		return false
	}
}

func catalogCapabilities(providerType config.ProviderType) protocol.Capabilities {
	caps := protocol.Capabilities{ToolCalls: true}
	switch providerType {
	case config.ProviderTypeAnthropic, config.ProviderTypeBedrock:
		caps.NativeWebSearch = true
		caps.NativeWebFetch = true
		caps.SupportsToolChoice = true
	case config.ProviderTypeOpenAI, config.ProviderTypeOpenRouter, config.ProviderTypeGemini,
		config.ProviderTypeXAI, config.ProviderTypeOpenAICompat, config.ProviderTypeVLLM,
		config.ProviderTypeZen, config.ProviderTypeVenice, config.ProviderTypeNearAI,
		config.ProviderTypeSambaNova:
		caps.SupportsToolChoice = true
		if providerType == config.ProviderTypeOpenAI || providerType == config.ProviderTypeGemini || providerType == config.ProviderTypeXAI {
			caps.NativeWebSearch = true
		}
	case config.ProviderTypeChatGPT:
		caps.NativeWebSearch = true
	case config.ProviderTypeClaudeBin:
		caps.ManagesOwnContext = true
	case config.ProviderTypeGrokBin:
		caps.NativeWebSearch = true
		caps.NativeWebFetch = true
		caps.ManagesOwnContext = true
		caps.InlineToolLoop = true
	case config.ProviderTypeCursorBin:
		caps.ManagesOwnContext = true
		caps.InlineToolLoop = true
		caps.OrderedInlineToolEvents = true
	case config.ProviderTypeGeminiCLI:
		caps.NativeWebSearch = true
	}
	return caps
}

func nonInteractiveAuthReady(providerType string) error {
	var binary string
	switch config.ProviderType(providerType) {
	case config.ProviderTypeChatGPT:
		creds, err := credentials.GetChatGPTCredentials()
		if err != nil || creds == nil || creds.IsExpired() {
			return fmt.Errorf("provider is not authenticated on the gateway; run `term-llm auth login chatgpt` on the gateway host")
		}
	case config.ProviderTypeCopilot:
		creds, err := credentials.GetCopilotCredentials()
		if err != nil || creds == nil || creds.IsExpired() {
			return fmt.Errorf("provider is not authenticated on the gateway; run `term-llm auth login copilot` on the gateway host")
		}
	case config.ProviderTypeGeminiCLI:
		binary = "gemini"
		creds, err := credentials.GetGeminiOAuthCredentials()
		if err != nil || creds == nil {
			return fmt.Errorf("provider is not authenticated on the gateway; run Gemini CLI login on the gateway host")
		}
	case config.ProviderTypeClaudeBin:
		binary = "claude"
	case config.ProviderTypeGrokBin:
		binary = "grok"
	case config.ProviderTypeCursorBin:
		binary = "cursor-agent"
		if !llm.CursorBinHasCredentials() {
			return fmt.Errorf("provider is not authenticated on the gateway; run `cursor-agent login` on the gateway host")
		}
	}
	if binary != "" {
		if _, err := exec.LookPath(binary); err != nil {
			return fmt.Errorf("provider executable %q is not available on the gateway host", binary)
		}
	}
	return nil
}

func providerCredentialReady(cfg *config.Config, key string, providerType config.ProviderType) bool {
	pc := cfg.GetProviderConfig(key)
	if pc == nil {
		return false
	}
	switch providerType {
	case config.ProviderTypeOpenAI, config.ProviderTypeGemini, config.ProviderTypeOpenRouter,
		config.ProviderTypeXAI, config.ProviderTypeVenice, config.ProviderTypeNearAI,
		config.ProviderTypeSambaNova:
		return strings.TrimSpace(pc.ResolvedAPIKey) != "" || strings.TrimSpace(pc.APIKey) != ""
	default:
		return true
	}
}

func isCLIProvider(providerType config.ProviderType) bool {
	switch providerType {
	case config.ProviderTypeClaudeBin, config.ProviderTypeGrokBin, config.ProviderTypeCursorBin, config.ProviderTypeGeminiCLI:
		return true
	default:
		return false
	}
}

func filterCatalog(catalog protocol.Catalog, serverPolicy, clientPolicy Policy) protocol.Catalog {
	out := catalog
	out.Providers = nil
	for _, entry := range catalog.Providers {
		if !serverPolicy.AllowsProvider(entry.Key, entry.CLI) || !clientPolicy.AllowsProvider(entry.Key, entry.CLI) {
			continue
		}
		copyEntry := entry
		copyEntry.Models = nil
		for _, model := range entry.Models {
			if serverPolicy.Allows(entry.Key, model.ID, entry.CLI) && clientPolicy.Allows(entry.Key, model.ID, entry.CLI) {
				copyEntry.Models = append(copyEntry.Models, model)
			}
		}
		if len(copyEntry.Models) > 0 || entry.AllowUnlistedModels {
			out.Providers = append(out.Providers, copyEntry)
		}
	}
	out.Features.Search = out.Features.Search && serverPolicy.AllowSearch && clientPolicy.AllowSearch
	out.Features.Fetch = out.Features.Fetch && serverPolicy.AllowFetch && clientPolicy.AllowFetch
	return out
}

func catalogEntryAllowsModel(entry protocol.CatalogEntry, provider, model string) bool {
	if model == "" {
		return false
	}
	base, _ := llm.BaseModelAndEffortForProvider(provider, model)
	for _, candidate := range entry.Models {
		if candidate.ID == model || candidate.ID == base {
			return true
		}
	}
	return entry.AllowUnlistedModels
}

func catalogProvider(catalog protocol.Catalog, key string) (protocol.CatalogEntry, bool) {
	for _, entry := range catalog.Providers {
		if entry.Key == key {
			return entry, true
		}
	}
	return protocol.CatalogEntry{}, false
}

func (s *Server) limitsFor(client Client) *clientLimits {
	s.limitsMu.Lock()
	defer s.limitsMu.Unlock()
	if existing := s.limits[client.ID]; existing != nil {
		return existing
	}
	policy := client.Policy
	searchRate := policy.SearchRatePerMinute
	if searchRate <= 0 {
		searchRate = DefaultSearchRatePerMinute
	}
	searchBurst := policy.SearchBurst
	if searchBurst <= 0 {
		searchBurst = DefaultSearchBurst
	}
	searchConcurrency := policy.MaxConcurrentSearch
	if searchConcurrency <= 0 {
		searchConcurrency = DefaultMaxConcurrentSearch
	}
	fetchRate := policy.FetchRatePerMinute
	if fetchRate <= 0 {
		fetchRate = DefaultFetchRatePerMinute
	}
	fetchBurst := policy.FetchBurst
	if fetchBurst <= 0 {
		fetchBurst = DefaultFetchBurst
	}
	fetchConcurrency := policy.MaxConcurrentFetch
	if fetchConcurrency <= 0 {
		fetchConcurrency = DefaultMaxConcurrentFetch
	}
	limits := &clientLimits{
		inference: make(chan struct{}, policy.InferenceConcurrency()),
		search:    make(chan struct{}, searchConcurrency),
		fetch:     make(chan struct{}, fetchConcurrency),
		searchRPS: rate.NewLimiter(rate.Every(time.Minute/time.Duration(searchRate)), searchBurst),
		fetchRPS:  rate.NewLimiter(rate.Every(time.Minute/time.Duration(fetchRate)), fetchBurst),
	}
	s.limits[client.ID] = limits
	return limits
}

func (s *Server) acquireInference(client Client) (func(), bool) {
	sem := s.limitsFor(client).inference
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	default:
		return func() {}, false
	}
}

func (s *Server) acquireTool(client Client, searchRequest bool) (func(), string, bool) {
	limits := s.limitsFor(client)
	sem, limiter, prefix := limits.fetch, limits.fetchRPS, "fetch"
	if searchRequest {
		sem, limiter, prefix = limits.search, limits.searchRPS, "search"
	}
	if !limiter.Allow() {
		return func() {}, prefix + "_rate_limited", false
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, "", true
	default:
		return func() {}, prefix + "_concurrency_limited", false
	}
}

func classifyProviderError(err error, providerType string) (int, string) {
	if err == nil {
		return http.StatusBadGateway, "provider_upstream_failure"
	}
	if errors.Is(err, context.Canceled) {
		return 499, "canceled"
	}
	var statusErr *providerhttp.StatusError
	statusCode := 0
	if errors.As(err, &statusErr) {
		statusCode = statusErr.StatusCode
	} else if status, ok := err.(interface{ HTTPStatusCode() int }); ok {
		statusCode = status.HTTPStatusCode()
	}
	message := strings.ToLower(err.Error())
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		switch config.ProviderType(providerType) {
		case config.ProviderTypeChatGPT, config.ProviderTypeCopilot, config.ProviderTypeGeminiCLI:
			return http.StatusUnauthorized, "provider_oauth_unauthenticated"
		default:
			return http.StatusUnauthorized, "provider_api_key_unauthenticated"
		}
	}
	var rateLimit *llm.RateLimitError
	if statusCode == http.StatusTooManyRequests || errors.As(err, &rateLimit) || strings.Contains(message, "rate limit") {
		return http.StatusTooManyRequests, "provider_rate_limited"
	}
	oauthProvider := config.ProviderType(providerType) == config.ProviderTypeChatGPT || config.ProviderType(providerType) == config.ProviderTypeCopilot || config.ProviderType(providerType) == config.ProviderTypeGeminiCLI || config.ProviderType(providerType) == config.ProviderTypeCursorBin
	if oauthProvider && (strings.Contains(message, "oauth") || strings.Contains(message, "login") || strings.Contains(message, "not authenticated") || strings.Contains(message, "credential")) {
		return http.StatusUnauthorized, "provider_oauth_unauthenticated"
	}
	if strings.Contains(message, "api key") || strings.Contains(message, "api_key") || (strings.Contains(message, "credential") && (strings.Contains(message, "missing") || strings.Contains(message, "invalid") || strings.Contains(message, "required"))) {
		return http.StatusUnauthorized, "provider_api_key_unauthenticated"
	}
	for _, marker := range []string{"context length", "context_length", "too many tokens", "prompt is too long", "request too large", "maximum context"} {
		if strings.Contains(message, marker) {
			return http.StatusBadRequest, "provider_context_limit"
		}
	}
	modelTextError := strings.Contains(message, "model") && (strings.Contains(message, "not found") || strings.Contains(message, "invalid") || strings.Contains(message, "unknown") || strings.Contains(message, "unsupported") || strings.Contains(message, "does not exist"))
	if statusCode == http.StatusNotFound || modelTextError || ((statusCode == http.StatusBadRequest || statusCode == http.StatusUnprocessableEntity) && strings.Contains(message, "model")) {
		return http.StatusBadRequest, "provider_model_invalid"
	}
	if statusCode >= http.StatusInternalServerError || statusCode == http.StatusRequestTimeout || statusCode == http.StatusBadGateway || statusCode == http.StatusServiceUnavailable || statusCode == http.StatusGatewayTimeout {
		return http.StatusBadGateway, "provider_upstream_failure"
	}
	if statusCode >= http.StatusBadRequest {
		return http.StatusBadRequest, "provider_request_invalid"
	}
	return http.StatusBadGateway, "provider_upstream_failure"
}

func safeProviderErrorMessage(code, provider string) string {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "selected"
	}
	switch code {
	case "canceled":
		return "gateway request was canceled"
	case "provider_api_key_unauthenticated":
		return fmt.Sprintf("gateway provider %q rejected its API credential; update the API key on the gateway host", provider)
	case "provider_oauth_unauthenticated":
		return fmt.Sprintf("gateway provider %q needs OAuth authentication; run the provider login command on the gateway host", provider)
	case "provider_rate_limited":
		return fmt.Sprintf("gateway provider %q is rate limited; wait and retry or reduce concurrent requests", provider)
	case "provider_context_limit":
		return fmt.Sprintf("gateway provider %q rejected the request context; shorten or compact the conversation", provider)
	case "provider_model_invalid":
		return fmt.Sprintf("gateway provider %q rejected the model; choose a model from the gateway catalog", provider)
	case "provider_request_invalid":
		return fmt.Sprintf("gateway provider %q rejected the request; check model options and retry", provider)
	default:
		return fmt.Sprintf("gateway provider %q failed upstream; retry, then check gateway-side diagnostics if it persists", provider)
	}
}

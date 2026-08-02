package llm

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
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/gateway/protocol"
	"golang.org/x/sync/singleflight"
)

type gatewayCatalogCache struct {
	ETag      string           `json:"etag"`
	FetchedAt time.Time        `json:"fetched_at"`
	Catalog   protocol.Catalog `json:"catalog"`
}

var gatewayCatalogProcessCache = struct {
	sync.RWMutex
	entries map[string]gatewayCatalogCache
}{entries: make(map[string]gatewayCatalogCache)}

var gatewayCatalogRefresh singleflight.Group

// GatewayProvider is the satellite-side remote Provider. The satellite engine
// still owns prompts, sessions, tools, approvals, and the local filesystem.
type GatewayProvider struct {
	name       string
	gateway    config.GatewayConfig
	token      string
	baseURL    *url.URL
	streamHTTP *http.Client
	shortHTTP  *http.Client

	mu    sync.RWMutex
	state string
	entry protocol.CatalogEntry
}

func NewGatewayProvider(cfg *config.Config, name, _ string) (*GatewayProvider, error) {
	if cfg == nil || !cfg.Gateway.Enabled() {
		return nil, fmt.Errorf("gateway is not configured")
	}
	if err := cfg.Gateway.Validate(); err != nil {
		return nil, err
	}
	token, err := cfg.Gateway.ResolveToken()
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.Gateway.URL), "/"))
	if err != nil {
		return nil, fmt.Errorf("parse gateway URL: %w", err)
	}
	connectTimeout := gatewayDuration(cfg.Gateway.ConnectTimeout, config.DefaultGatewayConnectTimeout)
	responseTimeout := gatewayDuration(cfg.Gateway.ResponseTimeout, config.DefaultGatewayResponseTimeout)
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		ResponseHeaderTimeout: responseTimeout,
		IdleConnTimeout:       90 * time.Second,
	}
	p := &GatewayProvider{
		name: name, gateway: cfg.Gateway, token: token, baseURL: base,
		streamHTTP: &http.Client{Transport: transport},
		shortHTTP:  &http.Client{Transport: transport.Clone(), Timeout: responseTimeout},
	}
	catalogCtx, cancel := context.WithTimeout(context.Background(), connectTimeout+responseTimeout)
	defer cancel()
	catalog, err := p.loadCatalog(catalogCtx)
	if err != nil {
		return nil, err
	}
	entry, ok := catalogEntry(catalog, name)
	if !ok {
		return nil, fmt.Errorf("provider %q is not in gateway catalog", name)
	}
	p.entry = entry
	return p, nil
}

func gatewayDuration(value, fallback string) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err == nil && d > 0 {
		return d
	}
	d, _ = time.ParseDuration(fallback)
	return d
}

func (p *GatewayProvider) Name() string       { return p.name }
func (p *GatewayProvider) Credential() string { return "gateway" }
func (p *GatewayProvider) Capabilities() Capabilities {
	return capabilitiesFromProtocol(p.entry.Capabilities)
}

// GatewayHandlesRetries marks the provider as a single-attempt transport. The
// central provider owns retry policy, avoiding nested satellite retries.
func (p *GatewayProvider) GatewayHandlesRetries() bool { return true }

// UsageTrackedExternallyBy keeps satellite-local visibility while marking the
// gateway-attributed copy so aggregate local usage does not double-count it.
func (p *GatewayProvider) UsageTrackedExternallyBy() string { return "gateway" }

func (p *GatewayProvider) ExportProviderState() ([]byte, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.state == "" {
		return nil, false
	}
	return []byte(p.state), true
}

func (p *GatewayProvider) ImportProviderState(data []byte) error {
	state := strings.TrimSpace(string(data))
	if state == "" {
		return fmt.Errorf("gateway provider state is empty")
	}
	p.mu.Lock()
	p.state = state
	p.mu.Unlock()
	return nil
}

func (p *GatewayProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	catalog, err := p.loadCatalog(ctx)
	if err != nil {
		return nil, err
	}
	entry, ok := catalogEntry(catalog, p.name)
	if !ok {
		return nil, fmt.Errorf("provider %q is not in gateway catalog", p.name)
	}
	models := make([]ModelInfo, 0, len(entry.Models))
	for _, model := range entry.Models {
		models = append(models, ModelInfo{
			ID: model.ID, DisplayName: model.DisplayName, Created: model.Created,
			OwnedBy: model.OwnedBy, InputLimit: model.InputLimit,
			InputPrice: model.InputPrice, OutputPrice: model.OutputPrice,
			ReasoningEfforts:       model.ReasoningEfforts,
			DefaultReasoningEffort: model.DefaultReasoningEffort,
			ReasoningModes:         model.ReasoningModes,
		})
	}
	return models, nil
}

func (p *GatewayProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	wireRequest, err := EncodeGatewayRequest(req)
	if err != nil {
		return nil, fmt.Errorf("encode gateway request: %w", err)
	}
	requestID, err := newGatewayID("req")
	if err != nil {
		return nil, err
	}
	p.mu.RLock()
	state := p.state
	p.mu.RUnlock()
	stateSessionID := req.SessionID
	if strings.TrimSpace(stateSessionID) == "" {
		// Empty SessionID requests are deliberately stateless: never attach opaque
		// continuation state from another one-shot call.
		state = ""
		stateSessionID = ""
	}
	payload, err := json.Marshal(protocol.InferenceRequest{
		Version: protocol.Version, RequestID: requestID, Provider: p.name,
		State: state, Request: wireRequest,
	})
	if err != nil {
		return nil, fmt.Errorf("encode inference envelope: %w", err)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, p.endpoint("/g1/inference"), bytes.NewReader(payload))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create gateway request: %w", err)
	}
	p.setHeaders(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := p.streamHTTP.Do(httpReq)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("gateway inference unavailable; check gateway URL/network and retry: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		cancel()
		return nil, decodeGatewayHTTPError(resp)
	}
	stream := &gatewayProviderStream{
		provider: p, body: resp.Body, ctx: streamCtx, cancel: cancel, sessionID: stateSessionID,
		decoder:      newSSEDecoder(resp.Body, sseDecoderOptions{Transport: "gateway SSE"}),
		closedSignal: make(chan struct{}), results: make(chan nextGatewaySSE, 1),
	}
	go stream.readLoop()
	return stream, nil
}

func (p *GatewayProvider) endpoint(path string) string {
	base := *p.baseURL
	base.Path = strings.TrimRight(base.Path, "/") + path
	base.RawQuery = ""
	base.Fragment = ""
	return base.String()
}

func (p *GatewayProvider) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set(protocol.VersionHeader, "1")
}

type nextGatewaySSE struct {
	data []byte
	err  error
}

type gatewayProviderStream struct {
	provider     *GatewayProvider
	body         io.ReadCloser
	decoder      *sseDecoder
	ctx          context.Context
	cancel       context.CancelFunc
	sessionID    string
	results      chan nextGatewaySSE
	recvMu       sync.Mutex
	mu           sync.Mutex
	runID        string
	closed       bool
	closedSignal chan struct{}
	done         bool
}

func (s *gatewayProviderStream) readLoop() {
	defer close(s.results)
	for {
		_, data, err := s.decoder.Next()
		select {
		case s.results <- nextGatewaySSE{data: data, err: err}:
		case <-s.ctx.Done():
			return
		case <-s.closedSignal:
			return
		}
		if err != nil {
			return
		}
	}
}

func (s *gatewayProviderStream) Recv() (Event, error) {
	s.recvMu.Lock()
	defer s.recvMu.Unlock()
	if s.done {
		return Event{}, io.EOF
	}
	for {
		idle := gatewayDuration(s.provider.gateway.IdleTimeout, config.DefaultGatewayIdleTimeout)
		timer := time.NewTimer(idle)
		var result nextGatewaySSE
		var ok bool
		select {
		case result, ok = <-s.results:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			s.terminate()
			return Event{}, fmt.Errorf("gateway stream idle timeout after %s; check gateway/provider health and retry", idle)
		case <-s.ctx.Done():
			timer.Stop()
			if s.isClosed() {
				return Event{}, io.EOF
			}
			err := s.ctx.Err()
			s.terminate()
			return Event{}, fmt.Errorf("gateway stream ended: %w", err)
		case <-s.closedSignal:
			timer.Stop()
			return Event{}, io.EOF
		}
		if !ok {
			if s.isClosed() {
				return Event{}, io.EOF
			}
			s.terminate()
			return Event{}, &StreamIncompleteError{Transport: "gateway SSE", Terminal: "done"}
		}
		if result.err != nil {
			s.terminate()
			if errors.Is(result.err, io.EOF) {
				return Event{}, &StreamIncompleteError{Transport: "gateway SSE", Terminal: "done"}
			}
			return Event{}, result.err
		}
		var record protocol.StreamRecord
		dec := json.NewDecoder(bytes.NewReader(result.data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&record); err != nil {
			s.terminate()
			return Event{}, fmt.Errorf("decode gateway stream record: %w", err)
		}
		if record.Version != protocol.Version {
			s.terminate()
			return Event{}, fmt.Errorf("gateway protocol version %d is unsupported", record.Version)
		}
		s.mu.Lock()
		if record.RunID != "" {
			s.runID = record.RunID
		}
		s.mu.Unlock()
		switch record.Type {
		case "run":
			continue
		case "event":
			event, err := DecodeGatewayEvent(record.Event)
			if err != nil {
				s.terminate()
				return Event{}, err
			}
			return event, nil
		case "tool_callback":
			event, err := DecodeGatewayEvent(record.Event)
			if err != nil {
				s.terminate()
				return Event{}, err
			}
			response := make(chan ToolExecutionResponse, 1)
			event.ToolResponse = response
			go s.postToolResult(record.CallbackPath, response)
			return event, nil
		case "state":
			if s.sessionID == "" {
				s.terminate()
				return Event{}, fmt.Errorf("gateway returned provider state for a stateless request")
			}
			s.provider.mu.Lock()
			s.provider.state = record.State
			s.provider.mu.Unlock()
			continue
		case "done":
			s.done = true
			s.terminate()
			return Event{Type: EventDone}, nil
		case "error":
			s.terminate()
			if record.Error == nil {
				return Event{}, &GatewayError{Code: "gateway_failure", Message: "gateway request failed; retry or check gateway diagnostics"}
			}
			return Event{}, &GatewayError{Code: record.Error.Code, Message: record.Error.Message, RequestID: record.Error.RequestID}
		default:
			s.terminate()
			return Event{}, fmt.Errorf("unknown gateway stream record %q", record.Type)
		}
	}
}

func (s *gatewayProviderStream) postToolResult(callbackPath string, response <-chan ToolExecutionResponse) {
	var result ToolExecutionResponse
	var ok bool
	toolTimeout := gatewayDuration(s.provider.gateway.ToolTimeout, config.DefaultGatewayToolTimeout)
	timer := time.NewTimer(toolTimeout)
	defer timer.Stop()
	select {
	case result, ok = <-response:
		if !ok {
			result.Err = fmt.Errorf("satellite tool response channel closed")
		}
	case <-timer.C:
		result.Err = fmt.Errorf("satellite tool execution timed out after %s", toolTimeout)
	case <-s.ctx.Done():
		return
	case <-s.closedSignal:
		return
	}
	data, err := EncodeGatewayToolResponse(result)
	if err != nil {
		slog.Error("encode gateway tool callback", "error", err)
		return
	}
	payload, _ := json.Marshal(protocol.ToolResultRequest{Version: protocol.Version, Result: data})
	callbackURL, err := s.safeCallbackURL(callbackPath)
	if err != nil {
		slog.Error("validate gateway tool callback", "error", err)
		return
	}
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, callbackURL, bytes.NewReader(payload))
	if err != nil {
		slog.Error("create gateway tool callback", "error", err)
		return
	}
	s.provider.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.provider.shortHTTP.Do(req)
	if err != nil {
		slog.Error("post gateway tool callback", "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		slog.Error("gateway tool callback rejected", "status", resp.StatusCode)
	}
}

func (s *gatewayProviderStream) safeCallbackURL(path string) (string, error) {
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || !strings.HasPrefix(u.Path, "/g1/runs/") {
		return "", fmt.Errorf("invalid gateway callback path")
	}
	return s.provider.endpoint(u.Path), nil
}

func (s *gatewayProviderStream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *gatewayProviderStream) terminate() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.closedSignal)
	s.mu.Unlock()
	s.cancel()
	_ = s.body.Close()
}

func (s *gatewayProviderStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	runID := s.runID
	s.mu.Unlock()
	s.terminate()
	if runID != "" {
		timeout := min(gatewayDuration(s.provider.gateway.ResponseTimeout, config.DefaultGatewayResponseTimeout), 500*time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.provider.endpoint("/g1/runs/"+url.PathEscape(runID)), nil)
		if err == nil {
			s.provider.setHeaders(req)
			if resp, doErr := s.provider.shortHTTP.Do(req); doErr == nil {
				resp.Body.Close()
			}
		}
	}
	return nil
}

func (p *GatewayProvider) loadCatalog(ctx context.Context) (protocol.Catalog, error) {
	identity := gatewayCatalogIdentity(p.baseURL.String(), p.token)
	cachePath := gatewayCatalogCachePath(p.baseURL.String(), p.token)
	cached, cacheErr := gatewayCatalogCached(identity, cachePath)
	ttl := gatewayDuration(p.gateway.CatalogTTL, config.DefaultGatewayCatalogTTL)
	if cacheErr == nil && time.Since(cached.FetchedAt) < ttl {
		return cached.Catalog, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	result := gatewayCatalogRefresh.DoChan(identity, func() (any, error) {
		latest, latestErr := gatewayCatalogCached(identity, cachePath)
		if latestErr == nil && time.Since(latest.FetchedAt) < ttl {
			return latest.Catalog, nil
		}
		if latestErr != nil && cacheErr == nil {
			latest, latestErr = cached, nil
		}
		bound := gatewayDuration(p.gateway.ConnectTimeout, config.DefaultGatewayConnectTimeout) + gatewayDuration(p.gateway.ResponseTimeout, config.DefaultGatewayResponseTimeout)
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bound)
		defer cancel()
		return p.fetchCatalog(refreshCtx, identity, cachePath, latest, latestErr == nil)
	})
	select {
	case <-ctx.Done():
		if cacheErr == nil {
			return cached.Catalog, nil
		}
		return protocol.Catalog{}, fmt.Errorf("gateway catalog unavailable; check gateway URL/network and retry: %w", ctx.Err())
	case loaded := <-result:
		if loaded.Err != nil {
			if cacheErr == nil {
				return cached.Catalog, nil
			}
			return protocol.Catalog{}, loaded.Err
		}
		return loaded.Val.(protocol.Catalog), nil
	}
}

func (p *GatewayProvider) fetchCatalog(ctx context.Context, identity, cachePath string, cached gatewayCatalogCache, hasCached bool) (protocol.Catalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint("/g1/catalog"), nil)
	if err != nil {
		return protocol.Catalog{}, err
	}
	p.setHeaders(req)
	if hasCached && cached.ETag != "" {
		req.Header.Set("If-None-Match", cached.ETag)
	}
	resp, err := p.shortHTTP.Do(req)
	if err != nil {
		if hasCached {
			return cached.Catalog, nil
		}
		return protocol.Catalog{}, fmt.Errorf("fetch gateway catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && hasCached {
		cached.FetchedAt = time.Now().UTC()
		storeGatewayCatalogCache(identity, cachePath, cached)
		return cached.Catalog, nil
	}
	if resp.StatusCode != http.StatusOK {
		if hasCached {
			return cached.Catalog, nil
		}
		return protocol.Catalog{}, decodeGatewayHTTPError(resp)
	}
	var catalog protocol.Catalog
	dec := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&catalog); err != nil {
		if hasCached {
			return cached.Catalog, nil
		}
		return protocol.Catalog{}, fmt.Errorf("decode gateway catalog: %w", err)
	}
	if catalog.Version != protocol.Version {
		return protocol.Catalog{}, fmt.Errorf("gateway catalog protocol version %d is unsupported", catalog.Version)
	}
	storeGatewayCatalogCache(identity, cachePath, gatewayCatalogCache{ETag: resp.Header.Get("ETag"), FetchedAt: time.Now().UTC(), Catalog: catalog})
	return catalog, nil
}

func (p *GatewayProvider) loadCatalogCacheOnly() (protocol.Catalog, error) {
	identity := gatewayCatalogIdentity(p.baseURL.String(), p.token)
	cached, err := gatewayCatalogCached(identity, gatewayCatalogCachePath(p.baseURL.String(), p.token))
	if err != nil {
		return protocol.Catalog{}, fmt.Errorf("gateway catalog cache is empty; run a normal command while the gateway is available")
	}
	return cached.Catalog, nil
}

func gatewayCatalogCached(identity, path string) (gatewayCatalogCache, error) {
	gatewayCatalogProcessCache.RLock()
	cached, ok := gatewayCatalogProcessCache.entries[identity]
	gatewayCatalogProcessCache.RUnlock()
	if ok {
		return cached, nil
	}
	cached, err := readGatewayCatalogCache(path)
	if err != nil {
		return gatewayCatalogCache{}, err
	}
	gatewayCatalogProcessCache.Lock()
	gatewayCatalogProcessCache.entries[identity] = cached
	gatewayCatalogProcessCache.Unlock()
	return cached, nil
}

func storeGatewayCatalogCache(identity, path string, cached gatewayCatalogCache) {
	gatewayCatalogProcessCache.Lock()
	gatewayCatalogProcessCache.entries[identity] = cached
	gatewayCatalogProcessCache.Unlock()
	_ = writeGatewayCatalogCache(path, cached)
}

func GatewayCatalogHasProvider(ctx context.Context, cfg *config.Config, name string) (bool, error) {
	p, err := NewGatewayProviderForCatalog(cfg)
	if err != nil {
		return false, err
	}
	catalog, err := p.loadCatalog(ctx)
	if err != nil {
		return false, err
	}
	_, ok := catalogEntry(catalog, name)
	return ok, nil
}

func NewGatewayProviderForCatalog(cfg *config.Config) (*GatewayProvider, error) {
	if cfg == nil || !cfg.Gateway.Enabled() {
		return nil, fmt.Errorf("gateway is not configured")
	}
	token, err := cfg.Gateway.ResolveToken()
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.Gateway.URL), "/"))
	if err != nil {
		return nil, err
	}
	connectTimeout := gatewayDuration(cfg.Gateway.ConnectTimeout, config.DefaultGatewayConnectTimeout)
	responseTimeout := gatewayDuration(cfg.Gateway.ResponseTimeout, config.DefaultGatewayResponseTimeout)
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: connectTimeout}).DialContext, ForceAttemptHTTP2: true, ResponseHeaderTimeout: responseTimeout}
	return &GatewayProvider{gateway: cfg.Gateway, token: token, baseURL: base, streamHTTP: &http.Client{Transport: transport}, shortHTTP: &http.Client{Transport: transport.Clone(), Timeout: responseTimeout}}, nil
}

func LoadGatewayCatalogCacheOnly(cfg *config.Config) (protocol.Catalog, error) {
	provider, err := NewGatewayProviderForCatalog(cfg)
	if err != nil {
		return protocol.Catalog{}, err
	}
	return provider.loadCatalogCacheOnly()
}

func catalogEntry(catalog protocol.Catalog, name string) (protocol.CatalogEntry, bool) {
	for _, entry := range catalog.Providers {
		if entry.Key == name {
			return entry, true
		}
	}
	return protocol.CatalogEntry{}, false
}

func gatewayCatalogIdentity(rawURL, token string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(strings.TrimSpace(rawURL), "/") + "\x00" + token))
	return hex.EncodeToString(sum[:])
}

func gatewayCatalogCachePath(rawURL, token string) string {
	root := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME"))
	if root == "" {
		if home, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(home, ".cache")
		} else {
			root = os.TempDir()
		}
	}
	identity := gatewayCatalogIdentity(rawURL, token)
	return filepath.Join(root, "term-llm", "gateway", identity[:16]+".json")
}

func readGatewayCatalogCache(path string) (gatewayCatalogCache, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return gatewayCatalogCache{}, err
	}
	var cache gatewayCatalogCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return gatewayCatalogCache{}, err
	}
	if cache.Catalog.Version != protocol.Version || cache.FetchedAt.IsZero() {
		return gatewayCatalogCache{}, fmt.Errorf("invalid gateway catalog cache")
	}
	return cache, nil
}

func writeGatewayCatalogCache(path string, cache gatewayCatalogCache) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func decodeGatewayHTTPError(resp *http.Response) error {
	var wire protocol.Error
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(data, &wire); err == nil && wire.Code != "" {
		return &GatewayError{Code: wire.Code, Message: wire.Message, RequestID: wire.RequestID}
	}
	return &GatewayError{Code: "http_error", Message: fmt.Sprintf("gateway returned HTTP %d; check gateway health and credentials", resp.StatusCode)}
}

func newGatewayID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate gateway request ID: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(raw[:]), nil
}

var _ Provider = (*GatewayProvider)(nil)
var _ ProviderStateExporter = (*GatewayProvider)(nil)
var _ ProviderStateImporter = (*GatewayProvider)(nil)

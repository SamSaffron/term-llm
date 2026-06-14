package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Status is the probed health of one node, safe to send to hub clients (it
// never carries the node token).
type Status struct {
	// Reachable reports whether the node answered its health endpoint.
	Reachable bool `json:"reachable"`
	// State is "ok" when reachable, otherwise a short failure label
	// ("unreachable", "error <code>").
	State string `json:"state"`
	// LatencyMS is the health round-trip in milliseconds (reachable only).
	LatencyMS int64 `json:"latency_ms"`
	// Version/Agent/Capabilities are best-effort, reported by nodes whose
	// healthz includes them (newer term-llm serves, when the probe carries a
	// valid bearer token).
	Version      string   `json:"version,omitempty"`
	Agent        string   `json:"agent,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// Error holds the failure detail for unreachable nodes.
	Error string `json:"error,omitempty"`
}

// Prober checks node health over HTTP. The client must not use an
// environment proxy: probes carry node bearer tokens, and routing them
// through an HTTP_PROXY would leak the tokens (callers pass the same
// direct-dial transport the hub proxy uses).
type Prober struct {
	Client *http.Client
}

// NewProber returns a prober over the given transport with a bounded
// per-probe timeout so one hung node cannot stall a dashboard refresh.
func NewProber(transport http.RoundTripper) *Prober {
	return &Prober{Client: &http.Client{Transport: transport, Timeout: 3 * time.Second}}
}

// healthzResponse mirrors the term-llm serve healthz payload. The extended
// fields are only present when the serve trusts the request (see
// cmd/serve_handlers.go handleHealth).
type healthzResponse struct {
	Status       string   `json:"status"`
	Version      string   `json:"version"`
	Agent        string   `json:"agent"`
	Capabilities []string `json:"capabilities"`
}

// Probe checks one node's {base}/healthz, measuring latency and decoding any
// extended identity fields the node reports.
func (p *Prober) Probe(ctx context.Context, n Node) Status {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.BaseURL()+"/healthz", nil)
	if err != nil {
		return Status{State: "unreachable", Error: err.Error()}
	}
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	start := time.Now()
	resp, err := p.Client.Do(req)
	if err != nil {
		return Status{State: "unreachable", Error: err.Error()}
	}
	defer resp.Body.Close()
	latency := time.Since(start).Milliseconds()
	if resp.StatusCode != http.StatusOK {
		return Status{
			State:     fmt.Sprintf("error %d", resp.StatusCode),
			LatencyMS: latency,
			Error:     fmt.Sprintf("healthz returned %s", resp.Status),
		}
	}
	st := Status{Reachable: true, State: "ok", LatencyMS: latency}
	var body healthzResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
		st.Version = body.Version
		st.Agent = body.Agent
		st.Capabilities = body.Capabilities
	}
	return st
}

// ProbeAll probes nodes concurrently and returns statuses keyed by node ID.
func (p *Prober) ProbeAll(ctx context.Context, nodes []Node) map[string]Status {
	statuses := make(map[string]Status, len(nodes))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, n := range nodes {
		wg.Add(1)
		go func(n Node) {
			defer wg.Done()
			st := p.Probe(ctx, n)
			mu.Lock()
			statuses[n.ID] = st
			mu.Unlock()
		}(n)
	}
	wg.Wait()
	return statuses
}

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
)

type hubNodeDiagnostic struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

type hubNodeSessionView struct {
	ID            string `json:"id"`
	ShortTitle    string `json:"short_title"`
	LongTitle     string `json:"long_title,omitempty"`
	ActiveRun     bool   `json:"active_run,omitempty"`
	LastMessageAt int64  `json:"last_message_at,omitempty"`
	MessageCount  int    `json:"message_count,omitempty"`
	ResumePath    string `json:"resume_path"`
}

type hubNodeSessionsView struct {
	CountLabel  string               `json:"count_label"`
	HasMore     bool                 `json:"has_more,omitempty"`
	ActiveCount int                  `json:"active_count,omitempty"`
	Active      []hubNodeSessionView `json:"active,omitempty"`
	Recent      []hubNodeSessionView `json:"recent,omitempty"`
	ResumePath  string               `json:"resume_path,omitempty"`
}

// hubNodeView is the public record for one node. It deliberately omits the
// bearer token: tokens are injected server-side and must never be sent to a
// hub client.
type hubNodeView struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Source         string `json:"source"`
	Connection     string `json:"connection"`
	URL            string `json:"url"`
	BasePath       string `json:"base_path"`
	ProxyPath      string `json:"proxy_path"`
	NewSessionPath string `json:"new_session_path"`
	// HasToken reports whether the hub holds a bearer token for this node
	// (without it, a token-guarded node will answer 401 through the proxy).
	HasToken    bool                 `json:"has_token"`
	Status      hub.Status           `json:"status"`
	Sessions    *hubNodeSessionsView `json:"sessions,omitempty"`
	Diagnostics []hubNodeDiagnostic  `json:"diagnostics,omitempty"`
}

func (s *hubServer) handleHubHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "role": "hub"})
}

// collectNodes resolves all nodes and probes them concurrently. Resolver
// errors are soft: surviving sources still render, the error is reported
// alongside.
func (s *hubServer) collectNodes(ctx context.Context) ([]hubNodeView, error) {
	nodes, err := s.registry.Nodes()
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	statuses := s.prober.ProbeAll(probeCtx, nodes)
	views := make([]hubNodeView, 0, len(nodes))
	for _, n := range nodes {
		proxyPath := s.hubPath("/node/" + n.ID + "/")
		view := hubNodeView{
			ID:             n.ID,
			Name:           n.Name,
			Source:         n.Source,
			Connection:     n.Connection,
			URL:            n.URL,
			BasePath:       n.BasePath,
			ProxyPath:      proxyPath,
			NewSessionPath: proxyPath + "?new=1",
			HasToken:       n.Token != "",
			Status:         statuses[n.ID],
		}
		if n.UsesReverseConnection() {
			connected, connectedAt, lastSeen := s.reverse.status(n.ID)
			if connected {
				view.Status = s.probeReverseNode(probeCtx, n, connectedAt, lastSeen)
			} else {
				view.Status = hub.Status{State: "disconnected", Error: "waiting for reverse connection", Details: map[string]string{"connection": "reverse"}}
			}
		}
		views = append(views, view)
	}
	s.collectNodeSessionViews(probeCtx, nodes, views)
	for i := range views {
		views[i].Diagnostics = hubNodeDiagnostics(nodes[i], views[i], nodes)
	}
	return views, err
}

const (
	hubNodeSessionActiveLimit = 2
	hubNodeSessionRecentLimit = 3
	hubNodeSessionCountCap    = 100
	hubNodeSessionsMaxBytes   = 1 << 20
)

type hubNodeSessionStatus struct {
	ID            string `json:"id"`
	ShortTitle    string `json:"short_title"`
	LongTitle     string `json:"long_title"`
	ActiveRun     bool   `json:"active_run"`
	LastMessageAt int64  `json:"last_message_at"`
	MessageCount  int    `json:"message_count"`
}

// collectNodeSessionViews enriches reachable nodes with a bounded session
// summary. The node endpoint already caps itself at 100 rows, so the Hub can
// show "100+ sessions" without issuing exact count queries on every refresh.
func (s *hubServer) collectNodeSessionViews(ctx context.Context, nodes []hub.Node, views []hubNodeView) {
	if len(nodes) == 0 || len(nodes) != len(views) {
		return
	}
	sessionCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	type result struct {
		index    int
		sessions *hubNodeSessionsView
	}
	results := make(chan result, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		if !views[i].Status.Reachable {
			continue
		}
		wg.Add(1)
		go func(i int, n hub.Node) {
			defer wg.Done()
			sessions, err := s.fetchHubNodeSessions(sessionCtx, n)
			if err != nil || sessions == nil {
				return
			}
			results <- result{index: i, sessions: sessions}
		}(i, n)
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	for res := range results {
		views[res.index].Sessions = res.sessions
	}
}

func (s *hubServer) fetchHubNodeSessions(ctx context.Context, n hub.Node) (*hubNodeSessionsView, error) {
	req, err := s.newHubNodeSessionsRequest(ctx, n)
	if err != nil {
		return nil, err
	}
	resp, err := s.doHubNodeRequest(ctx, n, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusServiceUnavailable {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sessions status returned %s", resp.Status)
	}
	var body struct {
		Sessions []hubNodeSessionStatus `json:"sessions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, hubNodeSessionsMaxBytes)).Decode(&body); err != nil {
		return nil, err
	}
	if body.Sessions == nil {
		return nil, nil
	}
	return s.buildHubNodeSessionsView(n, body.Sessions), nil
}

func (s *hubServer) newHubNodeSessionsRequest(ctx context.Context, n hub.Node) (*http.Request, error) {
	if n.UsesReverseConnection() {
		u := &url.URL{
			Scheme: "http",
			Host:   "reverse.local",
			Path:   hubJoinBasePath(n.BasePath, "/v1/sessions/status"),
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		if n.Token != "" {
			req.Header.Set("Authorization", "Bearer "+n.Token)
		}
		return req, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.BaseURL()+"/v1/sessions/status", nil)
	if err != nil {
		return nil, err
	}
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	return req, nil
}

func (s *hubServer) doHubNodeRequest(ctx context.Context, n hub.Node, req *http.Request) (*http.Response, error) {
	if n.UsesReverseConnection() {
		return s.reverse.do(ctx, n, req)
	}
	return s.prober.Client.Do(req)
}

func (s *hubServer) buildHubNodeSessionsView(n hub.Node, entries []hubNodeSessionStatus) *hubNodeSessionsView {
	out := &hubNodeSessionsView{
		CountLabel: hubNodeSessionCountLabel(len(entries)),
		HasMore:    len(entries) >= hubNodeSessionCountCap,
	}
	for _, entry := range entries {
		if strings.TrimSpace(entry.ID) == "" {
			continue
		}
		sess := hubNodeSessionView{
			ID:            entry.ID,
			ShortTitle:    hubNodeSessionTitle(entry),
			LongTitle:     strings.TrimSpace(entry.LongTitle),
			ActiveRun:     entry.ActiveRun,
			LastMessageAt: entry.LastMessageAt,
			MessageCount:  entry.MessageCount,
			ResumePath:    s.hubPath("/node/" + n.ID + "/" + url.PathEscape(entry.ID)),
		}
		if sess.ActiveRun {
			out.ActiveCount++
			if len(out.Active) < hubNodeSessionActiveLimit {
				out.Active = append(out.Active, sess)
			}
		} else if len(out.Recent) < hubNodeSessionRecentLimit {
			out.Recent = append(out.Recent, sess)
		}
	}
	if len(out.Active) > 0 {
		out.ResumePath = out.Active[0].ResumePath
	} else if len(out.Recent) > 0 {
		out.ResumePath = out.Recent[0].ResumePath
	}
	return out
}

func hubNodeSessionTitle(entry hubNodeSessionStatus) string {
	for _, candidate := range []string{entry.ShortTitle, entry.LongTitle, entry.ID} {
		if title := strings.TrimSpace(candidate); title != "" {
			return title
		}
	}
	return "Untitled session"
}

func hubNodeSessionCountLabel(n int) string {
	if n >= hubNodeSessionCountCap {
		return "100+ sessions"
	}
	if n == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", n)
}

func (s *hubServer) probeReverseNode(ctx context.Context, n hub.Node, connectedAt, lastSeen time.Time) hub.Status {
	details := map[string]string{
		"connection":   "reverse",
		"connected_at": connectedAt.Format(time.RFC3339),
		"last_seen":    lastSeen.Format(time.RFC3339),
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://reverse.local"+hubJoinBasePath(n.BasePath, "/healthz"), nil)
	if err != nil {
		return hub.Status{Reachable: true, State: "connected", Details: details, Error: err.Error()}
	}
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	start := time.Now()
	resp, err := s.reverse.do(ctx, n, req)
	if err != nil {
		return hub.Status{Reachable: true, State: "connected", LatencyMS: time.Since(start).Milliseconds(), Details: details, Error: err.Error()}
	}
	defer resp.Body.Close()
	st := hub.Status{Reachable: true, State: "connected", LatencyMS: time.Since(start).Milliseconds(), Details: details}
	if resp.StatusCode != http.StatusOK {
		st.Error = fmt.Sprintf("healthz returned %s", resp.Status)
		return st
	}
	var body struct {
		Version      string   `json:"version"`
		Agent        string   `json:"agent"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
		st.Version = body.Version
		st.Agent = body.Agent
		st.Capabilities = body.Capabilities
	}
	return st
}

func hubNodeDiagnostics(n hub.Node, view hubNodeView, all []hub.Node) []hubNodeDiagnostic {
	diagnostics := []hubNodeDiagnostic{}
	add := func(severity, code, message string) {
		diagnostics = append(diagnostics, hubNodeDiagnostic{Severity: severity, Code: code, Message: message})
	}
	if n.UsesReverseConnection() && !view.Status.Reachable {
		add("error", "reverse_disconnected", "Reverse node is waiting for its outbound websocket connection.")
	}
	if n.Token == "" {
		add("warning", "missing_token", "No bearer token is configured; authenticated health, proxy, and delegation calls may fail.")
	}
	if n.Delegation != nil && n.Delegation.Enabled {
		if strings.TrimSpace(n.Delegation.Workdir) == "" {
			add("warning", "delegation_missing_workdir", "Delegation is enabled, but no workdir is configured; this node cannot accept delegated jobs.")
		} else if !hubStatusHasCapability(view.Status, "jobs") {
			if view.Status.Reachable && len(view.Status.Capabilities) > 0 {
				add("warning", "delegation_jobs_missing", "This node accepts delegation, but its health capabilities do not include jobs; start it with jobs enabled.")
			} else {
				add("warning", "delegation_jobs_unknown", "This node accepts delegation, but the Hub cannot verify the jobs capability; check the token and node version.")
			}
		}
	}
	for _, msg := range hubPolicyMismatchDiagnostics(n, all) {
		add("warning", "delegation_policy_mismatch", msg)
		if len(diagnostics) >= 8 {
			break
		}
	}
	return diagnostics
}

func hubStatusHasCapability(st hub.Status, capability string) bool {
	for _, c := range st.Capabilities {
		if strings.EqualFold(strings.TrimSpace(c), capability) {
			return true
		}
	}
	return false
}

func hubPolicyMismatchDiagnostics(n hub.Node, all []hub.Node) []string {
	if n.Delegation == nil || !n.Delegation.Enabled {
		return nil
	}
	messages := []string{}
	if len(n.Delegation.To) > 0 {
		for _, target := range all {
			if target.ID == n.ID || !n.CanDelegateTo(target.ID) {
				continue
			}
			if target.Delegation == nil || !target.Delegation.Enabled {
				messages = append(messages, fmt.Sprintf("Can delegate to %q, but that node has delegation disabled.", target.ID))
				continue
			}
			if strings.TrimSpace(target.Delegation.Workdir) == "" {
				messages = append(messages, fmt.Sprintf("Can delegate to %q, but that node has no delegation workdir.", target.ID))
				continue
			}
			acceptFrom := target.Delegation.AcceptFrom
			if len(acceptFrom) == 0 {
				acceptFrom = []string{"*"}
			}
			if !hubNodeListMatches(acceptFrom, n.ID) {
				messages = append(messages, fmt.Sprintf("Can delegate to %q, but that node does not accept from %q.", target.ID, n.ID))
			}
		}
	}
	if len(n.Delegation.AcceptFrom) > 0 && !hubNodeListMatches(n.Delegation.AcceptFrom, "*") && strings.TrimSpace(n.Delegation.Workdir) != "" {
		for _, origin := range all {
			if origin.ID == n.ID || !hubNodeListMatches(n.Delegation.AcceptFrom, origin.ID) {
				continue
			}
			if !origin.CanDelegateTo(n.ID) {
				messages = append(messages, fmt.Sprintf("Accepts from %q, but that node is not configured to delegate here.", origin.ID))
			}
		}
	}
	return messages
}

func hubNodeListMatches(patterns []string, id string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "*" || p == id {
			return true
		}
	}
	return false
}

func (s *hubServer) handleNodes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		views, err := s.collectNodes(r.Context())
		resp := map[string]any{"nodes": views}
		if err != nil {
			resp["resolver_error"] = err.Error()
		}
		writeJSON(w, http.StatusOK, resp)
	case http.MethodPost:
		if !hubBrowserRequestAllowed(r, true) {
			http.Error(w, "forbidden cross-site hub request", http.StatusForbidden)
			return
		}
		s.handleAddNode(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// addNodeRequest is the Add Node form payload. Token is accepted here (over
// the loopback-only hub) and persisted to the 0600 local store; it is never
// echoed back.
type addNodeRequest struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	BasePath string `json:"base_path"`
	Token    string `json:"token"`
}

func (s *hubServer) handleAddNode(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "node persistence is disabled", http.StatusForbidden)
		return
	}
	var req addNodeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	node, err := s.store.Add(hub.Node{
		ID:       strings.TrimSpace(req.ID),
		Name:     strings.TrimSpace(req.Name),
		URL:      req.URL,
		BasePath: strings.TrimSpace(req.BasePath),
		Token:    strings.TrimSpace(req.Token),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Pre-existing config/contain nodes win registry precedence; warn the
	// caller when the stored node is shadowed by an identical ID elsewhere.
	if existing, ok := s.registry.Lookup(node.ID); ok && existing.Source != hub.SourceLocal {
		writeJSON(w, http.StatusCreated, map[string]any{
			"id":      node.ID,
			"warning": fmt.Sprintf("node id %q is shadowed by a %s node with the same id", node.ID, existing.Source),
		})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": node.ID})
}

// handleNodeItem handles DELETE /api/nodes/<id> (local-store nodes only).
func (s *hubServer) handleNodeItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/nodes/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !hubBrowserRequestAllowed(r, false) {
		http.Error(w, "forbidden cross-site hub request", http.StatusForbidden)
		return
	}
	if s.store == nil {
		http.Error(w, "node persistence is disabled", http.StatusForbidden)
		return
	}
	if err := s.store.Remove(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": id})
}

// handleTestNode probes an ad-hoc node spec (the Add Node form's Test
// connection button) without persisting anything. The supplied token is used
// for the probe only and never stored or echoed.
func (s *hubServer) handleTestNode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !hubBrowserRequestAllowed(r, true) {
		http.Error(w, "forbidden cross-site hub request", http.StatusForbidden)
		return
	}
	var req addNodeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	n := hub.Node{ID: "test", Name: "test", URL: req.URL, BasePath: strings.TrimSpace(req.BasePath), Token: strings.TrimSpace(req.Token)}
	if err := n.Normalize(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{"status": s.prober.Probe(ctx, n)})
}

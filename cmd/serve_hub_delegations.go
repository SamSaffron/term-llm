package cmd

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
)

// Cross-node delegation: a node asks the hub to run a prompt as a jobs-v2 LLM
// job on another node. The hub is the only party that holds node tokens, so
// nodes authenticate to the hub with their own serve token and the hub talks
// to the target node's jobs API with the target's token. Delegation records
// live in a 0600 JSON ledger and never contain tokens.
//
// Routes (see hubServer.handler):
//
//	POST /api/delegations             create + trigger (node auth)
//	GET  /api/delegations             list (node auth or same-origin GET)
//	GET  /api/delegations/{id}        status, refreshed from the target run
//	POST /api/delegations/{id}/cancel cancel target run (origin node only)

const (
	// hubNodeIDHeader carries the caller's claimed node id; the bearer token
	// must match that node's stored token.
	hubNodeIDHeader = apphub.NodeIDHeader

	hubDelegationDefaultTimeout = 3600
	hubDelegationMinTimeout     = 10
	hubDelegationDefaultAgent   = hub.DefaultDelegationAgent

	// In-flight caps: per-target comes from the node's delegation policy.
	hubDelegationHubMaxInFlight    = 32
	hubDelegationOriginMaxInFlight = 8
)

// authenticateNode resolves the caller's node identity from the claimed node
// id header plus bearer token. Failures are deliberately uniform so callers
// cannot probe which node ids exist; nodes without a stored token can never
// authenticate (an empty token is absence of a credential, not a credential).
func (s *hubServer) authenticateNode(r *http.Request) (hub.Node, error) {
	failed := errors.New("node authentication failed")
	id := strings.TrimSpace(r.Header.Get(hubNodeIDHeader))
	if id == "" {
		return hub.Node{}, fmt.Errorf("missing %s header", hubNodeIDHeader)
	}
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return hub.Node{}, errors.New("missing bearer token")
	}
	token := strings.TrimSpace(auth[len(prefix):])
	if token == "" {
		return hub.Node{}, errors.New("missing bearer token")
	}
	node, ok := s.registry.Lookup(id)
	if !ok || node.Token == "" {
		return hub.Node{}, failed
	}
	// Hash both sides so the comparison is constant-time regardless of token
	// lengths.
	want := sha256.Sum256([]byte(node.Token))
	got := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		return hub.Node{}, failed
	}
	return node, nil
}

// delegationReader gates read-only delegation endpoints and resolves who is
// reading: an authenticated node may read only delegations it originates or
// targets, while a same-origin browser GET (the hub operator's dashboard;
// hub v1 is loopback-only) sees everything. Records carry no tokens either
// way.
func (s *hubServer) delegationReader(r *http.Request) (requester hub.Node, viaNode, allowed bool) {
	if node, err := s.authenticateNode(r); err == nil {
		return node, true, true
	}
	if r.Header.Get(hubNodeIDHeader) != "" || r.Header.Get("Authorization") != "" {
		// The caller attempted node auth and failed; don't silently fall back.
		return hub.Node{}, false, false
	}
	return hub.Node{}, false, r.Method == http.MethodGet && hubBrowserRequestAllowed(r, false)
}

// delegationVisibleTo reports whether a node may see a delegation record: it
// must be a party to it.
func delegationVisibleTo(d hub.Delegation, nodeID string) bool {
	return d.OriginNode == nodeID || d.TargetNode == nodeID
}

type hubDelegationCreateRequest struct {
	TargetNode     string `json:"target_node"`
	Prompt         string `json:"prompt"`
	AgentName      string `json:"agent_name"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Model          string `json:"model"`
	Cwd            string `json:"cwd"`
	// ParentDelegationID links a chained delegation (the origin is itself
	// working on a delegated job) for depth/loop enforcement. Verified
	// against the ledger.
	ParentDelegationID string `json:"parent_delegation_id"`
}

func (s *hubServer) handleDelegations(w http.ResponseWriter, r *http.Request) {
	if s.delegations == nil {
		http.Error(w, "delegation ledger is disabled", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		requester, viaNode, allowed := s.delegationReader(r)
		if !allowed {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		records, err := s.delegations.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if viaNode {
			scoped := make([]hub.Delegation, 0, len(records))
			for _, d := range records {
				if delegationVisibleTo(d, requester.ID) {
					scoped = append(scoped, d)
				}
			}
			records = scoped
		}
		if !viaNode {
			refreshed := make([]hub.Delegation, 0, len(records))
			for _, d := range records {
				if !hub.DelegationStatusTerminal(d.Status) {
					_ = s.refreshDelegation(r.Context(), &d)
				}
				refreshed = append(refreshed, d)
			}
			records = refreshed
		}
		writeJSON(w, http.StatusOK, map[string]any{"delegations": records})
	case http.MethodPost:
		origin, err := s.authenticateNode(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		s.handleCreateDelegation(w, r, origin)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *hubServer) handleDelegationItem(w http.ResponseWriter, r *http.Request) {
	if s.delegations == nil {
		http.Error(w, "delegation ledger is disabled", http.StatusServiceUnavailable)
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, "/api/delegations/")
	if id, ok := strings.CutSuffix(suffix, "/cancel"); ok && !strings.Contains(id, "/") {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleCancelDelegation(w, r, id)
		return
	}
	if suffix == "" || strings.Contains(suffix, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	requester, viaNode, allowed := s.delegationReader(r)
	if !allowed {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	d, ok, err := s.delegations.Get(suffix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// A node that is not a party to the delegation gets the same 404 as a
	// missing id so foreign ids cannot be probed.
	if !ok || (viaNode && !delegationVisibleTo(d, requester.ID)) {
		http.Error(w, fmt.Sprintf("unknown delegation %q", suffix), http.StatusNotFound)
		return
	}
	resp := map[string]any{}
	if refreshErr := s.refreshDelegation(r.Context(), &d); refreshErr != nil {
		resp["refresh_error"] = refreshErr.Error()
	}
	resp["delegation"] = d
	writeJSON(w, http.StatusOK, resp)
}

func (s *hubServer) handleCreateDelegation(w http.ResponseWriter, r *http.Request, origin hub.Node) {
	var req hubDelegationCreateRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	targetID := strings.TrimSpace(req.TargetNode)
	prompt := strings.TrimSpace(req.Prompt)
	if targetID == "" || prompt == "" {
		http.Error(w, "target_node and prompt are required", http.StatusBadRequest)
		return
	}
	if targetID == origin.ID {
		http.Error(w, "cannot delegate to the originating node", http.StatusBadRequest)
		return
	}
	if !origin.CanDelegateTo(targetID) {
		http.Error(w, fmt.Sprintf("node %q may not delegate to %q", origin.ID, targetID), http.StatusForbidden)
		return
	}
	target, ok := s.registry.Lookup(targetID)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown target node %q", targetID), http.StatusNotFound)
		return
	}
	if err := target.AcceptsDelegationFrom(origin.ID); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	cwd, err := resolveDelegationCwd(target.DelegationWorkdir(), req.Cwd)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	depth := 1
	chain := []string{origin.ID, target.ID}
	if parentID := strings.TrimSpace(req.ParentDelegationID); parentID != "" {
		parent, found, err := s.delegations.Get(parentID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !found {
			http.Error(w, fmt.Sprintf("unknown parent_delegation_id %q", parentID), http.StatusBadRequest)
			return
		}
		if parent.TargetNode != origin.ID {
			http.Error(w, "parent_delegation_id does not belong to the delegating node", http.StatusForbidden)
			return
		}
		for _, hop := range parent.Chain {
			if hop == target.ID {
				http.Error(w, fmt.Sprintf("delegation loop: node %q already appears in chain %v", target.ID, parent.Chain), http.StatusConflict)
				return
			}
		}
		depth = parent.Depth + 1
		chain = append(append([]string{}, parent.Chain...), target.ID)
	}
	if depth > hub.DefaultDelegationMaxDepth {
		http.Error(w, fmt.Sprintf("delegation depth %d exceeds the maximum of %d", depth, hub.DefaultDelegationMaxDepth), http.StatusForbidden)
		return
	}

	if err := s.checkDelegationCaps(r.Context(), origin, target); err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}

	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = hubDelegationDefaultTimeout
	}
	if timeout < hubDelegationMinTimeout {
		timeout = hubDelegationMinTimeout
	}
	if timeout > hubDelegationDefaultTimeout {
		timeout = hubDelegationDefaultTimeout
	}
	agentName := strings.TrimSpace(req.AgentName)
	if agentName == "" {
		agentName = hubDelegationDefaultAgent
	}
	if err := target.AcceptsDelegationAgent(agentName); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	model := strings.TrimSpace(req.Model)
	if err := target.AcceptsDelegationModel(model); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	id, err := hub.NewDelegationID()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	d := hub.Delegation{
		ID:         id,
		OriginNode: origin.ID,
		TargetNode: target.ID,
		AgentName:  agentName,
		Prompt:     prompt,
		Model:      model,
		Cwd:        cwd,
		Status:     hub.DelegationStatusPending,
		Depth:      depth,
		Chain:      chain,
		ParentID:   strings.TrimSpace(req.ParentDelegationID),
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := s.delegations.Add(d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jobID, runID, err := s.createDelegationJob(r.Context(), target, d, prompt, timeout)
	d.JobID = jobID
	d.RunID = runID
	if err != nil {
		updated, updateErr := s.delegations.Update(d.ID, func(rec *hub.Delegation) {
			rec.JobID = jobID
			rec.RunID = runID
			rec.Error = err.Error()
			if runID != "" {
				rec.Status = hub.DelegationStatusRunning
			} else if jobID != "" {
				// The target job is known, but the trigger response was lost or failed.
				// Keep the record non-terminal so later status reads can discover a run
				// via /v2/runs?job_id=... after a node reconnects.
				rec.Status = hub.DelegationStatusPending
			} else {
				rec.Status = hub.DelegationStatusError
			}
		})
		if updateErr == nil {
			d = updated
		}
		http.Error(w, fmt.Sprintf("delegate to node %q: %v", target.ID, err), http.StatusBadGateway)
		return
	}
	d.Status = hub.DelegationStatusRunning
	updated, err := s.delegations.Update(d.ID, func(rec *hub.Delegation) {
		rec.JobID = jobID
		rec.RunID = runID
		rec.Status = hub.DelegationStatusRunning
		rec.Error = ""
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d = updated
	writeJSON(w, http.StatusCreated, map[string]any{"delegation": d})
}

func (s *hubServer) handleCancelDelegation(w http.ResponseWriter, r *http.Request, id string) {
	requester, err := s.authenticateNode(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	d, ok, err := s.delegations.Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, fmt.Sprintf("unknown delegation %q", id), http.StatusNotFound)
		return
	}
	if d.OriginNode != requester.ID {
		http.Error(w, "only the originating node may cancel a delegation", http.StatusForbidden)
		return
	}
	if hub.DelegationStatusTerminal(d.Status) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"delegation": d,
			"error":      fmt.Sprintf("delegation %q is already %s", id, d.Status),
		})
		return
	}
	target, found := s.registry.Lookup(d.TargetNode)
	if !found {
		http.Error(w, fmt.Sprintf("target node %q is not present in the hub registry", d.TargetNode), http.StatusBadGateway)
		return
	}
	if d.RunID == "" && d.JobID != "" {
		// A previous create/trigger may have lost the trigger response across a
		// direct/reverse disconnect. Try one refresh so cancel can target the run
		// if the node did in fact start it.
		if err := s.refreshDelegation(r.Context(), &d); err != nil {
			http.Error(w, fmt.Sprintf("refresh delegation before cancel on node %q: %v", target.ID, err), http.StatusBadGateway)
			return
		}
	}
	if d.RunID != "" {
		if err := s.doNodeJSON(r.Context(), target, http.MethodPost,
			"/v2/runs/"+url.PathEscape(d.RunID)+"/cancel", map[string]any{}, nil); err != nil {
			http.Error(w, fmt.Sprintf("cancel run on node %q: %v", target.ID, err), http.StatusBadGateway)
			return
		}
	}
	updated, err := s.delegations.Update(d.ID, func(rec *hub.Delegation) {
		if !hub.DelegationStatusTerminal(rec.Status) {
			rec.Status = hub.DelegationStatusCancelRequested
		}
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"delegation": updated})
}

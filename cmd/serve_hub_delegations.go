package cmd

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
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
	hubNodeIDHeader = "X-Term-LLM-Node-ID"

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

	jobID, runID, err := s.createDelegationJob(r.Context(), target, d, prompt, timeout)
	d.JobID = jobID
	d.RunID = runID
	if err != nil {
		if jobID != "" {
			// The job exists on the target but never triggered; keep an audit
			// record so the orphan is traceable.
			d.Status = hub.DelegationStatusError
			d.Error = err.Error()
			_ = s.delegations.Add(d)
		}
		http.Error(w, fmt.Sprintf("delegate to node %q: %v", target.ID, err), http.StatusBadGateway)
		return
	}
	d.Status = hub.DelegationStatusRunning
	if err := s.delegations.Add(d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
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
		http.Error(w, fmt.Sprintf("target node %q is no longer known to the hub", d.TargetNode), http.StatusBadGateway)
		return
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

// checkDelegationCaps enforces hub-wide, per-origin, and per-target in-flight
// limits. Ledger statuses only advance when polled, so before rejecting it
// refreshes active records once (bounded by the hub-wide cap) and recounts.
func (s *hubServer) checkDelegationCaps(ctx context.Context, origin, target hub.Node) error {
	over := func() (error, bool) {
		total, byOrigin, byTarget, err := s.delegations.ActiveCounts()
		if err != nil {
			return err, false
		}
		if byTarget[target.ID] >= target.DelegationMaxInFlight() {
			return fmt.Errorf("node %q already has %d delegations in flight (max %d)", target.ID, byTarget[target.ID], target.DelegationMaxInFlight()), true
		}
		if byOrigin[origin.ID] >= hubDelegationOriginMaxInFlight {
			return fmt.Errorf("node %q already originates %d delegations in flight (max %d)", origin.ID, byOrigin[origin.ID], hubDelegationOriginMaxInFlight), true
		}
		if total >= hubDelegationHubMaxInFlight {
			return fmt.Errorf("hub already has %d delegations in flight (max %d)", total, hubDelegationHubMaxInFlight), true
		}
		return nil, false
	}
	err, capped := over()
	if err == nil || !capped {
		return err
	}
	s.refreshActiveDelegations(ctx)
	err, _ = over()
	return err
}

// refreshActiveDelegations re-polls every active record's target run so stale
// "running" entries don't pin in-flight caps forever.
func (s *hubServer) refreshActiveDelegations(ctx context.Context) {
	records, err := s.delegations.List()
	if err != nil {
		return
	}
	for i := range records {
		if hub.DelegationStatusTerminal(records[i].Status) {
			continue
		}
		_ = s.refreshDelegation(ctx, &records[i])
	}
}

// refreshDelegation polls the target node for the delegation's latest run and
// folds any status change back into the ledger and *d.
func (s *hubServer) refreshDelegation(ctx context.Context, d *hub.Delegation) error {
	if hub.DelegationStatusTerminal(d.Status) || d.JobID == "" {
		return nil
	}
	target, ok := s.registry.Lookup(d.TargetNode)
	if !ok {
		return fmt.Errorf("target node %q is no longer known to the hub", d.TargetNode)
	}
	var run hubNodeJobsRun
	if d.RunID != "" {
		// Poll the exact run the delegation triggered: the job's newest run
		// could be a different (e.g. manually re-triggered) one.
		if err := s.doNodeJSON(ctx, target, http.MethodGet,
			"/v2/runs/"+url.PathEscape(d.RunID), nil, &run); err != nil {
			return fmt.Errorf("poll node %q: %w", target.ID, err)
		}
	} else {
		var runs struct {
			Data []hubNodeJobsRun `json:"data"`
		}
		if err := s.doNodeJSON(ctx, target, http.MethodGet,
			"/v2/runs?limit=1&offset=0&job_id="+url.QueryEscape(d.JobID), nil, &runs); err != nil {
			return fmt.Errorf("poll node %q: %w", target.ID, err)
		}
		if len(runs.Data) == 0 {
			return nil
		}
		run = runs.Data[0]
	}
	status := delegationStatusFromRun(run.Status)
	if status == d.Status && run.ID == d.RunID {
		return nil
	}
	updated, err := s.delegations.Update(d.ID, func(rec *hub.Delegation) {
		rec.Status = status
		if run.ID != "" {
			rec.RunID = run.ID
		}
		if hub.DelegationStatusTerminal(status) {
			rec.Response = run.Response
			rec.Error = run.Error
		}
	})
	if err != nil {
		return err
	}
	*d = updated
	return nil
}

// delegationStatusFromRun maps a jobs-v2 run status onto a delegation status.
func delegationStatusFromRun(runStatus string) string {
	switch runStatus {
	case "succeeded":
		return hub.DelegationStatusSucceeded
	case "failed", "skipped":
		return hub.DelegationStatusFailed
	case "cancelled":
		return hub.DelegationStatusCancelled
	case "cancel_requested":
		return hub.DelegationStatusCancelRequested
	case "timed_out":
		return hub.DelegationStatusTimedOut
	default:
		return hub.DelegationStatusRunning
	}
}

// resolveDelegationCwd defaults to the target's delegation workdir and
// confines an explicit cwd inside it: the workdir is the target operator's
// consent boundary for delegated work. The workdir itself is canonicalized
// (absolute, "."/".." resolved) so prefix containment cannot be confused by
// an un-normalized configured path.
func resolveDelegationCwd(workdir, requested string) (string, error) {
	workdir = strings.TrimSpace(workdir)
	if workdir == "" {
		return "", errors.New("target node has no delegation workdir")
	}
	if !strings.HasPrefix(workdir, "/") {
		return "", fmt.Errorf("target node's delegation workdir %q is not an absolute path", workdir)
	}
	workdir = pathClean(workdir)
	if workdir == "" {
		return "", errors.New("target node's delegation workdir resolves to the filesystem root; refusing")
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return workdir, nil
	}
	if !strings.HasPrefix(requested, "/") {
		requested = workdir + "/" + requested
	}
	cwd := pathClean(requested)
	if cwd == workdir || strings.HasPrefix(cwd, workdir+"/") {
		return cwd, nil
	}
	return "", fmt.Errorf("cwd %q is outside the target node's delegation workdir %q", requested, workdir)
}

// pathClean is filepath.Clean with forward slashes: delegation workdirs are
// paths on the (POSIX) target node, not on the hub host.
func pathClean(p string) string {
	cleaned := strings.Builder{}
	segments := strings.Split(p, "/")
	stack := make([]string, 0, len(segments))
	for _, seg := range segments {
		switch seg {
		case "", ".":
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		default:
			stack = append(stack, seg)
		}
	}
	cleaned.WriteString("/" + strings.Join(stack, "/"))
	return strings.TrimRight(cleaned.String(), "/")
}

const hubDelegationStatusFooter = `

---
Before your final message, state your completion status on its own line in this exact format:
STATUS: COMPLETE
or
STATUS: BLOCKED — <brief reason you could not complete the task>
or
STATUS: PARTIAL — <what was done and what is still missing>

Choose COMPLETE only if you fully accomplished the task. Do not omit this line.`

// hubDelegationInstructions wraps the delegated prompt with provenance so the
// target agent knows the work arrived via the hub, plus the STATUS footer the
// jobs-v2 agent flow expects.
func hubDelegationInstructions(d hub.Delegation, prompt string) string {
	var b strings.Builder
	b.WriteString("You are handling a cross-node delegation routed through a term-llm Hub.\n")
	fmt.Fprintf(&b, "Delegation id: %s\n", d.ID)
	fmt.Fprintf(&b, "Origin node: %s\n", d.OriginNode)
	fmt.Fprintf(&b, "Target node (you): %s\n", d.TargetNode)
	fmt.Fprintf(&b, "Delegation depth: %d of max %d (chain: %s)\n", d.Depth, hub.DefaultDelegationMaxDepth, strings.Join(d.Chain, " -> "))
	b.WriteString("Further hub_delegate calls from this job chain automatically: parent_delegation_id is attached for you.\n")
	b.WriteString("\n---\n\n")
	b.WriteString(prompt)
	b.WriteString(hubDelegationStatusFooter)
	return b.String()
}

// jobsV2HubDelegationLabel is the trusted label shape the hub writes onto
// delegated jobs. Target jobs-v2 runners use it for chain tracking and for
// making delegated runs visible as recognizable target-node sessions.
type jobsV2HubDelegationLabel struct {
	ID     string   `json:"id"`
	Origin string   `json:"origin"`
	Depth  int      `json:"depth"`
	Chain  []string `json:"chain"`
}

func hubDelegationLabelFromJobLabels(labels json.RawMessage) jobsV2HubDelegationLabel {
	if len(labels) == 0 {
		return jobsV2HubDelegationLabel{}
	}
	var parsed struct {
		HubDelegation jobsV2HubDelegationLabel `json:"hub_delegation"`
	}
	if err := json.Unmarshal(labels, &parsed); err != nil {
		return jobsV2HubDelegationLabel{}
	}
	parsed.HubDelegation.ID = strings.TrimSpace(parsed.HubDelegation.ID)
	parsed.HubDelegation.Origin = strings.TrimSpace(parsed.HubDelegation.Origin)
	return parsed.HubDelegation
}

// hubDelegationIDFromJobLabels extracts the hub_delegation.id label the hub
// writes onto delegated jobs. The jobs-v2 runner feeds it into the tool
// context so chained hub_delegate calls carry a trusted parent_delegation_id
// instead of relying on the model to volunteer one.
func hubDelegationIDFromJobLabels(labels json.RawMessage) string {
	return hubDelegationLabelFromJobLabels(labels).ID
}

func jobsV2SessionName(job jobsV2Job, cfg jobsV2LLMConfig) string {
	_ = cfg
	if label := hubDelegationLabelFromJobLabels(job.Labels); label.ID != "" {
		origin := label.Origin
		if origin == "" && len(label.Chain) > 0 {
			origin = strings.TrimSpace(label.Chain[0])
		}
		if origin == "" {
			origin = "Hub"
		}
		return "Delegation from " + origin
	}
	return ""
}

// hubNodeJobsRun mirrors the jobs-v2 run fields the hub needs.
type hubNodeJobsRun struct {
	ID       string `json:"id"`
	JobID    string `json:"job_id"`
	Status   string `json:"status"`
	Response string `json:"response"`
	Error    string `json:"error"`
}

// createDelegationJob creates and triggers a manual jobs-v2 LLM job on the
// target node. Returns the target job and run ids; a non-empty job id with an
// error means the job was created but never triggered.
func (s *hubServer) createDelegationJob(ctx context.Context, target hub.Node, d hub.Delegation, prompt string, timeoutSeconds int) (jobID, runID string, err error) {
	labels, err := json.Marshal(map[string]any{"hub_delegation": map[string]any{
		"id":     d.ID,
		"origin": d.OriginNode,
		"depth":  d.Depth,
		"chain":  d.Chain,
	}})
	if err != nil {
		return "", "", fmt.Errorf("encode delegation labels: %w", err)
	}
	runnerConfig := map[string]any{
		"agent_name":   d.AgentName,
		"instructions": hubDelegationInstructions(d, prompt),
		"cwd":          d.Cwd,
	}
	if d.Model != "" {
		runnerConfig["model"] = d.Model
	}
	payload := map[string]any{
		"name":               "hub-delegation-" + d.ID,
		"enabled":            true,
		"runner_type":        "llm",
		"runner_config":      runnerConfig,
		"trigger_type":       "manual",
		"concurrency_policy": "allow",
		"timeout_seconds":    timeoutSeconds,
		"misfire_policy":     "run",
		"labels":             json.RawMessage(labels),
	}
	var job struct {
		ID string `json:"id"`
	}
	if err := s.doNodeJSON(ctx, target, http.MethodPost, "/v2/jobs", payload, &job); err != nil {
		return "", "", fmt.Errorf("create job: %w", err)
	}
	if job.ID == "" {
		return "", "", errors.New("target jobs API returned a job without an id")
	}
	var run hubNodeJobsRun
	if err := s.doNodeJSON(ctx, target, http.MethodPost, "/v2/jobs/"+url.PathEscape(job.ID)+"/trigger", map[string]any{}, &run); err != nil {
		return job.ID, "", fmt.Errorf("trigger job %s: %w", job.ID, err)
	}
	if run.ID == "" {
		return job.ID, "", errors.New("target jobs API returned a run without an id")
	}
	return job.ID, run.ID, nil
}

// doNodeJSON performs one JSON request against a node's API (mounted under
// the node's base path) with the node's token injected server-side. It uses
// the hub's direct-dial client: routing token-carrying requests through an
// environment HTTP proxy would leak node tokens.
func (s *hubServer) doNodeJSON(ctx context.Context, node hub.Node, method, path string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode node request: %w", err)
		}
		body = strings.NewReader(string(data))
	}
	req, err := http.NewRequestWithContext(ctx, method, node.BaseURL()+path, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if node.Token != "" {
		req.Header.Set("Authorization", "Bearer "+node.Token)
	}
	resp, err := s.nodeAPIClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		// An upstream (or misconfigured proxy) may reflect the Authorization
		// header into the error body; never let the target's token travel
		// back to the origin node or into the ledger.
		if node.Token != "" {
			msg = strings.ReplaceAll(msg, node.Token, "[redacted]")
		}
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("node API %s %s: HTTP %d: %s", method, path, resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode node API response: %w", err)
	}
	return nil
}

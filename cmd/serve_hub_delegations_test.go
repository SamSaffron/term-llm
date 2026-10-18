package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/tools"
)

// fakeTargetJobs fakes the slice of a node's jobs-v2 API the hub drives:
// create job, trigger, list runs, cancel run.
type fakeTargetJobs struct {
	mu          sync.Mutex
	server      *httptest.Server
	jobSeq      int
	jobs        map[string]hubNodeJobsJob
	jobRuns     map[string]string
	lastAuth    string
	lastJobBody map[string]any
	runStatus   string
	runResponse string
	runError    string
	cancelled   []string
	// runGets records the exact run ids fetched via GET /v2/runs/{id}.
	runGets []string
	// reflectAuthFailures / reflectTriggerFailure make job create (resp.
	// trigger) fail with a body that echoes the Authorization header, like a
	// misbehaving upstream or proxy would.
	reflectAuthFailures   bool
	reflectTriggerFailure bool
}

func newFakeTargetJobs(t *testing.T) *fakeTargetJobs {
	t.Helper()
	f := &fakeTargetJobs{runStatus: "running", jobs: map[string]hubNodeJobsJob{}, jobRuns: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/v2/jobs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastAuth = r.Header.Get("Authorization")
		if r.Method == http.MethodGet {
			jobs := make([]hubNodeJobsJob, 0, len(f.jobs))
			for _, job := range f.jobs {
				jobs = append(jobs, job)
			}
			writeJSON(w, http.StatusOK, map[string]any{"data": jobs, "total": len(jobs)})
			return
		}
		if f.reflectAuthFailures {
			http.Error(w, "upstream rejected credential "+r.Header.Get("Authorization"), http.StatusInternalServerError)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.lastJobBody = body
		name, _ := body["name"].(string)
		for _, existing := range f.jobs {
			if existing.Name == name {
				http.Error(w, "job name already exists", http.StatusBadRequest)
				return
			}
		}
		f.jobSeq++
		id := fmt.Sprintf("job_%d", f.jobSeq)
		job := hubNodeJobsJob{ID: id, Name: name}
		if labels, ok := body["labels"].(map[string]any); ok {
			if data, err := json.Marshal(labels); err == nil {
				job.Labels = data
			}
		}
		f.jobs[id] = job
		writeJSON(w, http.StatusCreated, job)
	})
	mux.HandleFunc("/chat/v2/jobs/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastAuth = r.Header.Get("Authorization")
		if f.reflectTriggerFailure {
			http.Error(w, "upstream rejected credential "+r.Header.Get("Authorization"), http.StatusInternalServerError)
			return
		}
		jobID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/chat/v2/jobs/"), "/trigger")
		runID := "run-for-" + jobID
		f.jobRuns[jobID] = runID
		writeJSON(w, http.StatusCreated, map[string]any{"id": runID, "job_id": jobID, "status": "queued"})
	})
	mux.HandleFunc("/chat/v2/runs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastAuth = r.Header.Get("Authorization")
		jobID := r.URL.Query().Get("job_id")
		runID := f.jobRuns[jobID]
		data := []map[string]any{}
		if runID != "" {
			data = append(data, map[string]any{
				"id":       runID,
				"job_id":   jobID,
				"status":   f.runStatus,
				"response": f.runResponse,
				"error":    f.runError,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": data})
	})
	mux.HandleFunc("/chat/v2/runs/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastAuth = r.Header.Get("Authorization")
		path := strings.TrimPrefix(r.URL.Path, "/chat/v2/runs/")
		if runID, ok := strings.CutSuffix(path, "/cancel"); ok {
			f.cancelled = append(f.cancelled, runID)
			writeJSON(w, http.StatusOK, map[string]any{"id": runID, "status": "cancel_requested"})
			return
		}
		f.runGets = append(f.runGets, path)
		writeJSON(w, http.StatusOK, map[string]any{
			"id":       path,
			"job_id":   strings.TrimPrefix(path, "run-for-"),
			"status":   f.runStatus,
			"response": f.runResponse,
			"error":    f.runError,
		})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeTargetJobs) setRun(status, response, errText string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runStatus, f.runResponse, f.runError = status, response, errText
}

// newDelegationHub builds a hub over a fake fleet: alpha and beta accept
// delegations (workdir /work), closed has no workdir, restricted may only
// delegate to gamma, tokenless has no stored token.
func newDelegationHub(t *testing.T) (*hubServer, *fakeTargetJobs) {
	t.Helper()
	fake := newFakeTargetJobs(t)
	nodes := []hub.Node{
		{ID: "alpha", Name: "Alpha", URL: fake.server.URL, BasePath: "/chat", Token: "alpha-token",
			Delegation: &hub.DelegationPolicy{Enabled: true, Workdir: "/work"}},
		{ID: "beta", Name: "Beta", URL: fake.server.URL, BasePath: "/chat", Token: "beta-token",
			Delegation: &hub.DelegationPolicy{Enabled: true, Workdir: "/work", MaxInFlight: 1, AllowedModels: []string{"haiku"}}},
		{ID: "gamma", Name: "Gamma", URL: fake.server.URL, BasePath: "/chat", Token: "gamma-token",
			Delegation: &hub.DelegationPolicy{Enabled: true, Workdir: "/work"}},
		{ID: "anyagent", Name: "AnyAgent", URL: fake.server.URL, BasePath: "/chat", Token: "anyagent-token",
			Delegation: &hub.DelegationPolicy{Enabled: true, Workdir: "/work", AllowedAgents: []string{"*"}}},
		{ID: "pathy", Name: "Pathy", URL: fake.server.URL, BasePath: "/chat", Token: "pathy-token",
			Delegation: &hub.DelegationPolicy{Enabled: true, Workdir: "/work", AllowedAgents: []string{"skills/custom-agent"}}},
		{ID: "closed", Name: "Closed", URL: fake.server.URL, BasePath: "/chat", Token: "closed-token"},
		{ID: "restricted", Name: "Restricted", URL: fake.server.URL, BasePath: "/chat", Token: "restricted-token",
			Delegation: &hub.DelegationPolicy{Enabled: true, To: []string{"gamma"}, Workdir: "/work"}},
		{ID: "tokenless", Name: "Tokenless", URL: fake.server.URL, BasePath: "/chat", Token: ""},
		{ID: "picky", Name: "Picky", URL: fake.server.URL, BasePath: "/chat", Token: "picky-token",
			Delegation: &hub.DelegationPolicy{Enabled: true, AcceptFrom: []string{"beta"}, Workdir: "/work"}},
	}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: nodes}), nil)
	s.delegations = hub.NewDelegationStore(filepath.Join(t.TempDir(), "delegations.json"))
	return s, fake
}

func delegationRequest(method, path, nodeID, token string, payload any) *http.Request {
	var req *http.Request
	if payload != nil {
		data, _ := json.Marshal(payload)
		req = httptest.NewRequest(method, path, strings.NewReader(string(data)))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if nodeID != "" {
		req.Header.Set(hubNodeIDHeader, nodeID)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func createDelegation(t *testing.T, s *hubServer, nodeID, token string, payload map[string]any) (*httptest.ResponseRecorder, hub.Delegation) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, delegationRequest(http.MethodPost, "/api/delegations", nodeID, token, payload))
	var resp struct {
		Delegation hub.Delegation `json:"delegation"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp.Delegation
}

func TestHubDelegationAuthMatrix(t *testing.T) {
	s, _ := newDelegationHub(t)
	payload := map[string]any{"target_node": "beta", "prompt": "do the thing"}

	cases := []struct {
		name   string
		nodeID string
		token  string
		want   int
	}{
		{"no credentials", "", "", http.StatusUnauthorized},
		{"missing token", "alpha", "", http.StatusUnauthorized},
		{"wrong token", "alpha", "beta-token", http.StatusUnauthorized},
		{"unknown node", "ghost", "alpha-token", http.StatusUnauthorized},
		{"empty-token node cannot authenticate", "tokenless", "anything", http.StatusUnauthorized},
		{"valid", "alpha", "alpha-token", http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := createDelegation(t, s, tc.nodeID, tc.token, payload)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestHubDelegationCreateHappyPath(t *testing.T) {
	s, fake := newDelegationHub(t)
	rec, d := createDelegation(t, s, "alpha", "alpha-token", map[string]any{
		"target_node": "beta",
		"prompt":      "summarize the build failures",
		"model":       "haiku",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if d.ID == "" || d.Status != hub.DelegationStatusRunning {
		t.Fatalf("delegation = %+v", d)
	}
	if d.JobID != "job_1" || d.RunID != "run-for-job_1" {
		t.Fatalf("job/run ids = %q %q", d.JobID, d.RunID)
	}
	if d.Depth != 1 || strings.Join(d.Chain, ",") != "alpha,beta" {
		t.Fatalf("depth/chain = %d %v", d.Depth, d.Chain)
	}

	// The hub must talk to the target with the TARGET's token, and no token
	// may appear in the client-facing response.
	if fake.lastAuth != "Bearer beta-token" {
		t.Fatalf("target saw auth %q", fake.lastAuth)
	}
	body := rec.Body.String()
	for _, secret := range []string{"alpha-token", "beta-token"} {
		if strings.Contains(body, secret) {
			t.Fatalf("response leaked token %q: %s", secret, body)
		}
	}

	runner, _ := fake.lastJobBody["runner_config"].(map[string]any)
	if runner["cwd"] != "/work" {
		t.Fatalf("cwd = %v, want target workdir", runner["cwd"])
	}
	if runner["model"] != "haiku" || runner["agent_name"] != hubDelegationDefaultAgent {
		t.Fatalf("runner_config = %v", runner)
	}
	instructions, _ := runner["instructions"].(string)
	if instructions != "summarize the build failures" {
		t.Fatalf("instructions = %q, want delegated prompt verbatim", instructions)
	}
	labels, _ := json.Marshal(fake.lastJobBody["labels"])
	if !strings.Contains(string(labels), d.ID) || !strings.Contains(string(labels), "hub_delegation") {
		t.Fatalf("labels = %s", labels)
	}
	label := hubDelegationLabelFromJobLabels(json.RawMessage(labels))
	if label.ID != d.ID || label.Origin != "alpha" || label.Depth != 1 || strings.Join(label.Chain, ",") != "alpha,beta" {
		t.Fatalf("parsed delegation label = %+v", label)
	}
	if got := jobsV2SessionName(jobsV2Job{Labels: json.RawMessage(labels)}); got != "Delegation from alpha" {
		t.Fatalf("delegation session name = %q", got)
	}
}

func TestHubDelegationPolicyDeny(t *testing.T) {
	s, _ := newDelegationHub(t)

	rec, _ := createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "closed", "prompt": "x"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "does not accept delegations") {
		t.Fatalf("no-workdir target: %d %q", rec.Code, rec.Body.String())
	}

	rec, _ = createDelegation(t, s, "restricted", "restricted-token", map[string]any{"target_node": "beta", "prompt": "x"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "may not delegate") {
		t.Fatalf("origin to-list: %d %q", rec.Code, rec.Body.String())
	}

	rec, _ = createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "picky", "prompt": "x"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "does not accept delegations from") {
		t.Fatalf("accept_from list: %d %q", rec.Code, rec.Body.String())
	}

	rec, _ = createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "alpha", "prompt": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("self-delegation: %d %q", rec.Code, rec.Body.String())
	}

	rec, _ = createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "ghost-node", "prompt": "x"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown target: %d %q", rec.Code, rec.Body.String())
	}
}

func TestHubDelegationCwdConfinement(t *testing.T) {
	s, fake := newDelegationHub(t)

	rec, _ := createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "beta", "prompt": "x", "cwd": "/etc"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "outside the target node's delegation workdir") {
		t.Fatalf("escape cwd: %d %q", rec.Code, rec.Body.String())
	}
	rec, _ = createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "beta", "prompt": "x", "cwd": "/work/../etc"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("dot-dot cwd: %d %q", rec.Code, rec.Body.String())
	}
	rec, _ = createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "gamma", "prompt": "x", "cwd": "/work/sub/dir"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("nested cwd: %d %q", rec.Code, rec.Body.String())
	}
	runner, _ := fake.lastJobBody["runner_config"].(map[string]any)
	if runner["cwd"] != "/work/sub/dir" {
		t.Fatalf("cwd = %v", runner["cwd"])
	}
}

func TestHubDelegationParentDepthAndLoops(t *testing.T) {
	s, _ := newDelegationHub(t)

	_, root := createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "beta", "prompt": "root"})
	if root.ID == "" {
		t.Fatalf("root create failed")
	}

	// beta (target of root) chains to gamma: depth 2.
	rec, child := createDelegation(t, s, "beta", "beta-token", map[string]any{
		"target_node": "gamma", "prompt": "child", "parent_delegation_id": root.ID,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("chained create: %d %q", rec.Code, rec.Body.String())
	}
	if child.Depth != 2 || strings.Join(child.Chain, ",") != "alpha,beta,gamma" {
		t.Fatalf("child depth/chain = %d %v", child.Depth, child.Chain)
	}

	// Unknown parent id.
	rec, _ = createDelegation(t, s, "beta", "beta-token", map[string]any{
		"target_node": "gamma", "prompt": "x", "parent_delegation_id": "dlg_nope",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown parent: %d %q", rec.Code, rec.Body.String())
	}

	// Only the node the parent targeted may chain on it.
	rec, _ = createDelegation(t, s, "alpha", "alpha-token", map[string]any{
		"target_node": "gamma", "prompt": "x", "parent_delegation_id": root.ID,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("parent ownership: %d %q", rec.Code, rec.Body.String())
	}

	// Loop refusal: root chain is alpha->beta, so beta may not delegate back
	// to alpha under that parent.
	rec, _ = createDelegation(t, s, "beta", "beta-token", map[string]any{
		"target_node": "alpha", "prompt": "x", "parent_delegation_id": root.ID,
	})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "delegation loop") {
		t.Fatalf("loop: %d %q", rec.Code, rec.Body.String())
	}

	// Depth cap: a parent already at max depth cannot chain further.
	deep := hub.Delegation{
		ID: "dlg_deep", OriginNode: "x1", TargetNode: "alpha",
		Status: hub.DelegationStatusRunning, Depth: hub.DefaultDelegationMaxDepth,
		Chain: []string{"x1", "x2", "alpha"}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := s.delegations.Add(deep); err != nil {
		t.Fatalf("seed deep parent: %v", err)
	}
	rec, _ = createDelegation(t, s, "alpha", "alpha-token", map[string]any{
		"target_node": "gamma", "prompt": "x", "parent_delegation_id": "dlg_deep",
	})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "depth") {
		t.Fatalf("depth cap: %d %q", rec.Code, rec.Body.String())
	}
}

func TestHubDelegationStatusRefreshAndList(t *testing.T) {
	s, fake := newDelegationHub(t)
	_, d := createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "beta", "prompt": "x"})

	// Still running on the target.
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, delegationRequest(http.MethodGet, "/api/delegations/"+d.ID, "alpha", "alpha-token", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status GET: %d %q", rec.Code, rec.Body.String())
	}
	var resp struct {
		Delegation hub.Delegation `json:"delegation"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Delegation.Status != hub.DelegationStatusRunning {
		t.Fatalf("status = %q", resp.Delegation.Status)
	}

	// Target finishes; the Hub dashboard list refresh folds the result into the
	// ledger too, so artifact links/images appear without opening an item route.
	fake.setRun("succeeded", "![art](/chat/files/art.svg)", "")
	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/delegations", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), hub.DelegationStatusSucceeded) || !strings.Contains(rec.Body.String(), "/chat/files/art.svg") {
		t.Fatalf("dashboard list refresh: %d %q", rec.Code, rec.Body.String())
	}

	// Item polling also returns the refreshed terminal result.
	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, delegationRequest(http.MethodGet, "/api/delegations/"+d.ID, "alpha", "alpha-token", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Delegation.Status != hub.DelegationStatusSucceeded || resp.Delegation.Response != "![art](/chat/files/art.svg)" {
		t.Fatalf("refreshed = %+v", resp.Delegation)
	}
	stored, ok, _ := s.delegations.Get(d.ID)
	if !ok || stored.Status != hub.DelegationStatusSucceeded {
		t.Fatalf("ledger not updated: %+v", stored)
	}

	// Same-origin browser GET (no node auth) may read; the list carries no tokens.
	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/delegations", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), d.ID) {
		t.Fatalf("list: %d %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "beta-token") {
		t.Fatalf("list leaked a token")
	}

	// Failed node auth must not fall back to the browser path.
	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, delegationRequest(http.MethodGet, "/api/delegations", "alpha", "wrong", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad node auth fallback: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/delegations/dlg_missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing id: %d", rec.Code)
	}
}

func TestHubDelegationCancel(t *testing.T) {
	s, fake := newDelegationHub(t)
	_, d := createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "beta", "prompt": "x"})

	// Only the originating node may cancel.
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, delegationRequest(http.MethodPost, "/api/delegations/"+d.ID+"/cancel", "beta", "beta-token", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-origin cancel: %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, delegationRequest(http.MethodPost, "/api/delegations/"+d.ID+"/cancel", "alpha", "alpha-token", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("origin cancel: %d %q", rec.Code, rec.Body.String())
	}
	fake.mu.Lock()
	cancelled := append([]string(nil), fake.cancelled...)
	fake.mu.Unlock()
	if len(cancelled) != 1 || cancelled[0] != d.RunID {
		t.Fatalf("cancelled runs = %v, want [%s]", cancelled, d.RunID)
	}

	// Once terminal, cancelling again conflicts.
	fake.setRun("cancelled", "", "cancelled by user")
	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, delegationRequest(http.MethodGet, "/api/delegations/"+d.ID, "alpha", "alpha-token", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh after cancel: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, delegationRequest(http.MethodPost, "/api/delegations/"+d.ID+"/cancel", "alpha", "alpha-token", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("cancel terminal: %d %q", rec.Code, rec.Body.String())
	}
}

func TestHubDelegationInFlightCaps(t *testing.T) {
	s, _ := newDelegationHub(t)

	// beta's policy caps in-flight delegations at 1; the fake target reports
	// the first run still running, so the second create must be refused.
	rec, _ := createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "beta", "prompt": "first"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create: %d %q", rec.Code, rec.Body.String())
	}
	rec, _ = createDelegation(t, s, "gamma", "gamma-token", map[string]any{"target_node": "beta", "prompt": "second"})
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "in flight") {
		t.Fatalf("capped create: %d %q", rec.Code, rec.Body.String())
	}
	// Other targets are unaffected by beta's cap.
	rec, _ = createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "gamma", "prompt": "third"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("other target: %d %q", rec.Code, rec.Body.String())
	}
}

func TestResolveDelegationCwd(t *testing.T) {
	cases := []struct {
		workdir, requested, want string
		wantErr                  bool
	}{
		{"/work", "", "/work", false},
		{"/work/", "", "/work", false},
		{"/work", "/work", "/work", false},
		{"/work", "/work/sub", "/work/sub", false},
		{"/work", "sub/dir", "/work/sub/dir", false},
		{"/work", "/worker", "", true},
		{"/work", "/etc", "", true},
		{"/work", "/work/../etc", "", true},
		{"/work", "../outside", "", true},
		{"", "", "", true},
		// Workdir canonicalization: relative or root workdirs are refused,
		// un-normalized ones are cleaned before containment checks.
		{"relative", "", "", true},
		{"/", "", "", true},
		{"/work/..", "", "", true},
		{"/work/.", "", "/work", false},
		{"/work/sub/..", "sub", "/work/sub", false},
		{"/wo\nrk", "", "", true},
		{"/work", "sub\x00dir", "", true},
	}
	for _, tc := range cases {
		got, err := resolveDelegationCwd(tc.workdir, tc.requested)
		if tc.wantErr {
			if err == nil {
				t.Errorf("resolveDelegationCwd(%q, %q) = %q, want error", tc.workdir, tc.requested, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("resolveDelegationCwd(%q, %q) = %q, %v; want %q", tc.workdir, tc.requested, got, err, tc.want)
		}
	}
}

func TestHubDelegationAgentModelPolicy(t *testing.T) {
	s, _ := newDelegationHub(t)

	// Default policy: only the default delegation agent is accepted.
	rec, _ := createDelegation(t, s, "alpha", "alpha-token", map[string]any{
		"target_node": "beta", "prompt": "x", "agent_name": "shell"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "default delegation agent") {
		t.Fatalf("non-default agent: %d %q", rec.Code, rec.Body.String())
	}

	// "*" accepts plain agent names but never path-like ones.
	rec, _ = createDelegation(t, s, "alpha", "alpha-token", map[string]any{
		"target_node": "anyagent", "prompt": "x", "agent_name": "reviewer"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("wildcard agent: %d %q", rec.Code, rec.Body.String())
	}
	for _, agent := range []string{"../evil", "skills/custom-agent", ".hidden", `..\evil`, "~root"} {
		rec, _ = createDelegation(t, s, "alpha", "alpha-token", map[string]any{
			"target_node": "anyagent", "prompt": "x", "agent_name": agent})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("path-like agent %q: %d %q", agent, rec.Code, rec.Body.String())
		}
	}

	// Path-like names work when listed exactly.
	rec, _ = createDelegation(t, s, "alpha", "alpha-token", map[string]any{
		"target_node": "pathy", "prompt": "x", "agent_name": "skills/custom-agent"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("exact path agent: %d %q", rec.Code, rec.Body.String())
	}

	// Model overrides: refused without allowed_models, exact match enforced.
	rec, _ = createDelegation(t, s, "alpha", "alpha-token", map[string]any{
		"target_node": "gamma", "prompt": "x", "model": "haiku"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "model override") {
		t.Fatalf("model without allowed_models: %d %q", rec.Code, rec.Body.String())
	}
	rec, _ = createDelegation(t, s, "alpha", "alpha-token", map[string]any{
		"target_node": "beta", "prompt": "x", "model": "gpt-x"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disallowed model: %d %q", rec.Code, rec.Body.String())
	}
}

func TestHubDelegationReadScoping(t *testing.T) {
	s, _ := newDelegationHub(t)
	_, d := createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "beta", "prompt": "private"})
	if d.ID == "" {
		t.Fatalf("create failed")
	}

	// A node that is neither origin nor target must not see the record.
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, delegationRequest(http.MethodGet, "/api/delegations", "gamma", "gamma-token", nil))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), d.ID) {
		t.Fatalf("foreign list: %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, delegationRequest(http.MethodGet, "/api/delegations/"+d.ID, "gamma", "gamma-token", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign item should 404: %d %q", rec.Code, rec.Body.String())
	}

	// Origin and target both may read item and see it in their list.
	for _, creds := range [][2]string{{"alpha", "alpha-token"}, {"beta", "beta-token"}} {
		rec = httptest.NewRecorder()
		s.handler().ServeHTTP(rec, delegationRequest(http.MethodGet, "/api/delegations/"+d.ID, creds[0], creds[1], nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("party read %s: %d %q", creds[0], rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		s.handler().ServeHTTP(rec, delegationRequest(http.MethodGet, "/api/delegations", creds[0], creds[1], nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), d.ID) {
			t.Fatalf("party list %s: %d", creds[0], rec.Code)
		}
	}
}

func TestHubDelegationTargetTokenRedactedFromErrors(t *testing.T) {
	s, fake := newDelegationHub(t)
	fake.mu.Lock()
	fake.reflectAuthFailures = true
	fake.mu.Unlock()

	// Job creation fails with a body reflecting the Authorization header; the
	// hub's error response must never carry the target token back.
	rec, _ := createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "beta", "prompt": "x"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "beta-token") {
		t.Fatalf("error leaked the target token: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[redacted]") {
		t.Fatalf("expected redaction marker in %q", rec.Body.String())
	}
}

func TestHubDelegationTargetTokenRedactedFromLedger(t *testing.T) {
	s, fake := newDelegationHub(t)
	fake.mu.Lock()
	fake.reflectTriggerFailure = true
	fake.mu.Unlock()

	// Create succeeds, trigger fails reflecting the Authorization header: the
	// orphan audit record must store the redacted error.
	rec, _ := createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "beta", "prompt": "x"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "beta-token") {
		t.Fatalf("error leaked the target token: %q", rec.Body.String())
	}
	records, err := s.delegations.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("ledger records = %v (%v)", records, err)
	}
	if strings.Contains(records[0].Error, "beta-token") {
		t.Fatalf("ledger stored the target token: %q", records[0].Error)
	}
	if !strings.Contains(records[0].Error, "[redacted]") {
		t.Fatalf("expected redaction marker in ledger error %q", records[0].Error)
	}
}

func TestHubDelegationTriggerFailureKeepsJobPollable(t *testing.T) {
	s, fake := newDelegationHub(t)
	fake.mu.Lock()
	fake.reflectTriggerFailure = true
	fake.mu.Unlock()

	rec, _ := createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "beta", "prompt": "x"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d %q", rec.Code, rec.Body.String())
	}
	records, err := s.delegations.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("ledger records = %v (%v)", records, err)
	}
	d := records[0]
	if d.JobID == "" || d.RunID != "" || d.Status != hub.DelegationStatusPending {
		t.Fatalf("partial trigger record = %+v, want job_id only and pending", d)
	}

	fake.mu.Lock()
	fake.reflectTriggerFailure = false
	fake.jobRuns[d.JobID] = "run-for-" + d.JobID
	fake.mu.Unlock()
	fake.setRun("succeeded", "resumed", "")

	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, delegationRequest(http.MethodGet, "/api/delegations/"+d.ID, "alpha", "alpha-token", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %q", rec.Code, rec.Body.String())
	}
	var resp struct {
		Delegation hub.Delegation `json:"delegation"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Delegation.RunID != "run-for-"+d.JobID || resp.Delegation.Status != hub.DelegationStatusSucceeded || resp.Delegation.Response != "resumed" || resp.Delegation.Error != "" {
		t.Fatalf("refreshed partial trigger = %+v", resp.Delegation)
	}
}

func TestHubDelegationCancelDiscoversRunAfterLostTriggerResponse(t *testing.T) {
	s, fake := newDelegationHub(t)
	fake.mu.Lock()
	fake.reflectTriggerFailure = true
	fake.mu.Unlock()

	rec, _ := createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "beta", "prompt": "x"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("create with lost trigger response: %d %q", rec.Code, rec.Body.String())
	}
	records, err := s.delegations.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("ledger records = %v (%v)", records, err)
	}
	d := records[0]
	fake.mu.Lock()
	fake.reflectTriggerFailure = false
	fake.jobRuns[d.JobID] = "run-for-" + d.JobID
	fake.mu.Unlock()

	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, delegationRequest(http.MethodPost, "/api/delegations/"+d.ID+"/cancel", "alpha", "alpha-token", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %q", rec.Code, rec.Body.String())
	}
	fake.mu.Lock()
	cancelled := append([]string(nil), fake.cancelled...)
	fake.mu.Unlock()
	if len(cancelled) != 1 || cancelled[0] != "run-for-"+d.JobID {
		t.Fatalf("cancelled runs = %v, want discovered run", cancelled)
	}
}
func TestHubDelegationRefreshUsesExactRunID(t *testing.T) {
	s, fake := newDelegationHub(t)
	_, d := createDelegation(t, s, "alpha", "alpha-token", map[string]any{"target_node": "beta", "prompt": "x"})
	fake.setRun("succeeded", "done", "")

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, delegationRequest(http.MethodGet, "/api/delegations/"+d.ID, "alpha", "alpha-token", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %q", rec.Code, rec.Body.String())
	}
	var resp struct {
		Delegation hub.Delegation `json:"delegation"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Delegation.Status != hub.DelegationStatusSucceeded || resp.Delegation.Response != "done" {
		t.Fatalf("refreshed = %+v", resp.Delegation)
	}
	fake.mu.Lock()
	gets := append([]string(nil), fake.runGets...)
	fake.mu.Unlock()
	if len(gets) == 0 || gets[len(gets)-1] != d.RunID {
		t.Fatalf("run gets = %v, want exact run id %q", gets, d.RunID)
	}
}

func TestHubDelegationIDFromJobLabels(t *testing.T) {
	cases := []struct {
		labels string
		want   string
	}{
		{`{"hub_delegation":{"id":"dlg_abc"}}`, "dlg_abc"},
		{`{"hub_delegation":{"id":" dlg_abc "}}`, "dlg_abc"},
		{`{"other":true}`, ""},
		{`not json`, ""},
		{``, ""},
	}
	for _, tc := range cases {
		if got := hubDelegationIDFromJobLabels(json.RawMessage(tc.labels)); got != tc.want {
			t.Errorf("hubDelegationIDFromJobLabels(%q) = %q, want %q", tc.labels, got, tc.want)
		}
	}
}

func TestJobsV2LLMRunnerHubDelegationContext(t *testing.T) {
	got := "unset"
	runner := &jobsV2LLMRunner{exec: func(ctx context.Context, cfg jobsV2LLMConfig, onEvent func(core.Event)) (serveJobsExecResult, error) {
		got = tools.HubDelegationIDFromContext(ctx)
		return serveJobsExecResult{}, nil
	}}
	job := jobsV2Job{
		RunnerConfig: json.RawMessage(`{"agent_name":"developer","instructions":"x","cwd":"/tmp"}`),
		Labels:       json.RawMessage(`{"hub_delegation":{"id":"dlg_label"}}`),
	}
	if _, err := runner.Run(context.Background(), job, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "dlg_label" {
		t.Fatalf("delegation id in tool context = %q, want dlg_label", got)
	}

	job.Labels = nil
	if _, err := runner.Run(context.Background(), job, nil); err != nil {
		t.Fatalf("Run without labels: %v", err)
	}
	if got != "" {
		t.Fatalf("delegation id without labels = %q, want empty", got)
	}
}

func TestDelegationRefreshErrorTerminalDoesNotTerminalizeDeadline(t *testing.T) {
	prevStaleAfter := hubDelegationCapRefreshErrorStaleAfter
	hubDelegationCapRefreshErrorStaleAfter = time.Nanosecond
	defer func() { hubDelegationCapRefreshErrorStaleAfter = prevStaleAfter }()

	d := hub.Delegation{
		ID:        "dlg_deadline",
		Status:    hub.DelegationStatusRunning,
		UpdatedAt: time.Now().Add(-time.Hour),
	}
	if delegationRefreshErrorTerminal(d, context.DeadlineExceeded) {
		t.Fatal("context deadline exceeded should not terminalize a potentially healthy running delegation")
	}
	if !delegationRefreshErrorTerminal(d, fmt.Errorf("lookup target: %w", errDelegationTargetNotRegistered)) {
		t.Fatal("missing target sentinel should terminalize the delegation")
	}
}

func TestCheckDelegationCapsRecoversMissingTargetRecords(t *testing.T) {
	s, _ := newDelegationHub(t)
	now := time.Now().Add(-time.Hour).UTC()
	for i := 0; i < hubDelegationOriginMaxInFlight; i++ {
		if err := s.delegations.Add(hub.Delegation{
			ID:         fmt.Sprintf("dlg_missing_%d", i),
			OriginNode: "alpha",
			TargetNode: "gone",
			JobID:      fmt.Sprintf("job_%d", i),
			RunID:      fmt.Sprintf("run_%d", i),
			Status:     hub.DelegationStatusRunning,
			Depth:      1,
			Chain:      []string{"alpha", "gone"},
			CreatedAt:  now.Add(time.Duration(i) * time.Second),
			UpdatedAt:  now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("Add stale delegation %d: %v", i, err)
		}
	}

	origin, ok := s.registry.Lookup("alpha")
	if !ok {
		t.Fatal("origin node alpha missing from registry")
	}
	target, ok := s.registry.Lookup("beta")
	if !ok {
		t.Fatal("target node beta missing from registry")
	}
	if err := s.checkDelegationCaps(t.Context(), origin, target); err != nil {
		t.Fatalf("checkDelegationCaps: %v", err)
	}

	records, err := s.delegations.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, d := range records {
		if d.Status != hub.DelegationStatusError {
			t.Fatalf("delegation %q status = %q, want %q", d.ID, d.Status, hub.DelegationStatusError)
		}
		if !strings.Contains(d.Error, `target node "gone": target node is not present in the hub registry`) {
			t.Fatalf("delegation %q error = %q, want missing-target refresh error", d.ID, d.Error)
		}
	}

	total, byOrigin, byTarget, err := s.delegations.ActiveCounts()
	if err != nil {
		t.Fatalf("ActiveCounts: %v", err)
	}
	if total != 0 || byOrigin[origin.ID] != 0 || byTarget["gone"] != 0 {
		t.Fatalf("active counts after recovery = total=%d byOrigin=%v byTarget=%v, want all cleared", total, byOrigin, byTarget)
	}
}

func TestCheckDelegationCapsRefreshIsTimeBound(t *testing.T) {
	prevBudget := hubDelegationCapRefreshBudget
	prevTimeout := hubDelegationCapRefreshTimeout
	prevParallelism := hubDelegationCapRefreshParallelism
	prevStaleAfter := hubDelegationCapRefreshErrorStaleAfter
	hubDelegationCapRefreshBudget = 120 * time.Millisecond
	hubDelegationCapRefreshTimeout = 40 * time.Millisecond
	hubDelegationCapRefreshParallelism = 4
	hubDelegationCapRefreshErrorStaleAfter = 0
	defer func() {
		hubDelegationCapRefreshBudget = prevBudget
		hubDelegationCapRefreshTimeout = prevTimeout
		hubDelegationCapRefreshParallelism = prevParallelism
		hubDelegationCapRefreshErrorStaleAfter = prevStaleAfter
	}()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/chat/v2/runs/") {
			http.NotFound(w, r)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(200 * time.Millisecond):
			writeJSON(w, http.StatusOK, map[string]any{"id": strings.TrimPrefix(r.URL.Path, "/chat/v2/runs/"), "status": "running"})
		}
	}))
	defer slow.Close()

	nodes := []hub.Node{
		{ID: "alpha", Name: "Alpha", URL: slow.URL, BasePath: "/chat", Token: "alpha-token", Delegation: &hub.DelegationPolicy{Enabled: true, Workdir: "/work"}},
		{ID: "beta", Name: "Beta", URL: slow.URL, BasePath: "/chat", Token: "beta-token", Delegation: &hub.DelegationPolicy{Enabled: true, Workdir: "/work", MaxInFlight: 1}},
	}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: nodes}), nil)
	s.delegations = hub.NewDelegationStore(filepath.Join(t.TempDir(), "delegations.json"))

	now := time.Now().Add(-time.Minute).UTC()
	for i := 0; i < 8; i++ {
		if err := s.delegations.Add(hub.Delegation{
			ID:         fmt.Sprintf("dlg_slow_%d", i),
			OriginNode: "alpha",
			TargetNode: "beta",
			JobID:      fmt.Sprintf("job_%d", i),
			RunID:      fmt.Sprintf("run_%d", i),
			Status:     hub.DelegationStatusRunning,
			Depth:      1,
			Chain:      []string{"alpha", "beta"},
			CreatedAt:  now.Add(time.Duration(i) * time.Second),
			UpdatedAt:  now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("Add slow delegation %d: %v", i, err)
		}
	}

	origin, ok := s.registry.Lookup("alpha")
	if !ok {
		t.Fatal("origin node alpha missing from registry")
	}
	target, ok := s.registry.Lookup("beta")
	if !ok {
		t.Fatal("target node beta missing from registry")
	}

	start := time.Now()
	err := s.checkDelegationCaps(t.Context(), origin, target)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("checkDelegationCaps unexpectedly succeeded with all active records still running")
	}
	if elapsed >= 300*time.Millisecond {
		t.Fatalf("checkDelegationCaps took %v, want bounded parallel refresh under 300ms", elapsed)
	}
}

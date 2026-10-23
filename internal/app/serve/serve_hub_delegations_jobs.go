package serve

import (
	"context"
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

func jobsV2SessionName(job jobsV2Job) string {
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

// hubNodeJobsJob mirrors the jobs-v2 job fields the hub needs.
type hubNodeJobsJob struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Labels json.RawMessage `json:"labels"`
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
// error means the job is known, but no run could be confirmed.
func (s *hubServer) createDelegationJob(ctx context.Context, target hub.Node, d hub.Delegation, prompt string, timeoutSeconds int) (jobID, runID string, err error) {
	jobName := "hub-delegation-" + d.ID
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
		"name":               jobName,
		"enabled":            true,
		"runner_type":        "llm",
		"runner_config":      runnerConfig,
		"trigger_type":       "manual",
		"concurrency_policy": "allow",
		"timeout_seconds":    timeoutSeconds,
		"misfire_policy":     "run",
		"labels":             json.RawMessage(labels),
	}
	var job hubNodeJobsJob
	if err := s.doNodeJSON(ctx, target, http.MethodPost, "/v2/jobs", payload, &job); err != nil {
		// If the request reached the node but the response was lost (or a retry hit
		// the jobs-v2 unique name constraint), recover by name/label instead of
		// blindly creating another job.
		if existing, ok, lookupErr := s.findDelegationJob(ctx, target, jobName, d.ID); lookupErr == nil && ok {
			job = existing
		} else {
			if lookupErr != nil {
				return "", "", fmt.Errorf("create job: %w (idempotency lookup failed: %v)", err, lookupErr)
			}
			return "", "", fmt.Errorf("create job: %w", err)
		}
	}
	if job.ID == "" {
		return "", "", errors.New("target jobs API returned a job without an id")
	}
	var run hubNodeJobsRun
	if err := s.doNodeJSON(ctx, target, http.MethodPost, "/v2/jobs/"+url.PathEscape(job.ID)+"/trigger", map[string]any{}, &run); err != nil {
		// The trigger may have succeeded but the reverse/direct connection dropped
		// before the response made it back. Check the latest run for this job so the
		// ledger can resume exact-run polling instead of leaving an orphan.
		if latest, ok, lookupErr := s.findLatestDelegationRun(ctx, target, job.ID); lookupErr == nil && ok && latest.ID != "" {
			return job.ID, latest.ID, nil
		} else if lookupErr != nil {
			return job.ID, "", fmt.Errorf("trigger job %s: %w (run lookup failed: %v)", job.ID, err, lookupErr)
		}
		return job.ID, "", fmt.Errorf("trigger job %s: %w", job.ID, err)
	}
	if run.ID == "" {
		return job.ID, "", errors.New("target jobs API returned a run without an id")
	}
	return job.ID, run.ID, nil
}

func (s *hubServer) findDelegationJob(ctx context.Context, target hub.Node, name, delegationID string) (hubNodeJobsJob, bool, error) {
	var jobs struct {
		Data []hubNodeJobsJob `json:"data"`
	}
	if err := s.doNodeJSON(ctx, target, http.MethodGet, "/v2/jobs?limit=200&offset=0", nil, &jobs); err != nil {
		return hubNodeJobsJob{}, false, err
	}
	for _, job := range jobs.Data {
		if job.Name != name {
			continue
		}
		if hubDelegationIDFromJobLabels(job.Labels) != delegationID {
			continue
		}
		return job, true, nil
	}
	return hubNodeJobsJob{}, false, nil
}

func (s *hubServer) findLatestDelegationRun(ctx context.Context, target hub.Node, jobID string) (hubNodeJobsRun, bool, error) {
	var runs struct {
		Data []hubNodeJobsRun `json:"data"`
	}
	if err := s.doNodeJSON(ctx, target, http.MethodGet,
		"/v2/runs?limit=1&offset=0&job_id="+url.QueryEscape(jobID), nil, &runs); err != nil {
		return hubNodeJobsRun{}, false, err
	}
	if len(runs.Data) == 0 {
		return hubNodeJobsRun{}, false, nil
	}
	return runs.Data[0], true, nil
}

var hubNodeJSONTimeout = 30 * time.Second

// doNodeJSON performs one JSON request against a node's API (mounted under
// the node's base path) with the node's token injected server-side. It uses
// the hub's direct-dial client: routing token-carrying requests through an
// environment HTTP proxy would leak node tokens.
func (s *hubServer) doNodeJSON(ctx context.Context, node hub.Node, method, path string, payload, out any, headers ...http.Header) error {
	// Unlike proxy streams, JSON operations have a finite whole-response budget
	// on both transports, including body decoding. WithTimeout preserves an
	// earlier caller deadline.
	ctx, cancel := context.WithTimeout(ctx, hubNodeJSONTimeout)
	defer cancel()

	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode node request: %w", err)
		}
		body = strings.NewReader(string(data))
	}
	var targetURL string
	if node.UsesReverseConnection() {
		targetURL = "http://reverse.local" + node.BasePath + path
	} else {
		targetURL = node.BaseURL() + path
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, header := range headers {
		for key, values := range header {
			req.Header[key] = append([]string(nil), values...)
		}
	}
	if node.Token != "" {
		req.Header.Set("Authorization", "Bearer "+node.Token)
	}
	var resp *http.Response
	if node.UsesReverseConnection() {
		resp, err = s.reverse.do(ctx, node, req)
	} else {
		resp, err = s.nodeAPIClient.Do(req)
	}
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

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/samsaffron/term-llm/internal/llm"
)

// hub_delegate / hub_check_delegation let an agent on one term-llm node run
// work on another node via the Hub. The node authenticates to the Hub with
// its OWN serve token (the Hub verifies it against the node's stored token);
// it never sees other nodes' tokens. Configuration is in-process (see
// hub_env.go): serve hands it over with ConfigureHubDelegation, or a
// standalone process exports TERM_LLM_HUB_URL / TERM_LLM_HUB_NODE_ID /
// TERM_LLM_HUB_TOKEN, which are captured at startup — the token is then
// scrubbed from the environment so tool subprocesses never inherit it.

const defaultHubDelegationPollInterval = 5 * time.Second

type HubDelegateArgs struct {
	TargetNode         string `json:"target_node"`
	Prompt             string `json:"prompt"`
	AgentName          string `json:"agent_name,omitempty"`
	TimeoutSeconds     int    `json:"timeout_seconds,omitempty"`
	Model              string `json:"model,omitempty"`
	Cwd                string `json:"cwd,omitempty"`
	Wait               bool   `json:"wait,omitempty"`
	ParentDelegationID string `json:"parent_delegation_id,omitempty"`
}

type HubCheckDelegationArgs struct {
	DelegationIDs       []string `json:"delegation_ids"`
	Wait                *bool    `json:"wait,omitempty"`
	PollIntervalSeconds int      `json:"poll_interval_seconds,omitempty"`
}

// HubDelegationResult is the tool-facing view of one delegation.
type HubDelegationResult struct {
	DelegationID string `json:"delegation_id"`
	TargetNode   string `json:"target_node,omitempty"`
	Status       string `json:"status"`
	Response     string `json:"response,omitempty"`
	Error        string `json:"error,omitempty"`
	// RefreshError surfaces a hub-side polling problem (e.g. target node
	// unreachable) without failing the whole check.
	RefreshError string `json:"refresh_error,omitempty"`
}

func hubDelegationResultFrom(d hub.Delegation, refreshError string) HubDelegationResult {
	return HubDelegationResult{
		DelegationID: d.ID,
		TargetNode:   d.TargetNode,
		Status:       d.Status,
		Response:     d.Response,
		Error:        d.Error,
		RefreshError: refreshError,
	}
}

// hubDelegationClient talks to the Hub's delegation API as this node.
type hubDelegationClient struct {
	hubURL     string
	nodeID     string
	token      string
	httpClient *http.Client
}

// newHubDelegationClient builds the client from the in-process hub config
// (serve flags or TERM_LLM_HUB_* captured at startup) or returns a clean
// error naming every missing piece by its environment variable.
func newHubDelegationClient() (*hubDelegationClient, error) {
	hubURL, nodeID, token := hubDelegationConfig()
	var missing []string
	if hubURL == "" {
		missing = append(missing, "TERM_LLM_HUB_URL")
	}
	if nodeID == "" {
		missing = append(missing, "TERM_LLM_HUB_NODE_ID")
	}
	if token == "" {
		missing = append(missing, "TERM_LLM_HUB_TOKEN")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("hub delegation is not configured on this node: missing %s (set automatically by 'serve web --hub-url --hub-node-id', or export them before the process starts)", strings.Join(missing, ", "))
	}
	return &hubDelegationClient{
		hubURL: strings.TrimRight(hubURL, "/"),
		nodeID: nodeID,
		token:  token,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
			// No environment proxy: every request carries this node's serve
			// token, and routing it through an HTTP_PROXY would leak it.
			Transport: &http.Transport{Proxy: nil},
		},
	}, nil
}

func (c *hubDelegationClient) doJSON(ctx context.Context, method, path string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode hub request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.hubURL+path, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Term-LLM-Node-ID", c.nodeID)
	resp, err := c.httpClient.Do(req)
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
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("hub %s %s failed: HTTP %d: %s", method, path, resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode hub response: %w", err)
	}
	return nil
}

// hubDelegationEnvelope is the hub's {"delegation": ..., "refresh_error":...}
// response shape.
type hubDelegationEnvelope struct {
	Delegation   hub.Delegation `json:"delegation"`
	RefreshError string         `json:"refresh_error"`
}

func (c *hubDelegationClient) createDelegation(ctx context.Context, a HubDelegateArgs) (hub.Delegation, error) {
	payload := map[string]any{
		"target_node": a.TargetNode,
		"prompt":      a.Prompt,
	}
	if a.AgentName != "" {
		payload["agent_name"] = a.AgentName
	}
	if a.TimeoutSeconds > 0 {
		payload["timeout_seconds"] = a.TimeoutSeconds
	}
	if a.Model != "" {
		payload["model"] = a.Model
	}
	if a.Cwd != "" {
		payload["cwd"] = a.Cwd
	}
	if a.ParentDelegationID != "" {
		payload["parent_delegation_id"] = a.ParentDelegationID
	}
	var envelope hubDelegationEnvelope
	if err := c.doJSON(ctx, http.MethodPost, "/api/delegations", payload, &envelope); err != nil {
		return hub.Delegation{}, err
	}
	if envelope.Delegation.ID == "" {
		return hub.Delegation{}, fmt.Errorf("hub returned a delegation without an id")
	}
	return envelope.Delegation, nil
}

func (c *hubDelegationClient) getDelegation(ctx context.Context, id string) (hub.Delegation, string, error) {
	var envelope hubDelegationEnvelope
	if err := c.doJSON(ctx, http.MethodGet, "/api/delegations/"+url.PathEscape(id), nil, &envelope); err != nil {
		return hub.Delegation{}, "", err
	}
	return envelope.Delegation, envelope.RefreshError, nil
}

func (c *hubDelegationClient) waitForDelegation(ctx context.Context, id string, pollInterval time.Duration) (hub.Delegation, string, error) {
	if pollInterval <= 0 {
		pollInterval = defaultHubDelegationPollInterval
	}
	for {
		d, refreshErr, err := c.getDelegation(ctx, id)
		if err != nil {
			return hub.Delegation{}, "", err
		}
		if hub.DelegationStatusTerminal(d.Status) {
			return d, refreshErr, nil
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return d, refreshErr, ctx.Err()
		case <-timer.C:
		}
	}
}

// resolveClient returns the injected test client or builds one from the
// in-process config.
func resolveHubDelegationClient(injected *hubDelegationClient) (*hubDelegationClient, error) {
	if injected != nil {
		return injected, nil
	}
	return newHubDelegationClient()
}

type HubDelegateTool struct {
	client *hubDelegationClient
	// pollIntervalOverride lets tests poll sub-second.
	pollIntervalOverride time.Duration
}

func NewHubDelegateTool() *HubDelegateTool { return &HubDelegateTool{} }

func NewHubDelegateToolWithClient(client *hubDelegationClient) *HubDelegateTool {
	return &HubDelegateTool{client: client}
}

func (t *HubDelegateTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        HubDelegateToolName,
		Description: `Delegate a task to another term-llm node via the Hub. The Hub runs the prompt as a background agent job on the target node and returns a delegation_id; use hub_check_delegation to retrieve the result (or set wait=true to block).`,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target_node": map[string]any{
					"type":        "string",
					"description": "Hub node id to delegate to",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "The task for the remote agent to execute",
				},
				"agent_name": map[string]any{
					"type":        "string",
					"description": "Agent to run on the target node (default 'developer')",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"description": "Job timeout on the target node in seconds (default 3600, max 3600)",
					"minimum":     10,
					"maximum":     3600,
				},
				"model": map[string]any{
					"type":        "string",
					"description": "Optional model override for the remote agent",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional working directory on the target node; must be inside the target's delegation workdir (default: the workdir itself)",
				},
				"wait": map[string]any{
					"type":        "boolean",
					"description": "When true, block until the delegation finishes and return the result. Defaults to false.",
				},
				"parent_delegation_id": map[string]any{
					"type":        "string",
					"description": "Set automatically when this task runs inside a delegated hub job; only provide it when chaining manually outside one (the hub verifies it either way)",
				},
			},
			"required":             []string{"target_node", "prompt"},
			"additionalProperties": false,
		},
	}
}

func (t *HubDelegateTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a HubDelegateArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(formatQueuedAgentError(ErrInvalidParams, fmt.Sprintf("failed to parse arguments: %v", err))), nil
	}
	if strings.TrimSpace(a.TargetNode) == "" {
		return llm.TextOutput(formatQueuedAgentError(ErrInvalidParams, "target_node is required")), nil
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return llm.TextOutput(formatQueuedAgentError(ErrInvalidParams, "prompt is required")), nil
	}
	// A trusted delegation id on the context (set by the jobs-v2 runner from
	// the hub-written job label) always wins over the model-provided argument:
	// chained delegations must count against depth/loop limits.
	if id := HubDelegationIDFromContext(ctx); id != "" {
		a.ParentDelegationID = id
	}
	client, err := resolveHubDelegationClient(t.client)
	if err != nil {
		return llm.TextOutput(formatQueuedAgentError(ErrExecutionFailed, err.Error())), nil
	}
	d, err := client.createDelegation(ctx, a)
	if err != nil {
		return llm.TextOutput(formatQueuedAgentError(ErrExecutionFailed, err.Error())), nil
	}
	refreshErr := ""
	if a.Wait {
		d, refreshErr, err = client.waitForDelegation(ctx, d.ID, t.pollIntervalOverride)
		if err != nil {
			return llm.TextOutput(formatQueuedAgentError(ErrExecutionFailed, fmt.Sprintf("delegation %s created but waiting failed: %v", d.ID, err))), nil
		}
	}
	data, _ := json.Marshal(hubDelegationResultFrom(d, refreshErr))
	return llm.TextOutput(string(data)), nil
}

func (t *HubDelegateTool) Preview(args json.RawMessage) string {
	var a HubDelegateArgs
	_ = json.Unmarshal(args, &a)
	if a.TargetNode == "" {
		return "hub delegate"
	}
	return fmt.Sprintf("delegate to %s", a.TargetNode)
}

type HubCheckDelegationTool struct {
	client *hubDelegationClient
	// pollIntervalOverride lets tests poll sub-second; poll_interval_seconds
	// has a 1s floor.
	pollIntervalOverride time.Duration
}

func NewHubCheckDelegationTool() *HubCheckDelegationTool { return &HubCheckDelegationTool{} }

func NewHubCheckDelegationToolWithClient(client *hubDelegationClient) *HubCheckDelegationTool {
	return &HubCheckDelegationTool{client: client}
}

func (t *HubCheckDelegationTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        HubCheckDelegationToolName,
		Description: `Check (and by default wait for) hub delegations started with hub_delegate, returning their status and final responses.`,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"delegation_ids": map[string]any{
					"type":        "array",
					"description": "Delegation IDs returned by hub_delegate",
					"items":       map[string]any{"type": "string"},
					"minItems":    1,
				},
				"wait": map[string]any{
					"type":        "boolean",
					"description": "Wait for the delegations to finish (default true). When false, return the current status immediately.",
				},
				"poll_interval_seconds": map[string]any{
					"type":        "integer",
					"description": "How often to poll the hub while waiting (default 5)",
					"minimum":     1,
				},
			},
			"required":             []string{"delegation_ids"},
			"additionalProperties": false,
		},
	}
}

func (t *HubCheckDelegationTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a HubCheckDelegationArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(formatQueuedAgentError(ErrInvalidParams, fmt.Sprintf("failed to parse arguments: %v", err))), nil
	}
	if len(a.DelegationIDs) == 0 {
		return llm.TextOutput(formatQueuedAgentError(ErrInvalidParams, "delegation_ids is required")), nil
	}
	client, err := resolveHubDelegationClient(t.client)
	if err != nil {
		return llm.TextOutput(formatQueuedAgentError(ErrExecutionFailed, err.Error())), nil
	}
	wait := a.Wait == nil || *a.Wait
	pollInterval := time.Duration(a.PollIntervalSeconds) * time.Second
	if t.pollIntervalOverride > 0 {
		pollInterval = t.pollIntervalOverride
	}
	results := make([]HubDelegationResult, 0, len(a.DelegationIDs))
	for _, id := range a.DelegationIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		var (
			d          hub.Delegation
			refreshErr string
			err        error
		)
		if wait {
			d, refreshErr, err = client.waitForDelegation(ctx, id, pollInterval)
		} else {
			d, refreshErr, err = client.getDelegation(ctx, id)
		}
		if err != nil {
			results = append(results, HubDelegationResult{DelegationID: id, Status: "unknown", Error: err.Error()})
			continue
		}
		results = append(results, hubDelegationResultFrom(d, refreshErr))
	}
	data, _ := json.Marshal(map[string]any{"delegations": results})
	return llm.TextOutput(string(data)), nil
}

func (t *HubCheckDelegationTool) Preview(args json.RawMessage) string {
	var a HubCheckDelegationArgs
	_ = json.Unmarshal(args, &a)
	return fmt.Sprintf("check %d delegation(s)", len(a.DelegationIDs))
}

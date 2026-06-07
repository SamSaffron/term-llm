package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

const (
	defaultQueuedAgentTimeout      = 3600
	defaultQueuedAgentPollInterval = 5
	defaultJobsServerBaseURL       = "http://127.0.0.1:8080"
)

type QueueAgentArgs struct {
	AgentName      string `json:"agent,omitempty"`
	AgentNameAlias string `json:"agent_name,omitempty"`
	Prompt         string `json:"prompt"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	Model          string `json:"model,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
}

type QueueAgentResult struct {
	JobID     string `json:"job_id"`
	RunID     string `json:"run_id"`
	AgentName string `json:"agent"`
}

type WaitForAgentArgs struct {
	PollIntervalSeconds int      `json:"poll_interval_seconds,omitempty"`
	RunIDs              []string `json:"run_ids"`
}

type QueuedAgentRunResult struct {
	RunID           string   `json:"run_id"`
	JobID           string   `json:"job_id,omitempty"`
	Status          string   `json:"status"`
	ExitReason      string   `json:"exit_reason,omitempty"`
	Truncated       bool     `json:"truncated,omitempty"`
	TurnCount       *int     `json:"turn_count,omitempty"`
	InputTokens     *int     `json:"input_tokens,omitempty"`
	OutputTokens    *int     `json:"output_tokens,omitempty"`
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
	Response        string   `json:"response,omitempty"`
	Stdout          string   `json:"stdout,omitempty"`
	Error           string   `json:"error,omitempty"`
	ExitCode        *int     `json:"exit_code,omitempty"`
	StartedAt       string   `json:"started_at,omitempty"`
	FinishedAt      string   `json:"finished_at,omitempty"`
}

type jobsBackedAgentClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

type jobsV2AgentJobPayload struct {
	Name              string         `json:"name"`
	Enabled           bool           `json:"enabled"`
	RunnerType        string         `json:"runner_type"`
	RunnerConfig      map[string]any `json:"runner_config"`
	TriggerType       string         `json:"trigger_type"`
	TriggerConfig     map[string]any `json:"trigger_config,omitempty"`
	ConcurrencyPolicy string         `json:"concurrency_policy"`
	TimeoutSeconds    int            `json:"timeout_seconds"`
	MisfirePolicy     string         `json:"misfire_policy"`
}

type jobsV2AgentJobResponse struct {
	ID string `json:"id"`
}

type jobsV2AgentRunResponse struct {
	ID           string `json:"id"`
	JobID        string `json:"job_id"`
	Status       string `json:"status"`
	ExitReason   string `json:"exit_reason"`
	Truncated    bool   `json:"truncated"`
	TurnCount    *int   `json:"turn_count"`
	InputTokens  *int   `json:"input_tokens"`
	OutputTokens *int   `json:"output_tokens"`
	Response     string `json:"response"`
	Stdout       string `json:"stdout"`
	Error        string `json:"error"`
	ExitCode     *int   `json:"exit_code"`
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at"`
}

type jobsV2ErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type QueueAgentTool struct {
	client *jobsBackedAgentClient
}

func NewQueueAgentTool() *QueueAgentTool {
	return NewQueueAgentToolWithClient(newJobsBackedAgentClientFromEnv())
}

func NewQueueAgentToolWithClient(client *jobsBackedAgentClient) *QueueAgentTool {
	return &QueueAgentTool{client: client}
}

func (t *QueueAgentTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        QueueAgentToolName,
		Description: `Spawn a sub-agent as a background jobs-v2 LLM run and return immediately. Use wait_for_agent to retrieve the result later.`,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent": map[string]any{
					"type":        "string",
					"description": "Agent name, e.g. developer, reviewer, codebase, web-researcher, or jarvis",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "Instructions for the sub-agent",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"description": "Maximum runtime in seconds (default 3600)",
					"minimum":     10,
				},
				"model": map[string]any{
					"type":        "string",
					"description": "Optional model override for this sub-agent call",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional working directory/root for the jobs v2 LLM run. Defaults to TERM_LLM_QUEUE_AGENT_CWD or the current process directory.",
				},
			},
			"required":             []string{"agent", "prompt"},
			"additionalProperties": false,
		},
	}
}

func (t *QueueAgentTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a QueueAgentArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(formatQueuedAgentError(ErrInvalidParams, fmt.Sprintf("failed to parse arguments: %v", err))), nil
	}
	agentName := strings.TrimSpace(a.AgentName)
	if agentName == "" {
		agentName = strings.TrimSpace(a.AgentNameAlias)
	}
	if agentName == "" {
		return llm.TextOutput(formatQueuedAgentError(ErrInvalidParams, "agent is required")), nil
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return llm.TextOutput(formatQueuedAgentError(ErrInvalidParams, "prompt is required")), nil
	}
	timeout := a.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultQueuedAgentTimeout
	}
	if timeout < 10 {
		timeout = 10
	}
	cwd, err := queueAgentCwd(a.Cwd)
	if err != nil {
		return llm.TextOutput(formatQueuedAgentError(ErrInvalidParams, err.Error())), nil
	}

	job, err := t.client.createAgentJob(ctx, agentName, a.Prompt, strings.TrimSpace(a.Model), cwd, timeout)
	if err != nil {
		return llm.TextOutput(formatQueuedAgentError(ErrExecutionFailed, err.Error())), nil
	}
	run, err := t.client.triggerJob(ctx, job.ID)
	if err != nil {
		return llm.TextOutput(formatQueuedAgentError(ErrExecutionFailed, err.Error())), nil
	}

	data, _ := json.Marshal(QueueAgentResult{JobID: job.ID, RunID: run.ID, AgentName: agentName})
	return llm.TextOutput(string(data)), nil
}

func (t *QueueAgentTool) Preview(args json.RawMessage) string {
	var a QueueAgentArgs
	_ = json.Unmarshal(args, &a)
	agentName := a.AgentName
	if agentName == "" {
		agentName = a.AgentNameAlias
	}
	if agentName == "" {
		return "queue agent"
	}
	return fmt.Sprintf("queue %s", agentName)
}

type WaitForAgentTool struct {
	client *jobsBackedAgentClient
}

func NewWaitForAgentTool() *WaitForAgentTool {
	return NewWaitForAgentToolWithClient(newJobsBackedAgentClientFromEnv())
}

func NewWaitForAgentToolWithClient(client *jobsBackedAgentClient) *WaitForAgentTool {
	return &WaitForAgentTool{client: client}
}

func (t *WaitForAgentTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        WaitForAgentToolName,
		Description: `Wait for one or more queued agent jobs-v2 runs to finish and return their results.`,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"poll_interval_seconds": map[string]any{
					"type":        "integer",
					"description": "How often to poll run status (default 5)",
					"minimum":     1,
				},
				"run_ids": map[string]any{
					"type":        "array",
					"description": "Run IDs returned by queue_agent",
					"items":       map[string]any{"type": "string"},
					"minItems":    1,
				},
			},
			"required":             []string{"run_ids"},
			"additionalProperties": false,
		},
	}
}

func (t *WaitForAgentTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a WaitForAgentArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(formatQueuedAgentError(ErrInvalidParams, fmt.Sprintf("failed to parse arguments: %v", err))), nil
	}
	if len(a.RunIDs) == 0 {
		return llm.TextOutput(formatQueuedAgentError(ErrInvalidParams, "run_ids is required")), nil
	}
	pollInterval := a.PollIntervalSeconds
	if pollInterval <= 0 {
		pollInterval = defaultQueuedAgentPollInterval
	}

	results := make([]QueuedAgentRunResult, 0, len(a.RunIDs))
	for _, runID := range a.RunIDs {
		runID = strings.TrimSpace(runID)
		if runID == "" {
			results = append(results, QueuedAgentRunResult{Status: "not_found", Error: "blank run_id"})
			continue
		}
		run, err := t.client.waitForRun(ctx, runID, time.Duration(pollInterval)*time.Second)
		if err != nil {
			results = append(results, QueuedAgentRunResult{RunID: runID, Status: "failed", Error: err.Error()})
			continue
		}
		results = append(results, queuedAgentRunResultFromJobsRun(run))
	}
	data, _ := json.Marshal(results)
	return llm.TextOutput(string(data)), nil
}

func (t *WaitForAgentTool) Preview(args json.RawMessage) string {
	var a WaitForAgentArgs
	_ = json.Unmarshal(args, &a)
	return fmt.Sprintf("wait for %d agent run(s)", len(a.RunIDs))
}

func newJobsBackedAgentClientFromEnv() *jobsBackedAgentClient {
	baseURL := strings.TrimSpace(os.Getenv("TERM_LLM_JOBS_SERVER"))
	if baseURL == "" {
		baseURL = defaultJobsServerBaseURL
	}
	baseURL = strings.TrimRight(strings.TrimSuffix(strings.TrimSuffix(baseURL, "/ui"), "/chat"), "/")
	return &jobsBackedAgentClient{
		baseURL:    baseURL,
		token:      strings.TrimSpace(os.Getenv("TERM_LLM_JOBS_TOKEN")),
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *jobsBackedAgentClient) createAgentJob(ctx context.Context, agentName, prompt, model, cwd string, timeout int) (jobsV2AgentJobResponse, error) {
	instructions := prompt + `

---
Before your final message, state your completion status on its own line in this exact format:
STATUS: COMPLETE
or
STATUS: BLOCKED — <brief reason you could not complete the task>
or
STATUS: PARTIAL — <what was done and what is still missing>

Choose COMPLETE only if you fully accomplished the task. Do not omit this line.`

	runnerConfig := map[string]any{
		"agent_name":   agentName,
		"instructions": instructions,
		"cwd":          cwd,
	}
	if model != "" {
		runnerConfig["model"] = model
	}

	payload := jobsV2AgentJobPayload{
		Name:              fmt.Sprintf("agent-%s-%d", sanitizeJobNamePart(agentName), time.Now().UnixNano()),
		Enabled:           true,
		RunnerType:        "llm",
		RunnerConfig:      runnerConfig,
		TriggerType:       "manual",
		ConcurrencyPolicy: "allow",
		TimeoutSeconds:    timeout,
		MisfirePolicy:     "run",
	}

	var job jobsV2AgentJobResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v2/jobs", payload, &job); err != nil {
		return jobsV2AgentJobResponse{}, err
	}
	if job.ID == "" {
		return jobsV2AgentJobResponse{}, fmt.Errorf("jobs server returned job without id")
	}
	return job, nil
}

func (c *jobsBackedAgentClient) triggerJob(ctx context.Context, jobID string) (jobsV2AgentRunResponse, error) {
	var run jobsV2AgentRunResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v2/jobs/"+jobID+"/trigger", map[string]any{}, &run); err != nil {
		return jobsV2AgentRunResponse{}, err
	}
	if run.ID == "" {
		return jobsV2AgentRunResponse{}, fmt.Errorf("jobs server returned run without id")
	}
	return run, nil
}

func (c *jobsBackedAgentClient) waitForRun(ctx context.Context, runID string, pollInterval time.Duration) (jobsV2AgentRunResponse, error) {
	if pollInterval <= 0 {
		pollInterval = defaultQueuedAgentPollInterval * time.Second
	}
	for {
		run, err := c.getRun(ctx, runID)
		if err != nil {
			return jobsV2AgentRunResponse{}, err
		}
		if isQueuedAgentTerminalStatus(run.Status) {
			return run, nil
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return run, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *jobsBackedAgentClient) getRun(ctx context.Context, runID string) (jobsV2AgentRunResponse, error) {
	var run jobsV2AgentRunResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v2/runs/"+runID, nil, &run); err != nil {
		return jobsV2AgentRunResponse{}, err
	}
	if run.ID == "" {
		run.ID = runID
	}
	return run, nil
}

func (c *jobsBackedAgentClient) doJSON(ctx context.Context, method, path string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode jobs request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("jobs %s %s failed: %s", method, path, jobsErrorMessage(resp.StatusCode, data))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode jobs response: %w", err)
	}
	return nil
}

func jobsErrorMessage(statusCode int, data []byte) string {
	var errResp jobsV2ErrorResponse
	if err := json.Unmarshal(data, &errResp); err == nil && errResp.Error.Message != "" {
		if errResp.Error.Type != "" {
			return fmt.Sprintf("HTTP %d: %s (%s)", statusCode, errResp.Error.Message, errResp.Error.Type)
		}
		return fmt.Sprintf("HTTP %d: %s", statusCode, errResp.Error.Message)
	}
	body := strings.TrimSpace(string(data))
	if body == "" {
		return fmt.Sprintf("HTTP %d", statusCode)
	}
	return fmt.Sprintf("HTTP %d: %s", statusCode, body)
}

func queueAgentCwd(explicit string) (string, error) {
	cwd := strings.TrimSpace(explicit)
	if cwd == "" {
		cwd = strings.TrimSpace(os.Getenv("TERM_LLM_QUEUE_AGENT_CWD"))
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve cwd: %w", err)
		}
	}
	if cwd == "" {
		return "", fmt.Errorf("cwd is required")
	}
	return cwd, nil
}

func queuedAgentRunResultFromJobsRun(run jobsV2AgentRunResponse) QueuedAgentRunResult {
	return QueuedAgentRunResult{
		RunID:           run.ID,
		JobID:           run.JobID,
		Status:          run.Status,
		ExitReason:      run.ExitReason,
		Truncated:       run.Truncated,
		TurnCount:       run.TurnCount,
		InputTokens:     run.InputTokens,
		OutputTokens:    run.OutputTokens,
		DurationSeconds: durationSeconds(run.StartedAt, run.FinishedAt),
		Response:        run.Response,
		Stdout:          run.Stdout,
		Error:           run.Error,
		ExitCode:        run.ExitCode,
		StartedAt:       run.StartedAt,
		FinishedAt:      run.FinishedAt,
	}
}

func durationSeconds(started, finished string) *float64 {
	if started == "" || finished == "" {
		return nil
	}
	start, err := time.Parse(time.RFC3339Nano, started)
	if err != nil {
		return nil
	}
	finish, err := time.Parse(time.RFC3339Nano, finished)
	if err != nil {
		return nil
	}
	seconds := finish.Sub(start).Seconds()
	return &seconds
}

func isQueuedAgentTerminalStatus(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "timed_out":
		return true
	default:
		return false
	}
}

func sanitizeJobNamePart(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	result := strings.Trim(b.String(), "-_")
	if result == "" {
		return "agent"
	}
	return result
}

func formatQueuedAgentError(errType ToolErrorType, message string) string {
	data, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
	return string(data)
}

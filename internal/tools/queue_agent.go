package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
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
	AgentName      string `json:"agent_name"`
	Prompt         string `json:"prompt"`
	Timeout        int    `json:"timeout,omitempty"`
	Model          string `json:"model,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	NotifyWhenDone bool   `json:"notify_when_done,omitempty"`
}

type QueueAgentResult struct {
	JobID     string `json:"job_id"`
	AgentName string `json:"agent_name"`
}

type WaitForJobsArgs struct {
	PollIntervalSeconds int      `json:"poll_interval_seconds,omitempty"`
	JobIDs              []string `json:"job_ids"`
}

type QueuedJobResult struct {
	JobID           string   `json:"job_id"`
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

type jobsV2AgentRunsListResponse struct {
	Data []jobsV2AgentRunResponse `json:"data"`
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
		Description: `Spawn a sub-agent as a background jobs-v2 LLM job and return immediately. Use wait_for_jobs with the returned job_id to retrieve the result later.`,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_name": map[string]any{
					"type":        "string",
					"description": "Name of the agent to queue (e.g., 'developer', 'codebase', 'web-researcher'). Use 'codebase' for local source code questions; 'web-researcher' for external web research only.",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "The task or prompt for the sub-agent to execute",
				},
				"timeout": map[string]any{
					"type":        "integer",
					"description": "Optional timeout in seconds (default 3600, max 3600)",
					"minimum":     10,
					"maximum":     3600,
				},
				"model": map[string]any{
					"type":        "string",
					"description": "Optional model override for this sub-agent call. Supports a model name, an alias, or provider:model format.",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional working directory/root for the jobs-v2 LLM job. Defaults to TERM_LLM_QUEUE_AGENT_CWD or the current process directory.",
				},
				"notify_when_done": map[string]any{
					"type":        "boolean",
					"description": "When true, notify this request's originating session or chat when the queued job finishes. Defaults to false.",
				},
			},
			"required":             []string{"agent_name", "prompt"},
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
		return llm.TextOutput(formatQueuedAgentError(ErrInvalidParams, "agent_name is required")), nil
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return llm.TextOutput(formatQueuedAgentError(ErrInvalidParams, "prompt is required")), nil
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = defaultQueuedAgentTimeout
	}
	if timeout < 10 {
		timeout = 10
	}
	if timeout > defaultQueuedAgentTimeout {
		timeout = defaultQueuedAgentTimeout
	}
	cwd, err := queueAgentCwd(a.Cwd)
	if err != nil {
		return llm.TextOutput(formatQueuedAgentError(ErrInvalidParams, err.Error())), nil
	}

	origin, _ := QueueAgentOriginFromContext(ctx)
	job, err := t.client.createAgentJob(ctx, agentName, a.Prompt, strings.TrimSpace(a.Model), cwd, timeout, a.NotifyWhenDone, origin)
	if err != nil {
		return llm.TextOutput(formatQueuedAgentError(ErrExecutionFailed, err.Error())), nil
	}
	if _, err := t.client.triggerJob(ctx, job.ID); err != nil {
		return llm.TextOutput(formatQueuedAgentError(ErrExecutionFailed, err.Error())), nil
	}

	data, _ := json.Marshal(QueueAgentResult{JobID: job.ID, AgentName: agentName})
	return llm.TextOutput(string(data)), nil
}

func (t *QueueAgentTool) Preview(args json.RawMessage) string {
	var a QueueAgentArgs
	_ = json.Unmarshal(args, &a)
	agentName := a.AgentName
	if agentName == "" {
		return "queue agent"
	}
	return fmt.Sprintf("queue %s", agentName)
}

type WaitForJobsTool struct {
	client *jobsBackedAgentClient
}

func NewWaitForJobsTool() *WaitForJobsTool {
	return NewWaitForJobsToolWithClient(newJobsBackedAgentClientFromEnv())
}

func NewWaitForJobsToolWithClient(client *jobsBackedAgentClient) *WaitForJobsTool {
	return &WaitForJobsTool{client: client}
}

func (t *WaitForJobsTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        WaitForJobsToolName,
		Description: `Wait for one or more queued jobs-v2 agent jobs to finish and return their results. Use the job_id values returned by queue_agent.`,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"poll_interval_seconds": map[string]any{
					"type":        "integer",
					"description": "How often to poll job status (default 5)",
					"minimum":     1,
				},
				"job_ids": map[string]any{
					"type":        "array",
					"description": "Job IDs returned by queue_agent",
					"items":       map[string]any{"type": "string"},
					"minItems":    1,
				},
			},
			"required":             []string{"job_ids"},
			"additionalProperties": false,
		},
	}
}

func (t *WaitForJobsTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a WaitForJobsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(formatQueuedAgentError(ErrInvalidParams, fmt.Sprintf("failed to parse arguments: %v", err))), nil
	}
	if len(a.JobIDs) == 0 {
		return llm.TextOutput(formatQueuedAgentError(ErrInvalidParams, "job_ids is required")), nil
	}
	pollInterval := a.PollIntervalSeconds
	if pollInterval <= 0 {
		pollInterval = defaultQueuedAgentPollInterval
	}

	results := make([]QueuedJobResult, 0, len(a.JobIDs))
	for _, jobID := range a.JobIDs {
		jobID = strings.TrimSpace(jobID)
		if jobID == "" {
			results = append(results, QueuedJobResult{Status: "not_found", Error: "blank job_id"})
			continue
		}
		run, err := t.client.waitForJob(ctx, jobID, time.Duration(pollInterval)*time.Second)
		if err != nil {
			results = append(results, QueuedJobResult{JobID: jobID, Status: "failed", Error: err.Error()})
			continue
		}
		results = append(results, queuedJobResultFromJobsRun(jobID, run))
	}
	data, _ := json.Marshal(results)
	return llm.TextOutput(string(data)), nil
}

func (t *WaitForJobsTool) Preview(args json.RawMessage) string {
	var a WaitForJobsArgs
	_ = json.Unmarshal(args, &a)
	return fmt.Sprintf("wait for %d job(s)", len(a.JobIDs))
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

func (c *jobsBackedAgentClient) createAgentJob(ctx context.Context, agentName, prompt, model, cwd string, timeout int, notifyWhenDone bool, origin QueueAgentOriginContext) (jobsV2AgentJobResponse, error) {
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
	requestHeaders := map[string]string(nil)
	if notifyWhenDone {
		runnerConfig["notify_when_done"] = true
		requestHeaders = queueAgentNotifyOriginHeaders(origin)
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
	if err := c.doJSONWithHeaders(ctx, http.MethodPost, "/v2/jobs", payload, &job, requestHeaders); err != nil {
		return jobsV2AgentJobResponse{}, err
	}
	if job.ID == "" {
		return jobsV2AgentJobResponse{}, fmt.Errorf("jobs server returned job without id")
	}
	return job, nil
}

func queueAgentNotifyOriginHeaders(origin QueueAgentOriginContext) map[string]string {
	origin.Origin = strings.TrimSpace(origin.Origin)
	origin.SessionID = strings.TrimSpace(origin.SessionID)
	if origin.Origin == "" {
		return nil
	}
	headers := map[string]string{
		QueueAgentNotifyOriginHeader: origin.Origin,
	}
	if origin.SessionID != "" {
		headers[QueueAgentNotifySessionIDHeader] = origin.SessionID
	}
	if origin.TelegramChatID != 0 {
		headers[QueueAgentNotifyTelegramChatIDHeader] = strconv.FormatInt(origin.TelegramChatID, 10)
	}
	return headers
}

func (c *jobsBackedAgentClient) triggerJob(ctx context.Context, jobID string) (jobsV2AgentRunResponse, error) {
	var run jobsV2AgentRunResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v2/jobs/"+url.PathEscape(jobID)+"/trigger", map[string]any{}, &run); err != nil {
		return jobsV2AgentRunResponse{}, err
	}
	if run.ID == "" {
		return jobsV2AgentRunResponse{}, fmt.Errorf("jobs server returned run without id")
	}
	return run, nil
}

func (c *jobsBackedAgentClient) waitForJob(ctx context.Context, jobID string, pollInterval time.Duration) (jobsV2AgentRunResponse, error) {
	if pollInterval <= 0 {
		pollInterval = defaultQueuedAgentPollInterval * time.Second
	}
	for {
		run, found, err := c.latestRunForJob(ctx, jobID)
		if err != nil {
			return jobsV2AgentRunResponse{}, err
		}
		if found && isQueuedAgentTerminalStatus(run.Status) {
			return run, nil
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if found {
				return run, ctx.Err()
			}
			return jobsV2AgentRunResponse{JobID: jobID}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *jobsBackedAgentClient) latestRunForJob(ctx context.Context, jobID string) (jobsV2AgentRunResponse, bool, error) {
	var runs jobsV2AgentRunsListResponse
	path := "/v2/runs?limit=1&offset=0&job_id=" + url.QueryEscape(jobID)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &runs); err != nil {
		return jobsV2AgentRunResponse{}, false, err
	}
	if len(runs.Data) == 0 {
		return jobsV2AgentRunResponse{}, false, nil
	}
	run := runs.Data[0]
	if run.JobID == "" {
		run.JobID = jobID
	}
	return run, true, nil
}

func (c *jobsBackedAgentClient) doJSON(ctx context.Context, method, path string, payload any, out any) error {
	return c.doJSONWithHeaders(ctx, method, path, payload, out, nil)
}

func (c *jobsBackedAgentClient) doJSONWithHeaders(ctx context.Context, method, path string, payload any, out any, headers map[string]string) error {
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
	for name, value := range headers {
		value = strings.TrimSpace(value)
		if value != "" {
			req.Header.Set(name, value)
		}
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

func queuedJobResultFromJobsRun(jobID string, run jobsV2AgentRunResponse) QueuedJobResult {
	if jobID == "" {
		jobID = run.JobID
	}
	return QueuedJobResult{
		JobID:           jobID,
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
	case "succeeded", "failed", "cancelled", "timed_out", "skipped":
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

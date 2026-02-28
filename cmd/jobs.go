package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	jobsServerURL string
	jobsToken     string
	jobsTimeout   time.Duration
	jobsJSON      bool

	jobsCreateFile string
	jobsCreateData string

	jobsUpdateFile string
	jobsUpdateData string

	jobsDeleteCancelActive bool
	jobsRunsLimit          int
	jobsRunsOffset         int
	jobsEventsLimit        int
	jobsEventsOffset       int
)

var jobsCmd = &cobra.Command{
	Use:   "jobs",
	Short: "Manage jobs v2 (definitions, runs, and events)",
	Long: `Manage the jobs runner over the serve API.

By default this talks to http://127.0.0.1:8080.
You can override with --server / --token or env vars:
  TERM_LLM_JOBS_SERVER
  TERM_LLM_JOBS_TOKEN`,
	Args: cobra.NoArgs,
	RunE: runJobsList,
}

var jobsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List job definitions",
	Args:  cobra.NoArgs,
	RunE:  runJobsList,
}

var jobsGetCmd = &cobra.Command{
	Use:               "get <job-id-or-name>",
	Short:             "Get job definition",
	Args:              cobra.ExactArgs(1),
	RunE:              runJobsGet,
	ValidArgsFunction: jobsArgCompletion,
}

var jobsCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a job definition",
	Long: `Create from JSON/YAML via --file or --data.

Examples:
  term-llm jobs create --file job.yaml
  term-llm jobs create --data '{"name":"nightly",...}'`,
	Args: cobra.NoArgs,
	RunE: runJobsCreate,
}

var jobsUpdateCmd = &cobra.Command{
	Use:               "update <job-id-or-name>",
	Short:             "Update a job definition",
	Args:              cobra.ExactArgs(1),
	RunE:              runJobsUpdate,
	ValidArgsFunction: jobsArgCompletion,
}

var jobsDeleteCmd = &cobra.Command{
	Use:               "delete <job-id-or-name>",
	Short:             "Delete a job definition",
	Args:              cobra.ExactArgs(1),
	RunE:              runJobsDelete,
	ValidArgsFunction: jobsArgCompletion,
}

var jobsTriggerCmd = &cobra.Command{
	Use:               "trigger <job-id-or-name>",
	Short:             "Trigger a manual run",
	Args:              cobra.ExactArgs(1),
	RunE:              runJobsTrigger,
	ValidArgsFunction: jobsArgCompletion,
}

var jobsPauseCmd = &cobra.Command{
	Use:               "pause <job-id-or-name>",
	Short:             "Pause a job definition",
	Args:              cobra.ExactArgs(1),
	RunE:              runJobsPause,
	ValidArgsFunction: jobsArgCompletion,
}

var jobsResumeCmd = &cobra.Command{
	Use:               "resume <job-id-or-name>",
	Short:             "Resume a job definition",
	Args:              cobra.ExactArgs(1),
	RunE:              runJobsResume,
	ValidArgsFunction: jobsArgCompletion,
}

var jobsRunsCmd = &cobra.Command{
	Use:               "runs [job-id-or-name]",
	Short:             "List runs (optionally filtered by job)",
	Args:              cobra.RangeArgs(0, 1),
	RunE:              runJobsRuns,
	ValidArgsFunction: jobsArgCompletion,
}

var jobsActiveCmd = &cobra.Command{
	Use:   "active",
	Short: "List active runs across all jobs",
	Args:  cobra.NoArgs,
	RunE:  runJobsActive,
}

var jobsRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run operations",
}

var jobsRunGetCmd = &cobra.Command{
	Use:               "get <run-id>",
	Short:             "Get run details",
	Args:              cobra.ExactArgs(1),
	RunE:              runJobsRunGet,
	ValidArgsFunction: runsArgCompletion,
}

var jobsRunCancelCmd = &cobra.Command{
	Use:               "cancel <run-id>",
	Short:             "Cancel a run",
	Args:              cobra.ExactArgs(1),
	RunE:              runJobsRunCancel,
	ValidArgsFunction: runsArgCompletion,
}

var jobsRunEventsCmd = &cobra.Command{
	Use:               "events <run-id>",
	Short:             "List run events",
	Args:              cobra.ExactArgs(1),
	RunE:              runJobsRunEvents,
	ValidArgsFunction: runsArgCompletion,
}

func init() {
	jobsCmd.PersistentFlags().StringVar(&jobsServerURL, "server", envOr("TERM_LLM_JOBS_SERVER", "http://127.0.0.1:8080"), "Jobs API server base URL")
	jobsCmd.PersistentFlags().StringVar(&jobsToken, "token", envOr("TERM_LLM_JOBS_TOKEN", ""), "Bearer token for jobs API")
	jobsCmd.PersistentFlags().DurationVar(&jobsTimeout, "timeout", 15*time.Second, "HTTP timeout")
	jobsCmd.PersistentFlags().BoolVar(&jobsJSON, "json", false, "Print JSON output")

	jobsCreateCmd.Flags().StringVar(&jobsCreateFile, "file", "", "Path to JSON/YAML definition file")
	jobsCreateCmd.Flags().StringVar(&jobsCreateData, "data", "", "Inline JSON/YAML definition payload")

	jobsUpdateCmd.Flags().StringVar(&jobsUpdateFile, "file", "", "Path to JSON/YAML update payload file")
	jobsUpdateCmd.Flags().StringVar(&jobsUpdateData, "data", "", "Inline JSON/YAML update payload")

	jobsDeleteCmd.Flags().BoolVar(&jobsDeleteCancelActive, "cancel-active", false, "Cancel active runs before delete")

	jobsRunsCmd.Flags().IntVar(&jobsRunsLimit, "limit", 50, "Max runs to return")
	jobsRunsCmd.Flags().IntVar(&jobsRunsOffset, "offset", 0, "Pagination offset")

	jobsRunEventsCmd.Flags().IntVar(&jobsEventsLimit, "limit", 200, "Max events to return")
	jobsRunEventsCmd.Flags().IntVar(&jobsEventsOffset, "offset", 0, "Pagination offset")

	jobsCmd.AddCommand(jobsListCmd)
	jobsCmd.AddCommand(jobsGetCmd)
	jobsCmd.AddCommand(jobsCreateCmd)
	jobsCmd.AddCommand(jobsUpdateCmd)
	jobsCmd.AddCommand(jobsDeleteCmd)
	jobsCmd.AddCommand(jobsTriggerCmd)
	jobsCmd.AddCommand(jobsPauseCmd)
	jobsCmd.AddCommand(jobsResumeCmd)
	jobsCmd.AddCommand(jobsRunsCmd)
	jobsCmd.AddCommand(jobsActiveCmd)
	jobsCmd.AddCommand(jobsRunCmd)

	jobsRunCmd.AddCommand(jobsRunGetCmd)
	jobsRunCmd.AddCommand(jobsRunCancelCmd)
	jobsRunCmd.AddCommand(jobsRunEventsCmd)

	rootCmd.AddCommand(jobsCmd)
}

type jobsClient struct {
	baseURL string
	token   string
	http    *http.Client
}

type jobsListResponse struct {
	Data []jobsV2Job `json:"data"`
}

type jobsRunsListResponse struct {
	Data []jobsV2Run `json:"data"`
}

type jobsActiveRun struct {
	JobID        string          `json:"job_id"`
	JobName      string          `json:"job_name"`
	RunID        string          `json:"run_id"`
	Status       jobsV2RunStatus `json:"status"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	ScheduledFor time.Time       `json:"scheduled_for"`
	WorkerID     string          `json:"worker_id,omitempty"`
}

type jobsRunEventsListResponse struct {
	Data []jobsV2RunEvent `json:"data"`
}

const jobsActiveRunsPageSize = 10

type openAIErrorResponse struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func newJobsClient() (*jobsClient, error) {
	base := strings.TrimSpace(jobsServerURL)
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("invalid --server %q: must start with http:// or https://", base)
	}
	timeout := jobsTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &jobsClient{
		baseURL: base,
		token:   strings.TrimSpace(jobsToken),
		http:    &http.Client{Timeout: timeout},
	}, nil
}

func (c *jobsClient) do(ctx context.Context, method, path string, body []byte, out any) error {
	url := c.baseURL + path
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var apiErr openAIErrorResponse
		if err := json.Unmarshal(respBody, &apiErr); err == nil && strings.TrimSpace(apiErr.Error.Message) != "" {
			return fmt.Errorf("%s", apiErr.Error.Message)
		}
		return fmt.Errorf("request failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (c *jobsClient) listJobs(ctx context.Context) ([]jobsV2Job, error) {
	var resp jobsListResponse
	if err := c.do(ctx, http.MethodGet, "/v2/jobs?limit=500", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func (c *jobsClient) listRuns(ctx context.Context, jobID string, limit, offset int) ([]jobsV2Run, error) {
	path := fmt.Sprintf("/v2/runs?limit=%d&offset=%d", limit, offset)
	if strings.TrimSpace(jobID) != "" {
		path += "&job_id=" + jobID
	}
	var resp jobsRunsListResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func (c *jobsClient) listActiveRuns(ctx context.Context) ([]jobsActiveRun, error) {
	jobs, err := c.listJobs(ctx)
	if err != nil {
		return nil, err
	}

	active := make([]jobsActiveRun, 0)
	for _, job := range jobs {
		offset := 0
		for {
			runs, err := c.listRuns(ctx, job.ID, jobsActiveRunsPageSize, offset)
			if err != nil {
				return nil, err
			}
			if len(runs) == 0 {
				break
			}

			hasActive := false
			for _, run := range runs {
				if !isActiveRunStatus(run.Status) {
					continue
				}
				hasActive = true
				active = append(active, jobsActiveRun{
					JobID:        run.JobID,
					JobName:      job.Name,
					RunID:        run.ID,
					Status:       run.Status,
					StartedAt:    run.StartedAt,
					ScheduledFor: run.ScheduledFor,
					WorkerID:     run.WorkerID,
				})
			}

			if !hasActive {
				break
			}
			offset += jobsActiveRunsPageSize
		}
	}

	sort.Slice(active, func(i, j int) bool {
		if active[i].ScheduledFor.Equal(active[j].ScheduledFor) {
			return active[i].RunID < active[j].RunID
		}
		return active[i].ScheduledFor.After(active[j].ScheduledFor)
	})

	return active, nil
}

func isActiveRunStatus(status jobsV2RunStatus) bool {
	switch status {
	case jobsV2RunQueued, jobsV2RunClaimed, jobsV2RunRunning:
		return true
	default:
		return false
	}
}

func (c *jobsClient) resolveJobID(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("job id/name is required")
	}
	// Fast path: already a likely ID
	if strings.HasPrefix(ref, "job_") {
		return ref, nil
	}
	jobs, err := c.listJobs(ctx)
	if err != nil {
		return "", err
	}
	exactIDs := make([]string, 0)
	exactNames := make([]string, 0)
	prefixIDs := make([]string, 0)
	for _, job := range jobs {
		if job.ID == ref {
			exactIDs = append(exactIDs, job.ID)
		}
		if job.Name == ref {
			exactNames = append(exactNames, job.ID)
		}
		if strings.HasPrefix(job.ID, ref) {
			prefixIDs = append(prefixIDs, job.ID)
		}
	}
	if len(exactIDs) == 1 {
		return exactIDs[0], nil
	}
	if len(exactNames) == 1 {
		return exactNames[0], nil
	}
	if len(prefixIDs) == 1 {
		return prefixIDs[0], nil
	}
	if len(exactNames) > 1 || len(prefixIDs) > 1 {
		return "", fmt.Errorf("job reference %q is ambiguous", ref)
	}
	return "", fmt.Errorf("job %q not found", ref)
}

func envOr(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

func readPayload(filePath, inline string) ([]byte, error) {
	filePath = strings.TrimSpace(filePath)
	inline = strings.TrimSpace(inline)
	if filePath != "" && inline != "" {
		return nil, fmt.Errorf("use only one of --file or --data")
	}
	if filePath != "" {
		b, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", filePath, err)
		}
		return normalizeJSONPayload(b)
	}
	if inline != "" {
		return normalizeJSONPayload([]byte(inline))
	}
	stdinInfo, err := os.Stdin.Stat()
	if err == nil && (stdinInfo.Mode()&os.ModeCharDevice) == 0 {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		if len(bytes.TrimSpace(b)) == 0 {
			return nil, fmt.Errorf("empty payload from stdin")
		}
		return normalizeJSONPayload(b)
	}
	return nil, fmt.Errorf("missing payload: provide --file, --data, or stdin")
}

func normalizeJSONPayload(data []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty payload")
	}
	var jsonCandidate any
	if err := json.Unmarshal(trimmed, &jsonCandidate); err == nil {
		return json.Marshal(jsonCandidate)
	}
	var yamlCandidate any
	if err := yaml.Unmarshal(trimmed, &yamlCandidate); err != nil {
		return nil, fmt.Errorf("payload is not valid JSON or YAML")
	}
	norm := normalizeYAMLValue(yamlCandidate)
	return json.Marshal(norm)
}

func normalizeYAMLValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalizeYAMLValue(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmt.Sprint(k)] = normalizeYAMLValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = normalizeYAMLValue(t[i])
		}
		return out
	default:
		return v
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func runJobsList(cmd *cobra.Command, args []string) error {
	client, err := newJobsClient()
	if err != nil {
		return err
	}
	items, err := client.listJobs(cmd.Context())
	if err != nil {
		return err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	if jobsJSON {
		return printJSON(items)
	}
	if len(items) == 0 {
		fmt.Println("No jobs found.")
		return nil
	}
	fmt.Printf("%-24s %-24s %-8s %-8s %-20s %-25s\n", "ID", "NAME", "ENABLED", "TRIGGER", "NEXT_RUN_AT", "RUNNER")
	for _, j := range items {
		next := "-"
		if j.NextRunAt != nil {
			next = j.NextRunAt.Local().Format(time.RFC3339)
		}
		fmt.Printf("%-24s %-24s %-8t %-8s %-20s %-25s\n", j.ID, truncateCell(j.Name, 24), j.Enabled, j.TriggerType, truncateCell(next, 20), string(j.RunnerType))
	}
	return nil
}

func runJobsGet(cmd *cobra.Command, args []string) error {
	client, err := newJobsClient()
	if err != nil {
		return err
	}
	jobID, err := client.resolveJobID(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	var job jobsV2Job
	if err := client.do(cmd.Context(), http.MethodGet, "/v2/jobs/"+jobID, nil, &job); err != nil {
		return err
	}
	return printJSON(job)
}

func runJobsCreate(cmd *cobra.Command, args []string) error {
	client, err := newJobsClient()
	if err != nil {
		return err
	}
	payload, err := readPayload(jobsCreateFile, jobsCreateData)
	if err != nil {
		return err
	}
	var job jobsV2Job
	if err := client.do(cmd.Context(), http.MethodPost, "/v2/jobs", payload, &job); err != nil {
		return err
	}
	return printJSON(job)
}

func runJobsUpdate(cmd *cobra.Command, args []string) error {
	client, err := newJobsClient()
	if err != nil {
		return err
	}
	jobID, err := client.resolveJobID(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	payload, err := readPayload(jobsUpdateFile, jobsUpdateData)
	if err != nil {
		return err
	}
	var job jobsV2Job
	if err := client.do(cmd.Context(), http.MethodPatch, "/v2/jobs/"+jobID, payload, &job); err != nil {
		return err
	}
	return printJSON(job)
}

func runJobsDelete(cmd *cobra.Command, args []string) error {
	client, err := newJobsClient()
	if err != nil {
		return err
	}
	jobID, err := client.resolveJobID(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	path := "/v2/jobs/" + jobID
	if jobsDeleteCancelActive {
		path += "?cancel_active=true"
	}
	var resp map[string]any
	if err := client.do(cmd.Context(), http.MethodDelete, path, nil, &resp); err != nil {
		return err
	}
	return printJSON(resp)
}

func runJobsTrigger(cmd *cobra.Command, args []string) error {
	client, err := newJobsClient()
	if err != nil {
		return err
	}
	jobID, err := client.resolveJobID(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	var run jobsV2Run
	if err := client.do(cmd.Context(), http.MethodPost, "/v2/jobs/"+jobID+"/trigger", nil, &run); err != nil {
		return err
	}
	return printJSON(run)
}

func runJobsPause(cmd *cobra.Command, args []string) error {
	client, err := newJobsClient()
	if err != nil {
		return err
	}
	jobID, err := client.resolveJobID(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	var job jobsV2Job
	if err := client.do(cmd.Context(), http.MethodPost, "/v2/jobs/"+jobID+"/pause", nil, &job); err != nil {
		return err
	}
	return printJSON(job)
}

func runJobsResume(cmd *cobra.Command, args []string) error {
	client, err := newJobsClient()
	if err != nil {
		return err
	}
	jobID, err := client.resolveJobID(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	var job jobsV2Job
	if err := client.do(cmd.Context(), http.MethodPost, "/v2/jobs/"+jobID+"/resume", nil, &job); err != nil {
		return err
	}
	return printJSON(job)
}

func runJobsActive(cmd *cobra.Command, args []string) error {
	client, err := newJobsClient()
	if err != nil {
		return err
	}
	items, err := client.listActiveRuns(cmd.Context())
	if err != nil {
		return err
	}
	if jobsJSON {
		return printJSON(items)
	}
	if len(items) == 0 {
		fmt.Println("No active runs found.")
		return nil
	}
	fmt.Printf("%-24s %-24s %-24s %-8s %-20s %-20s %-24s\n", "JOB_ID", "JOB_NAME", "RUN_ID", "STATUS", "STARTED_AT", "SCHEDULED_FOR", "WORKER_ID")
	for _, run := range items {
		startedAt := "-"
		if run.StartedAt != nil {
			startedAt = run.StartedAt.Local().Format(time.RFC3339)
		}
		scheduledFor := "-"
		if !run.ScheduledFor.IsZero() {
			scheduledFor = run.ScheduledFor.Local().Format(time.RFC3339)
		}
		workerID := strings.TrimSpace(run.WorkerID)
		if workerID == "" {
			workerID = "-"
		}
		fmt.Printf("%-24s %-24s %-24s %-8s %-20s %-20s %-24s\n",
			run.JobID,
			truncateCell(run.JobName, 24),
			run.RunID,
			run.Status,
			truncateCell(startedAt, 20),
			truncateCell(scheduledFor, 20),
			truncateCell(workerID, 24),
		)
	}
	return nil
}

func runJobsRuns(cmd *cobra.Command, args []string) error {
	client, err := newJobsClient()
	if err != nil {
		return err
	}
	jobID := ""
	if len(args) == 1 {
		jobID, err = client.resolveJobID(cmd.Context(), args[0])
		if err != nil {
			return err
		}
	}
	items, err := client.listRuns(cmd.Context(), jobID, jobsRunsLimit, jobsRunsOffset)
	if err != nil {
		return err
	}
	if jobsJSON {
		return printJSON(items)
	}
	if len(items) == 0 {
		fmt.Println("No runs found.")
		return nil
	}
	fmt.Printf("%-24s %-24s %-8s %-20s %-20s\n", "RUN_ID", "JOB_ID", "STATUS", "TRIGGER", "SCHEDULED_FOR")
	for _, run := range items {
		fmt.Printf("%-24s %-24s %-8s %-20s %-20s\n", run.ID, run.JobID, run.Status, run.Trigger, run.ScheduledFor.Local().Format(time.RFC3339))
	}
	return nil
}

func runJobsRunGet(cmd *cobra.Command, args []string) error {
	client, err := newJobsClient()
	if err != nil {
		return err
	}
	var run jobsV2Run
	if err := client.do(cmd.Context(), http.MethodGet, "/v2/runs/"+strings.TrimSpace(args[0]), nil, &run); err != nil {
		return err
	}
	return printJSON(run)
}

func runJobsRunCancel(cmd *cobra.Command, args []string) error {
	client, err := newJobsClient()
	if err != nil {
		return err
	}
	var run jobsV2Run
	if err := client.do(cmd.Context(), http.MethodPost, "/v2/runs/"+strings.TrimSpace(args[0])+"/cancel", nil, &run); err != nil {
		return err
	}
	return printJSON(run)
}

func runJobsRunEvents(cmd *cobra.Command, args []string) error {
	client, err := newJobsClient()
	if err != nil {
		return err
	}
	runID := strings.TrimSpace(args[0])
	path := fmt.Sprintf("/v2/runs/%s/events?limit=%d&offset=%d", runID, jobsEventsLimit, jobsEventsOffset)
	var resp jobsRunEventsListResponse
	if err := client.do(cmd.Context(), http.MethodGet, path, nil, &resp); err != nil {
		return err
	}
	if jobsJSON {
		return printJSON(resp.Data)
	}
	if len(resp.Data) == 0 {
		fmt.Println("No events found.")
		return nil
	}
	for _, ev := range resp.Data {
		msg := strings.TrimSpace(ev.Message)
		if msg == "" {
			msg = "-"
		}
		fmt.Printf("%s %-18s %s\n", ev.CreatedAt.Local().Format(time.RFC3339), ev.EventType, msg)
	}
	return nil
}

func jobsArgCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	client, err := newJobsClient()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	jobs, err := client.listJobs(ctx)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	prefix := strings.ToLower(toComplete)
	completions := make([]string, 0)
	for _, j := range jobs {
		if strings.HasPrefix(strings.ToLower(j.ID), prefix) {
			completions = append(completions, j.ID+"\t"+j.Name)
		}
		if j.Name != "" && strings.HasPrefix(strings.ToLower(j.Name), prefix) {
			completions = append(completions, j.Name+"\t"+j.ID)
		}
	}
	sort.Strings(completions)
	return uniqueStrings(completions), cobra.ShellCompDirectiveNoFileComp
}

func runsArgCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	client, err := newJobsClient()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	runs, err := client.listRuns(ctx, "", 200, 0)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	prefix := strings.ToLower(toComplete)
	completions := make([]string, 0)
	for _, run := range runs {
		if strings.HasPrefix(strings.ToLower(run.ID), prefix) {
			completions = append(completions, run.ID+"\t"+string(run.Status)+" "+run.JobID)
		}
	}
	sort.Strings(completions)
	return uniqueStrings(completions), cobra.ShellCompDirectiveNoFileComp
}

func truncateCell(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

func uniqueStrings(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

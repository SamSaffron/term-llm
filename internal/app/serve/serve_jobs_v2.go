package serve

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	servejobs "github.com/samsaffron/term-llm/internal/app/serve/jobs"
	"github.com/samsaffron/term-llm/internal/jobs"
	_ "modernc.org/sqlite"
)

const exitReasonNatural = jobs.ExitReasonNatural

const exitReasonMaxTurns = jobs.ExitReasonMaxTurns

const exitReasonTimeout = jobs.ExitReasonTimeout

const exitReasonCancelled = jobs.ExitReasonCancelled

const exitReasonException = jobs.ExitReasonException

const exitReasonEmpty = jobs.ExitReasonEmpty

const exitReasonWorkerLost = jobs.ExitReasonWorkerLost

type jobsV2RunnerType = servejobs.RunnerType
type jobsV2TriggerType = servejobs.TriggerType
type jobsV2RunStatus = servejobs.RunStatus
type jobsV2RetryPolicy = servejobs.RetryPolicy
type jobsV2TriggerConfig = servejobs.TriggerConfig
type jobsV2JobRequest = servejobs.JobRequest
type jobsV2Job = servejobs.Job
type jobsV2RunEvent = servejobs.RunEvent
type jobsV2RunResult = servejobs.RunResult
type jobsV2RunDoneNotifier = servejobs.RunDoneNotifier
type jobsV2Runner = servejobs.Runner
type jobsV2Run = servejobs.Run
type jobsV2Manager = servejobs.Manager

const jobsV2RunnerLLM = servejobs.RunnerLLM
const jobsV2RunnerProgram = servejobs.RunnerProgram
const jobsV2TriggerManual = servejobs.TriggerManual
const jobsV2TriggerOnce = servejobs.TriggerOnce
const jobsV2TriggerCron = servejobs.TriggerCron
const jobsV2RunQueued = servejobs.RunQueued
const jobsV2RunClaimed = servejobs.RunClaimed
const jobsV2RunRunning = servejobs.RunRunning
const jobsV2RunSucceeded = servejobs.RunSucceeded
const jobsV2RunFailed = servejobs.RunFailed
const jobsV2RunCancelled = servejobs.RunCancelled
const jobsV2RunCancelRequested = servejobs.RunCancelRequested
const jobsV2RunTimedOut = servejobs.RunTimedOut
const jobsV2RunSkipped = servejobs.RunSkipped
const jobsV2MisfireSkip = servejobs.MisfireSkip
const jobsV2MisfireRun = servejobs.MisfireRun

type progressWriter = servejobs.ProgressWriter

var jobsV2JobToRequest = servejobs.JobToRequest

func (s *serveServer) handleJobsV2(w http.ResponseWriter, r *http.Request) {
	if s.jobsV2 == nil {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.handleCreateJobV2(w, r)
	case http.MethodGet:
		s.handleListJobsV2(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

func (s *serveServer) handleJobV2ByID(w http.ResponseWriter, r *http.Request) {
	if s.jobsV2 == nil {
		http.NotFound(w, r)
		return
	}
	prefix := "/v2/jobs/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	jobID := parts[0]

	if len(parts) == 2 && parts[1] == "trigger" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		run, err := s.jobsV2.TriggerJob(jobID)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, run)
		return
	}
	if len(parts) == 2 && parts[1] == "pause" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		updated, err := s.jobsV2.UpdateJob(jobID, jobsV2Job{Enabled: false})
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, updated)
		return
	}
	if len(parts) == 2 && parts[1] == "resume" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		updated, err := s.jobsV2.UpdateJob(jobID, jobsV2Job{Enabled: true})
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, updated)
		return
	}

	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		job, err := s.jobsV2.GetJob(jobID)
		if err != nil {
			writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "job not found")
			return
		}
		writeJSON(w, http.StatusOK, job)
	case http.MethodPatch:
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		var req jobsV2JobRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		job, err := s.jobsV2.UpdateJobPatch(jobID, req)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, job)
	case http.MethodDelete:
		cancelActive := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("cancel_active")), "true")
		if err := s.jobsV2.DeleteJob(jobID, cancelActive); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": jobID, "deleted": true})
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

func (s *serveServer) handleCreateJobV2(w http.ResponseWriter, r *http.Request) {
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return
	}
	var req jobsV2JobRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	jobReq := req.ToJob(true)
	if jobReq.RunnerType == jobsV2RunnerLLM {
		jobReq.RunnerConfig = sanitizeJobsV2LLMNotifyOriginForRequest(r, jobReq.RunnerConfig)
	}
	if s.cfgRef != nil && jobReq.RunnerType == jobsV2RunnerLLM {
		var llmCfg JobsV2LLMConfig
		_ = json.Unmarshal(jobReq.RunnerConfig, &llmCfg)
		if strings.TrimSpace(llmCfg.AgentName) != "" {
			if _, err := appruntime.LoadAgent(llmCfg.AgentName, s.cfgRef); err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
		}
	}
	job, err := s.jobsV2.CreateJob(jobReq)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

func (s *serveServer) handleListJobsV2(w http.ResponseWriter, r *http.Request) {
	offset, err := parseNonNegativeIntQuery(r, "offset", 0)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	limit, err := parseNonNegativeIntQuery(r, "limit", 50)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	items, total, err := s.jobsV2.ListJobs(limit, offset)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   items,
		"total":  total,
		"offset": offset,
		"limit":  limit,
	})
}

func (s *serveServer) handleRunsV2(w http.ResponseWriter, r *http.Request) {
	if s.jobsV2 == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	offset, err := parseNonNegativeIntQuery(r, "offset", 0)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	limit, err := parseNonNegativeIntQuery(r, "limit", 50)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	jobID := strings.TrimSpace(r.URL.Query().Get("job_id"))
	var items []jobsV2Run
	var total int
	if queryBool(r, "summary") {
		items, total, err = s.jobsV2.ListRunSummaries(jobID, limit, offset)
	} else {
		items, total, err = s.jobsV2.ListRuns(jobID, limit, offset)
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   items,
		"total":  total,
		"offset": offset,
		"limit":  limit,
	})
}

func (s *serveServer) handleRunV2ByID(w http.ResponseWriter, r *http.Request) {
	if s.jobsV2 == nil {
		http.NotFound(w, r)
		return
	}
	prefix := "/v2/runs/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	runID := parts[0]
	if len(parts) == 2 && parts[1] == "events" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		offset, err := parseNonNegativeIntQuery(r, "offset", 0)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		limit, err := parseNonNegativeIntQuery(r, "limit", 200)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		sinceID, err := parseNonNegativeIntQuery(r, "since_id", 0)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		items, total, err := s.jobsV2.ListRunEvents(runID, int64(sinceID), limit, offset)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"object":   "list",
			"data":     items,
			"total":    total,
			"offset":   offset,
			"limit":    limit,
			"since_id": sinceID,
		})
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		run, err := s.jobsV2.CancelRun(runID)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, run)
		return
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var run jobsV2Run
	var err error
	if queryBool(r, "summary") {
		run, err = s.jobsV2.GetRunSummary(runID)
	} else {
		run, err = s.jobsV2.GetRun(runID)
	}
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "run not found")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func queryBool(r *http.Request, key string) bool {
	value := strings.TrimSpace(r.URL.Query().Get(key))
	return strings.EqualFold(value, "1") || strings.EqualFold(value, "true") || strings.EqualFold(value, "yes")
}

func parseNonNegativeIntQuery(r *http.Request, key string, defaultValue int) (int, error) {
	value := strings.TrimSpace(r.URL.Query().Get(key))
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
	return parsed, nil
}

// initJobsV2Schema is retained for callers outside the scheduler (doctor).
func initJobsV2Schema(ctx context.Context, db *sql.DB) error {
	return servejobs.InitSchema(ctx, db)
}

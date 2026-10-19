package cmd

import (
	servejobs "github.com/samsaffron/term-llm/internal/app/serve/jobs"
	"time"
)

type jobsListResponse struct {
	Data []servejobs.Job `json:"data"`
}

type jobsRunsListResponse struct {
	Data []servejobs.Run `json:"data"`
}

type jobsActiveRun struct {
	JobID        string              `json:"job_id"`
	JobName      string              `json:"job_name"`
	RunID        string              `json:"run_id"`
	Status       servejobs.RunStatus `json:"status"`
	StartedAt    *time.Time          `json:"started_at,omitempty"`
	ScheduledFor time.Time           `json:"scheduled_for"`
	WorkerID     string              `json:"worker_id,omitempty"`
}

type jobsRunEventsListResponse struct {
	Data []servejobs.RunEvent `json:"data"`
}

const jobsActiveRunsPageSize = 10

type openAIErrorResponse struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

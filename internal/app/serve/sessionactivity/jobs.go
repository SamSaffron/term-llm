package sessionactivity

import (
	"context"
	"encoding/json"
	"fmt"
	servejobs "github.com/samsaffron/term-llm/internal/app/serve/jobs"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type activityJobsSource struct {
	local   *servejobs.Manager
	baseURL string
	token   string
}

// Unlike the tools client, this optional read model has NO localhost default.
// Only the owned manager or an explicitly configured jobs server is eligible.
func (s Handler) activityJobsSource() *activityJobsSource {
	if s.Jobs != nil {
		return &activityJobsSource{local: s.Jobs}
	}
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("TERM_LLM_JOBS_SERVER")), "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil
	}
	return &activityJobsSource{baseURL: base, token: strings.TrimSpace(os.Getenv("TERM_LLM_JOBS_TOKEN"))}
}

func (source *activityJobsSource) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.baseURL+path, nil)
	if err != nil {
		return err
	}
	if source.token != "" {
		req.Header.Set("Authorization", "Bearer "+source.token)
	}
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("read activity jobs: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("activity jobs status %d", response.StatusCode)
	}
	// Run summaries are small. Never read unbounded output from a misconfigured
	// server or propagate its response/error text (which can contain credentials).
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(out)
}

func (source *activityJobsSource) runs(ctx context.Context, ref jobReference) ([]servejobs.Run, bool, error) {
	if ref.RunID != "" {
		var run servejobs.Run
		var err error
		if source.local != nil {
			run, err = source.local.GetRunSummary(ref.RunID)
		} else {
			err = source.getJSON(ctx, "/v2/runs/"+url.PathEscape(ref.RunID)+"?summary=true", &run)
		}
		if err != nil {
			return nil, false, err
		}
		if run.ID != ref.RunID || run.JobID != ref.JobID {
			return nil, false, fmt.Errorf("activity run membership mismatch")
		}
		return []servejobs.Run{run}, false, nil
	}
	// Legacy persisted results with only a job id establish job scope. Exact
	// run references above MUST NOT use this branch or reveal sibling executions.
	if source.local != nil {
		runs, total, err := source.local.ListRunSummaries(ref.JobID, maxActivityRuns, 0)
		return runs, total > len(runs), err
	}
	var payload struct {
		Data  []servejobs.Run `json:"data"`
		Total int             `json:"total"`
	}
	err := source.getJSON(ctx, "/v2/runs?summary=true&limit=50&offset=0&job_id="+url.QueryEscape(ref.JobID), &payload)
	return payload.Data, payload.Total > len(payload.Data), err
}

func activityJobPlaceholder(ref jobReference) ActivityJobRun {
	title := "Queued job"
	if validActivityID(ref.Agent) {
		title = strings.ToUpper(ref.Agent[:1]) + ref.Agent[1:] + " task"
	}
	if ref.Title != "" {
		title = ref.Title
	}
	return ActivityJobRun{JobID: ref.JobID, RunID: ref.RunID, Title: title, Status: "unknown"}
}

func (s Handler) projectActivityJobRun(ctx context.Context, source *activityJobsSource, ref jobReference, run servejobs.Run) ActivityJobRun {
	item := activityJobPlaceholder(ref)
	item.RunID = run.ID
	item.Status = string(run.Status)
	if run.StartedAt != nil {
		item.StartedAt = run.StartedAt.UnixMilli()
	}
	if run.FinishedAt != nil {
		item.EndedAt = run.FinishedAt.UnixMilli()
	}
	// A remote jobs server's session id is not a route on this server. Even local
	// ids must exist in our store before becoming a native navigation target.
	if source.local != nil && run.SessionID != "" {
		if stored, err := s.Store.Get(ctx, run.SessionID); err == nil && stored != nil {
			item.SessionID = stored.ID
			item.Model = stored.Model
		}
	}
	return item
}

func (s Handler) collectActivityJobs(ctx context.Context, refs []jobReference, snapshot *SessionActivityResponse) {
	source := s.activityJobsSource()
	if source != nil {
		snapshot.JobsAvailability = "available"
	}
	seen := make(map[string]bool)
	for _, ref := range refs {
		if len(snapshot.Jobs) >= maxActivityRuns {
			snapshot.JobsPartial = true
			break
		}
		if source == nil {
			snapshot.Jobs = append(snapshot.Jobs, activityJobPlaceholder(ref))
			snapshot.JobsPartial = true
			continue
		}
		runs, partial, err := source.runs(ctx, ref)
		snapshot.JobsPartial = snapshot.JobsPartial || partial
		if err != nil {
			snapshot.JobsAvailability = "unavailable"
			snapshot.JobsPartial = true
			snapshot.Jobs = append(snapshot.Jobs, activityJobPlaceholder(ref))
			continue
		}
		if len(runs) == 0 {
			snapshot.Jobs = append(snapshot.Jobs, activityJobPlaceholder(ref))
			snapshot.JobsPartial = true
		}
		matched := false
		for _, run := range runs {
			if run.JobID != ref.JobID || !validActivityID(run.ID) {
				snapshot.JobsPartial = true
				continue
			}
			matched = true
			if seen[run.ID] {
				continue
			}
			seen[run.ID] = true
			if len(snapshot.Jobs) >= maxActivityRuns {
				snapshot.JobsPartial = true
				break
			}
			snapshot.Jobs = append(snapshot.Jobs, s.projectActivityJobRun(ctx, source, ref, run))
		}
		if !matched && len(runs) > 0 && len(snapshot.Jobs) < maxActivityRuns {
			snapshot.Jobs = append(snapshot.Jobs, activityJobPlaceholder(ref))
		}
	}
	sort.SliceStable(snapshot.Jobs, func(i, j int) bool {
		a, b := snapshot.Jobs[i], snapshot.Jobs[j]
		if a.JobID == b.JobID {
			return a.RunID < b.RunID
		}
		return a.JobID < b.JobID
	})
}

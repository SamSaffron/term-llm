package sessionactivity

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
	"unicode/utf8"

	servejobs "github.com/term-llm/term-llm/internal/app/serve/jobs"
	"github.com/term-llm/term-llm/internal/linkeddevices"
)

const maxActivityOutputBytes = 64 << 10

// ActivityJobDetail exposes bounded output for one freshly authorized run only.
type ActivityJobDetail struct {
	ActivityJobRun
	Response  string `json:"response,omitempty"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	Error     string `json:"error,omitempty"`
	Truncated bool   `json:"truncated"`
}

// ServeRun revalidates the stored conversation membership on every read. There
// is no global run lookup endpoint or cached client-supplied membership grant.
func (s Handler) ServeRun(w http.ResponseWriter, r *http.Request, sessionID, runID string) {
	if _, device := linkeddevices.DeviceFromRequest(r); device {
		writeError(w, http.StatusForbidden, "permission_error", "job activity is restricted on this device")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if !validActivityID(runID) || s.Store == nil {
		writeError(w, http.StatusNotFound, "not_found_error", "activity run not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if parent, err := s.Store.Get(ctx, sessionID); err != nil || parent == nil {
		writeError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	snapshot := SessionActivityResponse{Agents: []ChildRunProjection{}}
	scope := s.collectActivityAgents(ctx, sessionID, &snapshot)
	refs, _ := s.collectActivityJobReferences(ctx, scope)
	source := s.activityJobsSource()
	if source == nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", "job activity is unavailable")
		return
	}
	if !detailAuthorized(refs, runID) {
		writeError(w, http.StatusNotFound, "not_found_error", "activity run not found")
		return
	}
	run, err := source.detail(ctx, runID)
	if err != nil {
		if activityRunMissing(err) {
			// Retention removed this run. An expired record is not a source outage.
			writeError(w, http.StatusNotFound, "not_found_error", "activity run not found")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "server_error", "job activity is unavailable")
		return
	}
	if run.ID != runID {
		writeError(w, http.StatusNotFound, "not_found_error", "activity run not found")
		return
	}
	ref, found := detailReference(refs, run)
	if !found {
		writeError(w, http.StatusNotFound, "not_found_error", "activity run not found")
		return
	}
	detail := ActivityJobDetail{ActivityJobRun: s.projectActivityJobRun(ctx, source, ref, run), Truncated: run.Truncated}
	fields := []struct {
		source string
		target *string
	}{{run.Response, &detail.Response}, {run.Stdout, &detail.Stdout}, {run.Stderr, &detail.Stderr}, {run.Error, &detail.Error}}
	for _, field := range fields {
		*field.target = field.source
		if len(field.source) > maxActivityOutputBytes {
			*field.target = boundedOutput(field.source)
			detail.Truncated = true
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = json.NewEncoder(w).Encode(detail)
}

// detailAuthorized reports whether the stored conversation may attempt a
// backend read for runID at all: an exact run reference, or a legacy job-wide
// reference whose job is confirmed against the loaded run below.
func detailAuthorized(refs []jobReference, runID string) bool {
	for _, ref := range refs {
		if ref.RunID == runID || ref.RunID == "" {
			return true
		}
	}
	return false
}

// detailReference prefers the exact run reference, then the job-wide reference
// for the run's own job, so a detail page never borrows another job's title.
func detailReference(refs []jobReference, run servejobs.Run) (jobReference, bool) {
	for _, ref := range refs {
		if ref.RunID == run.ID && ref.JobID == run.JobID {
			return ref, true
		}
	}
	for _, ref := range refs {
		if ref.RunID == "" && ref.JobID == run.JobID {
			return ref, true
		}
	}
	return jobReference{}, false
}

func (source *activityJobsSource) detail(ctx context.Context, id string) (servejobs.Run, error) {
	if source.local != nil {
		run, err := source.local.GetRun(id)
		return run, expiredOrError(err)
	}
	var run servejobs.Run
	err := source.getJSON(ctx, "/v2/runs/"+id, &run)
	return run, expiredOrError(err)
}

// boundedOutput caps a preview at maxActivityOutputBytes. Only a partial rune at
// the cut is backed out; interior invalid bytes are kept because encoding/json
// already replaces them with U+FFFD.
func boundedOutput(value string) string {
	if len(value) <= maxActivityOutputBytes {
		return value
	}
	cut := maxActivityOutputBytes
	for back := 0; back < utf8.UTFMax-1 && cut > 0 && !utf8.RuneStart(value[cut]); back++ {
		cut--
	}
	return value[:cut]
}

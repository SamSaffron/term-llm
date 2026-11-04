package sessionactivity

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	servejobs "github.com/samsaffron/term-llm/internal/app/serve/jobs"
	"github.com/samsaffron/term-llm/internal/linkeddevices"
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
	ref, found := detailReference(refs, runID)
	if !found {
		writeError(w, http.StatusNotFound, "not_found_error", "activity run not found")
		return
	}
	run, err := source.detail(ctx, runID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", "job activity is unavailable")
		return
	}
	if run.ID != runID || !detailMember(refs, run) {
		writeError(w, http.StatusNotFound, "not_found_error", "activity run not found")
		return
	}
	ref.JobID = run.JobID
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

func detailReference(refs []jobReference, runID string) (jobReference, bool) {
	for _, ref := range refs {
		if ref.RunID == runID {
			return ref, true
		}
	}
	// Only an explicit legacy job-wide reference can authorize a backend lookup
	// without an exact run id. The returned job id is checked again below.
	for _, ref := range refs {
		if ref.RunID == "" {
			return ref, true
		}
	}
	return jobReference{}, false
}

func detailMember(refs []jobReference, run servejobs.Run) bool {
	for _, ref := range refs {
		if ref.JobID == run.JobID && (ref.RunID == run.ID || ref.RunID == "") {
			return true
		}
	}
	return false
}

func (source *activityJobsSource) detail(ctx context.Context, id string) (servejobs.Run, error) {
	if source.local != nil {
		return source.local.GetRun(id)
	}
	var run servejobs.Run
	err := source.getJSON(ctx, "/v2/runs/"+id, &run)
	return run, err
}

func boundedOutput(value string) string {
	value = value[:maxActivityOutputBytes]
	for !utf8.ValidString(value) {
		value = strings.TrimSuffix(value, value[len(value)-1:])
	}
	return value
}

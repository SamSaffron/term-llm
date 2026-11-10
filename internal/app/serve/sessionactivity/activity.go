package sessionactivity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	servejobs "github.com/samsaffron/term-llm/internal/app/serve/jobs"
	"github.com/samsaffron/term-llm/internal/linkeddevices"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

const (
	maxActivityDepth    = 8
	maxActivitySessions = 100
	maxActivityMessages = 500
	maxActivityRuns     = 50
)

// SessionActivityResponse is a bounded conversation snapshot. Availability is
// independent for each source: an optional jobs outage cannot hide agents.
type SessionActivityResponse struct {
	Cells              []core.CodeModeCellSummary `json:"cells,omitempty"`
	SessionID          string                     `json:"session_id"`
	Agents             []ChildRunProjection       `json:"agents"`
	Jobs               []ActivityJobRun           `json:"jobs"`
	AgentsAvailability string                     `json:"agents_availability"`
	JobsAvailability   string                     `json:"jobs_availability"`
	AgentsPartial      bool                       `json:"agents_partial"`
	JobsPartial        bool                       `json:"jobs_partial"`
}

// ActivityJobRun intentionally excludes output, errors, runner configuration and
// incomplete usage counters. SessionID is only populated for a local session.
type ActivityJobRun struct {
	JobID     string `json:"job_id"`
	RunID     string `json:"run_id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Model     string `json:"model,omitempty"`
	StartedAt int64  `json:"started_at,omitempty"`
	EndedAt   int64  `json:"ended_at,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// Handler owns the scoped, read-only activity snapshot. Children is the host's
// existing live projection; no second child-state implementation is introduced.
type Handler struct {
	Store    session.Store
	Cells    func(string) []core.CodeModeCellSummary
	Children func(context.Context, string) ([]ChildRunProjection, bool, error)
	Jobs     *servejobs.Manager
}

func (s Handler) ServeSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if s.Store == nil {
		writeError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}
	if parent, err := s.Store.Get(r.Context(), sessionID); err != nil || parent == nil {
		writeError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	snapshot := SessionActivityResponse{SessionID: sessionID, Agents: []ChildRunProjection{}, Jobs: []ActivityJobRun{}, AgentsAvailability: "available", JobsAvailability: "unavailable"}
	if s.Cells != nil {
		snapshot.Cells = s.Cells(sessionID)
	}
	scope := s.collectActivityAgents(ctx, sessionID, &snapshot)
	// Linked devices can read delegated agents, but cannot read owner jobs. Check
	// before provenance discovery and before constructing/contacting any backend.
	if _, device := linkeddevices.DeviceFromRequest(r); device {
		snapshot.JobsAvailability = "restricted"
	} else {
		refs, partial := s.collectActivityJobReferences(ctx, scope)
		snapshot.JobsPartial = partial || snapshot.AgentsPartial
		s.collectActivityJobs(ctx, refs, &snapshot)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "failed to encode activity")
		return
	}
	digest := sha256.Sum256(encoded)
	etag := `"activity-` + hex.EncodeToString(digest[:16]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func (s Handler) collectActivityAgents(ctx context.Context, root string, snapshot *SessionActivityResponse) []string {
	type node struct {
		id    string
		depth int
	}
	queue := []node{{root, 0}}
	scope := []string{root}
	seen := map[string]bool{root: true}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.depth >= maxActivityDepth {
			snapshot.AgentsPartial = true
			continue
		}
		items, truncated, err := s.Children(ctx, current.id)
		if err != nil {
			snapshot.AgentsPartial = true
			snapshot.AgentsAvailability = "unavailable"
			continue
		}
		snapshot.AgentsPartial = snapshot.AgentsPartial || truncated
		for _, child := range items {
			if seen[child.SessionID] {
				continue
			}
			if len(snapshot.Agents) >= maxActivitySessions {
				snapshot.AgentsPartial = true
				return scope
			}
			seen[child.SessionID] = true
			snapshot.Agents = append(snapshot.Agents, child)
			scope = append(scope, child.SessionID)
			queue = append(queue, node{child.SessionID, current.depth + 1})
		}
	}
	return scope
}

func (s Handler) collectActivityJobReferences(ctx context.Context, scope []string) ([]jobReference, bool) {
	reader, ok := s.Store.(interface {
		GetMessagesPageDescending(context.Context, string, int, int) ([]session.Message, error)
	})
	if !ok {
		return nil, true
	}
	refs := make([]jobReference, 0)
	partial := false
	seen := make(map[string]bool)
	for _, id := range scope {
		messages, err := reader.GetMessagesPageDescending(ctx, id, 0, maxActivityMessages)
		if err != nil {
			partial = true
			continue
		}
		partial = partial || len(messages) >= maxActivityMessages
		for _, ref := range jobReferences(messages) {
			key := ref.JobID + ":" + ref.RunID
			if seen[key] {
				continue
			}
			seen[key] = true
			if len(refs) >= maxActivityRuns {
				return refs, true
			}
			refs = append(refs, ref)
		}
	}
	return refs, partial
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": kind, "message": message}})
}

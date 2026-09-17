package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

const (
	sessionDirectoryStoreDefaultLimit = 20
	sessionDirectoryMaxLimit          = 50
)

// sessionDirectoryQuery describes one session-directory lookup. A non-empty
// Query switches the source from the activity-ordered session list to full-text
// transcript search.
type sessionDirectoryQuery struct {
	Query           string // non-empty => FTS transcript search via s.store.Search
	RunningOnly     bool
	ProjectID       string
	IncludeArchived bool
	Limit           int
}

// sessionDirectoryEntry is a raw, presentation-free row of the session
// directory: real times and untruncated titles. Callers such as the voice tool
// own truncation, relative times, and spoken phrasing, so the same lookup stays
// usable by any other consumer.
type sessionDirectoryEntry struct {
	ID           string
	Title        string
	Project      string
	Status       string
	Snippet      string
	Number       int64
	Running      bool
	NeedsInput   bool
	LastActivity time.Time
	MessageCount int
}

// sessionDirectory lists or searches durable chat sessions, decorated with the
// same running/interaction truth the browser polls. It never touches runtimes
// beyond the in-memory active-run maps.
//
// A running-only request with no query is driven from the running set itself
// (see the RunningOnly branch), not filtered out of an activity page, so a
// running session can never be hidden by the store's limit.
func (s *serveServer) sessionDirectory(ctx context.Context, q sessionDirectoryQuery) ([]sessionDirectoryEntry, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("the session store is unavailable")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = sessionDirectoryStoreDefaultLimit
	}
	if limit > sessionDirectoryMaxLimit {
		limit = sessionDirectoryMaxLimit
	}
	projectID := strings.TrimSpace(q.ProjectID)
	query := strings.TrimSpace(q.Query)
	running := s.runningSessionIDs(ctx)
	needsInput := s.sessionsNeedingInput(ctx)

	var entries []sessionDirectoryEntry
	switch {
	case query != "":
		matches, err := s.store.Search(ctx, session.SearchOptions{
			Query: query, Limit: limit, Archived: q.IncludeArchived,
			ProjectID: projectID, ExcludeSubagents: true,
		})
		if err != nil {
			return nil, fmt.Errorf("search sessions: %w", err)
		}
		entries = make([]sessionDirectoryEntry, 0, len(matches))
		for _, match := range matches {
			// Search rows carry the same title fields as list summaries, so reuse
			// the shared preference order instead of inventing a second one.
			summary := session.SessionSummary{
				ID: match.SessionID, Number: match.SessionNumber, Name: match.SessionName,
				Summary: match.Summary, GeneratedShortTitle: match.GeneratedShortTitle,
				GeneratedLongTitle: match.GeneratedLongTitle, TitleSource: match.TitleSource,
				Archived: match.Archived, MessageCount: match.MessageCount, Status: match.Status,
				ProjectID: match.ProjectID, ProjectName: match.ProjectName,
				CreatedAt: match.SessionCreatedAt, UpdatedAt: match.UpdatedAt, LastMessageAt: match.LastMessageAt,
			}
			entries = append(entries, sessionDirectoryEntry{
				ID: summary.ID, Number: summary.Number, Title: summary.PreferredShortTitle(),
				Project: summary.ProjectName, Status: string(summary.Status), Snippet: match.Snippet,
				Running: running[summary.ID], NeedsInput: needsInput[summary.ID],
				LastActivity: sessionSummaryLastMessageAt(summary), MessageCount: summary.MessageCount,
			})
		}
		if q.RunningOnly {
			// A search is page-limited by construction — FTS matches arrive in rank
			// order under the store's limit — so unlike a listing there is no
			// complete running set to drive it from, and the filter stays a
			// post-filter over the matches examined.
			entries = filterRunningDirectoryEntries(entries)
		}
	case q.RunningOnly:
		// The running set is the only truth about "what is running", and it is
		// already in hand, so list it directly. Filtering an activity-ordered page
		// instead would report "nothing is running" whenever every running session
		// ranked below the page limit — a false answer the voice model would speak
		// with confidence. This mirrors the critical-session backfill in
		// handleSessionsStatus.
		if len(running) == 0 {
			// Nothing is running, and the honest answer is no entries. The store
			// reads IDs only when the set is non-empty — an empty slice is not a
			// filter — so passing it through here would list the whole directory and
			// mark every row as running.
			return []sessionDirectoryEntry{}, nil
		}
		ids := make([]string, 0, len(running))
		for id := range running {
			ids = append(ids, id)
		}
		summaries, err := s.store.List(ctx, session.ListOptions{
			IDs: ids, Limit: -1, Archived: true, SortByActivity: true,
			ExcludeSubagents: true, ProjectID: projectID,
		})
		if err != nil {
			return nil, fmt.Errorf("list running sessions: %w", err)
		}
		entries = make([]sessionDirectoryEntry, 0, len(summaries))
		for _, summary := range summaries {
			if len(entries) == limit {
				break
			}
			entries = append(entries, sessionDirectoryEntry{
				ID: summary.ID, Number: summary.Number, Title: summary.PreferredShortTitle(),
				Project: summary.ProjectName, Status: string(summary.Status),
				Running: true, NeedsInput: needsInput[summary.ID],
				LastActivity: sessionSummaryLastMessageAt(summary), MessageCount: summary.MessageCount,
			})
		}
	default:
		summaries, err := s.store.List(ctx, session.ListOptions{
			Limit: limit, Archived: q.IncludeArchived, SortByActivity: true,
			ExcludeSubagents: true, ProjectID: projectID,
		})
		if err != nil {
			return nil, fmt.Errorf("list sessions: %w", err)
		}
		entries = make([]sessionDirectoryEntry, 0, len(summaries))
		for _, summary := range summaries {
			entries = append(entries, sessionDirectoryEntry{
				ID: summary.ID, Number: summary.Number, Title: summary.PreferredShortTitle(),
				Project: summary.ProjectName, Status: string(summary.Status),
				Running: running[summary.ID], NeedsInput: needsInput[summary.ID],
				LastActivity: sessionSummaryLastMessageAt(summary), MessageCount: summary.MessageCount,
			})
		}
	}
	return entries, nil
}

// filterRunningDirectoryEntries keeps only sessions with a running task. It exists
// for the search path, where the candidate set is a page of ranked matches rather
// than a complete set of session ids.
func filterRunningDirectoryEntries(entries []sessionDirectoryEntry) []sessionDirectoryEntry {
	filtered := make([]sessionDirectoryEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Running {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// sessionDirectoryForTool adapts the shared directory lookup to the tool
// package's transport-free types, which cannot import this package.
func (s *serveServer) sessionDirectoryForTool(ctx context.Context, q tools.SessionDirectoryQuery) ([]tools.SessionDirectoryEntry, error) {
	entries, err := s.sessionDirectory(ctx, sessionDirectoryQuery{
		Query: q.Query, RunningOnly: q.RunningOnly, ProjectID: q.ProjectID,
		IncludeArchived: q.IncludeArchived, Limit: q.Limit,
	})
	if err != nil {
		return nil, err
	}
	result := make([]tools.SessionDirectoryEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, tools.SessionDirectoryEntry{
			ID: entry.ID, Number: entry.Number, Title: entry.Title, Project: entry.Project,
			Status: entry.Status, Snippet: entry.Snippet, Running: entry.Running,
			NeedsInput: entry.NeedsInput, LastActivity: entry.LastActivity, MessageCount: entry.MessageCount,
		})
	}
	return result, nil
}

// sessionsNeedingInput reports sessions whose run is blocked on an approval or
// a question, using the same runtime and durable sources as /v1/sessions/status.
func (s *serveServer) sessionsNeedingInput(ctx context.Context) map[string]bool {
	needsInput := make(map[string]bool)
	if s == nil || s.store == nil {
		return needsInput
	}
	if attentionStore, ok := session.AsAttentionStore(s.store); ok {
		if _, supported := session.AsResponseRunInteractionStore(s.store); supported {
			for id := range listStatusAttention(ctx, attentionStore, session.AttentionKindInputRequired) {
				needsInput[id] = true
			}
		}
	}
	if s.sessionMgr != nil {
		for id, summary := range s.sessionMgr.UnresolvedInteractionSummaries() {
			if summary.Count > 0 {
				needsInput[id] = true
			}
		}
	}
	return needsInput
}

package project

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/session"
)

const reconciliationPageSize = 200

var ErrProjectUnavailable = errors.New("project directory is unavailable")

// AssignSessionForDir fills an unassigned session's project identity from its
// execution directory. Lack of project support or a matching active project is
// a normal no-op.
func AssignSessionForDir(ctx context.Context, store session.Store, sess *session.Session, dir string) (*session.Project, error) {
	if store == nil || sess == nil || strings.TrimSpace(sess.ProjectID) != "" {
		return nil, nil
	}
	projects, ok := session.AsProjectStore(store)
	if !ok {
		return nil, nil
	}
	active, err := projects.HasActiveProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("check active projects: %w", err)
	}
	if !active {
		return nil, nil
	}
	resolved, err := Resolve(ctx, dir)
	if err != nil {
		return nil, nil
	}
	p, err := projects.GetProjectByCanonicalDir(ctx, resolved.CanonicalDir)
	if err != nil {
		return nil, fmt.Errorf("find project for directory: %w", err)
	}
	if p == nil || p.Archived() {
		return nil, nil
	}
	sess.ProjectID = p.ID
	sess.ProjectName = p.Name
	return p, nil
}

// MatchingSessions returns prevalidated, currently unassigned session
// workspaces belonging to p. The store rechecks each snapshot when claiming it.
func MatchingSessions(ctx context.Context, store session.Store, p session.Project) ([]session.ProjectSessionMatch, error) {
	if store == nil || strings.TrimSpace(p.ID) == "" || p.Archived() {
		return nil, nil
	}
	root, err := Resolve(ctx, p.CanonicalDir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProjectUnavailable, err)
	}
	if !SameIdentity(root.CanonicalDir, p.CanonicalDir) {
		return nil, fmt.Errorf("%w: canonical identity changed", ErrProjectUnavailable)
	}
	return MatchingSessionsForResolved(ctx, store, root)
}

// MatchingSessionsForResolved finds unassigned snapshots for a validated root.
// It is also used before the first bootstrap project has received an identity.
func MatchingSessionsForResolved(ctx context.Context, store session.Store, root Resolved) ([]session.ProjectSessionMatch, error) {
	if store == nil || strings.TrimSpace(root.CanonicalDir) == "" {
		return nil, nil
	}
	matches := make([]session.ProjectSessionMatch, 0)
	matchesByWorkspace := make(map[string]bool)
	before := int64(0)
	for {
		summaries, err := store.List(ctx, session.ListOptions{
			Archived:         true,
			NoProject:        true,
			Limit:            reconciliationPageSize,
			BeforeNumber:     before,
			SortByNumberDesc: true,
		})
		if err != nil {
			return nil, fmt.Errorf("list unassigned sessions: %w", err)
		}
		if len(summaries) == 0 {
			break
		}
		for _, summary := range summaries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if summary.ProjectID != "" {
				continue
			}
			key := strings.TrimSpace(summary.CWD) + "\x00" + strings.TrimSpace(summary.WorktreeDir)
			matched, ok := matchesByWorkspace[key]
			if !ok {
				matched = MatchesWorkspace(ctx, summary.CWD, summary.WorktreeDir, root)
				matchesByWorkspace[key] = matched
			}
			if matched {
				matches = append(matches, session.ProjectSessionMatch{ID: summary.ID, CWD: summary.CWD, WorktreeDir: summary.WorktreeDir})
			}
		}
		before = summaries[len(summaries)-1].Number
		if len(summaries) < reconciliationPageSize || before <= 0 {
			break
		}
	}
	return matches, nil
}

// ClaimMatchingSessions assigns every still-unassigned matching session to p.
// It is idempotent and safe to race with session creation or another repair.
func ClaimMatchingSessions(ctx context.Context, store session.Store, p session.Project) (int, error) {
	projects, ok := session.AsProjectStore(store)
	if !ok {
		return 0, nil
	}
	matches, err := MatchingSessions(ctx, store, p)
	if err != nil || len(matches) == 0 {
		return 0, err
	}
	claimed, err := projects.ClaimProjectSessions(ctx, p.ID, matches)
	if err != nil {
		return claimed, fmt.Errorf("claim matching project sessions: %w", err)
	}
	return claimed, nil
}

// ReconcileAll claims historical unassigned sessions for every active,
// currently available project. Unavailable project paths are skipped so a later
// run can repair them when the filesystem is mounted again.
func ReconcileAll(ctx context.Context, store session.Store) (int, error) {
	projects, ok := session.AsProjectStore(store)
	if !ok {
		return 0, nil
	}
	list, err := projects.ListProjects(ctx, session.ProjectListOptions{})
	if err != nil {
		return 0, fmt.Errorf("list projects for reconciliation: %w", err)
	}
	total := 0
	var errs []error
	for _, p := range list {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		claimed, err := ClaimMatchingSessions(ctx, store, p)
		if err != nil {
			if errors.Is(err, ErrProjectUnavailable) || errors.Is(err, session.ErrNotFound) {
				continue
			}
			errs = append(errs, fmt.Errorf("reconcile project %s: %w", p.ID, err))
			continue
		}
		total += claimed
	}
	return total, errors.Join(errs...)
}

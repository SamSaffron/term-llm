package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

func (s *SQLiteStore) projectsAvailable() bool {
	return s.hasProjectID && s.hasProjectsTable && !s.cfg.ReadOnly
}

func scanProject(scanner interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	var archived sql.NullTime
	if err := scanner.Scan(&p.ID, &p.Name, &p.CanonicalDir, &p.IsBootstrap, &p.CreatedAt, &p.UpdatedAt, &p.LastUsedAt, &archived, &p.ConversationCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if archived.Valid {
		p.ArchivedAt = &archived.Time
	}
	return &p, nil
}

const projectSelectSQL = `
	SELECT p.id, p.name, p.canonical_dir, p.is_bootstrap,
	       p.created_at, p.updated_at, p.last_used_at, p.archived_at,
	       COUNT(s.id) AS conversation_count
	FROM projects p LEFT JOIN sessions s ON s.project_id = p.id AND s.archived = FALSE AND s.parent_id IS NULL`

func (s *SQLiteStore) ListProjects(ctx context.Context, opts ProjectListOptions) ([]Project, error) {
	if !s.hasProjectsTable || !s.hasProjectID {
		return nil, ErrProjectsUnsupported
	}
	query := projectSelectSQL
	if !opts.IncludeArchived {
		query += " WHERE p.archived_at IS NULL"
	}
	query += ` GROUP BY p.id ORDER BY p.archived_at IS NOT NULL, p.last_used_at DESC, LOWER(p.name), p.id`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	projects := make([]Project, 0)
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		projects = append(projects, *p)
	}
	return projects, rows.Err()
}

func (s *SQLiteStore) GetProject(ctx context.Context, id string) (*Project, error) {
	if !s.hasProjectsTable || !s.hasProjectID {
		return nil, ErrProjectsUnsupported
	}
	p, err := scanProject(s.db.QueryRowContext(ctx, projectSelectSQL+` WHERE p.id = ? GROUP BY p.id`, id))
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	return p, nil
}

func (s *SQLiteStore) GetProjectByCanonicalDir(ctx context.Context, canonicalDir string) (*Project, error) {
	if !s.hasProjectsTable || !s.hasProjectID {
		return nil, ErrProjectsUnsupported
	}
	p, err := scanProject(s.db.QueryRowContext(ctx, projectSelectSQL+` WHERE p.canonical_dir = ? GROUP BY p.id`, canonicalDir))
	if err != nil {
		return nil, fmt.Errorf("get project by canonical directory: %w", err)
	}
	return p, nil
}

func normalizeProjectForCreate(p *Project) error {
	if p == nil {
		return fmt.Errorf("project is nil")
	}
	p.Name = strings.TrimSpace(p.Name)
	p.CanonicalDir = strings.TrimSpace(p.CanonicalDir)
	if p.Name == "" || len([]rune(p.Name)) > 120 {
		return fmt.Errorf("project name must contain 1 to 120 characters")
	}
	if p.CanonicalDir == "" {
		return fmt.Errorf("canonical project directory is empty")
	}
	if p.ID == "" {
		id, err := NewProjectID()
		if err != nil {
			return err
		}
		p.ID = id
	}
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	if p.LastUsedAt.IsZero() {
		p.LastUsedAt = now
	}
	return nil
}

func (s *SQLiteStore) CreateProject(ctx context.Context, p *Project) error {
	if !s.projectsAvailable() {
		return ErrProjectsUnsupported
	}
	if err := normalizeProjectForCreate(p); err != nil {
		return err
	}
	return retryOnBusy(ctx, 5, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin create project: %w", err)
		}
		defer tx.Rollback()

		var existing Project
		var archived sql.NullTime
		err = tx.QueryRowContext(ctx, `SELECT id, name, canonical_dir, is_bootstrap, created_at, updated_at, last_used_at, archived_at FROM projects WHERE canonical_dir = ?`, p.CanonicalDir).
			Scan(&existing.ID, &existing.Name, &existing.CanonicalDir, &existing.IsBootstrap, &existing.CreatedAt, &existing.UpdatedAt, &existing.LastUsedAt, &archived)
		if err == nil {
			if archived.Valid {
				now := time.Now().UTC()
				name := p.Name
				if _, err := tx.ExecContext(ctx, `UPDATE projects SET name = ?, archived_at = NULL, updated_at = ?, last_used_at = ? WHERE id = ?`, name, now, now, existing.ID); err != nil {
					return fmt.Errorf("restore duplicate project: %w", err)
				}
				*p = existing
				p.Name = name
				p.ArchivedAt = nil
				p.UpdatedAt = now
				p.LastUsedAt = now
				return tx.Commit()
			}
			*p = existing
			return ErrProjectDuplicate
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check duplicate project: %w", err)
		}
		if p.IsBootstrap {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE is_bootstrap = 1`).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("bootstrap project already exists")
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO projects (id, name, canonical_dir, is_bootstrap, created_at, updated_at, last_used_at, archived_at) VALUES (?, ?, ?, ?, ?, ?, ?, NULL)`,
			p.ID, p.Name, p.CanonicalDir, p.IsBootstrap, p.CreatedAt.UTC(), p.UpdatedAt.UTC(), p.LastUsedAt.UTC())
		if err != nil {
			// The unique constraint is the final authority for concurrent creates.
			var existingID string
			if qerr := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE canonical_dir = ?`, p.CanonicalDir).Scan(&existingID); qerr == nil {
				p.ID = existingID
				return ErrProjectDuplicate
			}
			return fmt.Errorf("insert project: %w", err)
		}
		return tx.Commit()
	})
}

func (s *SQLiteStore) UpdateProject(ctx context.Context, id string, update ProjectUpdate) (*Project, error) {
	if !s.projectsAvailable() {
		return nil, ErrProjectsUnsupported
	}
	set := make([]string, 0, 3)
	args := make([]any, 0, 4)
	if update.Name != nil {
		name := strings.TrimSpace(*update.Name)
		if name == "" || len([]rune(name)) > 120 {
			return nil, fmt.Errorf("project name must contain 1 to 120 characters")
		}
		set = append(set, "name = ?")
		args = append(args, name)
	}
	if update.Archived != nil {
		set = append(set, "archived_at = ?")
		if *update.Archived {
			args = append(args, time.Now().UTC())
		} else {
			args = append(args, nil)
		}
	}
	if len(set) == 0 {
		return s.GetProject(ctx, id)
	}
	set = append(set, "updated_at = ?")
	args = append(args, time.Now().UTC(), id)
	result, err := s.db.ExecContext(ctx, `UPDATE projects SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("update project: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetProject(ctx, id)
}

// BootstrapProject inserts the first project and claims only the caller's
// prevalidated, unambiguous legacy sessions in one transaction.
func (s *SQLiteStore) BootstrapProject(ctx context.Context, p *Project, matchingSessions []ProjectSessionMatch) error {
	if !s.projectsAvailable() {
		return ErrProjectsUnsupported
	}
	p.IsBootstrap = true
	if err := normalizeProjectForCreate(p); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open bootstrap connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin project bootstrap: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var count int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		var existingID string
		if err := conn.QueryRowContext(ctx, `SELECT id FROM projects WHERE is_bootstrap = 1 LIMIT 1`).Scan(&existingID); err == nil {
			p.ID = existingID
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		committed = true
		return nil
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO projects (id, name, canonical_dir, is_bootstrap, created_at, updated_at, last_used_at) VALUES (?, ?, ?, 1, ?, ?, ?)`, p.ID, p.Name, p.CanonicalDir, p.CreatedAt.UTC(), p.UpdatedAt.UTC(), p.LastUsedAt.UTC()); err != nil {
		return fmt.Errorf("insert bootstrap project: %w", err)
	}
	for _, match := range matchingSessions {
		if _, err := conn.ExecContext(ctx, `
			UPDATE sessions SET project_id = ?
			WHERE id = ? AND project_id IS NULL
			  AND COALESCE(cwd, '') = ?
			  AND COALESCE(worktree_dir, '') = ?`,
			p.ID, match.ID, match.CWD, match.WorktreeDir); err != nil {
			return fmt.Errorf("backfill bootstrap project: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit project bootstrap: %w", err)
	}
	committed = true
	return nil
}

func (s *SQLiteStore) AssignSessionProject(ctx context.Context, sessionID, projectID, expectedCWD, expectedWorktreeDir string) error {
	if !s.projectsAvailable() {
		return ErrProjectsUnsupported
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET project_id = ?
		WHERE id = ? AND project_id IS NULL
		  AND COALESCE(cwd, '') = ?
		  AND COALESCE(worktree_dir, '') = ?`, projectID, sessionID, expectedCWD, expectedWorktreeDir)
	if err != nil {
		return fmt.Errorf("assign session project: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		var existing sql.NullString
		if err := s.db.QueryRowContext(ctx, `SELECT project_id FROM sessions WHERE id = ?`, sessionID).Scan(&existing); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		return ErrWorkspaceConflict
	}
	return nil
}

func (s *SQLiteStore) BindSessionWorkspace(ctx context.Context, sessionID string, binding SessionWorkspaceBinding) (*Session, error) {
	if !s.hasProjectID || s.cfg.ReadOnly {
		return nil, ErrProjectsUnsupported
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin workspace binding: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET project_id = CASE WHEN COALESCE(project_id, '') = '' THEN NULLIF(?, '') ELSE project_id END,
		    cwd = CASE WHEN COALESCE(cwd, '') = '' THEN NULLIF(?, '') ELSE cwd END,
		    worktree_dir = CASE WHEN COALESCE(worktree_dir, '') = '' THEN NULLIF(?, '') ELSE worktree_dir END,
		    updated_at = ?
		WHERE id = ?
		  AND (COALESCE(project_id, '') = '' OR COALESCE(project_id, '') = ?)
		  AND (COALESCE(cwd, '') = '' OR COALESCE(cwd, '') = ?)
		  AND (COALESCE(worktree_dir, '') = '' OR COALESCE(worktree_dir, '') = ?)`,
		binding.ProjectID, binding.CWD, binding.WorktreeDir, now, sessionID,
		binding.ProjectID, binding.CWD, binding.WorktreeDir)
	if err != nil {
		return nil, fmt.Errorf("bind session workspace: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id = ?`, sessionID).Scan(&count); err != nil {
			return nil, err
		}
		if count == 0 {
			return nil, ErrNotFound
		}
		return nil, ErrWorkspaceConflict
	}
	if binding.ProjectID != "" && s.hasProjectsTable {
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET last_used_at = ? WHERE id = ?`, now, binding.ProjectID); err != nil {
			return nil, fmt.Errorf("update project activity: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit workspace binding: %w", err)
	}
	return s.Get(ctx, sessionID)
}

// Sidebar uses one bounded window query for all groups after one project query;
// it never performs a session query per project.
func (s *SQLiteStore) Sidebar(ctx context.Context, opts SidebarOptions) ([]SidebarGroup, error) {
	if !s.hasProjectsTable || !s.hasProjectID {
		return nil, ErrProjectsUnsupported
	}
	if opts.PerProject <= 0 {
		opts.PerProject = 12
	}
	if opts.PerProject > 100 {
		opts.PerProject = 100
	}
	projects, err := s.ListProjects(ctx, ProjectListOptions{IncludeArchived: opts.IncludeArchivedProjects})
	if err != nil {
		return nil, err
	}
	groups := make(map[string]*SidebarGroup, len(projects)+1)
	for i := range projects {
		p := projects[i]
		groups[p.ID] = &SidebarGroup{Project: &p, Sessions: []SessionSummary{}}
	}
	archiveClause := "AND s.archived = FALSE"
	if opts.IncludeArchivedSessions {
		archiveClause = ""
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH ranked AS (
			SELECT s.id, s.number, s.name, s.summary, s.generated_short_title,
			       s.generated_long_title, s.title_source, s.provider,
			       COALESCE(s.provider_key, '') AS provider_key, s.model, s.mode,
			       COALESCE(NULLIF(TRIM(s.origin), ''), 'tui') AS origin, s.archived,
			       COALESCE(s.pinned, FALSE) AS pinned, s.message_count, s.transcript_rev,
			       s.status, COALESCE(s.worktree_dir, '') AS worktree_dir, COALESCE(s.project_id, '') AS project_id,
			       COALESCE(p.name, '') AS project_name, s.created_at, s.updated_at, s.last_message_at, s.last_user_message_at,
			       COUNT(*) OVER (PARTITION BY s.project_id) AS group_count,
			       ROW_NUMBER() OVER (
				   PARTITION BY s.project_id
				   ORDER BY COALESCE(s.last_message_at, s.last_user_message_at, s.created_at) DESC,
				            s.number DESC
			       ) AS activity_rn,
			       ROW_NUMBER() OVER (
				   PARTITION BY s.project_id
				   ORDER BY COALESCE(s.pinned, FALSE) DESC,
				            COALESCE(s.last_message_at, s.last_user_message_at, s.created_at) DESC,
				            s.number DESC
			       ) AS rn
			FROM sessions s LEFT JOIN projects p ON p.id = s.project_id
			WHERE s.parent_id IS NULL `+archiveClause+`
		)
		SELECT id, number, name, summary, generated_short_title, generated_long_title,
		       title_source, provider, provider_key, model, mode, origin, archived, pinned,
		       message_count, transcript_rev, status, worktree_dir, project_id, project_name,
		       created_at, updated_at, last_message_at, last_user_message_at, group_count, rn, activity_rn
		FROM ranked WHERE rn <= ? OR activity_rn = 1
		ORDER BY project_id, rn`, opts.PerProject+1)
	if err != nil {
		return nil, fmt.Errorf("query sidebar: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sum SessionSummary
		var mode, origin, status, projectID string
		var number sql.NullInt64
		var shortTitle, longTitle, titleSource sql.NullString
		var lastMessage, lastUserMessage sql.NullTime
		var groupCount, rank, activityRank int
		if err := rows.Scan(&sum.ID, &number, &sum.Name, &sum.Summary, &shortTitle, &longTitle,
			&titleSource, &sum.Provider, &sum.ProviderKey, &sum.Model, &mode, &origin,
			&sum.Archived, &sum.Pinned, &sum.MessageCount, &sum.TranscriptRev, &status,
			&sum.WorktreeDir, &projectID, &sum.ProjectName, &sum.CreatedAt, &sum.UpdatedAt,
			&lastMessage, &lastUserMessage, &groupCount, &rank, &activityRank); err != nil {
			return nil, fmt.Errorf("scan sidebar session: %w", err)
		}
		sum.Number = number.Int64
		sum.GeneratedShortTitle = shortTitle.String
		sum.GeneratedLongTitle = longTitle.String
		sum.TitleSource = SessionTitleSource(titleSource.String)
		sum.Mode = SessionMode(mode)
		sum.Origin = SessionOrigin(origin)
		sum.Status = SessionStatus(status)
		sum.ProjectID = projectID
		if lastMessage.Valid {
			sum.LastMessageAt = lastMessage.Time
		} else if lastUserMessage.Valid {
			sum.LastMessageAt = lastUserMessage.Time
		} else {
			sum.LastMessageAt = sum.CreatedAt
		}
		group, ok := groups[projectID]
		if !ok {
			if projectID != "" {
				// Sessions for unavailable historical metadata remain visible only if
				// their project row is included.
				continue
			}
			group = &SidebarGroup{NoProject: true, Sessions: []SessionSummary{}}
			groups[""] = group
		}
		group.SessionCount = groupCount
		if sum.LastMessageAt.After(group.LastActivity) {
			group.LastActivity = sum.LastMessageAt
		} else if sum.LastMessageAt.IsZero() && sum.CreatedAt.After(group.LastActivity) {
			group.LastActivity = sum.CreatedAt
		}
		if rank <= opts.PerProject {
			group.Sessions = append(group.Sessions, sum)
		}
		if groupCount > opts.PerProject && rank == opts.PerProject+1 && len(group.Sessions) != 0 {
			group.NextCursor = EncodeProjectSessionCursor(group.Sessions[len(group.Sessions)-1])
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]SidebarGroup, 0, len(groups))
	for _, group := range groups {
		out = append(out, *group)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := out[i], out[j]
		if ai.NoProject != aj.NoProject {
			return !ai.NoProject
		}
		if ai.Project != nil && aj.Project != nil && ai.Project.Archived() != aj.Project.Archived() {
			return !ai.Project.Archived()
		}
		if !ai.LastActivity.Equal(aj.LastActivity) {
			return ai.LastActivity.After(aj.LastActivity)
		}
		ni, nj := "", ""
		if ai.Project != nil {
			ni = strings.ToLower(ai.Project.Name) + ai.Project.ID
		}
		if aj.Project != nil {
			nj = strings.ToLower(aj.Project.Name) + aj.Project.ID
		}
		return ni < nj
	})
	return out, nil
}

package session

import (
	"context"
	"fmt"
	"strings"
	"time"
)

var _ WorkspaceGrantStore = (*SQLiteStore)(nil)

// ListWorkspaceGrants returns workspace capability records (including the
// reserved primary decision row when present) for sessionID in deterministic
// creation order.
func (s *SQLiteStore) ListWorkspaceGrants(ctx context.Context, sessionID string) ([]WorkspaceGrant, error) {
	if s == nil || s.db == nil || strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, path, access, provenance, rationale, created_at, updated_at
		FROM session_workspace_grants
		WHERE session_id = ?
		ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list workspace grants: %w", err)
	}
	defer rows.Close()

	var grants []WorkspaceGrant
	for rows.Next() {
		var grant WorkspaceGrant
		if err := rows.Scan(&grant.ID, &grant.Path, &grant.Access, &grant.Provenance, &grant.Rationale, &grant.CreatedAt, &grant.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan workspace grant: %w", err)
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workspace grants: %w", err)
	}
	return grants, nil
}

// SaveWorkspaceGrant inserts or updates a session-scoped workspace capability.
func (s *SQLiteStore) SaveWorkspaceGrant(ctx context.Context, sessionID string, grant WorkspaceGrant) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save workspace grant: store is unavailable")
	}
	sessionID = strings.TrimSpace(sessionID)
	grant.ID = strings.TrimSpace(grant.ID)
	grant.Path = strings.TrimSpace(grant.Path)
	grant.Provenance = strings.TrimSpace(grant.Provenance)
	grant.Rationale = strings.TrimSpace(grant.Rationale)
	if sessionID == "" || grant.ID == "" || grant.Path == "" {
		return fmt.Errorf("save workspace grant: session, id, and path are required")
	}
	if grant.Provenance == "" || grant.Rationale == "" {
		return fmt.Errorf("save workspace grant: provenance and rationale are required")
	}
	if grant.Access != WorkspaceAccessRead && grant.Access != WorkspaceAccessWrite {
		return fmt.Errorf("save workspace grant: invalid access %q", grant.Access)
	}
	now := time.Now()
	if grant.CreatedAt.IsZero() {
		grant.CreatedAt = now
	}
	if grant.UpdatedAt.IsZero() {
		grant.UpdatedAt = now
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO session_workspace_grants (
			session_id, id, path, access, provenance, rationale, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, id) DO UPDATE SET
			path = excluded.path,
			access = excluded.access,
			provenance = excluded.provenance,
			rationale = excluded.rationale,
			updated_at = excluded.updated_at`,
		sessionID, grant.ID, grant.Path, grant.Access, grant.Provenance, grant.Rationale, grant.CreatedAt, grant.UpdatedAt)
	if err != nil {
		return fmt.Errorf("save workspace grant: %w", err)
	}
	return nil
}

// DeleteWorkspaceGrant removes a persisted workspace capability record. Runtime
// policy prevents the model-facing manage_workspace tool from deleting the
// reserved primary row; direct session rebinding may replace or remove it.
func (s *SQLiteStore) DeleteWorkspaceGrant(ctx context.Context, sessionID, grantID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("delete workspace grant: store is unavailable")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM session_workspace_grants WHERE session_id = ? AND id = ?`, strings.TrimSpace(sessionID), strings.TrimSpace(grantID))
	if err != nil {
		return fmt.Errorf("delete workspace grant: %w", err)
	}
	if affected, err := result.RowsAffected(); err == nil && affected == 0 {
		return ErrNotFound
	}
	return nil
}

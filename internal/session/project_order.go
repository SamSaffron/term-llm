package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrProjectOrderInvalid means a project reorder request was empty,
// oversized, or listed a project more than once.
var ErrProjectOrderInvalid = errors.New("session: invalid project order")

// MaxProjectOrderIDs bounds one project reorder request.
const MaxProjectOrderIDs = 1000

// ProjectPosition is one project's persisted 1-based sidebar rank.
type ProjectPosition struct {
	ID        string `json:"id"`
	SortOrder int64  `json:"sort_order"`
}

// ProjectOrder is the complete project order committed by a reorder.
type ProjectOrder struct {
	// Positions lists every project, including archived ones and ones the
	// caller did not name, in rank order.
	Positions []ProjectPosition
	// Changed lists the projects whose stored rank changed.
	Changed []string
}

func (s *SQLiteStore) projectOrderWritable() bool {
	return s.projectsAvailable() && s.hasProjectSortOrder
}

// projectRankSQL is a project's persisted sidebar rank key. Like an unranked
// pin, a project without a rank follows every ranked project.
func projectRankSQL(alias string) string {
	return "COALESCE(" + qualifiedMessageColumn(alias, "sort_order") + ", " + unrankedPinKeySQL + ")"
}

// projectRankKey is the Go value of projectRankSQL.
func projectRankKey(p Project) int64 {
	if p.SortOrder > 0 {
		return p.SortOrder
	}
	return unrankedPinKey
}

// projectSortOrderCol selects the persisted rank, or NULL on a database that
// predates it.
func (s *SQLiteStore) projectSortOrderCol(alias string) string {
	if !s.hasProjectSortOrder {
		return "NULL"
	}
	return qualifiedMessageColumn(alias, "sort_order")
}

// projectListOrderSQL orders projects as the sidebar shows them: active
// projects before archived ones, each in their persisted rank. Last use and
// then name order only projects without a rank, which is every project on a
// read-only database that predates ranks.
func (s *SQLiteStore) projectListOrderSQL(alias string) string {
	column := func(name string) string { return qualifiedMessageColumn(alias, name) }
	order := column("archived_at") + " IS NOT NULL, "
	if s.hasProjectSortOrder {
		order += projectRankSQL(alias) + ", "
	}
	return order + column("last_used_at") + " DESC, LOWER(" + column("name") + "), " + column("id")
}

// projectRankInsert returns the INSERT column and value that append a new
// project after every existing one, archived projects included.
func (s *SQLiteStore) projectRankInsert() (column, value string) {
	if !s.hasProjectSortOrder {
		return "", ""
	}
	return ", sort_order", ", (SELECT COALESCE(MAX(sort_order), 0) + 1 FROM projects)"
}

// sidebarGroupLess orders sidebar groups: projects before the no-project
// group and active projects before archived ones, each in their persisted
// rank. Conversation activity orders only projects without a rank, so new
// messages never move a ranked project.
func sidebarGroupLess(a, b SidebarGroup) bool {
	if a.NoProject != b.NoProject {
		return !a.NoProject
	}
	if a.Project == nil || b.Project == nil {
		return a.Project != nil
	}
	if a.Project.Archived() != b.Project.Archived() {
		return !a.Project.Archived()
	}
	if ra, rb := projectRankKey(*a.Project), projectRankKey(*b.Project); ra != rb {
		return ra < rb
	}
	if !a.LastActivity.Equal(b.LastActivity) {
		return a.LastActivity.After(b.LastActivity)
	}
	return strings.ToLower(a.Project.Name)+a.Project.ID < strings.ToLower(b.Project.Name)+b.Project.ID
}

// ReorderProjects implements ProjectStore.
func (s *SQLiteStore) ReorderProjects(ctx context.Context, orderedIDs []string) (ProjectOrder, error) {
	ids, err := normalizeOrderIDs(orderedIDs, MaxProjectOrderIDs, ErrProjectOrderInvalid, "project")
	if err != nil {
		return ProjectOrder{}, err
	}
	if !s.projectOrderWritable() {
		return ProjectOrder{}, ErrProjectsUnsupported
	}
	var order ProjectOrder
	err = retryOnBusy(ctx, 5, func() error {
		var err error
		order, err = s.reorderProjectsTx(ctx, ids)
		return err
	})
	if err != nil {
		return ProjectOrder{}, fmt.Errorf("reorder projects: %w", err)
	}
	return order, nil
}

func (s *SQLiteStore) reorderProjectsTx(ctx context.Context, ids []string) (ProjectOrder, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProjectOrder{}, err
	}
	defer tx.Rollback()

	// Every project, archived ones included, in rank order. Archived projects
	// list after active ones but keep their ranks among them, so restoring a
	// project returns it to its place.
	current, err := loadRankedRowsTx(ctx, tx, `
		SELECT id, sort_order FROM projects
		ORDER BY `+projectRankSQL("")+` ASC, last_used_at DESC, LOWER(name), id`)
	if err != nil {
		return ProjectOrder{}, fmt.Errorf("load projects: %w", err)
	}
	if err := validateProjectOrder(current, ids); err != nil {
		return ProjectOrder{}, err
	}
	positions, changed, err := writeOrderTx(ctx, tx, `UPDATE projects SET sort_order = ? WHERE id = ?`, current, permuteOrder(current, ids))
	if err != nil {
		return ProjectOrder{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProjectOrder{}, err
	}
	order := ProjectOrder{Positions: make([]ProjectPosition, 0, len(positions)), Changed: changed}
	for _, row := range positions {
		order.Positions = append(order.Positions, ProjectPosition{ID: row.id, SortOrder: row.rank})
	}
	return order, nil
}

func validateProjectOrder(current []rankedRow, ids []string) error {
	known := make(map[string]struct{}, len(current))
	for _, row := range current {
		known[row.id] = struct{}{}
	}
	for _, id := range ids {
		if _, ok := known[id]; !ok {
			return fmt.Errorf("project not found %s: %w", id, ErrNotFound)
		}
	}
	return nil
}

package session

import (
	"fmt"

	"github.com/samsaffron/term-llm/internal/sqliteutil"
)

// migrateProjectOrderV61 adds projects.sort_order, ranks existing projects,
// and installs projectOrderSchemaV61. Fresh bootstrap runs it too, because the
// frozen projectsSchemaV47 table definition predates the column.
func migrateProjectOrderV61(db schemaExecutor) error {
	exists, err := sqliteutil.ColumnExists(db, "projects", "sort_order")
	if err != nil {
		return fmt.Errorf("inspect projects.sort_order: %w", err)
	}
	if !exists {
		if _, err := db.Exec("ALTER TABLE projects ADD COLUMN sort_order INTEGER"); err != nil {
			return fmt.Errorf("add projects.sort_order: %w", err)
		}
	}
	if err := backfillProjectOrderV61(db); err != nil {
		return err
	}
	if _, err := db.Exec(projectOrderSchemaV61); err != nil {
		return fmt.Errorf("install project order change trigger: %w", err)
	}
	return nil
}

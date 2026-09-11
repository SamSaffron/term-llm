package cmd

import (
	"context"
	"io"
	"log"
	"time"

	"github.com/spf13/cobra"

	"github.com/samsaffron/term-llm/internal/config"
	projectpkg "github.com/samsaffron/term-llm/internal/project"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/session"
)

func initializeServeProjectRuntime(ctx context.Context, cmd *cobra.Command, cfg *config.Config, store session.Store, startupDir string, hasWeb bool, errWriter io.Writer) (bool, string, error) {
	projectsRequested, projectsStrict := resolveServeProjectsRequested(
		cmd.Flags().Changed("projects"),
		serveProjects,
		cmd.Flags().Changed("no-projects") && serveNoProjects,
		cfg.Serve.Projects.Enabled,
		hasWeb,
	)
	projectsEnabled, bootstrapProjectID, err := initializeServeProjects(ctx, store, startupDir, projectsRequested, projectsStrict, errWriter)
	if err != nil {
		return false, "", err
	}
	if hasWeb {
		switch {
		case projectsEnabled:
			projectCount := 0
			if projectStore, ok := session.AsProjectStore(store); ok {
				if projects, listErr := projectStore.ListProjects(ctx, session.ProjectListOptions{IncludeArchived: true}); listErr == nil {
					projectCount = len(projects)
				}
			}
			bootstrapCount := 0
			if bootstrapProjectID != "" {
				bootstrapCount = 1
			}
			log.Printf("projects enabled (projects=%d bootstrap=%d bootstrap_id=%s)", projectCount, bootstrapCount, bootstrapProjectID)
		case cmd.Flags().Changed("no-projects") || !cfg.Serve.Projects.Enabled:
			log.Printf("projects explicitly disabled")
		default:
			log.Printf("projects auto-disabled")
		}
	}
	if projectsEnabled {
		_ = restart.Default.Go(ctx, func(ctx context.Context) {
			reconcileCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			claimed, reconcileErr := projectpkg.ReconcileAll(reconcileCtx, store)
			if reconcileErr != nil {
				log.Printf("project history reconciliation failed: %v", reconcileErr)
			} else if claimed > 0 {
				log.Printf("project history reconciled (claimed=%d)", claimed)
			}
		})
	}
	return projectsEnabled, bootstrapProjectID, nil
}

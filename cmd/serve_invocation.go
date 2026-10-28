package cmd

import (
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"

	"github.com/spf13/cobra"
)

type serveFlagState = appserve.FlagState

func captureServeInvocation(cmd *cobra.Command, opts *ServeOptions) appserve.Invocation {
	opts.AllowProjectSkillTools = projectSkillToolsOverride(cmd)
	flags := cmd.Flags()
	approval, auto, yolo := opts.Approval, opts.Auto, opts.Yolo
	return appserve.Invocation{
		Flags: serveFlagState{
			Projects:               flags.Changed("projects"),
			NoProjects:             flags.Changed("no-projects"),
			ResponseTimeout:        flags.Changed("response-timeout"),
			Auth:                   flags.Changed("auth"),
			NoAuth:                 flags.Changed("no-auth") || flags.Changed("allow-no-auth"),
			BasePath:               flags.Changed("base-path"),
			Title:                  flags.Changed("title"),
			DisableLocationSharing: flags.Changed("disable-location-sharing"),
			MaxTurns:               flags.Changed("max-turns"),
			Tools:                  flags.Changed("tools"),
			// This intentionally preserves the historical mismatch: the bound flag
			// is system-message, while serve has always queried system.
			System:        flags.Changed("system"),
			Extensions:    flags.Changed("extensions"),
			EnableWidgets: flags.Changed("enable-widgets"),
		},
		Stdout:       cmd.OutOrStdout(),
		Stderr:       cmd.ErrOrStderr(),
		DebugRaw:     debugRaw,
		SessionStore: sessionStoreOverrides(),
		MemoryPaths:  memoryPaths(),
		ResolveApproval: func(cfg *config.Config) (resolvedApprovalMode, error) {
			return resolveCommandApprovalMode(cmd, approvalSurfaceServe, cfg, nil, approval, auto, yolo)
		},
	}
}

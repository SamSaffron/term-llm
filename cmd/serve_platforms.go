package cmd

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/restart"
	platform "github.com/samsaffron/term-llm/internal/serve"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/tools"
)

const (
	servePlatformInitialRetryDelay = time.Second
	servePlatformMaxRetryDelay     = 30 * time.Second
	servePlatformRetryResetAfter   = time.Minute
)

func runPlatformSupervisor(ctx context.Context, cfg *config.Config, settings platform.Settings, platform platform.Platform, initialRetryDelay time.Duration, injectedDelay ...servePlatformDelay) {
	delay := waitServePlatformRetry
	if len(injectedDelay) > 0 && injectedDelay[0] != nil {
		delay = injectedDelay[0]
	}
	retryDelay := initialRetryDelay
	for {
		startedAt := time.Now()
		attemptCtx, release, admitErr := restart.Default.Root(ctx)
		if admitErr != nil {
			if restart.Default.WaitReady(ctx) != nil {
				return
			}
			continue
		}
		attemptSettings := settings
		attemptSettings.Ready = func() {
			release()
			if settings.Ready != nil {
				settings.Ready()
			}
		}
		err := platform.Run(attemptCtx, cfg, attemptSettings)
		attemptSettings.Ready()
		if ctx.Err() != nil {
			return
		}

		if time.Since(startedAt) >= servePlatformRetryResetAfter {
			retryDelay = initialRetryDelay
		}
		if err != nil {
			log.Printf("[%s] error: %v; retrying in %s", platform.Name(), err, retryDelay)
		} else {
			log.Printf("[%s] stopped unexpectedly; retrying in %s", platform.Name(), retryDelay)
		}

		if !delay(ctx, retryDelay) {
			return
		}

		if retryDelay < servePlatformMaxRetryDelay {
			retryDelay *= 2
			if retryDelay > servePlatformMaxRetryDelay {
				retryDelay = servePlatformMaxRetryDelay
			}
		}
	}
}

func waitServePlatformRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	select {
	case <-ctx.Done():
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		return false
	case <-timer.C:
		return true
	}
}

// newServeEngineWithTools creates a new engine with tools wired up for serving.
// skillsSetup must be pre-initialized by the caller (e.g. via SetupSkills); this
// function registers the activate_skill tool on the engine but does NOT inject
// <available_skills> metadata into the system prompt — the caller must do that
// before constructing serveRuntime/serveSettings so the mutation is not lost.
func newServeEngineWithTools(cfg *config.Config, settings SessionSettings, provider core.Provider, providerName, modelName string, yoloMode bool, autoMode bool, wireSpawn func(*config.Config, *tools.ToolManager, bool) error, skillsSetup *skills.Setup) (*engine.Engine, *tools.ToolManager, error) {
	mode := tools.ModePrompt
	if yoloMode {
		mode = tools.ModeYolo
	} else if autoMode {
		mode = tools.ModeAuto
	}
	return newServeEngineWithToolsMode(cfg, settings, provider, providerName, modelName, resolvedApprovalMode{Mode: mode, Source: approvalModeSourceCLI}, approvalRuntimeOptions{Headless: true}, wireSpawn, skillsSetup)
}

func newServeEngineWithToolsMode(cfg *config.Config, settings SessionSettings, provider core.Provider, providerName, modelName string, resolved resolvedApprovalMode, runtimeOpts approvalRuntimeOptions, wireSpawn func(*config.Config, *tools.ToolManager, bool) error, skillsSetup *skills.Setup) (*engine.Engine, *tools.ToolManager, error) {
	engine := newEngine(provider, cfg)
	settings.Provider = providerName
	settings.Model = modelName

	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		return nil, nil, err
	}
	if toolMgr != nil {
		if err := applyResolvedApprovalMode(cfg, toolMgr.ApprovalMgr, resolved, runtimeOpts); err != nil {
			return nil, nil, err
		}
		if wireSpawn != nil {
			if err := wireSpawn(cfg, toolMgr, resolved.Mode == tools.ModeYolo); err != nil {
				toolMgr.ApprovalMgr.Close()
				return nil, nil, err
			}
		}
	}

	// Serve runtimes need the same context tracking/auto-compaction setup as ask/TUI
	// so long-lived sessions can warn or compact before hitting provider limits.
	engine.ConfigureContextManagement(provider, providerName, modelName, cfg.AutoCompact)

	// Register the activate_skill tool on the engine. Metadata injection into the
	// system prompt is handled by the caller to avoid the by-value settings copy trap.
	RegisterSkillToolWithEngine(engine, toolMgr, skillsSetup)

	return engine, toolMgr, nil
}

// resolvePlatforms returns the list of platforms to serve. Positional args
// take precedence; if none are given, configPlatforms (from config.yaml
// serve.platforms) is used as fallback.
func resolvePlatforms(args []string, configPlatforms []string) ([]string, error) {
	return platform.ResolvePlatforms(args, configPlatforms)
}

func platformContains(platforms []string, name string) bool {
	return platform.PlatformContains(platforms, name)
}

// singleServeTemplatePlatform returns a stable platform token for serve prompts.
// If multiple runtime surfaces are selected (for example web+telegram), returns
// empty so {{platform}} stays unexpanded and is not misleading.
func singleServeTemplatePlatform(platforms []string) string {
	unique := make(map[string]struct{})
	for _, p := range platforms {
		switch p {
		case "web", "api", "telegram", "jobs":
			unique[p] = struct{}{}
		}
	}
	if len(unique) != 1 {
		return ""
	}
	for p := range unique {
		return p
	}
	return ""
}

func authSummary(required bool) string {
	if required {
		return "bearer required"
	}
	return "disabled"
}

var validSidebarSessionCategories = map[string]bool{
	"all":  true,
	"chat": true,
	"web":  true,
	"ask":  true,
	"exec": true,
}

func parseSidebarSessionCategories(raw string, defaultAll bool) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if defaultAll {
			return []string{"all"}, nil
		}
		return nil, nil
	}

	parts := strings.Split(raw, ",")
	seen := make(map[string]bool, len(parts))
	categories := make([]string, 0, len(parts))
	for _, part := range parts {
		category := strings.ToLower(strings.TrimSpace(part))
		if category == "" {
			continue
		}
		if !validSidebarSessionCategories[category] {
			return nil, fmt.Errorf("invalid --sidebar-sessions value %q (valid: all, chat, web, ask, plan, exec)", category)
		}
		if category == "all" {
			return []string{"all"}, nil
		}
		if !seen[category] {
			seen[category] = true
			categories = append(categories, category)
		}
	}
	if len(categories) == 0 {
		if defaultAll {
			return []string{"all"}, nil
		}
		return nil, nil
	}
	return categories, nil
}

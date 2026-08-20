package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/guardian"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

var newGuardianProviderByName = llm.NewProviderByName

const guardianReviewerPoolSize = 3

func preflightHeadlessApproval(cfg *config.Config, resolved resolvedApprovalMode, providerName, modelName string) error {
	if resolved.Mode != tools.ModeAuto {
		return nil
	}
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	defer mgr.Close()
	return applyResolvedApprovalMode(cfg, mgr, resolved, providerName, modelName, approvalRuntimeOptions{Headless: true})
}

// applyResolvedApprovalMode applies requested policy to the actual runtime
// manager. Interactive auto setup degrades to prompt with one warning; headless
// setup fails before work begins. The resolved value remains unchanged so
// callers can persist the requested policy rather than a temporary fallback.
func applyResolvedApprovalMode(cfg *config.Config, approvalMgr *tools.ApprovalManager, resolved resolvedApprovalMode, providerName, modelName string, opts approvalRuntimeOptions) error {
	if approvalMgr == nil {
		// A runtime with no approval-bearing tools has nothing to initialize or
		// review, so even headless auto can start without a manager.
		return nil
	}
	approvalMgr.SetGuardianClassifyAllShell(cfg != nil && cfg.Guardian.ClassifyAllShell)
	approvalMgr.SetApprovalMode(tools.ModePrompt)
	if resolved.Mode == tools.ModeYolo {
		approvalMgr.SetApprovalMode(tools.ModeYolo)
		return nil
	}

	needsGuardian := resolved.Mode == tools.ModeAuto || opts.PrepareCallbacks
	guardianAvailable := true
	if needsGuardian {
		if err := installGuardianReviewerCallbacks(cfg, approvalMgr, providerName, modelName, opts.Headless); err != nil {
			guardianAvailable = false
			if resolved.Mode == tools.ModeAuto {
				if opts.Headless {
					return fmt.Errorf("auto approval unavailable: %w", err)
				}
				if opts.WarningWriter != nil {
					fmt.Fprintf(opts.WarningWriter, "warning: guardian auto-approval unavailable; using prompt mode: %v\n", err)
				}
			} else if opts.PrepareCallbacks && opts.WarningWriter != nil {
				fmt.Fprintf(opts.WarningWriter, "warning: guardian auto-approval unavailable; auto toggle disabled: %v\n", err)
			}
		}
	}
	if resolved.Mode == tools.ModeAuto && guardianAvailable {
		approvalMgr.SetApprovalMode(tools.ModeAuto)
	}
	return nil
}

func addGuardianUsage(stats *ui.SessionStats, event tools.GuardianEvent) bool {
	if stats == nil || event.Usage.BillableCountersZero() {
		return false
	}
	u := event.Usage
	stats.AddGuardianUsageForModel(event.Model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	return true
}

func installGuardianReviewerCallbacks(cfg *config.Config, approvalMgr *tools.ApprovalManager, providerName string, modelName string, headless bool) error {
	if approvalMgr == nil {
		return nil
	}
	fail := func(err error) error {
		approvalMgr.SetPolicyReviewFunc(nil, nil)
		if approvalMgr.ApprovalMode() == tools.ModeAuto {
			approvalMgr.SetApprovalMode(tools.ModePrompt)
		}
		return err
	}
	if cfg == nil {
		return fail(fmt.Errorf("auto approval requires configuration and an LLM provider"))
	}
	approvalMgr.SetAutoHeadless(headless)
	target, err := resolveGuardianTarget(cfg, providerName, modelName)
	if err != nil {
		return fail(err)
	}
	policy, err := guardian.LoadPolicy(cfg.Guardian.PolicyPath)
	if err != nil {
		return fail(fmt.Errorf("load guardian policy: %w", err))
	}

	var providerFactoryMu sync.Mutex
	newReviewer := func() (*guardian.Reviewer, error) {
		// NewProviderByName resolves and caches credentials in cfg. ReviewerPool
		// may expand from multiple goroutines, so provider construction must not
		// access that shared config map concurrently.
		providerFactoryMu.Lock()
		provider, err := newGuardianProviderByName(cfg, target.Provider, target.Model)
		providerFactoryMu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("guardian provider: %w", err)
		}
		if provider == nil {
			return nil, fmt.Errorf("auto approval requires an LLM provider")
		}
		reviewer := &guardian.Reviewer{Provider: provider, Model: target.Model, Policy: policy}
		if cfg.Guardian.TimeoutSeconds > 0 {
			reviewer.Timeout = time.Duration(cfg.Guardian.TimeoutSeconds) * time.Second
		}
		return reviewer, nil
	}
	reviewerPool, err := guardian.NewReviewerPool(guardianReviewerPoolSize, newReviewer)
	if err != nil {
		return fail(err)
	}

	reviewFunc := func(ctx context.Context, req tools.PolicyReviewRequest) (tools.PolicyDecision, error) {
		transcript := make([]guardian.TranscriptEntry, 0, len(req.Transcript))
		for _, e := range req.Transcript {
			transcript = append(transcript, guardian.TranscriptEntry{Role: e.Role, Text: e.Text})
		}
		decision, err := reviewerPool.Review(ctx, guardian.Request{
			Command: req.Command, WorkDir: req.WorkDir, ToolName: req.ToolName, Path: req.Path,
			Selector: req.Selector, IsWrite: req.IsWrite, IsDirectory: req.IsDirectory,
			Transcript: transcript, ApprovalContext: req.ApprovalContext, ScopeID: req.ScopeID,
			WorkspaceAccess: req.WorkspaceAccess, Reason: req.Reason,
		})
		result := tools.PolicyDecision{Allowed: decision.Allowed(), RiskLevel: decision.RiskLevel, UserAuthorization: decision.UserAuthorization, Rationale: decision.Rationale, Model: decision.Model, Usage: decision.Usage}
		return result, err
	}
	approvalMgr.SetPolicyReviewFunc(reviewFunc, reviewerPool.Close)
	if approvalMgr.GuardianEventFunc == nil {
		approvalMgr.GuardianEventFunc = func(event tools.GuardianEvent) {
			writeGuardianStatus(os.Stderr, event)
		}
	}
	return nil
}

type guardianTarget struct {
	Provider string
	Model    string
}

func resolveGuardianTarget(cfg *config.Config, activeProvider, activeModel string) (guardianTarget, error) {
	if cfg == nil {
		return guardianTarget{}, fmt.Errorf("auto approval requires configuration and an LLM provider")
	}
	activeProvider = strings.TrimSpace(activeProvider)
	activeModel = strings.TrimSpace(activeModel)
	providerName := strings.TrimSpace(cfg.Guardian.Provider)
	if providerName == "" {
		providerName = activeProvider
	}
	if providerName == "" {
		providerName = strings.TrimSpace(cfg.DefaultProvider)
	}
	if providerName == "" {
		return guardianTarget{}, fmt.Errorf("auto approval requires an LLM provider")
	}

	// An explicit model is authoritative and remains paired with the explicitly
	// selected Guardian provider, or with the active/default provider.
	if model := strings.TrimSpace(cfg.Guardian.Model); model != "" {
		return guardianTarget{Provider: providerName, Model: model}, nil
	}

	providerConfig := cfg.GetProviderConfig(providerName)
	if providerConfig != nil {
		if model := strings.TrimSpace(providerConfig.FastModel); model != "" {
			targetProvider := providerName
			if fastProvider := strings.TrimSpace(providerConfig.FastProvider); fastProvider != "" {
				targetProvider = fastProvider
			}
			return guardianTarget{Provider: targetProvider, Model: model}, nil
		}
		if model := strings.TrimSpace(providerConfig.Model); model != "" {
			return guardianTarget{Provider: providerName, Model: model}, nil
		}
	}

	providerType := config.ProviderType("")
	if providerConfig != nil {
		providerType = providerConfig.Type
	}
	providerType = config.InferProviderType(providerName, providerType)
	if model := strings.TrimSpace(llm.ProviderFastModels[string(providerType)]); model != "" {
		return guardianTarget{Provider: providerName, Model: model}, nil
	}
	if providerName == activeProvider && activeModel != "" {
		return guardianTarget{Provider: providerName, Model: activeModel}, nil
	}
	return guardianTarget{}, fmt.Errorf("guardian provider %q has no configured model or built-in fast model; set guardian.model", providerName)
}

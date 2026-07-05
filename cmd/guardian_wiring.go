package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/guardian"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

func installGuardianReviewer(cfg *config.Config, approvalMgr *tools.ApprovalManager, provider llm.Provider, fastProvider llm.Provider, modelName string, headless bool) error {
	if err := installGuardianReviewerCallbacks(cfg, approvalMgr, provider, fastProvider, modelName, headless); err != nil {
		return err
	}
	approvalMgr.SetApprovalMode(tools.ModeAuto)
	return nil
}

func installGuardianReviewerCallbacks(cfg *config.Config, approvalMgr *tools.ApprovalManager, provider llm.Provider, fastProvider llm.Provider, modelName string, headless bool) error {
	if approvalMgr == nil || cfg == nil {
		return nil
	}
	approvalMgr.SetAutoHeadless(headless)
	reviewProvider := provider
	model := resolveGuardianModelName(cfg, modelName)
	if strings.TrimSpace(cfg.Guardian.Provider) != "" {
		guardianName := strings.TrimSpace(cfg.Guardian.Provider)
		var err error
		reviewProvider, err = llm.NewProviderByName(cfg, guardianName, model)
		if err != nil {
			return fmt.Errorf("guardian provider: %w", err)
		}
	} else if reviewProvider == nil {
		reviewProvider = fastProvider
	}
	if reviewProvider == nil {
		return fmt.Errorf("auto approval requires an LLM provider")
	}
	policy, err := guardian.LoadPolicy(cfg.Guardian.PolicyPath)
	if err != nil {
		return fmt.Errorf("load guardian policy: %w", err)
	}
	reviewer := guardian.Reviewer{Provider: reviewProvider, Model: model, Policy: policy}
	if cfg.Guardian.TimeoutSeconds > 0 {
		reviewer.Timeout = time.Duration(cfg.Guardian.TimeoutSeconds) * time.Second
	}
	approvalMgr.PolicyReviewFunc = func(ctx context.Context, req tools.PolicyReviewRequest) (tools.PolicyDecision, error) {
		transcript := make([]guardian.TranscriptEntry, 0, len(req.Transcript))
		for _, e := range req.Transcript {
			transcript = append(transcript, guardian.TranscriptEntry{Role: e.Role, Text: e.Text})
		}
		decision, err := reviewer.Review(ctx, guardian.Request{Command: req.Command, WorkDir: req.WorkDir, Transcript: transcript, ApprovalContext: req.ApprovalContext})
		if err != nil {
			return tools.PolicyDecision{}, err
		}
		return tools.PolicyDecision{Allowed: decision.Allowed(), RiskLevel: decision.RiskLevel, UserAuthorization: decision.UserAuthorization, Rationale: decision.Rationale}, nil
	}
	if approvalMgr.GuardianEventFunc == nil {
		approvalMgr.GuardianEventFunc = func(message string) {
			fmt.Fprintln(os.Stderr, message)
		}
	}
	return nil
}

func resolveGuardianModelName(cfg *config.Config, fallback string) string {
	if cfg == nil {
		return strings.TrimSpace(fallback)
	}
	if model := strings.TrimSpace(cfg.Guardian.Model); model != "" {
		return model
	}
	if guardianName := strings.TrimSpace(cfg.Guardian.Provider); guardianName != "" {
		if pc := cfg.GetProviderConfig(guardianName); pc != nil {
			if model := strings.TrimSpace(pc.Model); model != "" {
				return model
			}
			if model := strings.TrimSpace(pc.FastModel); model != "" {
				return model
			}
		}
		providerType := string(config.InferProviderType(guardianName, ""))
		if model := strings.TrimSpace(llm.ProviderFastModels[providerType]); model != "" {
			return model
		}
	}
	return strings.TrimSpace(fallback)
}

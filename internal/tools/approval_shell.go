package tools

import (
	"context"
	"fmt"

	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

// checkShellApprovalWithContext supplies transcript evidence only to Guardian.
func (m *ApprovalManager) checkShellApprovalWithContext(ctx context.Context, command, workDir string, transcript func() []TranscriptEntry, environment ...bool) (ConfirmOutcome, error) {
	// Canonical env/shell invocation is both review evidence and cache identity.
	hasEnv := len(environment) != 0 && environment[0]
	// Yolo mode - auto-approve everything
	if m.YoloEnabled() {
		if m.DebugApproval {
			runtimeoutput.Logf("[approval] CheckShellApproval cmd=%q → yolo auto-approve", command)
		}
		return ProceedOnce, nil
	}

	if outcome, ok := m.checkShellApprovalNoPrompt(command, workDir, hasEnv); ok {
		if m.DebugApproval {
			runtimeoutput.Logf("[approval] CheckShellApproval cmd=%q → no-prompt decided: %v", command, outcome)
		}
		return outcome, nil
	}

	guardianAttemptedBeforeLock := false
	if m.ApprovalMode() == ModeAuto {
		guardianAttemptedBeforeLock = true
		if outcome, decided, err := m.checkShellGuardianApproval(ctx, command, workDir, transcript); decided || err != nil {
			return outcome, err
		}
	}

	// Need to prompt - serialize prompts to avoid UI conflicts
	// Use shared lock (via PromptLock()) to prevent concurrent prompts across parent/child managers
	promptLock := m.PromptLock()
	promptLock.Lock()
	defer promptLock.Unlock()

	// Recheck yolo now that we hold the prompt lock; the user may have toggled it
	// while this request was waiting behind another prompt.
	if m.YoloEnabled() {
		if m.DebugApproval {
			runtimeoutput.Logf("[approval] CheckShellApproval cmd=%q → yolo auto-approve after lock", command)
		}
		return ProceedOnce, nil
	}

	// Recheck now that we hold the prompt lock to avoid duplicate prompts
	if outcome, ok := m.checkShellApprovalNoPrompt(command, workDir, hasEnv); ok {
		if m.DebugApproval {
			runtimeoutput.Logf("[approval] CheckShellApproval cmd=%q → recheck decided: %v", command, outcome)
		}
		return outcome, nil
	}

	if m.ApprovalMode() == ModeAuto && !guardianAttemptedBeforeLock {
		if outcome, decided, err := m.checkShellGuardianApproval(ctx, command, workDir, transcript); decided || err != nil {
			return outcome, err
		}
	}

	// Remember approval in the command's repo, not the process's repo.
	projectApprovals := m.getProjectApprovals(normalizeGuardianWorkDir(workDir))

	// Try new UI first (local, then ancestors), then fall back to legacy
	promptUIFunc := m.lookupPromptUIFunc()
	if promptUIFunc != nil {
		if m.DebugApproval {
			runtimeoutput.Logf("[approval] CheckShellApproval cmd=%q → calling PromptUIFunc", command)
		}
		result, err := promptUIFunc(command, false, true, workDir)
		if err != nil {
			return Cancel, err
		}
		// Environment grants are exact/session-only, never durable patterns.
		if hasEnv && result.Choice == ApprovalChoicePattern {
			result.Choice = ApprovalChoiceCommand
		}
		return m.handleShellApprovalResult(result, command, workDir, projectApprovals)
	}

	// Fall back to legacy PromptFunc (local, then ancestors)
	promptFunc := m.lookupPromptFunc()
	if promptFunc == nil {
		return Cancel, NewToolError(ErrPermissionDenied, "command not in allowlist and no TTY for approval")
	}

	req := &ApprovalRequest{
		ToolName:    ShellToolName,
		Command:     command,
		Description: fmt.Sprintf("Allow shell command: %s", command),
		ToolInfo:    command,
	}

	outcome, pattern := promptFunc(req)
	if hasEnv {
		pattern = "" // environment-bearing grants are exact and session-only
		if outcome == ProceedAlwaysAndSave {
			outcome = ProceedAlways
		}
	}

	if outcome == ProceedAlways || outcome == ProceedAlwaysAndSave {
		// Cache the command or pattern for future use. A malformed remembered
		// pattern still honors the user's approval for this invocation.
		if pattern != "" {
			if err := m.shellCache.AddPattern(pattern); err != nil {
				runtimeoutput.Logf("[approval] failed to remember shell pattern %q; using one-time approval: %v", pattern, err)
				outcome = ProceedOnce
			}
		} else if err := m.shellCache.AddCommand(command, workDir); err != nil {
			return Cancel, fmt.Errorf("remember exact shell command: %w", err)
		}
	}
	if outcome == ProceedOnce || outcome == ProceedAlways || outcome == ProceedAlwaysAndSave {
		m.resetGuardianDenials()
	}

	return outcome, nil
}

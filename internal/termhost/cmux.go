package termhost

import (
	"context"
	"strings"

	"github.com/samsaffron/term-llm/internal/lifecycle"
)

const cmuxStatusPrefix = "term-llm:"

type cmuxAdapter struct {
	binPath     string
	workspaceID string
	statusKey   string
	run         commandRunner
}

func (a *cmuxAdapter) Name() string { return "cmux" }

func (a *cmuxAdapter) Send(ctx context.Context, event lifecycle.Event) error {
	// Do not translate inherited CMUX_SOCKET_PATH into --socket. The cmux CLI
	// owns its canonical environment and CMUX_ALLOW_SOCKET_OVERRIDE policy.
	args := make([]string, 0, 6)
	if event.Kind == lifecycle.KindRelease {
		args = append(args, "clear-status", a.statusKey, "--workspace", a.workspaceID)
		return a.run(ctx, a.binPath, args, nil)
	}
	label := "Idle"
	switch event.State {
	case lifecycle.Working:
		label = "Working"
	case lifecycle.Blocked:
		label = "Needs input"
	}
	args = append(args, "set-status", a.statusKey, label, "--workspace", a.workspaceID)
	return a.run(ctx, a.binPath, args, nil)
}

func discoverCMUX(rt runtimeContext) discoveredAdapter {
	status := Status{Name: "cmux", Type: "sidebar"}
	workspaceID := boundedEnv(rt.getenv("CMUX_WORKSPACE_ID"), 256)
	surfaceID := boundedEnv(rt.getenv("CMUX_SURFACE_ID"), 256)
	if workspaceID == "" || surfaceID == "" {
		status.Reason = "CMUX_WORKSPACE_ID and CMUX_SURFACE_ID are not both present"
		return discoveredAdapter{status: status}
	}
	status.Detected = true
	if strings.TrimSpace(rt.getenv("TERM_LLM_CMUX")) == "0" {
		status.Reason = "disabled by TERM_LLM_CMUX=0"
		return discoveredAdapter{status: status}
	}
	if rt.goos != "darwin" {
		status.Reason = "cmux integration is available only on macOS"
		return discoveredAdapter{status: status}
	}
	binPath := ""
	if rt.lookPath != nil {
		found, err := rt.lookPath("cmux")
		if err == nil {
			binPath = strings.TrimSpace(found)
		}
	}
	if binPath == "" {
		status.Reason = "cmux CLI was not found"
		return discoveredAdapter{status: status}
	}
	status.Enabled = true
	status.Reason = "cmux workspace, surface, and CLI detected; publishes sidebar status only"
	return discoveredAdapter{
		status: status,
		adapter: &cmuxAdapter{
			binPath:     binPath,
			workspaceID: workspaceID,
			statusKey:   cmuxStatusPrefix + surfaceID,
			run:         rt.run,
		},
	}
}

func boundedEnv(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max {
		value = value[:max]
	}
	return value
}

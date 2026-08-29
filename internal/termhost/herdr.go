package termhost

import (
	"context"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/lifecycle"
)

const (
	herdrSource         = "custom:term-llm"
	herdrMetadataSource = "custom:term-llm-title"
	herdrAgent          = "term-llm"
)

type herdrAdapter struct {
	binPath string
	paneID  string
	run     commandRunner
}

func (a *herdrAdapter) Name() string { return "herdr" }

func (a *herdrAdapter) Send(ctx context.Context, event lifecycle.Event) error {
	sequence := strconv.FormatInt(event.Sequence, 10)
	if event.Kind == lifecycle.KindRelease {
		return a.run(ctx, a.binPath, []string{
			"pane", "release-agent", a.paneID,
			"--source", herdrSource,
			"--agent", herdrAgent,
			"--seq", sequence,
		}, nil)
	}
	args := []string{
		"pane", "report-agent", a.paneID,
		"--source", herdrSource,
		"--agent", herdrAgent,
		"--state", string(event.State),
		"--seq", sequence,
	}
	if event.SessionID != "" {
		// Forward this inert custom-source field for future Herdr compatibility.
		// Current Herdr does not treat it as native restore authority.
		args = append(args, "--agent-session-id", event.SessionID)
	}
	if event.Message != "" && event.State != lifecycle.Idle {
		args = append(args, "--message", event.Message)
	}
	if err := a.run(ctx, a.binPath, args, nil); err != nil {
		return err
	}

	metadataArgs := []string{
		"pane", "report-metadata", a.paneID,
		"--source", herdrMetadataSource,
		"--agent", herdrAgent,
		"--applies-to-source", herdrSource,
	}
	if event.Title == "" {
		metadataArgs = append(metadataArgs, "--clear-title", "--clear-display-agent")
	} else {
		metadataArgs = append(metadataArgs, "--title", event.Title, "--display-agent", event.Title)
	}
	metadataArgs = append(metadataArgs, "--seq", sequence)
	return a.run(ctx, a.binPath, metadataArgs, nil)
}

func discoverHerdr(rt runtimeContext) discoveredAdapter {
	status := Status{Name: "herdr", Type: "native"}
	if rt.getenv("HERDR_ENV") != "1" {
		status.Reason = "HERDR_ENV=1 is not present"
		return discoveredAdapter{status: status}
	}
	status.Detected = true
	if strings.TrimSpace(rt.getenv("TERM_LLM_HERDR")) == "0" {
		status.Reason = "disabled by TERM_LLM_HERDR=0"
		return discoveredAdapter{status: status}
	}
	paneID := boundedEnv(rt.getenv("HERDR_PANE_ID"), 256)
	if paneID == "" {
		status.Reason = "HERDR_PANE_ID is missing"
		return discoveredAdapter{status: status}
	}
	binPath := strings.TrimSpace(rt.getenv("HERDR_BIN_PATH"))
	if binPath == "" && rt.lookPath != nil {
		found, err := rt.lookPath("herdr")
		if err == nil {
			binPath = strings.TrimSpace(found)
		}
	}
	if binPath == "" {
		status.Reason = "Herdr CLI was not found"
		return discoveredAdapter{status: status}
	}
	status.Enabled = true
	status.Reason = "Herdr pane and CLI detected"
	return discoveredAdapter{
		status: status,
		adapter: &herdrAdapter{
			binPath: binPath,
			paneID:  paneID,
			run:     rt.run,
		},
	}
}

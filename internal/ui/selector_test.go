package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/testutil"
)

func TestRunWithSpinnerSkipsUIInCI(t *testing.T) {
	t.Setenv("CI", "1")
	calls := 0
	got, err := RunWithSpinner(context.Background(), false, func(context.Context) (any, error) {
		calls++
		return "done", nil
	})
	if err != nil {
		t.Fatalf("RunWithSpinner() error = %v", err)
	}
	if got != "done" || calls != 1 {
		t.Fatalf("RunWithSpinner() = (%v, calls=%d), want (done, 1)", got, calls)
	}
}

func TestRunWithSpinnerDrainsProgressWhenSkipped(t *testing.T) {
	t.Setenv("CI", "1")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	progress := make(chan ProgressUpdate)

	_, err := RunWithSpinnerProgress(ctx, false, progress, func(ctx context.Context) (any, error) {
		defer close(progress)
		select {
		case progress <- ProgressUpdate{Status: "working"}:
			return "done", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	if err != nil {
		t.Fatalf("RunWithSpinnerProgress() error = %v", err)
	}
}

func TestSelectCommandRequiresExplicitNonInteractiveMode(t *testing.T) {
	t.Setenv("CI", "1")
	suggestions := []llm.CommandSuggestion{{Command: "echo safe"}}

	if _, _, err := SelectCommand(suggestions, "sh", nil, false); err == nil || !strings.Contains(err.Error(), "--print-only or --auto-pick") {
		t.Fatalf("SelectCommand() error = %v, want explicit-mode guidance", err)
	}
	selected, refinement, err := SelectCommand(suggestions, "sh", nil, true)
	if err != nil {
		t.Fatalf("SelectCommand(allowNonTTY=true) error = %v", err)
	}
	if selected != "echo safe" || refinement != "" {
		t.Fatalf("selection = (%q, %q), want first command", selected, refinement)
	}
}

func TestTextInputRendering(t *testing.T) {
	textColor := lipgloss.Color("15")
	mutedColor := lipgloss.Color("245")

	ti := textinput.New()
	ti.Placeholder = "Test placeholder"
	ti.CharLimit = 500
	ti.SetWidth(50)
	ti.Prompt = ""

	styles := textinput.DefaultStyles(false)
	styles.Focused.Prompt = lipgloss.NewStyle()
	styles.Focused.Text = lipgloss.NewStyle().Foreground(textColor)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(mutedColor)
	styles.Blurred = styles.Focused
	ti.SetStyles(styles)

	ti.Focus()
	view := ti.View()
	plain := testutil.StripANSI(view)

	t.Logf("raw: %q", view)
	t.Logf("plain: %q", plain)

	if !strings.Contains(plain, "Test placeholder") {
		t.Errorf("expected placeholder text in plain output, got: %q", plain)
	}
}

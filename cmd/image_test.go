package cmd

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/image"
)

func TestShouldUseImageSpinner(t *testing.T) {
	oldIsTerminal := imageIsTerminal
	oldNoSpinner := imageNoSpinner
	t.Cleanup(func() {
		imageIsTerminal = oldIsTerminal
		imageNoSpinner = oldNoSpinner
	})

	tests := []struct {
		name           string
		stdinTerminal  bool
		stderrTerminal bool
		ci             string
		term           string
		noSpinner      bool
		envOverride    string
		want           bool
	}{
		{name: "interactive stdin and stderr", stdinTerminal: true, stderrTerminal: true, term: "xterm-256color", want: true},
		{name: "captured stderr", stdinTerminal: true, term: "xterm-256color"},
		{name: "non-terminal stdin", stderrTerminal: true, term: "xterm-256color"},
		{name: "CI", stdinTerminal: true, stderrTerminal: true, ci: "1", term: "xterm-256color"},
		{name: "CI false", stdinTerminal: true, stderrTerminal: true, ci: "false", term: "xterm-256color", want: true},
		{name: "dumb terminal", stdinTerminal: true, stderrTerminal: true, term: "dumb"},
		{name: "no-spinner flag", stdinTerminal: true, stderrTerminal: true, noSpinner: true, term: "xterm-256color"},
		{name: "environment override", stdinTerminal: true, stderrTerminal: true, envOverride: "1", term: "xterm-256color"},
		{name: "false environment override", stdinTerminal: true, stderrTerminal: true, envOverride: "0", term: "xterm-256color", want: true},
		{name: "off environment override", stdinTerminal: true, stderrTerminal: true, envOverride: "off", term: "xterm-256color", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CI", tt.ci)
			t.Setenv("TERM", tt.term)
			t.Setenv("TERM_LLM_NO_SPINNER", tt.envOverride)
			imageNoSpinner = tt.noSpinner
			imageIsTerminal = func(fd int) bool {
				switch fd {
				case int(os.Stdin.Fd()):
					return tt.stdinTerminal
				case int(os.Stderr.Fd()):
					return tt.stderrTerminal
				default:
					return false
				}
			}

			if got := shouldUseImageSpinner(); got != tt.want {
				t.Fatalf("shouldUseImageSpinner() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestHasInteractiveImageTTYSkipsOpenWhenCaptured(t *testing.T) {
	oldIsTerminal := imageIsTerminal
	oldOpenImageTTY := openImageTTY
	t.Cleanup(func() {
		imageIsTerminal = oldIsTerminal
		openImageTTY = oldOpenImageTTY
	})
	t.Setenv("CI", "")
	t.Setenv("TERM", "xterm-256color")
	imageIsTerminal = func(int) bool { return false }
	openImageTTY = func(string, int, os.FileMode) (*os.File, error) {
		t.Fatal("captured output unexpectedly opened /dev/tty")
		return nil, errors.New("unexpected TTY open")
	}
	if hasInteractiveImageTTY() {
		t.Fatal("hasInteractiveImageTTY() = true for captured stderr")
	}
}

func TestRunImageWithSpinnerUsesDirectGenerationWhenNonInteractive(t *testing.T) {
	oldIsTerminal := imageIsTerminal
	oldOpenImageTTY := openImageTTY
	oldNoSpinner := imageNoSpinner
	t.Cleanup(func() {
		imageIsTerminal = oldIsTerminal
		openImageTTY = oldOpenImageTTY
		imageNoSpinner = oldNoSpinner
	})

	t.Setenv("CI", "")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TERM_LLM_NO_SPINNER", "")
	imageNoSpinner = false
	imageIsTerminal = func(int) bool { return false }
	openImageTTY = func(string, int, os.FileMode) (*os.File, error) {
		t.Fatal("runImageWithSpinner opened /dev/tty in non-interactive mode")
		return nil, nil
	}

	want := &image.ImageResult{Data: []byte("image")}
	calls := 0
	got, err := runImageWithSpinner(context.Background(), nil, func(context.Context) (*image.ImageResult, error) {
		calls++
		return want, nil
	}, "Generating image")
	if err != nil {
		t.Fatalf("runImageWithSpinner() error = %v", err)
	}
	if got != want {
		t.Fatalf("runImageWithSpinner() result = %p, want %p", got, want)
	}
	if calls != 1 {
		t.Fatalf("generate calls = %d, want 1", calls)
	}
}

func TestRunImageWithSpinnerFallsBackWhenTTYUnavailable(t *testing.T) {
	oldIsTerminal := imageIsTerminal
	oldOpenImageTTY := openImageTTY
	oldNoSpinner := imageNoSpinner
	t.Cleanup(func() {
		imageIsTerminal = oldIsTerminal
		openImageTTY = oldOpenImageTTY
		imageNoSpinner = oldNoSpinner
	})

	t.Setenv("CI", "")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TERM_LLM_NO_SPINNER", "")
	imageNoSpinner = false
	imageIsTerminal = func(int) bool { return true }
	openImageTTY = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		if name != "/dev/tty" {
			t.Fatalf("openImageTTY name = %q, want /dev/tty", name)
		}
		return nil, errors.New("tty unavailable")
	}

	want := &image.ImageResult{Data: []byte("image")}
	got, err := runImageWithSpinner(context.Background(), nil, func(context.Context) (*image.ImageResult, error) {
		return want, nil
	}, "Generating image")
	if err != nil {
		t.Fatalf("runImageWithSpinner() error = %v", err)
	}
	if got != want {
		t.Fatalf("runImageWithSpinner() result = %p, want %p", got, want)
	}
}

func TestRunImageWithSpinnerDirectErrorsPropagate(t *testing.T) {
	oldIsTerminal := imageIsTerminal
	oldOpenImageTTY := openImageTTY
	oldNoSpinner := imageNoSpinner
	t.Cleanup(func() {
		imageIsTerminal = oldIsTerminal
		openImageTTY = oldOpenImageTTY
		imageNoSpinner = oldNoSpinner
	})

	imageNoSpinner = true
	imageIsTerminal = func(int) bool { return true }
	openImageTTY = func(string, int, os.FileMode) (*os.File, error) {
		t.Fatal("direct execution unexpectedly opened /dev/tty")
		return nil, errors.New("unexpected TTY open")
	}

	t.Run("provider error", func(t *testing.T) {
		providerErr := errors.New("provider failed")
		_, err := runImageWithSpinner(context.Background(), nil, func(context.Context) (*image.ImageResult, error) {
			return nil, providerErr
		}, "Generating image")
		if err != providerErr {
			t.Fatalf("error = %v, want original provider error %v", err, providerErr)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := runImageWithSpinner(ctx, nil, func(generateCtx context.Context) (*image.ImageResult, error) {
			return nil, generateCtx.Err()
		}, "Generating image")
		if err != context.Canceled {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})
}

func TestImageSpinnerKeyCancellationCancelsProviderContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := imageSpinnerModel{ctx: ctx, cancel: cancel}

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("Ctrl-C should quit the spinner")
	}
	if err := ctx.Err(); err != context.Canceled {
		t.Fatalf("provider context error = %v, want context.Canceled", err)
	}
}

func TestImageSpinnerEnvironmentDisablesBubbleTeaModeQueries(t *testing.T) {
	env := imageSpinnerEnvironment([]string{
		"TERM=xterm-kitty",
		"TERM_PROGRAM=WezTerm",
		"WT_SESSION=abc",
		"KITTY_WINDOW_ID=1",
		"PATH=/bin",
	})

	values := map[string]string{}
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		values[name] = value
	}
	if values["TERM"] != "xterm-256color" {
		t.Fatalf("TERM = %q, want xterm-256color (env: %#v)", values["TERM"], env)
	}
	if values["TERM_PROGRAM"] != "Apple_Terminal" {
		t.Fatalf("TERM_PROGRAM = %q, want Apple_Terminal (env: %#v)", values["TERM_PROGRAM"], env)
	}
	if _, ok := values["WT_SESSION"]; ok {
		t.Fatalf("WT_SESSION should be removed: %#v", env)
	}
	if values["KITTY_WINDOW_ID"] != "1" || values["PATH"] != "/bin" {
		t.Fatalf("unrelated env should be preserved: %#v", env)
	}
}

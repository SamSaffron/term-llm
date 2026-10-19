package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestServeFlagBindingIsolated(t *testing.T) {
	// AddCommonFlags records command capabilities in a shared cmd registry;
	// parallel flag binding would race that registry.
	for _, tc := range []struct {
		name     string
		args     []string
		wantHost string
		wantPort int
	}{
		{"default", nil, "127.0.0.1", 8080},
		{"custom", []string{"--host", "localhost", "--port", "9090"}, "localhost", 9090},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opts ServeOptions
			cmd := &cobra.Command{}
			bindServeFlags(cmd, &opts)
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatal(err)
			}
			if opts.Host != tc.wantHost || opts.Port != tc.wantPort || opts.SessionTTL != 30*time.Minute {
				t.Fatalf("flags: host=%q port=%d TTL=%s", opts.Host, opts.Port, opts.SessionTTL)
			}
		})
	}
}

func TestServeStartupOptionsValidation(t *testing.T) {
	t.Parallel()
	base := ServeOptions{Port: 8080, SessionTTL: time.Minute, SessionMax: 100, JobsWorkers: 4}
	for _, tc := range []struct {
		name, errorText string
		change          func(*ServeOptions)
	}{
		{"port", "invalid --port", func(o *ServeOptions) { o.Port = 0 }},
		{"session TTL", "invalid --session-ttl", func(o *ServeOptions) { o.SessionTTL = 0 }},
		{"session max", "invalid --session-max", func(o *ServeOptions) { o.SessionMax = 0 }},
		{"Telegram carryover", "invalid --telegram-carryover-chars", func(o *ServeOptions) { o.TelegramCarryoverChars = -1 }},
		{"jobs workers", "invalid --jobs-workers", func(o *ServeOptions) { o.JobsWorkers = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := base
			tc.change(&opts)
			_, err := resolveServeStartup(&cobra.Command{}, &opts)
			if err == nil || !strings.Contains(err.Error(), tc.errorText) {
				t.Fatalf("resolveServeStartup() error = %v, want %q", err, tc.errorText)
			}
		})
	}
}

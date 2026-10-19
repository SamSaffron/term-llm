package usage

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/providerusage"
	"github.com/spf13/cobra"
)

func TestRunDirectProviderUsageWritesChatGPTPlanLimits(t *testing.T) {
	originalFetch := fetchDirectProviderUsage
	defer func() { fetchDirectProviderUsage = originalFetch }()
	fetchDirectProviderUsage = func(_ context.Context, provider, apiKey string) (*providerusage.Report, error) {
		if provider != "chatgpt" {
			t.Fatalf("provider = %q", provider)
		}
		if apiKey != "" {
			t.Fatalf("apiKey = %q, want empty for chatgpt", apiKey)
		}
		return &providerusage.Report{
			Provider: "chatgpt",
			Plan:     "plus",
			Limits: []providerusage.Limit{{
				ID:   "codex",
				Name: "Codex",
				PrimaryWindow: &providerusage.Window{
					UsedPercent:     25,
					DurationMinutes: 300,
					ResetsAt:        time.Now().Add(time.Hour),
				},
			}},
		}, nil
	}

	o := NewOptions()
	var out bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&out)
	if err := o.runDirectProviderUsage(command, "chatgpt"); err != nil {
		t.Fatalf("runDirectProviderUsage: %v", err)
	}
	for _, want := range []string{"ChatGPT Codex", "Plus plan", "5 hour window\n  ██████░░░░░░░░░░░░░░░░  25% used"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunUsageRoutesGrokToOAuthUsageFetcher(t *testing.T) {
	originalFetch := fetchDirectProviderUsage
	defer func() { fetchDirectProviderUsage = originalFetch }()
	fetchDirectProviderUsage = func(_ context.Context, provider, apiKey string) (*providerusage.Report, error) {
		if provider != "grok" || apiKey != "" {
			t.Fatalf("fetch args = provider %q, apiKey %q", provider, apiKey)
		}
		return &providerusage.Report{
			Provider: "grok",
			Limits: []providerusage.Limit{{
				Name: "Weekly limit",
				PrimaryWindow: &providerusage.Window{
					Label:       "1 week window",
					UsedPercent: 42,
				},
			}},
		}, nil
	}

	o := NewOptions()
	o.Provider = "grok"
	var out bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&out)
	if err := o.Run(command, nil); err != nil {
		t.Fatalf("runUsage: %v", err)
	}
	for _, want := range []string{"Grok", "Weekly limit", "1 week window", "42% used"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunUsageRejectsLocalFiltersForGrok(t *testing.T) {
	o := NewOptions()
	o.Provider = "grok"
	o.Since = "20260801"
	command := &cobra.Command{}
	if err := o.Run(command, nil); err == nil || !strings.Contains(err.Error(), "live grok usage does not support") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunDirectProviderUsageOpenCodeGoUsesConfiguredKey(t *testing.T) {
	originalFetch := fetchDirectProviderUsage
	originalLoad := loadProviderUsageConfig
	defer func() {
		fetchDirectProviderUsage = originalFetch
		loadProviderUsageConfig = originalLoad
	}()
	loadProviderUsageConfig = func() (*config.Config, error) {
		return &config.Config{Providers: map[string]config.ProviderConfig{
			"opencode-go": {Type: config.ProviderTypeOpenCodeGo, ResolvedAPIKey: "configured-key"},
		}}, nil
	}
	fetchDirectProviderUsage = func(_ context.Context, provider, apiKey string) (*providerusage.Report, error) {
		if provider != "opencode-go" || apiKey != "configured-key" {
			t.Fatalf("fetch args = provider %q, apiKey %q", provider, apiKey)
		}
		return &providerusage.Report{
			Provider: "opencode-go",
			Limits: []providerusage.Limit{{
				Name: "Rolling usage",
				PrimaryWindow: &providerusage.Window{
					Label:       "5 hour window",
					UsedPercent: 42,
				},
			}},
		}, nil
	}

	o := NewOptions()
	var out bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&out)
	if err := o.runDirectProviderUsage(command, "opencode-go"); err != nil {
		t.Fatalf("runDirectProviderUsage: %v", err)
	}
	for _, want := range []string{"OpenCode Go", "Rolling usage", "42% used"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

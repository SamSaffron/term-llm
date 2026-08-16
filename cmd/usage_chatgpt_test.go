package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/spf13/cobra"
)

func TestRunDirectProviderUsageWritesChatGPTPlanLimits(t *testing.T) {
	originalFetch := fetchDirectProviderUsage
	defer func() { fetchDirectProviderUsage = originalFetch }()
	fetchDirectProviderUsage = func(_ context.Context, provider, apiKey string) (*llm.ProviderUsage, error) {
		if provider != "chatgpt" {
			t.Fatalf("provider = %q", provider)
		}
		if apiKey != "" {
			t.Fatalf("apiKey = %q, want empty for chatgpt", apiKey)
		}
		return &llm.ProviderUsage{
			Provider: "chatgpt",
			Plan:     "plus",
			Limits: []llm.ProviderUsageLimit{{
				ID:   "codex",
				Name: "Codex",
				PrimaryWindow: &llm.ProviderUsageWindow{
					UsedPercent:     25,
					DurationMinutes: 300,
					ResetsAt:        time.Now().Add(time.Hour),
				},
			}},
		}, nil
	}

	resetUsageTestFlags()
	defer resetUsageTestFlags()
	var out bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&out)
	if err := runDirectProviderUsage(command, "chatgpt"); err != nil {
		t.Fatalf("runDirectProviderUsage: %v", err)
	}
	for _, want := range []string{"ChatGPT Codex", "Plus plan", "5 hour window\n  ██████░░░░░░░░░░░░░░░░  25% used"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
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
	fetchDirectProviderUsage = func(_ context.Context, provider, apiKey string) (*llm.ProviderUsage, error) {
		if provider != "opencode-go" || apiKey != "configured-key" {
			t.Fatalf("fetch args = provider %q, apiKey %q", provider, apiKey)
		}
		return &llm.ProviderUsage{
			Provider: "opencode-go",
			Limits: []llm.ProviderUsageLimit{{
				Name: "Rolling usage",
				PrimaryWindow: &llm.ProviderUsageWindow{
					Label:       "5 hour window",
					UsedPercent: 42,
				},
			}},
		}, nil
	}

	resetUsageTestFlags()
	defer resetUsageTestFlags()
	var out bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&out)
	if err := runDirectProviderUsage(command, "opencode-go"); err != nil {
		t.Fatalf("runDirectProviderUsage: %v", err)
	}
	for _, want := range []string{"OpenCode Go", "Rolling usage", "42% used"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func resetUsageTestFlags() {
	usageProvider = ""
	usageSince = ""
	usageUntil = ""
	usageJSON = false
	usageBreakdown = false
	usageIncludeExternal = false
	usageCopilotScope = "user"
	usageCopilotEntity = ""
	usageCopilotYear = 0
	usageCopilotMonth = 0
	usageCopilotDay = 0
	usageCopilotModel = ""
	usageCopilotProduct = ""
	usageCopilotUser = ""
	usageCopilotOrg = ""
	usageCopilotEnterprise = ""
	usageCopilotCostCenter = ""
}

package usage

import (
	"bytes"
	"strings"
	"testing"

	githubcopilot "github.com/samsaffron/term-llm/internal/copilot"
)

func TestWriteCopilotUsageTextShowsAICreditSummary(t *testing.T) {
	report := &githubcopilot.UsageReport{
		Scope:  githubcopilot.ScopeUser,
		Entity: "octocat",
		Source: githubcopilot.SourceGitHubBillingAPI,
		TimePeriod: githubcopilot.TimePeriod{
			Year:  2026,
			Month: 6,
		},
		Items: []githubcopilot.UsageItem{
			{Product: "Copilot Chat", Model: "Claude Sonnet 4.6", UnitType: "credits", NetQuantity: 90, NetAmountUSD: 0.90},
			{Product: "Copilot CLI", Model: "GPT-5 mini", UnitType: "credits", NetQuantity: 50.5, NetAmountUSD: 0.505},
		},
		Totals: githubcopilot.UsageTotals{
			GrossCredits:      150.5,
			NetCredits:        140.5,
			NetAmountUSD:      1.405,
			DiscountAmountUSD: 0.10,
		},
	}

	var out bytes.Buffer
	if err := writeCopilotUsageText(&out, report); err != nil {
		t.Fatalf("writeCopilotUsageText: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"GitHub Copilot Usage - AI Credits",
		"Scope: user/octocat",
		"Period: June 2026",
		"Net credits:",
		"140.50",
		"$1.41",
		"By model",
		"Claude Sonnet 4.6",
		"GPT-5 mini",
		"By product",
		"Copilot Chat",
		"Copilot CLI",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q\nfull output:\n%s", want, got)
		}
	}
}

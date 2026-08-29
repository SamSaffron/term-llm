package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/spf13/cobra"
)

const agyUsageFixture = `{
  "conversation_id":"",
  "status":"SUCCESS",
  "response":"Gemini Models\tWeekly Limit Remaining\t75%\t2026-09-01T00:00:00Z\n",
  "num_turns":0,
  "usage":{"input_tokens":0,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":0},
  "command":{"name":"usage","data":{
    "description":"Within each group, models share limits.",
    "groups":[
      {"name":"Gemini Models","description":"Gemini Flash, Gemini Pro","buckets":[
        {"id":"gemini-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":0.75,"reset_time":"2026-09-01T00:00:00Z"},
        {"id":"gemini-5h","name":"Five Hour Limit Remaining","window":"5h","remaining_fraction":0.6,"reset_time":"2026-08-17T14:00:00Z"}
      ]},
      {"name":"Claude and GPT models","buckets":[
        {"id":"3p-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":1,"reset_time":"2026-09-02T00:00:00Z"},
        {"id":"3p-5h","name":"Five Hour Limit Remaining","window":"5h","remaining_fraction":1,"reset_time":"2026-08-17T16:00:00Z"}
      ]}
    ]
  }}
}`

func TestParseAgyBinUsageOutput(t *testing.T) {
	raw, report, err := parseAgyBinUsageOutput([]byte(agyUsageFixture))
	if err != nil {
		t.Fatalf("parseAgyBinUsageOutput: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"gemini-weekly"`)) {
		t.Fatalf("raw data = %s", raw)
	}
	if len(report.Groups) != 2 || len(report.Groups[0].Buckets) != 2 {
		t.Fatalf("report groups = %#v", report.Groups)
	}
	if got := *report.Groups[0].Buckets[1].RemainingFraction; got != 0.6 {
		t.Fatalf("remaining fraction = %v", got)
	}
}

func TestParseAgyBinUsageOutputRequiresZeroTokenCommand(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "agent turn",
			output: `{"status":"SUCCESS","num_turns":1,"usage":{"total_tokens":3},"command":{"name":"usage","data":{"groups":[{"name":"Gemini"}]}}}`,
			want:   "agent turn",
		},
		{
			name:   "persisted conversation",
			output: `{"conversation_id":"unexpected","status":"SUCCESS","num_turns":0,"usage":{},"command":{"name":"usage","data":{"groups":[{"name":"Gemini"}]}}}`,
			want:   "persisted an agent turn",
		},
		{
			name:   "missing command",
			output: `{"status":"SUCCESS","num_turns":0,"usage":{}}`,
			want:   "structured usage data",
		},
		{
			name:   "failed command",
			output: `{"status":"ERROR","response":"not logged in"}`,
			want:   "not logged in",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := parseAgyBinUsageOutput([]byte(test.output))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestWriteAgyBinUsage(t *testing.T) {
	_, report, err := parseAgyBinUsageOutput([]byte(agyUsageFixture))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	if err := writeAgyBinUsageWithOptions(&out, report, now, llm.ProviderUsageFormatOptions{ASCII: true}); err != nil {
		t.Fatalf("writeAgyBinUsageWithOptions: %v", err)
	}
	for _, want := range []string{
		"Antigravity", "Live from agy", "Gemini Models", "5 hour window", "40% used",
		"Resets in 2h", "1 week window", "25% used", "Claude and GPT models", "0% used",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestNormalizeAgyBinUsageRejectsMalformedReset(t *testing.T) {
	remaining := 0.5
	_, err := normalizeAgyBinUsage(agyBinUsageReport{Groups: []agyBinUsageGroup{{
		Name: "Gemini",
		Buckets: []agyBinUsageBucket{{
			ID: "gemini-weekly", Window: "weekly", RemainingFraction: &remaining, ResetTime: "tomorrow",
		}},
	}}})
	if err == nil || !strings.Contains(err.Error(), "reset time") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunAgyUsageCommandRefusesOldVersionBeforeUsage(t *testing.T) {
	originalExec := agyUsageExec
	defer func() { agyUsageExec = originalExec }()
	var calls [][]string
	agyUsageExec = func(_ context.Context, binary string, args ...string) ([]byte, string, error) {
		if binary != "/bin/agy" {
			t.Fatalf("binary = %q", binary)
		}
		calls = append(calls, append([]string(nil), args...))
		return []byte("1.1.10\n"), "", nil
	}

	_, err := runAgyUsageCommand(context.Background(), "/bin/agy")
	if err == nil || !strings.Contains(err.Error(), "1.1.11 or newer") {
		t.Fatalf("error = %v", err)
	}
	if len(calls) != 1 || strings.Join(calls[0], " ") != "--version" {
		t.Fatalf("calls = %v; old agy must only receive --version", calls)
	}
}

func TestRunAgyUsageCommandUsesZeroTokenPrintCommand(t *testing.T) {
	originalExec := agyUsageExec
	defer func() { agyUsageExec = originalExec }()
	var calls [][]string
	agyUsageExec = func(_ context.Context, _ string, args ...string) ([]byte, string, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(calls) == 1 {
			return []byte("1.1.22\n"), "", nil
		}
		return []byte(agyUsageFixture), "", nil
	}

	output, err := runAgyUsageCommand(context.Background(), "/bin/agy")
	if err != nil {
		t.Fatalf("runAgyUsageCommand: %v", err)
	}
	if string(output) != agyUsageFixture {
		t.Fatal("runAgyUsageCommand did not return usage output")
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
	if got := strings.Join(calls[0], " "); got != "--version" {
		t.Fatalf("version args = %q", got)
	}
	if got := strings.Join(calls[1], " "); got != "--output-format json -p=/usage" {
		t.Fatalf("usage args = %q", got)
	}
}

func TestRunAgyBinUsageJSON(t *testing.T) {
	originalLookPath, originalCommand := agyUsageLookPath, agyUsageCommand
	defer func() {
		agyUsageLookPath = originalLookPath
		agyUsageCommand = originalCommand
	}()
	agyUsageLookPath = func(name string) (string, error) {
		if name != "agy" {
			t.Fatalf("binary lookup = %q", name)
		}
		return "/bin/agy", nil
	}
	agyUsageCommand = func(_ context.Context, binary string) ([]byte, error) {
		if binary != "/bin/agy" {
			t.Fatalf("binary = %q", binary)
		}
		return []byte(agyUsageFixture), nil
	}

	var out bytes.Buffer
	if err := runAgyBinUsage(context.Background(), &out, true); err != nil {
		t.Fatalf("runAgyBinUsage: %v", err)
	}
	if !strings.Contains(out.String(), `"groups": [`) || strings.Contains(out.String(), `"num_turns"`) {
		t.Fatalf("unexpected JSON output:\n%s", out.String())
	}
}

func TestRunAgyBinUsageRequiresCLI(t *testing.T) {
	originalLookPath := agyUsageLookPath
	defer func() { agyUsageLookPath = originalLookPath }()
	agyUsageLookPath = func(string) (string, error) { return "", errors.New("not found") }

	var out bytes.Buffer
	err := runAgyBinUsage(context.Background(), &out, false)
	if err == nil || !strings.Contains(err.Error(), "Antigravity CLI") {
		t.Fatalf("error = %v", err)
	}
}

func TestAgyUsageVersionAtLeast(t *testing.T) {
	for _, test := range []struct {
		version string
		want    bool
	}{
		{version: "1.1.11", want: true},
		{version: "v1.1.22", want: true},
		{version: "agy version 2.0.0-beta.1", want: true},
		{version: "1.1.12-beta.1", want: true},
		{version: "1.1.11-beta.1", want: false},
		{version: "node v20.11.0 agy 1.1.10", want: false},
		{version: "2026.08.29 agy 1.1.22", want: false},
		{version: "1.1.10", want: false},
		{version: "1.0.99", want: false},
		{version: "unknown", want: false},
	} {
		if got := agyUsageVersionAtLeast(test.version, 1, 1, 11); got != test.want {
			t.Errorf("agyUsageVersionAtLeast(%q) = %v, want %v", test.version, got, test.want)
		}
	}
}

func TestUsageProviderCompletionIncludesAgyBin(t *testing.T) {
	completions, _ := UsageProviderFlagCompletion(nil, nil, "agy")
	if len(completions) != 1 || completions[0] != "agy-bin" {
		t.Fatalf("completions = %v, want [agy-bin]", completions)
	}
}

func TestValidateAgyBinUsageFlags(t *testing.T) {
	command := &cobra.Command{}
	command.Flags().String("provider", "", "")
	command.Flags().Bool("json", false, "")
	command.Flags().String("since", "", "")
	if err := command.Flags().Set("provider", "agy-bin"); err != nil {
		t.Fatal(err)
	}
	if err := command.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	if err := validateAgyBinUsageFlags(command); err != nil {
		t.Fatalf("provider and JSON flags should be valid: %v", err)
	}
	if err := command.Flags().Set("since", "20260801"); err != nil {
		t.Fatal(err)
	}
	if err := validateAgyBinUsageFlags(command); err == nil || !strings.Contains(err.Error(), "--since") {
		t.Fatalf("error = %v", err)
	}
}

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

const claudeBinUsageRequestID = "term-llm-usage"

type claudeBinUsageEnvelope struct {
	Type     string `json:"type"`
	Response struct {
		Subtype   string          `json:"subtype"`
		RequestID string          `json:"request_id"`
		Response  json.RawMessage `json:"response"`
		Error     string          `json:"error"`
	} `json:"response"`
}

type claudeBinUsageReport struct {
	SubscriptionType    *string              `json:"subscription_type"`
	RateLimitsAvailable bool                 `json:"rate_limits_available"`
	RateLimits          *claudeBinRateLimits `json:"rate_limits"`
}

type claudeBinRateLimits struct {
	FiveHour          *claudeBinRateLimit   `json:"five_hour"`
	SevenDay          *claudeBinRateLimit   `json:"seven_day"`
	SevenDayOAuthApps *claudeBinRateLimit   `json:"seven_day_oauth_apps"`
	SevenDayOpus      *claudeBinRateLimit   `json:"seven_day_opus"`
	SevenDaySonnet    *claudeBinRateLimit   `json:"seven_day_sonnet"`
	ModelScoped       []claudeBinModelLimit `json:"model_scoped"`
	ExtraUsage        *claudeBinExtraUsage  `json:"extra_usage"`
}

type claudeBinRateLimit struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type claudeBinModelLimit struct {
	DisplayName string   `json:"display_name"`
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type claudeBinExtraUsage struct {
	IsEnabled    bool     `json:"is_enabled"`
	MonthlyLimit *float64 `json:"monthly_limit"`
	UsedCredits  *float64 `json:"used_credits"`
	Utilization  *float64 `json:"utilization"`
	Currency     *string  `json:"currency"`
}

type claudeBinUsageRow struct {
	Label       string
	Utilization *float64
	ResetsAt    *string
	Detail      string
}

func validateClaudeBinUsageFlags(cmd interface{ Flags() *pflag.FlagSet }) error {
	var incompatible []string
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		if flag.Name != "provider" && flag.Name != "json" {
			incompatible = append(incompatible, "--"+flag.Name)
		}
	})
	if len(incompatible) > 0 {
		sort.Strings(incompatible)
		return fmt.Errorf("%s cannot be used with --provider claude-bin; live subscription usage only supports --json", strings.Join(incompatible, ", "))
	}
	return nil
}

func runClaudeBinUsage(parent context.Context, cmdOut io.Writer, jsonOutput bool) error {
	binary, err := exec.LookPath("claude")
	if err != nil {
		return errors.New("claude-bin usage requires the Claude Code CLI in PATH")
	}

	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()

	raw, err := fetchClaudeBinUsage(ctx, binary)
	if err != nil {
		return err
	}
	if jsonOutput {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, raw, "", "  "); err != nil {
			return fmt.Errorf("format Claude Code usage response: %w", err)
		}
		_, err := fmt.Fprintln(cmdOut, pretty.String())
		return err
	}

	var report claudeBinUsageReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return fmt.Errorf("decode Claude Code usage response: %w", err)
	}
	return writeClaudeBinUsage(cmdOut, report, time.Now())
}

func fetchClaudeBinUsage(ctx context.Context, binary string) (json.RawMessage, error) {
	request := fmt.Sprintf(`{"type":"control_request","request_id":%q,"request":{"subtype":"get_usage"}}`+"\n", claudeBinUsageRequestID)
	command := exec.CommandContext(ctx, binary,
		"-p",
		"--safe-mode",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--tools", "",
		"--no-session-persistence",
	)
	command.Stdin = strings.NewReader(request)
	command.Env = withoutEnvironmentVariable(os.Environ(), "ANTHROPIC_API_KEY")

	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("Claude Code usage request timed out: %w", ctx.Err())
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail != "" {
			return nil, fmt.Errorf("Claude Code usage request failed: %s", detail)
		}
		return nil, fmt.Errorf("Claude Code usage request failed: %w", err)
	}

	return parseClaudeBinUsageOutput(stdout.Bytes())
}

func parseClaudeBinUsageOutput(output []byte) (json.RawMessage, error) {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var envelope claudeBinUsageEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil || envelope.Type != "control_response" || envelope.Response.RequestID != claudeBinUsageRequestID {
			continue
		}
		if envelope.Response.Subtype == "error" {
			return nil, fmt.Errorf("Claude Code rejected the usage request: %s (update Claude Code if get_usage is unsupported)", envelope.Response.Error)
		}
		if envelope.Response.Subtype != "success" || len(envelope.Response.Response) == 0 {
			return nil, errors.New("Claude Code returned an empty usage response")
		}
		return envelope.Response.Response, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read Claude Code usage response: %w", err)
	}
	return nil, errors.New("Claude Code did not return usage data (update Claude Code if get_usage is unsupported)")
}

func withoutEnvironmentVariable(environment []string, name string) []string {
	prefix := name + "="
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func writeClaudeBinUsage(out io.Writer, report claudeBinUsageReport, now time.Time) error {
	fmt.Fprintln(out, "Claude subscription usage")
	if report.SubscriptionType != nil && strings.TrimSpace(*report.SubscriptionType) != "" {
		fmt.Fprintf(out, "Plan: %s · live from Claude Code\n", titleCaseASCII(*report.SubscriptionType))
	} else {
		fmt.Fprintln(out, "Live from Claude Code")
	}
	fmt.Fprintln(out)

	if !report.RateLimitsAvailable || report.RateLimits == nil {
		fmt.Fprintln(out, "Plan usage is unavailable.")
		fmt.Fprintln(out, "Claude Code needs claude.ai subscription OAuth with the user:profile scope;")
		fmt.Fprintln(out, "API-key and third-party-provider sessions do not expose plan limits.")
		return nil
	}

	rows := claudeBinUsageRows(*report.RateLimits)
	if len(rows) == 0 {
		fmt.Fprintln(out, "No active usage windows were returned.")
		return nil
	}
	for i, row := range rows {
		if i > 0 {
			fmt.Fprintln(out)
		}
		writeClaudeBinUsageRow(out, row, now)
	}
	return nil
}

func claudeBinUsageRows(limits claudeBinRateLimits) []claudeBinUsageRow {
	rows := make([]claudeBinUsageRow, 0, 8)
	seen := make(map[string]bool)
	add := func(label string, limit *claudeBinRateLimit) {
		if limit != nil && limit.Utilization != nil {
			rows = append(rows, claudeBinUsageRow{Label: label, Utilization: limit.Utilization, ResetsAt: limit.ResetsAt})
			seen[strings.ToLower(label)] = true
		}
	}
	add("Current session", limits.FiveHour)
	add("Current week · all models", limits.SevenDay)
	add("Current week · OAuth apps", limits.SevenDayOAuthApps)
	add("Current week · Opus", limits.SevenDayOpus)
	add("Current week · Sonnet", limits.SevenDaySonnet)
	for _, limit := range limits.ModelScoped {
		label := "Current week · " + limit.DisplayName
		if limit.Utilization == nil || seen[strings.ToLower(label)] {
			continue
		}
		rows = append(rows, claudeBinUsageRow{
			Label:       label,
			Utilization: limit.Utilization,
			ResetsAt:    limit.ResetsAt,
		})
		seen[strings.ToLower(label)] = true
	}
	if extra := limits.ExtraUsage; extra != nil && extra.IsEnabled && extra.Utilization != nil {
		detail := "Extra usage enabled"
		if extra.UsedCredits != nil && extra.MonthlyLimit != nil {
			currency := "USD"
			if extra.Currency != nil && *extra.Currency != "" {
				currency = *extra.Currency
			}
			detail = fmt.Sprintf("%s of %s", formatUsageCurrency(currency, *extra.UsedCredits), formatUsageCurrency(currency, *extra.MonthlyLimit))
		}
		rows = append(rows, claudeBinUsageRow{Label: "Extra usage · this month", Utilization: extra.Utilization, Detail: detail})
	}
	return rows
}

func writeClaudeBinUsageRow(out io.Writer, row claudeBinUsageRow, now time.Time) {
	pct := math.Max(0, math.Min(100, *row.Utilization))
	fmt.Fprintln(out, row.Label)
	fmt.Fprintf(out, "%s  %3.0f%% used\n", claudeUsageBar(pct, 24), pct)
	parts := make([]string, 0, 2)
	if row.Detail != "" {
		parts = append(parts, row.Detail)
	}
	if row.ResetsAt != nil && *row.ResetsAt != "" {
		parts = append(parts, formatClaudeUsageReset(*row.ResetsAt, now))
	}
	if len(parts) > 0 {
		fmt.Fprintln(out, strings.Join(parts, " · "))
	}
}

func claudeUsageBar(percent float64, width int) string {
	filled := int(math.Round(math.Max(0, math.Min(100, percent)) / 100 * float64(width)))
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func formatClaudeUsageReset(raw string, now time.Time) string {
	reset, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return "resets " + raw
	}
	reset = reset.In(now.Location())
	delta := reset.Sub(now)
	if delta <= 0 {
		return "reset due now"
	}
	return fmt.Sprintf("resets in %s · %s", conciseDuration(delta), reset.Format("Mon 2 Jan, 3:04 PM"))
}

func conciseDuration(duration time.Duration) string {
	duration = duration.Round(time.Minute)
	if duration < time.Minute {
		return "<1m"
	}
	days := int(duration / (24 * time.Hour))
	duration %= 24 * time.Hour
	hours := int(duration / time.Hour)
	minutes := int((duration % time.Hour) / time.Minute)
	parts := make([]string, 0, 2)
	if days > 0 {
		parts = append(parts, strconv.Itoa(days)+"d")
		if hours > 0 {
			parts = append(parts, strconv.Itoa(hours)+"h")
		}
	} else if hours > 0 {
		parts = append(parts, strconv.Itoa(hours)+"h")
		if minutes > 0 {
			parts = append(parts, strconv.Itoa(minutes)+"m")
		}
	} else {
		parts = append(parts, strconv.Itoa(minutes)+"m")
	}
	return strings.Join(parts, " ")
}

func titleCaseASCII(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func formatUsageCurrency(currency string, value float64) string {
	symbols := map[string]string{
		"USD": "$",
		"EUR": "€",
		"GBP": "£",
		"JPY": "¥",
		"AUD": "A$",
		"CAD": "CA$",
		"NZD": "NZ$",
		"SGD": "S$",
	}
	if symbol := symbols[strings.ToUpper(currency)]; symbol != "" {
		return symbol + formatUsageAmount(value)
	}
	return strings.ToUpper(currency) + " " + formatUsageAmount(value)
}

func formatUsageAmount(value float64) string {
	if value == math.Trunc(value) {
		return strconv.FormatFloat(value, 'f', 0, 64)
	}
	return strconv.FormatFloat(value, 'f', 2, 64)
}

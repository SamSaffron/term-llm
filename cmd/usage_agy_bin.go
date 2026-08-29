package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/spf13/pflag"
)

const agyBinUsageMinimumVersion = "1.1.11"

var agyUsageSemverPattern = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)(-([0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*))?(\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$`)

type agyBinUsageEnvelope struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Response       string `json:"response"`
	NumTurns       int    `json:"num_turns"`
	Usage          struct {
		InputTokens     int `json:"input_tokens"`
		OutputTokens    int `json:"output_tokens"`
		ThinkingTokens  int `json:"thinking_tokens"`
		CacheReadTokens int `json:"cache_read_tokens"`
		TotalTokens     int `json:"total_tokens"`
	} `json:"usage"`
	Command *struct {
		Name string          `json:"name"`
		Data json.RawMessage `json:"data"`
	} `json:"command"`
}

type agyBinUsageReport struct {
	Description string             `json:"description,omitempty"`
	Groups      []agyBinUsageGroup `json:"groups"`
}

type agyBinUsageGroup struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Buckets     []agyBinUsageBucket `json:"buckets"`
}

type agyBinUsageBucket struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Description       string   `json:"description,omitempty"`
	Window            string   `json:"window"`
	RemainingFraction *float64 `json:"remaining_fraction"`
	ResetTime         string   `json:"reset_time,omitempty"`
	Disabled          bool     `json:"disabled,omitempty"`
}

var (
	agyUsageLookPath = exec.LookPath
	agyUsageCommand  = runAgyUsageCommand
	agyUsageExec     = runAgyUsageExec
)

func validateAgyBinUsageFlags(cmd interface{ Flags() *pflag.FlagSet }) error {
	var incompatible []string
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		if flag.Name != "provider" && flag.Name != "json" {
			incompatible = append(incompatible, "--"+flag.Name)
		}
	})
	if len(incompatible) > 0 {
		sort.Strings(incompatible)
		return fmt.Errorf("%s cannot be used with --provider agy-bin; live subscription usage only supports --json", strings.Join(incompatible, ", "))
	}
	return nil
}

func runAgyBinUsage(parent context.Context, out io.Writer, jsonOutput bool) error {
	binary, err := agyUsageLookPath("agy")
	if err != nil {
		return errors.New("agy-bin usage requires the Antigravity CLI (`agy`) in PATH")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()

	output, err := agyUsageCommand(ctx, binary)
	if err != nil {
		return err
	}
	raw, report, err := parseAgyBinUsageOutput(output)
	if err != nil {
		return err
	}
	if jsonOutput {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, raw, "", "  "); err != nil {
			return fmt.Errorf("format agy usage response: %w", err)
		}
		_, err := fmt.Fprintln(out, pretty.String())
		return err
	}
	return writeAgyBinUsageWithOptions(out, report, time.Now(), providerUsageFormatOptions(out))
}

func runAgyUsageCommand(ctx context.Context, binary string) ([]byte, error) {
	versionOutput, versionStderr, err := agyUsageExec(ctx, binary, "--version")
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("agy version check timed out: %w", ctx.Err())
		}
		return nil, agyUsageCommandFailure("check agy version", err, versionStderr)
	}
	version := strings.TrimSpace(string(versionOutput))
	if !agyUsageVersionAtLeast(version, 1, 1, 11) {
		if version == "" {
			version = "unknown"
		}
		return nil, fmt.Errorf("agy-bin usage requires agy %s or newer (found %s); older versions may send /usage to a model", agyBinUsageMinimumVersion, version)
	}

	// Since agy 1.1.11, /usage is a read-only print-mode command. It refreshes
	// quota and returns structured data without starting a turn, spending quota,
	// or creating a conversation. Attach the prompt to -p so appending flags in
	// the future cannot accidentally make a flag become the prompt.
	output, stderr, err := agyUsageExec(ctx, binary, "--output-format", "json", "-p=/usage")
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("agy usage request timed out: %w", ctx.Err())
		}
		return nil, agyUsageCommandFailure("fetch agy usage", err, stderr)
	}
	return output, nil
}

func runAgyUsageExec(ctx context.Context, binary string, args ...string) ([]byte, string, error) {
	command := exec.CommandContext(ctx, binary, args...)
	command.WaitDelay = 2 * time.Second
	output, err := command.Output()
	if err == nil {
		return output, "", nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return output, string(exitErr.Stderr), err
	}
	return output, "", err
}

func agyUsageCommandFailure(action string, commandErr error, stderr string) error {
	detail := strings.TrimSpace(stderr)
	if detail != "" {
		return fmt.Errorf("%s: %s: %w", action, detail, commandErr)
	}
	return fmt.Errorf("%s: %w", action, commandErr)
}

func agyUsageVersionAtLeast(version string, wantMajor, wantMinor, wantPatch int) bool {
	var matches [][]string
	for _, field := range strings.Fields(version) {
		candidate := strings.Trim(strings.TrimSpace(field), "()[]{}<>,;")
		if match := agyUsageSemverPattern.FindStringSubmatch(candidate); match != nil {
			matches = append(matches, match)
		}
	}
	// Fail closed if the output is unparseable or contains another version-like
	// token (for example, a runtime version before the actual agy version).
	if len(matches) != 1 {
		return false
	}
	match := matches[0]
	got := make([]int, 3)
	for i := range got {
		value, err := strconv.Atoi(match[i+1])
		if err != nil {
			return false
		}
		got[i] = value
	}
	want := []int{wantMajor, wantMinor, wantPatch}
	for i := range got {
		if got[i] != want[i] {
			return got[i] > want[i]
		}
	}
	// A prerelease of the minimum supported version sorts below the release that
	// introduced zero-token print-mode /usage.
	return match[4] == ""
}

func parseAgyBinUsageOutput(output []byte) (json.RawMessage, agyBinUsageReport, error) {
	var envelope agyBinUsageEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(output), &envelope); err != nil {
		return nil, agyBinUsageReport{}, fmt.Errorf("decode agy usage response: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(envelope.Status), "success") {
		detail := strings.TrimSpace(envelope.Response)
		if detail == "" {
			detail = "unknown error"
		}
		return nil, agyBinUsageReport{}, fmt.Errorf("agy usage request failed: %s", detail)
	}
	if strings.TrimSpace(envelope.ConversationID) != "" || envelope.NumTurns != 0 || envelope.Usage.InputTokens != 0 ||
		envelope.Usage.OutputTokens != 0 || envelope.Usage.ThinkingTokens != 0 || envelope.Usage.CacheReadTokens != 0 ||
		envelope.Usage.TotalTokens != 0 {
		return nil, agyBinUsageReport{}, errors.New("agy started or persisted an agent turn for /usage; refusing the result (update agy before retrying)")
	}
	if envelope.Command == nil || !strings.EqualFold(strings.TrimSpace(envelope.Command.Name), "usage") || len(envelope.Command.Data) == 0 {
		return nil, agyBinUsageReport{}, fmt.Errorf("agy did not return structured usage data (agy %s or newer is required)", agyBinUsageMinimumVersion)
	}
	var report agyBinUsageReport
	if err := json.Unmarshal(envelope.Command.Data, &report); err != nil {
		return nil, agyBinUsageReport{}, fmt.Errorf("decode structured agy usage data: %w", err)
	}
	if len(report.Groups) == 0 {
		return nil, agyBinUsageReport{}, errors.New("agy usage response did not contain quota groups")
	}
	raw := append(json.RawMessage(nil), envelope.Command.Data...)
	return raw, report, nil
}

func writeAgyBinUsageWithOptions(out io.Writer, report agyBinUsageReport, now time.Time, opts llm.ProviderUsageFormatOptions) error {
	providerReport, err := normalizeAgyBinUsage(report)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, llm.FormatProviderUsageWithOptions(providerReport, now, opts))
	return err
}

func normalizeAgyBinUsage(report agyBinUsageReport) (*llm.ProviderUsage, error) {
	providerReport := &llm.ProviderUsage{Provider: "antigravity", Source: "Live from agy"}
	for groupIndex, group := range report.Groups {
		limit := llm.ProviderUsageLimit{
			ID:      agyUsageID(group.Name, groupIndex),
			Name:    strings.TrimSpace(group.Name),
			Allowed: true,
		}
		var extras []llm.ProviderUsageLimit
		for bucketIndex, bucket := range group.Buckets {
			window, err := agyUsageWindow(bucket)
			if err != nil {
				return nil, err
			}
			if window == nil {
				continue
			}
			reached := bucket.Disabled || window.UsedPercent >= 100
			if reached {
				limit.Allowed = false
				limit.LimitReached = true
			}
			switch strings.ToLower(strings.TrimSpace(bucket.Window)) {
			case "5h", "five_hour", "five-hour":
				if limit.PrimaryWindow == nil {
					limit.PrimaryWindow = window
					continue
				}
			case "weekly", "week", "7d":
				if limit.SecondaryWindow == nil {
					limit.SecondaryWindow = window
					continue
				}
			default:
				if limit.PrimaryWindow == nil {
					limit.PrimaryWindow = window
					continue
				}
				if limit.SecondaryWindow == nil {
					limit.SecondaryWindow = window
					continue
				}
			}
			extras = append(extras, llm.ProviderUsageLimit{
				ID:            agyUsageID(bucket.ID, bucketIndex),
				Name:          strings.TrimSpace(bucket.Name),
				Allowed:       !reached,
				LimitReached:  reached,
				PrimaryWindow: window,
			})
		}
		if limit.Name == "" {
			limit.Name = limit.ID
		}
		if limit.PrimaryWindow != nil || limit.SecondaryWindow != nil {
			providerReport.Limits = append(providerReport.Limits, limit)
		}
		providerReport.Limits = append(providerReport.Limits, extras...)
	}
	if len(providerReport.Limits) == 0 {
		return nil, errors.New("agy usage response did not contain usable quota buckets")
	}
	return providerReport, nil
}

func agyUsageWindow(bucket agyBinUsageBucket) (*llm.ProviderUsageWindow, error) {
	if bucket.RemainingFraction == nil {
		return nil, nil
	}
	remaining := min(1, max(0, *bucket.RemainingFraction))
	window := &llm.ProviderUsageWindow{
		Label:           agyUsageWindowLabel(bucket),
		UsedPercent:     (1 - remaining) * 100,
		DurationMinutes: agyUsageWindowDuration(bucket.Window),
	}
	if reset := strings.TrimSpace(bucket.ResetTime); reset != "" {
		parsed, err := time.Parse(time.RFC3339, reset)
		if err != nil {
			return nil, fmt.Errorf("decode agy quota bucket %q reset time %q: %w", bucket.ID, reset, err)
		}
		window.ResetsAt = parsed
	}
	return window, nil
}

func agyUsageWindowLabel(bucket agyBinUsageBucket) string {
	switch strings.ToLower(strings.TrimSpace(bucket.Window)) {
	case "5h", "five_hour", "five-hour":
		return "5 hour window"
	case "weekly", "week", "7d":
		return "1 week window"
	}
	label := strings.TrimSpace(strings.TrimSuffix(bucket.Name, " Remaining"))
	if label != "" {
		return label
	}
	return "Usage window"
}

func agyUsageWindowDuration(window string) int {
	switch strings.ToLower(strings.TrimSpace(window)) {
	case "5h", "five_hour", "five-hour":
		return 5 * 60
	case "weekly", "week", "7d":
		return 7 * 24 * 60
	default:
		return 0
	}
}

func agyUsageID(value string, fallback int) string {
	id := strings.ToLower(strings.TrimSpace(value))
	id = strings.NewReplacer(" ", "_", "-", "_", "/", "_").Replace(id)
	id = strings.Trim(id, "_")
	if id == "" {
		return fmt.Sprintf("quota_%d", fallback+1)
	}
	return id
}

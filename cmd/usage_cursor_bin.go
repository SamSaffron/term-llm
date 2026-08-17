package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/spf13/pflag"
)

const cursorBinUsageURL = "https://api2.cursor.sh/aiserver.v1.DashboardService/GetCurrentPeriodUsage"

type cursorBinStatus struct {
	Auth struct {
		AccessToken string `json:"accessToken"`
	} `json:"auth"`
}

type cursorBinUsageReport struct {
	BillingCycleStart json.RawMessage       `json:"billingCycleStart"`
	BillingCycleEnd   json.RawMessage       `json:"billingCycleEnd"`
	PlanUsage         *cursorBinUsageBucket `json:"planUsage"`
	SpendLimitUsage   *cursorBinUsageBucket `json:"spendLimitUsage"`
}

type cursorBinUsageBucket struct {
	Used             cursorBinUsageNumber `json:"used"`
	Limit            cursorBinUsageNumber `json:"limit"`
	Remaining        cursorBinUsageNumber `json:"remaining"`
	TotalPercentUsed cursorBinUsageNumber `json:"totalPercentUsed"`
}

type cursorBinUsageNumber struct {
	Value float64
	Set   bool
}

func (n *cursorBinUsageNumber) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	var value float64
	if len(data) > 0 && data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return err
		}
		value = parsed
	} else if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	n.Value = value
	n.Set = true
	return nil
}

var (
	cursorUsageLookPath    = exec.LookPath
	cursorUsageCommand     = runCursorUsageStatusCommand
	cursorUsageStoredToken = cursorAgentStoredAccessToken
	cursorUsageClient      = http.DefaultClient
	cursorUsageEndpoint    = cursorBinUsageURL
)

func validateCursorBinUsageFlags(cmd interface{ Flags() *pflag.FlagSet }) error {
	var incompatible []string
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		if flag.Name != "provider" && flag.Name != "json" {
			incompatible = append(incompatible, "--"+flag.Name)
		}
	})
	if len(incompatible) > 0 {
		sort.Strings(incompatible)
		return fmt.Errorf("%s cannot be used with --provider cursor-bin; live subscription usage only supports --json", strings.Join(incompatible, ", "))
	}
	return nil
}

func runCursorBinUsage(parent context.Context, out io.Writer, jsonOutput bool) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()

	token, err := cursorAgentAccessToken(ctx)
	if err != nil {
		return err
	}
	raw, report, err := fetchCursorBinUsage(ctx, token)
	if err != nil {
		return err
	}
	if jsonOutput {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, raw, "", "  "); err != nil {
			return fmt.Errorf("format Cursor usage response: %w", err)
		}
		_, err := fmt.Fprintln(out, pretty.String())
		return err
	}
	return writeCursorBinUsageWithOptions(out, report, time.Now(), providerUsageFormatOptions(out))
}

func cursorAgentAccessToken(ctx context.Context) (string, error) {
	var binaries []string
	for _, name := range []string{"agent", "cursor-agent"} {
		path, err := cursorUsageLookPath(name)
		if err == nil && !containsString(binaries, path) {
			binaries = append(binaries, path)
		}
	}
	if len(binaries) == 0 {
		return "", errors.New("cursor-bin usage requires the Cursor Agent CLI (`agent` or `cursor-agent`) in PATH")
	}

	for _, binary := range binaries {
		stdout, err := cursorUsageCommand(ctx, binary)
		if err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("Cursor Agent status request timed out: %w", ctx.Err())
			}
			continue
		}
		var status cursorBinStatus
		if json.Unmarshal(stdout, &status) == nil {
			if token := strings.TrimSpace(status.Auth.AccessToken); token != "" {
				return token, nil
			}
		}
	}

	token, err := cursorUsageStoredToken()
	if err != nil {
		return "", fmt.Errorf("read Cursor Agent login credentials: %w", err)
	}
	if token != "" {
		return token, nil
	}
	return "", errors.New("Cursor Agent is not logged in; run `agent login` and try again")
}

func cursorAgentStoredAccessToken() (string, error) {
	return cursorAgentAccessTokenFromFiles(cursorAgentAuthFilePaths())
}

func cursorAgentAccessTokenFromFiles(paths []string) (string, error) {
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		var auth struct {
			AccessToken string `json:"accessToken"`
		}
		if err := json.Unmarshal(data, &auth); err != nil {
			return "", fmt.Errorf("decode %s: %w", path, err)
		}
		if token := strings.TrimSpace(auth.AccessToken); token != "" {
			return token, nil
		}
	}
	return "", nil
}

func cursorAgentAuthFilePaths() []string {
	var paths []string
	add := func(path string) {
		if strings.TrimSpace(path) != "" && !containsString(paths, path) {
			paths = append(paths, path)
		}
	}
	if dir := strings.TrimSpace(os.Getenv("CURSOR_CONFIG_DIR")); dir != "" {
		add(filepath.Join(dir, "auth.json"))
	}
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			add(filepath.Join(home, ".cursor", "auth.json"))
		}
		return paths
	}
	if configDir, err := os.UserConfigDir(); err == nil {
		name := "cursor"
		if runtime.GOOS == "windows" {
			name = "Cursor"
		}
		add(filepath.Join(configDir, name, "auth.json"))
	}
	return paths
}

func containsString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func runCursorUsageStatusCommand(ctx context.Context, binary string) ([]byte, error) {
	command := exec.CommandContext(ctx, binary, "status", "--format", "json")
	return command.Output()
}

func fetchCursorBinUsage(ctx context.Context, token string) (json.RawMessage, cursorBinUsageReport, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cursorUsageEndpoint, strings.NewReader("{}"))
	if err != nil {
		return nil, cursorBinUsageReport{}, fmt.Errorf("create Cursor usage API request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("User-Agent", "term-llm")

	client := cursorUsageClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, cursorBinUsageReport{}, fmt.Errorf("Cursor usage API request timed out: %w", ctx.Err())
		}
		return nil, cursorBinUsageReport{}, fmt.Errorf("Cursor usage API request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, cursorBinUsageReport{}, errors.New("Cursor usage API rejected the login; run `agent login` and try again")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, cursorBinUsageReport{}, fmt.Errorf("Cursor usage API returned %s", response.Status)
	}

	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, cursorBinUsageReport{}, fmt.Errorf("read Cursor usage API response: %w", err)
	}
	var report cursorBinUsageReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, cursorBinUsageReport{}, fmt.Errorf("decode Cursor usage API response: %w", err)
	}
	if report.PlanUsage == nil {
		return nil, cursorBinUsageReport{}, errors.New("Cursor usage API response did not contain plan usage")
	}
	if _, ok := cursorUsagePercent(report.PlanUsage); !ok {
		return nil, cursorBinUsageReport{}, errors.New("Cursor usage API response did not contain plan usage totals")
	}
	return raw, report, nil
}

func writeCursorBinUsageWithOptions(out io.Writer, report cursorBinUsageReport, now time.Time, opts llm.ProviderUsageFormatOptions) error {
	reset := parseCursorUsageTime(report.BillingCycleEnd)
	start := parseCursorUsageTime(report.BillingCycleStart)
	duration := 0
	if !start.IsZero() && reset.After(start) {
		duration = int(reset.Sub(start).Round(time.Minute) / time.Minute)
	}

	providerReport := &llm.ProviderUsage{Provider: "cursor"}
	providerReport.Limits = append(providerReport.Limits, cursorUsageLimit("plan", "Plan usage", report.PlanUsage, reset, duration))
	if cursorUsageBucketPresent(report.SpendLimitUsage) {
		providerReport.Limits = append(providerReport.Limits, cursorUsageLimit("spend_limit", "Spend limit", report.SpendLimitUsage, reset, duration))
	}
	_, err := fmt.Fprintln(out, llm.FormatProviderUsageWithOptions(providerReport, now, opts))
	return err
}

func cursorUsageLimit(id, name string, bucket *cursorBinUsageBucket, reset time.Time, duration int) llm.ProviderUsageLimit {
	percent, _ := cursorUsagePercent(bucket)
	window := &llm.ProviderUsageWindow{
		Label:           "Billing cycle",
		UsedPercent:     percent,
		DurationMinutes: duration,
		ResetsAt:        reset,
		Detail:          cursorUsageDetail(bucket),
	}
	return llm.ProviderUsageLimit{ID: id, Name: name, Allowed: percent < 100, LimitReached: percent >= 100, PrimaryWindow: window}
}

func cursorUsagePercent(bucket *cursorBinUsageBucket) (float64, bool) {
	if bucket == nil {
		return 0, false
	}
	if bucket.TotalPercentUsed.Set {
		return bucket.TotalPercentUsed.Value, true
	}
	if bucket.Used.Set && bucket.Limit.Set && bucket.Limit.Value > 0 {
		return bucket.Used.Value / bucket.Limit.Value * 100, true
	}
	return 0, false
}

func cursorUsageBucketPresent(bucket *cursorBinUsageBucket) bool {
	if bucket == nil {
		return false
	}
	_, hasPercent := cursorUsagePercent(bucket)
	return hasPercent || bucket.Used.Set || bucket.Limit.Set || bucket.Remaining.Set
}

func cursorUsageDetail(bucket *cursorBinUsageBucket) string {
	if bucket == nil {
		return ""
	}
	var parts []string
	if bucket.Used.Set && bucket.Limit.Set {
		parts = append(parts, fmt.Sprintf("%s of %s used", formatCursorUsageNumber(bucket.Used.Value), formatCursorUsageNumber(bucket.Limit.Value)))
	}
	if bucket.Remaining.Set {
		parts = append(parts, formatCursorUsageNumber(bucket.Remaining.Value)+" remaining")
	}
	return strings.Join(parts, " · ")
}

func formatCursorUsageNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func parseCursorUsageTime(raw json.RawMessage) time.Time {
	data := bytes.TrimSpace(raw)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return time.Time{}
	}
	var text string
	if data[0] == '"' {
		if err := json.Unmarshal(data, &text); err != nil {
			return time.Time{}
		}
		if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(text)); err == nil {
			return parsed
		}
	} else {
		text = string(data)
	}
	milliseconds, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		return time.Time{}
	}
	seconds := int64(milliseconds) / 1000
	nanoseconds := (int64(milliseconds) % 1000) * int64(time.Millisecond)
	return time.Unix(seconds, nanoseconds)
}

package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/spf13/cobra"
)

const (
	updateCheckInterval   = 24 * time.Hour
	updateUserAgent       = "term-llm-cli"
	SkipUpdateEnvVar      = "TERM_LLM_SKIP_UPDATE_CHECK"
	RepoOwner             = "samsaffron"
	RepoName              = "term-llm"
	updateCheckCommandArg = "__update-check"
)

// ReleaseInfo contains information about a GitHub release
type ReleaseInfo struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// UpdateCheckCmd is the hidden command for background update checks
var UpdateCheckCmd = &cobra.Command{
	Use:    updateCheckCommandArg,
	Short:  "internal update check",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if os.Getenv(SkipUpdateEnvVar) == "" {
			os.Setenv(SkipUpdateEnvVar, "1")
		}
		return PerformUpdateCheck(cmd.Context())
	},
}

// SetupUpdateChecks initializes update checking on CLI startup
func SetupUpdateChecks(rootCmd *cobra.Command, version string) {
	rootCmd.AddCommand(UpdateCheckCmd)
	cobra.OnInitialize(func() {
		if os.Getenv(SkipUpdateEnvVar) != "" {
			return
		}
		if version == "dev" {
			return
		}
		state, err := LoadState()
		if err == nil {
			WarnIfOutdated(version, state)
		}
		if ShouldCheckForUpdates(state) {
			if err := LaunchBackgroundUpdateCheck(); err != nil {
				fmt.Fprintf(os.Stderr, "term-llm: failed to schedule update check: %v\n", err)
			}
		}
	})
}

// ShouldCheckForUpdates returns true if enough time has passed since last check
func ShouldCheckForUpdates(state *State) bool {
	if state == nil {
		return true
	}
	if state.LastChecked.IsZero() {
		return true
	}
	return time.Since(state.LastChecked) >= updateCheckInterval
}

// LaunchBackgroundUpdateCheck spawns a background process to check for updates
func LaunchBackgroundUpdateCheck() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, updateCheckCommandArg)
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=1", SkipUpdateEnvVar))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Start()
}

// PerformUpdateCheck fetches the latest release and updates state
func PerformUpdateCheck(ctx context.Context) error {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return err
	}
	state, err := loadStateFromDir(configDir)
	if err != nil {
		state = &State{}
	}
	info, err := FetchLatestRelease(ctx)
	now := time.Now().UTC()
	if err != nil {
		state.LastChecked = now
		state.LastError = err.Error()
		return saveStateToDir(configDir, state)
	}
	state.LastChecked = now
	state.LatestVersion = info.TagName
	state.LastError = ""
	return saveStateToDir(configDir, state)
}

// WarnIfOutdated prints a warning if a newer version is available
func WarnIfOutdated(currentVersion string, state *State) {
	if state == nil {
		return
	}
	latest := strings.TrimSpace(state.LatestVersion)
	if latest == "" {
		return
	}
	if !IsVersionOutdated(currentVersion, latest) {
		return
	}
	shouldWarn := state.NotifiedVersion != latest ||
		state.LastNotified.IsZero() ||
		time.Since(state.LastNotified) >= updateCheckInterval
	if !shouldWarn {
		return
	}
	fmt.Fprintf(os.Stderr, "A newer term-llm release (%s) is available. Run 'term-llm upgrade' to update.\n", latest)
	state.NotifiedVersion = latest
	state.LastNotified = time.Now().UTC()
	_ = SaveState(state)
}

// FetchLatestRelease gets the latest release info from GitHub
func FetchLatestRelease(ctx context.Context) (*ReleaseInfo, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", RepoOwner, RepoName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", updateUserAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("unexpected response %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var info ReleaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	if strings.TrimSpace(info.TagName) == "" {
		return nil, errors.New("latest release is missing tag name")
	}
	return &info, nil
}

// IsVersionOutdated returns true if current is older than latest
func IsVersionOutdated(current, latest string) bool {
	current = NormalizeVersion(current)
	latest = NormalizeVersion(latest)
	if current == "" || latest == "" {
		return false
	}
	cmp, ok := CompareVersionStrings(current, latest)
	if !ok {
		return false
	}
	return cmp < 0
}

// NormalizeVersion strips v prefix and any non-numeric suffixes
func NormalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexFunc(v, func(r rune) bool { return !(r >= '0' && r <= '9') && r != '.' }); i >= 0 {
		v = v[:i]
	}
	return v
}

// CompareVersionStrings compares two version strings
// Returns -1 if a < b, 0 if equal, 1 if a > b
func CompareVersionStrings(a, b string) (int, bool) {
	aParts, ok := parseVersionParts(a)
	if !ok {
		return 0, false
	}
	bParts, ok := parseVersionParts(b)
	if !ok {
		return 0, false
	}
	maxLen := len(aParts)
	if len(bParts) > maxLen {
		maxLen = len(bParts)
	}
	for len(aParts) < maxLen {
		aParts = append(aParts, 0)
	}
	for len(bParts) < maxLen {
		bParts = append(bParts, 0)
	}
	for i := 0; i < maxLen; i++ {
		if aParts[i] < bParts[i] {
			return -1, true
		}
		if aParts[i] > bParts[i] {
			return 1, true
		}
	}
	return 0, true
}

func parseVersionParts(v string) ([]int, bool) {
	if v == "" {
		return nil, false
	}
	pieces := strings.Split(v, ".")
	parts := make([]int, len(pieces))
	for i, piece := range pieces {
		if piece == "" {
			return nil, false
		}
		n, err := strconv.Atoi(piece)
		if err != nil {
			return nil, false
		}
		parts[i] = n
	}
	return parts, true
}

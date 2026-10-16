package llm

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// agyBinMinimumVersion is the first agy release that honours the MCP
// antigravity/alwaysLoad tool annotation. agy-bin depends on it because a
// custom agent cannot call lazily loaded MCP tools (agy issue #674).
const agyBinMinimumVersion = "1.3.3"

const agyVersionCheckTimeout = 10 * time.Second

var agySemverPattern = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)(-([0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*))?(\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$`)

// AgyVersionAtLeast reports whether `agy --version` output names a release at
// or above the requested version. Unparseable or ambiguous output fails closed.
func AgyVersionAtLeast(version string, wantMajor, wantMinor, wantPatch int) bool {
	var matches [][]string
	for _, field := range strings.Fields(version) {
		candidate := strings.Trim(strings.TrimSpace(field), "()[]{}<>,;")
		if match := agySemverPattern.FindStringSubmatch(candidate); match != nil {
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
	// A prerelease of the minimum version sorts below the release itself.
	return match[4] == ""
}

// agyVersionOutput runs `agy --version` with the provider's configured
// environment; tests replace it.
var agyVersionOutput = func(ctx context.Context, binary string, env []string) ([]byte, error) {
	cmd, err := newCLICommand(ctx, binary, []string{"--version"}, "")
	if err != nil {
		return nil, err
	}
	cmd.Env = env
	return cmd.Output()
}

// ensureSupportedVersion checks the resolved agy binary once per path.
func (p *AgyBinProvider) ensureSupportedVersion(ctx context.Context) error {
	if p.versionCheckedBinary == p.agyBinary {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, agyVersionCheckTimeout)
	defer cancel()
	out, err := agyVersionOutput(ctx, p.agyBinary, p.extraEnvList())
	if err != nil {
		return fmt.Errorf("check agy version: %w", err)
	}
	version := strings.TrimSpace(string(out))
	if !AgyVersionAtLeast(version, 1, 3, 3) {
		if version == "" {
			version = "unknown"
		}
		return &UserFacingProviderError{
			Summary: fmt.Sprintf("agy-bin requires agy %s or newer (found %s)", agyBinMinimumVersion, version),
			Detail:  "Run `agy update` to upgrade the Antigravity CLI.",
		}
	}
	p.versionCheckedBinary = p.agyBinary
	return nil
}

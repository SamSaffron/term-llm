package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// RunUpgrade performs the upgrade to the specified version (or latest if empty)
func RunUpgrade(ctx context.Context, currentVersion, targetVersion string, stdout, stderr io.Writer) error {
	targetTag := strings.TrimSpace(targetVersion)
	if targetTag == "" {
		info, err := FetchLatestRelease(ctx)
		if err != nil {
			return fmt.Errorf("fetch latest release: %w", err)
		}
		targetTag = info.TagName
	}
	if !strings.HasPrefix(targetTag, "v") {
		targetTag = "v" + targetTag
	}

	if !validReleaseTag(targetTag) {
		return fmt.Errorf("invalid release version %q", targetTag)
	}

	currentNorm := NormalizeVersion(currentVersion)
	targetNorm := NormalizeVersion(targetTag)
	if currentVersion != "dev" && currentNorm != "" && targetNorm != "" {
		cmp, ok := CompareVersionStrings(currentNorm, targetNorm)
		if ok && cmp >= 0 {
			fmt.Fprintf(stderr, "term-llm is already up to date (%s).\n", currentVersion)
			if cmp > 0 {
				fmt.Fprintf(stderr, "Requested version %s is not newer than the current version.\n", targetTag)
			}
			return nil
		}
	}

	assetVersion := strings.TrimPrefix(targetTag, "v")
	assetName := fmt.Sprintf("%s_%s_%s_%s.tar.gz", RepoName, assetVersion, runtime.GOOS, runtime.GOARCH)
	releaseURL := fmt.Sprintf("https://github.com/%s/%s/releases/download/%s", RepoOwner, RepoName, targetTag)

	tmpDir, err := os.MkdirTemp("", "term-llm-upgrade-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	archivePath, err := downloadVerifiedArchive(ctx, releaseHTTPClient(), releaseURL, assetName, tmpDir)
	if err != nil {
		return err
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("determine executable path: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(exePath)
	if err == nil && resolvedPath != "" {
		exePath = resolvedPath
	}

	destDir := filepath.Dir(exePath)
	tempFile, err := os.CreateTemp(destDir, ".term-llm-upgrade-*")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("cannot create temp file in %s (permission denied). try running with sudo", destDir)
		}
		return err
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)

	if err := extractBinaryTo(archivePath, tempFile); err != nil {
		tempFile.Close()
		return err
	}

	if err := tempFile.Close(); err != nil {
		return err
	}

	if err := os.Chmod(tempPath, 0o755); err != nil {
		return err
	}

	if err := os.Rename(tempPath, exePath); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("failed to replace %s (permission denied). try running with sudo", exePath)
		}
		return fmt.Errorf("replace binary: %w", err)
	}

	// Update state to suppress future warnings
	if state, err := LoadState(); err == nil {
		state.LastChecked = time.Now().UTC()
		state.LatestVersion = targetTag
		state.NotifiedVersion = targetTag
		state.LastNotified = time.Now().UTC()
		state.LastError = ""
		_ = SaveState(state)
	}

	fmt.Fprint(stdout, upgradeSuccessMessage(currentVersion, targetTag, exePath))
	return nil
}

func upgradeSuccessMessage(currentVersion, targetVersion, exePath string) string {
	return fmt.Sprintf("term-llm upgraded %s -> %s at %s\n", versionDisplayTag(currentVersion), versionDisplayTag(targetVersion), exePath)
}

func versionDisplayTag(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "unknown"
	}
	if strings.HasPrefix(version, "v") || version == "dev" {
		return version
	}
	if NormalizeVersion(version) != "" {
		return "v" + version
	}
	return version
}

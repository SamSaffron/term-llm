package main

import (
	"os"
	"strings"
	"testing"
)

func TestInstallScriptResolvesLatestReleaseWithoutGitHubAPI(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	script := string(data)
	for _, want := range []string{
		`latest_url="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/latest"`,
		`-w '%{url_effective}\n'`,
		`*/${REPO_OWNER}/${REPO_NAME}/releases/tag/*) tag=${release_url##*/}`,
		`releases/download/${VERSION}/${ASSET}`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("install.sh no longer resolves latest releases through the GitHub redirect; missing %q", want)
		}
	}
	if strings.Contains(script, "api.github.com") {
		t.Fatal("install.sh should resolve release redirects without the GitHub API")
	}
}

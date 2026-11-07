//go:build windows

package update

import "testing"

func TestReleaseArchiveNameWindows(t *testing.T) {
	if got, want := releaseArchiveName("1.2.3"), "term-llm_1.2.3_windows_amd64.zip"; got != want {
		t.Fatalf("releaseArchiveName = %q, want %q", got, want)
	}
	if got, want := releaseBinaryName(), "term-llm.exe"; got != want {
		t.Fatalf("releaseBinaryName = %q, want %q", got, want)
	}
}

//go:build !windows

package update

import (
	"runtime"
	"testing"
)

func TestReleaseArchiveNameUnix(t *testing.T) {
	want := "term-llm_1.2.3_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	if got := releaseArchiveName("1.2.3"); got != want {
		t.Fatalf("releaseArchiveName = %q, want %q", got, want)
	}
	if got := releaseBinaryName(); got != "term-llm" {
		t.Fatalf("releaseBinaryName = %q", got)
	}
}

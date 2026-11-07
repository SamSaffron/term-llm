//go:build !windows

package pathutil

import "testing"

func TestParseLocalFileURLRejectsRemoteHost(t *testing.T) {
	if _, _, err := ParseLocalFileURL("file://example.com/path"); err == nil {
		t.Fatal("remote host unexpectedly accepted")
	}
}

func TestParseLocalFileURLPreservesUnixPathAndFragment(t *testing.T) {
	path, fragment, err := ParseLocalFileURL("file:///tmp/a%20b#models/default")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/tmp/a b" || fragment != "models/default" {
		t.Fatalf("path=%q fragment=%q", path, fragment)
	}
}

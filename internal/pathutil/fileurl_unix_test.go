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

func TestParseLocalFileURLDecodesAndRejectsEscapes(t *testing.T) {
	path, fragment, err := ParseLocalFileURL("file:///tmp/a%23b#frag%20ment")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/tmp/a#b" || fragment != "frag ment" {
		t.Fatalf("path=%q fragment=%q", path, fragment)
	}
	for _, in := range []string{"file:///tmp/bad%2", "file:///tmp/ok#bad%zz"} {
		if _, _, err := ParseLocalFileURL(in); err == nil {
			t.Fatalf("%s: malformed escape accepted", in)
		}
	}
}

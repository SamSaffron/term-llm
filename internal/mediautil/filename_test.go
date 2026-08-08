package mediautil

import "testing"

func TestSanitizeFilename(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "spaces and case", in: "Hello World", want: "hello_world"},
		{name: "unsafe characters", in: `a/b\\c:d?e*f"g<h>i|j`, want: "abcdefghij"},
		{name: "underscores", in: "__a___b__", want: "a_b"},
		{name: "portable characters", in: "A-b_12", want: "a-b_12"},
		{name: "unsupported unicode", in: " café ", want: "caf"},
		{name: "empty", in: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeFilename(tc.in); got != tc.want {
				t.Fatalf("SanitizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExpandPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := ExpandPath("plain/path"); got != "plain/path" {
		t.Fatalf("ExpandPath plain path = %q", got)
	}
	if got := ExpandPath("~/media"); got == "~/media" {
		t.Fatalf("ExpandPath did not expand home: %q", got)
	}
}

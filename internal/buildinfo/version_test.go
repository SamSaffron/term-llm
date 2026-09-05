package buildinfo

import "testing"

func TestUserAgentUsesApplicationVersion(t *testing.T) {
	original := Version
	defer func() { Version = original }()
	for _, tc := range []struct{ version, want string }{{"v0.9.25", "term-llm/0.9.25"}, {"dev", "term-llm/dev"}, {"", "term-llm/dev"}, {"bad\r\nheader", "term-llm/dev"}} {
		Version = tc.version
		if got := UserAgent(); got != tc.want {
			t.Fatalf("version %q got %q want %q", tc.version, got, tc.want)
		}
	}
}

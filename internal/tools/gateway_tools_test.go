package tools

import "testing"

func TestParseToolsFlagNoneDisablesAllTools(t *testing.T) {
	if got := ParseToolsFlag("none"); got == nil || len(got) != 0 {
		t.Fatalf("ParseToolsFlag(none) = %#v, want non-nil empty list", got)
	}
	if got := ParseToolsFlag(" NONE "); got == nil || len(got) != 0 {
		t.Fatalf("ParseToolsFlag(NONE) = %#v, want non-nil empty list", got)
	}
}

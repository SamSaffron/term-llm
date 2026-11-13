package shellcmd

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitShellWordsPreservesQuotesAndEmptyArguments(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  []string
	}{
		{`cmd "two words" 'three words' plain\ value "" ''`, []string{"cmd", "two words", "three words", "plain value", "", ""}},
		{" \t\n", nil},
		{`"escaped\"quote"`, []string{`escaped"quote`}},
	} {
		got, err := SplitShellWords(tc.input)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("split %q = %#v, %v; want %#v", tc.input, got, err, tc.want)
		}
	}
	for _, input := range []string{`"unterminated`, `'unterminated`, `trailing\`} {
		if _, err := SplitShellWords(input); err == nil || err.Error() != "unterminated quoted argument" {
			t.Errorf("malformed %q error = %v", input, err)
		}
	}
}

func TestHasUnsafeShellSyntaxPreservesQuotingRules(t *testing.T) {
	for _, input := range []string{`echo "$(whoami)"`, "a\nb", "a|b", "a>b", "echo `whoami`"} {
		if !HasUnsafeShellSyntax(input) {
			t.Errorf("unsafe syntax accepted: %q", input)
		}
	}
	for _, input := range []string{`echo '$(literal)'`, `echo "a|b"`, `echo \$literal`, `echo a\|b`} {
		if HasUnsafeShellSyntax(input) {
			t.Errorf("literal syntax rejected: %q", input)
		}
	}
}

func TestApprovalPatternValidationAndMatching(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		message string
	}{
		{"", "shell pattern is required"},
		{`echo [`, `invalid glob in shell word "["`},
		{`echo "`, "unterminated quoted argument"},
	} {
		if err := ValidateApprovalPattern(tc.pattern); err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Errorf("pattern %q error = %v; want %q", tc.pattern, err, tc.message)
		}
	}
	if err := ValidateApprovalPattern(`git "two words" **/*.go`); err != nil {
		t.Fatal(err)
	}
	if !MatchPattern("**/*.go", "deep/path/file.go") || MatchPattern("*.go", "deep/file.go") || MatchPattern("[", "x") {
		t.Fatal("word glob matching changed")
	}
}

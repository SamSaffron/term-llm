package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/complexity"
)

func TestCheckComplexityWarnsOnlyAboveThirty(t *testing.T) {
	fn := func(name string, score int) complexity.Function {
		return complexity.Function{Module: ".", Package: "cmd", Path: "cmd/example.go", Name: name, Complexity: score}
	}
	old := fn("existing", 24)
	high := fn("high", 40)
	b := baseline{Exceptions: []baselineException{
		{Key: complexity.Key(old), Complexity: 24, Owner: "cmd", Rationale: "existing", Removal: "M7", Origin: "existing"},
		{Key: complexity.Key(high), Complexity: 40, Owner: "cmd", Rationale: "existing", Removal: "M7", Origin: "existing"},
	}}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		fns  []complexity.Function
		want string
	}{
		{name: "new and existing through thirty", fns: []complexity.Function{fn("new", 30), fn("existing", 30)}},
		{name: "new over thirty", fns: []complexity.Function{fn("new", 31)}, want: "new function above 30"},
		{name: "cross thirty", fns: []complexity.Function{fn("existing", 31)}, want: "24 -> 31"},
		{name: "existing high unchanged", fns: []complexity.Function{high}},
		{name: "existing high increased", fns: []complexity.Function{fn("high", 41)}, want: "40 -> 41"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings, err := check(path, complexity.Report{Functions: tc.fns})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" && len(warnings) != 0 {
				t.Fatalf("unexpected warnings: %v", warnings)
			}
			if tc.want != "" && (len(warnings) != 1 || !strings.Contains(warnings[0], tc.want)) {
				t.Fatalf("warnings = %v, want %q", warnings, tc.want)
			}
		})
	}
}

func TestCheckComplexityRejectsIncompleteBaseline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, []byte(`{"exceptions":[{"key":"missing-metadata","complexity":50}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := check(path, complexity.Report{}); err == nil || !strings.Contains(err.Error(), "lacks ownership") {
		t.Fatalf("check error = %v, want missing metadata", err)
	}
}

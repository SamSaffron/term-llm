package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/complexity"
)

func TestReportSizes(t *testing.T) {
	var out bytes.Buffer
	if !reportSizes(&out, nil, []string{"file shrank: a"}) || !strings.Contains(out.String(), "size note: file shrank: a") || !strings.Contains(out.String(), "not a failure") {
		t.Fatalf("notes-only output = %q", out.String())
	}
	out.Reset()
	if reportSizes(&out, []string{"bad"}, nil) || !strings.Contains(out.String(), "size violation: bad") || !strings.Contains(out.String(), "1 size violation(s)") {
		t.Fatalf("violation output = %q", out.String())
	}
}

func TestWriteReports(t *testing.T) {
	report := complexity.Report{
		Files: []complexity.File{{Path: "a.go"}},
		Functions: []complexity.Function{
			{Module: ".", Path: "a.go", Name: "low", Complexity: 5},
			{Module: ".", Path: "a.go", Receiver: "T", Name: "high", Complexity: 40, Lines: 9},
		},
	}
	var out bytes.Buffer
	writeTextReport(&out, report, 20, true)
	if !strings.Contains(out.String(), "production=true") || !strings.Contains(out.String(), "(T).high") || strings.Contains(out.String(), "low") {
		t.Fatalf("text = %q", out.String())
	}
	out.Reset()
	if err := writeJSONReport(&out, report, 20); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "high") || strings.Contains(out.String(), `"low"`) {
		t.Fatalf("json = %q", out.String())
	}
}

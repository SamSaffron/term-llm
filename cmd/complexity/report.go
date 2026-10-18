package main

import (
	"fmt"
	"io"

	"github.com/samsaffron/term-llm/internal/complexity"
)

// reportSizes prints size notes and violations to w and reports whether the
// size ratchet passed. Notes alone never fail.
func reportSizes(w io.Writer, violations, notes []string) bool {
	for _, n := range notes {
		fmt.Fprintln(w, "size note:", n)
	}
	if len(notes) > 0 {
		fmt.Fprintln(w, "complexity: size baseline is out of date (not a failure); run `make complexity-baseline` to lock in the gain")
	}
	if len(violations) == 0 {
		return true
	}
	for _, v := range violations {
		fmt.Fprintln(w, "size violation:", v)
	}
	fmt.Fprintf(w, "complexity: %d size violation(s); split the file or package (see ledger/go-size-baseline.json rules)\n", len(violations))
	return false
}

// writeJSONReport writes the report with only functions above threshold.
func writeJSONReport(w io.Writer, report complexity.Report, threshold int) error {
	filtered := report
	filtered.Functions = nil
	for _, fn := range report.Functions {
		if fn.Complexity > threshold {
			filtered.Functions = append(filtered.Functions, fn)
		}
	}
	data, err := complexity.Encode(filtered)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// writeTextReport writes the summary line and the worst functions first.
func writeTextReport(w io.Writer, report complexity.Report, threshold int, production bool) {
	s := summarize(report)
	fmt.Fprintf(w, "production=%t files=%d functions=%d median=%d above20=%d above50=%d above100=%d\n", production, s.Files, s.Functions, s.Median, s.Above20, s.Above50, s.Above100)
	for _, fn := range worstFirst(report.Functions, threshold) {
		name := fn.Name
		if fn.Receiver != "" {
			name = "(" + fn.Receiver + ")." + name
		}
		fmt.Fprintf(w, "%4d %5d lines %s %s:%s\n", fn.Complexity, fn.Lines, fn.Module, fn.Path, name)
	}
}

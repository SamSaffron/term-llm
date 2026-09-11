package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/complexity"
)

type baseline struct {
	CountingRules string              `json:"counting_rules"`
	Summary       summary             `json:"summary"`
	Exceptions    []baselineException `json:"exceptions"`
}

type baselineException struct {
	Key        string `json:"key"`
	Complexity int    `json:"complexity"`
	Owner      string `json:"owner"`
	Rationale  string `json:"rationale"`
	Removal    string `json:"removal_milestone"`
	Origin     string `json:"origin"`
}

type summary struct {
	Files     int `json:"files"`
	Functions int `json:"functions"`
	Median    int `json:"median_complexity"`
	Above20   int `json:"above_20"`
	Above50   int `json:"above_50"`
	Above100  int `json:"above_100"`
}

func main() {
	includeTests := flag.Bool("tests", false, "include _test.go files")
	jsonOutput := flag.Bool("json", false, "write the complete report as JSON")
	writeBaseline := flag.String("write-baseline", "", "write ratchet baseline to this path")
	checkBaseline := flag.String("check", "", "check against a ratchet baseline")
	threshold := flag.Int("threshold", 20, "minimum complexity shown in text output")
	flag.Parse()

	report, err := complexity.Analyze(".", complexity.Options{IncludeTests: *includeTests})
	if err != nil {
		fatal(err)
	}
	if *writeBaseline != "" {
		if err := writeBase(*writeBaseline, report); err != nil {
			fatal(err)
		}
	}
	if *checkBaseline != "" {
		if err := check(*checkBaseline, report); err != nil {
			fatal(err)
		}
	}
	if *jsonOutput {
		filtered := report
		filtered.Functions = nil
		for _, fn := range report.Functions {
			if fn.Complexity > *threshold {
				filtered.Functions = append(filtered.Functions, fn)
			}
		}
		data, err := complexity.Encode(filtered)
		if err != nil {
			fatal(err)
		}
		_, _ = os.Stdout.Write(data)
		return
	}
	s := summarize(report)
	fmt.Printf("production=%t files=%d functions=%d median=%d above20=%d above50=%d above100=%d\n", !*includeTests, s.Files, s.Functions, s.Median, s.Above20, s.Above50, s.Above100)
	for _, fn := range report.Functions {
		if fn.Complexity >= *threshold {
			name := fn.Name
			if fn.Receiver != "" {
				name = "(" + fn.Receiver + ")." + name
			}
			fmt.Printf("%4d %5d lines %s %s:%s\n", fn.Complexity, fn.Lines, fn.Module, fn.Path, name)
		}
	}
}

func summarize(report complexity.Report) summary {
	values := make([]int, 0, len(report.Functions))
	s := summary{Files: len(report.Files), Functions: len(report.Functions)}
	for _, fn := range report.Functions {
		values = append(values, fn.Complexity)
		if fn.Complexity > 20 {
			s.Above20++
		}
		if fn.Complexity > 50 {
			s.Above50++
		}
		if fn.Complexity > 100 {
			s.Above100++
		}
	}
	sort.Ints(values)
	if len(values) > 0 {
		s.Median = values[len(values)/2]
	}
	return s
}

func writeBase(path string, report complexity.Report) error {
	initial, err := readBaseline("plans/go-complexity-baseline-initial.json")
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read initial baseline: %w", err)
	}
	initialByKey := make(map[string]baselineException, len(initial.Exceptions))
	initialBySignature := make(map[string]baselineException, len(initial.Exceptions))
	for _, exception := range initial.Exceptions {
		initialByKey[exception.Key] = exception
		initialBySignature[exceptionSignature(exception.Key)] = exception
	}

	b := baseline{CountingRules: report.CountingRules, Summary: summarize(report)}
	for _, fn := range report.Functions {
		if fn.Complexity <= 20 {
			continue
		}
		key := complexity.Key(fn)
		exception := baselineException{
			Key:        key,
			Complexity: fn.Complexity,
			Owner:      complexityOwner(fn.Path),
			Removal:    complexityMilestone(fn),
		}
		switch previous, ok := initialByKey[key]; {
		case ok:
			exception.Origin = "existing"
			exception.Rationale = "existing debt retained at its original declaration; complexity may only decrease"
		case initialBySignature[exceptionSignature(key)].Key != "":
			previous = initialBySignature[exceptionSignature(key)]
			exception.Origin = "moved"
			exception.Rationale = fmt.Sprintf("mechanically moved from %s while preserving its named responsibility", previous.Key)
		default:
			exception.Origin = "extracted"
			exception.Rationale = fmt.Sprintf("%s extraction owns one ordered domain responsibility; further reduction is tracked by %s", exception.Owner, exception.Removal)
		}
		b.Exceptions = append(b.Exceptions, exception)
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func readBaseline(path string) (baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return baseline{}, err
	}
	var b baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return baseline{}, err
	}
	return b, nil
}

func exceptionSignature(key string) string {
	parts := strings.Split(key, "|")
	if len(parts) < 5 {
		return key
	}
	return strings.Join(parts[len(parts)-3:], "|")
}

func complexityOwner(path string) string {
	for _, family := range []string{"internal/tui/chat", "internal/llm", "internal/serve", "internal/session", "internal/memory"} {
		if strings.HasPrefix(path, family+"/") {
			return strings.TrimPrefix(family, "internal/")
		}
	}
	if strings.HasPrefix(path, "cmd/ask") {
		return "cmd/ask"
	}
	if strings.HasPrefix(path, "cmd/serve") {
		return "cmd/serve"
	}
	dir := strings.TrimSuffix(path, "/"+pathBase(path))
	if dir == path {
		return "root"
	}
	return dir
}

func pathBase(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

func complexityMilestone(fn complexity.Function) string {
	switch {
	case strings.HasPrefix(fn.Path, "internal/tui/chat/"):
		return "M1"
	case strings.HasPrefix(fn.Path, "internal/llm/engine"):
		return "M2"
	case fn.Path == "cmd/serve_runtime.go" || strings.HasPrefix(fn.Path, "cmd/serve_run_"):
		return "M3"
	case strings.HasPrefix(fn.Path, "cmd/ask"):
		return "M4"
	case strings.HasPrefix(fn.Path, "cmd/serve_response") || strings.HasPrefix(fn.Path, "cmd/serve_session"):
		return "M5"
	case strings.HasPrefix(fn.Path, "internal/serve/telegram"):
		return "M6"
	default:
		return "M7"
	}
}

func check(path string, report complexity.Report) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var b baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return err
	}
	allowed := map[string]baselineException{}
	for _, exception := range b.Exceptions {
		if exception.Owner == "" || exception.Rationale == "" || exception.Removal == "" || exception.Origin == "" {
			return fmt.Errorf("baseline exception %q lacks ownership or origin metadata", exception.Key)
		}
		allowed[exception.Key] = exception
	}
	var violations []string
	for _, fn := range report.Functions {
		if fn.Complexity <= 20 {
			continue
		}
		exception, ok := allowed[complexity.Key(fn)]
		if !ok {
			violations = append(violations, fmt.Sprintf("new function above 20: %s (%d)", complexity.Key(fn), fn.Complexity))
			continue
		}
		if fn.Complexity > exception.Complexity {
			violations = append(violations, fmt.Sprintf("complexity increased: %s %d -> %d", exception.Key, exception.Complexity, fn.Complexity))
		}
	}
	if len(violations) != 0 {
		sort.Strings(violations)
		return fmt.Errorf("complexity ratchet failed:\n  %s", strings.Join(violations, "\n  "))
	}
	return nil
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "complexity:", err); os.Exit(1) }

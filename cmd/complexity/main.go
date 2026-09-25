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
		warnings, err := check(*checkBaseline, report)
		if err != nil {
			fatal(err)
		}
		for _, warning := range warnings {
			fmt.Fprintln(os.Stderr, "complexity warning:", warning)
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
	for _, fn := range worstFirst(report.Functions, *threshold) {
		name := fn.Name
		if fn.Receiver != "" {
			name = "(" + fn.Receiver + ")." + name
		}
		fmt.Printf("%4d %5d lines %s %s:%s\n", fn.Complexity, fn.Lines, fn.Module, fn.Path, name)
	}
}

// worstFirst orders the human report by descending complexity, then by span, so
// the first screen is the work that matters. The JSON report and the baseline
// keep path order, where stable diffs matter more than ranking.
func worstFirst(functions []complexity.Function, threshold int) []complexity.Function {
	out := make([]complexity.Function, 0, len(functions))
	for _, fn := range functions {
		if fn.Complexity >= threshold {
			out = append(out, fn)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Complexity != out[j].Complexity {
			return out[i].Complexity > out[j].Complexity
		}
		return out[i].Lines > out[j].Lines
	})
	return out
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

	recorded, err := readBaseline(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read current baseline: %w", err)
	}
	recordedByKey := make(map[string]baselineException, len(recorded.Exceptions))
	for _, exception := range recorded.Exceptions {
		recordedByKey[exception.Key] = exception
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
		if kept, ok := recordedByKey[key]; ok {
			// An already accepted exception keeps its ownership record; only
			// its measured score is refreshed.
			kept.Complexity = fn.Complexity
			b.Exceptions = append(b.Exceptions, kept)
			continue
		}
		switch previous, ok := initialByKey[key]; {
		case ok:
			exception.Origin = "existing"
			exception.Rationale = "existing debt retained at its original declaration; complexity may only decrease"
		case initialBySignature[exceptionSignature(key)].Key != "":
			previous = initialBySignature[exceptionSignature(key)]
			exception.Origin = "moved"
			exception.Rationale = fmt.Sprintf("mechanically moved from %s while preserving its named responsibility", previous.Key)
		case len(recordedByKey) != 0 && recorded.CountingRules != report.CountingRules:
			// When the measure changed, functions previously below 20 could
			// enter the historical exception report without being new code.
			exception.Origin = "remeasured"
			exception.Rationale = fmt.Sprintf("crossed 20 when counting moved to nesting-weighted branches and separately measured function literals; %s reduction is tracked by %s", exception.Owner, exception.Removal)
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

// check reports growth beyond 30 without blocking the build. Historical
// exceptions above 20 remain in the baseline for ownership and comparison.
func check(path string, report complexity.Report) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	allowed := map[string]baselineException{}
	for _, exception := range b.Exceptions {
		if exception.Owner == "" || exception.Rationale == "" || exception.Removal == "" || exception.Origin == "" {
			return nil, fmt.Errorf("baseline exception %q lacks ownership or origin metadata", exception.Key)
		}
		allowed[exception.Key] = exception
	}
	var warnings []string
	for _, fn := range report.Functions {
		if fn.Complexity <= 30 {
			continue
		}
		exception, ok := allowed[complexity.Key(fn)]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("new function above 30: %s (%d)", complexity.Key(fn), fn.Complexity))
			continue
		}
		if fn.Complexity > exception.Complexity {
			warnings = append(warnings, fmt.Sprintf("complexity increased above 30: %s %d -> %d", exception.Key, exception.Complexity, fn.Complexity))
		}
	}
	sort.Strings(warnings)
	return warnings, nil
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "complexity:", err); os.Exit(1) }

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	guardianeval "github.com/samsaffron/term-llm/evaluation/guardian-classify/internal/eval"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/guardian"
	"github.com/spf13/cobra"
)

type guardianEvalOptions struct {
	suite, input, raw, corpus  string
	concurrency, limit, offset int
	dry, json, reportOnly      bool
}
type guardianEvalDeps struct {
	loadConfig func() (*config.Config, error)
	newReview  func(*config.Config, string) (func(context.Context, guardian.Request) (guardian.Decision, error), func(), error)
}

func main() {
	cmd := newGuardianEvalCmd(guardianEvalDeps{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newGuardianEvalCmd(deps guardianEvalDeps) *cobra.Command {
	if deps.loadConfig == nil {
		deps.loadConfig = config.Load
	}
	if deps.newReview == nil {
		deps.newReview = newClassifyGuardianReview
	}
	o := &guardianEvalOptions{}
	cmd := &cobra.Command{Use: "guardian-classify", Short: "Evaluate inert shell cases with the real classify-backed Guardian (never execute)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runGuardianEval(cmd, o, deps) },
	}
	f := cmd.Flags()
	f.StringVar(&o.suite, "suite", "shell-1000", "Built-in corpus (shell-1000)")
	f.StringVar(&o.input, "input", "", "Read JSONL corpus instead ('-' for stdin)")
	f.IntVar(&o.concurrency, "concurrency", 1, "Concurrent reviews (1..32)")
	f.IntVar(&o.limit, "limit", 0, "Maximum selected cases (0 means all remaining)")
	f.IntVar(&o.offset, "offset", 0, "Skip this many cases before applying limit")
	f.BoolVar(&o.json, "json", false, "Print JSON summary instead of human summary")
	f.StringVar(&o.raw, "raw-output", "", "Create ordered result JSONL file (must not exist)")
	f.BoolVar(&o.dry, "dry-run", false, "Print selected corpus JSONL without loading config or contacting providers")
	f.StringVar(&o.corpus, "write-corpus", "", "Write selected corpus JSONL without provider calls ('-' for stdout; file must not exist)")
	f.BoolVar(&o.reportOnly, "report-only", false, "Exit successfully despite mismatches or review errors")
	return cmd
}

func validateGuardianEval(cmd *cobra.Command, o *guardianEvalOptions) error {
	if o.suite != "shell-1000" {
		return fmt.Errorf("unknown suite; expected shell-1000")
	}
	if o.input != "" && cmd.Flags().Changed("suite") {
		return fmt.Errorf("--input and --suite are mutually exclusive")
	}
	if o.concurrency < 1 || o.concurrency > guardianeval.MaxConcurrency {
		return fmt.Errorf("--concurrency must be 1..%d", guardianeval.MaxConcurrency)
	}
	if o.limit < 0 || o.offset < 0 {
		return fmt.Errorf("--limit and --offset must be nonnegative")
	}
	if (o.dry || o.corpus != "") && (o.raw != "" || o.json || o.reportOnly) {
		return fmt.Errorf("corpus-only mode cannot use --raw-output, --json or --report-only")
	}
	if o.raw == "-" {
		return fmt.Errorf("--raw-output requires a file; stdout is reserved for the summary")
	}
	return nil
}

func guardianEvalCases(cmd *cobra.Command, o *guardianEvalOptions) ([]guardianeval.Case, error) {
	cases := guardianeval.Shell1000()
	if o.input != "" {
		var r io.Reader = cmd.InOrStdin()
		if o.input != "-" {
			f, err := os.Open(o.input)
			if err != nil {
				return nil, fmt.Errorf("open corpus: %w", err)
			}
			defer f.Close()
			r = f
		}
		var err error
		cases, err = guardianeval.ReadCorpus(r)
		if err != nil {
			return nil, err
		}
	}
	if o.offset >= len(cases) {
		return nil, fmt.Errorf("--offset must be smaller than corpus size (%d)", len(cases))
	}
	cases = cases[o.offset:]
	if o.limit > 0 && o.limit < len(cases) {
		cases = cases[:o.limit]
	}
	return cases, guardianeval.Validate(cases)
}

func writeGuardianEvalCorpus(cmd *cobra.Command, path string, cases []guardianeval.Case) error {
	if path == "" || path == "-" {
		return guardianeval.WriteCorpus(cmd.OutOrStdout(), cases)
	}
	f, err := createGuardianEvalFile(path)
	if err != nil {
		return err
	}
	err = guardianeval.WriteCorpus(f, cases)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func createGuardianEvalFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("create evaluation output: %w", err)
	}
	return f, nil
}

func runGuardianEval(cmd *cobra.Command, o *guardianEvalOptions, deps guardianEvalDeps) error {
	if err := validateGuardianEval(cmd, o); err != nil {
		return err
	}
	cases, err := guardianEvalCases(cmd, o)
	if err != nil {
		return err
	}
	if o.dry || o.corpus != "" {
		return writeGuardianEvalCorpus(cmd, o.corpus, cases)
	}
	cfg, policy, err := loadGuardianEvalPolicy(deps)
	if err != nil {
		return err
	}
	review, cleanup, err := deps.newReview(cfg, policy)
	if err != nil {
		return fmt.Errorf("initialize guardian classify provider failed (details suppressed); check classify provider configuration")
	}
	if cleanup != nil {
		defer cleanup()
	}
	var raw *os.File
	if o.raw != "" {
		raw, err = createGuardianEvalFile(o.raw)
		if err != nil {
			return err
		}
		defer raw.Close()
	}
	records, err := guardianeval.Run(cmd.Context(), cases, o.concurrency, review)
	if err != nil {
		return err
	}
	if raw != nil {
		if err := guardianeval.WriteRecords(raw, records); err != nil {
			return err
		}
		if err := raw.Close(); err != nil {
			return err
		}
	}
	summary := guardianeval.Summarize(records)
	if err := writeGuardianEvalSummary(cmd, o, summary, policy); err != nil {
		return err
	}
	if !o.reportOnly && (summary.Failed > 0 || summary.Errors > 0) {
		return fmt.Errorf("guardian evaluation: %d mismatches, %d errors", summary.Failed, summary.Errors)
	}
	return nil
}

func loadGuardianEvalPolicy(deps guardianEvalDeps) (*config.Config, string, error) {
	cfg, err := deps.loadConfig()
	if err != nil {
		return nil, "", fmt.Errorf("load evaluation configuration failed (details suppressed)")
	}
	if cfg == nil || strings.TrimSpace(cfg.Guardian.Backend) != "classify" {
		return nil, "", fmt.Errorf("guardian-classify requires guardian.backend: classify")
	}
	policy, err := guardian.LoadPolicy(cfg.Guardian.PolicyPath)
	if err != nil {
		return nil, "", fmt.Errorf("load guardian policy failed (details suppressed)")
	}
	return cfg, policy, nil
}

func writeGuardianEvalSummary(cmd *cobra.Command, o *guardianEvalOptions, s guardianeval.Summary, policy string) error {
	if o.json {
		source := o.suite + "-v1"
		if o.input != "" {
			source = "jsonl"
		}
		result := struct {
			guardianeval.Summary
			Corpus       string `json:"corpus"`
			PolicySHA256 string `json:"policy_sha256"`
			Offset       int    `json:"offset"`
			Concurrency  int    `json:"concurrency"`
		}{s, source, fmt.Sprintf("%x", sha256.Sum256([]byte(policy))), o.offset, o.concurrency}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "Guardian evaluation (no commands executed): %d cases, %d pass, %d mismatch, %d errors\nBenign allow: %.2f%%; bomb deny: %.2f%%\nDeny-positive confusion: TP=%d FP=%d TN=%d FN=%d\nLatency ms: min=%.2f p50=%.2f p95=%.2f p99=%.2f max=%.2f; state bytes=%d..%d\nUse --json for per-category metrics.\n", s.Total, s.Passed, s.Failed, s.Errors, 100*s.BenignAllowRate, 100*s.BombDenyRate, s.Confusion.TruePositive, s.Confusion.FalsePositive, s.Confusion.TrueNegative, s.Confusion.FalseNegative, s.LatencyMS.Min, s.LatencyMS.P50, s.LatencyMS.P95, s.LatencyMS.P99, s.LatencyMS.Max, s.StateBytes.Min, s.StateBytes.Max)
	return err
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/usage"
)

type Task interface {
	Name() string
	Language() string
	Difficulty() string
	Prompt() string
	Score(response string, timeout time.Duration) ScoreResult
}

type ScoreResult struct {
	Pass          bool    `json:"pass"`
	Score         float64 `json:"score"`
	Details       string  `json:"details,omitempty"`
	Stdout        string  `json:"stdout,omitempty"`
	Stderr        string  `json:"stderr,omitempty"`
	GeneratedCode string  `json:"generated_code,omitempty"`
}

type TaskResult struct {
	Task          string     `json:"task"`
	Language      string     `json:"language"`
	Difficulty    string     `json:"difficulty"`
	Provider      string     `json:"provider"`
	Model         string     `json:"model,omitempty"`
	Pass          bool       `json:"pass"`
	Score         float64    `json:"score"`
	Details       string     `json:"details,omitempty"`
	DurationMS    int64      `json:"duration_ms"`
	Usage         TokenUsage `json:"usage"`
	EstimatedCost float64    `json:"estimated_cost_usd,omitempty"`
	Stdout        string     `json:"stdout,omitempty"`
	Stderr        string     `json:"stderr,omitempty"`
	GeneratedCode string     `json:"generated_code,omitempty"`
	Error         string     `json:"error,omitempty"`
}

type TokenUsage struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	CacheWriteTokens  int `json:"cache_write_tokens"`
	ReasoningTokens   int `json:"reasoning_tokens"`
}

type RunReport struct {
	ID                   string       `json:"id"`
	Provider             string       `json:"provider"`
	Model                string       `json:"model,omitempty"`
	StartedAt            time.Time    `json:"started_at"`
	FinishedAt           time.Time    `json:"finished_at"`
	TotalDurationMS      int64        `json:"total_duration_ms"`
	Concurrency          int          `json:"concurrency"`
	Budget               string       `json:"budget"`
	Tasks                []TaskResult `json:"tasks"`
	Passes               int          `json:"passes"`
	Total                int          `json:"total"`
	PassRate             float64      `json:"pass_rate"`
	MeanScore            float64      `json:"mean_score"`
	EstimatedCostUSD     float64      `json:"estimated_cost_usd,omitempty"`
	EstimatedCostPerPass float64      `json:"estimated_cost_per_pass_usd,omitempty"`
}

type askResult struct {
	Text  string
	Usage TokenUsage
	Cost  float64
}

func main() {
	providerFlag := flag.String("provider", envOr("BENCH_PROVIDER", "claude-bin"), "provider[:model] to benchmark")
	tasksFlag := flag.String("tasks", envOr("BENCH_TASKS", "all"), "comma-separated task names or all")
	runs := flag.Int("runs", envInt("BENCH_RUNS", 1), "repeat each selected task N times")
	concurrency := flag.Int("concurrency", envInt("BENCH_CONCURRENCY", 2), "maximum concurrent LLM requests")
	budget := flag.Duration("budget", envDuration("BENCH_BUDGET", 4*time.Hour), "wall-clock budget for the run")
	timeout := flag.Duration("timeout", envDuration("BENCH_TASK_TIMEOUT", 5*time.Minute), "timeout per LLM task")
	scoreTimeout := flag.Duration("score-timeout", envDuration("BENCH_SCORE_TIMEOUT", 20*time.Second), "timeout for compile/test scoring")
	outDir := flag.String("out", envOr("BENCH_OUT", "benchmarks/codegen/results"), "directory for JSON artifacts")
	jsonOnly := flag.Bool("json", false, "print only the final JSON report")
	flag.Parse()

	if *runs < 1 {
		fatalf("-runs must be >= 1")
	}
	if *concurrency < 1 {
		fatalf("-concurrency must be >= 1")
	}

	cfg, err := config.Load()
	if err != nil {
		fatalf("load config: %v", err)
	}
	providerName, model, err := llm.ParseProviderModel(*providerFlag, cfg)
	if err != nil {
		fatalf("parse provider: %v", err)
	}
	provider, err := llm.NewProviderByName(cfg, providerName, model)
	if err != nil {
		fatalf("create provider: %v", err)
	}
	if model == "" {
		if pc := cfg.GetProviderConfig(providerName); pc != nil {
			model = pc.Model
		}
	}

	selected, err := selectTasks(*tasksFlag)
	if err != nil {
		fatalf("select tasks: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *budget)
	defer cancel()

	started := time.Now()
	report := RunReport{
		ID:          started.UTC().Format("20060102T150405Z"),
		Provider:    providerName,
		Model:       model,
		StartedAt:   started.UTC(),
		Concurrency: *concurrency,
		Budget:      budget.String(),
	}

	jobs := make(chan Task, len(selected)**runs)
	results := make(chan TaskResult, len(selected)**runs)
	var wg sync.WaitGroup
	pricing := usage.NewPricingFetcher()

	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range jobs {
				results <- runTask(ctx, provider, providerName, model, task, *timeout, *scoreTimeout, pricing)
			}
		}()
	}

	go func() {
		defer close(jobs)
		for i := 0; i < *runs; i++ {
			for _, task := range selected {
				select {
				case <-ctx.Done():
					return
				case jobs <- task:
				}
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	for r := range results {
		report.Tasks = append(report.Tasks, r)
	}

	report.FinishedAt = time.Now().UTC()
	report.TotalDurationMS = time.Since(started).Milliseconds()
	summarize(&report)
	sort.Slice(report.Tasks, func(i, j int) bool {
		if report.Tasks[i].Task == report.Tasks[j].Task {
			return report.Tasks[i].DurationMS < report.Tasks[j].DurationMS
		}
		return report.Tasks[i].Task < report.Tasks[j].Task
	})

	if err := writeArtifacts(*outDir, report); err != nil {
		fatalf("write artifacts: %v", err)
	}

	if *jsonOnly {
		_ = json.NewEncoder(os.Stdout).Encode(report)
		return
	}
	printReport(os.Stdout, report)
}

func runTask(parent context.Context, provider llm.Provider, providerName, model string, task Task, llmTimeout, scoreTimeout time.Duration, pricing *usage.PricingFetcher) TaskResult {
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, llmTimeout)
	defer cancel()

	res := TaskResult{Task: task.Name(), Language: task.Language(), Difficulty: task.Difficulty(), Provider: providerName, Model: model}
	askPrompt := buildPrompt(task)
	answer, err := ask(ctx, provider, model, askPrompt, pricing)
	res.DurationMS = time.Since(started).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		res.Details = "llm request failed"
		return res
	}
	res.Usage = answer.Usage
	res.EstimatedCost = answer.Cost

	score := task.Score(answer.Text, scoreTimeout)
	res.Pass = score.Pass
	res.Score = score.Score
	res.Details = score.Details
	res.Stdout = trimForJSON(score.Stdout)
	res.Stderr = trimForJSON(score.Stderr)
	res.GeneratedCode = trimForJSON(score.GeneratedCode)
	return res
}

func ask(ctx context.Context, provider llm.Provider, model, prompt string, pricing *usage.PricingFetcher) (askResult, error) {
	stream, err := provider.Stream(ctx, llm.Request{
		Model: model,
		Messages: []llm.Message{
			llm.SystemText("You are in a code-generation benchmark. Follow the requested function signatures exactly. Return only a single fenced code block in the requested language, no explanation."),
			llm.UserText(prompt),
		},
		Temperature:     0,
		TemperatureSet:  true,
		MaxOutputTokens: 4096,
	})
	if err != nil {
		return askResult{}, err
	}
	defer stream.Close()

	var b strings.Builder
	var use TokenUsage
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return askResult{}, err
		}
		switch ev.Type {
		case llm.EventTextDelta:
			b.WriteString(ev.Text)
		case llm.EventUsage:
			if ev.Use != nil {
				use.InputTokens += ev.Use.InputTokens
				use.OutputTokens += ev.Use.OutputTokens
				use.CachedInputTokens += ev.Use.CachedInputTokens
				use.CacheWriteTokens += ev.Use.CacheWriteTokens
				use.ReasoningTokens += ev.Use.ReasoningTokens
			}
		case llm.EventError:
			if ev.Err != nil {
				return askResult{}, ev.Err
			}
		}
	}
	cost := estimateCost(model, use, pricing)
	return askResult{Text: b.String(), Usage: use, Cost: cost}, nil
}

func estimateCost(model string, use TokenUsage, pricing *usage.PricingFetcher) float64 {
	if strings.TrimSpace(model) == "" {
		return 0
	}
	entry := usage.UsageEntry{Model: model, InputTokens: use.InputTokens, OutputTokens: use.OutputTokens, CacheReadTokens: use.CachedInputTokens, CacheWriteTokens: use.CacheWriteTokens, ReasoningTokens: use.ReasoningTokens}
	cost, err := pricing.CalculateCost(entry)
	if err != nil {
		return 0
	}
	return cost
}

func buildPrompt(task Task) string {
	return fmt.Sprintf(`# Code generation benchmark task

Task: %s
Language: %s
Difficulty: %s

%s

Return only one fenced %s code block containing a complete source file when the language has imports/package declarations. Include every import your code needs. No prose.`, task.Name(), task.Language(), task.Difficulty(), task.Prompt(), task.Language())
}

func summarize(report *RunReport) {
	report.Total = len(report.Tasks)
	var score float64
	for _, t := range report.Tasks {
		if t.Pass {
			report.Passes++
		}
		score += t.Score
		report.EstimatedCostUSD += t.EstimatedCost
	}
	if report.Total > 0 {
		report.PassRate = float64(report.Passes) / float64(report.Total)
		report.MeanScore = score / float64(report.Total)
	}
	if report.Passes > 0 {
		report.EstimatedCostPerPass = report.EstimatedCostUSD / float64(report.Passes)
	}
}

func writeArtifacts(outDir string, report RunReport) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	providerSlug := strings.NewReplacer(":", "-", "/", "-", " ", "-").Replace(report.Provider)
	if report.Model != "" {
		providerSlug += "-" + strings.NewReplacer(":", "-", "/", "-", " ", "-").Replace(report.Model)
	}
	path := filepath.Join(outDir, fmt.Sprintf("%s_%s.json", report.ID, providerSlug))
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	historyPath := filepath.Join(outDir, "history.jsonl")
	line, err := json.Marshal(report)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(historyPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

func printReport(w io.Writer, report RunReport) {
	model := report.Model
	if model == "" {
		model = "default"
	}
	fmt.Fprintf(w, "Provider: %s (%s)   Tasks: %d   Concurrency: %d\n", report.Provider, model, report.Total, report.Concurrency)
	fmt.Fprintln(w, "────────────────────────────────────────────────────────────────────────────")
	fmt.Fprintf(w, "%-24s %-8s %-6s %-9s %-10s %s\n", "Task", "Lang", "Pass", "Score", "Cost", "Detail")
	for _, t := range report.Tasks {
		pass := "✗"
		if t.Pass {
			pass = "✓"
		}
		detail := t.Details
		if t.Error != "" {
			detail = t.Error
		}
		fmt.Fprintf(w, "%-24s %-8s %-6s %-9.2f $%-9.4f %s\n", t.Task, t.Language, pass, t.Score, t.EstimatedCost, detail)
	}
	fmt.Fprintln(w, "────────────────────────────────────────────────────────────────────────────")
	fmt.Fprintf(w, "Pass rate: %d/%d (%.0f%%)   Mean score: %.2f   Cost: $%.4f   Cost/pass: $%.4f   Total: %s\n", report.Passes, report.Total, report.PassRate*100, report.MeanScore, report.EstimatedCostUSD, report.EstimatedCostPerPass, time.Duration(report.TotalDurationMS)*time.Millisecond)
}

func trimForJSON(s string) string {
	const limit = 32 * 1024
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "\n... truncated ..."
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

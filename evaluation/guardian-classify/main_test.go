package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	guardianeval "github.com/samsaffron/term-llm/evaluation/guardian-classify/internal/eval"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/guardian"
	"github.com/samsaffron/term-llm/internal/typesafe"
)

func executeGuardianEval(deps guardianEvalDeps, input string, args ...string) (string, error) {
	cmd := newGuardianEvalCmd(deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(input))
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestGuardianEvalDryRunAndValidation(t *testing.T) {
	deps := guardianEvalDeps{loadConfig: func() (*config.Config, error) { t.Fatal("dry-run loaded config"); return nil, nil }}
	out, err := executeGuardianEval(deps, "", "--dry-run", "--offset", "700", "--limit", "3")
	if err != nil {
		t.Fatal(err)
	}
	cases, err := guardianeval.ReadCorpus(strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 3 || cases[0].Expected != "deny" || cases[0].ID != guardianeval.Shell1000()[700].ID {
		t.Fatal("incorrect chunk")
	}
	path := filepath.Join(t.TempDir(), "corpus.jsonl")
	if _, err := executeGuardianEval(deps, "", "--write-corpus", path, "--limit", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := executeGuardianEval(deps, "", "--write-corpus", path); err == nil {
		t.Fatal("overwrote corpus")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	echoed, err := executeGuardianEval(deps, string(data), "--input", "-", "--dry-run")
	if err != nil || echoed != string(data) {
		t.Fatalf("input roundtrip: %v", err)
	}
	for _, args := range [][]string{
		{"--concurrency", "0"}, {"--concurrency", "33"}, {"--limit", "-1"}, {"--offset", "-1"}, {"--offset", "1000"}, {"--suite", "bad"}, {"--input", "-", "--suite", "shell-1000"}, {"--dry-run", "--json"}, {"--dry-run", "--raw-output", "x"}, {"--write-corpus", "-", "--report-only"}, {"--raw-output", "-"}, {"extra"},
	} {
		if _, err := executeGuardianEval(deps, "", args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

type evalClassifyStub struct {
	calls  atomic.Int32
	policy string
}

func (s *evalClassifyStub) ListModels(context.Context) (*typesafe.ModelsResponse, error) {
	return nil, errors.New("unexpected models call")
}
func (s *evalClassifyStub) Classify(_ context.Context, r typesafe.Request) (*typesafe.Response, error) {
	s.calls.Add(1)
	var state struct {
		Policy string `json:"policy"`
	}
	if err := json.Unmarshal(r.State, &state); err != nil {
		return nil, err
	}
	if state.Policy != s.policy {
		return nil, errors.New("wrong policy")
	}
	answers := map[string]typesafe.Answer{}
	for id, value := range map[string]string{"risk_level": "low", "user_authorization": "explicit", "outcome": "allow"} {
		v, c := value, .9
		answers[id] = typesafe.Answer{Type: "choice", Choice: &v, Confidence: &c}
	}
	return &typesafe.Response{Model: r.Model, Answers: answers}, nil
}

func TestGuardianEvalProductionWiringAndRaw(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "policy.md")
	if err := os.WriteFile(policy, []byte("fixture policy"), 0600); err != nil {
		t.Fatal(err)
	}
	stub := &evalClassifyStub{policy: "fixture policy"}
	original := newGuardianClassifyClient
	t.Cleanup(func() { newGuardianClassifyClient = original })
	constructed := 0
	newGuardianClassifyClient = func(o typesafe.Options) (classifyClient, error) {
		constructed++
		if stub.calls.Load() != 0 || o.APIKey != "fixture-key" {
			t.Fatal("wrong eager resolution")
		}
		return stub, nil
	}
	cfg := &config.Config{Guardian: config.GuardianConfig{Backend: "classify", PolicyPath: policy, Classify: config.GuardianClassifyConfig{Provider: "selected", MinConfidence: .95}}, Classify: config.ClassifyConfig{DefaultProvider: "wrong", Providers: map[string]config.ClassifyProviderConfig{"selected": {Type: "typesafe", Model: "fixture-model", APIKey: "fixture-key", BaseURL: "https://fixture.invalid"}}}}
	deps := guardianEvalDeps{loadConfig: func() (*config.Config, error) { return cfg, nil }}
	raw := filepath.Join(dir, "raw.jsonl")
	out, err := executeGuardianEval(deps, "", "--limit", "4", "--concurrency", "2", "--json", "--raw-output", raw)
	if err == nil {
		t.Fatal("confidence denials must fail benign evaluation")
	}
	if constructed != 1 || stub.calls.Load() != 4 {
		t.Fatalf("constructed=%d calls=%d", constructed, stub.calls.Load())
	}
	var summary guardianeval.Summary
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Failed != 4 || summary.Confusion.FalsePositive != 4 || summary.Errors != 0 {
		t.Fatalf("summary: %+v", summary)
	}
	data, err := os.ReadFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out+string(data), "fixture-key") {
		t.Fatal("credential leaked")
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 {
		t.Fatal("wrong raw count")
	}
	for i, line := range lines {
		var r guardianeval.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.ID != guardianeval.Shell1000()[i].ID || r.Actual != "deny" || r.StateBytes == 0 || !strings.Contains(r.Rationale, "confidence") {
			t.Fatalf("record: %+v", r)
		}
	}
	info, err := os.Stat(raw)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("raw permissions=%v", info.Mode())
	}
}

func TestGuardianEvalSuccessfulSuiteAndOutputFailure(t *testing.T) {
	cfg := &config.Config{Guardian: config.GuardianConfig{Backend: "classify"}}
	expected := map[string]string{}
	for _, c := range guardianeval.Shell1000() {
		expected[c.Command] = c.Expected
	}
	var calls atomic.Int32
	deps := guardianEvalDeps{loadConfig: func() (*config.Config, error) { return cfg, nil }, newReview: func(*config.Config, string) (func(context.Context, guardian.Request) (guardian.Decision, error), func(), error) {
		return func(_ context.Context, req guardian.Request) (guardian.Decision, error) {
			calls.Add(1)
			return guardian.Decision{Outcome: expected[req.Command]}, nil
		}, nil, nil
	}}
	out, err := executeGuardianEval(deps, "", "--json", "--concurrency", "8")
	if err != nil {
		t.Fatal(err)
	}
	var s guardianeval.Summary
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatal(err)
	}
	if s.Total != 1000 || s.Passed != 1000 || s.BenignAllowRate != 1 || s.BombDenyRate != 1 || calls.Load() != 1000 {
		t.Fatalf("full stub run: %+v", s)
	}
	path := filepath.Join(t.TempDir(), "existing")
	if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := executeGuardianEval(deps, "", "--raw-output", path, "--report-only"); err == nil {
		t.Fatal("output failure suppressed")
	}
	if calls.Load() != 1000 {
		t.Fatal("provider called before output validation")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "preserve" {
		t.Fatal("existing output changed")
	}
	cfg.Guardian.PolicyPath = filepath.Join(t.TempDir(), "missing-policy")
	if _, err := executeGuardianEval(deps, "", "--limit", "1"); err == nil {
		t.Fatal("missing policy accepted")
	}
}

func TestGuardianEvalErrorsReportOnlyAndSetup(t *testing.T) {
	cfg := &config.Config{Guardian: config.GuardianConfig{Backend: "classify"}}
	cleanup := 0
	deps := guardianEvalDeps{loadConfig: func() (*config.Config, error) { return cfg, nil }, newReview: func(*config.Config, string) (func(context.Context, guardian.Request) (guardian.Decision, error), func(), error) {
		return func(context.Context, guardian.Request) (guardian.Decision, error) {
			return guardian.Decision{}, errors.New("SECRET-provider-error")
		}, func() { cleanup++ }, nil
	}}
	out, err := executeGuardianEval(deps, "", "--limit", "2", "--json", "--report-only")
	if err != nil || strings.Contains(out, "SECRET") || cleanup != 1 {
		t.Fatalf("report only: %v %s cleanup=%d", err, out, cleanup)
	}
	var s guardianeval.Summary
	_ = json.Unmarshal([]byte(out), &s)
	if s.Errors != 2 {
		t.Fatal("errors missing")
	}
	if _, err := executeGuardianEval(deps, "", "--limit", "1"); err == nil {
		t.Fatal("error exit missing")
	}
	for _, backend := range []string{"", "llm", "other"} {
		cfg.Guardian.Backend = backend
		if _, err := executeGuardianEval(deps, "", "--limit", "1"); err == nil {
			t.Fatal("accepted backend")
		}
	}
	cfg.Guardian.Backend = "classify"
	deps.newReview = func(*config.Config, string) (func(context.Context, guardian.Request) (guardian.Decision, error), func(), error) {
		return nil, nil, errors.New("SECRET-key")
	}
	if _, err := executeGuardianEval(deps, "", "--limit", "1"); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("setup error: %v", err)
	}
	deps.loadConfig = func() (*config.Config, error) { return nil, errors.New("SECRET-config") }
	if _, err := executeGuardianEval(deps, "", "--limit", "1"); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("config error: %v", err)
	}
}

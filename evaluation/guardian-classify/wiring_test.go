package main

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/guardian"
	"github.com/samsaffron/term-llm/internal/typesafe"
)

type deadlineClient struct {
	t       *testing.T
	model   string
	timeout time.Duration
	called  bool
}

func (c *deadlineClient) Classify(ctx context.Context, req typesafe.Request) (*typesafe.Response, error) {
	c.called = true
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > c.timeout {
		c.t.Fatal("missing or incorrect guardian deadline")
	}
	if req.Model != c.model {
		c.t.Fatalf("model = %q, want %q", req.Model, c.model)
	}
	return (&evalClassifyStub{policy: "fixture policy"}).Classify(ctx, req)
}

func TestStandaloneProductionConfigSemantics(t *testing.T) {
	t.Setenv("GUARDIAN_EVAL_TEST_KEY", "fixture-key")
	t.Setenv("GUARDIAN_EVAL_TEST_URL", "https://fixture.invalid")
	original := newGuardianClassifyClient
	t.Cleanup(func() { newGuardianClassifyClient = original })
	for _, explicit := range []bool{false, true} {
		for _, timeout := range []int{0, 2} {
			cfg := &config.Config{
				Guardian: config.GuardianConfig{TimeoutSeconds: timeout, Classify: config.GuardianClassifyConfig{MinConfidence: .8}},
				Classify: config.ClassifyConfig{DefaultProvider: "selected", Providers: map[string]config.ClassifyProviderConfig{
					"selected": {Type: "typesafe", Model: "fixture-model", APIKey: "${GUARDIAN_EVAL_TEST_KEY}", BaseURL: "$GUARDIAN_EVAL_TEST_URL", TimeoutSeconds: 3},
				}},
			}
			if explicit {
				cfg.Guardian.Classify.Provider = "selected"
				cfg.Classify.DefaultProvider = "missing"
			}
			wantTimeout := guardian.DefaultTimeout
			if timeout > 0 {
				wantTimeout = time.Duration(timeout) * time.Second
			}
			stub := &deadlineClient{t: t, model: "fixture-model", timeout: wantTimeout}
			newGuardianClassifyClient = func(o typesafe.Options) (classifyClient, error) {
				if o.APIKey != "fixture-key" || o.BaseURL != "https://fixture.invalid" || o.Timeout != 3*time.Second {
					t.Fatal("provider references or transport timeout not resolved")
				}
				return stub, nil
			}
			review, _, err := newClassifyGuardianReview(cfg, "fixture policy")
			if err != nil {
				t.Fatal(err)
			}
			if stub.called {
				t.Fatal("setup made a classification call")
			}
			decision, err := review(context.Background(), guardian.Request{Command: "inert fixture"})
			if err != nil || decision.Outcome != "allow" {
				t.Fatalf("review: %+v, %v", decision, err)
			}
		}
	}
}

func TestStandaloneRejectsInvalidConfigBeforeClient(t *testing.T) {
	original := newGuardianClassifyClient
	t.Cleanup(func() { newGuardianClassifyClient = original })
	newGuardianClassifyClient = func(typesafe.Options) (classifyClient, error) {
		t.Fatal("invalid config constructed client")
		return nil, nil
	}
	cases := []struct {
		name   string
		change func(*config.Config)
	}{
		{"negative confidence", func(c *config.Config) { c.Guardian.Classify.MinConfidence = -.1 }},
		{"excess confidence", func(c *config.Config) { c.Guardian.Classify.MinConfidence = 1.1 }},
		{"NaN confidence", func(c *config.Config) { c.Guardian.Classify.MinConfidence = math.NaN() }},
		{"infinite confidence", func(c *config.Config) { c.Guardian.Classify.MinConfidence = math.Inf(1) }},
		{"missing provider", func(c *config.Config) { c.Guardian.Classify.Provider = "missing" }},
		{"negative transport timeout", func(c *config.Config) {
			c.Classify.Providers["selected"] = config.ClassifyProviderConfig{Type: "typesafe", TimeoutSeconds: -1}
		}},
		{"missing key reference", func(c *config.Config) {
			c.Classify.Providers["selected"] = config.ClassifyProviderConfig{Type: "typesafe", APIKey: "file://" + t.TempDir() + "/missing"}
		}},
		{"missing URL reference", func(c *config.Config) {
			c.Classify.Providers["selected"] = config.ClassifyProviderConfig{Type: "typesafe", BaseURL: "file://" + t.TempDir() + "/missing"}
		}},
	}
	if strconv.IntSize == 64 {
		cases = append(cases, struct {
			name   string
			change func(*config.Config)
		}{"overflow transport timeout", func(c *config.Config) {
			c.Classify.Providers["selected"] = config.ClassifyProviderConfig{Type: "typesafe", TimeoutSeconds: int(^uint(0) >> 1)}
		}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Classify: config.ClassifyConfig{DefaultProvider: "selected", Providers: map[string]config.ClassifyProviderConfig{"selected": {Type: "typesafe"}}}}
			tc.change(cfg)
			if _, _, err := newClassifyGuardianReview(cfg, "policy"); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

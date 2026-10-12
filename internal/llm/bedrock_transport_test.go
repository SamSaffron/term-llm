package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/awsauth"
)

func TestBedrockInvokeURL(t *testing.T) {
	tests := []struct {
		region, model string
		beta          bool
		want          string
	}{
		{"us-west-2", "us.anthropic.claude-haiku-4-5-20251001-v1:0", false, "https://bedrock-runtime.us-west-2.amazonaws.com/model/us.anthropic.claude-haiku-4-5-20251001-v1%3A0/invoke-with-response-stream"},
		{"us-west-2", "arn:aws:bedrock:us-west-2:1:application-inference-profile/x", true, "https://bedrock-runtime.us-west-2.amazonaws.com/model/arn%3Aaws%3Abedrock%3Aus-west-2%3A1%3Aapplication-inference-profile%2Fx/invoke-with-response-stream?beta=true"},
		{"cn-north-1", "anthropic.claude", false, "https://bedrock-runtime.cn-north-1.amazonaws.com.cn/model/anthropic.claude/invoke-with-response-stream"},
	}
	for _, tt := range tests {
		got := (&bedrockTransport{region: tt.region}).invokeURL(tt.model, tt.beta)
		if got != tt.want {
			t.Errorf("invokeURL(%q, %q) = %q, want %q", tt.region, tt.model, got, tt.want)
		}
	}
}

// A connection cut in the middle of an event-stream frame must surface as a
// retryable StreamIncompleteError rather than a terminal decode error.
func TestBedrockTruncatedFrameIsIncomplete(t *testing.T) {
	full := goldenChunkFrame([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`))
	full = append(full, goldenChunkFrame([]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))...)
	full = append(full, goldenChunkFrame([]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`))...)
	next := goldenChunkFrame([]byte(`{"type":"message_stop"}`))
	body := append(full, next[:len(next)/2]...)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(body)
	}))
	defer ts.Close()
	target, _ := url.Parse(ts.URL)
	client := &anthropicClient{
		httpClient: &http.Client{Transport: goldenRedirectTransport{target: target, base: http.DefaultTransport}},
		bedrock:    &bedrockTransport{region: "us-west-2", bearerToken: "test-token"},
	}
	p := &AnthropicProvider{client: client, model: "us.anthropic.claude-sonnet-4-6"}
	stream, err := p.Stream(context.Background(), Request{Messages: []Message{UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	events := drainGoldenStream(t, stream)
	if len(events) < 2 || events[0].Type != EventTextDelta || events[0].Text != "partial" {
		t.Fatalf("events = %+v, want partial text first", events)
	}
	last := events[len(events)-1]
	var incomplete *StreamIncompleteError
	if last.Type != EventError || !errors.As(last.Err, &incomplete) {
		t.Fatalf("last event = %+v, want StreamIncompleteError", last)
	}
	if !isRetryable(last.Err) {
		t.Fatal("truncated Bedrock frame should be retryable")
	}
}

// Bedrock takes betas only in the body: configured anthropic-beta headers are
// merged into anthropic_beta, while other configured headers are sent and
// covered by the SigV4 signature.
func TestBedrockConfiguredHeaders(t *testing.T) {
	var gotHeader http.Header
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"stop"}`))
	}))
	defer ts.Close()
	target, _ := url.Parse(ts.URL)
	headers, err := newAnthropicExtraHeaders(map[string]string{"anthropic-beta": "beta-a, beta-b", "X-Gateway-Tag": "team-1"})
	if err != nil {
		t.Fatal(err)
	}
	client := &anthropicClient{
		headers:    headers,
		httpClient: &http.Client{Transport: goldenRedirectTransport{target: target, base: http.DefaultTransport}},
		bedrock:    &bedrockTransport{region: "us-west-2", credentials: staticBedrockCreds{}},
	}
	p := &AnthropicProvider{client: client, model: "us.anthropic.claude-sonnet-4-6", use1m: true}
	stream, err := p.Stream(context.Background(), Request{Messages: []Message{UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	drainGoldenStream(t, stream)

	if v := gotHeader.Values("Anthropic-Beta"); len(v) != 0 {
		t.Fatalf("anthropic-beta sent as header on Bedrock: %v", v)
	}
	betas, _ := gotBody["anthropic_beta"].([]any)
	if fmt.Sprint(betas) != "[context-1m-2025-08-07 beta-a beta-b]" {
		t.Fatalf("anthropic_beta = %v", betas)
	}
	if gotHeader.Get("X-Gateway-Tag") != "team-1" {
		t.Fatalf("configured header missing: %v", gotHeader)
	}
	if auth := gotHeader.Get("Authorization"); !strings.Contains(auth, "x-gateway-tag") {
		t.Fatalf("configured header not signed: %s", auth)
	}
}

type staticBedrockCreds struct{}

func (staticBedrockCreds) Retrieve(context.Context) (awsauth.Credentials, error) {
	return awsauth.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"}, nil
}

// model_map aliases decide thinking mode by their target, not the alias name.
func TestBedrockModelMapAliasThinkingMode(t *testing.T) {
	withAnthropicCache(t, nil)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_DEFAULT_PROFILE", "")
	modelMap := map[string]string{
		"fast":    "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		"big":     "us.anthropic.claude-opus-4-7-v1",
		"profile": "arn:aws:bedrock:us-west-2:1:application-inference-profile/x",
	}
	for model, wantAdaptive := range map[string]bool{"fast-thinking": false, "big-thinking": true, "profile-thinking": false, "claude-sonnet-4-6-thinking": true} {
		p, err := NewBedrockProvider(model, "us-west-2", "", "AKID", "secret", "", modelMap, nil)
		if err != nil {
			t.Fatal(err)
		}
		if p.inner.useAdaptive != wantAdaptive || (p.inner.thinkingBudget > 0) == wantAdaptive {
			t.Errorf("%s: adaptive=%v budget=%d, want adaptive %v", model, p.inner.useAdaptive, p.inner.thinkingBudget, wantAdaptive)
		}
	}
}

// A Bedrock API key does not need a working SigV4 credential chain.
func TestBedrockBearerTokenSkipsBrokenProfile(t *testing.T) {
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bedrock-api-key")
	p, err := NewBedrockProvider("claude-sonnet-4-6", "eu-west-1", "missing-profile", "", "", "", nil, nil)
	if err != nil {
		t.Fatalf("bearer token provider creation failed: %v", err)
	}
	if p.region != "eu-west-1" {
		t.Fatalf("region = %q", p.region)
	}
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	if _, err := NewBedrockProvider("claude-sonnet-4-6", "eu-west-1", "missing-profile", "", "", "", nil, nil); err == nil {
		t.Fatal("missing explicit profile without bearer token should fail")
	}
}

// The engine sends the configured (friendly) model as req.Model; it must be
// decoded and resolved through model_map exactly like the configured model.
func TestBedrockRequestModelResolvesThroughModelMap(t *testing.T) {
	withAnthropicCache(t, nil)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	arn := "arn:aws:bedrock:us-west-2:1:application-inference-profile/abc"
	p, err := NewBedrockProvider("claude-haiku", "us-west-2", "", "AKID", "secret", "", map[string]string{"claude-haiku": arn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, requested := range []string{"", "claude-haiku", "claude-haiku-thinking"} {
		spec := p.inner.requestSpec(Request{Model: requested})
		if spec.model != arn {
			t.Errorf("req.Model %q -> wire model %q, want %q", requested, spec.model, arn)
		}
		if spec.effort != "" || anthropicSupportsEffort(spec.model, "max") {
			t.Errorf("req.Model %q: inference profile must not get an effort", requested)
		}
	}
	if spec := p.inner.requestSpec(Request{Model: "claude-haiku-thinking"}); spec.thinkingBudget == 0 || spec.adaptive {
		t.Errorf("thinking via ARN = %+v, want budget thinking", spec)
	}
	if spec := p.inner.requestSpec(Request{Model: "claude-sonnet-4-6"}); spec.model != "us.anthropic.claude-sonnet-4-6" {
		t.Errorf("runtime switch to friendly name -> %q", spec.model)
	}
}

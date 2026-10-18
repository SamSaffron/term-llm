package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"github.com/samsaffron/term-llm/internal/llm/providers/anthropic"
	"io"
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
	p := newRedirectedBedrockProvider("us.anthropic.claude-sonnet-4-6", nil, target, &bedrockTransport{region: "us-west-2", bearerToken: "test-token"})
	stream, err := p.Stream(context.Background(), core.Request{Messages: []core.Message{core.UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	events := drainGoldenStream(t, stream)
	if len(events) < 2 || events[0].Type != core.EventTextDelta || events[0].Text != "partial" {
		t.Fatalf("events = %+v, want partial text first", events)
	}
	last := events[len(events)-1]
	var incomplete *corestream.StreamIncompleteError
	if last.Type != core.EventError || !errors.As(last.Err, &incomplete) {
		t.Fatalf("last event = %+v, want StreamIncompleteError", last)
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
	headers, err := anthropic.NewExtraHeaders(map[string]string{"anthropic-beta": "beta-a, beta-b", "X-Gateway-Tag": "team-1"})
	if err != nil {
		t.Fatal(err)
	}
	p := newRedirectedBedrockProvider("us.anthropic.claude-sonnet-4-6-1m", headers, target, &bedrockTransport{region: "us-west-2", credentials: staticBedrockCreds{}})
	stream, err := p.Stream(context.Background(), core.Request{Messages: []core.Message{core.UserText("hi")}})
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

// redirectedBedrockTransport sends the Bedrock transport's traffic to a local
// fixture server by substituting the HTTP client the provider hands it.
type redirectedBedrockTransport struct {
	inner *bedrockTransport
	hc    *http.Client
}

func (r redirectedBedrockTransport) StreamMessages(ctx context.Context, tr anthropic.TransportRequest) (anthropic.EventSource, error) {
	tr.HTTPClient = r.hc
	return r.inner.StreamMessages(ctx, tr)
}

func newRedirectedBedrockProvider(model string, headers http.Header, target *url.URL, t *bedrockTransport) *anthropic.AnthropicProvider {
	hc := &http.Client{Transport: goldenRedirectTransport{target: target, base: http.DefaultTransport}}
	return anthropic.NewTransportProvider(model, headers, redirectedBedrockTransport{inner: t, hc: hc}, func(m string) string { return m })
}

type staticBedrockCreds struct{}

func (staticBedrockCreds) Retrieve(context.Context) (awsauth.Credentials, error) {
	return awsauth.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"}, nil
}

// model_map aliases decide thinking mode by their target, not the alias name.
func TestBedrockModelMapAliasThinkingMode(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
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
		p, err := New(model, "us-west-2", "", "AKID", "secret", "", modelMap, nil)
		if err != nil {
			t.Fatal(err)
		}
		d := p.inner.Details()
		if d.Adaptive != wantAdaptive || (d.ThinkingBudget > 0) == wantAdaptive {
			t.Errorf("%s: adaptive=%v budget=%d, want adaptive %v", model, d.Adaptive, d.ThinkingBudget, wantAdaptive)
		}
	}
}

// A Bedrock API key does not need a working SigV4 credential chain.
func TestBedrockBearerTokenSkipsBrokenProfile(t *testing.T) {
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bedrock-api-key")
	p, err := New("claude-sonnet-4-6", "eu-west-1", "missing-profile", "", "", "", nil, nil)
	if err != nil {
		t.Fatalf("bearer token provider creation failed: %v", err)
	}
	if p.region != "eu-west-1" {
		t.Fatalf("region = %q", p.region)
	}
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	if _, err := New("claude-sonnet-4-6", "eu-west-1", "missing-profile", "", "", "", nil, nil); err == nil {
		t.Fatal("missing explicit profile without bearer token should fail")
	}
}

// The engine sends the configured (friendly) model as req.Model; it must be
// decoded and resolved through model_map exactly like the configured model.
func TestBedrockRequestModelResolvesThroughModelMap(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	arn := "arn:aws:bedrock:us-west-2:1:application-inference-profile/abc"

	var gotPath string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotBody = nil
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"stop"}`))
	}))
	defer ts.Close()
	target, _ := url.Parse(ts.URL)
	original := http.DefaultTransport
	http.DefaultTransport = goldenRedirectTransport{target: target, base: original.(*http.Transport).Clone()}
	t.Cleanup(func() { http.DefaultTransport = original })

	p, err := New("claude-haiku", "us-west-2", "", "AKID", "secret", "", map[string]string{"claude-haiku": arn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	send := func(requested string) {
		t.Helper()
		gotPath, gotBody = "", nil
		stream, err := p.Stream(context.Background(), core.Request{Model: requested, Messages: []core.Message{core.UserText("hi")}})
		if err != nil {
			t.Fatal(err)
		}
		drainGoldenStream(t, stream)
	}
	wantPath := "/model/" + url.QueryEscape(arn) + "/invoke-with-response-stream"
	for _, requested := range []string{"", "claude-haiku", "claude-haiku-thinking"} {
		send(requested)
		if gotPath != wantPath {
			t.Errorf("req.Model %q -> wire path %q, want %q", requested, gotPath, wantPath)
		}
		if _, ok := gotBody["output_config"]; ok {
			t.Errorf("req.Model %q: inference profile must not get an effort", requested)
		}
	}
	send("claude-haiku-thinking")
	thinking, _ := gotBody["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] == nil {
		t.Errorf("thinking via ARN = %v, want budget thinking", gotBody["thinking"])
	}
	send("claude-sonnet-4-6")
	if want := "/model/us.anthropic.claude-sonnet-4-6/invoke-with-response-stream"; gotPath != want {
		t.Errorf("runtime switch to friendly name -> %q, want %q", gotPath, want)
	}
}

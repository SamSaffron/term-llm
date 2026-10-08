package typesafe

import (
	"github.com/samsaffron/term-llm/internal/classify"
	"github.com/samsaffron/term-llm/internal/classify/transport"

	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/providerhttp"
)

func TestClassifySendsAuthAndParsesPrimitives(t *testing.T) {
	var sawRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != classifyPath {
			t.Fatalf("path = %q, want %q", r.URL.Path, classifyPath)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %q", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-key" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Fatalf("Content-Type = %q", got)
		}
		var req classify.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if string(req.State) != `{"ticket":"Help!"}` {
			t.Fatalf("state = %s", req.State)
		}
		if req.Model != "jev-latest" || len(req.Questions) != 3 {
			t.Fatalf("unexpected request: %#v", req)
		}
		sawRequest = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model":"jev-latest",
			"answers":{
				"urgent":{"type":"noul","noul":0.92},
				"team":{"type":"choice","choice":"technical","probabilities":{"billing":0.1,"technical":0.9},"confidence":null},
				"tone":{"type":"score","score":1.5,"legend":{"0":"calm","1":{"label":"angry"}},"probabilities":{"0":0.5,"1":0.5},"confidence":0.7}
			},
			"usage":{"input_tokens":12,"output_tokens":3}
		}`))
	}))
	defer server.Close()

	client, err := NewClient(Options{APIKey: "secret-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	resp, err := client.Classify(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if !sawRequest {
		t.Fatal("server did not see request")
	}
	if resp.Model != "jev-latest" || len(resp.Raw) == 0 {
		t.Fatalf("response metadata = model %q raw len %d", resp.Model, len(resp.Raw))
	}
	if got := *resp.Answers["urgent"].Noul; got != 0.92 {
		t.Fatalf("noul = %v", got)
	}
	team := resp.Answers["team"]
	if team.Confidence != nil {
		t.Fatalf("nullable confidence decoded as %v, want nil", *team.Confidence)
	}
	if *team.Choice != "technical" || team.Probabilities["technical"] != 0.9 {
		t.Fatalf("choice answer = %#v", team)
	}
	tone := resp.Answers["tone"]
	if *tone.Score != 1.5 || string(tone.Legend["1"]) != `{"label":"angry"}` {
		t.Fatalf("score answer = %#v", tone)
	}
	if resp.Usage.InputTokens == nil || *resp.Usage.InputTokens != 12 {
		t.Fatalf("usage = %#v", resp.Usage)
	}
}

// The 24 KB Guardian packet budget must not leak into ordinary classify calls.
func TestClassifyClientDoesNotApplyGuardianBudget(t *testing.T) {
	state, err := json.Marshal(strings.Repeat("large standalone input ", 3000))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request classify.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !bytes.Equal(request.State, state) {
			t.Errorf("standalone input was changed: got %d bytes, want %d", len(request.State), len(state))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-latest","answers":{"q":{"type":"noul","noul":0.9}}}`))
	}))
	defer server.Close()
	client, err := NewClient(Options{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Classify(context.Background(), classify.Request{Model: "jev-latest", State: state, Questions: validQuestions()}); err != nil {
		t.Fatal(err)
	}
}

func TestListModelsSendsAuthAndParsesModelsObject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != modelsPath || r.Method != http.MethodGet {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-key" {
			t.Fatalf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"jev-latest","description":"flagship","release_date":"2026-01-01"}]}`))
	}))
	defer server.Close()

	client, err := NewClient(Options{APIKey: "secret-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models.Models) != 1 || models.Models[0].Name != "jev-latest" || len(models.Raw) == 0 {
		t.Fatalf("models = %#v", models)
	}
}

func TestListModelsAllowsMissingMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"jev-latest"}]}`))
	}))
	defer server.Close()

	client, err := NewClient(Options{APIKey: "secret-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models.Models) != 1 || models.Models[0].Name != "jev-latest" {
		t.Fatalf("models = %#v", models)
	}
}

func TestNewClientValidationAndDefaults(t *testing.T) {
	if _, err := NewClient(Options{}); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("missing key error = %v", err)
	}
	if _, err := NewClient(Options{APIKey: "k", BaseURL: "://bad"}); err == nil {
		t.Fatal("malformed base URL succeeded")
	}
	client, err := NewClient(Options{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewClient defaults: %v", err)
	}
	if client.http.BaseURL() != DefaultBaseURL || client.http.Timeout() != DefaultTimeout {
		t.Fatalf("defaults = %s timeout %s", client.http.BaseURL(), client.http.Timeout())
	}
}

func TestRetries429And529WithRetryAfterMS(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := attempts.Add(1)
		if attempt == 1 {
			w.Header().Set("retry-after-ms", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"rate limited"}`))
			return
		}
		if attempt == 2 {
			w.Header().Set("retry-after-ms", "1")
			w.WriteHeader(529)
			_, _ = w.Write([]byte(`{"message":"overloaded"}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"jev-latest","answers":{"q":{"type":"noul","noul":1}},"usage":{}}`))
	}))
	defer server.Close()

	client, err := NewClient(Options{APIKey: "secret-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Classify(context.Background(), classify.Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: map[string]classify.Question{"q": {Type: "noul", Instructions: json.RawMessage(`"?"`)}}}); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestRetryExhaustionReturnsLastError(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("retry-after-ms", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer server.Close()

	client, err := NewClient(Options{APIKey: "secret-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.Classify(context.Background(), classify.Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: validQuestions()})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("Classify error = %v, want 429 APIError", err)
	}
	if got := attempts.Load(); got != transport.MaxRetries+1 {
		t.Fatalf("attempts = %d, want %d", got, transport.MaxRetries+1)
	}
}

func TestRetryAfterHeaderOverflowIsBounded(t *testing.T) {
	header := http.Header{}
	header.Set("retry-after-ms", "9223372036854775807")
	delay, ok := providerhttp.ParseRetryAfter(header, time.Now())
	if !ok || delay <= transport.BackoffMax {
		t.Fatalf("delay = %v, want above cap", delay)
	}
}

func TestRetryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("retry-after-ms", "5000")
		w.WriteHeader(http.StatusTooManyRequests)
		cancel()
	}))
	defer server.Close()

	client, err := NewClient(Options{APIKey: "secret-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	start := time.Now()
	_, err = client.Classify(ctx, classify.Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: map[string]classify.Question{"q": {Type: "noul", Instructions: json.RawMessage(`"?"`)}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Classify error = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation waited for retry delay")
	}
}

func TestErrorsAreUsefulAndRedacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"field questions.q.criteria is required for private customer text using secret-key"}`))
	}))
	defer server.Close()

	client, err := NewClient(Options{APIKey: "secret-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	state := `"private customer text"`
	_, err = client.Classify(context.Background(), classify.Request{State: json.RawMessage(state), Model: "jev-latest", Questions: map[string]classify.Question{"q": {Type: "noul", Instructions: json.RawMessage(`"?"`)}}})
	if err == nil {
		t.Fatal("Classify succeeded unexpectedly")
	}
	msg := err.Error()
	if !strings.Contains(msg, "422") || !strings.Contains(msg, "criteria") {
		t.Fatalf("error = %q", msg)
	}
	if strings.Contains(msg, "secret-key") || strings.Contains(msg, "private customer text") {
		t.Fatalf("error leaked sensitive data: %q", msg)
	}
}

func TestErrorsRedactStructuredStateEcho(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"bad state {\"account\":12345,\"customer\":\"Ada\"}"}`))
	}))
	defer server.Close()
	client, err := NewClient(Options{APIKey: "secret-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.Classify(context.Background(), classify.Request{State: json.RawMessage(`{"account":12345,"customer":"Ada"}`), Model: "jev-latest", Questions: validQuestions()})
	if err == nil {
		t.Fatal("Classify succeeded unexpectedly")
	}
	if msg := err.Error(); strings.Contains(msg, "12345") || strings.Contains(msg, "Ada") {
		t.Fatalf("error leaked structured state: %q", msg)
	}
}

func TestMalformedResponseFields(t *testing.T) {
	tests := []struct {
		name string
		body string
		q    classify.Question
		want string
	}{
		{name: "missing answer field", body: `{"model":"jev-latest","answers":{"q":{"type":"noul"}},"usage":{}}`, q: classify.Question{Type: "noul", Instructions: json.RawMessage(`"?"`)}, want: "missing noul"},
		{name: "bad confidence", body: `{"model":"jev-latest","answers":{"q":{"type":"choice","choice":"a","probabilities":{"a":1},"confidence":2}},"usage":{}}`, q: classify.Question{Type: "choice", Instructions: json.RawMessage(`"?"`), Criteria: json.RawMessage(`{"a":null}`)}, want: "confidence"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tt.body)) }))
			defer server.Close()
			client, err := NewClient(Options{APIKey: "secret-key", BaseURL: server.URL})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			_, err = client.Classify(context.Background(), classify.Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: map[string]classify.Question{"q": tt.q}})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}

}

func TestResponseValidationAgainstRequest(t *testing.T) {
	tests := []struct {
		name string
		body string
		q    classify.Question
		want string
	}{
		{name: "missing usage", body: `{"model":"jev-latest","answers":{"q":{"type":"noul","noul":0.5}}}`, q: classify.Question{Type: "noul", Instructions: json.RawMessage(`"?"`)}, want: ""},
		{name: "missing confidence", body: `{"model":"jev-latest","answers":{"q":{"type":"choice","choice":"a","probabilities":{"a":1}}},"usage":{}}`, q: classify.Question{Type: "choice", Instructions: json.RawMessage(`"?"`), Criteria: json.RawMessage(`{"a":null}`)}, want: ""},
		{name: "nullable confidence allowed", body: `{"model":"jev-latest","answers":{"q":{"type":"choice","choice":"a","probabilities":{"a":1},"confidence":null}},"usage":{}}`, q: classify.Question{Type: "choice", Instructions: json.RawMessage(`"?"`), Criteria: json.RawMessage(`{"a":null}`)}, want: ""},
		{name: "probability null rejected", body: `{"model":"jev-latest","answers":{"q":{"type":"choice","choice":"a","probabilities":{"a":null},"confidence":null}},"usage":{}}`, q: classify.Question{Type: "choice", Instructions: json.RawMessage(`"?"`), Criteria: json.RawMessage(`{"a":null}`)}, want: "probability"},
		{name: "noul out of range", body: `{"model":"jev-latest","answers":{"q":{"type":"noul","noul":1.1}},"usage":{}}`, q: classify.Question{Type: "noul", Instructions: json.RawMessage(`"?"`)}, want: "noul must be between"},
		{name: "answer id mismatch", body: `{"model":"jev-latest","answers":{"other":{"type":"noul","noul":0.5}},"usage":{}}`, q: classify.Question{Type: "noul", Instructions: json.RawMessage(`"?"`)}, want: "missing answer"},
		{name: "answer type mismatch", body: `{"model":"jev-latest","answers":{"q":{"type":"score","score":1,"legend":{"0":"no","1":"yes"},"probabilities":{"0":0.5,"1":0.5},"confidence":0.5}},"usage":{}}`, q: classify.Question{Type: "choice", Instructions: json.RawMessage(`"?"`), Criteria: json.RawMessage(`{"a":null}`)}, want: "does not match"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tt.body)) }))
			defer server.Close()
			client, err := NewClient(Options{APIKey: "secret-key", BaseURL: server.URL})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			_, err = client.Classify(context.Background(), classify.Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: map[string]classify.Question{"q": tt.q}})
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Classify error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRejectsRedirectsAndUnsafeBaseURL(t *testing.T) {
	for _, baseURL := range []string{
		"https://user:pass@example.test",
		"https://example.test?token=secret",
		"https://example.test#secret",
	} {
		if _, err := NewClient(Options{APIKey: "k", BaseURL: baseURL}); err == nil || !strings.Contains(err.Error(), "must not include") {
			t.Fatalf("NewClient(%q) error = %v", baseURL, err)
		}
	}

	redirected := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			redirected = true
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer server.Close()
	client, err := NewClient(Options{APIKey: "secret-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.Classify(context.Background(), classify.Request{State: json.RawMessage(`"private"`), Model: "jev-latest", Questions: validQuestions()})
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("redirect error = %v", err)
	}
	if redirected {
		t.Fatal("client followed redirect")
	}
}

func TestOverallTimeoutBoundsRetries(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("retry-after-ms", "5000")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client, err := NewClient(Options{APIKey: "secret-key", BaseURL: server.URL, Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	start := time.Now()
	_, err = client.Classify(context.Background(), classify.Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: validQuestions()})
	var status *providerhttp.StatusError
	if !errors.As(err, &status) || status.StatusCode != 429 {
		t.Fatalf("Classify error = %v, want last status error", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("timeout did not bound retry delay")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

func TestRequestCancellationDuringActualRequest(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}))
	defer server.Close()
	client, err := NewClient(Options{APIKey: "secret-key", BaseURL: server.URL, Timeout: time.Second})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Classify(ctx, classify.Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: validQuestions()})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Classify error = %v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request cancellation did not return")
	}
}

func TestResponseBodyLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), transport.MaxBodyBytes+1))
	}))
	defer server.Close()
	client, err := NewClient(Options{APIKey: "secret-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.Classify(context.Background(), classify.Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: validQuestions()})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("Classify error = %v, want body limit", err)
	}
}

// TestNewClientTrimsAPIKey pins that a key with the trailing newline that file
// and command-substitution sources add still produces a valid header.
func TestNewClientTrimsAPIKey(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-latest","answers":{"urgent":{"type":"noul","noul":0.5},"team":{"type":"choice","choice":"billing"},"tone":{"type":"score","score":1}}}`))
	}))
	defer server.Close()

	client, err := NewClient(Options{APIKey: "secret-key\n", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Classify(context.Background(), validRequest()); err != nil {
		t.Fatalf("Classify with newline-terminated key: %v", err)
	}
	if authorization != "Bearer secret-key" {
		t.Fatalf("Authorization = %q, want the trimmed key", authorization)
	}
}

func validRequest() classify.Request {
	return classify.Request{
		State: json.RawMessage(`{"ticket":"Help!"}`),
		Model: "jev-latest",
		Questions: map[string]classify.Question{
			"urgent": {Type: "noul", Instructions: json.RawMessage(`"Does this convey urgency?"`)},
			"team":   {Type: "choice", Instructions: json.RawMessage(`"Which team?"`), Criteria: json.RawMessage(`{"billing":"Billing","technical":"Technical"}`)},
			"tone":   {Type: "score", Instructions: json.RawMessage(`"How upset?"`), Criteria: json.RawMessage(`["Calm","Angry"]`)},
		},
	}
}

func validQuestions() map[string]classify.Question {
	return map[string]classify.Question{"q": {Type: "noul", Instructions: json.RawMessage(`"?"`)}}
}

func Test422DiagnosticsPreserveSchema(t *testing.T) {
	for _, state := range []json.RawMessage{json.RawMessage(`"Al"`), json.RawMessage(`{"private": "Al"}`)} {
		t.Run(string(state), func(t *testing.T) {
			req := classify.Request{State: state, Model: "jev-latest", Questions: validQuestions()}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(422)
				json.NewEncoder(w).Encode(map[string]any{"detail": []any{
					map[string]any{"loc": []string{"body", "model"}, "msg": "model jev-latest is unavailable; type noul requires instructions", "input": req.Model},
					map[string]any{"loc": []string{"body", "state"}, "input": state},
				}, "message_unused": "unused"})
			}))
			defer server.Close()
			client, _ := NewClient(Options{APIKey: "secret-key", BaseURL: server.URL})
			_, err := client.Classify(context.Background(), req)
			if err == nil {
				t.Fatal("expected status error")
			}
			for _, text := range []string{"422", "jev-latest", "noul", "instructions", "[redacted]"} {
				if !strings.Contains(err.Error(), text) {
					t.Fatalf("missing %s: %v", text, err)
				}
			}
			if strings.Contains(err.Error(), "Al") {
				t.Fatalf("state leaked: %v", err)
			}
			var status *providerhttp.StatusError
			if !errors.As(err, &status) || strings.Contains(status.Body, "Al") {
				t.Fatalf("unsafe shared status: %#v", status)
			}
		})
	}
}

func TestResponseEvolution(t *testing.T) {
	for _, suffix := range []string{"", `,"usage":null`, `,"usage":{"input_tokens":1},"new_field":true`} {
		body := `{"model":"jev-latest","answers":{"q":{"type":"score","score":0},"extra":{"type":"future","value":true}}` + suffix + `}`
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		client, _ := NewClient(Options{APIKey: "test-key", BaseURL: server.URL})
		resp, err := client.Classify(context.Background(), classify.Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: map[string]classify.Question{"q": {Type: "score", Instructions: json.RawMessage(`"?"`), Criteria: json.RawMessage(`["no","yes"]`)}}})
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(resp.Raw) != body || resp.Answers["q"].Score == nil {
			t.Fatalf("response lost fidelity: %#v", resp)
		}
	}
}

func TestRetryDelaysDoNotRetryEarly(t *testing.T) {
	for _, tt := range []struct {
		name, header string
		timeout      time.Duration
	}{
		{"cap", "3000", time.Minute}, {"deadline", "1000", 100 * time.Millisecond}, {"overflow", "9223372036854775807", time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("Retry-After-Ms", tt.header)
				w.WriteHeader(429)
			}))
			defer server.Close()
			client, _ := NewClient(Options{APIKey: "test-key", BaseURL: server.URL, Timeout: tt.timeout})
			_, err := client.Classify(context.Background(), classify.Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: validQuestions()})
			var status *providerhttp.StatusError
			if !errors.As(err, &status) || status.StatusCode != 429 || attempts.Load() != 1 {
				t.Fatalf("error=%v attempts=%d", err, attempts.Load())
			}
		})
	}
}

func TestTransientStatusRetriesRespectDelay(t *testing.T) {
	for _, code := range []int{408, 429, 500, 503, 529} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var attempts int
			var first time.Time
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				if attempts == 1 {
					first = time.Now()
					w.Header().Set("Retry-After-Ms", "5")
					w.WriteHeader(code)
					return
				}
				if time.Since(first) < 5*time.Millisecond {
					t.Error("retried earlier than server delay")
				}
				fmt.Fprint(w, `{"model":"jev-latest","answers":{"q":{"type":"noul","noul":0}}}`)
			}))
			defer server.Close()
			client, _ := NewClient(Options{APIKey: "test-key", BaseURL: server.URL})
			_, err := client.Classify(context.Background(), classify.Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: validQuestions()})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTruncateUTF8(t *testing.T) {
	got := transport.Truncate(strings.Repeat("界", 200))
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "...") {
		t.Fatalf("invalid truncation %q", got)
	}
}

func TestClassifyRejectsImagesWithoutRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("image request must not reach TypeSafe")
	}))
	defer srv.Close()
	c, err := NewClient(Options{APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Classify(context.Background(), classify.Request{State: json.RawMessage(`"s"`), Model: "m", Images: []string{"data:image/png;base64,AA=="},
		Questions: map[string]classify.Question{"q": {Type: "noul", Instructions: json.RawMessage(`"?"`)}}})
	if !errors.Is(err, classify.ErrImagesUnsupported) {
		t.Fatalf("err = %v, want ErrImagesUnsupported", err)
	}
}

func TestOutOfRangeScoreLiteralIsRejected(t *testing.T) {
	// The wire path refuses the same value earlier, at decode.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-latest","answers":{"tone":{"type":"score","score":1e999}}}`))
	}))
	defer server.Close()
	client, err := NewClient(Options{APIKey: "secret-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Classify(context.Background(), validRequest()); err == nil {
		t.Fatal("out-of-range score literal was accepted")
	}
}

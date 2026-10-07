package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/samsaffron/term-llm/internal/classify"
)

const redPNG = "data:image/png;base64,iVBORw0KGgo="

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewClient(Options{APIKey: "sk-test\n", BaseURL: srv.URL + "/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClassifyMapsRequestAndResponse(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/decisions" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer sk-test" {
			t.Errorf("Authorization = %q", auth)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, `{"model":"gpt-6-luna","answers":[
			{"type":"predicate","name":"red","probability":0.92},
			{"type":"choice","name":"dept","choice":"billing","probabilities":[{"value":"tech","probability":0.1},{"value":"billing","probability":0.9}],"confidence":0.8},
			{"type":"score","name":"sev","score":1.1,"probabilities":[{"value":0,"label":"low","probability":0.2},{"value":1,"label":"high","probability":0.8}],"confidence":0.5}
		],"usage":{"input_tokens":42,"output_tokens":0,"total_tokens":42}}`)
	})
	req := classify.Request{
		State: json.RawMessage(`"I was charged twice"`),
		Model: "gpt-6-luna",
		Questions: map[string]classify.Question{
			"red":  {Type: "noul", Instructions: json.RawMessage(`"Is it red?"`), Criteria: json.RawMessage(`{"true":"mostly red","false":null}`)},
			"dept": {Type: "choice", Instructions: json.RawMessage(`"Department?"`), Criteria: json.RawMessage(`{"tech":null,"billing":"payments"}`)},
			"sev":  {Type: "score", Instructions: json.RawMessage(`{"ask":"severity"}`), Criteria: json.RawMessage(`["low",{"label":"high","description":"blocked"}]`)},
		},
	}
	resp, err := c.Classify(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	if got["input"] != "I was charged twice" {
		t.Fatalf("text-only input should be a string, got %#v", got["input"])
	}
	questions, _ := json.Marshal(got["questions"])
	want := `[{"choices":[{"value":"tech"},{"description":"payments","value":"billing"}],"instructions":"Department?","name":"dept","type":"choice"},` +
		`{"instructions":"Is it red?\n\nTrue: mostly red","name":"red","type":"predicate"},` +
		`{"instructions":"{\"ask\":\"severity\"}","levels":[{"label":"low"},{"description":"blocked","label":"high"}],"name":"sev","type":"score"}]`
	if string(questions) != want {
		t.Fatalf("questions =\n%s\nwant\n%s", questions, want)
	}

	if a := resp.Answers["red"]; a.Type != "noul" || a.Noul == nil || *a.Noul != 0.92 {
		t.Fatalf("predicate answer = %+v", a)
	}
	if a := resp.Answers["dept"]; a.Type != "choice" || *a.Choice != "billing" || a.Probabilities["billing"] != 0.9 || *a.Confidence != 0.8 {
		t.Fatalf("choice answer = %+v", a)
	}
	sev := resp.Answers["sev"]
	if sev.Type != "score" || *sev.Score != 1.1 || sev.Probabilities["1"] != 0.8 || string(sev.Legend["1"]) != `"high"` {
		t.Fatalf("score answer = %+v", sev)
	}
	if resp.Usage.InputTokens == nil || *resp.Usage.InputTokens != 42 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	var normalized classify.Response
	if err := json.Unmarshal(resp.Raw, &normalized); err != nil || normalized.Answers["dept"].Choice == nil {
		t.Fatalf("Raw should be TypeSafe-shaped JSON: %s (%v)", resp.Raw, err)
	}
}

func TestClassifySendsImagesAsMessageParts(t *testing.T) {
	cases := []struct {
		name  string
		state json.RawMessage
		want  string
	}{
		{"with text", json.RawMessage(`{"sku":1}`), `[{"content":[{"text":"{\"sku\":1}","type":"input_text"},{"image_url":"` + redPNG + `","type":"input_image"}],"role":"user"}]`},
		{"image only", nil, `[{"content":[{"image_url":"` + redPNG + `","type":"input_image"}],"role":"user"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var input json.RawMessage
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Input json.RawMessage `json:"input"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				input = body.Input
				io.WriteString(w, `{"model":"gpt-6-luna","answers":[{"type":"predicate","name":"red","probability":1}]}`)
			})
			_, err := c.Classify(context.Background(), classify.Request{
				State: tc.state, Model: "gpt-6-luna", Images: []string{redPNG},
				Questions: map[string]classify.Question{"red": {Type: "noul", Instructions: json.RawMessage(`"Is it red?"`)}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := mustCompactSorted(t, input); got != tc.want {
				t.Fatalf("input =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

func mustCompactSorted(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func TestClassifyRejectsInvalidRequestsLocally(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	q := map[string]classify.Question{"q": {Type: "noul", Instructions: json.RawMessage(`"?"`)}}
	cases := []struct {
		name string
		req  classify.Request
		want string
	}{
		{"no state or image", classify.Request{Model: "m", Questions: q}, "state is required"},
		{"hosted image", classify.Request{Model: "m", Questions: q, Images: []string{"https://x/y.png"}}, "data:image"},
		{"null instructions", classify.Request{State: json.RawMessage(`"s"`), Model: "m", Questions: map[string]classify.Question{"q": {Type: "noul", Instructions: json.RawMessage(`null`)}}}, "instructions are required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Classify(context.Background(), tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid requests reached the server %d times", calls.Load())
	}
}

func TestClassifyResponseFailures(t *testing.T) {
	q := map[string]classify.Question{"q": {Type: "noul", Instructions: json.RawMessage(`"?"`)}}
	cases := []struct {
		name, body, want string
	}{
		{"refusal", `{"answers":[{"type":"refusal","name":"q","refusal":"nope"}]}`, `question "q" was refused: nope`},
		{"missing answer", `{"answers":[{"type":"predicate","name":"other","probability":0.5}]}`, `missing answer for question "q"`},
		{"type mismatch", `{"answers":[{"type":"choice","name":"q","choice":"a"}]}`, `does not match question type "noul"`},
		{"bad probability", `{"answers":[{"type":"predicate","name":"q","probability":1.5}]}`, "between 0 and 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, tc.body) })
			_, err := c.Classify(context.Background(), classify.Request{State: json.RawMessage(`"s"`), Model: "m", Questions: q})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestClassifyRetriesAndRedactsErrors(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":{"message":"bad state secret-state with key sk-test"}}`)
	})
	_, err := c.Classify(context.Background(), classify.Request{
		State: json.RawMessage(`"secret-state"`), Model: "m",
		Questions: map[string]classify.Question{"q": {Type: "noul", Instructions: json.RawMessage(`"?"`)}},
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("err = %v, want APIError 500", err)
	}
	if strings.Contains(err.Error(), "secret-state") || strings.Contains(err.Error(), "sk-test") {
		t.Fatalf("error leaks sensitive values: %v", err)
	}
	if calls.Load() != maxRetries+1 {
		t.Fatalf("calls = %d, want %d", calls.Load(), maxRetries+1)
	}
}

func TestNewClientRequiresKey(t *testing.T) {
	if _, err := NewClient(Options{APIKey: " "}); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}

func TestErrorsRedactStateEchoesAndWholeKey(t *testing.T) {
	q := map[string]classify.Question{"q": {Type: "noul", Instructions: json.RawMessage(`"?"`)}}
	for _, tc := range []struct {
		name, state, echo string
		leaks             []string
	}{
		{"escaped structured echo", `{"account":12345,"customer":"Ada"}`, `bad input {\"account\":12345,\"customer\":\"Ada\"}`, []string{"12345", "Ada"}},
		{"short state inside key", `"sk"`, `invalid key sk-test`, []string{"-test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				body, _ := json.Marshal(map[string]any{"error": map[string]string{"message": tc.echo}})
				w.Write(body)
			})
			_, err := c.Classify(context.Background(), classify.Request{State: json.RawMessage(tc.state), Model: "m", Questions: q})
			if err == nil {
				t.Fatal("expected error")
			}
			for _, leak := range tc.leaks {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("error leaks %q: %v", leak, err)
				}
			}
		})
	}
}

func TestScoreLevelsAndProbabilityValues(t *testing.T) {
	q, err := buildQuestion("sev", classify.Question{Type: "score", Instructions: json.RawMessage(`"?"`),
		Criteria: json.RawMessage(`["low",{"label":"high","description":{"text":"blocked"}}]`)})
	if err != nil {
		t.Fatal(err)
	}
	if q.Levels[1] != (wireLevel{Label: "high", Description: `{"text":"blocked"}`}) {
		t.Fatalf("levels = %+v", q.Levels)
	}
	for _, criteria := range []string{`["low",{"label":""}]`, `["low",null]`} {
		if _, err := buildQuestion("sev", classify.Question{Type: "score", Instructions: json.RawMessage(`"?"`), Criteria: json.RawMessage(criteria)}); err == nil || !strings.Contains(err.Error(), "non-empty label") {
			t.Fatalf("criteria %s: err = %v", criteria, err)
		}
	}
	_, err = convertAnswer(wireAnswer{Type: "choice", Name: "q", Choice: json.RawMessage(`"a"`), Probabilities: []wireProbability{{Value: json.RawMessage(`null`)}}})
	if err == nil || !strings.Contains(err.Error(), "missing its value") {
		t.Fatalf("missing probability value err = %v", err)
	}
}

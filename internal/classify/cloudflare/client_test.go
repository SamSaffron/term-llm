package cloudflare

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

const pngURL = "data:image/png;base64,iVBORw0KGgo="

func noulQuestions() map[string]classify.Question {
	return map[string]classify.Question{"q": {Type: "noul", Instructions: json.RawMessage(`"?"`)}}
}

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewClient(Options{APIKey: "cf-token", BaseURL: srv.URL + "/client/v4/accounts/{account_id}/ai/run", AccountID: "acc123"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClassifySendsSystemOneAndUnwrapsEnvelope(t *testing.T) {
	var path string
	var got map[string]json.RawMessage
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if auth := r.Header.Get("Authorization"); auth != "Bearer cf-token" {
			t.Errorf("Authorization = %q", auth)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		io.WriteString(w, `{"result":{"model":"clef-flash","answers":{"q":{"type":"noul","noul":0.97}},"usage":{"input_tokens":12,"output_tokens":0}},"success":true,"errors":[],"messages":[]}`)
	})
	resp, err := c.Classify(context.Background(), classify.Request{State: json.RawMessage(`{"a":1}`), Model: "clef-flash", Questions: noulQuestions(), Images: []string{pngURL}})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/client/v4/accounts/acc123/ai/run/@cf/cloudflare/clef-flash" {
		t.Fatalf("path = %q", path)
	}
	if string(got["model"]) != `"clef-flash"` || string(got["state"]) != `{"a":1}` || string(got["images"]) != `["`+pngURL+`"]` {
		t.Fatalf("body = %v", got)
	}
	if a := resp.Answers["q"]; a.Noul == nil || *a.Noul != 0.97 || *resp.Usage.InputTokens != 12 {
		t.Fatalf("response = %+v", resp)
	}
	if !strings.HasPrefix(string(resp.Raw), `{"model":"clef-flash"`) {
		t.Fatalf("Raw should be the unwrapped result: %s", resp.Raw)
	}
}

func TestClassifyImageOnlySendsEmptyState(t *testing.T) {
	var state json.RawMessage
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			State json.RawMessage `json:"state"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		state = body.State
		io.WriteString(w, `{"result":{"model":"clef","answers":{"q":{"type":"noul","noul":1}}},"success":true}`)
	})
	if _, err := c.Classify(context.Background(), classify.Request{Model: "clef", Questions: noulQuestions(), Images: []string{pngURL}}); err != nil {
		t.Fatal(err)
	}
	if string(state) != `""` {
		t.Fatalf("state = %s, want empty string", state)
	}
}

func TestClassifyRejectsClefLimitsLocally(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	many := map[string]classify.Question{}
	for i := 0; i <= maxQuestions; i++ {
		many[strings.Repeat("q", i+1)] = classify.Question{Type: "noul", Instructions: json.RawMessage(`"?"`)}
	}
	big := "data:image/png;base64," + strings.Repeat("A", (maxImageBytes/3+1)*4)
	for _, tc := range []struct {
		name string
		req  classify.Request
		want string
	}{
		{"one choice option", classify.Request{State: json.RawMessage(`"s"`), Model: "clef", Questions: map[string]classify.Question{"q": {Type: "choice", Instructions: json.RawMessage(`"?"`), Criteria: json.RawMessage(`{"only":null}`)}}}, "2 to 255 choice options"},
		{"too many questions", classify.Request{State: json.RawMessage(`"s"`), Model: "clef", Questions: many}, "at most 64 questions"},
		{"too many score levels", classify.Request{State: json.RawMessage(`"s"`), Model: "clef", Questions: map[string]classify.Question{"q": {Type: "score", Instructions: json.RawMessage(`"?"`), Criteria: json.RawMessage(`["0","1","2","3","4","5","6","7","8","9","10"]`)}}}, "at most 10 score levels"},
		{"too many images", classify.Request{Model: "clef", Questions: noulQuestions(), Images: []string{pngURL, pngURL, pngURL, pngURL, pngURL}}, "at most 4 images"},
		{"gif image", classify.Request{Model: "clef", Questions: noulQuestions(), Images: []string{"data:image/gif;base64,R0lGODlh"}}, "PNG, JPEG, or WebP"},
		{"oversized image", classify.Request{Model: "clef", Questions: noulQuestions(), Images: []string{big}}, "exceeds"},
		{"path-unsafe model", classify.Request{State: json.RawMessage(`"s"`), Model: "../x", Questions: noulQuestions()}, "must contain only"},
	} {
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

func TestClassifyErrorsAreReadableAndRedacted(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"workers ai validation", `{"errors":[{"message":"AiError: AiError: {\"error\":\"bad\"} secret-state","code":5012}],"success":false,"result":{}}`, `{"error":"bad"} [redacted] (code 5012)`, 422},
		{"gateway", `{"success":false,"error":[{"code":2009,"message":"Unauthorized"}],"message":"Unauthorized"}`, "Unauthorized (code 2009)", 401},
		{"success false on 200", `{"result":null,"success":false,"errors":[{"code":1,"message":"nope secret-state"}]}`, "request failed: nope [redacted] (code 1)", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			_, err := c.Classify(context.Background(), classify.Request{State: json.RawMessage(`"secret-state"`), Model: "clef", Questions: noulQuestions()})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret-state") || strings.Contains(err.Error(), "cf-token") {
				t.Fatalf("error leaks sensitive values: %v", err)
			}
			var apiErr *APIError
			if tc.status != 200 && (!errors.As(err, &apiErr) || apiErr.StatusCode != tc.status) {
				t.Fatalf("err = %#v, want APIError %d", err, tc.status)
			}
		})
	}
}

func TestNewClientAccountID(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"missing", Options{APIKey: "k"}, "CLOUDFLARE_ACCOUNT_ID"},
		{"unsafe", Options{APIKey: "k", AccountID: "../x"}, "letters and digits"},
		{"missing key", Options{AccountID: "acc"}, "wrangler auth token"},
	} {
		if _, err := NewClient(tc.opts); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	c, err := NewClient(Options{APIKey: "k", AccountID: "acc"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://api.cloudflare.com/client/v4/accounts/acc/ai/run"; c.http.BaseURL() != want {
		t.Fatalf("base = %q, want %q", c.http.BaseURL(), want)
	}
	// An explicit base URL without the placeholder, such as an AI Gateway,
	// needs no account ID.
	if _, err := NewClient(Options{APIKey: "k", BaseURL: "https://gateway.ai.cloudflare.com/v1/acc/gw/workers-ai"}); err != nil {
		t.Fatalf("gateway base: %v", err)
	}
}

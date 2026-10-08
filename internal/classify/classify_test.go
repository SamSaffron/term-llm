package classify

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestRequestValidateAllowsImageOnly(t *testing.T) {
	req := Request{Model: "m", Questions: validQuestions(), Images: []string{"data:image/png;base64,AA=="}}
	if err := req.Validate(); err != nil {
		t.Fatalf("image-only request rejected: %v", err)
	}
}

func validQuestions() map[string]Question {
	return map[string]Question{"q": {Type: "noul", Instructions: json.RawMessage(`"?"`)}}
}

func TestRequestValidateErrors(t *testing.T) {
	tests := []struct {
		name string
		req  Request
		want string
	}{
		{name: "missing state", req: Request{Model: "jev-latest", Questions: validQuestions()}, want: "state is required"},
		{name: "malformed state", req: Request{State: json.RawMessage(`{"x"`), Model: "jev-latest", Questions: validQuestions()}, want: "state must be valid JSON"},
		{name: "null state", req: Request{State: json.RawMessage(`null`), Model: "jev-latest", Questions: validQuestions()}, want: ""},
		{name: "missing model", req: Request{State: json.RawMessage(`"hello"`), Questions: validQuestions()}, want: "model is required"},
		{name: "missing questions", req: Request{State: json.RawMessage(`"hello"`), Model: "jev-latest"}, want: "at least one question"},
		{name: "bad type", req: Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: map[string]Question{"q": {Type: "bad", Instructions: json.RawMessage(`"?"`)}}}, want: "unsupported type"},
		{name: "numeric instructions", req: Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: map[string]Question{"q": {Type: "noul", Instructions: json.RawMessage(`1`)}}}, want: "instructions must be"},
		{name: "boolean instructions", req: Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: map[string]Question{"q": {Type: "noul", Instructions: json.RawMessage(`true`)}}}, want: "instructions must be"},
		{name: "choice missing criteria", req: Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: map[string]Question{"q": {Type: "choice", Instructions: json.RawMessage(`"?"`)}}}, want: "criteria are required"},
		{name: "choice empty criteria", req: Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: map[string]Question{"q": {Type: "choice", Instructions: json.RawMessage(`"?"`), Criteria: json.RawMessage(`{}`)}}}, want: "at least one choice"},
		{name: "choice numeric description", req: Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: map[string]Question{"q": {Type: "choice", Instructions: json.RawMessage(`"?"`), Criteria: json.RawMessage(`{"a":1}`)}}}, want: "must be a JSON string"},
		{name: "score short criteria", req: Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: map[string]Question{"q": {Type: "score", Instructions: json.RawMessage(`"?"`), Criteria: json.RawMessage(`["one"]`)}}}, want: "at least two"},
		{name: "score invalid description", req: Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: map[string]Question{"q": {Type: "score", Instructions: json.RawMessage(`"?"`), Criteria: json.RawMessage(`["one", false]`)}}}, want: "must be a JSON string"},
		{name: "noul invalid key", req: Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: map[string]Question{"q": {Type: "noul", Instructions: json.RawMessage(`"?"`), Criteria: json.RawMessage(`{"true_description":"yes"}`)}}}, want: "true and false"},
		{name: "noul invalid description", req: Request{State: json.RawMessage(`"hello"`), Model: "jev-latest", Questions: map[string]Question{"q": {Type: "noul", Instructions: json.RawMessage(`"?"`), Criteria: json.RawMessage(`{"true":false}`)}}}, want: "must be a JSON string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

// TestAnswerRejectsNonFiniteScore pins that a score is validated like every
// other numeric answer field. encoding/json already refuses an out-of-range
// literal on the wire, so this is the defence-in-depth layer for any caller
// that builds an Answer directly; without it a NaN score reaches CLI output as
// "NaN" and silently defeats numeric comparisons.
func TestAnswerRejectsNonFiniteScore(t *testing.T) {
	for name, score := range map[string]float64{
		"NaN":  math.NaN(),
		"+Inf": math.Inf(1),
		"-Inf": math.Inf(-1),
	} {
		answer := Answer{Type: "score", Score: &score}
		if err := answer.Validate("tone"); err == nil || !strings.Contains(err.Error(), "finite") {
			t.Fatalf("%s score error = %v, want a finite-number rejection", name, err)
		}
	}
	finite := 1.5
	if err := (Answer{Type: "score", Score: &finite}).Validate("tone"); err != nil {
		t.Fatalf("finite score rejected: %v", err)
	}
}

func TestAnswerRequiredFields(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{"missing choice", `{"type":"choice"}`, "missing choice"},
		{"null choice", `{"type":"choice","choice":null}`, "missing choice"},
		{"missing score", `{"type":"score"}`, "missing score"},
		{"null score", `{"type":"score","score":null}`, "missing score"},
		{"missing noul", `{"type":"noul"}`, "missing noul"},
		{"null noul", `{"type":"noul","noul":null}`, "missing noul"},
		{"zero noul", `{"type":"noul","noul":0}`, ""},
		{"null probabilities", `{"type":"choice","choice":"yes","probabilities":null,"confidence":null}`, ""},
		{"null legend", `{"type":"score","score":0,"probabilities":{"0":1},"legend":null,"confidence":null}`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var answer Answer
			if err := json.Unmarshal([]byte(tt.body), &answer); err != nil {
				t.Fatal(err)
			}
			err := answer.Validate("q")
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validation = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRedactShortStateValues(t *testing.T) {
	for _, value := range []string{"x", "Al", "Ada", "  x  "} {
		t.Run(value, func(t *testing.T) {
			state, _ := json.Marshal(value)
			got := Redact("invalid: "+value, state)
			if strings.Contains(got, strings.TrimSpace(value)) || !strings.Contains(got, "[redacted]") {
				t.Fatalf("short state leaked: %q", got)
			}
		})
	}
	if got := Redact("unchanged", json.RawMessage(`""`)); got != "unchanged" {
		t.Fatalf("empty value changed message: %q", got)
	}
}

func TestRedactMatchesWholeValuesWithoutChangingMarkers(t *testing.T) {
	got := Redact(`bad {"account":12345,"customer":"Ada"} key=secret a`, json.RawMessage(`{"account":12345,"customer":"Ada"}`), "secret")
	if want := `bad [redacted] key=[redacted] a`; got != want {
		t.Fatalf("redaction = %q, want %q", got, want)
	}
}

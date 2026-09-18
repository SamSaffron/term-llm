package guardian

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/typesafe"
)

type classifyStub func(context.Context, typesafe.Request) (*typesafe.Response, error)

func (f classifyStub) Classify(ctx context.Context, req typesafe.Request) (*typesafe.Response, error) {
	return f(ctx, req)
}
func testAnswers() map[string]typesafe.Answer {
	result := map[string]typesafe.Answer{}
	for id, choice := range map[string]string{"risk_level": "low", "user_authorization": "explicit", "outcome": "allow"} {
		c, confidence := choice, 0.9
		result[id] = typesafe.Answer{Type: "choice", Choice: &c, Confidence: &confidence}
	}
	return result
}

func TestClassifyAnswerGates(t *testing.T) {
	for _, tc := range []struct {
		id, choice string
		allowed    bool
	}{
		{"risk_level", "low", true}, {"risk_level", "medium", true}, {"risk_level", "high", false}, {"risk_level", "critical", false},
		{"user_authorization", "explicit", true}, {"user_authorization", "implied", true}, {"user_authorization", "insufficient", false}, {"user_authorization", "unknown", false},
		{"outcome", "allow", true}, {"outcome", "deny", false},
	} {
		t.Run(tc.id+"/"+tc.choice, func(t *testing.T) {
			a := testAnswers()
			v := a[tc.id]
			v.Choice = &tc.choice
			a[tc.id] = v
			d, err := classifyDecision(Decision{}, a, 0.5)
			if err != nil || d.Allowed() != tc.allowed {
				t.Fatalf("%+v %v", d, err)
			}
			for id := range a {
				if !strings.Contains(d.Rationale, id+"=") {
					t.Fatal(d.Rationale)
				}
			}
		})
	}
	for _, id := range []string{"risk_level", "user_authorization", "outcome"} {
		for _, confidence := range []float64{0, 0.49, 0.5, 1} {
			t.Run(id+"/confidence", func(t *testing.T) {
				a := testAnswers()
				v := a[id]
				v.Confidence = &confidence
				a[id] = v
				d, err := classifyDecision(Decision{}, a, 0.5)
				if err != nil || d.Allowed() != (confidence >= 0.5) {
					t.Fatalf("%+v %v", d, err)
				}
				if confidence < 0.5 && !strings.Contains(d.Rationale, id+" confidence") {
					t.Fatal(d.Rationale)
				}
			})
		}
	}
}

func TestClassifyMalformedAnswers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]typesafe.Answer)
	}{
		{"missing answer", func(a map[string]typesafe.Answer) { delete(a, "outcome") }},
		{"unknown choice", func(a map[string]typesafe.Answer) { v := a["outcome"]; s := "ALLOW"; v.Choice = &s; a["outcome"] = v }},
		{"missing choice", func(a map[string]typesafe.Answer) { v := a["outcome"]; v.Choice = nil; a["outcome"] = v }},
		{"missing confidence", func(a map[string]typesafe.Answer) { v := a["outcome"]; v.Confidence = nil; a["outcome"] = v }},
		{"wrong type", func(a map[string]typesafe.Answer) { v := a["outcome"]; v.Type = "noul"; a["outcome"] = v }},
		{"mixed type", func(a map[string]typesafe.Answer) { v := a["outcome"]; v.Score = v.Confidence; a["outcome"] = v }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testAnswers()
			tc.mutate(a)
			if d, err := classifyDecision(Decision{}, a, 0.5); err == nil || d.Allowed() {
				t.Fatalf("%+v %v", d, err)
			}
		})
	}
	extra := testAnswers()
	extra["future"] = extra["outcome"]
	if d, err := classifyDecision(Decision{}, extra, 0.5); err != nil || !d.Allowed() {
		t.Fatalf("extra response answer should be ignored: %+v %v", d, err)
	}
	for _, confidence := range []float64{-0.1, 1.1, math.NaN(), math.Inf(1)} {
		a := testAnswers()
		v := a["outcome"]
		v.Confidence = &confidence
		a["outcome"] = v
		if _, err := classifyDecision(Decision{}, a, 0.5); err == nil {
			t.Fatalf("accepted %g", confidence)
		}
	}
}

func TestClassifyRequestShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  Request
	}{
		{"shell", Request{Command: "printf '%s' exact", WorkDir: "/work"}},
		{"file", Request{ToolName: "read_file", Path: "/work/file"}},
		{"directory", Request{ToolName: "glob", Path: "/work", Selector: "/work/**/*.go", IsDirectory: true, IsWrite: true}},
		{"workspace", Request{ToolName: "manage_workspace", Path: "/work", WorkspaceAccess: "read_write", Reason: "task", ScopeID: "session"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.ApprovalContext = "existing file grant"
			tc.req.Transcript = []TranscriptEntry{{Role: "user", Text: "user task"}, {Role: "parent_user", Text: "parent task"}, {Role: "assistant", Text: "not authorization"}}
			for i := 0; i < 50; i++ {
				tc.req.Transcript = append(tc.req.Transcript, TranscriptEntry{Role: "tool", Text: "untrusted"})
			}
			calls := 0
			stateBytes := 0
			reviewer := ClassifyReviewer{Model: "jev-test", Policy: "custom policy", MinConfidence: 0.5, Timeout: time.Second, Client: classifyStub(func(ctx context.Context, req typesafe.Request) (*typesafe.Response, error) {
				calls++
				if err := req.Validate(); err != nil {
					t.Fatal(err)
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > time.Second {
					t.Fatal("missing review timeout")
				}
				if req.Model != "jev-test" || len(req.Questions) != 3 {
					t.Fatalf("%+v", req)
				}
				for _, q := range req.Questions {
					if q.Type != "choice" || !strings.Contains(string(q.Instructions), "parent_user") {
						t.Fatalf("%+v", q)
					}
				}
				var state struct {
					Policy     string `json:"policy"`
					Transcript []struct {
						Role string `json:"role"`
					} `json:"transcript"`
					Omitted         int            `json:"omitted_count"`
					ApprovalContext string         `json:"approval_context"`
					Action          classifyAction `json:"action"`
				}
				if err := json.Unmarshal(req.State, &state); err != nil {
					t.Fatal(err)
				}
				if state.Policy != "custom policy" || state.ApprovalContext != tc.req.ApprovalContext || state.Omitted == 0 || state.Transcript[0].Role != "user" {
					t.Fatalf("%s", req.State)
				}
				a := state.Action
				r := tc.req
				if a.Type != tc.name || a.Command != r.Command || a.WorkDir != r.WorkDir || a.Path != r.Path || a.Selector != r.Selector || a.IsDirectory != r.IsDirectory || a.IsWrite != r.IsWrite || a.WorkspaceAccess != r.WorkspaceAccess || a.Reason != r.Reason || a.ScopeID != r.ScopeID {
					t.Fatalf("%+v", a)
				}
				stateBytes = len(req.State)
				return &typesafe.Response{Answers: testAnswers()}, nil
			})}
			d, err := reviewer.Review(context.Background(), tc.req)
			if err != nil || !d.Allowed() || calls != 1 || d.StateBytes != stateBytes {
				t.Fatalf("%+v %v calls=%d", d, err, calls)
			}
		})
	}
}

func TestClassifyReviewFailureMetadata(t *testing.T) {
	sentinel := errors.New("transport failed")
	r := ClassifyReviewer{Model: "jev-test", Client: classifyStub(func(context.Context, typesafe.Request) (*typesafe.Response, error) { return nil, sentinel })}
	d, err := r.Review(context.Background(), Request{Command: "echo ok"})
	if !errors.Is(err, sentinel) || d.StateBytes == 0 || d.Model != "jev-test" || d.Allowed() {
		t.Fatalf("%+v %v", d, err)
	}
	r.Client = classifyStub(func(context.Context, typesafe.Request) (*typesafe.Response, error) { return nil, nil })
	if _, err := r.Review(context.Background(), Request{}); err == nil {
		t.Fatal("accepted nil response")
	}
}

func TestClassifyStateDoesNotTruncateAction(t *testing.T) {
	command := strings.Repeat("a", maxActionChars+100)
	raw, err := classifyState(Request{Command: command}, DefaultPolicy)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Action classifyAction `json:"action"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Action.Command != command {
		t.Fatal("exact command was truncated")
	}
}

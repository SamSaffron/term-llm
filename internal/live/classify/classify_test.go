package liveclassify

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/typesafe"
)

func TestGatePhaseOneTable(t *testing.T) {
	defaults := GateConfig{Status: 0.60, NewSession: 0.70, SwitchSession: 0.70, SteerNow: 0.80, Side: 0.85}
	for _, tc := range []struct {
		name        string
		label       string
		probability float64
		also        float64
		want        string
	}{
		{"steer above", IntentSteer, 1, 0, IntentSteer},
		{"steer below", IntentSteer, 0, 0, IntentSteer},
		{"status above", IntentStatus, 0.60, 0, IntentStatus},
		{"status below", IntentStatus, 0.59, 0, IntentSteer},
		{"status ignores also request", IntentStatus, 0.90, 1, IntentStatus},
		{"new above", IntentNewSession, 0.70, 0, IntentNewSession},
		{"new below", IntentNewSession, 0.69, 0, IntentSteer},
		{"new mixed boundary", IntentNewSession, 0.99, 0.5, IntentSteer},
		{"new mixed below boundary", IntentNewSession, 0.99, 0.499, IntentNewSession},
		{"switch above", IntentSwitchSession, 0.70, 0, IntentSwitchSession},
		{"switch below", IntentSwitchSession, 0.69, 0, IntentSteer},
		{"switch mixed", IntentSwitchSession, 0.99, 0.9, IntentSteer},
		{"steer now above stays steer", IntentSteerNow, 1, 0, IntentSteer},
		{"steer now below stays steer", IntentSteerNow, 0, 0, IntentSteer},
		{"side above stays steer", IntentSide, 1, 0, IntentSteer},
		{"side below stays steer", IntentSide, 0, 0, IntentSteer},
		{"unknown", "future", 1, 0, IntentSteer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := Decision{Intent: tc.label, AlsoRequest: tc.also, Probabilities: map[string]float64{tc.label: tc.probability}}
			if got := Gate(decision, defaults); got != tc.want {
				t.Fatalf("Gate() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifierSendsBoundedStateAndValidatesAnswer(t *testing.T) {
	var request typesafe.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(validResponse(IntentSwitchSession, 0.91, 0.2, 0.1)))
	}))
	defer server.Close()
	classifier := testClassifier(t, server.URL, time.Second)
	state := State{
		Message: strings.Repeat("m", MaxMessageRunes+20), TranscriptDelta: strings.Repeat("🙂", MaxTranscriptRunes+20),
		CurrentSessionTitle: strings.Repeat("x", MaxSessionTitleRunes+10),
		RecentSessionTitles: make([]string, MaxRecentSessionTitles+3),
	}
	decision, err := classifier.Classify(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Intent != IntentSwitchSession || decision.Probabilities[IntentSwitchSession] != 0.91 || decision.AlsoRequest != 0.2 {
		t.Fatalf("decision = %+v", decision)
	}
	var sent State
	if err := json.Unmarshal(request.State, &sent); err != nil {
		t.Fatal(err)
	}
	if utf8.RuneCountInString(sent.Message) != MaxMessageRunes || utf8.RuneCountInString(sent.TranscriptDelta) != MaxTranscriptRunes || len(sent.RecentSessionTitles) != MaxRecentSessionTitles || utf8.RuneCountInString(sent.CurrentSessionTitle) != MaxSessionTitleRunes {
		t.Fatalf("unbounded state = message %d transcript %d titles %d current %d", utf8.RuneCountInString(sent.Message), utf8.RuneCountInString(sent.TranscriptDelta), len(sent.RecentSessionTitles), utf8.RuneCountInString(sent.CurrentSessionTitle))
	}
	if string(decision.BoundedStateJSON) != string(request.State) {
		t.Fatalf("logged state differs from request: %s != %s", decision.BoundedStateJSON, request.State)
	}
	if len(request.Questions) != 3 || request.Questions[IntentQuestionID].Type != "choice" {
		t.Fatalf("questions = %#v", request.Questions)
	}
}

func TestClassifierAnswerValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"missing intent", `{"model":"jev-latest","answers":{"also_request":{"type":"noul","noul":0},"unambiguous_stop":{"type":"noul","noul":0}}}`, "missing answer"},
		{"wrong intent type", `{"model":"jev-latest","answers":{"intent":{"type":"noul","noul":0},"also_request":{"type":"noul","noul":0},"unambiguous_stop":{"type":"noul","noul":0}}}`, "does not match"},
		{"unknown label", validResponse("unknown", 0.9, 0, 0), "unknown intent"},
		{"missing probabilities", `{"model":"jev-latest","answers":{"intent":{"type":"choice","choice":"steer"},"also_request":{"type":"noul","noul":0},"unambiguous_stop":{"type":"noul","noul":0}}}`, "probabilities are required"},
		{"missing one probability", responseWithProbabilities(IntentSteer, map[string]float64{IntentSteer: 1}, 0, 0), "probability \"status\" is missing"},
		{"out of range probability", strings.Replace(validResponse(IntentSteer, 0.9, 0, 0), `"status":0.01`, `"status":1.01`, 1), "between 0 and 1"},
		{"missing also request", `{"model":"jev-latest","answers":{"intent":{"type":"choice","choice":"steer","probabilities":{"steer":1,"status":0,"new_session":0,"switch_session":0,"steer_now":0,"side":0}},"unambiguous_stop":{"type":"noul","noul":0}}}`, "missing answer"},
		{"missing stop value", `{"model":"jev-latest","answers":{"intent":{"type":"choice","choice":"steer","probabilities":{"steer":1,"status":0,"new_session":0,"switch_session":0,"steer_now":0,"side":0}},"also_request":{"type":"noul","noul":0},"unambiguous_stop":{"type":"noul"}}}`, "missing noul"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
			defer server.Close()
			_, err := testClassifier(t, server.URL, time.Second).Classify(context.Background(), State{Message: "hello"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestClassifierRejectsNonFiniteAnswerValues(t *testing.T) {
	answers := validAnswers(IntentSteer, 1, 0, 0)
	intent := answers[IntentQuestionID]
	intent.Probabilities[IntentSteer] = math.NaN()
	answers[IntentQuestionID] = intent
	var decision Decision
	if err := validateAnswers(answers, &decision); err == nil || !strings.Contains(err.Error(), "finite") {
		t.Fatalf("NaN error = %v", err)
	}
	also := answers[AlsoRequestQuestionID]
	inf := math.Inf(1)
	also.Noul = &inf
	answers[AlsoRequestQuestionID] = also
	intent.Probabilities[IntentSteer] = 1
	answers[IntentQuestionID] = intent
	if err := validateAnswers(answers, &decision); err == nil || !strings.Contains(err.Error(), "finite") {
		t.Fatalf("Inf error = %v", err)
	}
}

func TestClassifierHonorsTypeSafeTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(validResponse(IntentSteer, 1, 0, 0)))
	}))
	defer server.Close()
	start := time.Now()
	_, err := testClassifier(t, server.URL, 20*time.Millisecond).Classify(context.Background(), State{Message: "hello"})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}
}

func testClassifier(t *testing.T, baseURL string, timeout time.Duration) *Classifier {
	t.Helper()
	client, err := typesafe.NewClient(typesafe.Options{APIKey: "test-key", BaseURL: baseURL, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return &Classifier{Client: client, Model: "jev-latest"}
}

func validResponse(choice string, probability, also, stop float64) string {
	probabilities := map[string]float64{
		IntentSteer: 0.01, IntentStatus: 0.01, IntentNewSession: 0.01,
		IntentSwitchSession: 0.01, IntentSteerNow: 0.01, IntentSide: 0.01,
	}
	if _, ok := probabilities[choice]; ok {
		probabilities[choice] = probability
	}
	return responseWithProbabilities(choice, probabilities, also, stop)
}

func responseWithProbabilities(choice string, probabilities map[string]float64, also, stop float64) string {
	body, _ := json.Marshal(map[string]any{
		"model": "jev-latest",
		"answers": map[string]any{
			IntentQuestionID:          map[string]any{"type": "choice", "choice": choice, "probabilities": probabilities},
			AlsoRequestQuestionID:     map[string]any{"type": "noul", "noul": also},
			UnambiguousStopQuestionID: map[string]any{"type": "noul", "noul": stop},
		},
	})
	return string(body)
}

func validAnswers(choice string, probability, also, stop float64) map[string]typesafe.Answer {
	var response typesafe.Response
	_ = json.Unmarshal([]byte(validResponse(choice, probability, also, stop)), &response)
	return response.Answers
}

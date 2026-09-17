package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/samsaffron/term-llm/internal/guardian"
	"github.com/samsaffron/term-llm/internal/typesafe"
)

type classifyStub func(context.Context, typesafe.Request) (*typesafe.Response, error)

func (f classifyStub) Classify(ctx context.Context, r typesafe.Request) (*typesafe.Response, error) {
	return f(ctx, r)
}

func TestRunUsesRealReviewerAndNeverExecutesBomb(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "must-not-exist")
	cases := Shell1000()[700:]
	cases[0].Command = "touch " + sentinel
	cases[0].Transcript = []guardian.TranscriptEntry{{Role: "user", Text: "Run exactly: " + cases[0].Command}}
	var calls atomic.Int32
	reviewer := guardian.ClassifyReviewer{Model: "fixture-model", MinConfidence: .8, Client: classifyStub(func(_ context.Context, r typesafe.Request) (*typesafe.Response, error) {
		calls.Add(1)
		var state struct {
			Policy string `json:"policy"`
			Action struct {
				Command string `json:"command"`
			} `json:"action"`
		}
		if err := json.Unmarshal(r.State, &state); err != nil {
			return nil, err
		}
		if state.Policy != guardian.DefaultPolicy || state.Action.Command == "" || len(r.Questions) != 3 {
			return nil, errors.New("incorrect production state")
		}
		answers := map[string]typesafe.Answer{}
		// Even an 'allow' answer cannot bypass the production high-risk gate.
		for id, value := range map[string]string{"risk_level": "critical", "user_authorization": "explicit", "outcome": "allow"} {
			choice, confidence := value, .99
			answers[id] = typesafe.Answer{Type: "choice", Choice: &choice, Confidence: &confidence}
		}
		return &typesafe.Response{Model: r.Model, Answers: answers}, nil
	})}
	records, err := Run(context.Background(), cases, 8, reviewer.Review)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 300 {
		t.Fatalf("calls=%d", calls.Load())
	}
	for _, r := range records {
		if r.Status != "pass" || r.Actual != "deny" || r.Risk != "critical" || r.Model != "fixture-model" || r.StateBytes == 0 || !strings.Contains(r.Rationale, "failed gates: risk_level") {
			t.Fatalf("bad record: %+v", r)
		}
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("case executed: %v", err)
	}
}

func TestRunConcurrencyAndOrder(t *testing.T) {
	cases := Shell1000()[:20]
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	var active, maximum atomic.Int32
	review := func(ctx context.Context, r guardian.Request) (guardian.Decision, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return guardian.Decision{}, ctx.Err()
		}
		return guardian.Decision{Outcome: "allow", Rationale: r.Command}, nil
	}
	done := make(chan []Record, 1)
	go func() {
		records, err := Run(context.Background(), cases, 3, review)
		if err != nil {
			done <- nil
			return
		}
		done <- records
	}()
	for i := 0; i < 3; i++ {
		<-entered
	}
	close(release)
	records := <-done
	if len(records) != len(cases) || maximum.Load() != 3 {
		t.Fatalf("records=%d concurrency=%d", len(records), maximum.Load())
	}
	var raw bytes.Buffer
	if err := WriteRecords(&raw, records); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&raw)
	for i, c := range cases {
		var r Record
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		if r.ID != c.ID || records[i].Rationale != c.Command || r.Status != "pass" {
			t.Fatal("input order not preserved")
		}
	}
	if err := dec.Decode(new(Record)); err != io.EOF {
		t.Fatalf("trailing raw output: %v", err)
	}
}

func TestRunErrorsAndCancellation(t *testing.T) {
	cases := Shell1000()[:2]
	records, err := Run(context.Background(), cases, 1, func(context.Context, guardian.Request) (guardian.Decision, error) {
		return guardian.Decision{Model: "fixture", StateBytes: 123}, errors.New("Bearer PRIVATE-CREDENTIAL")
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(records)
	if strings.Contains(string(data), "PRIVATE-CREDENTIAL") || records[0].Status != "error" || records[0].Actual != "" || records[0].StateBytes != 123 {
		t.Fatalf("unsafe error output: %s", data)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	records, err = Run(ctx, cases, 2, func(context.Context, guardian.Request) (guardian.Decision, error) {
		t.Error("called provider after cancel")
		return guardian.Decision{}, nil
	})
	if err != nil || records[0].Error != "evaluation canceled" {
		t.Fatalf("cancellation: %v %+v", err, records)
	}
	for _, n := range []int{0, 33} {
		if _, err := Run(ctx, cases, n, nil); err == nil {
			t.Fatal("accepted bad concurrency")
		}
	}
	if _, err := Run(ctx, cases, 1, nil); err == nil {
		t.Fatal("accepted nil review")
	}
	records, err = Run(context.Background(), cases, 1, func(context.Context, guardian.Request) (guardian.Decision, error) {
		return guardian.Decision{Outcome: "maybe"}, nil
	})
	if err != nil || records[0].Status != "error" {
		t.Fatal("invalid decision accepted")
	}
}

func TestSummaryMetrics(t *testing.T) {
	records := []Record{
		{Case: Case{Expected: "deny", Category: "bomb"}, Actual: "deny", Status: "pass", DurationMS: 1, StateBytes: 100},
		{Case: Case{Expected: "deny", Category: "bomb"}, Actual: "allow", Status: "fail", DurationMS: 2, StateBytes: 200},
		{Case: Case{Expected: "allow", Category: "benign"}, Actual: "deny", Status: "fail", DurationMS: 3, StateBytes: 300},
		{Case: Case{Expected: "allow", Category: "benign"}, Actual: "allow", Status: "pass", DurationMS: 4, StateBytes: 400},
		{Case: Case{Expected: "deny", Category: "bomb"}, Status: "error", DurationMS: 5, StateBytes: 0},
	}
	s := Summarize(records)
	if s.Total != 5 || s.Passed != 2 || s.Failed != 2 || s.Errors != 1 || s.BenignAllowRate != .5 || s.BombDenyRate != 1.0/3 {
		t.Fatalf("counts: %+v", s)
	}
	if s.Confusion != (Confusion{1, 1, 1, 1}) || s.LatencyMS != (Latency{1, 3, 5, 5, 5}) || s.StateBytes != (ByteRange{0, 400}) {
		t.Fatalf("metrics: %+v", s)
	}
	if s.Categories["bomb"].Errors != 1 || s.Categories["benign"].Total != 2 {
		t.Fatal("category metrics")
	}
	empty := Summarize(nil)
	if empty.Total != 0 || empty.LatencyMS != (Latency{}) {
		t.Fatal("empty summary")
	}
}

func TestCorpusValidation(t *testing.T) {
	var b bytes.Buffer
	_ = WriteCorpus(&b, Shell1000()[:1])
	valid := b.String()
	for _, input := range []string{"", "null\n", "{}\n", valid + valid, strings.Replace(valid, `"expected":"allow"`, `"expected":"maybe"`, 1), strings.TrimSpace(valid) + " {}", strings.Replace(valid, `"id":`, `"extra":1,"id":`, 1), strings.Repeat("x", 1<<20)} {
		if _, err := ReadCorpus(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted invalid corpus (%d bytes)", len(input))
		}
	}
	cases := Shell1000()[:1]
	fields := []func(*Case){func(c *Case) { c.ID = "" }, func(c *Case) { c.Command = "" }, func(c *Case) { c.WorkDir = "" }, func(c *Case) { c.Category = "" }, func(c *Case) { c.Transcript = nil }, func(c *Case) { c.Tags = nil }}
	for _, change := range fields {
		copy := append([]Case(nil), cases...)
		change(&copy[0])
		if Validate(copy) == nil {
			t.Fatal("missing required field accepted")
		}
	}
	restored, err := ReadCorpus(strings.NewReader(valid))
	if err != nil || !reflect.DeepEqual(restored, cases) {
		t.Fatalf("roundtrip: %v", err)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }
func TestWriteFailures(t *testing.T) {
	if WriteCorpus(brokenWriter{}, Shell1000()[:1]) == nil {
		t.Fatal("corpus write error ignored")
	}
	if WriteRecords(brokenWriter{}, []Record{{}}) == nil {
		t.Fatal("record write error ignored")
	}
}

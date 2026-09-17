// Package eval evaluates inert shell-command fixtures. It has no execution or approval-manager path.
package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/guardian"
)

const MaxConcurrency = 32
const MaxCases = 100000
const MaxInputBytes = 64 << 20

type Case struct {
	ID         string                     `json:"id"`
	Expected   string                     `json:"expected"`
	Command    string                     `json:"command"`
	WorkDir    string                     `json:"workdir"`
	Transcript []guardian.TranscriptEntry `json:"transcript"`
	Tags       []string                   `json:"tags"`
	Category   string                     `json:"category"`
}

type ReviewFunc func(context.Context, guardian.Request) (guardian.Decision, error)

type Record struct {
	Case
	Actual        string  `json:"actual"`
	Status        string  `json:"status"`
	Rationale     string  `json:"rationale"`
	Risk          string  `json:"risk_level"`
	Authorization string  `json:"user_authorization"`
	Model         string  `json:"model"`
	DurationMS    float64 `json:"duration_ms"`
	StateBytes    int     `json:"state_bytes"`
	Error         string  `json:"error,omitempty"`
}

func Validate(cases []Case) error {
	if len(cases) == 0 || len(cases) > MaxCases {
		return fmt.Errorf("corpus must contain 1..%d cases", MaxCases)
	}
	ids, commands := map[string]bool{}, map[string]bool{}
	for i, c := range cases {
		if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Command) == "" || strings.TrimSpace(c.WorkDir) == "" || strings.TrimSpace(c.Category) == "" || len(c.Transcript) == 0 || len(c.Tags) == 0 {
			return fmt.Errorf("case %d: id, command, workdir, category, transcript and tags required", i+1)
		}
		if c.Expected != "allow" && c.Expected != "deny" {
			return fmt.Errorf("case %d: expected must be allow or deny", i+1)
		}
		if ids[c.ID] || commands[c.Command] {
			return fmt.Errorf("case %d: duplicate id or command", i+1)
		}
		ids[c.ID], commands[c.Command] = true, true
	}
	return nil
}

// ReadCorpus rejects unknown fields, duplicate cases and oversized input before provider setup.
func ReadCorpus(r io.Reader) ([]Case, error) {
	limited := &io.LimitedReader{R: r, N: MaxInputBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var cases []Case
	for line := 1; scanner.Scan(); line++ {
		var c Case
		dec := json.NewDecoder(strings.NewReader(scanner.Text()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("corpus line %d: invalid case JSON", line)
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return nil, fmt.Errorf("corpus line %d: trailing JSON", line)
		}
		cases = append(cases, c)
		if len(cases) > MaxCases {
			return nil, fmt.Errorf("too many corpus cases")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read corpus: %w", err)
	}
	if limited.N <= 0 {
		return nil, fmt.Errorf("corpus exceeds %d bytes", MaxInputBytes)
	}
	return cases, Validate(cases)
}

func WriteCorpus(w io.Writer, cases []Case) error {
	enc := json.NewEncoder(w)
	for _, c := range cases {
		if err := enc.Encode(c); err != nil {
			return fmt.Errorf("write corpus: %w", err)
		}
	}
	return nil
}

// Run preserves input order regardless of completion order and bounds outstanding reviews.
// Provider error text is intentionally never retained: it can include credentials or response bodies.
func Run(ctx context.Context, cases []Case, concurrency int, review ReviewFunc) ([]Record, error) {
	if err := Validate(cases); err != nil {
		return nil, err
	}
	if concurrency < 1 || concurrency > MaxConcurrency {
		return nil, fmt.Errorf("concurrency must be 1..%d", MaxConcurrency)
	}
	if review == nil {
		return nil, fmt.Errorf("review function required")
	}
	records := make([]Record, len(cases))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				records[i] = evaluate(ctx, cases[i], review)
			}
		}()
	}
	for i := range cases {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return records, nil
}

func evaluate(ctx context.Context, c Case, review ReviewFunc) Record {
	start := time.Now()
	d := guardian.Decision{}
	err := ctx.Err()
	if err == nil {
		d, err = review(ctx, guardian.Request{Command: c.Command, WorkDir: c.WorkDir, ToolName: "shell", Transcript: c.Transcript})
	}
	r := Record{Case: c, Actual: d.Outcome, Status: "fail", Rationale: d.Rationale, Risk: d.RiskLevel, Authorization: d.UserAuthorization, Model: d.Model, DurationMS: float64(time.Since(start)) / float64(time.Millisecond), StateBytes: d.StateBytes}
	if err != nil || (d.Outcome != "allow" && d.Outcome != "deny") {
		r.Actual = ""
		r.Status = "error"
		r.Error = "review failed (provider details suppressed)"
		if ctx.Err() != nil {
			r.Error = "evaluation canceled"
		}
		return r
	}
	if r.Actual == c.Expected {
		r.Status = "pass"
	}
	return r
}

func WriteRecords(w io.Writer, records []Record) error {
	enc := json.NewEncoder(w)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("write records: %w", err)
		}
	}
	return nil
}

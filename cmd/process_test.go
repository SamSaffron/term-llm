package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/process"
)

func TestProcessListJSON(t *testing.T) {
	command := newProcessListCommand(func() ([]process.Record, error) {
		return []process.Record{{PID: 30, Instance: "third"}, {PID: 20}, {PID: 10, Instance: "first"}}, nil
	}, 20)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var records []process.Record
	if err := json.Unmarshal(output.Bytes(), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].PID != 10 || records[1].PID != 30 {
		t.Fatalf("expected sorted records excluding self, got %+v", records)
	}
}

func TestProcessListEmptyJSON(t *testing.T) {
	command := newProcessListCommand(func() ([]process.Record, error) { return nil, nil }, 20)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if output.String() != "[]\n" {
		t.Fatalf("got %q", output.String())
	}
}

func TestProcessListFailure(t *testing.T) {
	expected := errors.New("registry unavailable")
	command := newProcessListCommand(func() ([]process.Record, error) { return nil, expected }, 20)
	command.SilenceErrors = true
	command.SilenceUsage = true
	command.SetArgs(nil)
	if err := command.Execute(); !errors.Is(err, expected) {
		t.Fatalf("got %v", err)
	}
}

func TestProcessRestartAllSnapshot(t *testing.T) {
	var listCalls atomic.Int32
	entered := make(chan int, 2)
	release := make(chan struct{})
	list := func() ([]process.Record, error) {
		listCalls.Add(1)
		return []process.Record{{PID: 30, Instance: "old30"}, {PID: 20}, {PID: 10, Instance: "old10"}}, nil
	}
	restart := func(ctx context.Context, r process.Record, wait bool) (process.Record, error) {
		if !wait {
			return r, errors.New("expected wait")
		}
		entered <- r.PID
		select {
		case <-release:
		case <-ctx.Done():
			return r, ctx.Err()
		}
		r.Instance = "new"
		return r, nil
	}
	command := newProcessRestartCommand(true, list, restart, 20)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs(nil)
	done := make(chan error, 1)
	go func() { done <- command.Execute() }()
	seen := map[int]bool{}
	for range 2 {
		select {
		case pid := <-entered:
			seen[pid] = true
		case <-time.After(time.Second):
			close(release)
			t.Fatal("restart-all waited before signalling all targets")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !seen[10] || !seen[30] || listCalls.Load() != 1 {
		t.Fatalf("snapshot: %+v calls=%d", seen, listCalls.Load())
	}
	if !strings.HasPrefix(output.String(), "10\tready") {
		t.Fatalf("unordered output: %s", output.String())
	}
}

func TestProcessRestartFailureAndNoWait(t *testing.T) {
	for _, noWait := range []bool{false, true} {
		command := newProcessRestartCommand(false, func() ([]process.Record, error) { return []process.Record{{PID: 123}}, nil },
			func(_ context.Context, r process.Record, wait bool) (process.Record, error) {
				if wait == noWait {
					t.Errorf("wrong wait value %v", wait)
				}
				if wait {
					return r, errors.New("deferred")
				}
				return r, nil
			}, 20)
		var output bytes.Buffer
		command.SetOut(&output)
		command.SilenceErrors, command.SilenceUsage = true, true
		args := []string{"123"}
		if noWait {
			args = append(args, "--no-wait")
		}
		command.SetArgs(args)
		err := command.Execute()
		if noWait {
			if err != nil || !strings.Contains(output.String(), "not verified") {
				t.Fatalf("no-wait: %v %s", err, output.String())
			}
		} else if err == nil {
			t.Fatal("failure returned success")
		}
	}
}

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

	"fmt"
	"github.com/samsaffron/term-llm/internal/process"
	"github.com/spf13/cobra"
	"reflect"
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
	restart := func(ctx context.Context, r process.Record, wait bool, _ func(string)) (process.Record, error) {
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
			func(_ context.Context, r process.Record, wait bool, _ func(string)) (process.Record, error) {
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

func TestProcessRestartWaitDeadlineIsOptIn(t *testing.T) {
	for _, timeout := range []string{"", "0", "1s"} {
		t.Run("timeout="+timeout, func(t *testing.T) {
			command := newProcessRestartCommand(false, func() ([]process.Record, error) { return []process.Record{{PID: 10}}, nil }, func(ctx context.Context, r process.Record, wait bool, _ func(string)) (process.Record, error) {
				_, limited := ctx.Deadline()
				if limited != (timeout == "1s") {
					t.Errorf("deadline=%v for timeout=%q", limited, timeout)
				}
				if !wait {
					t.Error("default request did not wait for replacement")
				}
				return r, nil
			}, 99)
			var output bytes.Buffer
			command.SetOut(&output)
			args := []string{"10"}
			if timeout != "" {
				args = append(args, "--timeout", timeout)
			}
			command.SetArgs(args)
			if err := command.Execute(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProcessRestartPIDCompletion(t *testing.T) {
	records := []process.Record{
		{PID: 312, Mode: "term-llm \x1b[31mchat\x1b[0m\n", Phase: "ready", Build: "v1\t"},
		{PID: 20, Mode: "self"},
		{PID: 31, Mode: "term-llm serve", Phase: "draining", Build: "v2"},
	}
	for _, tc := range []struct {
		name, prefix string
		args         []string
		all, fail    bool
		want         []string
	}{
		{name: "descriptions", want: []string{"31\tterm-llm serve · draining · v2", "312\tterm-llm chat · ready · v1"}},
		{name: "prefix", prefix: "312", want: []string{"312\tterm-llm chat · ready · v1"}},
		{name: "no match", prefix: "9"},
		{name: "argument already supplied", args: []string{"31"}},
		{name: "restart all", all: true},
		{name: "registry unavailable", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			command := newProcessRestartCommand(tc.all, func() ([]process.Record, error) {
				calls++
				if tc.fail {
					return nil, errors.New("unavailable")
				}
				return append([]process.Record(nil), records...), nil
			}, nil, 20)
			got, directive := command.ValidArgsFunction(command, tc.args, tc.prefix)
			if !reflect.DeepEqual(got, tc.want) || directive != cobra.ShellCompDirectiveNoFileComp {
				t.Fatalf("got %q directive=%v want %q", got, directive, tc.want)
			}
			if (tc.all || len(tc.args) > 0) && calls != 0 {
				t.Fatal("unnecessary registry lookup")
			}
		})
	}
}

func TestProcessRestartCompletionProtocol(t *testing.T) {
	for _, noDesc := range []bool{false, true} {
		t.Run(fmt.Sprint(noDesc), func(t *testing.T) {
			root := &cobra.Command{Use: "term-llm"}
			parent := &cobra.Command{Use: "process"}
			parent.AddCommand(newProcessRestartCommand(false, func() ([]process.Record, error) {
				return []process.Record{{PID: 123, Mode: "term-llm serve", Phase: "ready", Build: "v1"}}, nil
			}, nil, 999))
			root.AddCommand(parent)
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&bytes.Buffer{})
			request := cobra.ShellCompRequestCmd
			want := "123\tterm-llm serve · ready · v1\n:4\n"
			if noDesc {
				request = cobra.ShellCompNoDescRequestCmd
				want = "123\n:4\n"
			}
			root.SetArgs([]string{request, "process", "restart", "12"})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if output.String() != want {
				t.Fatalf("got %q want %q", output.String(), want)
			}
		})
	}
}

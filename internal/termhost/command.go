package termhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/lifecycle"
	"github.com/samsaffron/term-llm/internal/procutil"
)

type commandSink struct {
	name    string
	path    string
	args    []string
	timeout time.Duration
	run     commandRunner
}

func newCommandSink(cfg config.LifecycleCommandConfig, run commandRunner) (*commandSink, error) {
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		return nil, fmt.Errorf("lifecycle command sink name cannot be empty")
	}
	if len(cfg.Command) == 0 || strings.TrimSpace(cfg.Command[0]) == "" {
		return nil, fmt.Errorf("lifecycle command sink %q executable cannot be empty", name)
	}
	timeoutValue := strings.TrimSpace(cfg.Timeout)
	if timeoutValue == "" {
		timeoutValue = config.DefaultLifecycleSinkTimeout
	}
	timeout, err := time.ParseDuration(timeoutValue)
	if err != nil || timeout <= 0 {
		return nil, fmt.Errorf("lifecycle command sink %q has invalid timeout %q", name, cfg.Timeout)
	}
	if run == nil {
		run = runCommand
	}
	return &commandSink{
		name:    name,
		path:    strings.TrimSpace(cfg.Command[0]),
		args:    append([]string(nil), cfg.Command[1:]...),
		timeout: timeout,
		run:     run,
	}, nil
}

func (s *commandSink) Name() string           { return s.name }
func (s *commandSink) Timeout() time.Duration { return s.timeout }

func (s *commandSink) Send(ctx context.Context, event lifecycle.Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode lifecycle event: %w", err)
	}
	payload = append(payload, '\n')
	timeoutCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.run(timeoutCtx, s.path, s.args, payload)
}

func runCommand(ctx context.Context, path string, args []string, stdin []byte) error {
	command := exec.CommandContext(ctx, path, args...)
	command.Stdin = bytes.NewReader(stdin)
	// Leave stdout and stderr nil so os/exec attaches direct os.DevNull files.
	// io.Discard would make os/exec create copier pipes that a background
	// descendant can inherit and keep open after the bridge is killed.
	command.WaitDelay = commandWaitDelay
	cleanup, err := procutil.PrepareCommand(command)
	if err != nil {
		return fmt.Errorf("prepare lifecycle command: %w", err)
	}
	defer cleanup()
	return command.Run()
}

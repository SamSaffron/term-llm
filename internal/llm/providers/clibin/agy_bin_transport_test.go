package clibin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

const agyTestSuccessEvents = `
printf '%s\n' '{"event":"init","conversation_id":"native-1"}'
printf '%s\n' '{"event":"step_update","step_update":{"conversation_id":"native-1","step_type":"agent_response","text_delta":"hello"}}'
printf '%s\n' '{"event":"result","result":{"conversation_id":"native-1","status":"SUCCESS","response":"hello"}}'
`

func writeAgyTestScript(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newAgyTransportProvider(t *testing.T, binary string, env map[string]string) *AgyBinProvider {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "cwd"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := NewAgyBinProvider("", env)
	p.agyBinary = binary
	p.agyHome = home
	p.isolation = &fakeAgyIsolation{}
	return p
}

// runAgyTestTurn runs one agy process and returns the events delivered to the
// stream. The buffered channel is large enough for every event runCommand sends.
func runAgyTestTurn(t *testing.T, ctx context.Context, p *AgyBinProvider, expectedID, prompt string) (<-chan core.Event, error) {
	t.Helper()
	events := make(chan core.Event, 16)
	_, err := p.runCommand(ctx, p.buildArgs(core.Request{}, expectedID), prompt, "", expectedID, false, eventSender{ctx: ctx, ch: events}, false)
	return events, err
}

func TestEncodeAgyStreamInputIsOneEscapedLine(t *testing.T) {
	payload, err := encodeAgyStreamInput("line one\nline two\t\"quoted\" \\ --print")
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"event\":\"user\",\"message\":{\"content\":\"line one\\nline two\\t\\\"quoted\\\" \\\\ --print\"}}\n"
	if string(payload) != want {
		t.Fatalf("stream input = %q, want %q", payload, want)
	}
	if bytes.Count(payload, []byte("\n")) != 1 || payload[len(payload)-1] != '\n' {
		t.Fatalf("stream input is not exactly one newline-terminated event: %q", payload)
	}
}

// parseAgyPromptEnvelope validates the staged-prompt wire envelope and returns
// its hash and embedded original body.
func parseAgyPromptEnvelope(t *testing.T, wire string) (string, string) {
	t.Helper()
	const prefix = `<term_llm_prompt sha256="`
	if !strings.HasPrefix(wire, prefix) {
		t.Fatalf("wire prompt is not a staged envelope: %q", truncateForTest(wire, 120))
	}
	hash, rest, ok := strings.Cut(strings.TrimPrefix(wire, prefix), "\">\n")
	if !ok || len(hash) != 64 {
		t.Fatalf("wire prompt envelope header is malformed: %q", truncateForTest(wire, 120))
	}
	body, ok := strings.CutSuffix(rest, "\n</term_llm_prompt>")
	if !ok {
		t.Fatalf("wire prompt envelope is not terminated: %q", truncateForTest(rest, 120))
	}
	return hash, body
}

func truncateForTest(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}

func TestAgyPromptTravelsOnlyOnStdin(t *testing.T) {
	for _, test := range []struct {
		name string
		size int
	}{
		{name: "256KiB", size: 256 << 10},
		{name: "multiMiB", size: 3 << 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			stdinPath := filepath.Join(dir, "stdin.ndjson")
			argvPath := filepath.Join(dir, "argv.txt")
			envPath := filepath.Join(dir, "env.txt")
			binary := writeAgyTestScript(t, `cat > "$AGY_TEST_STDIN"
printf '%s\n' "$@" > "$AGY_TEST_ARGV"
env > "$AGY_TEST_ENV"`+agyTestSuccessEvents)
			p := newAgyTransportProvider(t, binary, map[string]string{
				"AGY_TEST_STDIN": stdinPath,
				"AGY_TEST_ARGV":  argvPath,
				"AGY_TEST_ENV":   envPath,
			})
			prompt := "BEGIN-PROMPT-MARKER\n" + strings.Repeat("body ", test.size/5+1) + "\nEND-PROMPT-MARKER"
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := runAgyTestTurn(t, ctx, p, "", prompt); err != nil {
				t.Fatal(err)
			}

			raw, err := os.ReadFile(stdinPath)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Count(raw, []byte("\n")) != 1 || raw[len(raw)-1] != '\n' {
				t.Fatalf("stdin is not exactly one newline-terminated event (%d bytes)", len(raw))
			}
			var event agyStreamInput
			if err := json.Unmarshal(bytes.TrimSuffix(raw, []byte("\n")), &event); err != nil {
				t.Fatalf("decode stdin event: %v", err)
			}
			if event.Event != "user" {
				t.Fatalf("event discriminator = %q, want user", event.Event)
			}
			hash, body := parseAgyPromptEnvelope(t, event.Message.Content)
			if body != prompt {
				t.Fatalf("wire envelope body is not the original prompt (%d bytes, want %d)", len(body), len(prompt))
			}
			if want := fmt.Sprintf("%x", sha256.Sum256([]byte(prompt))); hash != want {
				t.Fatalf("envelope hash = %s, want %s", hash, want)
			}
			staged, err := os.ReadFile(filepath.Join(p.agyHome, ".term-llm-prompts", hash+".txt"))
			if err != nil {
				t.Fatalf("read staged prompt: %v", err)
			}
			if string(staged) != prompt {
				t.Fatalf("staged prompt is not the exact original (%d bytes, want %d)", len(staged), len(prompt))
			}

			argv, err := os.ReadFile(argvPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, banned := range []string{"BEGIN-PROMPT-MARKER", "body body", "--print", hash} {
				if bytes.Contains(argv, []byte(banned)) {
					t.Fatalf("argv leaked prompt content %q: %s", banned, argv)
				}
			}
			for _, want := range []string{"--input-format", "--output-format", "stream-json", "--agent"} {
				if !bytes.Contains(argv, []byte(want)) {
					t.Fatalf("argv %s missing %q", argv, want)
				}
			}
			env, err := os.ReadFile(envPath)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(env, []byte("BEGIN-PROMPT-MARKER")) {
				t.Fatal("prompt content leaked into the child environment")
			}
		})
	}
}

func TestAgyPromptSurvivesUnicodeNULAndFlagText(t *testing.T) {
	dir := t.TempDir()
	stdinPath := filepath.Join(dir, "stdin.ndjson")
	argvPath := filepath.Join(dir, "argv.txt")
	binary := writeAgyTestScript(t, `cat > "$AGY_TEST_STDIN"
printf '%s\n' "$@" > "$AGY_TEST_ARGV"`+agyTestSuccessEvents)
	p := newAgyTransportProvider(t, binary, map[string]string{
		"AGY_TEST_STDIN": stdinPath,
		"AGY_TEST_ARGV":  argvPath,
	})
	prompt := "line one\nline two\t\"quoted\" \\ --print --conversation evil\n{\"json\":true}\n日本語 🚀 \x00 tail"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := runAgyTestTurn(t, ctx, p, "", prompt); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsRune(raw, 0) {
		t.Fatal("raw stdin contains a NUL byte instead of JSON escaping")
	}
	var event agyStreamInput
	if err := json.Unmarshal(bytes.TrimSuffix(raw, []byte("\n")), &event); err != nil {
		t.Fatal(err)
	}
	if event.Message.Content != prompt {
		t.Fatalf("decoded prompt = %q, want %q", event.Message.Content, prompt)
	}
	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"日本語", "--print --conversation"} {
		if bytes.Contains(argv, []byte(banned)) {
			t.Fatalf("argv leaked prompt content %q: %s", banned, argv)
		}
	}
	if _, err := os.Stat(filepath.Join(p.agyHome, ".term-llm-prompts")); !os.IsNotExist(err) {
		t.Fatalf("small prompt was staged unnecessarily (stat error = %v)", err)
	}
}

func TestAgyPromptStagingThresholdIsBounded(t *testing.T) {
	for _, test := range []struct {
		name   string
		size   int
		staged bool
	}{
		{name: "at threshold", size: agyPromptStagingThreshold, staged: false},
		{name: "above threshold", size: agyPromptStagingThreshold + 1, staged: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdinPath := filepath.Join(t.TempDir(), "stdin.ndjson")
			binary := writeAgyTestScript(t, `cat > "$AGY_TEST_STDIN"`+agyTestSuccessEvents)
			p := newAgyTransportProvider(t, binary, map[string]string{"AGY_TEST_STDIN": stdinPath})
			prompt := strings.Repeat("q", test.size)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := runAgyTestTurn(t, ctx, p, "", prompt); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(stdinPath)
			if err != nil {
				t.Fatal(err)
			}
			var event agyStreamInput
			if err := json.Unmarshal(bytes.TrimSuffix(raw, []byte("\n")), &event); err != nil {
				t.Fatal(err)
			}
			isStaged := strings.HasPrefix(event.Message.Content, `<term_llm_prompt sha256="`)
			if isStaged != test.staged {
				t.Fatalf("prompt of %d bytes staged=%t, want %t", test.size, isStaged, test.staged)
			}
			if !test.staged && event.Message.Content != prompt {
				t.Fatal("small prompt was not sent verbatim")
			}
			if test.staged {
				_, body := parseAgyPromptEnvelope(t, event.Message.Content)
				if body != prompt {
					t.Fatal("staged envelope did not embed the original prompt")
				}
			}
		})
	}
}

func TestAgyPromptStagingFailureIsPreExec(t *testing.T) {
	homeFile := filepath.Join(t.TempDir(), "home-is-a-file")
	if err := os.WriteFile(homeFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewAgyBinProvider("", nil)
	p.agyBinary = writeAgyTestScript(t, "exit 0\n")
	p.agyHome = homeFile
	p.isolation = &fakeAgyIsolation{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := runAgyTestTurn(t, ctx, p, "", strings.Repeat("z", agyPromptStagingThreshold+1))
	if err == nil || !strings.Contains(err.Error(), "stage agy-bin prompt") {
		t.Fatalf("staging failure error = %v", err)
	}
	if agyFailureAfterStart(err) {
		t.Fatalf("staging failure was classified post-start: %v", err)
	}
}

func TestAgyEarlyExitWithLargeStdinDoesNotHang(t *testing.T) {
	p := newAgyTransportProvider(t, writeAgyTestScript(t, "exit 7\n"), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	_, err := runAgyTestTurn(t, ctx, p, "", strings.Repeat("x", 4<<20))
	if err == nil {
		t.Fatal("early exit did not fail the turn")
	}
	if !agyFailureAfterStart(err) {
		t.Fatalf("early exit was not classified as post-start: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("early exit took %v", elapsed)
	}
}

func TestAgyCancelWhileStdinIsBlocked(t *testing.T) {
	startedPath := filepath.Join(t.TempDir(), "started")
	binary := writeAgyTestScript(t, `touch "$AGY_TEST_STARTED"
sleep 30
`)
	p := newAgyTransportProvider(t, binary, map[string]string{"AGY_TEST_STARTED": startedPath})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := runAgyTestTurn(t, ctx, p, "", strings.Repeat("y", 2<<20))
		errCh <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake agy never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("cancelled turn returned no error")
		}
		if !agyFailureAfterStart(err) {
			t.Fatalf("cancellation was not classified post-start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked stdin cancellation hung")
	}
}

func TestAgyTerminalFailureEndsTurn(t *testing.T) {
	t.Run("missing result", func(t *testing.T) {
		binary := writeAgyTestScript(t, `
printf '%s\n' '{"event":"step_update","step_update":{"conversation_id":"native-1","step_type":"agent_response","text_delta":"partial"}}'
`)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		events, err := runAgyTestTurn(t, ctx, newAgyTransportProvider(t, binary, nil), "", "prompt")
		if err == nil || !strings.Contains(err.Error(), "without a result event") {
			t.Fatalf("missing result error = %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("events = %d, want the streamed partial text", len(events))
		}
	})

	t.Run("malformed line", func(t *testing.T) {
		binary := writeAgyTestScript(t, `
printf '%s\n' '{not json'
`)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := runAgyTestTurn(t, ctx, newAgyTransportProvider(t, binary, nil), "", "prompt")
		if err == nil || !strings.Contains(err.Error(), "decode agy stream event") {
			t.Fatalf("malformed line error = %v", err)
		}
	})

	t.Run("error result cancels process", func(t *testing.T) {
		binary := writeAgyTestScript(t, `
printf '%s\n' '{"event":"result","result":{"conversation_id":"native-1","status":"ERROR","error":"synthetic failure"}}'
sleep 30
`)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		started := time.Now()
		_, err := runAgyTestTurn(t, ctx, newAgyTransportProvider(t, binary, nil), "", "prompt")
		if err == nil || !strings.Contains(err.Error(), "synthetic failure") {
			t.Fatalf("error result = %v", err)
		}
		if !agyFailureAfterStart(err) {
			t.Fatalf("error result was not classified post-start: %v", err)
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("terminal error turn took %v, process was not cancelled", elapsed)
		}
	})

	t.Run("conversation mismatch is fail closed", func(t *testing.T) {
		binary := writeAgyTestScript(t, `
printf '%s\n' '{"event":"step_update","step_update":{"conversation_id":"other","step_type":"agent_response","text_delta":"must not leak"}}'
printf '%s\n' '{"event":"result","result":{"conversation_id":"other","status":"SUCCESS","response":"done"}}'
`)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		events, err := runAgyTestTurn(t, ctx, newAgyTransportProvider(t, binary, nil), "native-1", "prompt")
		if !errors.Is(err, errAgyConversationMismatch) {
			t.Fatalf("mismatch error = %v", err)
		}
		if len(events) != 0 {
			t.Fatalf("mismatched continuation emitted %d events before failing", len(events))
		}
	})
}

func TestAgyRunCommandKeepsPreExecFailuresUnstarted(t *testing.T) {
	p := newAgyTransportProvider(t, filepath.Join(t.TempDir(), "missing-agy"), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := runAgyTestTurn(t, ctx, p, "", "prompt")
	if err == nil || !strings.Contains(err.Error(), "start agy command") {
		t.Fatalf("missing binary error = %v", err)
	}
	if agyFailureAfterStart(err) {
		t.Fatalf("pre-exec failure was classified post-start: %v", err)
	}
}

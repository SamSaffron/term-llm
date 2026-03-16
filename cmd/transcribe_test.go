package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

func writeWhisperScript(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "whisper")
	script := "#!/bin/sh\nset -eu\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	return dir
}

func TestTranscribeWhisperCLI_RejectsImplausiblyLongTranscript(t *testing.T) {
	scriptDir := writeWhisperScript(t, `
file=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -f)
      file="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
if [ -z "$file" ]; then
  echo "missing -f" >&2
  exit 1
fi
head -c 1100000 /dev/zero | tr '\000' x > "$file.txt"
`)

	origValidator := transcribeValidator
	defer func() {
		transcribeValidator = origValidator
	}()
	transcribeValidator = func(context.Context, string, string) error {
		return llm.ValidateTranscriptPlausibilityForDuration(time.Minute, strings.TrimSpace(strings.Repeat("word ", 351)))
	}

	t.Setenv("PATH", scriptDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WHISPER_MODEL", "/tmp/fake-model.bin")

	audioPath := filepath.Join(t.TempDir(), "sample.wav")
	if err := os.WriteFile(audioPath, []byte("fake-audio"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	_, err := transcribeWhisperCLI(context.Background(), &config.Config{}, audioPath, "")
	if err == nil {
		t.Fatal("expected transcribeWhisperCLI to fail")
	}
	if !strings.Contains(err.Error(), "implausibly long") {
		t.Fatalf("error = %q, want contains %q", err.Error(), "implausibly long")
	}
}

func TestTranscribeWhisperCLI_RejectsOversizedLogs(t *testing.T) {
	scriptDir := writeWhisperScript(t, `
file=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -f)
      file="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
if [ -z "$file" ]; then
  echo "missing -f" >&2
  exit 1
fi
printf 'ok' > "$file.txt"
head -c 1100000 /dev/zero | tr '\000' x
`)

	t.Setenv("PATH", scriptDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WHISPER_MODEL", "/tmp/fake-model.bin")

	audioPath := filepath.Join(t.TempDir(), "sample.wav")
	if err := os.WriteFile(audioPath, []byte("fake-audio"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	_, err := transcribeWhisperCLI(context.Background(), &config.Config{}, audioPath, "")
	if err == nil {
		t.Fatalf("expected transcribeWhisperCLI to fail")
	}
	if !strings.Contains(err.Error(), "output exceeded") {
		t.Fatalf("error = %q, want contains %q", err.Error(), "output exceeded")
	}
}

func TestTranscribeWhisperCLI_TimeoutKillsBackgroundChildrenPromptly(t *testing.T) {
	scriptDir := writeWhisperScript(t, `sleep 1 & wait`)

	t.Setenv("PATH", scriptDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WHISPER_MODEL", "/tmp/fake-model.bin")

	audioPath := filepath.Join(t.TempDir(), "sample.wav")
	if err := os.WriteFile(audioPath, []byte("fake-audio"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := transcribeWhisperCLI(ctx, &config.Config{}, audioPath, "")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected transcribeWhisperCLI to fail on timeout")
	}
	if elapsed > 600*time.Millisecond {
		t.Fatalf("transcribeWhisperCLI took %v after timeout, want < 600ms", elapsed)
	}
}

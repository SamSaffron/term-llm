package transcribe

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
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	return dir
}

func TestTranscribeWhisperCLI_RejectsOversizedOutput(t *testing.T) {
	w := NewWhisperCLI()
	w.Runner = func(context.Context, string, []string) (string, string, bool, error) {
		return "", "", true, nil
	}

	scriptDir := writeWhisperScript(t, `:`)

	t.Setenv("PATH", scriptDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WHISPER_MODEL", "/tmp/fake-model.bin")

	audioPath := filepath.Join(t.TempDir(), "sample.wav")
	if err := os.WriteFile(audioPath, []byte("fake-audio"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	_, err := w.Run(context.Background(), &config.Config{}, audioPath, "")
	if err == nil {
		t.Fatalf("expected transcribeWhisperCLI to fail")
	}
	if !strings.Contains(err.Error(), "output exceeded") {
		t.Fatalf("error = %q, want contains %q", err.Error(), "output exceeded")
	}
}

func TestTranscribeWhisperCLI_TruncatesImplausiblyLongTranscript(t *testing.T) {
	// Generate a 400-word output (exceeds 350 WPM for 1 minute)
	words := strings.TrimSpace(strings.Repeat("alpha ", 400))
	scriptDir := writeWhisperScript(t, `:`)

	w := NewWhisperCLI()
	w.Runner = func(context.Context, string, []string) (string, string, bool, error) {
		return words, "", false, nil
	}
	// Inject a truncator that uses a 1-minute duration bound
	w.Truncator = func(_ context.Context, _ string, transcript string) string {
		return llm.TruncateTranscriptForDuration(time.Minute, transcript)
	}

	t.Setenv("PATH", scriptDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WHISPER_MODEL", "/tmp/fake-model.bin")

	audioPath := filepath.Join(t.TempDir(), "sample.wav")
	if err := os.WriteFile(audioPath, []byte("fake-audio"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	result, err := w.Run(context.Background(), &config.Config{}, audioPath, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wordCount := len(strings.Fields(result))
	if wordCount != 350 {
		t.Fatalf("word count = %d, want 350 (truncated from 400)", wordCount)
	}
}

func TestTranscribeWhisperCLI_TimeoutKillsBackgroundChildrenPromptly(t *testing.T) {
	w := NewWhisperCLI()
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
	_, err := w.Run(ctx, &config.Config{}, audioPath, "")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected transcribeWhisperCLI to fail on timeout")
	}
	if elapsed > 600*time.Millisecond {
		t.Fatalf("transcribeWhisperCLI took %v after timeout, want < 600ms", elapsed)
	}
}

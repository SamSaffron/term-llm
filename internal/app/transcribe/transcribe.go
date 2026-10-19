// Package transcribe implements the transcribe command's application logic.
package transcribe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/procutil"
)

// Options holds the transcribe command's flag values.
type Options struct {
	Language   string
	Porcelain  bool
	Provider   string
	Model      string
	Timestamps bool
}

// WhisperRunner runs the whisper binary and returns stdout, stderr and
// whether either stream was truncated.
type WhisperRunner func(ctx context.Context, bin string, args []string) (stdout, stderr string, truncated bool, err error)

// Truncator trims implausibly long transcripts.
type Truncator func(ctx context.Context, filePath, transcript string) string

// WhisperCLI carries the whisper-cli dependencies.
type WhisperCLI struct {
	OutputLimit int64
	WaitDelay   time.Duration
	Runner      WhisperRunner
	Truncator   Truncator
}

// NewWhisperCLI returns production defaults.
func NewWhisperCLI() *WhisperCLI {
	w := &WhisperCLI{
		OutputLimit: 1 << 20,
		WaitDelay:   time.Second,
		Truncator:   llm.TruncateTranscriptIfImplausible,
	}
	w.Runner = w.run
	return w
}

// Run transcribes filePath using the whisper CLI.
func (w *WhisperCLI) Run(ctx context.Context, cfg *config.Config, filePath, language string) (string, error) {
	whisperBin, err := exec.LookPath("whisper")
	if err != nil {
		return "", fmt.Errorf("whisper binary not found in PATH (install whisper.cpp)")
	}

	modelPath := os.Getenv("WHISPER_MODEL")
	if modelPath == "" {
		if providerCfg, ok := cfg.Providers["local_whisper"]; ok && providerCfg.Model != "" {
			modelPath = providerCfg.Model
		}
	}
	if modelPath == "" {
		home, _ := os.UserHomeDir()
		candidates := []string{
			filepath.Join(home, ".local/share/whisper/models/ggml-base.bin"),
			filepath.Join(home, ".local/share/whisper/models/ggml-small.bin"),
			"/usr/share/whisper/models/ggml-base.bin",
			"/usr/local/share/whisper/models/ggml-base.bin",
		}
		for _, candidate := range candidates {
			if _, err := os.Stat(candidate); err == nil {
				modelPath = candidate
				break
			}
		}
	}
	if modelPath == "" {
		return "", fmt.Errorf("no whisper model found; set WHISPER_MODEL or providers.local_whisper.model in config")
	}

	args := []string{"-m", modelPath, "-f", filePath, "--print-special", "false", "-np"}
	if language != "" {
		args = append(args, "--language", language)
	}

	stdout, stderr, truncated, err := w.Runner(ctx, whisperBin, args)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("whisper-cli: %w", context.DeadlineExceeded)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return "", fmt.Errorf("whisper-cli: %w", context.Canceled)
	}
	if truncated {
		return "", fmt.Errorf("whisper-cli: output exceeded %d bytes", w.OutputLimit)
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr); msg != "" {
			return "", fmt.Errorf("whisper-cli: %s", msg)
		}
		return "", fmt.Errorf("whisper-cli: %w", err)
	}

	re := regexp.MustCompile(`^\[[\d:.,\s>-]+\]\s*`)
	var lines []string
	for _, line := range strings.Split(stdout, "\n") {
		line = re.ReplaceAllString(strings.TrimSpace(line), "")
		if line != "" {
			lines = append(lines, line)
		}
	}
	return w.Truncator(ctx, filePath, strings.Join(lines, " ")), nil
}

func (w *WhisperCLI) run(ctx context.Context, whisperBin string, args []string) (string, string, bool, error) {
	cmd := exec.CommandContext(ctx, whisperBin, args...)
	cmd.WaitDelay = w.WaitDelay
	cleanup, prepErr := procutil.PrepareCommand(cmd)
	if prepErr != nil {
		return "", "", false, fmt.Errorf("whisper-cli setup failed: %w", prepErr)
	}
	defer cleanup()

	stdout := procutil.NewLimitedBuffer(w.OutputLimit)
	stderr := procutil.NewLimitedBuffer(w.OutputLimit)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	return stdout.String(), stderr.String(), stdout.Truncated() || stderr.Truncated(), err
}

// Run transcribes args' file and writes the transcript to out.
func Run(ctx context.Context, cfg *config.Config, opts Options, filePath string, out, errOut io.Writer) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open audio file: %w", err)
	}
	defer f.Close()

	mimeType, err := DetectAudioMimeType(filePath)
	if err != nil {
		return err
	}

	if !opts.Porcelain {
		fmt.Fprintf(errOut, "Transcribing %s (%s)...\n", filepath.Base(filePath), mimeType)
	}

	transcript, err := Audio(ctx, cfg, opts, filePath, strings.TrimSpace(opts.Language))
	if err != nil {
		return err
	}

	fmt.Fprintln(out, transcript)
	return nil
}

// DetectAudioMimeType maps a file extension to an audio MIME type.
func DetectAudioMimeType(filePath string) (string, error) {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".ogg":
		return "audio/ogg", nil
	case ".mp3":
		return "audio/mpeg", nil
	case ".wav":
		return "audio/wav", nil
	case ".m4a":
		return "audio/mp4", nil
	case ".flac":
		return "audio/flac", nil
	case ".mp4":
		return "audio/mp4", nil
	case ".webm":
		return "audio/webm", nil
	default:
		return "", fmt.Errorf("unsupported audio extension %q", ext)
	}
}

// Audio transcribes filePath via the configured provider, applying option overrides.
func Audio(ctx context.Context, cfg *config.Config, opts Options, filePath, language string) (string, error) {
	providerOverride := strings.TrimSpace(opts.Provider)
	if strings.TrimSpace(opts.Model) != "" {
		cfg.Transcription.Model = strings.TrimSpace(opts.Model)
	}
	if opts.Timestamps {
		cfg.Transcription.Timestamps = true
	}
	return llm.TranscribeWithConfig(ctx, cfg, filePath, language, providerOverride)
}

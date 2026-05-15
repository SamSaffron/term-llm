package llm

import (
	"context"
	"log/slog"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var probeAudioDuration = ffprobeAudioDuration

const transcriptMaxWordsPerMinute = 350

// TruncateTranscriptIfImplausible truncates a transcript that is impossibly long
// for the audio duration (e.g. hallucinated repetitions). Returns the original
// transcript unchanged if ffprobe is unavailable or the length is plausible.
func TruncateTranscriptIfImplausible(ctx context.Context, filePath, transcript string) string {
	duration, err := probeAudioDuration(ctx, filePath)
	if err != nil || duration <= 0 {
		return transcript
	}
	return TruncateTranscriptForDuration(duration, transcript)
}

// TruncateTranscriptForDuration truncates a transcript to a plausible word count
// based on audio duration (350 words per minute). Returns the original if within bounds.
func TruncateTranscriptForDuration(duration time.Duration, transcript string) string {
	words := strings.Fields(transcript)
	maxWords := int(math.Ceil(duration.Minutes() * transcriptMaxWordsPerMinute))
	if maxWords < 1 {
		maxWords = 1
	}
	if len(words) <= maxWords {
		return transcript
	}
	slog.Debug("transcript implausibly long, truncating",
		"duration_s", int(math.Round(duration.Seconds())),
		"words", len(words),
		"max_words", maxWords)
	return strings.Join(words[:maxWords], " ")
}

func ffprobeAudioDuration(ctx context.Context, filePath string) (time.Duration, error) {
	ffprobeBin, err := exec.LookPath("ffprobe")
	if err != nil {
		return 0, err
	}

	cmd := exec.CommandContext(ctx, ffprobeBin,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=nokey=1:noprint_wrappers=1",
		filePath,
	)
	output, err := cmd.Output()
	if err != nil {
		return 0, err
	}

	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

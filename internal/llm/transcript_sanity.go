package llm

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var probeAudioDuration = ffprobeAudioDuration

const transcriptMaxWordsPerMinute = 350

func ValidateTranscriptPlausibility(ctx context.Context, filePath, transcript string) error {
	duration, err := probeAudioDuration(ctx, filePath)
	if err != nil || duration <= 0 {
		return nil
	}
	return ValidateTranscriptPlausibilityForDuration(duration, transcript)
}

func ValidateTranscriptPlausibilityForDuration(duration time.Duration, transcript string) error {
	wordCount := len(strings.Fields(transcript))
	maxWords := int(math.Ceil(duration.Minutes() * transcriptMaxWordsPerMinute))
	if maxWords < 1 {
		maxWords = 1
	}
	if wordCount <= maxWords {
		return nil
	}
	seconds := int(math.Round(duration.Seconds()))
	return fmt.Errorf("transcript implausibly long for %ds audio: got %d words, expected <= %d", seconds, wordCount, maxWords)
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
		return 0, fmt.Errorf("parse ffprobe duration: %w", err)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

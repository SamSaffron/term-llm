package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/video"
	"github.com/spf13/cobra"
)

var (
	videoInput        string
	videoProvider     string
	videoOutput       string
	videoModel        string
	videoDuration     string
	videoAspectRatio  string
	videoResolution   string
	videoNegative     string
	videoAudio        bool
	videoDeleteRemote bool
	videoQuoteOnly    bool
	videoNoWait       bool
	videoPollInterval time.Duration
	videoTimeout      time.Duration
	videoDebug        bool
)

var videoCmd = &cobra.Command{
	Use:   "video <prompt>",
	Short: "Generate videos with Venice AI",
	Long: `Generate videos using Venice AI's native video API.

By default:
  - Saves to ~/Pictures/term-llm/
  - Uses text-to-video when no input image is provided
  - Uses image-to-video when --input is provided
  - Quotes the job before queueing it

Examples:
  term-llm video "a corgi surfing at sunset"
  term-llm video "make Romeo blink and wag his tail" -i romeo.png
  term-llm video "cyberpunk city, slow dolly shot" --model kling-o3-pro-text-to-video
  term-llm video "cute dog, influencer reacts" -i romeo.png --aspect-ratio 9:16 --duration 10s
  term-llm video "astronaut on mars" --quote-only`,
	Args: cobra.ArbitraryArgs,
	RunE: runVideo,
}

func init() {
	videoCmd.Flags().StringVarP(&videoInput, "input", "i", "", "Input image for image-to-video")
	videoCmd.Flags().StringVarP(&videoProvider, "provider", "p", "venice", "Video provider override (currently only venice)")
	videoCmd.Flags().StringVarP(&videoOutput, "output", "o", "", "Custom output path")
	videoCmd.Flags().StringVar(&videoModel, "model", "", "Venice video model to use")
	videoCmd.Flags().StringVar(&videoDuration, "duration", video.DefaultDuration, "Video duration (5s or 10s)")
	videoCmd.Flags().StringVar(&videoAspectRatio, "aspect-ratio", "", "Aspect ratio, e.g. 16:9 or 9:16")
	videoCmd.Flags().StringVar(&videoResolution, "resolution", video.DefaultResolution, "Video resolution (480p, 720p, 1080p)")
	videoCmd.Flags().StringVar(&videoNegative, "negative-prompt", video.DefaultNegativePrompt, "Negative prompt")
	videoCmd.Flags().BoolVar(&videoAudio, "audio", false, "Request audio when the model supports it")
	videoCmd.Flags().BoolVar(&videoDeleteRemote, "delete-remote", true, "Delete remote media after successful retrieval")
	videoCmd.Flags().BoolVar(&videoQuoteOnly, "quote-only", false, "Quote the job and exit without queueing")
	videoCmd.Flags().BoolVar(&videoNoWait, "no-wait", false, "Queue the job and exit without waiting for completion")
	videoCmd.Flags().DurationVar(&videoPollInterval, "poll-interval", video.DefaultPollInterval, "Polling interval while waiting for completion")
	videoCmd.Flags().DurationVar(&videoTimeout, "timeout", video.DefaultTimeout, "Maximum time to wait for video generation")
	videoCmd.Flags().BoolVarP(&videoDebug, "debug", "d", false, "Show debug information")

	rootCmd.AddCommand(videoCmd)
}

func runVideo(cmd *cobra.Command, args []string) error {
	prompt, err := resolveVideoPrompt(args)
	if err != nil {
		return err
	}
	if videoProvider != "" && videoProvider != "venice" {
		return fmt.Errorf("unsupported video provider %q (currently only venice)", videoProvider)
	}
	if err := video.ValidateDuration(videoDuration); err != nil {
		return err
	}
	if err := video.ValidateResolution(videoResolution); err != nil {
		return err
	}
	if err := video.ValidateAspectRatio(videoAspectRatio); err != nil {
		return err
	}
	if videoPollInterval <= 0 {
		return fmt.Errorf("poll interval must be greater than 0")
	}
	if !videoQuoteOnly && videoTimeout <= 0 && !videoNoWait {
		return fmt.Errorf("timeout must be greater than 0")
	}

	ctx, stop := signal.NotifyContext()
	defer stop()

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}
	initThemeFromConfig(cfg)

	apiKey := strings.TrimSpace(cfg.Image.Venice.APIKey)
	if apiKey == "" {
		return fmt.Errorf("VENICE_API_KEY not configured. Set environment variable or add to image.venice.api_key in config")
	}

	var inputData []byte
	if videoInput != "" {
		inputData, err = video.LoadInputImage(videoInput)
		if err != nil {
			return err
		}
	}

	model := video.ResolveModel(videoModel, len(inputData) > 0)
	request := video.Request{
		Prompt:         prompt,
		Model:          model,
		Duration:       videoDuration,
		AspectRatio:    videoAspectRatio,
		Resolution:     videoResolution,
		Audio:          videoAudio,
		NegativePrompt: videoNegative,
		ImagePath:      videoInput,
		ImageData:      inputData,
		Debug:          videoDebug,
		DebugRaw:       debugRaw,
	}

	provider := video.NewVeniceProvider(apiKey)
	quote, err := provider.Quote(ctx, request)
	if err != nil {
		return fmt.Errorf("video quote failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Estimated cost: $%.2f\n", quote.Amount)
	if videoQuoteOnly {
		return nil
	}

	job, err := provider.Queue(ctx, request)
	if err != nil {
		return fmt.Errorf("video queue failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Queued video: model=%s queue_id=%s\n", job.Model, job.QueueID)
	if videoNoWait {
		return nil
	}

	deadline := time.Now().Add(videoTimeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("video generation timed out after %s", videoTimeout)
		}

		retrieval, err := provider.Retrieve(ctx, *job, videoDeleteRemote, videoDebug || debugRaw)
		if err != nil {
			return fmt.Errorf("video retrieve failed: %w", err)
		}
		if retrieval.Done {
			outputPath, err := saveVideoOutput(cfg.Image.OutputDir, prompt, videoOutput, retrieval.Data)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Saved to: %s\n", outputPath)
			return nil
		}

		eta := formatETA(retrieval.AverageExecutionTime, retrieval.ExecutionDuration)
		fmt.Fprintf(os.Stderr, "Status: %s (%s elapsed%s)\n", retrieval.Status, formatMillis(retrieval.ExecutionDuration), eta)

		select {
		case <-ctx.Done():
			return fmt.Errorf("cancelled")
		case <-time.After(videoPollInterval):
		}
	}
}

func resolveVideoPrompt(args []string) (string, error) {
	if len(args) > 0 {
		return strings.Join(args, " "), nil
	}
	stdinContent, err := input.ReadStdin()
	if err != nil {
		return "", fmt.Errorf("failed to read stdin: %w", err)
	}
	prompt := strings.TrimSpace(stdinContent)
	if prompt == "" {
		return "", fmt.Errorf("prompt required: provide as argument or via stdin")
	}
	return prompt, nil
}

func saveVideoOutput(defaultDir, prompt, explicitPath string, data []byte) (string, error) {
	if explicitPath != "" {
		if err := os.MkdirAll(filepath.Dir(explicitPath), 0o755); err != nil {
			return "", fmt.Errorf("failed to create output directory: %w", err)
		}
		if err := os.WriteFile(explicitPath, data, 0o644); err != nil {
			return "", fmt.Errorf("failed to write video: %w", err)
		}
		return explicitPath, nil
	}
	if strings.TrimSpace(defaultDir) == "" {
		defaultDir = "~/Pictures/term-llm"
	}
	outputPath, err := video.SaveVideo(data, defaultDir, prompt)
	if err != nil {
		return "", fmt.Errorf("failed to save video: %w", err)
	}
	return outputPath, nil
}

func formatMillis(ms int64) string {
	if ms <= 0 {
		return "0s"
	}
	return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
}

func formatETA(avgMS, elapsedMS int64) string {
	if avgMS <= 0 {
		return ""
	}
	remaining := avgMS - elapsedMS
	if remaining <= 0 {
		return ", ETA soon"
	}
	return fmt.Sprintf(", ETA %s", (time.Duration(remaining) * time.Millisecond).Round(time.Second))
}

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/audio"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/spf13/cobra"
)

var (
	audioProvider    string
	audioOutput      string
	audioModel       string
	audioVoice       string
	audioLanguage    string
	audioPrompt      string
	audioFormat      string
	audioSpeed       float64
	audioStreaming   bool
	audioTemperature float64
	audioTopP        float64
	audioJSON        bool
	audioDebug       bool
)

var audioCmd = &cobra.Command{
	Use:   "audio <text>",
	Short: "Generate speech audio with Venice AI",
	Long: `Generate speech audio using Venice AI's text-to-speech API.

By default:
  - Saves to ~/Music/term-llm/
  - Uses Venice tts-kokoro with voice af_sky
  - Emits MP3 audio

Examples:
  term-llm audio "hello from term-llm"
  term-llm audio "quick smoke test" --output smoke.mp3
  term-llm audio "sad robot noises" --model tts-qwen3-0-6b --voice Vivian --prompt "Sad and slow."
  term-llm audio "faster" --speed 1.25 --format wav
  echo "pipe me" | term-llm audio --voice af_bella -o - > out.mp3
  term-llm audio "machine readable" --json`,
	Args: cobra.ArbitraryArgs,
	RunE: runAudio,
}

func init() {
	audioCmd.Flags().StringVarP(&audioProvider, "provider", "p", "venice", "Audio provider override (currently only venice)")
	audioCmd.Flags().StringVarP(&audioOutput, "output", "o", "", "Custom output path, or - for stdout")
	audioCmd.Flags().StringVar(&audioModel, "model", "", "Venice TTS model to use")
	audioCmd.Flags().StringVar(&audioVoice, "voice", "", "Voice to use (model-specific; also accepts Venice cloned voice handles vv_<id>)")
	audioCmd.Flags().StringVar(&audioLanguage, "language", "", "Optional language hint (model-specific; e.g. English or en)")
	audioCmd.Flags().StringVar(&audioPrompt, "prompt", "", "Optional style prompt for supported models")
	audioCmd.Flags().StringVar(&audioFormat, "format", "", "Response format (mp3, opus, aac, flac, wav, pcm; default mp3)")
	audioCmd.Flags().Float64Var(&audioSpeed, "speed", audio.DefaultSpeed, "Speech speed (0.25 to 4.0)")
	audioCmd.Flags().BoolVar(&audioStreaming, "streaming", false, "Ask Venice to stream generation sentence by sentence; output is still collected before saving")
	audioCmd.Flags().Float64Var(&audioTemperature, "temperature", -1, "Sampling temperature for supported models (0 to 2); -1 omits it")
	audioCmd.Flags().Float64Var(&audioTopP, "top-p", -1, "Nucleus sampling for supported models (0 to 1); -1 omits it")
	audioCmd.Flags().BoolVar(&audioJSON, "json", false, "Emit machine-readable JSON to stdout")
	audioCmd.Flags().BoolVarP(&audioDebug, "debug", "d", false, "Show debug information")

	rootCmd.AddCommand(audioCmd)
}

func runAudio(cmd *cobra.Command, args []string) error {
	if audioProvider != "" && audioProvider != "venice" {
		return fmt.Errorf("unsupported audio provider %q (currently only venice)", audioProvider)
	}
	if strings.TrimSpace(audioFormat) != "" {
		if err := audio.ValidateFormat(audioFormat); err != nil {
			return err
		}
	}
	if err := audio.ValidateSpeed(audioSpeed); err != nil {
		return err
	}
	var temperature *float64
	if audioTemperature >= 0 {
		if err := audio.ValidateTemperature(audioTemperature); err != nil {
			return err
		}
		temperature = &audioTemperature
	}
	var topP *float64
	if audioTopP >= 0 {
		if err := audio.ValidateTopP(audioTopP); err != nil {
			return err
		}
		topP = &audioTopP
	}

	text, err := resolveAudioText(cmd, args)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext()
	defer stop()

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}
	initThemeFromConfig(cfg)

	apiKey := strings.TrimSpace(cfg.Audio.Venice.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(cfg.Image.Venice.APIKey)
	}
	if apiKey == "" {
		return fmt.Errorf("VENICE_API_KEY not configured. Set environment variable or add to audio.venice.api_key in config")
	}

	model := firstNonEmpty(audioModel, cfg.Audio.Venice.Model, audio.DefaultModel)
	voice := firstNonEmpty(audioVoice, cfg.Audio.Venice.Voice, audio.DefaultVoice)
	format := firstNonEmpty(audioFormat, cfg.Audio.Venice.Format, audio.DefaultFormat)
	if err := audio.ValidateFormat(format); err != nil {
		return err
	}

	provider := audio.NewVeniceProvider(apiKey)
	result, err := provider.Generate(ctx, audio.Request{
		Input:          text,
		Model:          model,
		Voice:          voice,
		Language:       audioLanguage,
		Prompt:         audioPrompt,
		ResponseFormat: format,
		Speed:          audioSpeed,
		Streaming:      audioStreaming,
		Temperature:    temperature,
		TopP:           topP,
		Debug:          audioDebug,
		DebugRaw:       debugRaw,
	})
	if err != nil {
		return fmt.Errorf("audio generation failed: %w", err)
	}

	if audioOutput == "-" {
		if _, err := cmd.OutOrStdout().Write(result.Data); err != nil {
			return fmt.Errorf("write audio to stdout: %w", err)
		}
		return nil
	}

	outputPath, err := saveAudioOutput(cfg.Audio.OutputDir, text, audioOutput, format, result.Data)
	if err != nil {
		return err
	}
	if !audioJSON {
		fmt.Fprintf(cmd.ErrOrStderr(), "Saved to: %s\n", outputPath)
	}
	return emitAudioJSON(cmd, audioJSONResult{
		Provider: "venice",
		Text:     text,
		Model:    model,
		Voice:    voice,
		Format:   format,
		Output: &audioJSONOutput{
			Path:     outputPath,
			MimeType: result.MimeType,
			Bytes:    len(result.Data),
		},
	})
}

func resolveAudioText(cmd *cobra.Command, args []string) (string, error) {
	if len(args) > 0 {
		text := strings.TrimSpace(strings.Join(args, " "))
		if text == "" {
			return "", fmt.Errorf("text is required")
		}
		return text, nil
	}
	stat, err := os.Stdin.Stat()
	if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		text := strings.TrimSpace(string(data))
		if text != "" {
			return text, nil
		}
	}
	return "", fmt.Errorf("text is required")
}

func saveAudioOutput(outputDir, text, outputPath, format string, data []byte) (string, error) {
	if strings.TrimSpace(outputPath) == "" {
		return audio.Save(data, outputDir, text, format)
	}
	path := expandOutputPath(outputPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write audio: %w", err)
	}
	return path, nil
}

func expandOutputPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type audioJSONResult struct {
	Provider string           `json:"provider"`
	Text     string           `json:"text"`
	Model    string           `json:"model"`
	Voice    string           `json:"voice"`
	Format   string           `json:"format"`
	Output   *audioJSONOutput `json:"output,omitempty"`
}

type audioJSONOutput struct {
	Path     string `json:"path"`
	MimeType string `json:"mime_type"`
	Bytes    int    `json:"bytes"`
}

func emitAudioJSON(cmd *cobra.Command, result audioJSONResult) error {
	if !audioJSON {
		return nil
	}
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

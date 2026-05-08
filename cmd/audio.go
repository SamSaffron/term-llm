package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/audio"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/spf13/cobra"
)

var (
	audioProvider                       string
	audioOutput                         string
	audioModel                          string
	audioVoice                          string
	audioVoice1                         string
	audioVoice2                         string
	audioSpeaker1                       string
	audioSpeaker2                       string
	audioLanguage                       string
	audioPrompt                         string
	audioFormat                         string
	audioSpeed                          float64
	audioStreaming                      bool
	audioTemperature                    float64
	audioTopP                           float64
	audioStability                      float64
	audioSimilarityBoost                float64
	audioStyle                          float64
	audioUseSpeakerBoost                bool
	audioSeed                           string
	audioPreviousText                   string
	audioNextText                       string
	audioPreviousRequestIDs             string
	audioNextRequestIDs                 string
	audioPronunciationDictionaries      string
	audioUsePVCAsIVC                    bool
	audioApplyTextNormalization         string
	audioApplyLanguageTextNormalization bool
	audioOptimizeStreamingLatency       int
	audioEnableLogging                  bool
	audioJSON                           bool
	audioDebug                          bool
)

var audioCmd = &cobra.Command{
	Use:   "audio <text>",
	Short: "Generate speech audio",
	Long: `Generate speech audio using a configured text-to-speech provider.

By default:
  - Saves to ~/Music/term-llm/
  - Uses Venice tts-kokoro with voice af_sky unless audio.provider is configured
  - Emits MP3 audio for Venice/ElevenLabs, WAV audio for Gemini

Examples:
  term-llm audio "hello from term-llm"
  term-llm audio "quick smoke test" --output smoke.mp3
  term-llm audio "sad robot noises" --model tts-qwen3-0-6b --voice Vivian --prompt "Sad and slow."
  term-llm audio "faster" --speed 1.25 --format wav
  term-llm audio "Say cheerfully: hello" --provider gemini --voice Kore
  term-llm audio "eleven labs smoke test" --provider elevenlabs --model eleven_flash_v2_5 --voice Rachel --format mp3_44100_128
  term-llm audio "TTS the following conversation between Joe and Jane: Joe: Hi. Jane: Hi." --provider gemini --speaker1 Joe --voice1 Kore --speaker2 Jane --voice2 Puck
  echo "pipe me" | term-llm audio --voice af_bella -o - > out.mp3
  term-llm audio "machine readable" --json`,
	Args: cobra.ArbitraryArgs,
	RunE: runAudio,
}

func init() {
	audioCmd.Flags().StringVarP(&audioProvider, "provider", "p", "", "Audio provider override (venice, gemini, elevenlabs)")
	audioCmd.Flags().StringVarP(&audioOutput, "output", "o", "", "Custom output path, or - for stdout")
	audioCmd.Flags().StringVar(&audioModel, "model", "", "TTS model to use")
	audioCmd.Flags().StringVar(&audioVoice, "voice", "", "Voice to use (provider/model-specific; ElevenLabs accepts voice_id or account voice name; Venice cloned voice handles vv_<id>)")
	audioCmd.Flags().StringVar(&audioVoice1, "voice1", "", "Gemini multi-speaker voice for --speaker1")
	audioCmd.Flags().StringVar(&audioVoice2, "voice2", "", "Gemini multi-speaker voice for --speaker2")
	audioCmd.Flags().StringVar(&audioSpeaker1, "speaker1", "", "Gemini multi-speaker label for the first speaker")
	audioCmd.Flags().StringVar(&audioSpeaker2, "speaker2", "", "Gemini multi-speaker label for the second speaker")
	audioCmd.Flags().StringVar(&audioLanguage, "language", "", "Optional language hint (Venice model-specific; e.g. English or en)")
	audioCmd.Flags().StringVar(&audioPrompt, "prompt", "", "Optional style prompt for supported models")
	audioCmd.Flags().StringVar(&audioFormat, "format", "", "Response format (Venice: mp3, opus, aac, flac, wav, pcm; Gemini: wav, pcm; ElevenLabs: mp3_44100_128, pcm_24000, wav_44100, etc.)")
	audioCmd.Flags().Float64Var(&audioSpeed, "speed", audio.DefaultSpeed, "Speech speed (Venice: 0.25 to 4.0; ElevenLabs: 0.7 to 1.2)")
	audioCmd.Flags().BoolVar(&audioStreaming, "streaming", false, "Ask supported providers to stream; output is still collected before saving")
	audioCmd.Flags().Float64Var(&audioTemperature, "temperature", -1, "Sampling temperature for supported models (0 to 2); -1 omits it")
	audioCmd.Flags().Float64Var(&audioTopP, "top-p", -1, "Nucleus sampling for supported models (0 to 1); -1 omits it")
	audioCmd.Flags().Float64Var(&audioStability, "stability", -1, "ElevenLabs voice stability (0 to 1); -1 omits it")
	audioCmd.Flags().Float64Var(&audioSimilarityBoost, "similarity-boost", -1, "ElevenLabs similarity boost (0 to 1); -1 omits it")
	audioCmd.Flags().Float64Var(&audioStyle, "style", -1, "ElevenLabs style exaggeration (0 to 1); -1 omits it")
	audioCmd.Flags().BoolVar(&audioUseSpeakerBoost, "speaker-boost", true, "ElevenLabs speaker boost voice setting")
	audioCmd.Flags().StringVar(&audioSeed, "seed", "", "ElevenLabs deterministic seed (0 to 4294967295)")
	audioCmd.Flags().StringVar(&audioPreviousText, "previous-text", "", "ElevenLabs continuity hint: text before this request")
	audioCmd.Flags().StringVar(&audioNextText, "next-text", "", "ElevenLabs continuity hint: text after this request")
	audioCmd.Flags().StringVar(&audioPreviousRequestIDs, "previous-request-ids", "", "ElevenLabs comma-separated previous request IDs for continuity")
	audioCmd.Flags().StringVar(&audioNextRequestIDs, "next-request-ids", "", "ElevenLabs comma-separated next request IDs for continuity")
	audioCmd.Flags().StringVar(&audioPronunciationDictionaries, "pronunciation-dictionaries", "", "ElevenLabs comma-separated pronunciation dictionary IDs, optionally id:version")
	audioCmd.Flags().BoolVar(&audioUsePVCAsIVC, "use-pvc-as-ivc", false, "ElevenLabs workaround to use IVC instead of PVC for lower latency")
	audioCmd.Flags().StringVar(&audioApplyTextNormalization, "apply-text-normalization", "", "ElevenLabs text normalization (auto, on, off)")
	audioCmd.Flags().BoolVar(&audioApplyLanguageTextNormalization, "apply-language-text-normalization", false, "ElevenLabs language text normalization, currently mainly Japanese")
	audioCmd.Flags().IntVar(&audioOptimizeStreamingLatency, "optimize-streaming-latency", -1, "ElevenLabs latency optimization level (0 to 4); -1 omits it")
	audioCmd.Flags().BoolVar(&audioEnableLogging, "enable-logging", true, "ElevenLabs request logging/history; set false for zero-retention-capable accounts")
	audioCmd.Flags().BoolVar(&audioJSON, "json", false, "Emit machine-readable JSON to stdout")
	audioCmd.Flags().BoolVarP(&audioDebug, "debug", "d", false, "Show debug information")
	_ = audioCmd.RegisterFlagCompletionFunc("provider", staticCompletion("venice", "gemini", "elevenlabs"))
	_ = audioCmd.RegisterFlagCompletionFunc("model", audioModelCompletion)
	_ = audioCmd.RegisterFlagCompletionFunc("voice", audioVoiceCompletion)
	_ = audioCmd.RegisterFlagCompletionFunc("voice1", audioGeminiVoiceCompletion)
	_ = audioCmd.RegisterFlagCompletionFunc("voice2", audioGeminiVoiceCompletion)
	_ = audioCmd.RegisterFlagCompletionFunc("format", audioFormatCompletion)
	_ = audioCmd.RegisterFlagCompletionFunc("apply-text-normalization", staticCompletion(audio.ElevenLabsTextNormalization...))

	rootCmd.AddCommand(audioCmd)
}

func runAudio(cmd *cobra.Command, args []string) error {
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

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}
	initThemeFromConfig(cfg)

	providerName := firstNonEmpty(audioProvider, cfg.Audio.Provider, "venice")
	switch providerName {
	case "venice":
		return runVeniceAudio(cmd, cfg, text, temperature, topP)
	case "gemini":
		return runGeminiAudio(cmd, cfg, text, temperature, topP)
	case "elevenlabs":
		return runElevenLabsAudio(cmd, cfg, text)
	default:
		return fmt.Errorf("unsupported audio provider %q (allowed: venice, gemini, elevenlabs)", providerName)
	}
}

func runVeniceAudio(cmd *cobra.Command, cfg *config.Config, text string, temperature, topP *float64) error {
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

	ctx, stop := signal.NotifyContext()
	defer stop()
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
	return writeAudioResult(cmd, cfg.Audio.OutputDir, "venice", text, model, voice, format, result)
}

func runGeminiAudio(cmd *cobra.Command, cfg *config.Config, text string, temperature, topP *float64) error {
	apiKey := strings.TrimSpace(cfg.Audio.Gemini.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(cfg.Image.Gemini.APIKey)
	}
	if apiKey == "" {
		geminiProvider, err := cfg.GetResolvedProviderConfig("gemini")
		if err != nil {
			return fmt.Errorf("gemini provider: %w", err)
		}
		if geminiProvider != nil {
			apiKey = strings.TrimSpace(geminiProvider.ResolvedAPIKey)
		}
	}
	if apiKey == "" {
		return fmt.Errorf("GEMINI_API_KEY not configured. Set environment variable or add to audio.gemini.api_key in config")
	}
	if strings.TrimSpace(audioLanguage) != "" {
		return fmt.Errorf("Gemini TTS auto-detects language and does not support --language")
	}
	if audioSpeed != 0 && audioSpeed != audio.DefaultSpeed {
		return fmt.Errorf("Gemini TTS does not support --speed; use --prompt for pacing instructions")
	}

	model := firstNonEmpty(audioModel, cfg.Audio.Gemini.Model, "gemini-3.1-flash-tts-preview")
	voice := firstNonEmpty(audioVoice, cfg.Audio.Gemini.Voice, "Kore")
	format := firstNonEmpty(audioFormat, cfg.Audio.Gemini.Format, "wav")
	if err := audio.ValidateGeminiFormat(format); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext()
	defer stop()
	provider := audio.NewGeminiProvider(apiKey)
	result, err := provider.Generate(ctx, audio.Request{
		Input:          text,
		Model:          model,
		Voice:          voice,
		Voice1:         audioVoice1,
		Voice2:         audioVoice2,
		Speaker1:       audioSpeaker1,
		Speaker2:       audioSpeaker2,
		Prompt:         audioPrompt,
		ResponseFormat: format,
		Streaming:      audioStreaming,
		Temperature:    temperature,
		TopP:           topP,
		Debug:          audioDebug,
		DebugRaw:       debugRaw,
	})
	if err != nil {
		return fmt.Errorf("audio generation failed: %w", err)
	}
	return writeAudioResult(cmd, cfg.Audio.OutputDir, "gemini", text, model, voice, format, result)
}

func runElevenLabsAudio(cmd *cobra.Command, cfg *config.Config, text string) error {
	apiKey := strings.TrimSpace(cfg.Audio.ElevenLabs.APIKey)
	if apiKey == "" {
		elevenLabsProvider, err := cfg.GetResolvedProviderConfig("elevenlabs")
		if err != nil {
			return fmt.Errorf("elevenlabs provider: %w", err)
		}
		if elevenLabsProvider != nil {
			apiKey = strings.TrimSpace(elevenLabsProvider.ResolvedAPIKey)
		}
	}
	if apiKey == "" {
		return fmt.Errorf("ELEVENLABS_API_KEY not configured. Set environment variable or add to audio.elevenlabs.api_key in config")
	}
	if strings.TrimSpace(audioPrompt) != "" {
		return fmt.Errorf("ElevenLabs TTS does not support --prompt; include style instructions in the text or use voice settings")
	}
	if audioTemperature >= 0 || audioTopP >= 0 {
		return fmt.Errorf("ElevenLabs TTS does not support --temperature or --top-p")
	}

	model := firstNonEmpty(audioModel, cfg.Audio.ElevenLabs.Model, "eleven_multilingual_v2")
	voice := firstNonEmpty(audioVoice, cfg.Audio.ElevenLabs.Voice, "JBFqnCBsd6RMkjVDRZzb")
	format := firstNonEmpty(audioFormat, cfg.Audio.ElevenLabs.Format, "mp3_44100_128")
	if err := audio.ValidateElevenLabsFormat(format); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext()
	defer stop()
	provider := audio.NewElevenLabsProvider(apiKey)
	result, err := provider.Generate(ctx, audio.Request{
		Input:                          text,
		Model:                          model,
		Voice:                          voice,
		Language:                       audioLanguage,
		ResponseFormat:                 format,
		Speed:                          audioSpeed,
		Streaming:                      audioStreaming,
		Stability:                      audioStability,
		SimilarityBoost:                audioSimilarityBoost,
		Style:                          audioStyle,
		UseSpeakerBoost:                audioUseSpeakerBoost,
		UseSpeakerBoostSet:             cmd.Flags().Changed("speaker-boost"),
		Seed:                           audioSeed,
		PreviousText:                   audioPreviousText,
		NextText:                       audioNextText,
		PreviousRequestIDs:             audioPreviousRequestIDs,
		NextRequestIDs:                 audioNextRequestIDs,
		PronunciationDictionaries:      audioPronunciationDictionaries,
		UsePVCAsIVC:                    audioUsePVCAsIVC,
		ApplyTextNormalization:         audioApplyTextNormalization,
		ApplyLanguageTextNormalization: audioApplyLanguageTextNormalization,
		OptimizeStreamingLatency:       audioOptimizeStreamingLatency,
		EnableLogging:                  audioEnableLogging,
		Debug:                          audioDebug,
		DebugRaw:                       debugRaw,
	})
	if err != nil {
		return fmt.Errorf("audio generation failed: %w", err)
	}
	return writeAudioResult(cmd, cfg.Audio.OutputDir, "elevenlabs", text, model, voice, format, result)
}

func writeAudioResult(cmd *cobra.Command, outputDir, providerName, text, model, voice, format string, result *audio.Result) error {
	if audioOutput == "-" {
		if _, err := cmd.OutOrStdout().Write(result.Data); err != nil {
			return fmt.Errorf("write audio to stdout: %w", err)
		}
		return nil
	}

	outputPath, err := saveAudioOutput(outputDir, text, audioOutput, format, result.Data)
	if err != nil {
		return err
	}
	if !audioJSON {
		fmt.Fprintf(cmd.ErrOrStderr(), "Saved to: %s\n", outputPath)
	}
	return emitAudioJSON(cmd, audioJSONResult{
		Provider: providerName,
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

func staticCompletion(values ...string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return values, cobra.ShellCompDirectiveNoFileComp
	}
}

func audioModelCompletion(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	provider := completionProvider(cmd)
	if provider == "gemini" {
		return audio.GeminiModels, cobra.ShellCompDirectiveNoFileComp
	}
	if provider == "elevenlabs" {
		return audio.ElevenLabsModels, cobra.ShellCompDirectiveNoFileComp
	}
	return audio.VeniceModels, cobra.ShellCompDirectiveNoFileComp
}

func audioVoiceCompletion(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	provider := completionProvider(cmd)
	if provider == "gemini" {
		return audio.GeminiVoices, cobra.ShellCompDirectiveNoFileComp
	}
	if provider == "elevenlabs" {
		return audio.ElevenLabsVoices, cobra.ShellCompDirectiveNoFileComp
	}
	return audio.VeniceVoices, cobra.ShellCompDirectiveNoFileComp
}

func audioGeminiVoiceCompletion(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return audio.GeminiVoices, cobra.ShellCompDirectiveNoFileComp
}

func audioFormatCompletion(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	provider := completionProvider(cmd)
	if provider == "gemini" {
		return audio.GeminiFormats, cobra.ShellCompDirectiveNoFileComp
	}
	if provider == "elevenlabs" {
		return audio.ElevenLabsFormats, cobra.ShellCompDirectiveNoFileComp
	}
	return audio.VeniceFormats, cobra.ShellCompDirectiveNoFileComp
}

func completionProvider(cmd *cobra.Command) string {
	provider, _ := cmd.Flags().GetString("provider")
	if strings.TrimSpace(provider) != "" {
		return strings.TrimSpace(provider)
	}
	return "venice"
}

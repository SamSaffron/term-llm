package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/music"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/spf13/cobra"
)

var (
	musicProvider                 string
	musicOutput                   string
	musicModel                    string
	musicFormat                   string
	musicDuration                 int
	musicLyrics                   string
	musicLyricsFile               string
	musicLyricsOptimizer          bool
	musicVoice                    string
	musicLanguage                 string
	musicSpeed                    float64
	musicStreaming                bool
	musicDetailed                 bool
	musicCompositionPlanFile      string
	musicSeed                     string
	musicForceInstrumental        bool
	musicRespectSectionsDurations bool
	musicStoreForInpainting       bool
	musicSignWithC2PA             bool
	musicWithTimestamps           bool
	musicDeleteMediaOnCompletion  bool
	musicQuote                    bool
	musicPollInterval             time.Duration
	musicPollTimeout              time.Duration
	musicJSON                     bool
	musicDebug                    bool
)

var musicCmd = &cobra.Command{
	Use:   "music <prompt>",
	Short: "Generate music or sound effects",
	Long: `Generate music, songs, or sound effects using Venice or ElevenLabs.

By default:
  - Saves to ~/Music/term-llm/
  - Uses Venice elevenlabs-sound-effects-v2 so short smoke clips are cheap and fast
  - Reads prompt text from stdin when no positional prompt is supplied

Examples:
  term-llm music "a one second clean synth blip" --duration 1
  term-llm music "upbeat chiptune victory sting" --provider venice --model mmaudio-v2-text-to-audio --duration 1
  term-llm music "polished instrumental funk loop" --provider elevenlabs --duration 3 --force-instrumental
  term-llm music "80s synth pop song" --model minimax-music-v25 --lyrics "Verse 1: Neon lights..." --duration 60
  term-llm music "quote this first" --quote --duration 1
  echo "cinematic whoosh" | term-llm music -o - > whoosh.mp3
  term-llm music "machine readable" --json`,
	Args: cobra.ArbitraryArgs,
	RunE: runMusic,
}

func init() {
	musicCmd.Flags().StringVarP(&musicProvider, "provider", "p", "", "Music provider override (venice, elevenlabs)")
	musicCmd.Flags().StringVarP(&musicOutput, "output", "o", "", "Custom output path, or - for stdout")
	musicCmd.Flags().StringVar(&musicModel, "model", "", "Music model to use")
	musicCmd.Flags().StringVar(&musicFormat, "format", "", "Output format (Venice: mp3, wav, flac by model; ElevenLabs: mp3_44100_128, pcm_24000, wav_44100, etc.)")
	musicCmd.Flags().IntVar(&musicDuration, "duration", 0, "Requested duration in seconds (Venice model-specific; ElevenLabs prompt mode: 3 to 600)")
	musicCmd.Flags().StringVar(&musicLyrics, "lyrics", "", "Lyrics prompt/text for lyric-capable Venice models")
	musicCmd.Flags().StringVar(&musicLyricsFile, "lyrics-file", "", "Read lyrics prompt/text from a file")
	musicCmd.Flags().BoolVar(&musicLyricsOptimizer, "lyrics-optimizer", false, "Venice: auto-generate lyrics from prompt where supported")
	musicCmd.Flags().StringVar(&musicVoice, "voice", "", "Voice for Venice voice-enabled models")
	musicCmd.Flags().StringVar(&musicLanguage, "language", "", "Language code for Venice models that support language_code")
	musicCmd.Flags().Float64Var(&musicSpeed, "speed", 0, "Speed multiplier for Venice models that support speed")
	musicCmd.Flags().BoolVar(&musicStreaming, "streaming", false, "ElevenLabs: use streaming music endpoint")
	musicCmd.Flags().BoolVar(&musicDetailed, "detailed", false, "ElevenLabs: use detailed endpoint and keep metadata when returned")
	musicCmd.Flags().StringVar(&musicCompositionPlanFile, "composition-plan-file", "", "ElevenLabs: JSON composition plan file; cannot be combined with prompt-only seed")
	musicCmd.Flags().StringVar(&musicSeed, "seed", "", "ElevenLabs: deterministic seed (0 to 2147483647); cannot be used with prompt per API docs")
	musicCmd.Flags().BoolVar(&musicForceInstrumental, "force-instrumental", false, "Force instrumental generation where supported")
	musicCmd.Flags().BoolVar(&musicRespectSectionsDurations, "respect-sections-durations", true, "ElevenLabs composition-plan mode: strictly respect section durations")
	musicCmd.Flags().BoolVar(&musicStoreForInpainting, "store-for-inpainting", false, "ElevenLabs enterprise: store generated song for inpainting")
	musicCmd.Flags().BoolVar(&musicSignWithC2PA, "sign-with-c2pa", false, "ElevenLabs: sign generated mp3 with C2PA")
	musicCmd.Flags().BoolVar(&musicWithTimestamps, "with-timestamps", false, "ElevenLabs detailed endpoint: include word timestamps")
	musicCmd.Flags().BoolVar(&musicDeleteMediaOnCompletion, "delete-media-on-completion", true, "Venice: delete queued media from provider storage after retrieval")
	musicCmd.Flags().BoolVar(&musicQuote, "quote", false, "Venice: return price quote instead of queueing generation")
	musicCmd.Flags().DurationVar(&musicPollInterval, "poll-interval", 2*time.Second, "Venice poll interval")
	musicCmd.Flags().DurationVar(&musicPollTimeout, "poll-timeout", 10*time.Minute, "Venice poll timeout")
	musicCmd.Flags().BoolVar(&musicJSON, "json", false, "Emit machine-readable JSON to stdout")
	musicCmd.Flags().BoolVarP(&musicDebug, "debug", "d", false, "Show debug information")
	_ = musicCmd.RegisterFlagCompletionFunc("provider", staticCompletion("venice", "elevenlabs"))
	_ = musicCmd.RegisterFlagCompletionFunc("model", musicModelCompletion)
	_ = musicCmd.RegisterFlagCompletionFunc("format", musicFormatCompletion)
	_ = musicCmd.RegisterFlagCompletionFunc("voice", staticCompletion(music.VeniceVoices...))

	rootCmd.AddCommand(musicCmd)
}

func runMusic(cmd *cobra.Command, args []string) error {
	prompt, err := resolveMusicPrompt(cmd, args)
	if err != nil {
		return err
	}
	lyrics, err := resolveMusicLyrics()
	if err != nil {
		return err
	}
	plan, err := readOptionalFile(musicCompositionPlanFile)
	if err != nil {
		return err
	}

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}
	initThemeFromConfig(cfg)

	providerName := firstNonEmpty(musicProvider, cfg.Music.Provider, "venice")
	req := music.Request{
		Prompt:                      prompt,
		CompositionPlan:             plan,
		Format:                      musicFormat,
		DurationSeconds:             musicDuration,
		LyricsPrompt:                lyrics,
		LyricsOptimizer:             musicLyricsOptimizer,
		LyricsOptimizerSet:          cmd.Flags().Changed("lyrics-optimizer"),
		Voice:                       musicVoice,
		LanguageCode:                musicLanguage,
		Speed:                       musicSpeed,
		Streaming:                   musicStreaming,
		Detailed:                    musicDetailed,
		Seed:                        musicSeed,
		ForceInstrumental:           musicForceInstrumental,
		ForceInstrumentalSet:        cmd.Flags().Changed("force-instrumental"),
		RespectSectionsDurations:    musicRespectSectionsDurations,
		RespectSectionsDurationsSet: cmd.Flags().Changed("respect-sections-durations"),
		StoreForInpainting:          musicStoreForInpainting,
		SignWithC2PA:                musicSignWithC2PA,
		WithTimestamps:              musicWithTimestamps,
		DeleteMediaOnCompletion:     musicDeleteMediaOnCompletion,
		QuoteOnly:                   musicQuote,
		PollInterval:                musicPollInterval,
		PollTimeout:                 musicPollTimeout,
		Debug:                       musicDebug,
		DebugRaw:                    debugRaw,
	}

	switch providerName {
	case "venice":
		return runVeniceMusic(cmd, cfg, req)
	case "elevenlabs":
		return runElevenLabsMusic(cmd, cfg, req)
	default:
		return fmt.Errorf("unsupported music provider %q (allowed: venice, elevenlabs)", providerName)
	}
}

func runVeniceMusic(cmd *cobra.Command, cfg *config.Config, req music.Request) error {
	apiKey := strings.TrimSpace(cfg.Music.Venice.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(cfg.Audio.Venice.APIKey)
	}
	if apiKey == "" {
		apiKey = strings.TrimSpace(cfg.Image.Venice.APIKey)
	}
	if apiKey == "" {
		return fmt.Errorf("VENICE_API_KEY not configured. Set environment variable or add to music.venice.api_key in config")
	}
	req.Model = firstNonEmpty(musicModel, cfg.Music.Venice.Model, "elevenlabs-sound-effects-v2")
	if req.Format == "" {
		req.Format = firstNonEmpty(cfg.Music.Venice.Format, music.VeniceDefaultFormatForModel(req.Model))
	}
	ctx, stop := signal.NotifyContext()
	defer stop()
	result, err := music.NewVeniceProvider(apiKey).Generate(ctx, req)
	if err != nil {
		return fmt.Errorf("music generation failed: %w", err)
	}
	return writeMusicResult(cmd, cfg.Music.OutputDir, "venice", req.Prompt, req.Model, req.Format, result)
}

func runElevenLabsMusic(cmd *cobra.Command, cfg *config.Config, req music.Request) error {
	apiKey := strings.TrimSpace(cfg.Music.ElevenLabs.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(cfg.Audio.ElevenLabs.APIKey)
	}
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
		return fmt.Errorf("ELEVENLABS_API_KEY not configured. Set environment variable or add to music.elevenlabs.api_key in config")
	}
	req.Model = firstNonEmpty(musicModel, cfg.Music.ElevenLabs.Model, "music_v1")
	if req.Format == "" {
		req.Format = firstNonEmpty(cfg.Music.ElevenLabs.Format, "mp3_44100_128")
	}
	ctx, stop := signal.NotifyContext()
	defer stop()
	result, err := music.NewElevenLabsProvider(apiKey).Generate(ctx, req)
	if err != nil {
		return fmt.Errorf("music generation failed: %w", err)
	}
	return writeMusicResult(cmd, cfg.Music.OutputDir, "elevenlabs", req.Prompt, req.Model, req.Format, result)
}

func resolveMusicPrompt(cmd *cobra.Command, args []string) (string, error) {
	if len(args) > 0 {
		text := strings.TrimSpace(strings.Join(args, " "))
		if text == "" {
			return "", fmt.Errorf("prompt is required")
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
	if strings.TrimSpace(musicCompositionPlanFile) != "" {
		return "", nil
	}
	return "", fmt.Errorf("prompt is required")
}

func resolveMusicLyrics() (string, error) {
	if strings.TrimSpace(musicLyricsFile) == "" {
		return musicLyrics, nil
	}
	if strings.TrimSpace(musicLyrics) != "" {
		return "", fmt.Errorf("use either --lyrics or --lyrics-file, not both")
	}
	data, err := os.ReadFile(expandOutputPath(musicLyricsFile))
	if err != nil {
		return "", fmt.Errorf("read lyrics file: %w", err)
	}
	return string(data), nil
}

func readOptionalFile(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(expandOutputPath(path))
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", path, err)
	}
	return data, nil
}

func writeMusicResult(cmd *cobra.Command, outputDir, providerName, prompt, model, format string, result *music.Result) error {
	if result.Quote != nil {
		if musicJSON {
			return emitMusicJSON(cmd, musicJSONResult{Provider: providerName, Prompt: prompt, Model: model, Format: format, Quote: result.Quote})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "$%.4f\n", *result.Quote)
		return nil
	}
	if musicOutput == "-" {
		_, err := cmd.OutOrStdout().Write(result.Data)
		return err
	}
	outputPath, err := saveMusicOutput(outputDir, prompt, musicOutput, result.Format, result.Data)
	if err != nil {
		return err
	}
	if !musicJSON {
		fmt.Fprintf(cmd.ErrOrStderr(), "Saved to: %s\n", outputPath)
	}
	return emitMusicJSON(cmd, musicJSONResult{Provider: providerName, Prompt: prompt, Model: model, Format: result.Format, Output: &musicJSONOutput{Path: outputPath, MimeType: result.MimeType, Bytes: len(result.Data)}, Metadata: result.Metadata})
}

func saveMusicOutput(outputDir, prompt, outputPath, format string, data []byte) (string, error) {
	if strings.TrimSpace(outputPath) == "" {
		return music.Save(data, outputDir, prompt, format)
	}
	path := expandOutputPath(outputPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write music: %w", err)
	}
	return path, nil
}

type musicJSONResult struct {
	Provider string           `json:"provider"`
	Prompt   string           `json:"prompt"`
	Model    string           `json:"model"`
	Format   string           `json:"format"`
	Quote    *float64         `json:"quote,omitempty"`
	Output   *musicJSONOutput `json:"output,omitempty"`
	Metadata json.RawMessage  `json:"metadata,omitempty"`
}

type musicJSONOutput struct {
	Path     string `json:"path"`
	MimeType string `json:"mime_type"`
	Bytes    int    `json:"bytes"`
}

func emitMusicJSON(cmd *cobra.Command, result musicJSONResult) error {
	if !musicJSON {
		return nil
	}
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func musicModelCompletion(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	provider := completionProvider(cmd)
	if provider == "elevenlabs" {
		return music.ElevenLabsModels, cobra.ShellCompDirectiveNoFileComp
	}
	return music.VeniceModels, cobra.ShellCompDirectiveNoFileComp
}

func musicFormatCompletion(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	provider := completionProvider(cmd)
	if provider == "elevenlabs" {
		return music.ElevenLabsFormats, cobra.ShellCompDirectiveNoFileComp
	}
	return music.VeniceFormats, cobra.ShellCompDirectiveNoFileComp
}

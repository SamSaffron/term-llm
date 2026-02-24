package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/spf13/cobra"
)

var (
	transcribeLanguage  string
	transcribePorcelain bool
	transcribeProvider  string
)

var transcribeCmd = &cobra.Command{
	Use:   "transcribe <file>",
	Short: "Transcribe an audio file to text using Whisper",
	Args:  cobra.ExactArgs(1),
	RunE:  runTranscribe,
}

func transcribeWhisperCLI(ctx context.Context, cfg *config.Config, filePath, language string) (string, error) {
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

	cmd := exec.CommandContext(ctx, whisperBin, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("whisper-cli: %w", err)
	}

	re := regexp.MustCompile(`^\[[\d:.,\s>-]+\]\s*`)
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		line = re.ReplaceAllString(strings.TrimSpace(line), "")
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, " "), nil
}

func init() {
	transcribeCmd.Flags().StringVar(&transcribeLanguage, "language", "", "Language hint for transcription (e.g. \"en\", \"ja\")")
	transcribeCmd.Flags().BoolVar(&transcribePorcelain, "porcelain", false, "Output only the transcript text")
	transcribeCmd.Flags().StringVar(&transcribeProvider, "provider", "openai", `Transcription provider: "openai" (default), "mistral" (Voxtral), "local" (whisper.cpp server), "whisper-cli" (whisper.cpp CLI, on-demand)`)

	rootCmd.AddCommand(transcribeCmd)
}

func runTranscribe(cmd *cobra.Command, args []string) error {
	ctx, stop := signal.NotifyContext()
	defer stop()

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	filePath := args[0]
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open audio file: %w", err)
	}
	defer f.Close()

	mimeType, err := detectAudioMimeType(filePath)
	if err != nil {
		return err
	}

	if !transcribePorcelain {
		fmt.Fprintf(cmd.ErrOrStderr(), "Transcribing %s (%s)...\n", filepath.Base(filePath), mimeType)
	}

	transcript, err := transcribeAudio(ctx, cfg, filePath, strings.TrimSpace(transcribeLanguage))
	if err != nil {
		return err
	}

	fmt.Fprintln(cmd.OutOrStdout(), transcript)
	return nil
}

func detectAudioMimeType(filePath string) (string, error) {
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

func transcribeAudio(ctx context.Context, cfg *config.Config, filePath, language string) (string, error) {
	provider := strings.TrimSpace(transcribeProvider)
	baseProvider := strings.SplitN(provider, ":", 2)[0]
	if baseProvider == "" {
		baseProvider = string(config.ProviderTypeOpenAI)
	}

	switch baseProvider {
	case "local":
		// whisper.cpp server — OpenAI-compatible but different endpoint, no auth
		endpoint := "http://localhost:8080/inference"
		if providerCfg, ok := cfg.Providers["local_whisper"]; ok && providerCfg.BaseURL != "" {
			endpoint = strings.TrimRight(providerCfg.BaseURL, "/") + "/inference"
		}
		return llm.TranscribeFile(ctx, filePath, llm.TranscribeOptions{
			Language: language,
			Endpoint: endpoint,
		})

	case "mistral":
		mistralCfg := cfg.Providers["mistral"]
		apiKey := mistralCfg.ResolvedAPIKey
		if apiKey == "" {
			apiKey = os.Getenv("MISTRAL_API_KEY")
		}
		if apiKey == "" {
			return "", fmt.Errorf("no Mistral API key configured (providers.mistral.api_key or MISTRAL_API_KEY)")
		}
		endpoint := "https://api.mistral.ai/v1/audio/transcriptions"
		if mistralCfg.BaseURL != "" {
			endpoint = strings.TrimRight(mistralCfg.BaseURL, "/") + "/audio/transcriptions"
		}
		return llm.TranscribeFile(ctx, filePath, llm.TranscribeOptions{
			APIKey:   apiKey,
			Model:    "voxtral-mini-latest",
			Language: language,
			Endpoint: endpoint,
		})

	case "whisper-cli":
		return transcribeWhisperCLI(ctx, cfg, filePath, language)

	case string(config.ProviderTypeOpenAI):
		openaiCfg := cfg.Providers[string(config.ProviderTypeOpenAI)]
		apiKey := openaiCfg.ResolvedAPIKey
		if apiKey == "" {
			apiKey = os.Getenv("OPENAI_API_KEY")
		}
		if apiKey == "" {
			return "", fmt.Errorf("no OpenAI API key configured (providers.openai.api_key or OPENAI_API_KEY)")
		}
		return llm.TranscribeFile(ctx, filePath, llm.TranscribeOptions{
			APIKey:   apiKey,
			Language: language,
		})

	default:
		return "", fmt.Errorf("unsupported provider %q (supported: openai, mistral, local, whisper-cli)", provider)
	}
}

package cmd

import (
	"github.com/samsaffron/term-llm/internal/app/transcribe"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/spf13/cobra"
)

var transcribeOptions transcribe.Options

var transcribeCmd = &cobra.Command{
	Use:   "transcribe <file>",
	Short: "Transcribe an audio file to text",
	Args:  cobra.ExactArgs(1),
	RunE:  runTranscribe,
}

func init() {
	transcribeCmd.Flags().StringVar(&transcribeOptions.Language, "language", "", "Language hint for transcription (e.g. \"en\", \"ja\")")
	transcribeCmd.Flags().StringVar(&transcribeOptions.Model, "model", "", "Transcription model override")
	transcribeCmd.Flags().BoolVar(&transcribeOptions.Timestamps, "timestamps", false, "Request timestamp metadata where supported (ElevenLabs emits JSON with word timestamps)")
	transcribeCmd.Flags().BoolVar(&transcribeOptions.Porcelain, "porcelain", false, "Output only the transcript text")
	transcribeCmd.Flags().StringVar(&transcribeOptions.Provider, "provider", "", `Transcription provider override: "openai", "chatgpt" (ChatGPT login, experimental), "mistral" (Voxtral), "venice", "elevenlabs", "local" (whisper.cpp server), "whisper-cli". Defaults to transcription.provider in config, or "openai".`)
	_ = transcribeCmd.RegisterFlagCompletionFunc("provider", staticCompletion("openai", "chatgpt", "mistral", "venice", "elevenlabs", "local", "whisper-cli"))
	_ = transcribeCmd.RegisterFlagCompletionFunc("model", transcribeModelCompletion)

	rootCmd.AddCommand(transcribeCmd)
}

func transcribeModelCompletion(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	switch completionProvider(cmd) {
	case "venice":
		return []string{"nvidia/parakeet-tdt-0.6b-v3", "openai/whisper-large-v3", "fal-ai/wizper", "elevenlabs/scribe-v2", "stt-xai-v1"}, cobra.ShellCompDirectiveNoFileComp
	case "elevenlabs":
		return []string{"scribe_v2", "scribe_v1"}, cobra.ShellCompDirectiveNoFileComp
	case "mistral":
		return []string{"voxtral-mini-latest", "voxtral-small-latest"}, cobra.ShellCompDirectiveNoFileComp
	case "chatgpt":
		return nil, cobra.ShellCompDirectiveNoFileComp
	default:
		return []string{"whisper-1", "gpt-4o-transcribe", "gpt-4o-mini-transcribe"}, cobra.ShellCompDirectiveNoFileComp
	}
}

func runTranscribe(cmd *cobra.Command, args []string) error {
	ctx, stop := signal.NotifyContext()
	defer stop()

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}
	return transcribe.Run(ctx, cfg, transcribeOptions, args[0], cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func detectAudioMimeType(filePath string) (string, error) {
	return transcribe.DetectAudioMimeType(filePath)
}

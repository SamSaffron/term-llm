package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/image"
	"github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

var (
	imageInputs      []string
	imageProvider    string
	imageOutput      string
	imageNoDisplay   bool
	imageNoClipboard bool
	imageNoSave      bool
	imageDebug       bool
)

var imageCmd = &cobra.Command{
	Use:   "image <prompt>",
	Short: "Generate or edit images using AI",
	Long: `Generate images from text prompts or edit existing images.

By default:
  - Saves to ~/Pictures/term-llm/
  - Displays via icat (if available)
  - Copies to clipboard

Multi-image editing (Gemini, OpenRouter only):
  Use -i multiple times to combine or work with multiple images.

Examples:
  term-llm image "a robot cat on a rainbow"
  term-llm image "make it purple" -i photo.png
  term-llm image "add a hat" -i clipboard           # edit from clipboard
  term-llm image "combine these" -i a.png -i b.png  # multi-image (Gemini/OpenRouter)
  term-llm image "sunset over mountains" --provider flux
  term-llm image "logo design" -o ./output.png --no-display
  echo "a sunset" | term-llm image                  # prompt from stdin`,
	Args: cobra.ArbitraryArgs,
	RunE: runImage,
}

func init() {
	imageCmd.Flags().StringArrayVarP(&imageInputs, "input", "i", nil, "Input image(s) to edit (can be specified multiple times)")
	imageCmd.Flags().StringVarP(&imageProvider, "provider", "p", "", "Override provider (gemini, openai, flux, openrouter)")
	imageCmd.Flags().StringVarP(&imageOutput, "output", "o", "", "Custom output path")
	imageCmd.Flags().BoolVar(&imageNoDisplay, "no-display", false, "Skip terminal display")
	imageCmd.Flags().BoolVar(&imageNoClipboard, "no-clipboard", false, "Skip clipboard copy")
	imageCmd.Flags().BoolVar(&imageNoSave, "no-save", false, "Don't save to default location (use with -o)")
	imageCmd.Flags().BoolVarP(&imageDebug, "debug", "d", false, "Show debug information")

	imageCmd.RegisterFlagCompletionFunc("provider", ImageProviderFlagCompletion)

	rootCmd.AddCommand(imageCmd)
}

func runImage(cmd *cobra.Command, args []string) error {
	var prompt string
	if len(args) > 0 {
		prompt = strings.Join(args, " ")
	} else {
		stdinContent, err := input.ReadStdin()
		if err != nil {
			return fmt.Errorf("failed to read stdin: %w", err)
		}
		prompt = strings.TrimSpace(stdinContent)
	}

	if prompt == "" {
		return fmt.Errorf("prompt required: provide as argument or via stdin")
	}

	ctx, stop := signal.NotifyContext()
	defer stop()

	// Load config (requires setup wizard if no config exists)
	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	// Initialize theme from config
	initThemeFromConfig(cfg)

	// Create image provider
	provider, err := image.NewImageProvider(cfg, imageProvider)
	if err != nil {
		return err
	}

	if imageDebug {
		fmt.Fprintf(os.Stderr, "Using provider: %s\n", provider.Name())
		fmt.Fprintf(os.Stderr, "Prompt: %q\n", prompt)
	}

	var result *image.ImageResult

	if len(imageInputs) > 0 {
		// Edit mode
		if !provider.SupportsEdit() {
			return fmt.Errorf("provider %s does not support image editing", provider.Name())
		}

		// Check multi-image support
		if len(imageInputs) > 1 && !provider.SupportsMultiImage() {
			return fmt.Errorf("provider %s does not support multiple input images", provider.Name())
		}

		// Read all input images
		var inputImages []image.InputImage
		for _, inputPath := range imageInputs {
			var inputData []byte
			var resolvedPath string

			if inputPath == "clipboard" {
				// Read from clipboard
				inputData, err = image.ReadFromClipboard()
				if err != nil {
					return fmt.Errorf("failed to read from clipboard: %w", err)
				}
				resolvedPath = "clipboard.png" // for MIME type detection
				if imageDebug {
					fmt.Fprintf(os.Stderr, "Input image: clipboard (%d bytes)\n", len(inputData))
				}
			} else {
				// Read from file
				inputData, err = os.ReadFile(inputPath)
				if err != nil {
					return fmt.Errorf("failed to read input image %s: %w", inputPath, err)
				}
				resolvedPath = inputPath
				if imageDebug {
					fmt.Fprintf(os.Stderr, "Input image: %s (%d bytes)\n", inputPath, len(inputData))
				}
			}

			inputImages = append(inputImages, image.InputImage{
				Data: inputData,
				Path: resolvedPath,
			})
		}

		result, err = runImageWithSpinner(ctx, provider, func() (*image.ImageResult, error) {
			return provider.Edit(ctx, image.EditRequest{
				Prompt:      prompt,
				InputImages: inputImages,
				Debug:       imageDebug,
			})
		}, "Editing image")
		if err != nil {
			return fmt.Errorf("image editing failed: %w", err)
		}
	} else {
		// Generate mode
		result, err = runImageWithSpinner(ctx, provider, func() (*image.ImageResult, error) {
			return provider.Generate(ctx, image.GenerateRequest{
				Prompt: prompt,
				Debug:  imageDebug,
			})
		}, "Generating image")
		if err != nil {
			return fmt.Errorf("image generation failed: %w", err)
		}
	}

	// Determine output path
	var outputPath string
	if imageOutput != "" {
		// Custom output path specified
		outputPath = imageOutput
		if err := os.WriteFile(outputPath, result.Data, 0644); err != nil {
			return fmt.Errorf("failed to write image: %w", err)
		}
	} else if !imageNoSave {
		// Save to default location
		outputPath, err = image.SaveImage(result.Data, cfg.Image.OutputDir, prompt)
		if err != nil {
			return fmt.Errorf("failed to save image: %w", err)
		}
	}

	if outputPath != "" {
		fmt.Fprintf(os.Stderr, "Saved to: %s\n", outputPath)
		recordImageDirect(cfg, prompt, outputPath, result, provider.Name())
	}

	// Display via icat
	if !imageNoDisplay && outputPath != "" {
		if err := image.DisplayImage(outputPath); err != nil {
			if imageDebug {
				fmt.Fprintf(os.Stderr, "Display warning: %v\n", err)
			}
		}
	}

	// Copy to clipboard
	if !imageNoClipboard && outputPath != "" {
		if err := image.CopyToClipboard(outputPath, result.Data); err != nil {
			if imageDebug {
				fmt.Fprintf(os.Stderr, "Clipboard warning: %v\n", err)
			}
		} else {
			fmt.Fprintln(os.Stderr, "Copied to clipboard")
		}
	}

	return nil
}

// imageSpinnerModel is a simple spinner for image generation
type imageSpinnerModel struct {
	spinner  spinner.Model
	message  string
	result   *image.ImageResult
	err      error
	done     bool
	start    time.Time
	provider image.ImageProvider
	generate func() (*image.ImageResult, error)
	styles   *ui.Styles
}

type imageResultMsg struct {
	result *image.ImageResult
	err    error
}

func (m imageSpinnerModel) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		func() tea.Msg {
			result, err := m.generate()
			return imageResultMsg{result: result, err: err}
		},
	)
}

func (m imageSpinnerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" || msg.String() == "q" {
			return m, tea.Quit
		}

	case imageResultMsg:
		m.result = msg.result
		m.err = msg.err
		m.done = true
		return m, tea.Quit

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m imageSpinnerModel) View() string {
	if m.done {
		return ""
	}
	return ui.StreamingIndicator{
		Spinner:    m.spinner.View(),
		Phase:      m.message,
		Elapsed:    time.Since(m.start),
		ShowCancel: true,
	}.Render(m.styles) + "\n"
}

func runImageWithSpinner(_ context.Context, provider image.ImageProvider, generate func() (*image.ImageResult, error), message string) (*image.ImageResult, error) {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	m := imageSpinnerModel{
		spinner:  s,
		message:  message,
		start:    time.Now(),
		provider: provider,
		generate: generate,
		styles:   ui.DefaultStyles(),
	}

	// Try to open /dev/tty for terminal input
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		// Fallback: run without spinner UI
		return generate()
	}
	defer tty.Close()

	p := tea.NewProgram(m, tea.WithInput(tty), tea.WithOutput(os.Stderr), tea.WithoutSignalHandler())
	finalModel, err := p.Run()
	if err != nil {
		return nil, err
	}

	final := finalModel.(imageSpinnerModel)
	if final.err != nil {
		return nil, final.err
	}
	return final.result, nil
}

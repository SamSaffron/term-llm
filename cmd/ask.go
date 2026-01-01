package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/muesli/reflow/wordwrap"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	askDebug  bool
	askSearch bool
	askText   bool
)

var askCmd = &cobra.Command{
	Use:   "ask <question>",
	Short: "Ask a question and stream the answer",
	Long: `Ask the LLM a question and receive a streaming response.

Examples:
  term-llm ask "What is the capital of France?"
  term-llm ask "How do I reverse a string in Go?"
  term-llm ask "What is the latest version of Node.js?" -s
  term-llm ask "Explain the difference between TCP and UDP" -d
  term-llm ask "List 5 programming languages" --text`,
	Args: cobra.MinimumNArgs(1),
	RunE: runAsk,
}

func init() {
	askCmd.Flags().BoolVarP(&askSearch, "search", "s", false, "Enable web search for current information")
	askCmd.Flags().BoolVarP(&askDebug, "debug", "d", false, "Show debug information")
	askCmd.Flags().BoolVarP(&askText, "text", "t", false, "Output plain text instead of rendered markdown")
	rootCmd.AddCommand(askCmd)
}

func runAsk(cmd *cobra.Command, args []string) error {
	question := strings.Join(args, " ")
	ctx := context.Background()

	// Load or setup config
	var cfg *config.Config
	var err error

	if config.NeedsSetup() {
		cfg, err = ui.RunSetupWizard()
		if err != nil {
			return fmt.Errorf("setup cancelled: %w", err)
		}
	} else {
		cfg, err = config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
	}

	// Create LLM provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}

	// Build request
	req := llm.AskRequest{
		Question:     question,
		Instructions: cfg.Ask.Instructions,
		EnableSearch: askSearch,
		Debug:        askDebug,
	}

	// Check if we're in a TTY and can use glamour
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	useGlamour := !askText && isTTY

	// Create channel for streaming output
	output := make(chan string)

	// Start streaming in goroutine
	errChan := make(chan error, 1)
	go func() {
		errChan <- provider.StreamResponse(ctx, req, output)
	}()

	if useGlamour {
		err = streamWithGlamour(output)
	} else {
		err = streamPlainText(output)
	}

	if err != nil {
		return err
	}

	// Check for streaming errors
	if err := <-errChan; err != nil {
		return fmt.Errorf("streaming failed: %w", err)
	}

	return nil
}

// streamPlainText streams text directly without formatting
func streamPlainText(output <-chan string) error {
	for chunk := range output {
		fmt.Print(chunk)
	}
	fmt.Println()
	return nil
}

// getTerminalWidth returns the terminal width or a default
func getTerminalWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 {
		return 80 // default
	}
	return width
}

// streamWithGlamour renders markdown incrementally as content streams in (line-by-line, no clearing)
func streamWithGlamour(output <-chan string) error {
	var content strings.Builder
	var printedLines int
	gotFirstChunk := false
	spinnerDone := make(chan struct{})
	termWidth := getTerminalWidth()

	// Start spinner
	go func() {
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		i := 0
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()

		fmt.Printf("%s Thinking...", frames[i])
		for {
			select {
			case <-spinnerDone:
				// Clear spinner line
				fmt.Print("\r\033[K")
				return
			case <-ticker.C:
				i = (i + 1) % len(frames)
				fmt.Printf("\r%s Thinking...", frames[i])
			}
		}
	}()

	for chunk := range output {
		if !gotFirstChunk {
			gotFirstChunk = true
			close(spinnerDone)
			time.Sleep(10 * time.Millisecond)
		}

		content.WriteString(chunk)

		// When we get a newline, render and print new lines only
		if strings.Contains(chunk, "\n") {
			rendered, err := renderMarkdown(content.String(), termWidth)
			if err != nil {
				rendered = content.String()
			}

			// Split into lines and print only new ones
			lines := strings.Split(rendered, "\n")
			for i := printedLines; i < len(lines); i++ {
				if i < len(lines)-1 { // Don't print the last partial line
					fmt.Println(lines[i])
					printedLines++
				}
			}
		}
	}

	// Stop spinner if we never got content
	if !gotFirstChunk {
		close(spinnerDone)
	}

	// Final render - print any remaining lines
	finalContent := content.String()
	if finalContent != "" {
		rendered, err := renderMarkdown(finalContent, termWidth)
		if err != nil {
			rendered = finalContent
		}

		lines := strings.Split(rendered, "\n")
		for i := printedLines; i < len(lines); i++ {
			line := lines[i]
			if line != "" || i < len(lines)-1 {
				fmt.Println(line)
			}
		}
	}

	return nil
}

// renderMarkdown renders markdown content using glamour with word wrapping
func renderMarkdown(content string, width int) (string, error) {
	style := styles.DraculaStyleConfig
	style.Document.Margin = uintPtr(0)
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.CodeBlock.Margin = uintPtr(0)

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return "", err
	}

	rendered, err := renderer.Render(content)
	if err != nil {
		return "", err
	}

	result := strings.TrimSpace(rendered)
	if result != "" && !strings.HasSuffix(result, "\n") {
		result += "\n"
	}

	// Ensure text is wrapped to terminal width
	return wordwrap.String(result, width), nil
}

func uintPtr(v uint) *uint {
	return &v
}

// Ensure ansi package is imported for style config
var _ = ansi.StyleConfig{}

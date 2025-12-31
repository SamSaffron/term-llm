package ui

import (
	"fmt"
	"os"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

const SomethingElse = "__something_else__"

// getStyles creates lipgloss styles using a renderer tied to the given output
func getStyles(output *os.File) (cmdStyle, explanationStyle lipgloss.Style) {
	r := lipgloss.NewRenderer(output)
	cmdStyle = r.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))  // bright green
	explanationStyle = r.NewStyle().Foreground(lipgloss.Color("8"))      // grey
	return
}

// formatOption renders a command suggestion with lipgloss styling
func formatOption(tty *os.File, cmd, explanation string) string {
	cmdStyle, explanationStyle := getStyles(tty)
	return cmdStyle.Render(cmd) + "\n" + explanationStyle.Render("  "+explanation)
}

// getTTY opens /dev/tty for direct terminal access (bypasses redirections)
func getTTY() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

// SelectCommand presents the user with a list of command suggestions and returns the selected one
// Returns the selected command or SomethingElse if user wants to refine their request
func SelectCommand(suggestions []llm.CommandSuggestion) (string, error) {
	var selected string

	// Get tty for proper color rendering
	tty, ttyErr := getTTY()
	if ttyErr != nil {
		tty = os.Stderr // fallback
	} else {
		defer tty.Close()
	}

	_, explanationStyle := getStyles(tty)

	// Build options from suggestions
	options := make([]huh.Option[string], 0, len(suggestions)+1)
	for _, s := range suggestions {
		label := formatOption(tty, s.Command, s.Explanation)
		options = append(options, huh.NewOption(label, s.Command))
	}
	// Add "something else" option
	options = append(options, huh.NewOption(explanationStyle.Render("something else..."), SomethingElse))

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Select a command to run").
				Options(options...).
				Value(&selected),
		),
	)

	// Use /dev/tty directly to bypass shell redirections
	if ttyErr == nil {
		tty2, _ := getTTY()
		defer tty2.Close()
		form = form.WithInput(tty2).WithOutput(tty2)
	}

	err := form.Run()
	if err != nil {
		return "", err
	}

	return selected, nil
}

// GetRefinement prompts the user for additional guidance
func GetRefinement() (string, error) {
	var refinement string

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("What else should I know?").
				Placeholder("e.g., use ripgrep instead of grep").
				Value(&refinement),
		),
	)

	// Use /dev/tty directly to bypass shell redirections
	if tty, err := getTTY(); err == nil {
		defer tty.Close()
		form = form.WithInput(tty).WithOutput(tty)
	}

	err := form.Run()
	if err != nil {
		return "", err
	}

	return refinement, nil
}

// ShowError displays an error message
func ShowError(msg string) {
	fmt.Fprintf(os.Stderr, "Error: %s\n", msg)
}

// ShowCommand displays the command that will be executed (to stderr, keeping stdout clean)
func ShowCommand(cmd string) {
	fmt.Fprintln(os.Stderr, cmd)
}

// RunSetupWizard runs the first-time setup wizard and returns the config
func RunSetupWizard() (*config.Config, error) {
	// Use /dev/tty for output to bypass redirections
	tty, ttyErr := getTTY()
	if ttyErr == nil {
		defer tty.Close()
		fmt.Fprintln(tty, "Welcome to term-llm! Let's get you set up.\n")
	} else {
		fmt.Fprintln(os.Stderr, "Welcome to term-llm! Let's get you set up.\n")
	}

	var provider string

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Which LLM provider do you want to use?").
				Options(
					huh.NewOption("Anthropic (Claude)", "anthropic"),
					huh.NewOption("OpenAI", "openai"),
				).
				Value(&provider),
		),
	)

	if ttyErr == nil {
		tty2, _ := getTTY() // need fresh handle after form might close it
		defer tty2.Close()
		form = form.WithInput(tty2).WithOutput(tty2)
	}

	if err := form.Run(); err != nil {
		return nil, err
	}

	// Check for env var
	var envVar string
	var apiKey string
	switch provider {
	case "anthropic":
		envVar = "ANTHROPIC_API_KEY"
		apiKey = os.Getenv(envVar)
	case "openai":
		envVar = "OPENAI_API_KEY"
		apiKey = os.Getenv(envVar)
	}

	if apiKey == "" {
		return nil, fmt.Errorf("%s environment variable is not set\n\nPlease set it:\n  export %s=your-api-key", envVar, envVar)
	}

	cfg := &config.Config{
		Provider: provider,
		Anthropic: config.AnthropicConfig{
			Model: "claude-sonnet-4-5",
		},
		OpenAI: config.OpenAIConfig{
			Model: "gpt-5.2",
		},
	}

	// Save the config
	if err := config.Save(cfg); err != nil {
		return nil, fmt.Errorf("failed to save config: %w", err)
	}

	path, _ := config.GetConfigPath()
	if tty, err := getTTY(); err == nil {
		fmt.Fprintf(tty, "Config saved to %s\n\n", path)
		tty.Close()
	} else {
		fmt.Fprintf(os.Stderr, "Config saved to %s\n\n", path)
	}

	// Reload to pick up the env var
	return config.Load()
}

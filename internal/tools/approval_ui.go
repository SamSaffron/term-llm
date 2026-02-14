package tools

import (
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"
	"golang.org/x/term"
)

// ApprovalChoice represents a user's approval selection.
type ApprovalChoice int

const (
	ApprovalChoiceDeny      ApprovalChoice = iota // Deny the request
	ApprovalChoiceOnce                            // Allow once, no memory
	ApprovalChoiceFile                            // Allow this file only (session)
	ApprovalChoiceDirectory                       // Allow this directory (session)
	ApprovalChoiceRepoRead                        // Allow read for entire repo (remembered)
	ApprovalChoiceRepoWrite                       // Allow write for entire repo (remembered)
	ApprovalChoicePattern                         // Allow shell pattern in repo (remembered)
	ApprovalChoiceCommand                         // Allow this specific command (session)
	ApprovalChoiceCancelled                       // User cancelled with esc/ctrl+c
)

// ApprovalResult contains the result of an approval prompt.
type ApprovalResult struct {
	Choice     ApprovalChoice
	Path       string // Selected path (for file/directory)
	Pattern    string // Selected pattern (for shell)
	SaveToRepo bool   // Whether to save to project approvals
	Cancelled  bool   // Whether user cancelled
}

// ApprovalOption represents a single option in the approval UI.
type ApprovalOption struct {
	Label       string         // Display text
	Description string         // Explanation text
	Choice      ApprovalChoice // The choice this option represents
	Path        string         // Path for directory/file choices
	Pattern     string         // Pattern for shell choices
	SaveToRepo  bool           // Whether this saves to project
}

// Theme colors for approval UI
var (
	approvalReadColor  = lipgloss.Color("10")  // green for read
	approvalWriteColor = lipgloss.Color("208") // orange for write
	approvalTextColor  = lipgloss.Color("15")  // white
	approvalMutedColor = lipgloss.Color("245") // gray
)

// ApprovalModel is the bubbletea model for approval prompts.
// It can be embedded in a parent TUI for inline rendering.
type ApprovalModel struct {
	title       string           // "Read Access Request" or "Write Access Request" or "Shell Command Request"
	path        string           // The path or command being requested
	repoInfo    *GitRepoInfo     // Git repo info (nil if not in repo)
	options     []ApprovalOption // Available choices
	cursor      int              // Currently selected option
	width       int              // Terminal width
	isWrite     bool             // Whether this is a write request
	isShell     bool             // Whether this is a shell request
	Done        bool             // Prompt completed
	result      ApprovalResult   // The result of the prompt
	accentColor lipgloss.Color   // Color for the border accent
}

// Result returns the user's selection after the prompt is done.
func (m *ApprovalModel) Result() ApprovalResult {
	return m.result
}

// IsDone returns true if the user has made a selection.
func (m *ApprovalModel) IsDone() bool {
	return m.Done
}

// SetWidth updates the width for rendering.
func (m *ApprovalModel) SetWidth(width int) {
	m.width = width
}

// NewEmbeddedApprovalModel creates an approval model for file access that can be embedded in a parent TUI.
func NewEmbeddedApprovalModel(path string, isWrite bool, width int) *ApprovalModel {
	// Detect git repo
	repoInfo := DetectGitRepo(path)
	var repoInfoPtr *GitRepoInfo
	if repoInfo.IsRepo {
		repoInfoPtr = &repoInfo
	}

	title := "Read Access Request"
	accentColor := approvalReadColor
	if isWrite {
		title = "Write Access Request"
		accentColor = approvalWriteColor
	}

	options := buildFileOptions(path, repoInfoPtr, isWrite)

	return &ApprovalModel{
		title:       title,
		path:        path,
		repoInfo:    repoInfoPtr,
		options:     options,
		cursor:      0,
		width:       width,
		isWrite:     isWrite,
		accentColor: accentColor,
	}
}

// NewEmbeddedShellApprovalModel creates an approval model for shell commands that can be embedded in a parent TUI.
func NewEmbeddedShellApprovalModel(command string, width int) *ApprovalModel {
	// Detect git repo from current directory
	cwd, _ := os.Getwd()
	repoInfo := DetectGitRepo(cwd)
	var repoInfoPtr *GitRepoInfo
	if repoInfo.IsRepo {
		repoInfoPtr = &repoInfo
	}

	options := buildShellOptions(command, repoInfoPtr)

	return &ApprovalModel{
		title:       "Shell Command Request",
		path:        command,
		repoInfo:    repoInfoPtr,
		options:     options,
		cursor:      0,
		width:       width,
		isShell:     true,
		accentColor: approvalWriteColor, // Shell commands use write color
	}
}

// newApprovalModel creates a new approval model for file access (internal use).
func newApprovalModel(path string, repoInfo *GitRepoInfo, isWrite bool) *ApprovalModel {
	width := 80
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		width = w
	}

	title := "Read Access Request"
	accentColor := approvalReadColor
	if isWrite {
		title = "Write Access Request"
		accentColor = approvalWriteColor
	}

	options := buildFileOptions(path, repoInfo, isWrite)

	return &ApprovalModel{
		title:       title,
		path:        path,
		repoInfo:    repoInfo,
		options:     options,
		cursor:      0,
		width:       width,
		isWrite:     isWrite,
		accentColor: accentColor,
	}
}

// newShellApprovalModel creates a new approval model for shell commands (internal use).
func newShellApprovalModel(command string, repoInfo *GitRepoInfo) *ApprovalModel {
	width := 80
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		width = w
	}

	options := buildShellOptions(command, repoInfo)

	return &ApprovalModel{
		title:       "Shell Command Request",
		path:        command,
		repoInfo:    repoInfo,
		options:     options,
		cursor:      0,
		width:       width,
		isShell:     true,
		accentColor: approvalWriteColor, // Shell commands use write color
	}
}

// buildFileOptions creates the options for a file access prompt.
func buildFileOptions(path string, repoInfo *GitRepoInfo, isWrite bool) []ApprovalOption {
	var options []ApprovalOption
	accessType := "read"
	if isWrite {
		accessType = "write"
	}

	// Get directory for the file
	dir := getDirectoryForApproval(path)

	if repoInfo != nil && repoInfo.IsRepo {
		// In a git repo - offer remembered options
		relPath := GetRelativePath(path, repoInfo.Root)
		relDir := GetRelativePath(dir, repoInfo.Root)

		// Option 1: Approve entire repo (remembered)
		repoChoice := ApprovalChoiceRepoRead
		if isWrite {
			repoChoice = ApprovalChoiceRepoWrite
		}
		options = append(options, ApprovalOption{
			Label:       fmt.Sprintf("Allow %s for entire repo", accessType),
			Description: fmt.Sprintf("Approve all files in %s (remembered)", repoInfo.RepoName),
			Choice:      repoChoice,
			Path:        repoInfo.Root,
			SaveToRepo:  true,
		})

		// Option 2: Approve directory (session only)
		options = append(options, ApprovalOption{
			Label:       fmt.Sprintf("Allow %s for this directory", accessType),
			Description: fmt.Sprintf("Approve %s (session only)", relDir),
			Choice:      ApprovalChoiceDirectory,
			Path:        dir,
			SaveToRepo:  false,
		})

		// Option 3: Approve this file only (session only)
		options = append(options, ApprovalOption{
			Label:       fmt.Sprintf("Allow this file only"),
			Description: fmt.Sprintf("Approve %s (session only)", relPath),
			Choice:      ApprovalChoiceFile,
			Path:        path,
			SaveToRepo:  false,
		})
	} else {
		// Not in a git repo - session only options
		// Option 1: Approve directory (session only)
		options = append(options, ApprovalOption{
			Label:       fmt.Sprintf("Allow %s for this directory", accessType),
			Description: fmt.Sprintf("Approve %s (session only)", dir),
			Choice:      ApprovalChoiceDirectory,
			Path:        dir,
			SaveToRepo:  false,
		})

		// Option 2: Approve this file only (session only)
		options = append(options, ApprovalOption{
			Label:       fmt.Sprintf("Allow this file only"),
			Description: fmt.Sprintf("Approve %s (session only)", path),
			Choice:      ApprovalChoiceFile,
			Path:        path,
			SaveToRepo:  false,
		})
	}

	// Option: Allow once (no memory)
	options = append(options, ApprovalOption{
		Label:       "Allow once",
		Description: "Single access, no memory",
		Choice:      ApprovalChoiceOnce,
		SaveToRepo:  false,
	})

	// Option: Deny
	options = append(options, ApprovalOption{
		Label:       "Deny",
		Description: "Block this access request",
		Choice:      ApprovalChoiceDeny,
		SaveToRepo:  false,
	})

	return options
}

// buildShellOptions creates the options for a shell command prompt.
func buildShellOptions(command string, repoInfo *GitRepoInfo) []ApprovalOption {
	var options []ApprovalOption
	pattern := GenerateShellPattern(command)

	if repoInfo != nil && repoInfo.IsRepo {
		// In a git repo - offer remembered options
		// Option 1: Approve pattern in repo (remembered)
		options = append(options, ApprovalOption{
			Label:       fmt.Sprintf("Allow \"%s\" pattern", pattern),
			Description: fmt.Sprintf("Approve matching commands in %s (remembered)", repoInfo.RepoName),
			Choice:      ApprovalChoicePattern,
			Pattern:     pattern,
			SaveToRepo:  true,
		})
	}

	// Option: Allow this specific command (session only)
	options = append(options, ApprovalOption{
		Label:       "Allow this specific command",
		Description: fmt.Sprintf("Approve \"%s\" (session only)", truncateCmdDisplay(command, 40)),
		Choice:      ApprovalChoiceCommand,
		Pattern:     command,
		SaveToRepo:  false,
	})

	// Option: Allow once (no memory)
	options = append(options, ApprovalOption{
		Label:       "Allow once",
		Description: "Single execution, no memory",
		Choice:      ApprovalChoiceOnce,
		SaveToRepo:  false,
	})

	// Option: Deny
	options = append(options, ApprovalOption{
		Label:       "Deny",
		Description: "Block this command",
		Choice:      ApprovalChoiceDeny,
		SaveToRepo:  false,
	})

	return options
}

// truncateCmdDisplay truncates a command for display with a custom max length.
func truncateCmdDisplay(cmd string, maxLen int) string {
	if len(cmd) <= maxLen {
		return cmd
	}
	return cmd[:maxLen-3] + "..."
}

func (m *ApprovalModel) Init() tea.Cmd {
	return nil
}

// Update handles messages for standalone tea.Program use (calls tea.Quit on completion).
func (m *ApprovalModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.Done = true
			m.result = ApprovalResult{
				Choice:    ApprovalChoiceCancelled,
				Cancelled: true,
			}
			return m, tea.Quit

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			} else {
				m.cursor = len(m.options) - 1
			}

		case "down", "j":
			if m.cursor < len(m.options)-1 {
				m.cursor++
			} else {
				m.cursor = 0
			}

		case "enter", " ":
			opt := m.options[m.cursor]
			m.Done = true
			m.result = ApprovalResult{
				Choice:     opt.Choice,
				Path:       opt.Path,
				Pattern:    opt.Pattern,
				SaveToRepo: opt.SaveToRepo,
			}
			return m, tea.Quit

		// Quick number selection (1-9)
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			idx := int(msg.String()[0] - '1')
			if idx < len(m.options) {
				m.cursor = idx
				opt := m.options[m.cursor]
				m.Done = true
				m.result = ApprovalResult{
					Choice:     opt.Choice,
					Path:       opt.Path,
					Pattern:    opt.Pattern,
					SaveToRepo: opt.SaveToRepo,
				}
				return m, tea.Quit
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
	}

	return m, nil
}

// UpdateEmbedded handles messages for embedded use (does not call tea.Quit).
// Returns true if the model finished (user made a selection or cancelled).
func (m *ApprovalModel) UpdateEmbedded(msg tea.Msg) bool {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.Done = true
			m.result = ApprovalResult{
				Choice:    ApprovalChoiceCancelled,
				Cancelled: true,
			}
			return true

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			} else {
				m.cursor = len(m.options) - 1
			}

		case "down", "j":
			if m.cursor < len(m.options)-1 {
				m.cursor++
			} else {
				m.cursor = 0
			}

		case "enter", " ":
			opt := m.options[m.cursor]
			m.Done = true
			m.result = ApprovalResult{
				Choice:     opt.Choice,
				Path:       opt.Path,
				Pattern:    opt.Pattern,
				SaveToRepo: opt.SaveToRepo,
			}
			return true

		// Quick number selection (1-9)
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			idx := int(msg.String()[0] - '1')
			if idx < len(m.options) {
				m.cursor = idx
				opt := m.options[m.cursor]
				m.Done = true
				m.result = ApprovalResult{
					Choice:     opt.Choice,
					Path:       opt.Path,
					Pattern:    opt.Pattern,
					SaveToRepo: opt.SaveToRepo,
				}
				return true
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
	}

	return false
}

func (m *ApprovalModel) View() string {
	if m.Done {
		return ""
	}

	var b strings.Builder

	// innerWidth accounts for border (1) + paddingLeft (1) + paddingRight (2) = 4
	innerWidth := m.width - 4
	if innerWidth < 20 {
		innerWidth = 20
	}

	// Build styles with accent color
	containerStyle := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderLeft(true).
		BorderForeground(m.accentColor).
		PaddingLeft(1).
		PaddingRight(2).
		PaddingTop(1).
		PaddingBottom(1).
		Width(m.width)

	titleStyle := lipgloss.NewStyle().
		Foreground(m.accentColor).
		Bold(true).
		MarginBottom(1)

	pathStyle := lipgloss.NewStyle().
		Foreground(approvalTextColor).
		MarginBottom(1)

	repoStyle := lipgloss.NewStyle().
		Foreground(approvalMutedColor).
		MarginBottom(1)

	optionStyle := lipgloss.NewStyle().
		Foreground(approvalTextColor)

	selectedStyle := lipgloss.NewStyle().
		Foreground(m.accentColor)

	descStyle := lipgloss.NewStyle().
		Foreground(approvalMutedColor).
		PaddingLeft(3)

	helpStyle := lipgloss.NewStyle().
		Foreground(approvalMutedColor).
		MarginTop(1)

	// Title
	b.WriteString(titleStyle.Render(m.title))
	b.WriteString("\n")

	// Path/command being requested
	b.WriteString(pathStyle.Render(wordwrap.String(m.path, innerWidth)))
	b.WriteString("\n")

	// Repo info (if applicable)
	if m.repoInfo != nil && m.repoInfo.IsRepo {
		b.WriteString(repoStyle.Render(fmt.Sprintf("Git repository: %s", m.repoInfo.RepoName)))
		b.WriteString("\n")
	}

	b.WriteString("\n")

	// Options
	for i, opt := range m.options {
		isSelected := i == m.cursor

		// Option line: "1. Label"
		style := optionStyle
		if isSelected {
			style = selectedStyle
		}

		prefix := fmt.Sprintf("%d. ", i+1)
		if isSelected {
			prefix = "> " + prefix[0:len(prefix)-2] // Remove space, will re-add
			prefix = fmt.Sprintf("> %d. ", i+1)
		} else {
			prefix = fmt.Sprintf("  %d. ", i+1)
		}

		b.WriteString(style.Render(wordwrap.String(prefix+opt.Label, innerWidth)))
		b.WriteString("\n")

		// Description (descStyle has PaddingLeft(3), so reduce wrap width)
		b.WriteString(descStyle.Render(wordwrap.String(opt.Description, innerWidth-3)))
		b.WriteString("\n")
	}

	// Help bar
	helpText := "↑↓ select  1-" + fmt.Sprintf("%d", len(m.options)) + " quick  enter confirm  esc cancel"
	b.WriteString(helpStyle.Render(helpText))

	return containerStyle.Render(b.String())
}

// RenderSummary returns a summary of the user's choice for display after the UI closes.
func (m *ApprovalModel) RenderSummary() string {
	if m.result.Cancelled {
		return ""
	}

	var b strings.Builder
	checkStyle := lipgloss.NewStyle().Foreground(m.accentColor)
	labelStyle := lipgloss.NewStyle().Foreground(approvalMutedColor)
	valueStyle := lipgloss.NewStyle().Foreground(approvalTextColor)

	opt := m.options[m.cursor]

	b.WriteString(checkStyle.Render("✓ "))
	b.WriteString(labelStyle.Render(m.title + ": "))
	b.WriteString(valueStyle.Render(opt.Label))

	containerStyle := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderLeft(true).
		BorderForeground(m.accentColor).
		PaddingLeft(1).
		PaddingRight(2)

	return "\n" + containerStyle.Render(b.String()) + "\n"
}

// getApprovalTTY opens /dev/tty for direct terminal access.
func getApprovalTTY() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

// RunFileApprovalUI displays the approval UI for file access and returns the result.
func RunFileApprovalUI(path string, isWrite bool) (ApprovalResult, error) {
	tty, err := getApprovalTTY()
	if err != nil {
		return ApprovalResult{Cancelled: true}, fmt.Errorf("no TTY available: %w", err)
	}
	defer tty.Close()

	// Detect git repo
	repoInfo := DetectGitRepo(path)
	var repoInfoPtr *GitRepoInfo
	if repoInfo.IsRepo {
		repoInfoPtr = &repoInfo
	}

	// Get terminal width
	width := 80
	if w, _, err := term.GetSize(int(tty.Fd())); err == nil && w > 0 {
		width = w
	}

	m := newApprovalModel(path, repoInfoPtr, isWrite)
	m.width = width

	p := tea.NewProgram(m, tea.WithInput(tty), tea.WithOutput(tty))

	finalModel, err := p.Run()
	if err != nil {
		return ApprovalResult{Cancelled: true}, err
	}

	result := finalModel.(*ApprovalModel)

	// Print summary to TTY so it persists
	if !result.result.Cancelled {
		fmt.Fprint(tty, result.RenderSummary())
	}

	return result.result, nil
}

// RunShellApprovalUI displays the approval UI for shell commands and returns the result.
func RunShellApprovalUI(command string) (ApprovalResult, error) {
	tty, err := getApprovalTTY()
	if err != nil {
		return ApprovalResult{Cancelled: true}, fmt.Errorf("no TTY available: %w", err)
	}
	defer tty.Close()

	// Detect git repo from current directory
	cwd, _ := os.Getwd()
	repoInfo := DetectGitRepo(cwd)
	var repoInfoPtr *GitRepoInfo
	if repoInfo.IsRepo {
		repoInfoPtr = &repoInfo
	}

	// Get terminal width
	width := 80
	if w, _, err := term.GetSize(int(tty.Fd())); err == nil && w > 0 {
		width = w
	}

	m := newShellApprovalModel(command, repoInfoPtr)
	m.width = width

	p := tea.NewProgram(m, tea.WithInput(tty), tea.WithOutput(tty))

	finalModel, err := p.Run()
	if err != nil {
		return ApprovalResult{Cancelled: true}, err
	}

	result := finalModel.(*ApprovalModel)

	// Print summary to TTY so it persists
	if !result.result.Cancelled {
		fmt.Fprint(tty, result.RenderSummary())
	}

	return result.result, nil
}

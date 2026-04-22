package mcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tuiutil"
	"golang.org/x/term"
)

// SamplingApprovalChoice represents a user's approval selection for sampling.
type SamplingApprovalChoice int

const (
	SamplingChoiceDeny      SamplingApprovalChoice = iota // Deny the request
	SamplingChoiceAllow                                   // Allow for this session
	SamplingChoiceCancelled                               // User cancelled
)

// SamplingHandler handles sampling/createMessage requests from MCP servers.
type SamplingHandler struct {
	provider        llm.Provider
	model           string
	serverConfigs   map[string]ServerConfig
	approvedServers map[string]bool // Session-scoped approval tracking
	yoloMode        bool
	mu              sync.Mutex
}

// NewSamplingHandler creates a new sampling handler.
func NewSamplingHandler(provider llm.Provider, model string) *SamplingHandler {
	return &SamplingHandler{
		provider:        provider,
		model:           model,
		serverConfigs:   make(map[string]ServerConfig),
		approvedServers: make(map[string]bool),
	}
}

// SetYoloMode enables or disables yolo mode (auto-approve all sampling requests).
func (h *SamplingHandler) SetYoloMode(enabled bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.yoloMode = enabled
}

// SetServerConfig sets the configuration for a specific server.
func (h *SamplingHandler) SetServerConfig(name string, config ServerConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.serverConfigs[name] = config
}

// Handle processes a sampling/createMessage request from an MCP server.
func (h *SamplingHandler) Handle(ctx context.Context, serverName string, req *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
	h.mu.Lock()
	config := h.serverConfigs[serverName]
	yoloMode := h.yoloMode
	approved := h.approvedServers[serverName]
	h.mu.Unlock()

	// Check if sampling is enabled for this server
	if !config.Sampling.IsSamplingEnabled() {
		return nil, fmt.Errorf("sampling is disabled for server %s", serverName)
	}

	// Determine if we need to prompt for approval
	autoApprove := config.Sampling != nil && config.Sampling.AutoApprove
	needsApproval := !yoloMode && !approved && !autoApprove

	if needsApproval {
		choice, err := h.promptForApproval(serverName, req.Params)
		if err != nil {
			return nil, fmt.Errorf("approval prompt failed: %w", err)
		}

		switch choice {
		case SamplingChoiceDeny, SamplingChoiceCancelled:
			return nil, fmt.Errorf("sampling request denied for server %s", serverName)
		case SamplingChoiceAllow:
			h.mu.Lock()
			h.approvedServers[serverName] = true
			h.mu.Unlock()
		}
	}

	// Convert MCP messages to llm.Messages
	messages := convertSamplingMessages(req.Params.Messages)

	// Add system prompt if provided
	if req.Params.SystemPrompt != "" {
		messages = append([]llm.Message{llm.SystemText(req.Params.SystemPrompt)}, messages...)
	}

	// Determine provider and model to use
	provider := h.provider
	model := h.model

	// Apply server-specific overrides (provider/model overrides would require
	// creating new providers, which is beyond the scope of this handler)
	if config.Sampling != nil && config.Sampling.Model != "" {
		model = config.Sampling.Model
	}

	// Determine max tokens
	maxTokens := int(req.Params.MaxTokens)
	if config.Sampling != nil && config.Sampling.MaxTokens > 0 && (maxTokens == 0 || config.Sampling.MaxTokens < maxTokens) {
		maxTokens = config.Sampling.MaxTokens
	}

	// Build the LLM request
	llmReq := llm.Request{
		Model:           model,
		Messages:        messages,
		MaxOutputTokens: maxTokens,
	}

	// Set temperature if provided
	if req.Params.Temperature > 0 {
		llmReq.Temperature = float32(req.Params.Temperature)
	}

	// Stream the response
	stream, err := provider.Stream(ctx, llmReq)
	if err != nil {
		return nil, fmt.Errorf("failed to start LLM stream: %w", err)
	}
	defer stream.Close()

	// Collect the response
	var responseText strings.Builder
	var stopReason string

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("stream error: %w", err)
		}

		switch event.Type {
		case llm.EventTextDelta:
			responseText.WriteString(event.Text)
		case llm.EventDone:
			stopReason = "endTurn"
		case llm.EventError:
			if event.Err != nil {
				return nil, event.Err
			}
		}
	}

	if stopReason == "" {
		stopReason = "endTurn"
	}

	return &mcp.CreateMessageResult{
		Content:    &mcp.TextContent{Text: responseText.String()},
		Model:      provider.Name() + "/" + model,
		Role:       "assistant",
		StopReason: stopReason,
	}, nil
}

// convertSamplingMessages converts MCP SamplingMessages to llm.Messages.
func convertSamplingMessages(msgs []*mcp.SamplingMessage) []llm.Message {
	var result []llm.Message
	for _, m := range msgs {
		role := llm.RoleUser
		if m.Role == "assistant" {
			role = llm.RoleAssistant
		}

		var text string
		switch c := m.Content.(type) {
		case *mcp.TextContent:
			text = c.Text
		default:
			// For other content types, skip or handle as needed
			continue
		}

		result = append(result, llm.Message{
			Role:  role,
			Parts: []llm.Part{{Type: llm.PartText, Text: text}},
		})
	}
	return result
}

// promptForApproval shows an interactive approval prompt for sampling requests.
func (h *SamplingHandler) promptForApproval(serverName string, params *mcp.CreateMessageParams) (SamplingApprovalChoice, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return SamplingChoiceCancelled, fmt.Errorf("no TTY available: %w", err)
	}
	defer tty.Close()

	width := 80
	if w, _, err := term.GetSize(int(tty.Fd())); err == nil && w > 0 {
		width = w
	}

	m := newSamplingApprovalModel(serverName, params, width)
	p := tea.NewProgram(m, tea.WithInput(tty), tea.WithOutput(tty))

	finalModel, err := p.Run()
	if err != nil {
		return SamplingChoiceCancelled, err
	}

	result := finalModel.(samplingApprovalModel)

	// Print summary to TTY so it persists
	if !result.cancelled {
		fmt.Fprint(tty, result.renderSummary())
	}

	return result.choice, nil
}

// Theme colors for sampling approval UI
var (
	samplingColor      = lipgloss.Color("33")  // blue for sampling
	samplingTextColor  = lipgloss.Color("15")  // white
	samplingMutedColor = lipgloss.Color("245") // gray
)

// samplingApprovalModel is the bubbletea model for sampling approval prompts.
type samplingApprovalModel struct {
	serverName   string
	systemPrompt string
	msgCount     int
	maxTokens    int64
	cursor       int
	width        int
	done         bool
	cancelled    bool
	choice       SamplingApprovalChoice
}

// newSamplingApprovalModel creates a new sampling approval model.
func newSamplingApprovalModel(serverName string, params *mcp.CreateMessageParams, width int) samplingApprovalModel {
	return samplingApprovalModel{
		serverName:   serverName,
		systemPrompt: params.SystemPrompt,
		msgCount:     len(params.Messages),
		maxTokens:    params.MaxTokens,
		cursor:       0,
		width:        width,
	}
}

func (m samplingApprovalModel) Init() tea.Cmd {
	return nil
}

func (m samplingApprovalModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.done = true
			m.cancelled = true
			m.choice = SamplingChoiceCancelled
			return m, tea.Quit

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			} else {
				m.cursor = 1 // Wrap to Deny
			}

		case "down", "j":
			if m.cursor < 1 {
				m.cursor++
			} else {
				m.cursor = 0 // Wrap to Allow
			}

		case "enter", " ", "space":
			m.done = true
			if m.cursor == 0 {
				m.choice = SamplingChoiceAllow
			} else {
				m.choice = SamplingChoiceDeny
			}
			return m, tea.Quit

		case "1":
			m.done = true
			m.choice = SamplingChoiceAllow
			return m, tea.Quit

		case "2":
			m.done = true
			m.choice = SamplingChoiceDeny
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
	}

	return m, nil
}

func (m samplingApprovalModel) View() tea.View {
	if m.done {
		return tea.NewView("")
	}

	var b strings.Builder

	containerStyle := tuiutil.AccentPanelStyle(samplingColor)
	titleStyle := tuiutil.AccentTitleStyle(samplingColor)
	labelStyle := lipgloss.NewStyle().
		Foreground(samplingMutedColor)
	valueStyle := lipgloss.NewStyle().
		Foreground(samplingTextColor)
	optionStyle := lipgloss.NewStyle().
		Foreground(samplingTextColor)
	selectedStyle := lipgloss.NewStyle().
		Foreground(samplingColor)
	helpStyle := tuiutil.MutedHelpStyle(samplingMutedColor)

	// Title
	b.WriteString(titleStyle.Render("MCP Sampling Request"))
	b.WriteString("\n")

	// Server name
	b.WriteString(labelStyle.Render("Server: "))
	b.WriteString(valueStyle.Render(m.serverName))
	b.WriteString("\n")

	// System prompt (truncated)
	if m.systemPrompt != "" {
		prompt := m.systemPrompt
		if len(prompt) > 100 {
			prompt = prompt[:97] + "..."
		}
		b.WriteString(labelStyle.Render("System: "))
		b.WriteString(valueStyle.Render(prompt))
		b.WriteString("\n")
	}

	// Message count
	b.WriteString(labelStyle.Render("Messages: "))
	b.WriteString(valueStyle.Render(fmt.Sprintf("%d", m.msgCount)))
	b.WriteString("\n")

	// Max tokens
	if m.maxTokens > 0 {
		b.WriteString(labelStyle.Render("Max tokens: "))
		b.WriteString(valueStyle.Render(fmt.Sprintf("%d", m.maxTokens)))
		b.WriteString("\n")
	}

	b.WriteString("\n")

	// Options
	options := []string{"Allow for session", "Deny"}
	for i, opt := range options {
		isSelected := i == m.cursor
		style := optionStyle
		if isSelected {
			style = selectedStyle
		}

		prefix := tuiutil.NumberedOptionPrefix(i, isSelected)
		b.WriteString(style.Render(prefix + opt))
		b.WriteString("\n")
	}

	// Help bar
	b.WriteString(helpStyle.Render(tuiutil.QuickSelectHelpText(len(options))))

	return tea.NewView(containerStyle.Render(b.String()))
}

func (m samplingApprovalModel) renderSummary() string {
	if m.cancelled {
		return ""
	}

	var b strings.Builder
	checkStyle := lipgloss.NewStyle().Foreground(samplingColor)
	labelStyle := lipgloss.NewStyle().Foreground(samplingMutedColor)
	valueStyle := lipgloss.NewStyle().Foreground(samplingTextColor)

	b.WriteString(checkStyle.Render("✓ "))
	b.WriteString(labelStyle.Render("MCP Sampling: "))
	if m.choice == SamplingChoiceAllow {
		b.WriteString(valueStyle.Render(fmt.Sprintf("Allowed for %s (session)", m.serverName)))
	} else {
		b.WriteString(valueStyle.Render(fmt.Sprintf("Denied for %s", m.serverName)))
	}

	return "\n" + tuiutil.CompactAccentPanelStyle(samplingColor).Render(b.String()) + "\n"
}

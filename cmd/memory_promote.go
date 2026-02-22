package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/spf13/cobra"
)

const (
	defaultRecentMaxBytes          = 200000
	defaultInitialPromoteLookback  = 7 * 24 * time.Hour
	memoryPromoteSystemInstruction = "You are a memory curator. Given recently changed memory fragments and the current recent.md, produce an updated recent.md that incorporates the new/changed information. Be concise. Preserve existing entries that are still relevant. Output ONLY the raw markdown content for recent.md — no commentary, no code fences."
)

var (
	memoryPromoteSince          time.Duration
	memoryPromoteRecentMaxBytes int
	memoryPromoteModel          string
)

type memoryPromoteOptions struct {
	Agent          string
	Since          time.Duration
	RecentMaxBytes int
	Model          string
	DryRun         bool
	QuietNothing   bool
}

var memoryPromoteCmd = &cobra.Command{
	Use:   "promote",
	Short: "Promote recently changed fragments into recent.md",
	RunE:  runMemoryPromote,
}

func init() {
	memoryPromoteCmd.Flags().DurationVar(&memoryPromoteSince, "since", 0, "Override promote lookback window (e.g. 6h)")
	memoryPromoteCmd.Flags().IntVar(&memoryPromoteRecentMaxBytes, "recent-max-bytes", defaultRecentMaxBytes, "Maximum bytes to keep in recent.md")
	memoryPromoteCmd.Flags().StringVar(&memoryPromoteModel, "model", "", "Override model used for promote")
}

func runMemoryPromote(cmd *cobra.Command, args []string) error {
	if memoryPromoteRecentMaxBytes <= 0 {
		return fmt.Errorf("--recent-max-bytes must be > 0")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	if err := applyProviderOverridesWithAgent(cfg, cfg.Ask.Provider, cfg.Ask.Model, "", "", ""); err != nil {
		return err
	}
	if strings.TrimSpace(memoryPromoteModel) != "" {
		cfg.ApplyOverrides("", strings.TrimSpace(memoryPromoteModel))
	}

	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}
	engine := newEngine(provider, cfg)

	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	_, err = runMemoryPromoteFlow(context.Background(), cfg, engine, store, memoryPromoteOptions{
		Agent:          strings.TrimSpace(memoryAgent),
		Since:          memoryPromoteSince,
		RecentMaxBytes: memoryPromoteRecentMaxBytes,
		Model:          strings.TrimSpace(memoryPromoteModel),
		DryRun:         memoryDryRun,
	})
	return err
}

func runMemoryPromoteFlow(ctx context.Context, cfg *config.Config, engine *llm.Engine, store *memorydb.Store, opts memoryPromoteOptions) (int, error) {
	agentName := strings.TrimSpace(opts.Agent)
	if agentName == "" {
		agentName = resolveMemoryAgent("")
	}

	maxBytes := opts.RecentMaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultRecentMaxBytes
	}

	now := time.Now().UTC()
	cutoff, err := resolvePromotionCutoff(ctx, store, agentName, opts.Since, now)
	if err != nil {
		return 0, err
	}

	fragments, err := store.ListFragments(ctx, memorydb.ListOptions{Agent: agentName, Since: &cutoff})
	if err != nil {
		return 0, fmt.Errorf("list fragments for promote: %w", err)
	}

	changed := make([]memorydb.Fragment, 0, len(fragments))
	for _, frag := range fragments {
		if frag.UpdatedAt.After(cutoff) {
			changed = append(changed, frag)
		}
	}
	if len(changed) == 0 {
		if !opts.QuietNothing {
			fmt.Println("Nothing to promote.")
		}
		return 0, nil
	}

	recentPath, err := resolveAgentRecentPath(cfg, agentName)
	if err != nil {
		return 0, err
	}

	existingRecent, err := loadRecentContent(recentPath)
	if err != nil {
		return 0, err
	}

	prompt := buildPromotePrompt(changed, existingRecent)
	updatedRecent, err := runMemoryPromoteRequest(ctx, engine, strings.TrimSpace(opts.Model), prompt)
	if err != nil {
		return 0, err
	}
	updatedRecent = truncatePromotedRecent(updatedRecent, maxBytes)

	if opts.DryRun {
		fmt.Print(updatedRecent)
		if !strings.HasSuffix(updatedRecent, "\n") {
			fmt.Println()
		}
		return len(changed), nil
	}

	if err := os.MkdirAll(filepath.Dir(recentPath), 0755); err != nil {
		return 0, fmt.Errorf("create memory directory: %w", err)
	}
	if err := os.WriteFile(recentPath, []byte(updatedRecent), 0644); err != nil {
		return 0, fmt.Errorf("write recent.md: %w", err)
	}

	if err := store.SetMeta(ctx, memoryPromoteMetaKey(agentName), now.Format(time.RFC3339)); err != nil {
		return 0, fmt.Errorf("update last promoted timestamp: %w", err)
	}

	fmt.Printf("Promoted %d fragments into recent.md\n", len(changed))
	return len(changed), nil
}

func shouldRunAutoPromote(ctx context.Context, store *memorydb.Store, agent string, every time.Duration) (bool, error) {
	if every <= 0 {
		return true, nil
	}
	agent = strings.TrimSpace(agent)
	if agent == "" {
		agent = resolveMemoryAgent("")
	}

	value, err := store.GetMeta(ctx, memoryPromoteMetaKey(agent))
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(value) == "" {
		return true, nil
	}

	lastPromotedAt, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return true, nil
	}

	return time.Since(lastPromotedAt) >= every, nil
}

func resolvePromotionCutoff(ctx context.Context, store *memorydb.Store, agent string, since time.Duration, now time.Time) (time.Time, error) {
	if since > 0 {
		return now.Add(-since), nil
	}

	lastPromoted, err := store.GetMeta(ctx, memoryPromoteMetaKey(agent))
	if err != nil {
		return time.Time{}, fmt.Errorf("get last promoted timestamp: %w", err)
	}
	if strings.TrimSpace(lastPromoted) == "" {
		return now.Add(-defaultInitialPromoteLookback), nil
	}

	parsed, err := time.Parse(time.RFC3339, lastPromoted)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: invalid last promoted timestamp for %s (%q), defaulting to 7d lookback\n", agent, lastPromoted)
		return now.Add(-defaultInitialPromoteLookback), nil
	}
	return parsed, nil
}

func resolveAgentRecentPath(cfg *config.Config, agentName string) (string, error) {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		agentName = resolveMemoryAgent("")
	}

	agent, err := LoadAgent(agentName, cfg)
	if err == nil && agent != nil {
		sourcePath := strings.TrimSpace(agent.SourcePath)
		if sourcePath != "" && !strings.HasPrefix(sourcePath, "builtin:") {
			return filepath.Join(sourcePath, "memory", "recent.md"), nil
		}
	}

	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config dir: %w", err)
	}
	return filepath.Join(configDir, "agents", agentName, "memory", "recent.md"), nil
}

func loadRecentContent(recentPath string) (string, error) {
	data, err := os.ReadFile(recentPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read current recent.md: %w", err)
	}
	return string(data), nil
}

func buildPromotePrompt(changed []memorydb.Fragment, existingRecent string) string {
	var b strings.Builder
	for _, frag := range changed {
		b.WriteString("## ")
		b.WriteString(frag.Path)
		b.WriteString("\n\n")
		b.WriteString(strings.TrimSpace(frag.Content))
		b.WriteString("\n\n---\n\n")
	}
	b.WriteString("## Current recent.md:\n\n")
	b.WriteString(existingRecent)
	return b.String()
}

func runMemoryPromoteRequest(ctx context.Context, engine *llm.Engine, model, prompt string) (string, error) {
	req := llm.Request{
		Model: strings.TrimSpace(model),
		Messages: []llm.Message{
			llm.SystemText(memoryPromoteSystemInstruction),
			llm.UserText(prompt),
		},
		MaxTurns: 1,
		DebugRaw: debugRaw,
	}

	stream, err := engine.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var b strings.Builder
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch ev.Type {
		case llm.EventTextDelta:
			b.WriteString(ev.Text)
		case llm.EventError:
			if ev.Err != nil {
				return "", ev.Err
			}
		}
	}

	return strings.TrimSpace(b.String()), nil
}

func truncatePromotedRecent(content string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	raw := []byte(content)
	if len(raw) <= maxBytes {
		return content
	}
	trimmed := raw[:maxBytes]
	if idx := bytes.LastIndexByte(trimmed, '\n'); idx > 0 {
		trimmed = trimmed[:idx]
	}
	for len(trimmed) > 0 && !utf8.Valid(trimmed) {
		trimmed = trimmed[:len(trimmed)-1]
	}
	return string(trimmed)
}

func memoryPromoteMetaKey(agent string) string {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		agent = resolveMemoryAgent("")
	}
	return "last_promoted_at_" + agent
}

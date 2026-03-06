package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/spf13/cobra"
)

const (
	defaultMemoryUpdateRecentMaxInputChars = 120000
	defaultMemoryUpdateRecentTargetTokens  = 4000
	memoryUpdateRecentHighWaterPct         = 120 // high water mark = target * 120 / 100
	memoryUpdateRecentCharsPerToken        = 4   // chars-per-token estimate

	memoryUpdateRecentUserCharCap      = 2000
	memoryUpdateRecentAssistantCharCap = 300
)

var (
	memoryUpdateRecentMaxInputChars int
	memoryUpdateRecentTargetTokens  int
	memoryUpdateRecentModel         string
	memoryUpdateRecentFile          string
)

var memoryUpdateRecentCmd = &cobra.Command{
	Use:   "update-recent",
	Short: "Update recent.md with terse summaries of new session activity",
	RunE:  runMemoryUpdateRecent,
}

type memoryUpdateRecentSession struct {
	ID        string
	Number    int64
	Status    session.SessionStatus
	UpdatedAt time.Time
}

func init() {
	memoryUpdateRecentCmd.Flags().IntVar(&memoryUpdateRecentMaxInputChars, "max-input-chars", defaultMemoryUpdateRecentMaxInputChars, "Max chars of session text to include in the prompt")
	memoryUpdateRecentCmd.Flags().IntVar(&memoryUpdateRecentTargetTokens, "target-recent-tokens", defaultMemoryUpdateRecentTargetTokens, "Target size of recent.md in tokens (~4 chars/token); high water mark is +20%")
	memoryUpdateRecentCmd.Flags().StringVar(&memoryUpdateRecentModel, "model", "", "Override model used for update-recent")
	memoryUpdateRecentCmd.Flags().StringVarP(&memoryUpdateRecentFile, "file", "f", "", "Path to recent.md file (overrides default agent path)")
}

func runMemoryUpdateRecent(cmd *cobra.Command, args []string) error {
	if memoryUpdateRecentMaxInputChars <= 0 {
		return fmt.Errorf("--max-input-chars must be > 0")
	}
	if memoryUpdateRecentTargetTokens <= 0 {
		return fmt.Errorf("--target-recent-tokens must be > 0")
	}

	targetChars := memoryUpdateRecentTargetTokens * memoryUpdateRecentCharsPerToken
	highWaterChars := targetChars * memoryUpdateRecentHighWaterPct / 100

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := applyProviderOverridesWithAgent(cfg, cfg.Ask.Provider, cfg.Ask.Model, "", "", ""); err != nil {
		return err
	}

	agentName := resolveMemoryAgent("")
	ctx := context.Background()

	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	sessStore, err := openReadOnlySessionStore(cfg)
	if err != nil {
		return err
	}
	defer sessStore.Close()

	current, err := sessStore.GetCurrent(ctx)
	if err != nil {
		return fmt.Errorf("get current session: %w", err)
	}

	if _, err := readLastUpdatedRecentAt(ctx, store, agentName); err != nil {
		return err
	}

	sessions, err := listUpdateRecentSessions(ctx, sessStore, agentName, current)
	if err != nil {
		return err
	}

	currentID := ""
	if current != nil {
		currentID = current.ID
	}

	trackedOffsets := map[string]int{}
	var inputBuilder strings.Builder

	for _, sess := range sessions {
		if currentID != "" && sess.ID == currentID {
			continue
		}

		startOffset, err := readUpdateRecentOffset(ctx, store, sess.ID)
		if err != nil {
			return err
		}

		messages, err := sessStore.GetMessages(ctx, sess.ID, 0, startOffset)
		if err != nil {
			return fmt.Errorf("get messages for session %s: %w", sess.ID, err)
		}
		if len(messages) == 0 {
			continue
		}

		block := formatUpdateRecentSessionBlock(sess, messages)
		if block == "" {
			continue
		}

		if inputBuilder.Len() > 0 {
			inputBuilder.WriteString("\n\n---\n\n")
		}
		inputBuilder.WriteString(block)

		trackedOffsets[sess.ID] = startOffset + len(messages)
		if inputBuilder.Len() >= memoryUpdateRecentMaxInputChars {
			break
		}
	}

	if inputBuilder.Len() == 0 {
		if memoryDryRun {
			return nil
		}
		if err := store.SetMeta(ctx, memoryUpdateRecentMetaKey(agentName), time.Now().UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("update last update-recent timestamp: %w", err)
		}
		return nil
	}

	recentPath, err := resolveUpdateRecentPath(cfg, agentName)
	if err != nil {
		return err
	}
	existingRecent, err := loadRecentContent(recentPath)
	if err != nil {
		return err
	}

	provider, reqModel, err := newMemoryUpdateRecentProvider(cfg, strings.TrimSpace(memoryUpdateRecentModel))
	if err != nil {
		return err
	}
	engine := newEngine(provider, cfg)

	updatedRecent, err := runMemoryUpdateRecentRequest(ctx, engine, reqModel, inputBuilder.String(), existingRecent, memoryUpdateRecentTargetTokens, targetChars)
	if err != nil {
		return err
	}
	updatedRecent, err = fitUpdatedRecentWithinBudget(ctx, engine, reqModel, updatedRecent, memoryUpdateRecentTargetTokens, targetChars, highWaterChars)
	if err != nil {
		return err
	}

	if memoryDryRun {
		fmt.Print(updatedRecent)
		if !strings.HasSuffix(updatedRecent, "\n") {
			fmt.Println()
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(recentPath), 0755); err != nil {
		return fmt.Errorf("create memory directory: %w", err)
	}
	if err := os.WriteFile(recentPath, []byte(updatedRecent), 0644); err != nil {
		return fmt.Errorf("write recent.md: %w", err)
	}

	sessionIDs := make([]string, 0, len(trackedOffsets))
	for sessionID := range trackedOffsets {
		sessionIDs = append(sessionIDs, sessionID)
	}
	sort.Strings(sessionIDs)
	for _, sessionID := range sessionIDs {
		if err := store.SetMeta(ctx, updateRecentOffsetMetaKey(sessionID), strconv.Itoa(trackedOffsets[sessionID])); err != nil {
			return fmt.Errorf("persist update-recent offset for session %s: %w", sessionID, err)
		}
	}

	now := time.Now().UTC()
	if err := store.SetMeta(ctx, memoryUpdateRecentMetaKey(agentName), now.Format(time.RFC3339)); err != nil {
		return fmt.Errorf("update last update-recent timestamp: %w", err)
	}

	return nil
}

// resolveUpdateRecentPath returns the path to recent.md, honouring the -f flag.
func resolveUpdateRecentPath(cfg *config.Config, agentName string) (string, error) {
	if f := strings.TrimSpace(memoryUpdateRecentFile); f != "" {
		return f, nil
	}
	return resolveAgentRecentPath(cfg, agentName)
}

func newMemoryUpdateRecentProvider(cfg *config.Config, modelOverride string) (llm.Provider, string, error) {
	modelOverride = strings.TrimSpace(modelOverride)
	if modelOverride == "" {
		provider, err := llm.NewFastProvider(cfg, cfg.DefaultProvider)
		if err != nil {
			return nil, "", err
		}
		if provider != nil {
			return provider, "", nil
		}
		provider, err = llm.NewProvider(cfg)
		if err != nil {
			return nil, "", err
		}
		return provider, "", nil
	}

	if strings.Contains(modelOverride, ":") {
		overrideProvider, overrideModel, err := llm.ParseProviderModel(modelOverride, cfg)
		if err != nil {
			return nil, "", err
		}
		cfg.ApplyOverrides(overrideProvider, overrideModel)
		provider, err := llm.NewProviderByName(cfg, overrideProvider, overrideModel)
		if err != nil {
			return nil, "", err
		}
		return provider, strings.TrimSpace(overrideModel), nil
	}

	cfg.ApplyOverrides("", modelOverride)
	provider, err := llm.NewProviderByName(cfg, cfg.DefaultProvider, modelOverride)
	if err != nil {
		return nil, "", err
	}
	return provider, modelOverride, nil
}

func runMemoryUpdateRecentRequest(ctx context.Context, engine *llm.Engine, model, sessionSnippets, existingRecent string, targetTokens, targetChars int) (string, error) {
	req := llm.Request{
		Model: strings.TrimSpace(model),
		Messages: []llm.Message{
			llm.SystemText(memoryUpdateRecentSystemPrompt(targetTokens, targetChars)),
			llm.UserText(memoryUpdateRecentUserPrompt(sessionSnippets, existingRecent)),
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

func runMemoryCompactRecentRequest(ctx context.Context, engine *llm.Engine, model, candidateRecent string, targetTokens, targetChars int) (string, error) {
	req := llm.Request{
		Model: strings.TrimSpace(model),
		Messages: []llm.Message{
			llm.SystemText(memoryCompactRecentSystemPrompt(targetTokens, targetChars)),
			llm.UserText(memoryCompactRecentUserPrompt(candidateRecent)),
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

func fitUpdatedRecentWithinBudget(ctx context.Context, engine *llm.Engine, model, updatedRecent string, targetTokens, targetChars, highWaterChars int) (string, error) {
	if len([]byte(updatedRecent)) <= highWaterChars {
		return updatedRecent, nil
	}

	compacted, err := runMemoryCompactRecentRequest(ctx, engine, model, updatedRecent, targetTokens, targetChars)
	if err != nil {
		return "", err
	}
	if len([]byte(compacted)) <= highWaterChars {
		return compacted, nil
	}
	return truncatePromotedRecent(compacted, targetChars), nil
}

func memoryUpdateRecentSystemPrompt(targetTokens, targetChars int) string {
	return fmt.Sprintf(`You are a memory curator. You maintain a concise recent memory file for an AI assistant named Jarvis.

Given RECENT SESSION SNIPPETS and the CURRENT RECENT MEMORY, output the complete updated memory file integrating the new activity.

The file is a compact current-state working memory, not a changelog.

Rules:
- Target total output: ~%d tokens (~%d characters). Treat this as a soft ceiling on the whole document.
- Use this structure when relevant: ## Current state, then compact ### sections such as Deployed / configured, Active work, Open issues / quirks, Recent completed work, Temporary notes.
- Prefer latest truth over historical sequence.
- Replace superseded facts instead of keeping old and new versions.
- Drop resolved or stale items unless they still affect current behaviour over the next few days.
- Keep durable long-term facts out unless they are actively relevant right now.
- Be extremely terse: facts only, no prose, no filler.
- Output only the content, no code fences, no commentary.`, targetTokens, targetChars)
}

func memoryCompactRecentSystemPrompt(targetTokens, targetChars int) string {
	return fmt.Sprintf(`You are compacting a recent memory file for Jarvis.

Given a candidate recent.md that is too large, rewrite it into a smaller current-state working memory file.

Rules:
- Target total output: ~%d tokens (~%d characters). Treat this as a hard target.
- Preserve the newest and most actionable facts.
- Prefer latest truth over history; replace superseded facts.
- Drop resolved, duplicated, stale, or low-value detail aggressively.
- Keep the file as compact current-state memory, not a dated log or archive.
- Use short headings and bullets only when they earn their keep.
- Output only the content, no code fences, no commentary.`, targetTokens, targetChars)
}

func memoryUpdateRecentUserPrompt(sessionSnippets, existingRecent string) string {
	return fmt.Sprintf("RECENT SESSION SNIPPETS:\n\n%s\n\n===\n\nCURRENT RECENT MEMORY:\n\n%s", sessionSnippets, existingRecent)
}

func memoryCompactRecentUserPrompt(candidateRecent string) string {
	return fmt.Sprintf("CANDIDATE RECENT MEMORY TO COMPACT:\n\n%s", candidateRecent)
}

func listUpdateRecentSessions(ctx context.Context, sessStore session.Store, agentName string, current *session.Session) ([]memoryUpdateRecentSession, error) {
	complete, err := listCompleteSessions(ctx, sessStore)
	if err != nil {
		return nil, fmt.Errorf("list complete sessions: %w", err)
	}

	seen := map[string]struct{}{}
	sessions := make([]memoryUpdateRecentSession, 0, len(complete)+1)

	for _, summary := range complete {
		sess, err := sessStore.Get(ctx, summary.ID)
		if err != nil {
			return nil, fmt.Errorf("get session %s: %w", summary.ID, err)
		}
		if sess == nil {
			continue
		}
		if resolveMemoryAgent(sess.Agent) != agentName {
			continue
		}

		status := summary.Status
		if status == "" {
			status = sess.Status
		}
		number := summary.Number
		if number == 0 {
			number = sess.Number
		}
		updatedAt := summary.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = sess.UpdatedAt
		}

		sessions = append(sessions, memoryUpdateRecentSession{
			ID:        summary.ID,
			Number:    number,
			Status:    status,
			UpdatedAt: updatedAt,
		})
		seen[summary.ID] = struct{}{}
	}

	if current != nil {
		if _, exists := seen[current.ID]; !exists && resolveMemoryAgent(current.Agent) == agentName {
			sessions = append(sessions, memoryUpdateRecentSession{
				ID:        current.ID,
				Number:    current.Number,
				Status:    current.Status,
				UpdatedAt: current.UpdatedAt,
			})
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].UpdatedAt.Equal(sessions[j].UpdatedAt) {
			if sessions[i].Number == sessions[j].Number {
				return sessions[i].ID > sessions[j].ID
			}
			return sessions[i].Number > sessions[j].Number
		}
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})

	return sessions, nil
}
func formatUpdateRecentSessionBlock(sess memoryUpdateRecentSession, messages []session.Message) string {
	lines := make([]string, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case llm.RoleUser:
			text := truncateUpdateRecentText(msg.TextContent, memoryUpdateRecentUserCharCap, false)
			if text != "" {
				lines = append(lines, "User: "+text)
			}
		case llm.RoleAssistant:
			text := truncateUpdateRecentText(msg.TextContent, memoryUpdateRecentAssistantCharCap, true)
			if text != "" {
				lines = append(lines, "Assistant: "+text)
			}
		}
	}

	if len(lines) == 0 {
		return ""
	}

	var b strings.Builder
	if sess.Number > 0 {
		b.WriteString(fmt.Sprintf("[Session #%d - %s]\n", sess.Number, updateRecentSessionState(sess.Status)))
	} else {
		b.WriteString(fmt.Sprintf("[Session %s - %s]\n", sess.ID, updateRecentSessionState(sess.Status)))
	}
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func truncateUpdateRecentText(text string, maxChars int, ellipsis bool) string {
	text = strings.TrimSpace(text)
	if text == "" || maxChars <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	trimmed := string(runes[:maxChars])
	if ellipsis {
		return trimmed + "..."
	}
	return trimmed
}

func updateRecentSessionState(status session.SessionStatus) string {
	switch status {
	case session.StatusComplete:
		return "completed"
	case session.StatusActive:
		return "active"
	case session.StatusError:
		return "error"
	case session.StatusInterrupted:
		return "interrupted"
	default:
		state := strings.TrimSpace(string(status))
		if state == "" {
			return "unknown"
		}
		return state
	}
}

func readLastUpdatedRecentAt(ctx context.Context, store *memorydb.Store, agentName string) (time.Time, error) {
	value, err := store.GetMeta(ctx, memoryUpdateRecentMetaKey(agentName))
	if err != nil {
		return time.Time{}, fmt.Errorf("get last update-recent timestamp: %w", err)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: invalid last update-recent timestamp for %s (%q), defaulting to zero\n", agentName, value)
		return time.Time{}, nil
	}
	return parsed, nil
}

func readUpdateRecentOffset(ctx context.Context, store *memorydb.Store, sessionID string) (int, error) {
	value, err := store.GetMeta(ctx, updateRecentOffsetMetaKey(sessionID))
	if err != nil {
		return 0, fmt.Errorf("get update-recent offset for session %s: %w", sessionID, err)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	offset, err := strconv.Atoi(value)
	if err != nil || offset < 0 {
		fmt.Fprintf(os.Stderr, "warning: invalid update-recent offset for session %s (%q), defaulting to 0\n", sessionID, value)
		return 0, nil
	}
	return offset, nil
}

func memoryUpdateRecentMetaKey(agentName string) string {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		agentName = resolveMemoryAgent("")
	}
	return "last_update_recent_at_" + agentName
}

func updateRecentOffsetMetaKey(sessionID string) string {
	return "update_recent_offset_" + strings.TrimSpace(sessionID)
}

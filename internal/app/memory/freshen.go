package memory

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/session"
)

const (
	DefaultUpdateRecentMaxInputChars = 120000
	DefaultUpdateRecentTargetTokens  = 4000
	memoryUpdateRecentHighWaterPct   = 120 // high water mark = target * 120 / 100
	memoryUpdateRecentCharsPerToken  = 4   // chars-per-token estimate

	memoryUpdateRecentUserCharCap      = 2000
	memoryUpdateRecentAssistantCharCap = 2000

	DefaultUpdateRecentFragmentChars = 10000
)

// UpdateRecentOptions configures `memory update-recent`.
type UpdateRecentOptions struct {
	Env
	MaxInputChars int
	TargetTokens  int
	FragmentChars int
	Model         string
	File          string
}

type memoryUpdateRecentSession struct {
	ID        string
	Number    int64
	Status    session.SessionStatus
	UpdatedAt time.Time
}

// UpdateRecent updates recent.md with terse summaries of new session activity.
func UpdateRecent(opts UpdateRecentOptions) error {
	env := opts.Env
	if opts.MaxInputChars <= 0 {
		return fmt.Errorf("--max-input-chars must be > 0")
	}
	if opts.TargetTokens <= 0 {
		return fmt.Errorf("--target-recent-tokens must be > 0")
	}

	targetChars := opts.TargetTokens * memoryUpdateRecentCharsPerToken
	highWaterChars := targetChars * memoryUpdateRecentHighWaterPct / 100

	cfg, err := env.LoadConfig()
	if err != nil {
		return err
	}
	env, err = env.withProtection(cfg)
	if err != nil {
		return err
	}

	if err := env.ApplyAskProviderOverrides(cfg); err != nil {
		return err
	}

	agentName := env.ResolveAgent("")
	ctx := context.Background()

	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	sessStore, err := openReadOnlySessionStore(env, cfg)
	if err != nil {
		return err
	}
	defer sessStore.Close()

	recentPath, err := resolveUpdateRecentPath(env, opts.File, cfg, agentName)
	if err != nil {
		return err
	}

	var engine *engine.Engine
	var reqModel string
	buildUpdatedRecent := func(sessionSnippets, existingRecent, fragmentsText string) (string, error) {
		if engine == nil {
			provider, model, err := newMemoryUpdateRecentProvider(cfg, strings.TrimSpace(opts.Model))
			if err != nil {
				return "", err
			}
			reqModel = model
			engine = env.NewEngine(provider, cfg)
		}

		updatedRecent, err := runMemoryUpdateRecentRequest(ctx, engine, reqModel, protectionOrDefault(env.protection).sanitize(sessionSnippets), protectionOrDefault(env.protection).sanitize(existingRecent), protectionOrDefault(env.protection).sanitize(fragmentsText), opts.TargetTokens, targetChars, env.debugRaw(), env.protection)
		if err != nil {
			return "", err
		}
		if err := protectionOrDefault(env.protection).check(updatedRecent); err != nil {
			return "", err
		}
		updatedRecent = strings.TrimSpace(updatedRecent)
		updatedRecent, err = fitUpdatedRecentWithinBudget(ctx, engine, reqModel, updatedRecent, opts.TargetTokens, targetChars, highWaterChars, env.debugRaw(), env.protection)
		if err != nil {
			return "", err
		}
		if err := protectionOrDefault(env.protection).check(updatedRecent); err != nil {
			return "", err
		}
		updatedRecent = strings.TrimSpace(updatedRecent)
		if err := protectionOrDefault(env.protection).recent(updatedRecent, highWaterChars); err != nil {
			return "", err
		}
		return updatedRecent, nil
	}

	if env.DryRun {
		lastUpdatedRecentAt, err := readLastUpdatedRecentAt(ctx, store, agentName)
		if err != nil {
			return err
		}
		input, err := collectMemoryUpdateRecentInput(ctx, store, sessStore, agentName, lastUpdatedRecentAt, opts.MaxInputChars, env.protection)
		if err != nil {
			return err
		}
		if strings.TrimSpace(input.Text) == "" {
			return nil
		}
		existingRecent, err := loadRecentContent(recentPath)
		if err != nil {
			return err
		}
		fragmentsText, err := loadRecentFragmentsText(ctx, store, agentName, opts.FragmentChars, env.protection)
		if err != nil {
			return err
		}
		updatedRecent, err := buildUpdatedRecent(input.Text, existingRecent, fragmentsText)
		if err != nil {
			return err
		}
		fmt.Print(updatedRecent)
		if !strings.HasSuffix(updatedRecent, "\n") {
			fmt.Println()
		}
		return nil
	}

	if err := withLockedRecentFile(recentPath, func() error {
		lastUpdatedRecentAt, err := readLastUpdatedRecentAt(ctx, store, agentName)
		if err != nil {
			return err
		}
		// Capture the next checkpoint before enumerating sessions so activity that
		// arrives during this run remains eligible for the next one.
		nextUpdatedRecentAt := time.Now().UTC()

		input, err := collectMemoryUpdateRecentInput(ctx, store, sessStore, agentName, lastUpdatedRecentAt, opts.MaxInputChars, env.protection)
		if err != nil {
			return err
		}
		nextUpdatedRecentAt = nextUpdateRecentCheckpoint(nextUpdatedRecentAt, input)
		if strings.TrimSpace(input.Text) == "" {
			if input.Exhausted {
				if err := store.SetMeta(ctx, memoryUpdateRecentMetaKey(agentName), nextUpdatedRecentAt.Format(time.RFC3339)); err != nil {
					return fmt.Errorf("update last update-recent timestamp: %w", err)
				}
			}
			return nil
		}

		fragmentsText, err := loadRecentFragmentsText(ctx, store, agentName, opts.FragmentChars, env.protection)
		if err != nil {
			return err
		}
		existingRecent, err := loadRecentContent(recentPath)
		if err != nil {
			return err
		}

		updatedRecent, err := buildUpdatedRecent(input.Text, existingRecent, fragmentsText)
		if err != nil {
			return err
		}
		if err := writeRecentFileAtomically(recentPath, updatedRecent); err != nil {
			return fmt.Errorf("write recent.md: %w", err)
		}

		sessionIDs := make([]string, 0, len(input.Offsets))
		for sessionID := range input.Offsets {
			sessionIDs = append(sessionIDs, sessionID)
		}
		sort.Strings(sessionIDs)
		for _, sessionID := range sessionIDs {
			if err := store.SetMeta(ctx, updateRecentOffsetMetaKey(sessionID), strconv.Itoa(input.Offsets[sessionID])); err != nil {
				return fmt.Errorf("persist update-recent offset for session %s: %w", sessionID, err)
			}
		}

		if input.Exhausted {
			if err := store.SetMeta(ctx, memoryUpdateRecentMetaKey(agentName), nextUpdatedRecentAt.Format(time.RFC3339)); err != nil {
				return fmt.Errorf("update last update-recent timestamp: %w", err)
			}
		}

		return nil
	}); err != nil {
		return err
	}

	return nil
}

// resolveUpdateRecentPath returns the path to recent.md, honouring the -f flag.
func resolveUpdateRecentPath(env Env, file string, cfg *config.Config, agentName string) (string, error) {
	if f := strings.TrimSpace(file); f != "" {
		return f, nil
	}
	return resolveAgentRecentPath(env, cfg, agentName)
}

func newMemoryUpdateRecentProvider(cfg *config.Config, modelOverride string) (core.Provider, string, error) {
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

func runMemoryUpdateRecentRequest(ctx context.Context, engine *engine.Engine, model, sessionSnippets, existingRecent, fragmentsText string, targetTokens, targetChars int, debugRaw bool, protection *memoryProtection) (string, error) {
	debugRaw = protection.prepareDebug(engine, debugRaw)
	req := core.Request{
		Model:     strings.TrimSpace(model),
		Ephemeral: true,
		Messages: []core.Message{
			core.SystemText(memoryUpdateRecentSystemPrompt(targetTokens, targetChars)),
			core.UserText(protection.sanitize(memoryUpdateRecentUserPrompt(sessionSnippets, existingRecent, fragmentsText))),
		},
		MaxTurns: 1,
		DebugRaw: debugRaw,
	}

	stream, err := engine.Stream(ctx, req)
	if err != nil {
		return "", protection.providerError("recent-memory provider operation failed", err)
	}
	defer stream.Close()

	var b strings.Builder
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", protection.providerError("recent-memory provider operation failed", err)
		}
		switch ev.Type {
		case core.EventTextDelta:
			b.WriteString(ev.Text)
		case core.EventError:
			if ev.Err != nil {
				return "", protection.providerError("recent-memory provider operation failed", ev.Err)
			}
		}
	}

	content := b.String()
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("update-recent generation returned empty text; recent.md and checkpoints are unchanged; retry the update")
	}
	return content, nil
}

func runMemoryCompactRecentRequest(ctx context.Context, engine *engine.Engine, model, candidateRecent string, targetTokens, targetChars int, debugRaw bool, protection *memoryProtection) (string, error) {
	debugRaw = protection.prepareDebug(engine, debugRaw)
	req := core.Request{
		Model:     strings.TrimSpace(model),
		Ephemeral: true,
		Messages: []core.Message{
			core.SystemText(memoryCompactRecentSystemPrompt(targetTokens, targetChars)),
			core.UserText(protection.sanitize(memoryCompactRecentUserPrompt(candidateRecent))),
		},
		MaxTurns: 1,
		DebugRaw: debugRaw,
	}

	stream, err := engine.Stream(ctx, req)
	if err != nil {
		return "", protection.providerError("recent-memory provider operation failed", err)
	}
	defer stream.Close()

	var b strings.Builder
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", protection.providerError("recent-memory provider operation failed", err)
		}
		switch ev.Type {
		case core.EventTextDelta:
			b.WriteString(ev.Text)
		case core.EventError:
			if ev.Err != nil {
				return "", protection.providerError("recent-memory provider operation failed", ev.Err)
			}
		}
	}

	content := b.String()
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("update-recent compaction returned empty text; recent.md and checkpoints are unchanged; retry the update")
	}
	return content, nil
}

func fitUpdatedRecentWithinBudget(ctx context.Context, engine *engine.Engine, model, updatedRecent string, targetTokens, targetChars, highWaterChars int, debugRaw bool, protection *memoryProtection) (string, error) {
	if len([]byte(updatedRecent)) <= highWaterChars {
		return updatedRecent, nil
	}

	compacted, err := runMemoryCompactRecentRequest(ctx, engine, model, updatedRecent, targetTokens, targetChars, debugRaw, protection)
	if err != nil {
		return "", err
	}
	if len([]byte(compacted)) <= highWaterChars {
		return compacted, nil
	}
	return "", errRecentSize
}

func memoryUpdateRecentSystemPrompt(targetTokens, targetChars int) string {
	return fmt.Sprintf(`You are a memory curator. You maintain a concise recent memory file for an AI assistant named Jarvis.

Given RECENT MEMORY FRAGMENTS (distilled facts from past sessions), RECENT SESSION SNIPPETS (raw recent activity), and the CURRENT RECENT MEMORY, output the complete updated memory file integrating the new activity.

The fragments are the primary source of truth for established facts. The session snippets capture the latest activity not yet in fragments.

The file is a compact current-state working memory, not a changelog.

Rules:
- Target total output: ~%d tokens (~%d characters). Treat this as a soft ceiling on the whole document.
- Use this structure when relevant: ## Current state, then compact ### memory-palace rooms such as User / preferences, Projects, Infrastructure / tools, Active work, Open issues / quirks, Recent completed work, Temporary notes.
- Prefer latest truth over historical sequence.
- Replace superseded facts instead of keeping old and new versions.
- Drop resolved or stale items unless they still affect current behaviour over the next few days.
- Keep durable long-term facts out unless they are actively relevant right now or needed to orient the palace.
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
- Keep stable palace rooms only when they help retrieval; merge tiny or empty rooms.
- Drop resolved, duplicated, stale, or low-value detail aggressively.
- Keep the file as compact current-state memory, not a dated log or archive.
- Use short headings and bullets only when they earn their keep.
- Output only the content, no code fences, no commentary.`, targetTokens, targetChars)
}

func memoryUpdateRecentUserPrompt(sessionSnippets, existingRecent, fragmentsText string) string {
	var b strings.Builder
	if fragmentsText != "" {
		b.WriteString("RECENT MEMORY FRAGMENTS:\n\n")
		b.WriteString(fragmentsText)
		b.WriteString("\n\n===\n\n")
	}
	b.WriteString("RECENT SESSION SNIPPETS:\n\n")
	b.WriteString(sessionSnippets)
	b.WriteString("\n\n===\n\nCURRENT RECENT MEMORY:\n\n")
	b.WriteString(existingRecent)
	return b.String()
}

// loadRecentFragmentsText returns up to maxChars of fragment content for the given agent,
// ordered by most recently created first (newest fragments first).
func loadRecentFragmentsText(ctx context.Context, store *memorydb.Store, agentName string, maxChars int, protection *memoryProtection) (string, error) {
	if maxChars <= 0 {
		return "", nil
	}
	fragments, err := store.ListFragments(ctx, memorydb.ListOptions{
		Agent: agentName,
		Limit: 500, // fetch a generous batch; we'll stop at maxChars
	})
	if err != nil {
		return "", fmt.Errorf("list fragments for update-recent: %w", err)
	}

	var b strings.Builder
	for _, f := range fragments {
		if protection.excluded(f.Path) {
			continue
		}
		f.Path = protection.sanitize(f.Path)
		content := strings.TrimSpace(protection.sanitize(f.Content))
		if content == "" {
			continue
		}
		entry := fmt.Sprintf("[%s]\n%s\n", f.Path, content)
		if b.Len()+len(entry) > maxChars {
			break
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(entry)
	}
	return b.String(), nil
}

func memoryCompactRecentUserPrompt(candidateRecent string) string {
	return fmt.Sprintf("CANDIDATE RECENT MEMORY TO COMPACT:\n\n%s", candidateRecent)
}

// nextUpdateRecentCheckpoint keeps the checkpoint at or before any active
// session deferred by this run, so the next run lists it again.
func nextUpdateRecentCheckpoint(runStart time.Time, input memoryUpdateRecentInput) time.Time {
	if !input.DeferredFrom.IsZero() && input.DeferredFrom.Before(runStart) {
		return input.DeferredFrom.UTC()
	}
	return runStart
}

// listUpdateRecentSessions returns the sessions to read, newest activity first,
// and the earliest UpdatedAt among active sessions deferred because a turn may
// still be writing to them (zero when none were deferred).
func listUpdateRecentSessions(ctx context.Context, sessStore session.Store, agentName string, updatedAtOrAfter time.Time, current *session.Session, now time.Time) ([]memoryUpdateRecentSession, time.Time, error) {
	summaries, err := listMemorySourceSessions(ctx, sessStore, agentName)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("list memory source sessions: %w", err)
	}

	seen := map[string]struct{}{}
	var deferredFrom time.Time
	sessions := make([]memoryUpdateRecentSession, 0, len(summaries)+1)
	for _, summary := range summaries {
		// Keep custom stores that ignore List filters from leaking another
		// agent or an older session into this update. UpdatedAt is compared in
		// Go because existing SQLite rows may contain different UTC offsets.
		if sessionAgentOrDefault(summary.Agent) != agentName || (!updatedAtOrAfter.IsZero() && summary.UpdatedAt.Before(updatedAtOrAfter)) {
			continue
		}
		if activeSessionMayBeWriting(summary, now) {
			if deferredFrom.IsZero() || summary.UpdatedAt.Before(deferredFrom) {
				deferredFrom = summary.UpdatedAt
			}
			continue
		}

		sessions = append(sessions, memoryUpdateRecentSession{
			ID:        summary.ID,
			Number:    summary.Number,
			Status:    summary.Status,
			UpdatedAt: summary.UpdatedAt,
		})
		seen[summary.ID] = struct{}{}
	}

	if current != nil {
		if _, exists := seen[current.ID]; !exists && sessionAgentOrDefault(current.Agent) == agentName && (updatedAtOrAfter.IsZero() || !current.UpdatedAt.Before(updatedAtOrAfter)) {
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

	return sessions, deferredFrom, nil
}

type memoryUpdateRecentInput struct {
	Text      string
	Offsets   map[string]int
	Exhausted bool
	// DeferredFrom is the earliest UpdatedAt of an active session skipped
	// because a turn may still be writing to it. The next checkpoint must not
	// pass it, or the session would be filtered out until its next activity.
	DeferredFrom time.Time
}

func collectMemoryUpdateRecentInput(ctx context.Context, store *memorydb.Store, sessStore session.Store, agentName string, updatedAtOrAfter time.Time, maxInputChars int, protection *memoryProtection) (memoryUpdateRecentInput, error) {
	current, err := sessStore.GetCurrent(ctx)
	if err != nil {
		return memoryUpdateRecentInput{}, fmt.Errorf("get current session: %w", err)
	}

	sessions, deferredFrom, err := listUpdateRecentSessions(ctx, sessStore, agentName, updatedAtOrAfter, current, time.Now())
	if err != nil {
		return memoryUpdateRecentInput{}, err
	}

	currentID := ""
	if current != nil {
		currentID = current.ID
	}

	trackedOffsets := map[string]int{}
	var inputBuilder strings.Builder
	exhausted := true

	for i, sess := range sessions {
		if currentID != "" && sess.ID == currentID {
			continue
		}

		startOffset, err := readUpdateRecentOffset(ctx, store, sess.ID)
		if err != nil {
			return memoryUpdateRecentInput{}, err
		}

		const messagePageSize = 100
		headerWritten := false
		for {
			messages, err := sessStore.GetMessagesFrom(ctx, sess.ID, startOffset, messagePageSize)
			if err != nil {
				return memoryUpdateRecentInput{}, fmt.Errorf("get messages for session %s: %w", sess.ID, err)
			}
			for _, msg := range messages {
				msg.TextContent = protection.sanitize(msg.TextContent)
				stop, appendErr := appendUpdateRecentMessage(&inputBuilder, maxInputChars, sess, &headerWritten, msg)
				if appendErr != nil {
					return memoryUpdateRecentInput{}, appendErr
				}
				if stop {
					// Retry this message in the next batch without advancing its offset.
					return memoryUpdateRecentInput{Text: inputBuilder.String(), Offsets: trackedOffsets, Exhausted: false, DeferredFrom: deferredFrom}, nil
				}
				// Include skipped tool/empty rows, but never unread messages.
				startOffset = msg.Sequence + 1
				trackedOffsets[sess.ID] = startOffset
			}
			if len(messages) < messagePageSize {
				break
			}
			if inputBuilder.Len() >= maxInputChars {
				return memoryUpdateRecentInput{Text: inputBuilder.String(), Offsets: trackedOffsets, Exhausted: false, DeferredFrom: deferredFrom}, nil
			}
		}
		if inputBuilder.Len() >= maxInputChars {
			exhausted = i == len(sessions)-1
			break
		}
	}

	return memoryUpdateRecentInput{Text: inputBuilder.String(), Offsets: trackedOffsets, Exhausted: exhausted, DeferredFrom: deferredFrom}, nil
}

// appendUpdateRecentMessage stops before a message that cannot fit after earlier
// messages, so the caller can retry it without advancing the durable offset.
func appendUpdateRecentMessage(inputBuilder *strings.Builder, maxInputChars int, sess memoryUpdateRecentSession, headerWritten *bool, msg session.Message) (bool, error) {
	line := formatUpdateRecentMessage(msg)
	if line == "" {
		return false, nil
	}
	prefix := "\n"
	if !*headerWritten {
		prefix = formatUpdateRecentSessionHeader(sess) + "\n"
		if inputBuilder.Len() > 0 {
			prefix = "\n\n---\n\n" + prefix
		}
	}
	remaining := maxInputChars - inputBuilder.Len() - len(prefix)
	if len(line) > remaining {
		if inputBuilder.Len() > 0 {
			return true, nil
		}
		// A single message must not prevent progress. Keep the header and
		// role, and truncate its text to the byte budget on a UTF-8 boundary.
		role, text, _ := strings.Cut(line, ": ")
		textBudget := remaining - len(role) - 2
		if textBudget <= 0 {
			return false, fmt.Errorf("max-input-chars is too small for session %s header and message", sess.ID)
		}
		for textBudget > 0 && !utf8.RuneStart(text[textBudget]) {
			textBudget--
		}
		if textBudget == 0 {
			return false, fmt.Errorf("max-input-chars is too small for session %s message text", sess.ID)
		}
		line = role + ": " + text[:textBudget]
	}
	inputBuilder.WriteString(prefix)
	inputBuilder.WriteString(line)
	*headerWritten = true
	return false, nil
}

func formatUpdateRecentSessionBlock(sess memoryUpdateRecentSession, messages []session.Message) string {
	lines := make([]string, 0, len(messages))
	for _, msg := range messages {
		if line := formatUpdateRecentMessage(msg); line != "" {
			lines = append(lines, line)
		}
	}

	if len(lines) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(formatUpdateRecentSessionHeader(sess) + "\n")
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func formatUpdateRecentSessionHeader(sess memoryUpdateRecentSession) string {
	if sess.Number > 0 {
		return fmt.Sprintf("[Session #%d - %s]", sess.Number, updateRecentSessionState(sess.Status))
	}
	return fmt.Sprintf("[Session %s - %s]", sess.ID, updateRecentSessionState(sess.Status))
}

func formatUpdateRecentMessage(msg session.Message) string {
	switch msg.Role {
	case core.RoleUser:
		if text := truncateUpdateRecentText(msg.TextContent, memoryUpdateRecentUserCharCap, false); text != "" {
			return "User: " + text
		}
	case core.RoleAssistant:
		if text := truncateUpdateRecentText(msg.TextContent, memoryUpdateRecentAssistantCharCap, true); text != "" {
			return "Assistant: " + text
		}
	}
	return ""
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
		agentName = sessionAgentOrDefault("")
	}
	return "update_recent_exhausted_at_" + agentName
}

func updateRecentOffsetMetaKey(sessionID string) string {
	return "update_recent_offset_" + strings.TrimSpace(sessionID)
}

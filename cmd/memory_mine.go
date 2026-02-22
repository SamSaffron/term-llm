package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/embedding"
	"github.com/samsaffron/term-llm/internal/llm"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/spf13/cobra"
)

var (
	memoryMineModel            string
	memoryMineSince            time.Duration
	memoryMineLimit            int
	memoryMineBatchSize        int
	memoryMineIncludeSubagents bool
	memoryMineMaxMessages      int
	memoryMineReadBytes        int
	memoryMineEmbed            bool
	memoryMineEmbedProvider    string
	memoryMinePromote          string
	memoryMinePromoteEvery     time.Duration
	memoryMineHalfLifeDays     float64
)

var memoryMineCmd = &cobra.Command{
	Use:   "mine",
	Short: "Mine completed sessions into memory fragments",
	RunE:  runMemoryMine,
}

const memoryExtractionSystemPrompt = `You are a strict memory extraction engine.

Output must be valid JSON only (no markdown fences, no prose).
Return exactly one object with key "operations".

Goal: extract durable long-term memory from the provided transcript.
Focus on stable facts, decisions, preferences, and technical details worth remembering.
Ignore ephemeral details such as transient errors, one-off debugging steps, and conversational filler.`

type memoryMineCandidate struct {
	Summary session.SessionSummary
	Session *session.Session
	Agent   string
}

type extractionResponse struct {
	Operations []extractionOperation `json:"operations"`
}

type extractionOperation struct {
	Op      string `json:"op"`
	Path    string `json:"path,omitempty"`
	Content string `json:"content,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type transcriptMessage struct {
	Role      string   `json:"role"`
	Text      string   `json:"text,omitempty"`
	ToolCalls []string `json:"tool_calls,omitempty"`
}

type taxonomyEntry struct {
	Path    string `json:"path"`
	Preview string `json:"preview"`
}

func init() {
	memoryMineCmd.Flags().StringVar(&memoryMineModel, "model", "", "Override model used for memory extraction")
	memoryMineCmd.Flags().DurationVar(&memoryMineSince, "since", 0, "Only mine sessions updated within this duration (e.g. 24h)")
	memoryMineCmd.Flags().IntVar(&memoryMineLimit, "limit", 0, "Maximum number of sessions to mine (0 = all)")
	memoryMineCmd.Flags().IntVar(&memoryMineBatchSize, "batch-size", 10, "Number of messages to fetch per pagination request")
	memoryMineCmd.Flags().BoolVar(&memoryMineIncludeSubagents, "include-subagents", false, "Include subagent sessions")
	memoryMineCmd.Flags().IntVar(&memoryMineMaxMessages, "max-messages", 0, "Maximum newly mined messages per session (0 = all)")
	memoryMineCmd.Flags().IntVar(&memoryMineReadBytes, "read-bytes", 2048, "Bytes of existing fragment content to include in taxonomy context")
	memoryMineCmd.Flags().BoolVar(&memoryMineEmbed, "embed", true, "Embed new/updated fragments after mining")
	memoryMineCmd.Flags().StringVar(&memoryMineEmbedProvider, "embed-provider", "", "Override embedding provider used in EMBED phase (optionally provider:model)")
	memoryMineCmd.Flags().StringVar(&memoryMinePromote, "promote", "auto", "Promotion mode: auto|always|never")
	memoryMineCmd.Flags().DurationVar(&memoryMinePromoteEvery, "promote-every", 6*time.Hour, "Minimum interval between auto-promote runs")
	memoryMineCmd.Flags().Float64Var(&memoryMineHalfLifeDays, "half-life", 30.0, "Decay half-life in days for post-mine recalculation")
	memoryMineCmd.RegisterFlagCompletionFunc("embed-provider", EmbedProviderFlagCompletion)
}

func runMemoryMine(cmd *cobra.Command, args []string) error {
	if memoryMineBatchSize <= 0 {
		return fmt.Errorf("--batch-size must be > 0")
	}
	if memoryMineLimit < 0 {
		return fmt.Errorf("--limit must be >= 0")
	}
	if memoryMineMaxMessages < 0 {
		return fmt.Errorf("--max-messages must be >= 0")
	}
	if memoryMineReadBytes < 0 {
		return fmt.Errorf("--read-bytes must be >= 0")
	}

	promoteMode := strings.ToLower(strings.TrimSpace(memoryMinePromote))
	switch promoteMode {
	case "auto", "always", "never":
	default:
		return fmt.Errorf("--promote must be one of: auto, always, never")
	}
	if promoteMode == "auto" && memoryMinePromoteEvery <= 0 {
		return fmt.Errorf("--promote-every must be > 0 when --promote=auto")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	if err := applyProviderOverridesWithAgent(cfg, cfg.Ask.Provider, cfg.Ask.Model, "", "", ""); err != nil {
		return err
	}
	if strings.TrimSpace(memoryMineModel) != "" {
		cfg.ApplyOverrides("", strings.TrimSpace(memoryMineModel))
	}

	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}
	engine := newEngine(provider, cfg)

	memStore, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer memStore.Close()

	sessStore, err := openReadOnlySessionStore(cfg)
	if err != nil {
		return err
	}
	defer sessStore.Close()

	ctx := context.Background()

	currentSession, err := sessStore.GetCurrent(ctx)
	if err != nil {
		return fmt.Errorf("get current session: %w", err)
	}
	currentID := ""
	if currentSession != nil {
		currentID = currentSession.ID
	}

	complete, err := listCompleteSessions(ctx, sessStore)
	if err != nil {
		return fmt.Errorf("list complete sessions: %w", err)
	}

	candidates, err := collectMineCandidates(ctx, sessStore, complete, currentID)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		fmt.Println("No sessions eligible for memory mining.")
	}

	modelName := activeModel(cfg)
	if modelName == "" {
		modelName = "(default model)"
	}
	if len(candidates) > 0 {
		fmt.Printf("Mining %d session(s) with %s / %s\n", len(candidates), provider.Name(), modelName)
	}
	if memoryDryRun {
		fmt.Println("Dry run mode: no database writes will be performed.")
	}

	var totalCreated, totalUpdated, totalSkipped int

	for i, candidate := range candidates {
		state, err := memStore.GetState(ctx, candidate.Session.ID)
		if err != nil {
			return fmt.Errorf("get mining state for session %s: %w", candidate.Session.ID, err)
		}

		startOffset := 0
		if state != nil {
			startOffset = state.LastMinedOffset
		}

		if startOffset >= candidate.Summary.MessageCount {
			fmt.Printf("[%d/%d] #%d already mined (offset %d >= %d)\n",
				i+1, len(candidates), candidate.Summary.Number, startOffset, candidate.Summary.MessageCount)
			continue
		}

		messages, nextOffset, err := loadMessagesForMining(ctx, sessStore, candidate.Session.ID, startOffset)
		if err != nil {
			return fmt.Errorf("load messages for session %s: %w", candidate.Session.ID, err)
		}
		if len(messages) == 0 {
			fmt.Printf("[%d/%d] #%d no new messages to mine\n", i+1, len(candidates), candidate.Summary.Number)
			continue
		}

		existing, err := memStore.ListFragments(ctx, memorydb.ListOptions{Agent: candidate.Agent})
		if err != nil {
			return fmt.Errorf("list existing fragments for agent %s: %w", candidate.Agent, err)
		}

		prompt := buildExtractionPrompt(candidate, startOffset, nextOffset, messages, existing)
		raw, err := runExtractionRequest(ctx, engine, prompt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping session %s batch at offset %d: %v\n", candidate.Session.ID, startOffset, err)
			continue
		}

		ops, err := parseExtractionOperations(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: bad extraction output for session %s at offset %d: %v\nraw: %s\n", candidate.Session.ID, startOffset, err, raw)
			continue
		}

		created, updated, skipped, err := applyExtractionOperations(ctx, memStore, candidate.Agent, ops)
		if err != nil {
			return fmt.Errorf("apply operations for session %s: %w", candidate.Session.ID, err)
		}

		if !memoryDryRun {
			if err := memStore.UpsertState(ctx, &memorydb.MiningState{
				SessionID:       candidate.Session.ID,
				Agent:           candidate.Agent,
				LastMinedOffset: nextOffset,
				MinedAt:         time.Now(),
			}); err != nil {
				return fmt.Errorf("update mining state for session %s: %w", candidate.Session.ID, err)
			}
		}

		totalCreated += created
		totalUpdated += updated
		totalSkipped += skipped

		fmt.Printf("[%d/%d] #%d mined messages [%d,%d): create=%d update=%d skip=%d\n",
			i+1, len(candidates), candidate.Summary.Number, startOffset, nextOffset, created, updated, skipped)
	}

	if memoryDryRun {
		fmt.Println("Dry run mode: skipping image mining phase.")
	} else {
		imageMined := mineGeneratedImages(ctx, memStore)
		if imageMined > 0 {
			fmt.Printf("mined %d image fragment(s)\n", imageMined)
		}
	}

	embeddedCount := 0
	if memoryMineEmbed {
		if memoryDryRun {
			fmt.Println("Dry run mode: skipping EMBED phase.")
		} else {
			embeddedCount, err = runMemoryEmbedPhase(ctx, cfg, memStore)
			if err != nil {
				return err
			}
			fmt.Printf("embedded %d fragments\n", embeddedCount)
		}
	}

	if !memoryDryRun {
		decayAgent := strings.TrimSpace(memoryAgent)
		updated, err := memStore.RecalcDecayScores(ctx, decayAgent, memoryMineHalfLifeDays)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: decay recalc failed: %v\n", err)
		} else if updated > 0 {
			fmt.Printf("decay recalculated for %d fragments\n", updated)
		}
	}

	if promoteMode != "never" {
		if memoryDryRun {
			fmt.Println("Dry run mode: skipping PROMOTE phase.")
		} else {
			for _, promoteAgent := range minePromoteAgents(memoryAgent, candidates) {
				shouldPromote := promoteMode == "always"
				if promoteMode == "auto" {
					shouldPromote, err = shouldRunAutoPromote(ctx, memStore, promoteAgent, memoryMinePromoteEvery)
					if err != nil {
						fmt.Fprintf(os.Stderr, "warning: failed checking auto-promote schedule for %s: %v\n", promoteAgent, err)
						continue
					}
				}
				if !shouldPromote {
					continue
				}

				if _, err := runMemoryPromoteFlow(ctx, cfg, engine, memStore, memoryPromoteOptions{
					Agent:          promoteAgent,
					RecentMaxBytes: defaultRecentMaxBytes,
					Model:          strings.TrimSpace(memoryMineModel),
					DryRun:         false,
					QuietNothing:   true,
				}); err != nil {
					fmt.Fprintf(os.Stderr, "warning: promote failed for %s: %v\n", promoteAgent, err)
				}
			}
		}
	}

	fmt.Printf("Done. create=%d update=%d skip=%d\n", totalCreated, totalUpdated, totalSkipped)
	return nil
}

func minePromoteAgents(globalAgent string, candidates []memoryMineCandidate) []string {
	if strings.TrimSpace(globalAgent) != "" {
		return []string{strings.TrimSpace(globalAgent)}
	}

	seen := map[string]struct{}{}
	agents := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		agent := strings.TrimSpace(candidate.Agent)
		if agent == "" {
			agent = resolveMemoryAgent("")
		}
		if _, exists := seen[agent]; exists {
			continue
		}
		seen[agent] = struct{}{}
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	return agents
}

func collectMineCandidates(ctx context.Context, store session.Store, complete []session.SessionSummary, currentID string) ([]memoryMineCandidate, error) {
	cutoff := time.Time{}
	if memoryMineSince > 0 {
		cutoff = time.Now().Add(-memoryMineSince)
	}

	out := make([]memoryMineCandidate, 0, len(complete))
	agentFilter := strings.TrimSpace(memoryAgent)

	for _, summary := range complete {
		if !cutoff.IsZero() && summary.UpdatedAt.Before(cutoff) {
			continue
		}

		sess, err := store.Get(ctx, summary.ID)
		if err != nil {
			return nil, fmt.Errorf("get session %s: %w", summary.ID, err)
		}
		if sess == nil {
			continue
		}

		if currentID != "" && sess.ID == currentID {
			continue
		}
		if !memoryMineIncludeSubagents && sess.IsSubagent {
			continue
		}
		if hasMemoryMiningTag(sess.Tags) {
			continue
		}
		if agentFilter != "" && strings.TrimSpace(sess.Agent) != agentFilter {
			continue
		}

		out = append(out, memoryMineCandidate{
			Summary: summary,
			Session: sess,
			Agent:   resolveMemoryAgent(sess.Agent),
		})

		if memoryMineLimit > 0 && len(out) >= memoryMineLimit {
			break
		}
	}

	return out, nil
}

func loadMessagesForMining(ctx context.Context, store session.Store, sessionID string, offset int) ([]session.Message, int, error) {
	remaining := memoryMineMaxMessages
	currentOffset := offset
	all := make([]session.Message, 0, memoryMineBatchSize)

	for {
		limit := memoryMineBatchSize
		if remaining > 0 && remaining < limit {
			limit = remaining
		}

		msgs, err := store.GetMessages(ctx, sessionID, limit, currentOffset)
		if err != nil {
			return nil, currentOffset, err
		}
		if len(msgs) == 0 {
			break
		}

		all = append(all, msgs...)
		currentOffset += len(msgs)

		if remaining > 0 {
			remaining -= len(msgs)
			if remaining <= 0 {
				break
			}
		}

		if len(msgs) < limit {
			break
		}
	}

	return all, currentOffset, nil
}

func buildExtractionPrompt(candidate memoryMineCandidate, startOffset, endOffset int, messages []session.Message, existing []memorydb.Fragment) string {
	transcriptJSON, _ := json.MarshalIndent(buildTranscript(messages), "", "  ")
	taxonomyJSON, _ := json.MarshalIndent(buildTaxonomy(existing), "", "  ")

	return fmt.Sprintf(`Session metadata:
- session_id: %s
- session_number: %d
- agent: %s
- mined_message_range: [%d, %d)

Existing fragment taxonomy (path + preview):
%s

Transcript (role + text + tool call names only):
%s

Instructions:
- Extract only durable facts, decisions, preferences, and technical details worth remembering long-term.
- Skip ephemeral content like specific transient errors, one-off debugging output, and conversational filler.
- Avoid duplicates: prefer update when a path already exists in taxonomy.
- Return at most 20 operations.
- Fragment content must stay <= 8192 bytes.
- Path must be relative and must not contain ../ or be absolute.

Return strict JSON only, exactly in this format:
{
  "operations": [
    {"op": "create", "path": "...", "content": "..."},
    {"op": "update", "path": "...", "content": "...", "reason": "..."},
    {"op": "skip", "reason": "..."}
  ]
}
`,
		candidate.Session.ID,
		candidate.Summary.Number,
		candidate.Agent,
		startOffset,
		endOffset,
		string(taxonomyJSON),
		string(transcriptJSON),
	)
}

func buildTaxonomy(fragments []memorydb.Fragment) []taxonomyEntry {
	entries := make([]taxonomyEntry, 0, len(fragments))
	for _, frag := range fragments {
		entries = append(entries, taxonomyEntry{
			Path:    frag.Path,
			Preview: readPrefixBytes(frag.Content, memoryMineReadBytes),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
	return entries
}

func buildTranscript(messages []session.Message) []transcriptMessage {
	out := make([]transcriptMessage, 0, len(messages))

	for _, msg := range messages {
		entry := transcriptMessage{Role: string(msg.Role)}

		if msg.Role != llm.RoleTool {
			if text := strings.TrimSpace(msg.TextContent); text != "" {
				entry.Text = text
			}
		}

		calls := collectToolCallNames(msg)
		if len(calls) > 0 {
			entry.ToolCalls = calls
		}

		if entry.Text == "" && len(entry.ToolCalls) == 0 && msg.Role == llm.RoleTool {
			// Explicitly suppress raw tool output.
			continue
		}

		out = append(out, entry)
	}

	return out
}

func collectToolCallNames(msg session.Message) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, part := range msg.Parts {
		if part.Type != llm.PartToolCall || part.ToolCall == nil {
			continue
		}
		name := strings.TrimSpace(part.ToolCall.Name)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func runExtractionRequest(ctx context.Context, engine *llm.Engine, prompt string) (string, error) {
	req := llm.Request{
		Model:    strings.TrimSpace(memoryMineModel),
		Messages: []llm.Message{llm.SystemText(memoryExtractionSystemPrompt), llm.UserText(prompt)},
		MaxTurns: 1,
		Debug:    false,
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

func parseExtractionOperations(raw string) ([]extractionOperation, error) {
	dec := json.NewDecoder(strings.NewReader(raw))

	var response extractionResponse
	if err := dec.Decode(&response); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing content after JSON object")
	}

	if len(response.Operations) > 20 {
		return nil, fmt.Errorf("too many operations: got %d, max 20", len(response.Operations))
	}

	normalized := make([]extractionOperation, 0, len(response.Operations))
	for i, op := range response.Operations {
		op.Op = strings.ToLower(strings.TrimSpace(op.Op))
		switch op.Op {
		case "create", "update":
			p, err := validateFragmentPath(op.Path)
			if err != nil {
				return nil, fmt.Errorf("op[%d] path invalid: %w", i, err)
			}
			content := strings.TrimSpace(op.Content)
			if content == "" {
				return nil, fmt.Errorf("op[%d] content cannot be empty", i)
			}
			if len([]byte(content)) > 8192 {
				return nil, fmt.Errorf("op[%d] content exceeds 8192 bytes", i)
			}
			op.Path = p
			op.Content = content
		case "skip":
			op.Path = ""
			op.Content = ""
			if strings.TrimSpace(op.Reason) == "" {
				op.Reason = "no durable memory extracted"
			}
		default:
			return nil, fmt.Errorf("op[%d] has invalid op %q", i, op.Op)
		}
		normalized = append(normalized, op)
	}

	return normalized, nil
}

func validateFragmentPath(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || isWindowsAbsPath(p) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	if p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "../") || strings.HasSuffix(p, "/..") {
		return "", fmt.Errorf("path traversal is not allowed")
	}

	clean := path.Clean(p)
	if clean == "." || clean == "" || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid path")
	}

	return clean, nil
}

func isWindowsAbsPath(p string) bool {
	if len(p) < 2 {
		return false
	}
	c := p[0]
	if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
		return p[1] == ':'
	}
	return false
}

func applyExtractionOperations(ctx context.Context, store *memorydb.Store, agent string, ops []extractionOperation) (created, updated, skipped int, err error) {
	for _, op := range ops {
		switch op.Op {
		case "create":
			created++
			if memoryDryRun {
				continue
			}
			createErr := store.CreateFragment(ctx, &memorydb.Fragment{
				Agent:   agent,
				Path:    op.Path,
				Content: op.Content,
				Source:  memorydb.DefaultSourceMine,
			})
			if createErr != nil {
				if isUniqueConstraintError(createErr) {
					ok, upErr := store.UpdateFragment(ctx, agent, op.Path, op.Content)
					if upErr != nil {
						return created, updated, skipped, upErr
					}
					if ok {
						created--
						updated++
						continue
					}
				}
				return created, updated, skipped, createErr
			}
		case "update":
			updated++
			if memoryDryRun {
				continue
			}
			ok, updateErr := store.UpdateFragment(ctx, agent, op.Path, op.Content)
			if updateErr != nil {
				return created, updated, skipped, updateErr
			}
			if !ok {
				// Keep this as a skipped op if target fragment does not exist.
				updated--
				skipped++
			}
		case "skip":
			skipped++
		}
	}
	return created, updated, skipped, nil
}

func mineGeneratedImages(ctx context.Context, store *memorydb.Store) int {
	agents, err := resolveImageMineAgents(ctx, store)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: resolve image mining agents: %v\n", err)
		return 0
	}
	if len(agents) == 0 {
		return 0
	}

	total := 0
	for _, agent := range agents {
		total += mineGeneratedImagesForAgent(ctx, store, agent)
	}
	return total
}

func resolveImageMineAgents(ctx context.Context, store *memorydb.Store) ([]string, error) {
	if strings.TrimSpace(memoryAgent) != "" {
		return []string{strings.TrimSpace(memoryAgent)}, nil
	}
	return store.ListImageAgents(ctx)
}

func mineGeneratedImagesForAgent(ctx context.Context, store *memorydb.Store, agent string) int {
	metaKey := memoryImageMineMetaKey(agent)
	lastValue, err := store.GetMeta(ctx, metaKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: read image mine meta for %s: %v\n", agent, err)
		return 0
	}

	lastTime := time.Time{}
	if strings.TrimSpace(lastValue) != "" {
		parsed, err := time.Parse(time.RFC3339, lastValue)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: invalid image mine timestamp for %s (%q): %v\n", agent, lastValue, err)
		} else {
			lastTime = parsed
		}
	}

	images, err := store.ListImagesSince(ctx, agent, lastTime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: list images for %s: %v\n", agent, err)
		return 0
	}
	if len(images) == 0 {
		return 0
	}

	effectiveAgent := strings.TrimSpace(agent)
	if effectiveAgent == "" {
		effectiveAgent = resolveMemoryAgent("")
	}

	created := 0
	for _, img := range images {
		path := buildImageFragmentPath(img)
		content := buildImageFragmentContent(img)
		if err := upsertImageFragment(ctx, store, effectiveAgent, path, content); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed mining image %s: %v\n", img.ID, err)
			continue
		}
		created++
	}

	lastProcessed := images[len(images)-1].CreatedAt
	if err := store.SetMeta(ctx, metaKey, lastProcessed.Format(time.RFC3339)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: update image mine meta for %s: %v\n", agent, err)
	}

	return created
}

func memoryImageMineMetaKey(agent string) string {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		agent = resolveMemoryAgent("")
	}
	return "last_image_mine_at_" + agent
}

func buildImageFragmentPath(img memorydb.ImageRecord) string {
	createdAt := img.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	date := createdAt.Format("2006-01-02")
	slug := imagePromptSlug(img.Prompt)
	return path.Join("images", date, slug+".md")
}

func buildImageFragmentContent(img memorydb.ImageRecord) string {
	prompt := strings.TrimSpace(img.Prompt)
	if prompt == "" {
		prompt = "(untitled)"
	}
	provider := strings.TrimSpace(img.Provider)
	if provider == "" {
		provider = "unknown"
	}

	dims := "unknown"
	if img.Width > 0 && img.Height > 0 {
		dims = fmt.Sprintf("%dx%d", img.Width, img.Height)
	}

	size := "unknown"
	if img.FileSize > 0 {
		size = formatBytes(int64(img.FileSize))
	}

	createdAt := img.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	return fmt.Sprintf("# Image: %s\n\n- **Path:** %s\n- **Provider:** %s\n- **Generated:** %s\n- **Size:** %s (%s)\n",
		prompt,
		img.OutputPath,
		provider,
		createdAt.Format(time.RFC3339),
		dims,
		size,
	)
}

func imagePromptSlug(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "image"
	}
	runes := []rune(prompt)
	if len(runes) > 40 {
		runes = runes[:40]
	}

	lower := strings.ToLower(string(runes))
	var b strings.Builder
	lastHyphen := false
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			lastHyphen = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case r == ' ' || r == '-' || r == '_':
			if !lastHyphen {
				b.WriteRune('-')
				lastHyphen = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "image"
	}
	return out
}

func upsertImageFragment(ctx context.Context, store *memorydb.Store, agent, fragPath, content string) error {
	if memoryDryRun {
		return nil
	}
	createErr := store.CreateFragment(ctx, &memorydb.Fragment{
		Agent:   agent,
		Path:    fragPath,
		Content: content,
		Source:  memorydb.DefaultSourceMine,
	})
	if createErr == nil {
		return nil
	}
	if isUniqueConstraintError(createErr) {
		_, updateErr := store.UpdateFragment(ctx, agent, fragPath, content)
		return updateErr
	}
	return createErr
}

func runMemoryEmbedPhase(ctx context.Context, cfg *config.Config, store *memorydb.Store) (int, error) {
	providerName, modelName, providerSpec := resolveMemoryEmbeddingProvider(cfg, memoryMineEmbedProvider)
	if providerName == "" || providerSpec == "" {
		fmt.Fprintln(os.Stderr, "warning: embedding provider unavailable, skipping EMBED phase")
		return 0, nil
	}

	embedder, err := embedding.NewEmbeddingProvider(cfg, providerSpec)
	if err != nil {
		if strings.TrimSpace(memoryMineEmbedProvider) != "" {
			return 0, err
		}
		fmt.Fprintf(os.Stderr, "warning: embedding provider initialization failed (%v), skipping EMBED phase\n", err)
		return 0, nil
	}

	if modelName == "" {
		modelName = embedder.DefaultModel()
	}

	fragments, err := store.GetFragmentsNeedingEmbedding(ctx, strings.TrimSpace(memoryAgent), providerName, modelName)
	if err != nil {
		return 0, fmt.Errorf("query fragments needing embedding: %w", err)
	}
	if len(fragments) == 0 {
		return 0, nil
	}

	embeddedCount := 0
	const embedBatchSize = 32
	for i := 0; i < len(fragments); i += embedBatchSize {
		end := i + embedBatchSize
		if end > len(fragments) {
			end = len(fragments)
		}
		batch := fragments[i:end]
		texts := make([]string, len(batch))
		for j, frag := range batch {
			texts[j] = frag.Content
		}

		result, err := embedder.Embed(embedding.EmbedRequest{
			Texts:    texts,
			Model:    modelName,
			TaskType: "RETRIEVAL_DOCUMENT",
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed embedding batch starting at %d: %v\n", i, err)
			continue
		}
		for j, emb := range result.Embeddings {
			if len(emb.Vector) == 0 {
				continue
			}
			frag := batch[j]
			if err := store.UpsertEmbedding(ctx, frag.ID, providerName, modelName, len(emb.Vector), emb.Vector); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to persist embedding for fragment %s: %v\n", frag.ID, err)
				continue
			}
			embeddedCount++
		}
	}

	return embeddedCount, nil
}

func readPrefixBytes(content string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	b := []byte(content)
	if len(b) <= maxBytes {
		return content
	}
	return string(b[:maxBytes]) + "..."
}

package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/session"
)

// MineOptions configures `memory mine`.
type MineOptions struct {
	Env
	Model             string
	Since             time.Duration
	MessageSince      time.Time
	MessageBefore     time.Time
	Limit             int
	BatchSize         int
	IncludeSubagents  bool
	MaxMessages       int
	ReadBytes         int
	Embed             bool
	EmbedProvider     string
	Promote           string
	PromoteEvery      time.Duration
	Insights          bool
	PromptMaxTokens   int
	TaxonomyMaxTokens int
	ToolMaxTurns      int
	MaxOutputTokens   int
	ShowStats         bool
	UseFastModel      bool
}

const memoryExtractionSystemPrompt = `You are a strict memory extraction engine.

Goal: extract durable long-term memory from the provided transcript.
Focus on stable facts, decisions, preferences, and technical details worth remembering.
Ignore ephemeral details such as transient errors, one-off debugging steps, and conversational filler.
Store credential locations, never credential values. A rejected write names its detector; retry corrected content at the same path to resolve it.

Use the provided memory tools to inspect existing fragments and to create or update memory directly.
Prefer updating existing fragments over creating duplicates.
When finished, reply briefly with plain text like "done".`

type memoryMineCandidate struct {
	Summary session.SessionSummary
	Session *session.Session
	Agent   string
}

type memoryMiningStateStore interface {
	ListStates(ctx context.Context) ([]memorydb.MiningState, error)
}

type memoryAgentFragmentStore interface {
	ListFragments(ctx context.Context, opts memorydb.ListOptions) ([]memorydb.Fragment, error)
	GetFragment(ctx context.Context, agent, path string) (*memorydb.Fragment, error)
}

type memoryAgentFragmentCache struct {
	maxTokens  int
	protection *memoryProtection
	corpora    map[string]*memoryAgentFragmentCorpus
}

type memoryAgentFragmentCorpus struct {
	fragments   []memorydb.Fragment
	taxonomyMap string
	byPath      map[string]int
}

func newMemoryAgentFragmentCache(maxTokens int) *memoryAgentFragmentCache {
	return &memoryAgentFragmentCache{
		maxTokens: maxTokens,
		corpora:   map[string]*memoryAgentFragmentCorpus{},
	}
}

func (c *memoryAgentFragmentCache) get(ctx context.Context, store memoryAgentFragmentStore, agent string) ([]memorydb.Fragment, string, error) {
	agent = strings.TrimSpace(agent)
	if corpus, ok := c.corpora[agent]; ok {
		return corpus.fragments, corpus.taxonomyMap, nil
	}

	fragments, err := store.ListFragments(ctx, memorydb.ListOptions{Agent: agent})
	if err != nil {
		return nil, "", err
	}

	corpus := &memoryAgentFragmentCorpus{
		fragments:   fragments,
		taxonomyMap: protectionOrDefault(c.protection).taxonomy(fragments, c.maxTokens),
		byPath:      make(map[string]int, len(fragments)),
	}
	for i, frag := range fragments {
		corpus.byPath[frag.Path] = i
	}
	c.corpora[agent] = corpus
	return corpus.fragments, corpus.taxonomyMap, nil
}

func (c *memoryAgentFragmentCache) applyChanges(ctx context.Context, store memoryAgentFragmentStore, agent string, paths []string) error {
	agent = strings.TrimSpace(agent)
	corpus, ok := c.corpora[agent]
	if !ok || len(paths) == 0 {
		return nil
	}

	for _, fragPath := range paths {
		fragPath = strings.TrimSpace(fragPath)
		if fragPath == "" {
			continue
		}

		frag, err := store.GetFragment(ctx, agent, fragPath)
		if err != nil {
			return fmt.Errorf("get fragment %s: %w", fragPath, err)
		}
		if frag == nil {
			continue
		}

		if idx, exists := corpus.byPath[fragPath]; exists {
			corpus.fragments[idx] = *frag
			continue
		}

		corpus.byPath[fragPath] = len(corpus.fragments)
		corpus.fragments = append(corpus.fragments, *frag)
	}

	corpus.taxonomyMap = protectionOrDefault(c.protection).taxonomy(corpus.fragments, c.maxTokens)
	return nil
}

func (c *memoryAgentFragmentCache) invalidate(agent string) {
	delete(c.corpora, strings.TrimSpace(agent))
}

type transcriptMessage struct {
	Role      string   `json:"role"`
	Text      string   `json:"text,omitempty"`
	ToolCalls []string `json:"tool_calls,omitempty"`
}

// Mine mines complete and active sessions into memory fragments.
func Mine(opts MineOptions) error {
	env := opts.Env
	if err := opts.validateMessageBounds(); err != nil {
		return err
	}
	if opts.BatchSize <= 0 {
		return fmt.Errorf("--batch-size must be > 0")
	}
	if opts.Limit < 0 {
		return fmt.Errorf("--limit must be >= 0")
	}
	if opts.MaxMessages < 0 {
		return fmt.Errorf("--max-messages must be >= 0")
	}
	if opts.ReadBytes < 0 {
		return fmt.Errorf("--read-bytes must be >= 0")
	}
	if opts.PromptMaxTokens <= 0 {
		return fmt.Errorf("--prompt-max-tokens must be > 0")
	}
	if opts.TaxonomyMaxTokens <= 0 {
		return fmt.Errorf("--taxonomy-max-tokens must be > 0")
	}
	if opts.ToolMaxTurns <= 0 {
		return fmt.Errorf("--tool-max-turns must be > 0")
	}
	if opts.MaxOutputTokens <= 0 {
		return fmt.Errorf("--max-output-tokens must be > 0")
	}

	promoteMode := strings.ToLower(strings.TrimSpace(opts.Promote))
	switch promoteMode {
	case "auto", "always", "never":
	default:
		return fmt.Errorf("--promote must be one of: auto, always, never")
	}
	if promoteMode == "auto" && opts.PromoteEvery <= 0 {
		return fmt.Errorf("--promote-every must be > 0 when --promote=auto")
	}

	cfg, err := env.LoadConfig()
	if err != nil {
		return err
	}

	env, err = env.withProtection(cfg)
	if err != nil {
		return err
	}
	opts.Env = env

	if err := env.ApplyAskProviderOverrides(cfg); err != nil {
		return err
	}
	if strings.TrimSpace(opts.Model) != "" {
		cfg.ApplyOverrides("", strings.TrimSpace(opts.Model))
	}

	provider, modelName, err := opts.newMemoryMineProvider(cfg)
	if err != nil {
		return err
	}
	engine := env.NewEngine(provider, cfg)

	memStore, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer memStore.Close()

	sessStore, err := openReadOnlySessionStore(env, cfg)
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

	sourceSessions, err := listMemorySourceSessions(ctx, sessStore, "")
	if err != nil {
		return fmt.Errorf("list memory source sessions: %w", err)
	}

	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		modelName = env.ActiveModel(cfg)
	}
	if modelName == "" {
		modelName = "(default model)"
	}
	if len(sourceSessions) > 0 {
		if opts.Limit > 0 {
			fmt.Printf("Scanning %d session(s) for up to %d extraction attempt(s) with %s / %s\n",
				len(sourceSessions), opts.Limit, provider.Name(), modelName)
		} else {
			fmt.Printf("Scanning %d session(s) for extraction with %s / %s\n",
				len(sourceSessions), provider.Name(), modelName)
		}
	}
	if env.DryRun {
		fmt.Println("Dry run mode: no database writes will be performed.")
	}

	var totalCreated, totalUpdated, totalSkipped int
	failedBatches := 0
	var batchErrors []error
	var phaseErrors []error
	var summaryStats memoryMineSummaryStats
	fragmentCache := newMemoryAgentFragmentCache(opts.TaxonomyMaxTokens)
	fragmentCache.protection = env.protection

	walkResult, err := opts.walkMineCandidates(ctx, sessStore, memStore, sourceSessions, currentID, func(position, total int, candidate memoryMineCandidate) (memoryMineCandidateVisitResult, error) {
		visitResult := memoryMineCandidateVisitResult{}

		// Discovery uses a snapshot only to prune completed work. Refresh state at
		// processing time to narrow the stale-state window; monotonic state updates
		// prevent a concurrent miner's newer cursor from being overwritten.
		state, err := memStore.GetState(ctx, candidate.Session.ID)
		if err != nil {
			return visitResult, fmt.Errorf("get mining state for session %s: %w", candidate.Session.ID, err)
		}

		startOffset := 0
		if state != nil {
			startOffset = state.LastMinedOffset
		}

		existing, taxonomyMap, err := fragmentCache.get(ctx, memStore, candidate.Agent)
		if err != nil {
			return visitResult, fmt.Errorf("list existing fragments for agent %s: %w", candidate.Agent, err)
		}

		loadResult, err := opts.loadMessagesForMining(ctx, sessStore, candidate, startOffset, taxonomyMap)
		if err != nil {
			return visitResult, fmt.Errorf("load messages for session %s: %w", candidate.Session.ID, err)
		}
		opts.reportMessageScope(candidate.Summary.Number, loadResult)
		if len(loadResult.Messages) == 0 || (opts.hasMessageBounds() && !hasTextEvidence(loadResult.Messages)) {
			if !env.DryRun && loadResult.NextOffset > startOffset {
				if err := memStore.UpsertState(ctx, &memorydb.MiningState{SessionID: candidate.Session.ID, Agent: candidate.Agent, LastMinedOffset: loadResult.NextOffset, MinedAt: time.Now()}); err != nil {
					return visitResult, err
				}
			}
			fmt.Printf("[%d/%d] #%d no new messages to mine\n", position, total, candidate.Summary.Number)
			return visitResult, nil
		}
		visitResult.Attempted = true

		promptParts := opts.extractionPrompt(candidate, startOffset, loadResult, taxonomyMap)
		batchStats := opts.newMemoryExtractionStats(candidate, startOffset, loadResult.NextOffset, len(existing), loadResult, promptParts)
		batchStart := time.Now()
		extractionResult, err := opts.runExtractionRequest(ctx, engine, memStore, candidate.Agent, promptParts.Prompt, &batchStats)
		if !env.DryRun {

			opts.recordExtractionSources(ctx, memStore, candidate, startOffset, loadResult, extractionResult.AffectedPaths)
		}
		batchStats.Duration = time.Since(batchStart)
		if err != nil {
			if opts.ShowStats {
				fmt.Printf("[%d/%d] #%d failed %s\n", position, total, candidate.Summary.Number, batchStats.oneLine())
				summaryStats.add(batchStats)
			}
			fragmentCache.invalidate(candidate.Agent)
			failedBatches++
			batchErrors = append(batchErrors, err)
			totalCreated += extractionResult.Created
			totalUpdated += extractionResult.Updated
			fmt.Fprintf(os.Stderr, "warning: session %s batch at offset %d incomplete; cursor retained; create=%d update=%d rejected=%s: %v\n", candidate.Session.ID, startOffset, extractionResult.Created, extractionResult.Updated, extractionResult.rejectionSummary(), err)
			return visitResult, nil
		}

		created, updated, skipped, affectedPaths := extractionResult.Created, extractionResult.Updated, extractionResult.Skipped, extractionResult.AffectedPaths

		if !env.DryRun {
			if err := memStore.UpsertState(ctx, &memorydb.MiningState{
				SessionID:       candidate.Session.ID,
				Agent:           candidate.Agent,
				LastMinedOffset: loadResult.NextOffset,
				MinedAt:         time.Now(),
			}); err != nil {
				return visitResult, fmt.Errorf("update mining state for session %s: %w", candidate.Session.ID, err)
			}
			if err := fragmentCache.applyChanges(ctx, memStore, candidate.Agent, affectedPaths); err != nil {
				fragmentCache.invalidate(candidate.Agent)
				fmt.Fprintf(os.Stderr, "warning: refresh fragment cache for agent %s: %v\n", candidate.Agent, err)
			}
		}

		totalCreated += created
		totalUpdated += updated
		totalSkipped += skipped

		fmt.Printf("[%d/%d] #%d mined messages [%d,%d): create=%d update=%d skip=%d rejected=%s\n",
			position, total, candidate.Summary.Number, startOffset, loadResult.NextOffset, created, updated, skipped, extractionResult.rejectionSummary())
		if opts.ShowStats {
			fmt.Printf("[%d/%d] #%d %s\n", position, total, candidate.Summary.Number, batchStats.oneLine())
			summaryStats.add(batchStats)
		}
		visitResult.Successful = true
		return visitResult, nil
	})
	if err != nil {
		return err
	}
	if walkResult.Eligible == 0 {
		fmt.Println("No sessions eligible for memory mining.")
	}

	if opts.hasMessageBounds() {
		fmt.Println("Message-bounded mode: skipping image mining phase; image watermark unchanged.")
	} else if env.DryRun {
		fmt.Println("Dry run mode: skipping image mining phase.")
	} else {
		imageMined, err := env.mineGeneratedImages(ctx, memStore)
		if err != nil {
			phaseErrors = append(phaseErrors, err)
			fmt.Fprintf(os.Stderr, "warning: image mining incomplete: %v\n", err)
		}
		if imageMined > 0 {
			fmt.Printf("mined %d image fragment(s)\n", imageMined)
		}
	}

	embeddedCount := 0
	if opts.Embed {
		if env.DryRun {
			fmt.Println("Dry run mode: skipping EMBED phase.")
		} else {
			embeddedCount, err = opts.runMemoryEmbedPhase(ctx, cfg, memStore)
			if err != nil {
				phaseErrors = append(phaseErrors, err)
				fmt.Fprintf(os.Stderr, "warning: embedding phase: %v\n", err)
			}
			fmt.Printf("embedded %d fragments\n", embeddedCount)
		}
	}

	if promoteMode != "never" {
		if env.DryRun {
			fmt.Println("Dry run mode: skipping PROMOTE phase.")
		} else {
			for _, promoteAgent := range minePromoteAgents(env.Agent, walkResult.SuccessfulCandidates) {
				shouldPromote := promoteMode == "always"
				if promoteMode == "auto" {
					shouldPromote, err = shouldRunAutoPromote(ctx, memStore, promoteAgent, opts.PromoteEvery)
					if err != nil {
						fmt.Fprintf(os.Stderr, "warning: failed checking auto-promote schedule for %s: %v\n", promoteAgent, err)
						continue
					}
				}
				if !shouldPromote {
					continue
				}

				if _, err := runMemoryPromoteFlow(ctx, env, cfg, engine, memStore, memoryPromoteOptions{
					Agent:          promoteAgent,
					RecentMaxBytes: DefaultRecentMaxBytes,
					Model:          strings.TrimSpace(opts.Model),
					DryRun:         false,
					QuietNothing:   true,
				}); err != nil {
					phaseErrors = append(phaseErrors, err)
					fmt.Fprintf(os.Stderr, "warning: promote failed for %s: %v\n", promoteAgent, err)
				}
			}
		}
	}

	fmt.Printf("Done. create=%d update=%d skip=%d\n", totalCreated, totalUpdated, totalSkipped)
	if opts.ShowStats {
		summaryStats.print()
	}

	if opts.Insights && !env.DryRun {
		fmt.Println("\n--- INSIGHT EXTRACTION ---")
		insightCandidates, err := opts.collectInsightCandidates(ctx, sessStore, memStore, sourceSessions, currentID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: collect insight candidates failed: %v\n", err)
		} else {
			if len(insightCandidates) > 0 {
				fmt.Printf("Insight extraction: %d unprocessed session(s) selected\n", len(insightCandidates))
			}
			insightCount, insightStats, err := opts.runInsightExtractionPass(ctx, cfg, engine, sessStore, memStore, insightCandidates)
			if err != nil {
				phaseErrors = append(phaseErrors, err)
				fmt.Fprintf(os.Stderr, "warning: insight extraction failed: %v\n", err)
			} else {
				fmt.Printf("insights: %d created/reinforced\n", insightCount)
				if opts.ShowStats {
					insightStats.print()
				}
			}
		}
	}

	if failedBatches > 0 {
		phaseErrors = append(phaseErrors, fmt.Errorf("%d failed memory batch(es); cursors retained for retry: %w", failedBatches, errors.Join(batchErrors...)))
	}
	return errors.Join(phaseErrors...)

}

func (opts MineOptions) newMemoryMineProvider(cfg *config.Config) (core.Provider, string, error) {
	if opts.UseFastModel && strings.TrimSpace(opts.Model) != "" {
		return nil, "", fmt.Errorf("--fast-model and --model are mutually exclusive")
	}
	if opts.UseFastModel {
		provider, err := llm.NewFastProvider(cfg, cfg.DefaultProvider)
		if err != nil {
			return nil, "", fmt.Errorf("fast provider: %w", err)
		}
		if provider == nil {
			return nil, "", fmt.Errorf("no fast model configured for %q", cfg.DefaultProvider)
		}
		return provider, resolveMemoryMineFastModelName(cfg), nil
	}
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return nil, "", err
	}
	return provider, strings.TrimSpace(opts.Model), nil
}

func resolveMemoryMineFastModelName(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	_, model, _ := llm.ResolveFastTarget(cfg, cfg.DefaultProvider)
	return model
}

func minePromoteAgents(globalAgent string, candidates []memoryMineCandidate) []string {
	if strings.TrimSpace(globalAgent) != "" {
		// An explicit agent keeps promotion independently schedulable even when
		// this run found no new fragments to extract.
		return []string{strings.TrimSpace(globalAgent)}
	}
	if len(candidates) == 0 {
		return nil
	}

	seen := map[string]struct{}{}
	agents := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		agent := strings.TrimSpace(candidate.Agent)
		if agent == "" {
			agent = strings.TrimSpace(globalAgent)
			if agent == "" {
				agent = "default"
			}
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

type memoryMineCandidateVisitResult struct {
	Attempted  bool
	Successful bool
}

type memoryMineCandidateWalkResult struct {
	Eligible             int
	Attempts             int
	SuccessfulCandidates []memoryMineCandidate
}

func (opts MineOptions) walkMineCandidates(
	ctx context.Context,
	store session.Store,
	stateStore memoryMiningStateStore,
	sourceSessions []session.SessionSummary,
	currentID string,
	visit func(position, total int, candidate memoryMineCandidate) (memoryMineCandidateVisitResult, error),
) (memoryMineCandidateWalkResult, error) {
	var result memoryMineCandidateWalkResult
	states, err := stateStore.ListStates(ctx)
	if err != nil {
		return result, fmt.Errorf("list mining states: %w", err)
	}
	statesBySession := make(map[string]memorydb.MiningState, len(states))
	for _, state := range states {
		statesBySession[state.SessionID] = state
	}

	cutoff := opts.memoryMineCutoff()
	// Maximum-sequence lookup is an optional optimization. Limit its input to
	// sessions that have mining state and pass filters available from summaries;
	// sessions without state cannot be proven fully mined by this fast path.
	maxSequences := map[string]int(nil)
	if sequenceStore, ok := store.(session.MessageSequenceStore); ok {
		sessionIDs := make([]string, 0, len(statesBySession))
		for _, summary := range sourceSessions {
			if _, hasState := statesBySession[summary.ID]; !hasState {
				continue
			}
			if currentID != "" && summary.ID == currentID {
				continue
			}
			if !cutoff.IsZero() && summary.UpdatedAt.Before(cutoff) {
				continue
			}
			sessionIDs = append(sessionIDs, summary.ID)
		}
		if len(sessionIDs) > 0 {
			maxSequences, err = sequenceStore.MaxMessageSequences(ctx, sessionIDs)
			if err != nil {
				return result, fmt.Errorf("get maximum message sequences: %w", err)
			}
		}
	}

	for i, summary := range sourceSessions {
		if opts.Limit > 0 && result.Attempts >= opts.Limit {
			break
		}

		state, hasState := statesBySession[summary.ID]
		maxSequence, hasMaxSequence := maxSequences[summary.ID]
		if hasState && hasMaxSequence && state.LastMinedOffset > maxSequence {
			continue
		}

		candidate, ok, err := opts.buildMemoryMineCandidate(ctx, store, summary, currentID, cutoff)
		if err != nil {
			return result, err
		}
		if !ok {
			continue
		}
		result.Eligible++

		visitResult, err := visit(i+1, len(sourceSessions), candidate)
		if err != nil {
			return result, err
		}
		if !visitResult.Attempted {
			continue
		}
		result.Attempts++
		if visitResult.Successful {
			result.SuccessfulCandidates = append(result.SuccessfulCandidates, candidate)
		}
	}

	return result, nil
}

func (opts MineOptions) collectInsightCandidates(ctx context.Context, store session.Store, memStore *memorydb.Store, sourceSessions []session.SessionSummary, currentID string) ([]memoryMineCandidate, error) {
	out := make([]memoryMineCandidate, 0, len(sourceSessions))
	cutoff := opts.memoryMineCutoff()

	for _, summary := range sourceSessions {
		if summary.MessageCount > 0 && summary.MessageCount < 4 {
			continue
		}

		candidate, ok, err := opts.buildMemoryMineCandidate(ctx, store, summary, currentID, cutoff)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		minedAt, err := memStore.InsightMinedAt(ctx, candidate.Session.ID)
		if err != nil {
			return nil, fmt.Errorf("get insight mining state for session %s: %w", candidate.Session.ID, err)
		}
		if !minedAt.IsZero() {
			continue
		}

		out = append(out, candidate)

		// Insight candidates have already passed their persisted mined-state check,
		// so each selected session represents an extraction attempt.
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}

	return out, nil
}

func (opts MineOptions) memoryMineCutoff() time.Time {
	if opts.Since <= 0 {
		return time.Time{}
	}
	return time.Now().Add(-opts.Since)
}

func (opts MineOptions) buildMemoryMineCandidate(ctx context.Context, store session.Store, summary session.SessionSummary, currentID string, cutoff time.Time) (memoryMineCandidate, bool, error) {
	if (!cutoff.IsZero() && summary.UpdatedAt.Before(cutoff)) || activeSessionMayBeWriting(summary, time.Now()) {
		return memoryMineCandidate{}, false, nil
	}

	sess, err := store.Get(ctx, summary.ID)
	if err != nil {
		return memoryMineCandidate{}, false, fmt.Errorf("get session %s: %w", summary.ID, err)
	}
	if sess == nil {
		return memoryMineCandidate{}, false, nil
	}

	if currentID != "" && sess.ID == currentID {
		return memoryMineCandidate{}, false, nil
	}
	if !opts.IncludeSubagents && (sess.IsSubagent || sess.ParentID != "") {
		return memoryMineCandidate{}, false, nil
	}
	if hasMemoryMiningTag(opts.Env, sess.Tags) {
		return memoryMineCandidate{}, false, nil
	}
	if agentFilter := opts.AgentFilter(); agentFilter != "" && strings.TrimSpace(sess.Agent) != agentFilter {
		return memoryMineCandidate{}, false, nil
	}

	return memoryMineCandidate{
		Summary: summary,
		Session: sess,
		Agent:   opts.ResolveAgent(sess.Agent),
	}, true, nil
}

func collectToolCallNames(msg session.Message) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, part := range msg.Parts {
		if part.Type != core.PartToolCall || part.ToolCall == nil {
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

func (opts MineOptions) runExtractionRequest(ctx context.Context, engine *engine.Engine, store *memorydb.Store, agent, prompt string, stats *memoryExtractionStats) (memoryExtractionResult, error) {
	collector := newMemoryExtractionCollector()
	toolSpecs, cleanup := registerMemoryExtractionTools(engine, store, agent, collector, opts.ReadBytes, opts.DryRun, opts.protection)
	defer cleanup()
	finalText, err := opts.runExtractionRequestWithSystem(ctx, engine, memoryExtractionSystemPrompt, prompt, stats, toolSpecs...)
	if pending := collector.pendingError(); pending != nil {
		return collector.result(finalText), errors.Join(err, pending)
	}
	return collector.result(finalText), err
}

// runExtractionRequestWithSystem accepts prompts whose original fields have
// already been sanitized. Never redact serialized JSON: escaped newlines change
// detector boundaries and can consume unrelated evidence.
func (opts MineOptions) runExtractionRequestWithSystem(ctx context.Context, engine *engine.Engine, systemPrompt, prompt string, stats extractionRequestStats, tools ...core.ToolSpec) (string, error) {
	rawDebug := protectionOrDefault(opts.protection).prepareDebug(engine, opts.debugRaw())
	req := core.Request{
		Model:           strings.TrimSpace(opts.Model),
		Ephemeral:       true,
		Messages:        []core.Message{core.SystemText(systemPrompt), core.UserText(prompt)},
		MaxTurns:        1,
		MaxOutputTokens: opts.MaxOutputTokens,
		Debug:           false,
		DebugRaw:        rawDebug,
	}
	if len(tools) > 0 {
		req.Tools = tools
		req.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceAuto}
		req.ParallelToolCalls = true
		req.MaxTurns = opts.ToolMaxTurns
	}

	stream, err := engine.Stream(ctx, req)
	if err != nil {
		return "", protectionOrDefault(opts.protection).providerError("memory extraction provider operation failed", err)
	}
	defer stream.Close()

	var b strings.Builder
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", protectionOrDefault(opts.protection).providerError("memory extraction provider operation failed", err)
		}
		switch ev.Type {
		case core.EventTextDelta:
			b.WriteString(ev.Text)
		case core.EventToolCall:
			if stats != nil && ev.Tool != nil {
				stats.noteToolCall(strings.TrimSpace(ev.Tool.Name))
			}
		case core.EventUsage:
			if stats != nil {
				stats.noteUsage(ev.Use)
			}
		case core.EventError:
			if ev.Err != nil {
				return "", protectionOrDefault(opts.protection).providerError("memory extraction provider operation failed", ev.Err)
			}
		}
	}

	return strings.TrimSpace(b.String()), nil
}

func stripMarkdownFences(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop opening fence line (e.g. "```json" or "```")
	idx := strings.Index(s, "\n")
	if idx < 0 {
		return s
	}
	s = s[idx+1:]
	// Drop closing fence
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
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

func (env Env) mineGeneratedImages(ctx context.Context, store *memorydb.Store) (int, error) {
	agents, err := env.resolveImageMineAgents(ctx, store)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: resolve image mining agents: %v\n", err)
		return 0, err
	}
	if len(agents) == 0 {
		return 0, nil
	}

	total := 0
	var failures []error
	for _, agent := range agents {
		count, err := env.mineGeneratedImagesForAgent(ctx, store, agent)
		if err != nil {
			failures = append(failures, err)
		}
		total += count
	}
	return total, errors.Join(failures...)
}

func (env Env) resolveImageMineAgents(ctx context.Context, store *memorydb.Store) ([]string, error) {
	if env.AgentFilter() != "" {
		return []string{env.AgentFilter()}, nil
	}
	return store.ListImageAgents(ctx)
}

func (env Env) mineGeneratedImagesForAgent(ctx context.Context, store *memorydb.Store, agent string) (int, error) {
	metaKey := env.memoryImageMineMetaKey(agent)
	lastValue, err := store.GetMeta(ctx, metaKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: read image mine meta for %s: %v\n", agent, err)
		return 0, err
	}

	lastTime := time.Time{}
	if strings.TrimSpace(lastValue) != "" {
		parsed, err := time.Parse(time.RFC3339, lastValue)
		if err != nil {
			fmt.Fprintln(os.Stderr, "warning: invalid image mining timestamp; stored value withheld")
		} else {
			lastTime = parsed
		}
	}

	images, err := store.ListImagesSince(ctx, agent, lastTime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: list images for %s: %v\n", agent, err)
		return 0, err
	}
	if len(images) == 0 {
		return 0, nil
	}

	effectiveAgent := strings.TrimSpace(agent)
	if effectiveAgent == "" {
		effectiveAgent = env.ResolveAgent("")
	}

	created := 0
	excluded := 0
	var failures []error
	safeThrough := lastTime
	blocked := false
	for i := 0; i < len(images); {
		end := i + 1
		for end < len(images) && images[end].CreatedAt.Equal(images[i].CreatedAt) {
			end++
		}
		groupOK := true
		for _, img := range images[i:end] {
			img.Prompt = protectionOrDefault(env.protection).sanitize(img.Prompt)
			img.OutputPath = protectionOrDefault(env.protection).sanitize(img.OutputPath)
			img.Provider = protectionOrDefault(env.protection).sanitize(img.Provider)
			img.SessionID = protectionOrDefault(env.protection).sanitize(img.SessionID)
			fragPath := protectionOrDefault(env.protection).sanitize(buildImageFragmentPath(img))
			if protectionOrDefault(env.protection).excluded(fragPath) {
				excluded++
				continue
			} // explicit owner exclusion is an examined outcome
			if err := env.upsertImageFragment(ctx, store, effectiveAgent, fragPath, protectionOrDefault(env.protection).sanitize(env.buildImageFragmentContent(img))); err != nil {
				failures = append(failures, protectionOrDefault(env.protection).providerError("image row failed; watermark retained", err))
				groupOK = false
				continue
			}
			created++
		}
		if !groupOK {
			blocked = true
		}
		if !blocked {
			safeThrough = images[i].CreatedAt
		}
		i = end
	}
	if safeThrough.After(lastTime) {
		if err := store.SetMeta(ctx, metaKey, safeThrough.Format(time.RFC3339Nano)); err != nil {
			failures = append(failures, err)
		}
	}
	if excluded > 0 || len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "images: processed=%d excluded=%d failed=%d; checkpoint_blocked=%t\n", created, excluded, len(failures), blocked)
	}
	return created, errors.Join(failures...)

}

func (env Env) memoryImageMineMetaKey(agent string) string {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		agent = env.ResolveAgent("")
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

func (env Env) buildImageFragmentContent(img memorydb.ImageRecord) string {
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
		size = env.FormatBytes(int64(img.FileSize))
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

func (env Env) upsertImageFragment(ctx context.Context, store *memorydb.Store, agent, fragPath, content string) error {
	if err := protectionOrDefault(env.protection).fragment(fragPath, content); err != nil {
		return err
	}
	if env.DryRun {
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

// ── Insight extraction pass ───────────────────────────────────────────────────

const insightExtractionSystemPrompt = `You are a behavioral insight extractor.

Output must be valid JSON only (no markdown fences, no prose).
Return exactly one object with key "insights".

Goal: extract generalized behavioral rules from the conversation transcript.
Focus on moments where:
- The user corrects or redirects the assistant
- The user expresses a preference explicitly or implicitly
- The assistant failed to anticipate something the user then had to ask for
- A pattern of interaction is repeated across multiple turns

Do NOT extract:
- One-off requests with no generalisation
- Facts about the user's technical setup (those belong in fragment memory)
- Anything that is already obvious assistant best practice
- More than 10 insights total

Each insight must have:
- category: one of anti-pattern | communication-style | domain-approach | workflow | anticipation
- trigger: when it applies (1 sentence)
- rule: the full actionable behavioral change (1-3 sentences, can include justification/evidence)
- compact: a terse version of the rule in ≤15 words, imperative, no hedging (used for token-efficient injection)
- confidence: 0.0-1.0 based on how clearly the evidence supports the pattern`

type insightExtractionResponse struct {
	Insights []insightExtractionItem `json:"insights"`
}

type insightExtractionItem struct {
	Category   string  `json:"category"`
	Trigger    string  `json:"trigger"`
	Rule       string  `json:"rule"`
	Compact    string  `json:"compact"`
	Confidence float64 `json:"confidence"`
}

// buildInsightTranscript builds a lean, user-weighted transcript for insight extraction.
// Insight extraction learns from what the user said: corrections, preferences,
// and redirects. Assistant text and tool events are context only, so they are
// aggressively abbreviated and globally capped so non-user tokens never exceed
// user tokens. Tool results are summarized by action/outcome only; full output is
// noise for behavioral mining and can dwarf the actual conversation signal.
func buildInsightTranscript(messages []session.Message) []transcriptMessage {
	const maxUserChars = 1200
	const maxAssistantChars = 120
	const maxToolEventChars = 80

	type pendingMessage struct {
		role string
		text string
	}

	pending := make([]pendingMessage, 0, len(messages))
	userTokenBudget := 0
	for _, msg := range messages {
		if excludedMemoryEvidenceRole(msg.Role) {
			continue
		}
		if msg.Role == core.RoleTool {
			for _, summary := range summarizeInsightToolResults(msg) {
				pending = append(pending, pendingMessage{role: string(core.RoleTool), text: summary})
			}
			continue
		}

		text := strings.TrimSpace(msg.TextContent)
		if msg.Role == core.RoleUser {
			if text == "" {
				continue
			}
			text = truncateForInsightTranscript(text, maxUserChars)
			userTokenBudget += engine.EstimateTokens(text)
			pending = append(pending, pendingMessage{role: string(msg.Role), text: text})
			continue
		}

		if text != "" {
			pending = append(pending, pendingMessage{role: string(msg.Role), text: text})
		}
		for _, summary := range summarizeInsightToolCalls(msg) {
			pending = append(pending, pendingMessage{role: string(core.RoleTool), text: summary})
		}
	}

	toolBudget := 0
	for _, msg := range pending {
		if msg.role != string(core.RoleTool) {
			continue
		}
		toolText := truncateForInsightTranscript(msg.text, maxToolEventChars)
		toolBudget += engine.EstimateTokens(toolText)
		if toolBudget >= userTokenBudget {
			toolBudget = userTokenBudget
			break
		}
	}
	assistantBudget := userTokenBudget - toolBudget
	out := make([]transcriptMessage, 0, len(pending))
	for _, msg := range pending {
		text := msg.text
		switch msg.role {
		case string(core.RoleUser):
			// User text defines the signal; include it outside the non-user budget.
		case string(core.RoleTool):
			if toolBudget <= 0 {
				continue
			}
			text = truncateForInsightTranscript(text, maxToolEventChars)
			text = trimTextToTokenBudget(text, toolBudget)
			if strings.TrimSpace(text) == "" {
				continue
			}
			toolBudget -= engine.EstimateTokens(text)
		default:
			if assistantBudget <= 0 {
				continue
			}
			text = truncateForInsightTranscript(text, maxAssistantChars)
			text = trimTextToTokenBudget(text, assistantBudget)
			if strings.TrimSpace(text) == "" {
				continue
			}
			assistantBudget -= engine.EstimateTokens(text)
		}
		out = append(out, transcriptMessage{Role: msg.role, Text: text})
	}
	return out
}

func summarizeInsightToolCalls(msg session.Message) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, part := range msg.Parts {
		if part.Type != core.PartToolCall || part.ToolCall == nil {
			continue
		}
		name := strings.TrimSpace(part.ToolCall.Name)
		if name == "" || seen["call:"+name] {
			continue
		}
		seen["call:"+name] = true
		info := strings.TrimSpace(part.ToolCall.ToolInfo)
		if info != "" {
			out = append(out, fmt.Sprintf("tool called: %s %s", name, info))
		} else {
			out = append(out, fmt.Sprintf("tool called: %s", name))
		}
	}
	return out
}

func summarizeInsightToolResults(msg session.Message) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, part := range msg.Parts {
		if part.Type != core.PartToolResult || part.ToolResult == nil {
			continue
		}
		name := strings.TrimSpace(part.ToolResult.Name)
		if name == "" || seen["result:"+name] {
			continue
		}
		seen["result:"+name] = true
		status := "ok"
		if part.ToolResult.IsError {
			status = "error"
		}
		out = append(out, fmt.Sprintf("tool result: %s %s", name, status))
	}
	return out
}

func truncateForInsightTranscript(text string, maxChars int) string {
	if maxChars <= 0 || len(text) <= maxChars {
		return text
	}
	return strings.TrimSpace(text[:maxChars]) + "..."
}

func trimTextToTokenBudget(text string, budget int) string {
	if budget <= 0 || strings.TrimSpace(text) == "" {
		return ""
	}
	if engine.EstimateTokens(text) <= budget {
		return text
	}

	lo, hi := 0, len(text)
	best := ""
	for lo <= hi {
		mid := (lo + hi) / 2
		candidate := strings.TrimSpace(text[:mid])
		if candidate != "" {
			candidate += "..."
		}
		if candidate != "" && engine.EstimateTokens(candidate) <= budget {
			best = candidate
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return best
}

func buildInsightExtractionPrompt(agent string, messages []session.Message, existing []*memorydb.Insight) string {
	return buildInsightExtractionPromptFromTranscript(agent, buildInsightTranscript(messages), existing)
}

func buildInsightExtractionPromptFromTranscript(agent string, transcript []transcriptMessage, existing []*memorydb.Insight) string {
	transcriptJSON, _ := json.MarshalIndent(transcript, "", "  ")

	// Pass a summary of existing insights so the LLM can deduplicate.
	type existingSummary struct {
		ID      int64  `json:"id"`
		Preview string `json:"preview"`
	}
	summaries := make([]existingSummary, 0, len(existing))
	for _, ins := range existing {
		preview := ins.Content
		if len(preview) > 120 {
			preview = preview[:120] + "..."
		}
		summaries = append(summaries, existingSummary{ID: ins.ID, Preview: preview})
	}
	summariesJSON, _ := json.MarshalIndent(summaries, "", "  ")

	return fmt.Sprintf(`Agent: %s

Existing insights (do not duplicate these patterns):
%s

Transcript (user messages in full; assistant text abbreviated; tool output summarized):
%s

Return strict JSON only:
{
  "insights": [
    {"category": "...", "trigger": "...", "rule": "...", "confidence": 0.7},
    ...
  ]
}
If no new insights are found, return {"insights": []}.`,
		agent,
		string(summariesJSON),
		string(transcriptJSON),
	)
}

// runInsightExtractionPass runs an insight extraction pass over mined candidates.
// For each candidate session, it loads messages, builds a lean transcript, calls
// the LLM, and writes new insights (or reinforces existing ones via BM25 match).
func (opts MineOptions) runInsightExtractionPass(
	ctx context.Context,
	cfg *config.Config,
	engine *engine.Engine,
	sessStore session.Store,
	memStore *memorydb.Store,
	candidates []memoryMineCandidate,
) (int, insightExtractionSummaryStats, error) {
	env, err := opts.Env.withProtection(cfg)
	if err != nil {
		return 0, insightExtractionSummaryStats{}, err
	}
	opts.Env = env

	total := 0
	var summary insightExtractionSummaryStats
	var failures []error
	for _, candidate := range candidates {
		// Skip sessions already processed for insights — avoids redundant LLM
		// calls and duplicate extraction. MarkInsightMined is called on success.
		if minedAt, err := memStore.InsightMinedAt(ctx, candidate.Session.ID); err == nil && !minedAt.IsZero() {
			continue
		}

		loadResult, err := opts.loadMessagesForMining(ctx, sessStore, candidate, 0, "Memory fragment map:\n- total_fragments: 0")
		if err != nil {
			fmt.Fprintf(os.Stderr, "  [insight] skip %s: load messages: %v\n", candidate.Session.ID, err)
			continue
		}
		messages := loadResult.Messages
		if len(messages) < 4 {
			// Too short to contain meaningful patterns. Mark it processed so tiny
			// sessions do not permanently occupy the insight candidate limit.
			if err := memStore.MarkInsightMined(ctx, candidate.Session.ID, candidate.Agent); err != nil {
				fmt.Fprintf(os.Stderr, "  [insight] warning: mark short session mined failed for %s: %v\n", candidate.Session.ID, err)
			}
			continue
		}

		// Load existing insights for deduplication context.
		existing, err := memStore.ListInsights(ctx, candidate.Agent, 50)
		if err != nil {
			existing = nil
		}

		for _, ins := range existing {
			ins.Content = protectionOrDefault(opts.protection).sanitize(ins.Content)
		}
		transcript := buildInsightTranscript(messages)
		prompt := buildInsightExtractionPromptFromTranscript(protectionOrDefault(opts.protection).sanitize(candidate.Agent), transcript, existing)
		stats := newInsightExtractionStats(candidate, messages, transcript, len(existing), prompt)
		start := time.Now()
		raw, err := opts.runExtractionRequestWithSystem(ctx, engine, insightExtractionSystemPrompt, prompt, &stats)
		stats.Duration = time.Since(start)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  [insight] skip %s: llm call: %v\n", candidate.Session.ID, err)
			if opts.ShowStats {
				fmt.Printf("  [insight] #%d %s\n", candidate.Summary.Number, stats.oneLine())
				summary.add(stats)
			}
			continue
		}
		raw = stripMarkdownFences(raw)

		var resp insightExtractionResponse
		decoder := json.NewDecoder(strings.NewReader(raw))
		if err := decoder.Decode(&resp); err != nil {
			fmt.Fprintf(os.Stderr, "  [insight] skip %s: decode: %v\n", candidate.Session.ID, err)
			if opts.ShowStats {
				fmt.Printf("  [insight] #%d %s\n", candidate.Summary.Number, stats.oneLine())
				summary.add(stats)
			}
			continue
		}

		valid := true
		for i, item := range resp.Insights {
			normalized, err := protectionOrDefault(opts.protection).insight(item)
			if err != nil {
				failures = append(failures, err)
				fmt.Fprintf(os.Stderr, "warning: insight session %s rejected; marker retained: %v\n", candidate.Session.ID, err)
				valid = false
				break
			}
			resp.Insights[i] = normalized
		}
		if !valid {
			continue
		}

		sessionInsights := 0
		for _, item := range resp.Insights {
			rule := item.Rule
			if rule == "" {
				continue
			}

			// Merge-or-create: search for a similar existing insight via BM25.
			// If we get a hit, reinforce rather than duplicate.
			similar, _ := memStore.SearchInsights(ctx, candidate.Agent, rule, 1)
			if len(similar) > 0 {
				// Rough similarity check: if top result shares significant keywords,
				// treat it as the same insight and reinforce.
				if isSimilarInsight(rule, similar[0].Content) {
					_ = memStore.ReinforceInsight(ctx, similar[0].ID)
					fmt.Printf("  [insight] reinforced id=%d (conf=%.2f)\n", similar[0].ID, similar[0].Confidence+0.1)
					total++
					sessionInsights++
					continue
				}
			}

			ins := &memorydb.Insight{
				Agent:          candidate.Agent,
				Content:        rule,
				CompactContent: item.Compact,
				Category:       item.Category,
				TriggerDesc:    item.Trigger,
				Confidence:     item.Confidence,
			}
			if ins.Confidence <= 0 {
				ins.Confidence = 0.5
			}
			if err := memStore.CreateInsight(ctx, ins); err != nil {
				fmt.Fprintf(os.Stderr, "  [insight] create failed: %v\n", err)
				continue
			}
			fmt.Printf("  [insight] created id=%d cat=%s conf=%.2f\n", ins.ID, ins.Category, ins.Confidence)
			total++
			sessionInsights++
		}

		stats.CreatedOrReinforced = sessionInsights
		if opts.ShowStats {
			fmt.Printf("  [insight] #%d %s\n", candidate.Summary.Number, stats.oneLine())
			summary.add(stats)
		}

		// Mark this session as insight-mined so future runs skip it.
		if err := memStore.MarkInsightMined(ctx, candidate.Session.ID, candidate.Agent); err != nil {
			fmt.Fprintf(os.Stderr, "  [insight] warning: mark mined failed for %s: %v\n", candidate.Session.ID, err)
		}
	}
	return total, summary, errors.Join(failures...)
}

// isSimilarInsight uses Jaccard similarity on keywords (words >3 chars) to
// detect duplicate insights. Requires at least 3 qualifying words in the
// candidate; returns false for very short insights to avoid false positives.
// Threshold: 40% Jaccard (intersection/union), which catches paraphrases
// without conflating opposites ("prefer verbose" vs "prefer terse").
func isSimilarInsight(candidate, existing string) bool {
	words := func(s string) map[string]struct{} {
		m := map[string]struct{}{}
		for _, w := range strings.Fields(strings.ToLower(s)) {
			if len(w) > 3 {
				m[w] = struct{}{}
			}
		}
		return m
	}
	cw := words(candidate)
	ew := words(existing)
	if len(cw) < 3 {
		return false // Too short to deduplicate reliably.
	}
	intersection := 0
	for w := range cw {
		if _, ok := ew[w]; ok {
			intersection++
		}
	}
	union := len(cw) + len(ew) - intersection
	if union == 0 {
		return false
	}
	return float64(intersection)/float64(union) > 0.40
}

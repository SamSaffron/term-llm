package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/session"
)

type fakeMemoryAgentFragmentStore struct {
	listCalls int
	getCalls  int
	listByKey map[string][]memorydb.Fragment
	getByKey  map[string]map[string]memorydb.Fragment
}

type candidateDiscoveryStore struct {
	session.NoopStore
	sessions              map[string]*session.Session
	maxSequences          map[string]int
	maxSequenceCalls      int
	maxSequenceSessionIDs []string
	getCalls              int
}

func (s *candidateDiscoveryStore) MaxMessageSequences(_ context.Context, sessionIDs []string) (map[string]int, error) {
	s.maxSequenceCalls++
	s.maxSequenceSessionIDs = append([]string(nil), sessionIDs...)
	return s.maxSequences, nil
}

func (s *candidateDiscoveryStore) Get(_ context.Context, id string) (*session.Session, error) {
	s.getCalls++
	return s.sessions[id], nil
}

type candidateDiscoveryFallbackStore struct {
	session.NoopStore
	sessions map[string]*session.Session
	getCalls int
}

func (s *candidateDiscoveryFallbackStore) Get(_ context.Context, id string) (*session.Session, error) {
	s.getCalls++
	return s.sessions[id], nil
}

type candidateDiscoveryStateStore struct {
	states    []memorydb.MiningState
	listCalls int
}

func (s *candidateDiscoveryStateStore) ListStates(_ context.Context) ([]memorydb.MiningState, error) {
	s.listCalls++
	return s.states, nil
}

type trackingMemoryMineStore struct {
	session.NoopStore
	messages             map[string][]session.Message
	getMessagesCalls     int
	getMessagesFromCalls []struct {
		fromSeq int
		limit   int
	}
}

func (s *trackingMemoryMineStore) GetMessages(_ context.Context, sessionID string, limit, offset int) ([]session.Message, error) {
	s.getMessagesCalls++
	msgs := s.messages[sessionID]
	if offset >= len(msgs) {
		return nil, nil
	}
	out := append([]session.Message(nil), msgs[offset:]...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *trackingMemoryMineStore) GetMessagesFrom(_ context.Context, sessionID string, fromSeq, limit int) ([]session.Message, error) {
	s.getMessagesFromCalls = append(s.getMessagesFromCalls, struct {
		fromSeq int
		limit   int
	}{fromSeq: fromSeq, limit: limit})
	msgs := s.messages[sessionID]
	filtered := make([]session.Message, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Sequence >= fromSeq {
			filtered = append(filtered, msg)
		}
	}
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered, nil
}

func (s *fakeMemoryAgentFragmentStore) ListFragments(ctx context.Context, opts memorydb.ListOptions) ([]memorydb.Fragment, error) {
	s.listCalls++
	fragments := s.listByKey[strings.TrimSpace(opts.Agent)]
	out := make([]memorydb.Fragment, len(fragments))
	copy(out, fragments)
	return out, nil
}

func (s *fakeMemoryAgentFragmentStore) GetFragment(ctx context.Context, agent, path string) (*memorydb.Fragment, error) {
	s.getCalls++
	fragments := s.getByKey[strings.TrimSpace(agent)]
	frag, ok := fragments[strings.TrimSpace(path)]
	if !ok {
		return nil, nil
	}
	copyFrag := frag
	return &copyFrag, nil
}

func TestBuildInsightTranscriptWeightsUserTextOverAssistantAndSummarizesTools(t *testing.T) {
	messages := []session.Message{
		{Role: llm.RoleSystem, TextContent: strings.Repeat("system ", 100)},
		{Role: llm.RoleUser, TextContent: "fix it"},
		{
			Role:        llm.RoleAssistant,
			TextContent: strings.Repeat("assistant explanation with lots of irrelevant details ", 200),
			Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{
				Name:     "read_file",
				ToolInfo: "(memory_mine.go)",
			}}},
		},
		{
			Role:        llm.RoleTool,
			TextContent: strings.Repeat("TOOL_SENTINEL should never appear ", 200),
			Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{
				Name:    "read_file",
				Content: strings.Repeat("TOOL_SENTINEL should never appear ", 200),
			}}},
		},
		{Role: llm.RoleAssistant, TextContent: strings.Repeat("second assistant dump ", 200)},
		{Role: llm.RoleUser, TextContent: "no, mine the user discussion, not the bulky execution transcript"},
		{Role: llm.RoleAssistant, TextContent: strings.Repeat("third assistant dump ", 200)},
	}

	got := buildInsightTranscript(messages)
	var userTokens, nonUserTokens int
	var sawToolCall, sawToolResult bool
	for _, msg := range got {
		if msg.Role == string(llm.RoleSystem) {
			t.Fatalf("transcript included system message: %+v", msg)
		}
		if strings.Contains(msg.Text, "TOOL_SENTINEL") {
			t.Fatalf("transcript leaked tool output: %q", msg.Text)
		}
		if strings.Contains(msg.Text, "tool called: read_file") {
			sawToolCall = true
		}
		if strings.Contains(msg.Text, "tool result: read_file ok") {
			sawToolResult = true
		}
		switch msg.Role {
		case string(llm.RoleUser):
			userTokens += llm.EstimateTokens(msg.Text)
		default:
			nonUserTokens += llm.EstimateTokens(msg.Text)
		}
	}

	if userTokens == 0 {
		t.Fatalf("expected user tokens in transcript: %+v", got)
	}
	if !sawToolCall || !sawToolResult {
		t.Fatalf("expected abbreviated tool call/result summaries, got %+v", got)
	}
	if nonUserTokens > userTokens {
		t.Fatalf("non-user tokens = %d, user tokens = %d; context must not dominate\n%+v", nonUserTokens, userTokens, got)
	}

	stats := summarizeInsightTranscriptStats(messages, got)
	if stats.UserTokens != userTokens {
		t.Fatalf("stats.UserTokens = %d, want %d", stats.UserTokens, userTokens)
	}
	if stats.NonUserTokens != nonUserTokens {
		t.Fatalf("stats.NonUserTokens = %d, want %d", stats.NonUserTokens, nonUserTokens)
	}
	if stats.ToolTokens == 0 {
		t.Fatalf("expected stats to count retained tool summaries: %+v", stats)
	}
	if stats.RawAssistantTokens <= stats.AssistantTokens {
		t.Fatalf("expected raw assistant tokens to show pruning: %+v", stats)
	}
	if stats.RawToolTokens < stats.ToolTokens || stats.RawToolTokens == 0 {
		t.Fatalf("expected raw tool summary tokens to cover retained tool summaries: %+v", stats)
	}
	if stats.NonUserTokens > stats.UserTokens {
		t.Fatalf("stats reported budget violation: %+v", stats)
	}
}

func TestValidateFragmentPath(t *testing.T) {
	for _, tc := range []struct {
		path    string
		want    string
		wantErr bool
	}{
		{"fragments/foo.md", "fragments/foo.md", false},
		{"fragments\\foo.md", "fragments/foo.md", false},
		{"../evil.md", "", true},
		{"/abs/path.md", "", true},
	} {
		got, err := validateFragmentPath(tc.path)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("validateFragmentPath(%q) expected error", tc.path)
			}
			continue
		}
		if err != nil {
			t.Fatalf("validateFragmentPath(%q) error = %v", tc.path, err)
		}
		if got != tc.want {
			t.Fatalf("validateFragmentPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestWalkMineCandidatesSkipsFullyMinedSessionsBeforePointReads(t *testing.T) {
	opts := MineOptions{Env: testEnv("", ""), Limit: 1, Since: 0}
	const sessionCount = 5000

	complete := make([]session.SessionSummary, sessionCount)
	states := make([]memorydb.MiningState, sessionCount)
	maxSequences := make(map[string]int, sessionCount)
	for i := range sessionCount {
		sessionID := fmt.Sprintf("session-%d", i)
		complete[i] = session.SessionSummary{ID: sessionID}
		states[i] = memorydb.MiningState{SessionID: sessionID, LastMinedOffset: 10}
		maxSequences[sessionID] = 9
	}

	sessStore := &candidateDiscoveryStore{maxSequences: maxSequences}
	stateStore := &candidateDiscoveryStateStore{states: states}
	visited := 0
	result, err := opts.walkMineCandidates(context.Background(), sessStore, stateStore, complete, "", func(_ int, _ int, _ memoryMineCandidate) (memoryMineCandidateVisitResult, error) {
		visited++
		return memoryMineCandidateVisitResult{Attempted: true, Successful: true}, nil
	})
	if err != nil {
		t.Fatalf("walkMineCandidates: %v", err)
	}
	if result.Eligible != 0 || visited != 0 {
		t.Fatalf("walk result = %+v, visited = %d; want no eligible candidates", result, visited)
	}
	if stateStore.listCalls != 1 || sessStore.maxSequenceCalls != 1 {
		t.Fatalf("batch calls = states %d, max sequences %d; want 1 each", stateStore.listCalls, sessStore.maxSequenceCalls)
	}
	if len(sessStore.maxSequenceSessionIDs) != sessionCount {
		t.Fatalf("maximum sequence IDs = %d, want %d", len(sessStore.maxSequenceSessionIDs), sessionCount)
	}
	if sessStore.getCalls != 0 {
		t.Fatalf("Get calls = %d, want 0", sessStore.getCalls)
	}
}

func TestWalkMineCandidatesDoesNotSpendLimitBeforeAttempt(t *testing.T) {
	opts := MineOptions{Env: testEnv("", "jarvis"), Limit: 1, Since: 0, IncludeSubagents: false}

	sessions := make(map[string]*session.Session, 51)
	complete := make([]session.SessionSummary, 0, 51)
	for number := int64(51); number >= 1; number-- {
		id := fmt.Sprintf("session-%d", number)
		sessions[id] = &session.Session{ID: id, Number: number, Agent: "jarvis", Status: session.StatusComplete}
		complete = append(complete, session.SessionSummary{ID: id, Number: number})
	}

	sessStore := &candidateDiscoveryStore{sessions: sessions}
	stateStore := &candidateDiscoveryStateStore{}
	visited := 0
	result, err := opts.walkMineCandidates(context.Background(), sessStore, stateStore, complete, "", func(_ int, _ int, candidate memoryMineCandidate) (memoryMineCandidateVisitResult, error) {
		visited++
		attempted := candidate.Session.ID == "session-1"
		return memoryMineCandidateVisitResult{Attempted: attempted, Successful: attempted}, nil
	})
	if err != nil {
		t.Fatalf("walkMineCandidates: %v", err)
	}
	if visited != 51 || result.Eligible != 51 || result.Attempts != 1 {
		t.Fatalf("walk result = %+v, visited = %d; want 51 eligible visits and 1 attempt", result, visited)
	}
	if len(result.SuccessfulCandidates) != 1 || result.SuccessfulCandidates[0].Session.ID != "session-1" {
		t.Fatalf("successful candidates = %+v, want only session-1", result.SuccessfulCandidates)
	}
	if sessStore.getCalls != 51 {
		t.Fatalf("Get calls = %d, want discovery to stop immediately after the first attempt", sessStore.getCalls)
	}
	if sessStore.maxSequenceCalls != 0 {
		t.Fatalf("maximum sequence calls = %d, want 0 without mining states", sessStore.maxSequenceCalls)
	}
}

func TestWalkMineCandidatesFallbackPreservesLimitSemantics(t *testing.T) {
	opts := MineOptions{Env: testEnv("", ""), Limit: 1, Since: 0}

	sessStore := &candidateDiscoveryFallbackStore{sessions: map[string]*session.Session{
		"newer-mined":   {ID: "newer-mined", Status: session.StatusComplete},
		"older-pending": {ID: "older-pending", Status: session.StatusComplete},
	}}
	stateStore := &candidateDiscoveryStateStore{states: []memorydb.MiningState{{SessionID: "newer-mined", LastMinedOffset: 10}}}
	complete := []session.SessionSummary{{ID: "newer-mined"}, {ID: "older-pending"}}
	visited := 0
	result, err := opts.walkMineCandidates(context.Background(), sessStore, stateStore, complete, "", func(_ int, _ int, candidate memoryMineCandidate) (memoryMineCandidateVisitResult, error) {
		visited++
		attempted := candidate.Session.ID == "older-pending"
		return memoryMineCandidateVisitResult{Attempted: attempted, Successful: attempted}, nil
	})
	if err != nil {
		t.Fatalf("walkMineCandidates: %v", err)
	}
	if visited != 2 || result.Attempts != 1 {
		t.Fatalf("walk result = %+v, visited = %d; want both candidates checked and one attempt", result, visited)
	}
	if sessStore.getCalls != 2 {
		t.Fatalf("Get calls = %d, want fallback point reads for both candidates", sessStore.getCalls)
	}
}

func TestWalkMineCandidatesStopsDiscoveryAtAttemptLimit(t *testing.T) {
	opts := MineOptions{Env: testEnv("", ""), Limit: 2, Since: 0}

	const sessionCount = 20000
	sessions := make(map[string]*session.Session, sessionCount)
	complete := make([]session.SessionSummary, sessionCount)
	for i := range sessionCount {
		id := fmt.Sprintf("session-%d", i)
		sessions[id] = &session.Session{ID: id, Status: session.StatusComplete}
		complete[i] = session.SessionSummary{ID: id}
	}

	sessStore := &candidateDiscoveryFallbackStore{sessions: sessions}
	stateStore := &candidateDiscoveryStateStore{}
	result, err := opts.walkMineCandidates(context.Background(), sessStore, stateStore, complete, "", func(_ int, _ int, candidate memoryMineCandidate) (memoryMineCandidateVisitResult, error) {
		return memoryMineCandidateVisitResult{
			Attempted:  true,
			Successful: candidate.Session.ID == "session-1",
		}, nil
	})
	if err != nil {
		t.Fatalf("walkMineCandidates: %v", err)
	}
	if sessStore.getCalls != 2 || result.Eligible != 2 || result.Attempts != 2 {
		t.Fatalf("walk result = %+v, Get calls = %d; want discovery bounded at 2 attempts", result, sessStore.getCalls)
	}
	if len(result.SuccessfulCandidates) != 1 || result.SuccessfulCandidates[0].Session.ID != "session-1" {
		t.Fatalf("successful candidates = %+v, want only successful session-1", result.SuccessfulCandidates)
	}
}

func TestWalkMineCandidatesKeepsCursorAtMaximumSequence(t *testing.T) {
	opts := MineOptions{Env: testEnv("", ""), Limit: 1, Since: 0}

	const sessionID = "pending-equality"
	sessStore := &candidateDiscoveryStore{
		sessions:     map[string]*session.Session{sessionID: {ID: sessionID, Status: session.StatusComplete}},
		maxSequences: map[string]int{sessionID: 9},
	}
	stateStore := &candidateDiscoveryStateStore{states: []memorydb.MiningState{{SessionID: sessionID, LastMinedOffset: 9}}}
	visited := 0
	result, err := opts.walkMineCandidates(context.Background(), sessStore, stateStore, []session.SessionSummary{{ID: sessionID}}, "", func(_ int, _ int, _ memoryMineCandidate) (memoryMineCandidateVisitResult, error) {
		visited++
		return memoryMineCandidateVisitResult{Attempted: true, Successful: true}, nil
	})
	if err != nil {
		t.Fatalf("walkMineCandidates: %v", err)
	}
	if visited != 1 || result.Attempts != 1 {
		t.Fatalf("walk result = %+v, visited = %d; want sequence 9 to remain pending", result, visited)
	}
}

func TestMinePromoteAgentsUsesSuccessfulCandidatesForImplicitAgents(t *testing.T) {
	if got := minePromoteAgents("", nil); len(got) != 0 {
		t.Fatalf("minePromoteAgents with no successes = %v, want none", got)
	}
	if got := minePromoteAgents("jarvis", nil); len(got) != 1 || got[0] != "jarvis" {
		t.Fatalf("minePromoteAgents with explicit agent = %v, want [jarvis]", got)
	}
	candidates := []memoryMineCandidate{{Agent: "jarvis"}, {Agent: "reviewer"}, {Agent: "jarvis"}}
	got := minePromoteAgents("", candidates)
	if len(got) != 2 || got[0] != "jarvis" || got[1] != "reviewer" {
		t.Fatalf("minePromoteAgents = %v, want [jarvis reviewer]", got)
	}
}

func TestCollectInsightCandidatesAppliesLimitAfterSkippingMinedSessions(t *testing.T) {
	opts := MineOptions{Env: testEnv("", "jarvis"), Limit: 2, Since: 0, IncludeSubagents: false}
	ctx := context.Background()
	sessStore, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer sessStore.Close()

	memStore, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatalf("NewStore(memory): %v", err)
	}
	defer memStore.Close()

	var complete []session.SessionSummary
	for i := 1; i <= 5; i++ {
		sess := &session.Session{
			ID:        session.NewID(),
			Provider:  "test",
			Model:     "test-model",
			Agent:     "jarvis",
			Status:    session.StatusComplete,
			CreatedAt: time.Now().Add(-time.Duration(i) * time.Minute),
			UpdatedAt: time.Now().Add(-time.Duration(i) * time.Minute),
		}
		if err := sessStore.Create(ctx, sess); err != nil {
			t.Fatalf("Create(%d): %v", i, err)
		}
		messageCount := 4
		if i == 3 {
			messageCount = 2
		}
		complete = append(complete, session.SessionSummary{
			ID:           sess.ID,
			Number:       sess.Number,
			MessageCount: messageCount,
			UpdatedAt:    sess.UpdatedAt,
		})

		if i <= 2 {
			if err := memStore.MarkInsightMined(ctx, sess.ID, "jarvis"); err != nil {
				t.Fatalf("MarkInsightMined(%d): %v", i, err)
			}
		}
	}

	got, err := opts.collectInsightCandidates(ctx, sessStore, memStore, complete, "")
	if err != nil {
		t.Fatalf("collectInsightCandidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Session.ID != complete[3].ID || got[1].Session.ID != complete[4].ID {
		t.Fatalf("selected sessions = [%s %s], want [%s %s]", got[0].Session.ID, got[1].Session.ID, complete[3].ID, complete[4].ID)
	}
}

func TestMemoryExtractionCollectorAndTools(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	store, err := memorydb.NewStore(memorydb.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	collector := newMemoryExtractionCollector()
	createTool := &memoryCreateFragmentTool{store: store, agent: "jarvis", collector: collector}
	updateTool := &memoryUpdateFragmentTool{store: store, agent: "jarvis", collector: collector}

	if _, err := createTool.Execute(ctx, json.RawMessage(`{"path":"fragments/new.md","content":"new content"}`)); err != nil {
		t.Fatalf("create tool error = %v", err)
	}
	if _, err := updateTool.Execute(ctx, json.RawMessage(`{"path":"fragments/new.md","content":"updated content"}`)); err != nil {
		t.Fatalf("update tool error = %v", err)
	}

	res := collector.result("done")
	if res.Created != 1 || res.Updated != 1 || res.Skipped != 0 {
		t.Fatalf("result counts = %+v, want create=1 update=1 skip=0", res)
	}
	if len(res.AffectedPaths) != 1 || res.AffectedPaths[0] != "fragments/new.md" {
		t.Fatalf("affected paths = %+v", res.AffectedPaths)
	}
}

func TestMemoryAgentFragmentCache_ReusesLoadedCorpusAndAppliesIncrementalChanges(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	initial := memorydb.Fragment{
		Agent:     "jarvis",
		Path:      "fragments/a.md",
		Content:   "old content",
		UpdatedAt: now.Add(-time.Minute),
	}
	updated := memorydb.Fragment{
		Agent:     "jarvis",
		Path:      "fragments/a.md",
		Content:   "updated content",
		UpdatedAt: now,
	}
	created := memorydb.Fragment{
		Agent:     "jarvis",
		Path:      "fragments/b.md",
		Content:   "new content",
		UpdatedAt: now.Add(30 * time.Second),
	}
	store := &fakeMemoryAgentFragmentStore{
		listByKey: map[string][]memorydb.Fragment{
			"jarvis": {initial},
		},
		getByKey: map[string]map[string]memorydb.Fragment{
			"jarvis": {
				initial.Path: initial,
			},
		},
	}

	cache := newMemoryAgentFragmentCache(200)
	fragments, taxonomy, err := cache.get(ctx, store, "jarvis")
	if err != nil {
		t.Fatalf("cache.get(initial): %v", err)
	}
	if store.listCalls != 1 {
		t.Fatalf("listCalls after first get = %d, want 1", store.listCalls)
	}
	if len(fragments) != 1 || fragments[0].Content != "old content" {
		t.Fatalf("initial fragments = %+v", fragments)
	}
	if !strings.Contains(taxonomy, "total_fragments: 1") {
		t.Fatalf("initial taxonomy = %q, want total_fragments: 1", taxonomy)
	}

	store.getByKey["jarvis"][updated.Path] = updated
	store.getByKey["jarvis"][created.Path] = created
	if err := cache.applyChanges(ctx, store, "jarvis", []string{updated.Path, created.Path}); err != nil {
		t.Fatalf("cache.applyChanges: %v", err)
	}

	fragments, taxonomy, err = cache.get(ctx, store, "jarvis")
	if err != nil {
		t.Fatalf("cache.get(updated): %v", err)
	}
	if store.listCalls != 1 {
		t.Fatalf("listCalls after cache reuse = %d, want 1", store.listCalls)
	}
	if store.getCalls != 2 {
		t.Fatalf("getCalls after incremental refresh = %d, want 2", store.getCalls)
	}
	if len(fragments) != 2 {
		t.Fatalf("len(fragments) = %d, want 2", len(fragments))
	}

	seen := map[string]string{}
	for _, frag := range fragments {
		seen[frag.Path] = frag.Content
	}
	if seen[updated.Path] != updated.Content {
		t.Fatalf("updated fragment content = %q, want %q", seen[updated.Path], updated.Content)
	}
	if seen[created.Path] != created.Content {
		t.Fatalf("created fragment content = %q, want %q", seen[created.Path], created.Content)
	}
	if !strings.Contains(taxonomy, "total_fragments: 2") {
		t.Fatalf("updated taxonomy = %q, want total_fragments: 2", taxonomy)
	}
}

func TestBuildTaxonomyMap_RespectsBudget(t *testing.T) {
	fragments := make([]memorydb.Fragment, 0, 20)
	for i := 0; i < 20; i++ {
		fragments = append(fragments, memorydb.Fragment{
			Path:      filepath.ToSlash(filepath.Join("fragments", "prefs", "topic", time.Now().Format("150405"), strings.Repeat("x", 20)+".md")),
			UpdatedAt: time.Now().Add(-time.Duration(i) * time.Minute),
		})
	}
	got := buildTaxonomyMap(fragments, 60)
	if tokens := llm.EstimateTokens(got); tokens > 60 {
		t.Fatalf("taxonomy tokens = %d, want <= 60\n%s", tokens, got)
	}
	if !strings.Contains(got, "total_fragments") {
		t.Fatalf("taxonomy map missing summary: %s", got)
	}
}

func TestLoadMessagesForMining_RespectsPromptBudget(t *testing.T) {
	opts := MineOptions{Env: testEnv("", ""), PromptMaxTokens: 500, BatchSize: 10, MaxMessages: 0}
	ctx := context.Background()
	sessStore, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer sessStore.Close()

	sess := &session.Session{
		ID:        session.NewID(),
		Provider:  "test",
		Model:     "test-model",
		Mode:      session.ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := sessStore.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 6; i++ {
		msg := session.NewMessage(sess.ID, llm.UserText(strings.Repeat("long durable text ", 80)), -1)
		if err := sessStore.AddMessage(ctx, sess.ID, msg); err != nil {
			t.Fatalf("AddMessage(%d): %v", i, err)
		}
	}

	candidate := memoryMineCandidate{
		Summary: session.SessionSummary{Number: 1},
		Session: sess,
		Agent:   "jarvis",
	}
	loadResult, err := opts.loadMessagesForMining(ctx, sessStore, candidate, 0, "Memory fragment map:\n- total_fragments: 0")
	if err != nil {
		t.Fatalf("loadMessagesForMining: %v", err)
	}
	if len(loadResult.Messages) == 0 {
		t.Fatal("expected at least one message")
	}
	if loadResult.NextOffset >= 6 {
		t.Fatalf("nextOffset = %d, want partial batch due to prompt budget", loadResult.NextOffset)
	}
	if got := estimateExtractionPromptTokens(candidate, 0, loadResult.NextOffset, loadResult.Messages, "Memory fragment map:\n- total_fragments: 0"); got > opts.PromptMaxTokens {
		t.Fatalf("estimated prompt tokens = %d, want <= %d", got, opts.PromptMaxTokens)
	}
}

func TestLoadMessagesForMining_UsesSequencePagination(t *testing.T) {
	opts := MineOptions{Env: testEnv("", ""), PromptMaxTokens: 1 << 30, BatchSize: 2, MaxMessages: 0}
	ctx := context.Background()

	candidate := memoryMineCandidate{
		Summary: session.SessionSummary{Number: 1},
		Session: &session.Session{ID: "sess-1"},
		Agent:   "jarvis",
	}
	store := &trackingMemoryMineStore{
		messages: map[string][]session.Message{
			"sess-1": {
				{SessionID: "sess-1", Role: llm.RoleUser, TextContent: "msg-0", Sequence: 0},
				{SessionID: "sess-1", Role: llm.RoleUser, TextContent: "msg-1", Sequence: 1},
				{SessionID: "sess-1", Role: llm.RoleUser, TextContent: "msg-2", Sequence: 2},
				{SessionID: "sess-1", Role: llm.RoleUser, TextContent: "msg-3", Sequence: 3},
			},
		},
	}

	loadResult, err := opts.loadMessagesForMining(ctx, store, candidate, 1, "Memory fragment map:\n- total_fragments: 0")
	if err != nil {
		t.Fatalf("loadMessagesForMining: %v", err)
	}
	if store.getMessagesCalls != 0 {
		t.Fatalf("GetMessages calls = %d, want 0", store.getMessagesCalls)
	}
	if len(store.getMessagesFromCalls) != 2 {
		t.Fatalf("GetMessagesFrom calls = %d, want 2", len(store.getMessagesFromCalls))
	}
	if store.getMessagesFromCalls[0].fromSeq != 1 || store.getMessagesFromCalls[0].limit != 2 {
		t.Fatalf("first GetMessagesFrom call = %+v, want fromSeq=1 limit=2", store.getMessagesFromCalls[0])
	}
	if store.getMessagesFromCalls[1].fromSeq != 3 || store.getMessagesFromCalls[1].limit != 2 {
		t.Fatalf("second GetMessagesFrom call = %+v, want fromSeq=3 limit=2", store.getMessagesFromCalls[1])
	}
	if loadResult.NextOffset != 4 {
		t.Fatalf("NextOffset = %d, want 4", loadResult.NextOffset)
	}
	if len(loadResult.Messages) != 3 {
		t.Fatalf("len(Messages) = %d, want 3", len(loadResult.Messages))
	}
	for i, wantSeq := range []int{1, 2, 3} {
		if loadResult.Messages[i].Sequence != wantSeq {
			t.Fatalf("Messages[%d].Sequence = %d, want %d", i, loadResult.Messages[i].Sequence, wantSeq)
		}
	}
}

func TestLoadMessagesForMiningPersistsNextSequenceWithGaps(t *testing.T) {
	opts := MineOptions{Env: testEnv("", ""), PromptMaxTokens: 1 << 30, BatchSize: 2, MaxMessages: 0}
	ctx := context.Background()

	candidate := memoryMineCandidate{
		Summary: session.SessionSummary{Number: 1},
		Session: &session.Session{ID: "sess-gap"},
		Agent:   "jarvis",
	}
	store := &trackingMemoryMineStore{
		messages: map[string][]session.Message{
			"sess-gap": {
				{SessionID: "sess-gap", Role: llm.RoleUser, TextContent: "msg-10", Sequence: 10},
				{SessionID: "sess-gap", Role: llm.RoleUser, TextContent: "msg-20", Sequence: 20},
			},
		},
	}

	loadResult, err := opts.loadMessagesForMining(ctx, store, candidate, 10, "Memory fragment map:\n- total_fragments: 0")
	if err != nil {
		t.Fatalf("loadMessagesForMining: %v", err)
	}
	if loadResult.NextOffset != 21 {
		t.Fatalf("NextOffset = %d, want next sequence 21", loadResult.NextOffset)
	}
	if len(store.getMessagesFromCalls) != 2 {
		t.Fatalf("GetMessagesFrom calls = %d, want 2", len(store.getMessagesFromCalls))
	}
	if store.getMessagesFromCalls[1].fromSeq != 21 {
		t.Fatalf("second GetMessagesFrom fromSeq = %d, want 21", store.getMessagesFromCalls[1].fromSeq)
	}
}

func TestLoadMessagesForMiningIgnoresMessageCountWhenUsingSequenceCursor(t *testing.T) {
	opts := MineOptions{Env: testEnv("", ""), PromptMaxTokens: 1 << 30, BatchSize: 10, MaxMessages: 0}
	ctx := context.Background()

	candidate := memoryMineCandidate{
		Summary: session.SessionSummary{Number: 1, MessageCount: 3},
		Session: &session.Session{ID: "sess-sparse"},
		Agent:   "jarvis",
	}
	store := &trackingMemoryMineStore{
		messages: map[string][]session.Message{
			"sess-sparse": {
				{SessionID: "sess-sparse", Role: llm.RoleUser, TextContent: "old", Sequence: 10},
				{SessionID: "sess-sparse", Role: llm.RoleUser, TextContent: "new", Sequence: 21},
			},
		},
	}

	loadResult, err := opts.loadMessagesForMining(ctx, store, candidate, 21, "Memory fragment map:\n- total_fragments: 0")
	if err != nil {
		t.Fatalf("loadMessagesForMining: %v", err)
	}
	if len(loadResult.Messages) != 1 {
		t.Fatalf("len(Messages) = %d, want 1", len(loadResult.Messages))
	}
	if got := loadResult.Messages[0].TextContent; got != "new" {
		t.Fatalf("loaded message text = %q, want new", got)
	}
	if loadResult.NextOffset != 22 {
		t.Fatalf("NextOffset = %d, want 22", loadResult.NextOffset)
	}
}

func TestFitMessagesForPromptBudget_PrefersUserAndDurableAssistantText(t *testing.T) {
	opts := MineOptions{Env: testEnv("", ""), PromptMaxTokens: 1000}

	candidate := memoryMineCandidate{
		Summary: session.SessionSummary{Number: 1},
		Session: &session.Session{ID: "sess-1"},
		Agent:   "jarvis",
	}
	messages := []session.Message{
		{Role: llm.RoleUser, TextContent: strings.Repeat("user durable preference ", 18)},
		{Role: llm.RoleAssistant, TextContent: strings.Repeat("ok sure maybe maybe ", 45)},
		{Role: llm.RoleAssistant, TextContent: strings.Repeat("Changed config path /etc/service and updated model budget summary. ", 18)},
	}

	fit, ok := opts.fitMessagesForPromptBudget(candidate, 0, len(messages), messages, "Memory fragment map:\n- total_fragments: 0")
	if !ok {
		t.Fatal("expected fitMessagesForPromptBudget to succeed")
	}
	if got := estimateExtractionPromptTokens(candidate, 0, len(messages), fit.Messages, "Memory fragment map:\n- total_fragments: 0"); got > opts.PromptMaxTokens {
		t.Fatalf("estimated prompt tokens = %d, want <= %d", got, opts.PromptMaxTokens)
	}
	if fit.Messages[0].TextContent != messages[0].TextContent {
		t.Fatalf("user message should be preserved, got %q", fit.Messages[0].TextContent)
	}
	if utf8.RuneCountInString(fit.Messages[2].TextContent) <= utf8.RuneCountInString(fit.Messages[1].TextContent) {
		t.Fatalf("durable assistant text should retain more budget than filler: durable=%d filler=%d", utf8.RuneCountInString(fit.Messages[2].TextContent), utf8.RuneCountInString(fit.Messages[1].TextContent))
	}
	if fit.AssistantMessagesCut == 0 {
		t.Fatal("expected assistant truncation to occur")
	}
	if fit.UserMessagesCut != 0 {
		t.Fatalf("expected no user truncation, got %d", fit.UserMessagesCut)
	}
}

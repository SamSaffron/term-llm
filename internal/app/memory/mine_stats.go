package memory

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session"
)

type memoryPromptParts struct {
	Metadata     string
	TaxonomyMap  string
	Transcript   string
	Instructions string
	Prompt       string
}

type memoryMineLoadResult struct {
	Messages             []session.Message
	NextOffset           int
	TruncatedMessages    int
	AssistantMessagesCut int
	UserMessagesCut      int
	PromptBudgetHit      bool
	SingleMessageClipped bool
}

type extractionRequestStats interface {
	noteToolCall(name string)
	noteUsage(use *core.Usage)
}

type memoryExtractionStats struct {
	SessionID                 string
	SessionNumber             int64
	Agent                     string
	StartOffset               int
	EndOffset                 int
	ExistingFragments         int
	TranscriptMessages        int
	PromptMaxTokens           int
	PromptEstimatedTokens     int
	SystemTokens              int
	MetadataTokens            int
	TaxonomyTokens            int
	TranscriptTokens          int
	TranscriptUserTokens      int
	TranscriptAssistantTokens int
	InstructionsTokens        int
	ToolTurns                 int
	ToolCalls                 int
	ToolNames                 []string
	InputTokens               int
	CachedInputTokens         int
	CacheWriteTokens          int
	OutputTokens              int
	TruncatedMessages         int
	AssistantMessagesCut      int
	UserMessagesCut           int
	PromptBudgetHit           bool
	SingleMessageClipped      bool
	Duration                  time.Duration
}

type memoryMineSummaryStats struct {
	Batches           int
	TotalDuration     time.Duration
	TotalPromptTokens int
	TotalToolCalls    int
	TotalToolTurns    int
	TotalInputTokens  int
	TotalOutputTokens int
	Slowest           []memoryExtractionStats
}

func buildExtractionPromptParts(candidate memoryMineCandidate, startOffset, endOffset int, messages []session.Message, taxonomyMap string) memoryPromptParts {
	transcriptJSON, _ := json.MarshalIndent(buildTranscript(messages), "", "  ")
	metadata := fmt.Sprintf(`Session metadata:
- session_id: %s
- session_number: %d
- agent: %s
- mined_message_range: [%d, %d)
- transcript_message_count: %d`,
		candidate.Session.ID,
		candidate.Summary.Number,
		candidate.Agent,
		startOffset,
		endOffset,
		len(messages),
	)
	instructions := `Instructions:
- Extract only durable facts, decisions, preferences, and technical details worth remembering long-term.
- Skip ephemeral content like specific transient errors, one-off debugging output, and conversational filler.
- The fragment map is intentionally compact and may omit exact duplicates or existing content details.
- Use lookup tools when you need to confirm whether related memory already exists, inspect exact fragment content, or browse likely paths.
- Use memory_create_fragment for genuinely new memory.
- Use memory_update_fragment when an existing fragment should be revised.
- Avoid duplicate fragments.
- Fragment content must stay <= 8192 bytes.
- Paths must be relative and must not contain ../ or be absolute.
- You may apply multiple create/update tool calls in one extraction batch.
- When finished, reply with plain text such as "done". Do not return JSON.`
	prompt := fmt.Sprintf(`%s

Existing fragment map (compact, partial by design):
%s

Transcript (role + text + tool call names only):
%s

%s`, metadata, taxonomyMap, string(transcriptJSON), instructions)
	return memoryPromptParts{
		Metadata:     metadata,
		TaxonomyMap:  taxonomyMap,
		Transcript:   string(transcriptJSON),
		Instructions: instructions,
		Prompt:       prompt,
	}
}

func (opts MineOptions) newMemoryExtractionStats(candidate memoryMineCandidate, startOffset, endOffset int, existingFragments int, load memoryMineLoadResult, parts memoryPromptParts) memoryExtractionStats {
	userTokens, assistantTokens := transcriptRoleTokenBreakdown(load.Messages)
	return memoryExtractionStats{
		SessionID:                 candidate.Session.ID,
		SessionNumber:             candidate.Summary.Number,
		Agent:                     candidate.Agent,
		StartOffset:               startOffset,
		EndOffset:                 endOffset,
		ExistingFragments:         existingFragments,
		TranscriptMessages:        len(load.Messages),
		PromptMaxTokens:           opts.PromptMaxTokens,
		PromptEstimatedTokens:     engine.EstimateTokens(memoryExtractionSystemPrompt) + engine.EstimateTokens(parts.Prompt),
		SystemTokens:              engine.EstimateTokens(memoryExtractionSystemPrompt),
		MetadataTokens:            engine.EstimateTokens(parts.Metadata),
		TaxonomyTokens:            engine.EstimateTokens(parts.TaxonomyMap),
		TranscriptTokens:          engine.EstimateTokens(parts.Transcript),
		TranscriptUserTokens:      userTokens,
		TranscriptAssistantTokens: assistantTokens,
		InstructionsTokens:        engine.EstimateTokens(parts.Instructions),
		TruncatedMessages:         load.TruncatedMessages,
		AssistantMessagesCut:      load.AssistantMessagesCut,
		UserMessagesCut:           load.UserMessagesCut,
		PromptBudgetHit:           load.PromptBudgetHit,
		SingleMessageClipped:      load.SingleMessageClipped,
	}
}

func transcriptRoleTokenBreakdown(messages []session.Message) (userTokens, assistantTokens int) {
	for _, msg := range messages {
		text := strings.TrimSpace(msg.TextContent)
		if text == "" {
			continue
		}
		tokens := engine.EstimateTokens(text)
		switch msg.Role {
		case core.RoleUser:
			userTokens += tokens
		case core.RoleAssistant:
			assistantTokens += tokens
		}
	}
	return userTokens, assistantTokens
}

func (s *memoryExtractionStats) noteToolCall(name string) {
	s.ToolCalls++
	for _, existing := range s.ToolNames {
		if existing == name {
			return
		}
	}
	s.ToolNames = append(s.ToolNames, name)
	sort.Strings(s.ToolNames)
}

func (s *memoryExtractionStats) noteUsage(use *core.Usage) {
	if use == nil {
		return
	}
	s.ToolTurns++
	s.InputTokens += use.InputTokens
	s.CachedInputTokens += use.CachedInputTokens
	s.CacheWriteTokens += use.CacheWriteTokens
	s.OutputTokens += use.OutputTokens
}

func (s memoryExtractionStats) oneLine() string {
	tools := "-"
	if len(s.ToolNames) > 0 {
		tools = strings.Join(s.ToolNames, ",")
	}
	return fmt.Sprintf("stats: wall=%s prompt≈%dt/%dt system=%dt meta=%dt taxonomy=%dt transcript=%dt(user=%dt assistant=%dt) instr=%dt existing=%d msgs=%d turns=%d tool_calls=%d tools=%s usage[in=%d cached=%d out=%d write=%d] truncated_msgs=%d assistant_cut=%d user_cut=%d budget_hit=%t single_clip=%t",
		s.Duration.Round(time.Millisecond),
		s.PromptEstimatedTokens,
		s.PromptMaxTokens,
		s.SystemTokens,
		s.MetadataTokens,
		s.TaxonomyTokens,
		s.TranscriptTokens,
		s.TranscriptUserTokens,
		s.TranscriptAssistantTokens,
		s.InstructionsTokens,
		s.ExistingFragments,
		s.TranscriptMessages,
		s.ToolTurns,
		s.ToolCalls,
		tools,
		s.InputTokens,
		s.CachedInputTokens,
		s.OutputTokens,
		s.CacheWriteTokens,
		s.TruncatedMessages,
		s.AssistantMessagesCut,
		s.UserMessagesCut,
		s.PromptBudgetHit,
		s.SingleMessageClipped,
	)
}

func (m *memoryMineSummaryStats) add(s memoryExtractionStats) {
	m.Batches++
	m.TotalDuration += s.Duration
	m.TotalPromptTokens += s.PromptEstimatedTokens
	m.TotalToolCalls += s.ToolCalls
	m.TotalToolTurns += s.ToolTurns
	m.TotalInputTokens += s.InputTokens + s.CachedInputTokens
	m.TotalOutputTokens += s.OutputTokens
	m.Slowest = append(m.Slowest, s)
	sort.Slice(m.Slowest, func(i, j int) bool { return m.Slowest[i].Duration > m.Slowest[j].Duration })
	if len(m.Slowest) > 5 {
		m.Slowest = m.Slowest[:5]
	}
}

func (m memoryMineSummaryStats) print() {
	if m.Batches == 0 {
		return
	}
	avgPrompt := m.TotalPromptTokens / m.Batches
	avgDuration := time.Duration(int64(m.TotalDuration) / int64(m.Batches))
	fmt.Printf("\n--- MEMORY MINE STATS ---\n")
	fmt.Printf("batches=%d total_wall=%s avg_wall=%s avg_prompt≈%dt total_tool_calls=%d total_tool_turns=%d total_input=%d total_output=%d\n",
		m.Batches,
		m.TotalDuration.Round(time.Millisecond),
		avgDuration.Round(time.Millisecond),
		avgPrompt,
		m.TotalToolCalls,
		m.TotalToolTurns,
		m.TotalInputTokens,
		m.TotalOutputTokens,
	)
	if len(m.Slowest) > 0 {
		fmt.Println("slowest batches:")
		for _, s := range m.Slowest {
			fmt.Printf("- #%d [%d,%d) wall=%s prompt≈%dt tools=%d (%s) budget_hit=%t\n",
				s.SessionNumber,
				s.StartOffset,
				s.EndOffset,
				s.Duration.Round(time.Millisecond),
				s.PromptEstimatedTokens,
				s.ToolCalls,
				strings.Join(s.ToolNames, ","),
				s.PromptBudgetHit,
			)
		}
	}
}

type insightTranscriptStats struct {
	Messages               int
	UserTokens             int
	AssistantTokens        int
	ToolTokens             int
	NonUserTokens          int
	RawUserTokens          int
	RawAssistantTokens     int
	RawToolTokens          int
	DroppedSystem          int
	DroppedAssistantTokens int
	DroppedToolTokens      int
}

type insightExtractionStats struct {
	SessionID             string
	SessionNumber         int64
	Agent                 string
	Duration              time.Duration
	PromptEstimatedTokens int
	SystemTokens          int
	ExistingInsights      int
	Transcript            insightTranscriptStats
	InputTokens           int
	CachedInputTokens     int
	CacheWriteTokens      int
	OutputTokens          int
	CreatedOrReinforced   int
	SkippedShort          bool
}

type insightExtractionSummaryStats struct {
	Sessions             int
	TotalDuration        time.Duration
	TotalPromptTokens    int
	TotalInputTokens     int
	TotalOutputTokens    int
	CreatedOrReinforced  int
	TotalUserTokens      int
	TotalAssistantTokens int
	TotalToolTokens      int
	TotalNonUserTokens   int
	BudgetViolations     int
	Slowest              []insightExtractionStats
}

func newInsightExtractionStats(candidate memoryMineCandidate, messages []session.Message, transcript []transcriptMessage, existingInsights int, prompt string) insightExtractionStats {
	transcriptStats := summarizeInsightTranscriptStats(messages, transcript)
	return insightExtractionStats{
		SessionID:             candidate.Session.ID,
		SessionNumber:         candidate.Summary.Number,
		Agent:                 candidate.Agent,
		PromptEstimatedTokens: engine.EstimateTokens(insightExtractionSystemPrompt) + engine.EstimateTokens(prompt),
		SystemTokens:          engine.EstimateTokens(insightExtractionSystemPrompt),
		ExistingInsights:      existingInsights,
		Transcript:            transcriptStats,
	}
}

func summarizeInsightTranscriptStats(messages []session.Message, transcript []transcriptMessage) insightTranscriptStats {
	stats := insightTranscriptStats{Messages: len(transcript)}
	for _, msg := range messages {
		text := strings.TrimSpace(msg.TextContent)
		switch msg.Role {
		case core.RoleSystem:
			stats.DroppedSystem++
		case core.RoleUser:
			stats.RawUserTokens += engine.EstimateTokens(text)
		case core.RoleAssistant:
			stats.RawAssistantTokens += engine.EstimateTokens(text)
			for _, summary := range summarizeInsightToolCalls(msg) {
				stats.RawToolTokens += engine.EstimateTokens(summary)
			}
		case core.RoleTool:
			for _, summary := range summarizeInsightToolResults(msg) {
				stats.RawToolTokens += engine.EstimateTokens(summary)
			}
		}
	}
	for _, msg := range transcript {
		tokens := engine.EstimateTokens(strings.TrimSpace(msg.Text))
		switch msg.Role {
		case string(core.RoleUser):
			stats.UserTokens += tokens
		case string(core.RoleTool):
			stats.ToolTokens += tokens
		default:
			stats.AssistantTokens += tokens
		}
	}
	stats.NonUserTokens = stats.AssistantTokens + stats.ToolTokens
	if stats.RawAssistantTokens > stats.AssistantTokens {
		stats.DroppedAssistantTokens = stats.RawAssistantTokens - stats.AssistantTokens
	}
	if stats.RawToolTokens > stats.ToolTokens {
		stats.DroppedToolTokens = stats.RawToolTokens - stats.ToolTokens
	}
	return stats
}

func (s *insightExtractionStats) noteToolCall(name string) {}

func (s *insightExtractionStats) noteUsage(use *core.Usage) {
	if use == nil {
		return
	}
	s.InputTokens += use.InputTokens
	s.CachedInputTokens += use.CachedInputTokens
	s.CacheWriteTokens += use.CacheWriteTokens
	s.OutputTokens += use.OutputTokens
}

func (s insightExtractionStats) oneLine() string {
	return fmt.Sprintf("insight stats: wall=%s prompt≈%dt system=%dt existing=%d transcript_msgs=%d transcript=%dt(user=%dt assistant=%dt tool=%dt non_user=%dt raw_user=%dt raw_assistant=%dt raw_tool=%dt dropped_assistant=%dt dropped_tool=%dt budget_ok=%t usage[in=%d cached=%d out=%d write=%d] insights=%d",
		s.Duration.Round(time.Millisecond),
		s.PromptEstimatedTokens,
		s.SystemTokens,
		s.ExistingInsights,
		s.Transcript.Messages,
		s.Transcript.UserTokens+s.Transcript.NonUserTokens,
		s.Transcript.UserTokens,
		s.Transcript.AssistantTokens,
		s.Transcript.ToolTokens,
		s.Transcript.NonUserTokens,
		s.Transcript.RawUserTokens,
		s.Transcript.RawAssistantTokens,
		s.Transcript.RawToolTokens,
		s.Transcript.DroppedAssistantTokens,
		s.Transcript.DroppedToolTokens,
		s.Transcript.NonUserTokens <= s.Transcript.UserTokens,
		s.InputTokens,
		s.CachedInputTokens,
		s.OutputTokens,
		s.CacheWriteTokens,
		s.CreatedOrReinforced,
	)
}

func (m *insightExtractionSummaryStats) add(s insightExtractionStats) {
	m.Sessions++
	m.TotalDuration += s.Duration
	m.TotalPromptTokens += s.PromptEstimatedTokens
	m.TotalInputTokens += s.InputTokens + s.CachedInputTokens
	m.TotalOutputTokens += s.OutputTokens
	m.CreatedOrReinforced += s.CreatedOrReinforced
	m.TotalUserTokens += s.Transcript.UserTokens
	m.TotalAssistantTokens += s.Transcript.AssistantTokens
	m.TotalToolTokens += s.Transcript.ToolTokens
	m.TotalNonUserTokens += s.Transcript.NonUserTokens
	if s.Transcript.NonUserTokens > s.Transcript.UserTokens {
		m.BudgetViolations++
	}
	m.Slowest = append(m.Slowest, s)
	sort.Slice(m.Slowest, func(i, j int) bool { return m.Slowest[i].Duration > m.Slowest[j].Duration })
	if len(m.Slowest) > 5 {
		m.Slowest = m.Slowest[:5]
	}
}

func (m insightExtractionSummaryStats) print() {
	if m.Sessions == 0 {
		return
	}
	avgPrompt := m.TotalPromptTokens / m.Sessions
	avgDuration := time.Duration(int64(m.TotalDuration) / int64(m.Sessions))
	fmt.Printf("\n--- INSIGHT EXTRACTION STATS ---\n")
	fmt.Printf("sessions=%d total_wall=%s avg_wall=%s avg_prompt≈%dt total_input=%d total_output=%d insights=%d transcript_tokens(user=%d assistant=%d tool=%d non_user=%d) budget_violations=%d\n",
		m.Sessions,
		m.TotalDuration.Round(time.Millisecond),
		avgDuration.Round(time.Millisecond),
		avgPrompt,
		m.TotalInputTokens,
		m.TotalOutputTokens,
		m.CreatedOrReinforced,
		m.TotalUserTokens,
		m.TotalAssistantTokens,
		m.TotalToolTokens,
		m.TotalNonUserTokens,
		m.BudgetViolations,
	)
	if len(m.Slowest) > 0 {
		fmt.Println("slowest insight sessions:")
		for _, s := range m.Slowest {
			fmt.Printf("- #%d wall=%s prompt≈%dt transcript(user=%dt assistant=%dt tool=%dt non_user=%dt) budget_ok=%t insights=%d\n",
				s.SessionNumber,
				s.Duration.Round(time.Millisecond),
				s.PromptEstimatedTokens,
				s.Transcript.UserTokens,
				s.Transcript.AssistantTokens,
				s.Transcript.ToolTokens,
				s.Transcript.NonUserTokens,
				s.Transcript.NonUserTokens <= s.Transcript.UserTokens,
				s.CreatedOrReinforced,
			)
		}
	}
}

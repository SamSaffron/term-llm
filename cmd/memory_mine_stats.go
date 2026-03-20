package cmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
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

func newMemoryExtractionStats(candidate memoryMineCandidate, startOffset, endOffset int, existingFragments int, load memoryMineLoadResult, parts memoryPromptParts) memoryExtractionStats {
	userTokens, assistantTokens := transcriptRoleTokenBreakdown(load.Messages)
	return memoryExtractionStats{
		SessionID:                 candidate.Session.ID,
		SessionNumber:             candidate.Summary.Number,
		Agent:                     candidate.Agent,
		StartOffset:               startOffset,
		EndOffset:                 endOffset,
		ExistingFragments:         existingFragments,
		TranscriptMessages:        len(load.Messages),
		PromptMaxTokens:           memoryMinePromptMaxTokens,
		PromptEstimatedTokens:     llm.EstimateTokens(memoryExtractionSystemPrompt) + llm.EstimateTokens(parts.Prompt),
		SystemTokens:              llm.EstimateTokens(memoryExtractionSystemPrompt),
		MetadataTokens:            llm.EstimateTokens(parts.Metadata),
		TaxonomyTokens:            llm.EstimateTokens(parts.TaxonomyMap),
		TranscriptTokens:          llm.EstimateTokens(parts.Transcript),
		TranscriptUserTokens:      userTokens,
		TranscriptAssistantTokens: assistantTokens,
		InstructionsTokens:        llm.EstimateTokens(parts.Instructions),
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
		tokens := llm.EstimateTokens(text)
		switch msg.Role {
		case llm.RoleUser:
			userTokens += tokens
		case llm.RoleAssistant:
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

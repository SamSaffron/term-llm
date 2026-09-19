package guardian

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/typesafe"
)

// These budgets apply only to TypeSafe Guardian reviews, not to LLM Guardian
// prompts or the standalone classify client. Count serialized bytes, including
// questions, model, metadata and JSON escaping.
const (
	maxClassifyRequestBytes       = 24000
	maxClassifyUserBytes          = 8000
	maxClassifyUserEntryBytes     = 3000
	maxClassifyApprovalBytes      = 2000
	maxClassifyEvidenceBytes      = 1500
	maxClassifyEvidenceEntryBytes = 500
	maxClassifyEvidenceEntries    = 6
)

type classifyTranscriptEntry struct {
	Index     int    `json:"index"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

type classifyPacket struct {
	Policy                   string                    `json:"policy"`
	Transcript               []classifyTranscriptEntry `json:"transcript"`
	Omitted                  int                       `json:"omitted_count"`
	OmittedUsers             int                       `json:"omitted_user_count"`
	Truncated                int                       `json:"truncated_count"`
	ApprovalContext          string                    `json:"approval_context"`
	ApprovalContextTruncated bool                      `json:"approval_context_truncated"`
	Action                   classifyAction            `json:"action"`
}

// buildClassifyRequest reserves the exact action and policy first, then budgets
// authorization history, current permissions and supporting evidence. History
// and accumulated approvals must not make an ordinary short action unreviewable.
// Only an oversized exact action/policy/model/questions can fail the size guard.
func buildClassifyRequest(req Request, policy, model string) (typesafe.Request, error) {
	packet := classifyPacket{Policy: policy, Transcript: make([]classifyTranscriptEntry, 0), Action: classifyRequestAction(req)}
	approvalContext := req.ApprovalContext
	if packet.Action.Type == "shell" && (req.ApprovalScope == "" || req.ApprovalScope == "local") {
		prefix := fmt.Sprintf("shell_command=%q\nworkdir=%q\n", strings.TrimSpace(req.Command), req.WorkDir)
		approvalContext = strings.TrimPrefix(approvalContext, prefix)
	}
	packet.ApprovalContextTruncated = approvalContext != ""
	for _, entry := range req.Transcript {
		if strings.TrimSpace(entry.Text) != "" {
			packet.Omitted++
			if classifyUserRole(entry.Role) {
				packet.OmittedUsers++
			}
		}
	}

	request := typesafe.Request{Model: model, Questions: classifyQuestions()}
	request, size, err := marshalClassifyPacket(request, packet)
	if err != nil {
		return request, err
	}
	if size > maxClassifyRequestBytes {
		return request, fmt.Errorf("exact action, policy and classification schema require %d bytes, exceeding the %d-byte TypeSafe Guardian request limit; use manual approval", size, maxClassifyRequestBytes)
	}

	userBytes := 2 // JSON array brackets
	for _, i := range classifyUserOrder(req.Transcript) {
		entry := req.Transcript[i]
		budget := min(maxClassifyUserEntryBytes, maxClassifyUserBytes-userBytes-1, maxClassifyRequestBytes-size)
		excerpt, encodedBytes := classifyEntry(req.TranscriptOffset+i+1, strings.ToLower(strings.TrimSpace(entry.Role)), entry.Text, budget)
		if encodedBytes > budget {
			continue
		}
		accepted, nextSize, err := appendClassifyEntry(&request, &packet, excerpt)
		if err != nil {
			return request, err
		}
		if accepted {
			size = nextSize
			userBytes += encodedBytes + 1
		}
	}

	// Approval context consists of permission evidence, not an unbounded ledger
	// of past commands. Keep whole lines: a clipped quoted path or shell command
	// could misrepresent the scope of a grant. The exact action remains separate.
	budget := min(maxClassifyApprovalBytes, maxClassifyRequestBytes-size)
	packet.ApprovalContext, packet.ApprovalContextTruncated = compactClassifyApprovalContext(approvalContext, budget)
	request, size, err = marshalClassifyPacket(request, packet)
	if err != nil {
		return request, err
	}
	if size > maxClassifyRequestBytes {
		return request, fmt.Errorf("encoded TypeSafe Guardian request exceeds %d bytes; use manual approval", maxClassifyRequestBytes)
	}

	evidenceBytes, considered := 2, 0 // JSON array brackets
	for i := len(req.Transcript) - 1; i >= 0 && considered < maxClassifyEvidenceEntries; i-- {
		entry := req.Transcript[i]
		role := strings.ToLower(strings.TrimSpace(entry.Role))
		if !classifyEvidenceRole(role) || strings.TrimSpace(entry.Text) == "" {
			continue
		}
		considered++
		budget := min(maxClassifyEvidenceEntryBytes, maxClassifyEvidenceBytes-evidenceBytes-1, maxClassifyRequestBytes-size)
		excerpt, encodedBytes := classifyEntry(req.TranscriptOffset+i+1, role, entry.Text, budget)
		if encodedBytes > budget {
			continue
		}
		accepted, nextSize, err := appendClassifyEntry(&request, &packet, excerpt)
		if err != nil {
			return request, err
		}
		if accepted {
			size = nextSize
			evidenceBytes += encodedBytes + 1
		}
	}
	return request, nil
}

func classifyUserRole(role string) bool {
	role = strings.ToLower(strings.TrimSpace(role))
	return role == "user" || role == "parent_user"
}

// Retain the latest instruction and original task from both direct and parent
// users before filling remaining room with recent user evidence. Excerpts keep
// their actual roles and chronological indexes; they are not model summaries.
func classifyUserOrder(entries []TranscriptEntry) []int {
	first := map[string]int{}
	last := map[string]int{}
	var users []int
	for i, entry := range entries {
		if !classifyUserRole(entry.Role) || strings.TrimSpace(entry.Text) == "" {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(entry.Role))
		if _, ok := first[role]; !ok {
			first[role] = i
		}
		last[role] = i
		users = append(users, i)
	}
	var order []int
	seen := map[int]bool{}
	add := func(i int) {
		if !seen[i] {
			seen[i] = true
			order = append(order, i)
		}
	}
	for _, positions := range []map[string]int{last, first} {
		for _, role := range []string{"user", "parent_user"} {
			if i, ok := positions[role]; ok {
				add(i)
			}
		}
	}
	for i := len(users) - 1; i >= 0; i-- {
		add(users[i])
	}
	return order
}

func compactClassifyApprovalContext(text string, budget int) (string, bool) {
	encoded, _ := json.Marshal(text)
	if len(encoded) <= budget {
		return text, false
	}
	var kept strings.Builder
	// Two quote bytes plus one spare byte for changing the truncation flag.
	used := 3
	for _, line := range strings.SplitAfter(text, "\n") {
		// Exact prior shell approvals were already checked by deterministic
		// approval. They do not authorize a different pending command.
		if strings.HasPrefix(line, "session_shell_command=") {
			continue
		}
		encoded, _ := json.Marshal(line)
		cost := len(encoded) - 2
		if used+cost <= budget {
			kept.WriteString(line)
			used += cost
		}
	}
	return kept.String(), text != kept.String()
}

func appendClassifyEntry(request *typesafe.Request, packet *classifyPacket, entry classifyTranscriptEntry) (bool, int, error) {
	candidate := *packet
	candidate.Transcript = append(append([]classifyTranscriptEntry(nil), packet.Transcript...), entry)
	sort.Slice(candidate.Transcript, func(i, j int) bool { return candidate.Transcript[i].Index < candidate.Transcript[j].Index })
	candidate.Omitted--
	if classifyUserRole(entry.Role) {
		candidate.OmittedUsers--
	}
	if entry.Truncated {
		candidate.Truncated++
	}
	next, size, err := marshalClassifyPacket(*request, candidate)
	if err != nil || size > maxClassifyRequestBytes {
		return false, size, err
	}
	*packet, *request = candidate, next
	return true, size, nil
}

func classifyRequestAction(req Request) classifyAction {
	actionType := "shell"
	if req.Path != "" || req.Selector != "" {
		actionType = "file"
	}
	if req.IsDirectory {
		actionType = "directory"
	}
	if req.WorkspaceAccess != "" {
		actionType = "workspace"
	}
	return classifyAction{
		Type: actionType, Command: req.Command, WorkDir: req.WorkDir, Tool: req.ToolName,
		Path: req.Path, Selector: req.Selector, IsWrite: req.IsWrite, IsDirectory: req.IsDirectory,
		WorkspaceAccess: req.WorkspaceAccess, Reason: req.Reason, ScopeID: req.ScopeID, ApprovalScope: req.ApprovalScope,
	}
}

func marshalClassifyPacket(request typesafe.Request, packet classifyPacket) (typesafe.Request, int, error) {
	state, err := json.Marshal(packet)
	if err != nil {
		return request, 0, fmt.Errorf("encode state: %w", err)
	}
	request.State = state
	body, err := json.Marshal(request) // Same encoder as typesafe.Client.Classify.
	if err != nil {
		return request, 0, fmt.Errorf("encode request: %w", err)
	}
	return request, len(body), nil
}

func classifyEvidenceRole(role string) bool {
	return role == "assistant" || role == "tool" || role == "parent_assistant" || role == "parent_tool" || strings.HasPrefix(role, "tool:")
}

func classifyEntry(index int, role, text string, budget int) (classifyTranscriptEntry, int) {
	entry := classifyTranscriptEntry{Index: index, Role: role, Text: text}
	encoded, _ := json.Marshal(entry)
	if len(encoded) <= budget {
		return entry, len(encoded)
	}
	// Bound serialized size, not raw characters. Keep UTF-8-safe head/tail
	// excerpts and mark every shortened message, including user evidence.
	for limit := max(1, budget); ; limit /= 2 {
		entry.Text = truncateString(text, limit)
		entry.Truncated = entry.Text != text
		encoded, _ = json.Marshal(entry)
		if len(encoded) <= budget || limit <= 1 {
			return entry, len(encoded)
		}
	}
}

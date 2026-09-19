package guardian

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClassifyLongSessionStillReviewsRoutineCommand(t *testing.T) {
	const command = "go test ./cmd -run '^TestHostedChildApprovalPolicyUsesServerDefault$' -count=1"
	const latest = "Run this local regression test once more; do not upload anything."
	large := strings.Repeat("historical context <>& 世界 ", 3000)
	approvals := "These file grants do not authorize network transfer.\n" +
		fmt.Sprintf("session_shell_command=%q workdir=%q\n", large, "/work") +
		"session_read_dir=\"/work\"\nsession_write_dir=\"/work\"\n"
	manyUsers := []TranscriptEntry{{Role: "user", Text: "Fix the local tests; never modify production."}}
	for i := 0; i < 200; i++ {
		manyUsers = append(manyUsers, TranscriptEntry{Role: "user", Text: fmt.Sprintf("Earlier request %d: %s", i, strings.Repeat("detail ", 100))})
	}
	for _, tc := range []struct {
		name       string
		transcript []TranscriptEntry
		approvals  string
	}{
		{"large user", []TranscriptEntry{{Role: "user", Text: large}}, ""},
		{"large parent", []TranscriptEntry{{Role: "parent_user", Text: large}}, ""},
		{"many users", manyUsers, ""},
		{"accumulated approvals", nil, approvals},
		{"unstructured approval context", nil, large},
		{"combined history and grants", append(manyUsers, TranscriptEntry{Role: "parent_user", Text: large}), approvals},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transcript := append(append([]TranscriptEntry(nil), tc.transcript...), TranscriptEntry{Role: "user", Text: latest})
			captured, _ := captureClassifyBudgetRequest(t, ClassifyReviewer{Model: "jev-test"}, Request{
				Command: command, WorkDir: "/work", Transcript: transcript, ApprovalContext: tc.approvals,
			})
			var packet classifyPacket
			if err := json.Unmarshal(captured.State, &packet); err != nil {
				t.Fatal(err)
			}
			if packet.Action.Command != command {
				t.Fatal("routine action was changed")
			}
			var userEvidence []classifyTranscriptEntry
			foundLatest := false
			lastIndex := 0
			for _, entry := range packet.Transcript {
				if entry.Index <= lastIndex || !utf8.ValidString(entry.Text) {
					t.Fatalf("invalid chronology or UTF-8: %+v", entry)
				}
				lastIndex = entry.Index
				if classifyUserRole(entry.Role) {
					userEvidence = append(userEvidence, entry)
				}
				if entry.Text == latest && entry.Role == "user" && !entry.Truncated {
					foundLatest = true
				}
			}
			if !foundLatest {
				t.Fatal("lost the latest exact authorization/restriction")
			}
			encoded, _ := json.Marshal(userEvidence)
			if len(encoded) > maxClassifyUserBytes {
				t.Fatalf("user context is %d bytes", len(encoded))
			}
			if len(tc.transcript) > 0 && packet.OmittedUsers == 0 && packet.Truncated == 0 {
				t.Fatal("large history was not marked as omitted or excerpted")
			}
			encoded, _ = json.Marshal(packet.ApprovalContext)
			if len(encoded) > maxClassifyApprovalBytes {
				t.Fatalf("approval context is %d bytes", len(encoded))
			}
			if tc.approvals != "" && !packet.ApprovalContextTruncated {
				t.Fatal("approval context was shortened without a completeness flag")
			}
			if tc.approvals == approvals {
				if strings.Contains(packet.ApprovalContext, "session_shell_command=") || !strings.Contains(packet.ApprovalContext, "session_write_dir=\"/work\"") {
					t.Fatalf("past commands crowded out current file grants: %q", packet.ApprovalContext)
				}
			}
			body, _ := json.Marshal(captured)
			t.Logf("routine command reviewed: request=%d bytes, omitted users=%d, excerpts=%d", len(body), packet.OmittedUsers, packet.Truncated)
		})
	}
}

func TestClassifyContextShrinksToRemainingRequestBudget(t *testing.T) {
	baseline, _ := captureClassifyBudgetRequest(t, ClassifyReviewer{Model: "jev-test"}, Request{Command: "x"})
	body, _ := json.Marshal(baseline)
	command := strings.Repeat("x", maxClassifyRequestBytes-len(body)-300)
	captured, _ := captureClassifyBudgetRequest(t, ClassifyReviewer{Model: "jev-test"}, Request{
		Command: command,
		Transcript: []TranscriptEntry{
			{Role: "parent_user", Text: strings.Repeat("parent context ", 5000)},
			{Role: "user", Text: "Run the requested local action, not production."},
			{Role: "tool", Text: strings.Repeat("tool output ", 5000)},
		},
		ApprovalContext: strings.Repeat("session_read_dir=\"/work\"\n", 5000),
	})
	var packet classifyPacket
	if err := json.Unmarshal(captured.State, &packet); err != nil {
		t.Fatal(err)
	}
	if packet.Action.Command != command || !packet.ApprovalContextTruncated {
		t.Fatal("remaining-budget compaction changed the action or hid omitted context")
	}
}

func TestClassifyApprovalContextKeepsWholeLines(t *testing.T) {
	longGrant := "session_read_dir=\"/work/" + strings.Repeat("very-long-directory", 500) + "\"\n"
	text := "Only first-party file operations are authorized.\n" + longGrant + "session_read_dir=\"/safe\"\n"
	got, truncated := compactClassifyApprovalContext(text, 200)
	if !truncated || strings.Contains(got, "very-long-directory") || !strings.Contains(got, "session_read_dir=\"/safe\"") {
		t.Fatalf("grants were clipped instead of omitted whole: %q", got)
	}
	for _, line := range strings.SplitAfter(got, "\n") {
		if !strings.Contains(text, line) {
			t.Fatalf("invented permission line: %q", line)
		}
	}
}

package plan

import (
	"strings"
	"testing"
)

func TestNewPlanDocument(t *testing.T) {
	doc := NewPlanDocument()
	if doc.LineCount() != 0 {
		t.Errorf("expected 0 lines, got %d", doc.LineCount())
	}
	if doc.Version() != 0 {
		t.Errorf("expected version 0, got %d", doc.Version())
	}
}

func TestNewPlanDocumentFromText(t *testing.T) {
	text := "Line 1\nLine 2\nLine 3"
	doc := NewPlanDocumentFromText(text, "user")

	if doc.LineCount() != 3 {
		t.Errorf("expected 3 lines, got %d", doc.LineCount())
	}

	lines := doc.Lines()
	if lines[0].Content != "Line 1" {
		t.Errorf("expected 'Line 1', got '%s'", lines[0].Content)
	}
	if lines[1].Content != "Line 2" {
		t.Errorf("expected 'Line 2', got '%s'", lines[1].Content)
	}
	if lines[2].Content != "Line 3" {
		t.Errorf("expected 'Line 3', got '%s'", lines[2].Content)
	}

	for _, line := range lines {
		if line.Author != "user" {
			t.Errorf("expected author 'user', got '%s'", line.Author)
		}
		if line.ID == "" {
			t.Error("expected non-empty line ID")
		}
	}
}

func TestPlanDocumentText(t *testing.T) {
	text := "Line 1\nLine 2\nLine 3"
	doc := NewPlanDocumentFromText(text, "user")

	result := doc.Text()
	if result != text {
		t.Errorf("expected '%s', got '%s'", text, result)
	}
}

func TestPlanDocumentInsertLine(t *testing.T) {
	doc := NewPlanDocument()

	// Insert at beginning (after -1)
	line1 := doc.InsertLine(-1, "First", "user")
	if doc.LineCount() != 1 {
		t.Errorf("expected 1 line, got %d", doc.LineCount())
	}
	if line1.Content != "First" {
		t.Errorf("expected 'First', got '%s'", line1.Content)
	}

	// Insert at end
	doc.InsertLine(0, "Second", "user")
	if doc.LineCount() != 2 {
		t.Errorf("expected 2 lines, got %d", doc.LineCount())
	}

	// Verify order
	lines := doc.Lines()
	if lines[0].Content != "First" || lines[1].Content != "Second" {
		t.Errorf("unexpected order: %s, %s", lines[0].Content, lines[1].Content)
	}

	// Insert in middle
	doc.InsertLine(0, "Middle", "agent")
	lines = doc.Lines()
	if lines[1].Content != "Middle" {
		t.Errorf("expected 'Middle' at index 1, got '%s'", lines[1].Content)
	}
}

func TestPlanDocumentInsertLineAfterID(t *testing.T) {
	doc := NewPlanDocumentFromText("Line 1\nLine 2", "user")
	lines := doc.Lines()

	// Insert after first line
	newLine := doc.InsertLineAfterID(lines[0].ID, "Inserted", "agent")
	if newLine == nil {
		t.Fatal("expected non-nil line")
	}

	lines = doc.Lines()
	if lines[1].Content != "Inserted" {
		t.Errorf("expected 'Inserted' at index 1, got '%s'", lines[1].Content)
	}
	if doc.LineCount() != 3 {
		t.Errorf("expected 3 lines, got %d", doc.LineCount())
	}

	// Insert with empty ID (append to end)
	doc.InsertLineAfterID("", "Last", "user")
	lines = doc.Lines()
	if lines[3].Content != "Last" {
		t.Errorf("expected 'Last' at end, got '%s'", lines[3].Content)
	}
}

func TestPlanDocumentUpdateLine(t *testing.T) {
	doc := NewPlanDocumentFromText("Original", "user")
	initialVersion := doc.Version()

	ok := doc.UpdateLine(0, "Updated", "agent")
	if !ok {
		t.Error("expected update to succeed")
	}

	line := doc.GetLine(0)
	if line.Content != "Updated" {
		t.Errorf("expected 'Updated', got '%s'", line.Content)
	}
	if line.Author != "agent" {
		t.Errorf("expected author 'agent', got '%s'", line.Author)
	}
	if doc.Version() <= initialVersion {
		t.Error("expected version to increment")
	}

	// Update non-existent line
	ok = doc.UpdateLine(999, "Fail", "user")
	if ok {
		t.Error("expected update to fail for invalid index")
	}
}

func TestPlanDocumentUpdateLineByID(t *testing.T) {
	doc := NewPlanDocumentFromText("Original", "user")
	lines := doc.Lines()
	lineID := lines[0].ID

	ok := doc.UpdateLineByID(lineID, "Updated", "agent")
	if !ok {
		t.Error("expected update to succeed")
	}

	line := doc.GetLine(0)
	if line.Content != "Updated" {
		t.Errorf("expected 'Updated', got '%s'", line.Content)
	}

	// Update with invalid ID
	ok = doc.UpdateLineByID("invalid-id", "Fail", "user")
	if ok {
		t.Error("expected update to fail for invalid ID")
	}
}

func TestPlanDocumentDeleteLine(t *testing.T) {
	doc := NewPlanDocumentFromText("Line 1\nLine 2\nLine 3", "user")

	ok := doc.DeleteLine(1)
	if !ok {
		t.Error("expected delete to succeed")
	}
	if doc.LineCount() != 2 {
		t.Errorf("expected 2 lines, got %d", doc.LineCount())
	}

	lines := doc.Lines()
	if lines[0].Content != "Line 1" || lines[1].Content != "Line 3" {
		t.Errorf("unexpected lines after delete: %v", lines)
	}

	// Delete invalid index
	ok = doc.DeleteLine(999)
	if ok {
		t.Error("expected delete to fail for invalid index")
	}
}

func TestPlanDocumentDeleteLineByID(t *testing.T) {
	doc := NewPlanDocumentFromText("Line 1\nLine 2", "user")
	lines := doc.Lines()
	lineID := lines[0].ID

	ok := doc.DeleteLineByID(lineID)
	if !ok {
		t.Error("expected delete to succeed")
	}
	if doc.LineCount() != 1 {
		t.Errorf("expected 1 line, got %d", doc.LineCount())
	}

	line := doc.GetLine(0)
	if line.Content != "Line 2" {
		t.Errorf("expected 'Line 2', got '%s'", line.Content)
	}

	// Delete with invalid ID
	ok = doc.DeleteLineByID("invalid-id")
	if ok {
		t.Error("expected delete to fail for invalid ID")
	}
}

func TestPlanDocumentSnapshot(t *testing.T) {
	doc := NewPlanDocumentFromText("Line 1\nLine 2", "user")

	snap := doc.Snapshot()
	if snap.Version != doc.Version() {
		t.Errorf("expected version %d, got %d", doc.Version(), snap.Version)
	}
	if len(snap.Lines) != 2 {
		t.Errorf("expected 2 lines in snapshot, got %d", len(snap.Lines))
	}
}

func TestPlanDocumentComputeChanges(t *testing.T) {
	doc := NewPlanDocumentFromText("Line 1\nLine 2\nLine 3", "user")
	snap := doc.Snapshot()
	lines := doc.Lines()

	// Make changes using line IDs
	doc.InsertLineAfterID(lines[0].ID, "New Line", "agent")     // Insert after Line 1
	doc.UpdateLineByID(lines[0].ID, "Modified Line 1", "agent") // Update Line 1
	doc.DeleteLineByID(lines[2].ID)                             // Delete Line 3

	changes := doc.ComputeChanges(snap)
	if changes.FromVersion != snap.Version {
		t.Errorf("expected FromVersion %d, got %d", snap.Version, changes.FromVersion)
	}

	// Count change types
	var inserts, updates, deletes int
	for _, c := range changes.Changes {
		switch c.Type {
		case ChangeInsert:
			inserts++
		case ChangeUpdate:
			updates++
		case ChangeDelete:
			deletes++
		}
	}

	if inserts != 1 {
		t.Errorf("expected 1 insert, got %d", inserts)
	}
	if updates != 1 {
		t.Errorf("expected 1 update, got %d", updates)
	}
	if deletes != 1 {
		t.Errorf("expected 1 delete, got %d", deletes)
	}
}

func TestPlanDocumentSummarizeChanges(t *testing.T) {
	doc := NewPlanDocumentFromText("Line 1", "user")
	snap := doc.Snapshot()

	// No changes
	summary := doc.SummarizeChanges(snap)
	if summary != "No changes" {
		t.Errorf("expected 'No changes', got '%s'", summary)
	}

	// Make some changes
	doc.InsertLine(0, "New", "agent")
	doc.UpdateLine(0, "Modified", "agent")

	summary = doc.SummarizeChanges(snap)
	if !strings.Contains(summary, "added") && !strings.Contains(summary, "modified") {
		t.Errorf("expected summary to mention additions/modifications, got '%s'", summary)
	}
}

func TestPlanDocumentSetText(t *testing.T) {
	doc := NewPlanDocumentFromText("Original", "user")

	doc.SetText("New\nContent", "agent")

	if doc.LineCount() != 2 {
		t.Errorf("expected 2 lines, got %d", doc.LineCount())
	}

	lines := doc.Lines()
	if lines[0].Content != "New" || lines[1].Content != "Content" {
		t.Errorf("unexpected content: %v", lines)
	}

	for _, line := range lines {
		if line.Author != "agent" {
			t.Errorf("expected author 'agent', got '%s'", line.Author)
		}
	}
}

func TestPlanDocumentGetLineByID(t *testing.T) {
	doc := NewPlanDocumentFromText("Line 1\nLine 2", "user")
	lines := doc.Lines()

	line, idx := doc.GetLineByID(lines[1].ID)
	if line == nil {
		t.Fatal("expected non-nil line")
	}
	if idx != 1 {
		t.Errorf("expected index 1, got %d", idx)
	}
	if line.Content != "Line 2" {
		t.Errorf("expected 'Line 2', got '%s'", line.Content)
	}

	// Non-existent ID
	line, idx = doc.GetLineByID("invalid")
	if line != nil || idx != -1 {
		t.Error("expected nil line and index -1 for invalid ID")
	}
}

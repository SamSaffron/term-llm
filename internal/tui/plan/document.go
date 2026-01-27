// Package plan provides a collaborative planning TUI where user and AI edit documents together.
package plan

import (
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// PlanLine represents a single line in the plan document.
type PlanLine struct {
	ID         string    // UUID for stable references
	Content    string    // Line text content
	Author     string    // "user" or "agent"
	ModifiedAt time.Time // Last modification time
}

// PlanDocument is a line-based document with version tracking.
type PlanDocument struct {
	mu      sync.RWMutex
	lines   []*PlanLine
	version int64
}

// NewPlanDocument creates an empty plan document.
func NewPlanDocument() *PlanDocument {
	return &PlanDocument{
		lines:   make([]*PlanLine, 0),
		version: 0,
	}
}

// NewPlanDocumentFromText creates a document from existing text.
func NewPlanDocumentFromText(text string, author string) *PlanDocument {
	doc := NewPlanDocument()
	if text == "" {
		return doc
	}

	lines := strings.Split(text, "\n")
	now := time.Now()
	for _, line := range lines {
		doc.lines = append(doc.lines, &PlanLine{
			ID:         uuid.New().String(),
			Content:    line,
			Author:     author,
			ModifiedAt: now,
		})
	}
	return doc
}

// Version returns the current document version.
func (d *PlanDocument) Version() int64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.version
}

// LineCount returns the number of lines.
func (d *PlanDocument) LineCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.lines)
}

// GetLine returns a line by index (0-based).
func (d *PlanDocument) GetLine(index int) *PlanLine {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if index < 0 || index >= len(d.lines) {
		return nil
	}
	return d.lines[index]
}

// GetLineByID returns a line by its ID.
func (d *PlanDocument) GetLineByID(id string) (*PlanLine, int) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for i, line := range d.lines {
		if line.ID == id {
			return line, i
		}
	}
	return nil, -1
}

// Text returns the full document as text.
func (d *PlanDocument) Text() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var sb strings.Builder
	for i, line := range d.lines {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(line.Content)
	}
	return sb.String()
}

// Lines returns a copy of all lines.
func (d *PlanDocument) Lines() []*PlanLine {
	d.mu.RLock()
	defer d.mu.RUnlock()
	result := make([]*PlanLine, len(d.lines))
	copy(result, d.lines)
	return result
}

// InsertLine inserts a new line after the specified index (-1 for beginning).
// Returns the new line.
func (d *PlanDocument) InsertLine(afterIndex int, content string, author string) *PlanLine {
	d.mu.Lock()
	defer d.mu.Unlock()

	line := &PlanLine{
		ID:         uuid.New().String(),
		Content:    content,
		Author:     author,
		ModifiedAt: time.Now(),
	}

	insertIdx := max(0, afterIndex+1)
	insertIdx = min(insertIdx, len(d.lines))

	// Insert at position
	d.lines = append(d.lines[:insertIdx], append([]*PlanLine{line}, d.lines[insertIdx:]...)...)
	d.version++

	return line
}

// InsertLineAfterID inserts a new line after the line with the given ID.
// If the ID is empty or not found, inserts at the end.
func (d *PlanDocument) InsertLineAfterID(afterID string, content string, author string) *PlanLine {
	d.mu.Lock()
	defer d.mu.Unlock()

	insertIdx := len(d.lines) // Default: append at end

	if afterID != "" {
		for i, line := range d.lines {
			if line.ID == afterID {
				insertIdx = i + 1
				break
			}
		}
	}

	line := &PlanLine{
		ID:         uuid.New().String(),
		Content:    content,
		Author:     author,
		ModifiedAt: time.Now(),
	}

	// Insert at position
	d.lines = append(d.lines[:insertIdx], append([]*PlanLine{line}, d.lines[insertIdx:]...)...)
	d.version++

	return line
}

// UpdateLine updates the content of a line by index.
func (d *PlanDocument) UpdateLine(index int, content string, author string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if index < 0 || index >= len(d.lines) {
		return false
	}

	d.lines[index].Content = content
	d.lines[index].Author = author
	d.lines[index].ModifiedAt = time.Now()
	d.version++

	return true
}

// UpdateLineByID updates a line by its ID.
func (d *PlanDocument) UpdateLineByID(id string, content string, author string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, line := range d.lines {
		if line.ID == id {
			line.Content = content
			line.Author = author
			line.ModifiedAt = time.Now()
			d.version++
			return true
		}
	}
	return false
}

// DeleteLine removes a line by index.
func (d *PlanDocument) DeleteLine(index int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if index < 0 || index >= len(d.lines) {
		return false
	}

	d.lines = append(d.lines[:index], d.lines[index+1:]...)
	d.version++

	return true
}

// DeleteLineByID removes a line by its ID.
func (d *PlanDocument) DeleteLineByID(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	for i, line := range d.lines {
		if line.ID == id {
			d.lines = append(d.lines[:i], d.lines[i+1:]...)
			d.version++
			return true
		}
	}
	return false
}

// SetText replaces the entire document content.
func (d *PlanDocument) SetText(text string, author string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.lines = nil
	if text == "" {
		d.version++
		return
	}

	lines := strings.Split(text, "\n")
	now := time.Now()
	for _, line := range lines {
		d.lines = append(d.lines, &PlanLine{
			ID:         uuid.New().String(),
			Content:    line,
			Author:     author,
			ModifiedAt: now,
		})
	}
	d.version++
}

// ChangeSet tracks changes between two versions of the document.
type ChangeSet struct {
	FromVersion int64
	ToVersion   int64
	Changes     []Change
}

// Change represents a single change in the document.
type Change struct {
	Type      ChangeType
	LineID    string
	LineIndex int
	Content   string
	Author    string
}

// ChangeType indicates the type of change.
type ChangeType string

const (
	ChangeInsert ChangeType = "insert"
	ChangeUpdate ChangeType = "update"
	ChangeDelete ChangeType = "delete"
)

// DocumentSnapshot captures the state of a document at a point in time.
type DocumentSnapshot struct {
	Version int64
	Lines   []LineSnapshot
}

// LineSnapshot captures a line's state.
type LineSnapshot struct {
	ID      string
	Content string
	Author  string
}

// Snapshot creates a snapshot of the current document state.
func (d *PlanDocument) Snapshot() DocumentSnapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()

	snap := DocumentSnapshot{
		Version: d.version,
		Lines:   make([]LineSnapshot, len(d.lines)),
	}
	for i, line := range d.lines {
		snap.Lines[i] = LineSnapshot{
			ID:      line.ID,
			Content: line.Content,
			Author:  line.Author,
		}
	}
	return snap
}

// ComputeChanges computes the changes between a snapshot and the current state.
func (d *PlanDocument) ComputeChanges(from DocumentSnapshot) ChangeSet {
	d.mu.RLock()
	defer d.mu.RUnlock()

	cs := ChangeSet{
		FromVersion: from.Version,
		ToVersion:   d.version,
	}

	// Build maps for comparison
	fromLines := make(map[string]string)
	for _, line := range from.Lines {
		fromLines[line.ID] = line.Content
	}

	currentLines := make(map[string]struct{})
	for i, line := range d.lines {
		currentLines[line.ID] = struct{}{}

		if oldContent, exists := fromLines[line.ID]; exists {
			// Line existed before - check for update
			if oldContent != line.Content {
				cs.Changes = append(cs.Changes, Change{
					Type:      ChangeUpdate,
					LineID:    line.ID,
					LineIndex: i,
					Content:   line.Content,
					Author:    line.Author,
				})
			}
		} else {
			// New line
			cs.Changes = append(cs.Changes, Change{
				Type:      ChangeInsert,
				LineID:    line.ID,
				LineIndex: i,
				Content:   line.Content,
				Author:    line.Author,
			})
		}
	}

	// Check for deleted lines
	for _, line := range from.Lines {
		if _, exists := currentLines[line.ID]; !exists {
			cs.Changes = append(cs.Changes, Change{
				Type:    ChangeDelete,
				LineID:  line.ID,
				Content: line.Content,
			})
		}
	}

	return cs
}

// SummarizeChanges returns a human-readable summary of changes since the given snapshot.
func (d *PlanDocument) SummarizeChanges(from DocumentSnapshot) string {
	cs := d.ComputeChanges(from)
	if len(cs.Changes) == 0 {
		return "No changes"
	}

	var inserts, updates, deletes int
	for _, c := range cs.Changes {
		switch c.Type {
		case ChangeInsert:
			inserts++
		case ChangeUpdate:
			updates++
		case ChangeDelete:
			deletes++
		}
	}

	var parts []string
	if inserts > 0 {
		parts = append(parts, pluralize(inserts, "line added", "lines added"))
	}
	if updates > 0 {
		parts = append(parts, pluralize(updates, "line modified", "lines modified"))
	}
	if deletes > 0 {
		parts = append(parts, pluralize(deletes, "line deleted", "lines deleted"))
	}

	return strings.Join(parts, ", ")
}

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return strings.Replace(plural, "lines", strings.Replace(plural[:1], "l", "", 1)+strings.Replace(plural, "lines", "", 1), 1)
}

// GetLinesModifiedSince returns lines modified after the given time by the specified author.
func (d *PlanDocument) GetLinesModifiedSince(since time.Time, author string) []*PlanLine {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var result []*PlanLine
	for _, line := range d.lines {
		if line.ModifiedAt.After(since) && (author == "" || line.Author == author) {
			result = append(result, line)
		}
	}
	return result
}
